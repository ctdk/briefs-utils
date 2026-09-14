package fuse

import (
	"context"
	"strings"
	"syscall"
	"testing"

	"github.com/ctdk/briefs-utils/briefs"
)

// TestFslabel covers FS_IOC_{GET,SET}FSLABEL: round-trip, the 64-char cap
// (the on-disk field size), strnlen semantics, persistence to block 0, and
// the ioctl dispatch (SET requires a caller with CAP_SYS_ADMIN; GET does
// not). Mirrors the kernel's briefs_ioctl cases (file.c:282-323).
func TestFslabel(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)

	get := func() string {
		out := make([]byte, fslabelMax)
		b.fslabelGetOp(out)
		return strings.TrimRight(string(out), "\x00")
	}

	if err := b.fslabelSetOp([]byte("testvol")); err != nil {
		t.Fatalf("set label: %v", err)
	}
	if got := get(); got != "testvol" {
		t.Fatalf("label round-trip: want %q, got %q", "testvol", got)
	}
	if string(b.sb.Label[:8]) != "testvol\x00" {
		t.Fatalf("on-disk label not null-padded: %q", b.sb.Label[:8])
	}

	// strnlen: the label ends at the first NUL.
	if err := b.fslabelSetOp([]byte{'a', 'b', 0, 'c'}); err != nil {
		t.Fatalf("set label with embedded NUL: %v", err)
	}
	if got := get(); got != "ab" {
		t.Fatalf("embedded NUL: want %q, got %q", "ab", got)
	}

	// 64 chars fit the field; 65 is EINVAL.
	long := strings.Repeat("l", 64)
	if err := b.fslabelSetOp([]byte(long)); err != nil {
		t.Fatalf("set 64-char label: %v", err)
	}
	if got := get(); got != long {
		t.Fatalf("64-char label round-trip failed")
	}
	if err := b.fslabelSetOp([]byte(strings.Repeat("l", 65))); err != syscall.EINVAL {
		t.Errorf("65-char label: want EINVAL, got %v", err)
	}

	// Persisted: a second reader of the image sees the label.
	dev2, _, err := OpenBlockDevice(img)
	if err != nil {
		t.Fatalf("reopen image: %v", err)
	}
	defer dev2.Close()
	sb2, err := readSuperblock(dev2)
	if err != nil {
		t.Fatalf("reread superblock: %v", err)
	}
	if strings.TrimRight(string(sb2.Label[:]), "\x00") != long {
		t.Fatalf("label not persisted: %q", sb2.Label[:])
	}

	// Dispatch: GET works without a caller context, SET is EPERM without
	// one (FUSE does not re-check capabilities for the daemon).
	out := make([]byte, fslabelMax)
	_, errno, handled := b.ioctlMount(context.Background(), fsIocGetfslabel, nil, out)
	if !handled || errno != 0 {
		t.Fatalf("ioctl GETFSLABEL dispatch: handled=%v errno=%v", handled, errno)
	}
	if strings.TrimRight(string(out), "\x00") != long {
		t.Fatalf("ioctl GETFSLABEL output mismatch")
	}
	_, errno, handled = b.ioctlMount(context.Background(), fsIocSetfslabel, []byte("x"), nil)
	if !handled || errno != syscall.EPERM {
		t.Fatalf("ioctl SETFSLABEL without caller: handled=%v errno=%v", handled, errno)
	}

	fsckClean(t, b, img)
}

