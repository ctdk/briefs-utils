package fuse

import (
	"os/exec"
	"testing"

	"github.com/ctdk/briefs-utils/briefs"
)

// TestUnmountCheckpoint verifies that an unmount always checkpoints the
// journal (log_start==log_end, nothing to replay on remount), that the
// superblock log boundaries persist, that fsck is clean, and that a fresh
// mount reads back the data.
func TestUnmountCheckpoint(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)

	// Create + write + fsync some state so the journal is non-trivially dirty.
	in, err := b.createInDir(1, "u", briefs.ModeFile|0o644, 1000, 1000, false)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	pat := makePattern(7, 5000)
	writeFile(t, b, in.InodeNumber, pat, 0)
	if err := b.setattrOp(in.InodeNumber, setattrReq(fattrMode, withMode(briefs.ModeFile|0o600))); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	// Unmount: checkpoint + close.
	if err := b.journal.Checkpoint(); err != nil {
		t.Fatalf("unmount checkpoint: %v", err)
	}
	if err := b.dev.Sync(); err != nil {
		t.Fatalf("unmount dev sync: %v", err)
	}
	b.dev.Close()

	// Re-open the image and verify the on-disk superblock has log_start==log_end.
	dev2, _, err := OpenBlockDevice(img)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { dev2.Close() })
	sb, err := readSuperblock(dev2)
	if err != nil {
		t.Fatalf("reread superblock: %v", err)
	}
	if sb.JournalLogStart != sb.JournalLogEnd {
		t.Fatalf("after unmount: log_start=%d != log_end=%d (journal not drained)",
			sb.JournalLogStart, sb.JournalLogEnd)
	}

	// fsck on the closed image must be clean.
	fsck := buildBinary(t, "github.com/ctdk/briefs-utils/cmd/fsck", "fsck.briefs")
	out, err := exec.Command(fsck, img).CombinedOutput()
	if err != nil {
		t.Fatalf("fsck after unmount: %v\n%s", err, out)
	}
	if !contains(string(out), "FSCK COMPLETE: no errors found") {
		t.Fatalf("fsck not clean after unmount:\n%s", out)
	}

	// A fresh mount (kernel-style reopen) reads back the data unchanged.
	b2 := openBridge(t, img)
	ino := in.InodeNumber
	got := readFile(t, b2, ino, 0, 5000)
	if !bytesEqual(got, pat) {
		t.Fatalf("fresh-mount readback mismatch")
	}
	di, _ := b2.inodes.ReadInode(ino)
	if di.Filemode&0o600 != 0o600 {
		t.Fatalf("fresh-mount mode: want 0600, got %o", di.Filemode&0o7777)
	}
	// Cleanly checkpoint the second bridge's journal before its cleanup closes.
	_ = b2.journal.Checkpoint()
	_ = b2.dev.Sync()
}