package fuse

import (
	"os/exec"
	"testing"

	"github.com/ctdk/briefs-utils/briefs"
)

// makePattern returns n bytes of a deterministic, self-checking pattern.
func makePattern(seed, n int) []byte {
	b := make([]byte, n)
	for i := 0; i < n; i++ {
		b[i] = byte((i*7 + seed) % 251)
	}
	return b
}

// writeFile is a test helper that writes data to ino at off and fatals on error.
func writeFile(t *testing.T, b *BrieFS, ino uint64, data []byte, off int64) {
	t.Helper()
	if _, err := b.writeFileData(ino, data, off); err != nil {
		t.Fatalf("writeFileData ino %d off %d len %d: %v", ino, off, len(data), err)
	}
}

// readFile reads len bytes from ino at off, fataling on error.
func readFile(t *testing.T, b *BrieFS, ino uint64, off, n int64) []byte {
	t.Helper()
	got, err := b.readFileData(ino, make([]byte, n), off)
	if err != nil {
		t.Fatalf("readFileData ino %d off %d len %d: %v", ino, off, n, err)
	}
	return got
}

// fsckClean checkpoints the journal, closes the device, and runs fsck.briefs,
// fataling unless it reports no errors.
func fsckClean(t *testing.T, b *BrieFS, img string) {
	t.Helper()
	if err := b.journal.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	b.dev.Close()
	fsck := buildBinary(t, "github.com/ctdk/briefs-utils/cmd/fsck", "fsck.briefs")
	out, err := exec.Command(fsck, img).CombinedOutput()
	if err != nil {
		t.Fatalf("fsck failed: %v\n%s", err, out)
	}
	if !contains(string(out), "FSCK COMPLETE: no errors found") {
		t.Fatalf("fsck not clean:\n%s", out)
	}
}

// TestFileWriteInline exercises the inline-data path: a small write stays in the
// inode's inline region, a gap within the 256-byte region reads as zeros, and a
// later write that exceeds inline capacity promotes the inode to extent-backed.
func TestFileWriteInline(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)

	in, err := b.createInDir(1, "inline", briefs.ModeFile|0o644, 1000, 1000, false)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ino := in.InodeNumber

	// 100 bytes at offset 0 -> inline.
	p0 := makePattern(1, 100)
	writeFile(t, b, ino, p0, 0)
	di, _ := b.inodes.ReadInode(ino)
	if di.Flags&briefs.InodeFlagInlineData == 0 {
		t.Fatalf("after 100B write: expected InodeFlagInlineData, flags=0x%x", di.Flags)
	}
	if di.FileSize != 100 {
		t.Fatalf("inline size: want 100, got %d", di.FileSize)
	}
	if got := readFile(t, b, ino, 0, 100); !bytesEqual(got, p0) {
		t.Fatalf("inline readback mismatch")
	}

	// 50 bytes at offset 200 (gap [100,200) must read as zero); total 250 <= 256.
	p1 := makePattern(2, 50)
	writeFile(t, b, ino, p1, 200)
	di, _ = b.inodes.ReadInode(ino)
	if di.Flags&briefs.InodeFlagInlineData == 0 {
		t.Fatalf("after 250B write: expected still-inline, flags=0x%x", di.Flags)
	}
	if di.FileSize != 250 {
		t.Fatalf("inline size: want 250, got %d", di.FileSize)
	}
	got := readFile(t, b, ino, 0, 250)
	if !bytesEqual(got[:100], p0) {
		t.Fatalf("inline prefix mismatch after gap write")
	}
	for i := 100; i < 200; i++ {
		if got[i] != 0 {
			t.Fatalf("inline gap byte %d: want 0, got %d", i, got[i])
		}
	}
	if !bytesEqual(got[200:250], p1) {
		t.Fatalf("inline tail mismatch")
	}

	// Write 300 bytes at offset 0 -> exceeds inline capacity -> promote + extent.
	p2 := makePattern(3, 300)
	writeFile(t, b, ino, p2, 0)
	di, _ = b.inodes.ReadInode(ino)
	if di.Flags&briefs.InodeFlagInlineData != 0 {
		t.Fatalf("after promote: InodeFlagInlineData still set, flags=0x%x", di.Flags)
	}
	if di.Flags&briefs.InodeFlagIndexed != 0 {
		t.Fatalf("after promote: should be inline-only extents, got Indexed")
	}
	if di.NumExtentsInline != 1 || di.NumExtentsTotal != 1 {
		t.Fatalf("after promote: extents inline=%d total=%d, want 1/1", di.NumExtentsInline, di.NumExtentsTotal)
	}
	if di.FileSize != 300 {
		t.Fatalf("promoted size: want 300, got %d", di.FileSize)
	}
	if got := readFile(t, b, ino, 0, 300); !bytesEqual(got, p2) {
		t.Fatalf("promoted readback mismatch")
	}

	fsckClean(t, b, img)
}

