package briefs

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestBtreeBuildRoundTrip builds a 300-extent B+ tree from scratch via
// BuildBtreeLeaves + BuildBtreeIndex, writes it to a temp image, and reads it
// back through the production reader (IterateInodeExtents / btreeWalk) to confirm
// the builders produce a tree the reader accepts, with extents in ascending
// offset order and hole extents preserved verbatim.
func TestBtreeBuildRoundTrip(t *testing.T) {
	const blockSize = uint64(4096)
	const dataRegionStart = uint64(1000)
	const nExtents = 300

	// Build extents at offsets 0,2,4,...,598 (gaps, no holes yet). Phys distinct
	// from node-block abs (1..300) so the round trip is unambiguous.
	want := make([]Extent, nExtents)
	for i := 0; i < nExtents; i++ {
		want[i] = Extent{Offset: uint64(i) * 2, Phys: uint64(i) + 1, Len: 1}
	}

	alloc := NewAllocBuilder(5000)
	allocBlock := func() (uint64, error) {
		rel, err := alloc.AllocateBlock()
		if err != nil {
			return 0, err
		}
		return rel + dataRegionStart, nil
	}

	leafBlocks, leafFirstOffsets, leafBufs, err := BuildBtreeLeaves(want, blockSize, allocBlock)
	if err != nil {
		t.Fatalf("BuildBtreeLeaves: %v", err)
	}
	if len(leafBlocks) != 3 { // ceil(300/126) = 3
		t.Fatalf("expected 3 leaves, got %d", len(leafBlocks))
	}
	rootBlock, _, idxBlocks, idxBufs, err := BuildBtreeIndex(leafBlocks, leafFirstOffsets, blockSize, 1, allocBlock)
	if err != nil {
		t.Fatalf("BuildBtreeIndex: %v", err)
	}
	// 3 leaves -> 1 idx root (level 1); no further levels.
	if len(idxBlocks) != 1 {
		t.Fatalf("expected 1 idx node, got %d", len(idxBlocks))
	}

	// Write all nodes to a temp image large enough to hold them.
	imgPath := filepath.Join(t.TempDir(), "btree_build.briefs")
	f, err := os.OpenFile(imgPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		t.Fatalf("create image: %v", err)
	}
	if err := f.Truncate(int64(5000 * blockSize)); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	allBlocks := append(append([]uint64{}, leafBlocks...), idxBlocks...)
	allBufs := append(append([][]byte{}, leafBufs...), idxBufs...)
	if err := WriteBtreeNodes(f, allBlocks, allBufs, blockSize); err != nil {
		t.Fatalf("WriteBtreeNodes: %v", err)
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}

	// Read back through the production walker.
	in := &Inode{
		ExtentInlineBase: rootBlock,
		NumExtentsTotal:  uint64(nExtents),
		Flags:            InodeFlagIndexed,
	}
	var got []Extent
	if err := IterateInodeExtents(f, in, blockSize, InodeExtentVisitor{
		VisitExtent: func(e Extent) error { got = append(got, e); return nil },
	}); err != nil {
		t.Fatalf("IterateInodeExtents: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round-trip mismatch: got %d extents, want %d", len(got), len(want))
	}

	// Verify every node checksums cleanly.
	for i, b := range allBlocks {
		buf := make([]byte, blockSize)
		if _, err := f.ReadAt(buf, int64(b*blockSize)); err != nil {
			t.Fatalf("read node %d: %v", b, err)
		}
		if err := VerifyBtreeNodeChecksum(buf, blockSize); err != nil {
			t.Fatalf("node %d (idx %d) checksum invalid: %v", b, i, err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// TestBtreeBuildSingleLeafAndHoles covers two edge cases:
//   - A tree with <=126 extents builds to a single leaf that IS the root (no idx
//     level), and the reader walks it as a root-leaf.
//   - Hole extents (ExtentFlagHole, Phys=0) are packed and read back verbatim.
func TestBtreeBuildSingleLeafAndHoles(t *testing.T) {
	const blockSize = uint64(4096)
	const dataRegionStart = uint64(1000)

	// 10 extents, two of which are holes at offsets 3 and 7.
	want := []Extent{
		{Offset: 0, Phys: 1, Len: 1},
		{Offset: 1, Phys: 2, Len: 1},
		{Offset: 2, Phys: 3, Len: 1},
		{Offset: 3, Phys: 0, Len: 1, Flags: ExtentFlagHole},
		{Offset: 4, Phys: 4, Len: 1},
		{Offset: 5, Phys: 5, Len: 1},
		{Offset: 6, Phys: 6, Len: 1},
		{Offset: 7, Phys: 0, Len: 1, Flags: ExtentFlagHole},
		{Offset: 8, Phys: 7, Len: 1},
		{Offset: 9, Phys: 8, Len: 1},
	}

	alloc := NewAllocBuilder(5000)
	allocBlock := func() (uint64, error) {
		rel, err := alloc.AllocateBlock()
		if err != nil {
			return 0, err
		}
		return rel + dataRegionStart, nil
	}
	leafBlocks, leafFirst, leafBufs, err := BuildBtreeLeaves(want, blockSize, allocBlock)
	if err != nil {
		t.Fatalf("BuildBtreeLeaves: %v", err)
	}
	if len(leafBlocks) != 1 {
		t.Fatalf("expected 1 leaf, got %d", len(leafBlocks))
	}
	rootBlock, _, idxBlocks, idxBufs, err := BuildBtreeIndex(leafBlocks, leafFirst, blockSize, 1, allocBlock)
	if err != nil {
		t.Fatalf("BuildBtreeIndex: %v", err)
	}
	if rootBlock != leafBlocks[0] || len(idxBlocks) != 0 || len(idxBufs) != 0 {
		t.Fatalf("single-leaf tree must have the leaf as root with no idx nodes")
	}

	imgPath := filepath.Join(t.TempDir(), "btree_holes.briefs")
	f, err := os.OpenFile(imgPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := f.Truncate(int64(5000 * blockSize)); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if err := WriteBtreeNodes(f, []uint64{rootBlock}, [][]byte{leafBufs[0]}, blockSize); err != nil {
		t.Fatalf("WriteBtreeNodes: %v", err)
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}

	in := &Inode{ExtentInlineBase: rootBlock, NumExtentsTotal: uint64(len(want)), Flags: InodeFlagIndexed}
	var got []Extent
	if err := IterateInodeExtents(f, in, blockSize, InodeExtentVisitor{
		VisitExtent: func(e Extent) error { got = append(got, e); return nil },
	}); err != nil {
		t.Fatalf("IterateInodeExtents: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("hole round-trip mismatch:\n got=%+v\nwant=%+v", got, want)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}