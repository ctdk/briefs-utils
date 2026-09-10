package fuse

// Regression tests for punch-hole boundary-block handling, ported from the
// kernel's briefs_do_punch_hole (file.c:1574).  The bridge's punchHole used to
// free whole blocks from off/bs, destroying the live bytes of a partially
// punched head/tail block — found by fsx (2026-09-10): PUNCH 0x10c2d-0x13468
// zeroed the whole head block 0x10000 including data below the punch start,
// and op 104's READ BAD DATA reported zeros where a copy_file_range had
// written.  The kernel keeps partially punched boundary blocks allocated and
// zeroes only their punched byte ranges; only interior, fully-covered blocks
// are freed.

import (
	"testing"

	"github.com/ctdk/briefs-utils/briefs"
)

// punchMultiBlockBoundary: punch [bs+100, 3*bs+200) out of a five-block file.
// The head block (partial start) and tail block (partial end) must keep their
// un-punched bytes; the fully-covered middle block becomes a hole.
func TestPunchMultiBlockBoundary(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)

	const rootIno = 1
	in, err := b.createInDir(rootIno, "f", briefs.ModeFile|0o644, 1000, 1000, false)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ino := in.InodeNumber

	bs := int64(b.blockSize)
	// Distinct pattern per block so a mis-offset copy is visible.
	blocks := make([][]byte, 5)
	for i := range blocks {
		blocks[i] = makePattern(i+1, int(bs))
		writeFile(t, b, ino, blocks[i], int64(i)*bs)
	}

	off := int64(bs) + 100
	size := int64(2*bs) + 100 // ends 200 bytes into block 3
	if err := b.fallocateOp(ino, uint64(off), uint64(size), fallocPunchHole|fallocKeepSize); err != nil {
		t.Fatalf("punch: %v", err)
	}

	got := readFile(t, b, ino, 0, 5*bs)

	// Block 0 untouched.
	if !bytesEqual(got[:bs], blocks[0]) {
		t.Fatal("block 0 corrupted by punch")
	}
	// Head boundary block 1: [0,100) intact, [100,bs) zeroed.
	if !bytesEqual(got[bs:bs+100], blocks[1][:100]) {
		t.Fatal("punch destroyed bytes BEFORE the punched range in the head block")
	}
	for i := bs + 100; i < 2*bs; i++ {
		if got[i] != 0 {
			t.Fatalf("head block byte %d: want 0 (punched), got %d", i-bs, got[i])
		}
	}
	// Fully-covered middle block 2: a hole, reads as zeros.
	for i := 2 * bs; i < 3*bs; i++ {
		if got[i] != 0 {
			t.Fatalf("middle block byte %d: want 0 (freed), got %d", i-2*bs, got[i])
		}
	}
	// Tail boundary block 3: [0,200) zeroed, [200,bs) intact.
	for i := 3 * bs; i < 3*bs+200; i++ {
		if got[i] != 0 {
			t.Fatalf("tail block byte %d: want 0 (punched), got %d", i-3*bs, got[i])
		}
	}
	if !bytesEqual(got[3*bs+200:4*bs], blocks[3][200:]) {
		t.Fatal("punch destroyed bytes AFTER the punched range in the tail block")
	}
	// Block 4 untouched.
	if !bytesEqual(got[4*bs:], blocks[4]) {
		t.Fatal("block 4 corrupted by punch")
	}
}