// TestFstrim covers FITRIM: the whole-device trim discards every free data
// block (the deleted file's block reads back as zeros, the survivor's data
// is untouched), sub-range requests clamp to the requested blocks, minlen
// filters short slices, and the EINVAL guards mirror the kernel's
// ext4_trim_fs rules (alloc.c:525). Mirrors briefs_trim_fs (alloc.c:512).
func TestFstrim(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)
	bs := uint64(b.blockSize)

	// Two one-block files; one survives, one is deleted so its block
	// becomes a free run FSTRIM must punch.
	inKeep, _ := b.createInDir(1, "keep", briefs.ModeFile|0o644, 1000, 1000, false, 0)
	inGone, _ := b.createInDir(1, "gone", briefs.ModeFile|0o644, 1000, 1000, false, 0)
	pat := makePattern(7, int(bs))
	writeFile(t, b, inKeep.InodeNumber, pat, 0)
	writeFile(t, b, inGone.InodeNumber, pat, 0)

	diGone, _ := b.inodes.ReadInode(inGone.InodeNumber)
	exts := extentListOf(t, b, diGone)
	if len(exts) != 1 {
		t.Fatalf("gone file extents: %+v", exts)
	}
	goneAbs := exts[0].Phys
	goneRel := goneAbs - b.dataRegionStart

	freeBefore := b.dataAlloc.FreeCount()
	if err := b.unlinkInDir(1, "gone", false); err != nil {
		t.Fatalf("unlink gone: %v", err)
	}
	// The unlink's block frees are deferred until their records commit
	// (deferBlockFree); sync the journal to apply them.
	if err := b.journal.Sync(false); err != nil {
		t.Fatalf("journal sync after unlink: %v", err)
	}
	if got := b.dataAlloc.FreeCount(); got != freeBefore+1 {
		t.Fatalf("unlink did not free the data block: %d -> %d", freeBefore, got)
	}

	// Whole-device trim: every free data block is discarded.
	trimmed, err := b.fstrimOp(fsTrimRange{start: 0, length: ^uint64(0)})
	if err != nil {
		if err == syscall.EOPNOTSUPP {
			t.Skip("backing filesystem cannot punch holes")
		}
		t.Fatalf("fstrim: %v", err)
	}
	if want := b.dataAlloc.FreeCount() * bs; trimmed != want {
		t.Fatalf("whole-device trim: want %d bytes, got %d", want, trimmed)
	}
	// The freed block was punched: it reads back as zeros.
	blk, err := b.dev.ReadBlock(goneAbs)
	if err != nil {
		t.Fatalf("read punched block: %v", err)
	}
	if !allZero(blk) {
		t.Fatalf("freed block %d was not punched", goneAbs)
	}
	// The survivor's data is untouched.
	if got := readFile(t, b, inKeep.InodeNumber, 0, int64(bs)); !bytesEqual(got, pat) {
		t.Fatalf("fstrim corrupted live data")
	}

	// Sub-range: only the requested blocks count.
	trimmed, err = b.fstrimOp(fsTrimRange{start: goneRel * bs, length: bs})
	if err != nil {
		t.Fatalf("sub-range fstrim: %v", err)
	}
	if trimmed != bs {
		t.Fatalf("sub-range trim: want %d, got %d", bs, trimmed)
	}

	// minlen filters short slices: one block requested, minlen two.
	trimmed, err = b.fstrimOp(fsTrimRange{start: goneRel * bs, length: bs, minlen: 2 * bs})
	if err != nil {
		t.Fatalf("minlen fstrim: %v", err)
	}
	if trimmed != 0 {
		t.Fatalf("minlen filter: want 0 trimmed, got %d", trimmed)
	}

	// EINVAL guards (kernel rules, alloc.c:525).
	if _, err := b.fstrimOp(fsTrimRange{start: 0, length: bs - 1}); err != syscall.EINVAL {
		t.Errorf("len < blocksize: want EINVAL, got %v", err)
	}
	maxBlks := b.dataAlloc.TotalBlocks()
	if _, err := b.fstrimOp(fsTrimRange{start: maxBlks * bs, length: bs}); err != syscall.EINVAL {
		t.Errorf("start beyond device: want EINVAL, got %v", err)
	}

	// Dispatch: no FUSE caller context -> EPERM.
	in := make([]byte, sizeFsTrimRange)
	encodeFsTrimRange(in, fsTrimRange{start: 0, length: ^uint64(0)})
	_, errno, handled := b.ioctlMount(context.Background(), fiTrim, in, make([]byte, sizeFsTrimRange))
	if !handled || errno != syscall.EPERM {
		t.Fatalf("ioctl FITRIM without caller: handled=%v errno=%v", handled, errno)
	}

	fsckClean(t, b, img)
}
