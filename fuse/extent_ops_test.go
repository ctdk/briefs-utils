package fuse

import (
	"syscall"
	"testing"

	"github.com/ctdk/briefs-utils/briefs"
)

// setattrReq builds a fuseSetAttrIn with the given fields set.
func setattrReq(valid uint32, opts ...func(*fuseSetAttrIn)) *fuseSetAttrIn {
	r := &fuseSetAttrIn{valid: valid}
	for _, o := range opts {
		o(r)
	}
	return r
}

func withSize(s uint64) func(*fuseSetAttrIn)  { return func(r *fuseSetAttrIn) { r.size = s } }
func withMode(m uint32) func(*fuseSetAttrIn)  { return func(r *fuseSetAttrIn) { r.mode = m } }
func withUID(u uint32) func(*fuseSetAttrIn)   { return func(r *fuseSetAttrIn) { r.uid = u } }
func withGID(g uint32) func(*fuseSetAttrIn)   { return func(r *fuseSetAttrIn) { r.gid = g } }

// TestFallocatePreallocate covers KEEP_SIZE preallocate (unwritten extents that
// read as zeros and convert on write) and plain preallocate (grows size).
func TestFallocatePreallocate(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)

	in, _ := b.createInDir(1, "p", briefs.ModeFile|0o644, 1000, 1000, false)
	ino := in.InodeNumber

	// KEEP_SIZE preallocate [0, 8192): two unwritten blocks (merged into one
	// extent of len 2), size unchanged (0).
	freeBeforePre := b.dataAlloc.FreeCount()
	if err := b.fallocateOp(ino, 0, 8192, fallocKeepSize); err != nil {
		t.Fatalf("fallocate keep-size: %v", err)
	}
	di, _ := b.inodes.ReadInode(ino)
	if di.FileSize != 0 {
		t.Fatalf("KEEP_SIZE grew size: want 0, got %d", di.FileSize)
	}
	if got := b.dataAlloc.FreeCount(); got != freeBeforePre-2 {
		t.Fatalf("preallocate allocated blocks: free %d -> %d (want -2)", freeBeforePre, got)
	}
	exts, _, _ := b.collectExtentsAndNodes(di)
	if len(exts) != 1 || exts[0].Offset != 0 || exts[0].Len != 2 || exts[0].Flags&briefs.ExtentFlagUnwritten == 0 {
		t.Fatalf("preallocate extents: want one unwritten {0,2}, got %+v", exts)
	}
	// Reads return zeros (unwritten).
	got := readFile(t, b, ino, 0, 8192)
	for i, v := range got {
		if v != 0 {
			t.Fatalf("unwritten read byte %d: want 0, got %d", i, v)
		}
	}

	// A write into the unwritten range converts block 0 and grows the size.
	pat := makePattern(1, 100)
	writeFile(t, b, ino, pat, 0)
	di, _ = b.inodes.ReadInode(ino)
	if di.FileSize != 100 {
		t.Fatalf("after write into preallocated: size want 100, got %d", di.FileSize)
	}
	if got := readFile(t, b, ino, 0, 100); !bytesEqual(got, pat) {
		t.Fatalf("written region readback mismatch")
	}
	// Block 1 is still unwritten -> zeros.
	if got := readFile(t, b, ino, 4096, 4096); !allZero(got) {
		t.Fatalf("unwritten block 1 should read zeros")
	}

	// Re-preallocating the same range is a no-op (blocks already mapped).
	freeBefore := b.dataAlloc.FreeCount()
	if err := b.fallocateOp(ino, 0, 8192, fallocKeepSize); err != nil {
		t.Fatalf("re-fallocate: %v", err)
	}
	if got := b.dataAlloc.FreeCount(); got != freeBefore {
		t.Fatalf("re-fallocate consumed blocks: %d -> %d", freeBefore, got)
	}

	// Plain preallocate past EOF grows the size.
	if err := b.fallocateOp(ino, 8192, 4096, 0); err != nil {
		t.Fatalf("plain fallocate: %v", err)
	}
	di, _ = b.inodes.ReadInode(ino)
	if di.FileSize != 12288 {
		t.Fatalf("plain fallocate size: want 12288, got %d", di.FileSize)
	}

	fsckClean(t, b, img)
}

// TestFallocatePunchHole writes data, punches a hole in the middle, and verifies
// the hole reads zeros while the data outside survives.
func TestFallocatePunchHole(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)

	in, _ := b.createInDir(1, "h", briefs.ModeFile|0o644, 1000, 1000, false)
	ino := in.InodeNumber
	bs := int64(b.blockSize)

	// Write three blocks of distinct data.
	p0 := makePattern(1, int(bs))
	p1 := makePattern(2, int(bs))
	p2 := makePattern(3, int(bs))
	writeFile(t, b, ino, p0, 0)
	writeFile(t, b, ino, p1, bs)
	writeFile(t, b, ino, p2, 2*bs)

	// Punch a hole in block 1.
	if err := b.fallocateOp(ino, uint64(bs), uint64(bs), fallocPunchHole|fallocKeepSize); err != nil {
		t.Fatalf("punch hole: %v", err)
	}
	if got := readFile(t, b, ino, bs, bs); !allZero(got) {
		t.Fatalf("punched block should read zeros")
	}
	if got := readFile(t, b, ino, 0, bs); !bytesEqual(got, p0) {
		t.Fatalf("block 0 changed after punch")
	}
	if got := readFile(t, b, ino, 2*bs, bs); !bytesEqual(got, p2) {
		t.Fatalf("block 2 changed after punch")
	}
	// The punched block's data block must be freed.
	di, _ := b.inodes.ReadInode(ino)
	exts, _, _ := b.collectExtentsAndNodes(di)
	for _, e := range exts {
		if e.Offset == 1 && e.Phys != 0 {
			t.Fatalf("punched block 1 still mapped (phys %d)", e.Phys)
		}
	}

	fsckClean(t, b, img)
}