// punchSingleBlockBoundary: a punch wholly inside one block (the kernel's
// same_boundary_block case, file.c:1599) must zero only the sub-range — not
// the union of head and tail zeroing, which covers the whole block.
func TestPunchSingleBlockBoundary(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)

	in, err := b.createInDir(1, "f", briefs.ModeFile|0o644, 1000, 1000, false)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ino := in.InodeNumber

	bs := int64(b.blockSize)
	data := makePattern(7, int(3*bs))
	writeFile(t, b, ino, data, 0)

	off := bs + 100
	size := 100 // [bs+100, bs+200): entirely inside block 1
	if err := b.fallocateOp(ino, uint64(off), uint64(size), fallocPunchHole|fallocKeepSize); err != nil {
		t.Fatalf("punch: %v", err)
	}

	got := readFile(t, b, ino, 0, 3*bs)
	if !bytesEqual(got[:bs+100], data[:bs+100]) {
		t.Fatal("single-block punch destroyed bytes before the range")
	}
	for i := bs + 100; i < bs+200; i++ {
		if got[i] != 0 {
			t.Fatalf("punched byte %d: want 0, got %d", i-bs, got[i])
		}
	}
	if !bytesEqual(got[bs+200:], data[bs+200:]) {
		t.Fatal("single-block punch destroyed bytes after the range")
	}
}

// punchInline zeroes the sub-range of the inline region and nothing else.
func TestPunchInline(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)

	in, err := b.createInDir(1, "f", briefs.ModeFile|0o644, 1000, 1000, false)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ino := in.InodeNumber

	data := makePattern(3, 100)
	writeFile(t, b, ino, data, 0)
	di, _ := b.inodes.ReadInode(ino)
	if di.Flags&briefs.InodeFlagInlineData == 0 {
		t.Fatalf("expected inline data, flags=0x%x", di.Flags)
	}

	if err := b.fallocateOp(ino, 10, 20, fallocPunchHole|fallocKeepSize); err != nil {
		t.Fatalf("punch inline: %v", err)
	}

	got := readFile(t, b, ino, 0, 100)
	if !bytesEqual(got[:10], data[:10]) {
		t.Fatal("inline punch destroyed bytes before the range")
	}
	for i := 10; i < 30; i++ {
		if got[i] != 0 {
			t.Fatalf("inline punched byte %d: want 0, got %d", i, got[i])
		}
	}
	if !bytesEqual(got[30:], data[30:]) {
		t.Fatal("inline punch destroyed bytes after the range")
	}
	if di, _ = b.inodes.ReadInode(ino); di.FileSize != 100 {
		t.Fatalf("punch changed inline size: %d", di.FileSize)
	}
}

// punchHoleRange: a punch over an all-hole range is a no-op, and a
// block-aligned punch still frees whole blocks.
func TestPunchAlignedAndHoleRange(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)

	in, err := b.createInDir(1, "f", briefs.ModeFile|0o644, 1000, 1000, false)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ino := in.InodeNumber

	bs := int64(b.blockSize)
	data := makePattern(9, int(3*bs))
	writeFile(t, b, ino, data, 0)

	// Block-aligned punch of block 1: whole block freed, neighbours intact.
	if err := b.fallocateOp(ino, uint64(bs), uint64(bs), fallocPunchHole|fallocKeepSize); err != nil {
		t.Fatalf("aligned punch: %v", err)
	}
	got := readFile(t, b, ino, 0, 3*bs)
	if !bytesEqual(got[:bs], data[:bs]) {
		t.Fatal("aligned punch corrupted block 0")
	}
	for i := bs; i < 2*bs; i++ {
		if got[i] != 0 {
			t.Fatalf("aligned-punch byte %d: want 0, got %d", i-bs, got[i])
		}
	}
	if !bytesEqual(got[2*bs:], data[2*bs:]) {
		t.Fatal("aligned punch corrupted block 2")
	}

	// Punch the (now) hole again: no-op, no error.
	if err := b.fallocateOp(ino, uint64(bs), uint64(bs), fallocPunchHole|fallocKeepSize); err != nil {
		t.Fatalf("punch over hole: %v", err)
	}
	got = readFile(t, b, ino, 0, 3*bs)
	if !bytesEqual(got[:bs], data[:bs]) || !bytesEqual(got[2*bs:], data[2*bs:]) {
		t.Fatal("punch over hole corrupted neighbouring blocks")
	}
}