// TestFileWriteSpillBtree writes enough non-adjacent blocks to exceed the 8
// inline-extent capacity and spill into a B+ tree, then verifies readback
// (including zero-filled holes), an in-place overwrite, and fsck cleanliness.
func TestFileWriteSpillBtree(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)

	in, err := b.createInDir(1, "big", briefs.ModeFile|0o644, 1000, 1000, false)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ino := in.InodeNumber

	bs := int64(b.blockSize)
	// Write one block at every other block offset: 9 separate (non-mergeable)
	// extents -> the 9th spills inline -> tree.
	patterns := make([][]byte, 9)
	for k := 0; k < 9; k++ {
		iblock := int64(k * 2) // blocks 0,2,4,...,16
		patterns[k] = makePattern(k+10, int(bs))
		writeFile(t, b, ino, patterns[k], iblock*bs)
	}

	di, _ := b.inodes.ReadInode(ino)
	if di.Flags&briefs.InodeFlagIndexed == 0 {
		t.Fatalf("after 9 extents: expected InodeFlagIndexed, flags=0x%x", di.Flags)
	}
	if di.ExtentInlineBase == 0 {
		t.Fatalf("after spill: ExtentInlineBase should be set")
	}
	if di.NumExtentsInline != 0 {
		t.Fatalf("after spill: NumExtentsInline should be 0, got %d", di.NumExtentsInline)
	}
	if di.NumExtentsTotal != 9 {
		t.Fatalf("after spill: NumExtentsTotal want 9, got %d", di.NumExtentsTotal)
	}
	wantSize := uint64(17 * bs) // last block 16 + 1
	if di.FileSize != wantSize {
		t.Fatalf("spill size: want %d, got %d", wantSize, di.FileSize)
	}

	// Read back each written block and the holes between them.
	for k := 0; k < 9; k++ {
		iblock := int64(k * 2)
		got := readFile(t, b, ino, iblock*bs, bs)
		if !bytesEqual(got, patterns[k]) {
			t.Fatalf("spill readback block %d mismatch", iblock)
		}
		// The block after a written one is a hole (except the last).
		if k < 8 {
			hole := readFile(t, b, ino, (iblock+1)*bs, bs)
			for i, v := range hole {
				if v != 0 {
					t.Fatalf("hole block %d byte %d: want 0, got %d", iblock+1, i, v)
				}
			}
		}
	}

	// Overwrite block 0 in place (RMW of a mapped block) and re-read.
	ovr := makePattern(99, int(bs))
	writeFile(t, b, ino, ovr, 0)
	if got := readFile(t, b, ino, 0, bs); !bytesEqual(got, ovr) {
		t.Fatalf("overwrite block 0 readback mismatch")
	}
	// Block 2 (un-touched by the overwrite) must be unchanged.
	if got := readFile(t, b, ino, 2*bs, bs); !bytesEqual(got, patterns[1]) {
		t.Fatalf("block 2 changed after overwrite of block 0")
	}

	fsckClean(t, b, img)
}

// TestFileWriteHoleAndAppend writes a block, then a block far past it (leaving a
// multi-block hole), and verifies the hole reads as zeros and the file size is
// correct.
func TestFileWriteHoleAndAppend(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)

	in, err := b.createInDir(1, "holes", briefs.ModeFile|0o644, 1000, 1000, false)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ino := in.InodeNumber
	bs := int64(b.blockSize)

	p0 := makePattern(5, int(bs))
	writeFile(t, b, ino, p0, 0) // block 0
	p5 := makePattern(6, int(bs))
	writeFile(t, b, ino, p5, 5*bs) // block 5, leaving blocks 1-4 as holes

	di, _ := b.inodes.ReadInode(ino)
	if di.FileSize != uint64(6*bs) {
		t.Fatalf("size: want %d, got %d", 6*bs, di.FileSize)
	}
	if got := readFile(t, b, ino, 0, bs); !bytesEqual(got, p0) {
		t.Fatalf("block 0 readback mismatch")
	}
	if got := readFile(t, b, ino, 5*bs, bs); !bytesEqual(got, p5) {
		t.Fatalf("block 5 readback mismatch")
	}
	// Blocks 1-4 are holes.
	for hb := int64(1); hb <= 4; hb++ {
		hole := readFile(t, b, ino, hb*bs, bs)
		for i, v := range hole {
			if v != 0 {
				t.Fatalf("hole block %d byte %d: want 0, got %d", hb, i, v)
			}
		}
	}

	fsckClean(t, b, img)
}

// bytesEqual is a small helper to avoid pulling in bytes/reflect for hot loops.
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}