// TestTruncate covers truncate down (frees extents, zeroes EOF tail) and up
// (grows size, gap reads zeros).
func TestTruncate(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)

	in, _ := b.createInDir(1, "t", briefs.ModeFile|0o644, 1000, 1000, false)
	ino := in.InodeNumber
	bs := int64(b.blockSize)

	// Write ~2.5 blocks.
	writeFile(t, b, ino, makePattern(1, int(2*bs+1000)), 0)
	freeBefore := b.dataAlloc.FreeCount()

	// Truncate down to 1000 (frees blocks 1+; zeroes the tail of block 0).
	if err := b.truncateInode(ino, 1000); err != nil {
		t.Fatalf("truncate down: %v", err)
	}
	di, _ := b.inodes.ReadInode(ino)
	if di.FileSize != 1000 {
		t.Fatalf("truncate down size: want 1000, got %d", di.FileSize)
	}
	if got := b.dataAlloc.FreeCount(); got <= freeBefore {
		t.Fatalf("truncate down did not free blocks: %d -> %d", freeBefore, got)
	}
	if got := readFile(t, b, ino, 0, 1000); !bytesEqual(got, makePattern(1, 1000)) {
		t.Fatalf("truncate down data mismatch")
	}

	// Truncate up to 2*bs; the gap reads zeros.
	if err := b.truncateInode(ino, uint64(2*bs)); err != nil {
		t.Fatalf("truncate up: %v", err)
	}
	di, _ = b.inodes.ReadInode(ino)
	if di.FileSize != uint64(2*bs) {
		t.Fatalf("truncate up size: want %d, got %d", 2*bs, di.FileSize)
	}
	got := readFile(t, b, ino, 1000, int64(2*bs)-1000)
	for i, v := range got {
		if v != 0 {
			t.Fatalf("truncate-up gap byte %d: want 0, got %d", i, v)
		}
	}

	fsckClean(t, b, img)
}

// TestKillpriv checks that suid/sgid and security.capability are stripped on
// write and chown (generic/093/193).
func TestKillpriv(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)

	in, _ := b.createInDir(1, "k", briefs.ModeFile|0o644, 1000, 1000, false)
	ino := in.InodeNumber

	// chmod 4755 (setuid).
	if err := b.setattrOp(ino, setattrReq(fattrMode, withMode(briefs.ModeFile|0o4755))); err != nil {
		t.Fatalf("chmod setuid: %v", err)
	}
	di, _ := b.inodes.ReadInode(ino)
	if di.Filemode&0o4000 == 0 {
		t.Fatalf("setuid not set by chmod")
	}
	// A write strips setuid.
	writeFile(t, b, ino, makePattern(1, 50), 0)
	di, _ = b.inodes.ReadInode(ino)
	if di.Filemode&0o4000 != 0 {
		t.Fatalf("setuid not stripped on write: mode %o", di.Filemode)
	}

	// chmod setuid again, then chown -> strips setuid.
	if err := b.setattrOp(ino, setattrReq(fattrMode, withMode(briefs.ModeFile|0o4755))); err != nil {
		t.Fatalf("chmod setuid 2: %v", err)
	}
	if err := b.setattrOp(ino, setattrReq(fattrUID, withUID(2000))); err != nil {
		t.Fatalf("chown: %v", err)
	}
	di, _ = b.inodes.ReadInode(ino)
	if di.Filemode&0o4000 != 0 {
		t.Fatalf("setuid not stripped on chown: mode %o", di.Filemode)
	}
	if di.Uid != 2000 {
		t.Fatalf("chown uid: want 2000, got %d", di.Uid)
	}

	// security.capability is cleared on a write.
	if err := b.setXattr(ino, "security.capability", []byte{0,0,0,1,2,3,4,5,6,7,8,9,0,1,2,3}, 0); err != nil {
		t.Fatalf("set security.capability: %v", err)
	}
	if got, _ := b.getXattr(ino, "security.capability"); len(got) == 0 {
		t.Fatalf("security.capability not set")
	}
	writeFile(t, b, ino, makePattern(2, 50), 0)
	if _, err := b.getXattr(ino, "security.capability"); err != syscall.ENODATA {
		t.Fatalf("security.capability not cleared on write: err %v", err)
	}

	fsckClean(t, b, img)
}

// allZero reports whether b is all zero bytes.
func allZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}