package fuse

// Regression tests for the inline-data truncate family fixed 2026-09-11:
// truncateLocked used to run inline files through the generic extent path,
// where rebuildExtentIndex's SetInlineExtents([8]Extent{}) clobbered the
// inline data region (the inline_extents array and the inline_data region are
// the same 256 bytes), wiping the surviving head of a down-truncate; and a
// truncate-up past the region left InodeFlagInlineData set with FileSize >
// 256, panicking every later read/write that sliced the region by FileSize
// (the readFileData/promoteInlineData daemon panics, generic/551, 2026-09-11).

import (
	"context"
	"testing"

	"github.com/ctdk/briefs-utils/briefs"
)

// truncateTo is a helper that truncates ino and fatals on error.
func truncateTo(t *testing.T, b *BrieFS, ino uint64, newSize uint64) {
	t.Helper()
	if err := b.truncateInode(context.Background(), ino, newSize); err != nil {
		t.Fatalf("truncateInode ino %d to %d: %v", ino, newSize, err)
	}
}

// TestTruncateUpPromotesInline: an inline file truncated past the 256-byte
// region must promote to extent-backed, keeping its old bytes and reading
// zeros for the growth (kernel parity: truncate-up extends with a hole).
func TestTruncateUpPromotesInline(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)

	in, err := b.createInDir(1, "f", briefs.ModeFile|0o644, 1000, 1000, false, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ino := in.InodeNumber

	p0 := makePattern(1, 100)
	writeFile(t, b, ino, p0, 0)

	bs := uint64(b.blockSize)
	truncateTo(t, b, ino, bs)

	di, err := b.inodes.ReadInode(ino)
	if err != nil {
		t.Fatalf("ReadInode: %v", err)
	}
	if di.Flags&briefs.InodeFlagInlineData != 0 {
		t.Fatalf("truncate past inline cap left InodeFlagInlineData set (FileSize=%d)", di.FileSize)
	}
	if di.FileSize != bs {
		t.Fatalf("FileSize after truncate: got %d, want %d", di.FileSize, bs)
	}

	// Old bytes survive at offset 0; the growth reads as zeros.
	got := readFile(t, b, ino, 0, int64(bs))
	if !bytesEqual(got[:100], p0) {
		t.Fatal("promoted inline content corrupted by truncate-up")
	}
	for i := 100; i < int(bs); i++ {
		if got[i] != 0 {
			t.Fatalf("byte %d after old EOF: want 0 (hole), got %d", i, got[i])
		}
	}

	// The promoted inode must keep working as an extent file: write past
	// block 0, read it back.
	p1 := makePattern(2, 100)
	writeFile(t, b, ino, p1, int64(bs)-100)
	got = readFile(t, b, ino, int64(bs)-100, 100)
	if !bytesEqual(got, p1) {
		t.Fatal("post-promotion write lost")
	}

	fsckClean(t, b, img)
}

// TestTruncateInlineDownUpZeroesTail: bytes truncated away must not reappear
// when the file is truncated back up within the inline region — the region
// holds stale bytes below the cap that a bare FileSize update would expose.
func TestTruncateInlineDownUpZeroesTail(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)

	in, err := b.createInDir(1, "f", briefs.ModeFile|0o644, 1000, 1000, false, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ino := in.InodeNumber

	p0 := makePattern(1, 100)
	writeFile(t, b, ino, p0, 0)
	truncateTo(t, b, ino, 50)
	truncateTo(t, b, ino, 200)

	di, _ := b.inodes.ReadInode(ino)
	if di.Flags&briefs.InodeFlagInlineData == 0 {
		t.Fatal("200-byte truncate should have stayed inline")
	}

	got := readFile(t, b, ino, 0, 200)
	if !bytesEqual(got[:50], p0[:50]) {
		t.Fatal("truncated-away half survived")
	}
	for i := 50; i < 200; i++ {
		if got[i] != 0 {
			t.Fatalf("byte %d: stale byte from before the down-truncate reappeared", i)
		}
	}

	fsckClean(t, b, img)
}

// TestWriteInlineGapAfterTruncateDown: an inline write past EOF must zero the
// gap it exposes, not just copy at its offset — the gap can hold stale region
// bytes from before a down-truncate.
func TestWriteInlineGapAfterTruncateDown(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)

	in, err := b.createInDir(1, "f", briefs.ModeFile|0o644, 1000, 1000, false, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ino := in.InodeNumber

	p0 := makePattern(1, 100)
	writeFile(t, b, ino, p0, 0)
	truncateTo(t, b, ino, 50)
	p1 := makePattern(2, 10)
	writeFile(t, b, ino, p1, 150)

	got := readFile(t, b, ino, 0, 160)
	if !bytesEqual(got[:50], p0[:50]) {
		t.Fatal("pre-truncate bytes corrupted by the gap write")
	}
	for i := 50; i < 150; i++ {
		if got[i] != 0 {
			t.Fatalf("gap byte %d: want 0, got stale byte %d", i, got[i])
		}
	}
	if !bytesEqual(got[150:160], p1) {
		t.Fatal("gap write lost")
	}

	fsckClean(t, b, img)
}
