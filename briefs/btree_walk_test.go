package briefs

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// buildBtreeImage builds a B+ tree for @want extents, writes it to a temp
// image, and returns the open file, the root block, and the list of all node
// blocks (leaves then idx nodes). It is a shared helper for the WalkBtree tests.
func buildBtreeImage(t *testing.T, want []Extent) (f *os.File, root uint64, allBlocks []uint64) {
	t.Helper()
	const blockSize = uint64(4096)
	const dataRegionStart = uint64(1000)

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
	rootBlock, _, idxBlocks, idxBufs, err := BuildBtreeIndex(leafBlocks, leafFirst, blockSize, 1, allocBlock)
	if err != nil {
		t.Fatalf("BuildBtreeIndex: %v", err)
	}
	all := append(append([]uint64{}, leafBlocks...), idxBlocks...)
	allBufs := append(append([][]byte{}, leafBufs...), idxBufs...)

	imgPath := filepath.Join(t.TempDir(), "btree_walk.briefs")
	ff, err := os.OpenFile(imgPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		t.Fatalf("create image: %v", err)
	}
	t.Cleanup(func() { ff.Close() })
	if err := ff.Truncate(int64(5000 * blockSize)); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if err := WriteBtreeNodes(ff, all, allBufs, blockSize); err != nil {
		t.Fatalf("WriteBtreeNodes: %v", err)
	}
	if err := ff.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}
	return ff, rootBlock, all
}

// TestWalkBtreeStrict collects every extent and node of a healthy tree through
// WalkBtree with the strict policy (VerifyCRC, no tolerance) and confirms the
// result matches the input extents and the full node set. This is the
// happy-path parity check against the production IterateInodeExtents reader.
func TestWalkBtreeStrict(t *testing.T) {
	const blockSize = uint64(4096)
	const nExtents = 300
	want := make([]Extent, nExtents)
	for i := 0; i < nExtents; i++ {
		want[i] = Extent{Offset: uint64(i) * 2, Phys: uint64(i) + 1, Len: 1}
	}

	f, root, allBlocks := buildBtreeImage(t, want)

	var got []Extent
	var nodes []uint64
	err := WalkBtree(f, root, BtreeWalkOptions{BlockSize: blockSize, VerifyCRC: true}, BtreeNodeVisitor{
		VisitNode: func(info BtreeNodeInfo) error { nodes = append(nodes, info.Block); return nil },
		VisitLeaf: func(info BtreeNodeInfo, extents []Extent) error {
			got = append(got, extents...)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("WalkBtree: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("extents mismatch: got %d, want %d", len(got), len(want))
	}
	if !reflect.DeepEqual(sortUint64s(nodes), sortUint64s(allBlocks)) {
		t.Fatalf("node set mismatch:\n got=%v\nwant=%v", nodes, allBlocks)
	}
}

// TestWalkBtreeTolerantCorruptLeaf corrupts one leaf's checksum and confirms
// the tolerant policy: WalkBtree reports the fault via OnFault, skips the
// corrupt subtree (its block is NOT visited and its extents NOT collected),
// but still collects the surviving leaves' extents. lost is set and the
// corrupt leaf's block is absent from the visited node set. This is the policy
// fsck's btreeCollectExtents relies on (Phase 4 recovery).
func TestWalkBtreeTolerantCorruptLeaf(t *testing.T) {
	const blockSize = uint64(4096)
	const nExtents = 300 // 3 leaves of 126/126/48
	want := make([]Extent, nExtents)
	for i := 0; i < nExtents; i++ {
		want[i] = Extent{Offset: uint64(i) * 2, Phys: uint64(i) + 1, Len: 1}
	}

	f, root, allBlocks := buildBtreeImage(t, want)

	// Corrupt the first leaf's checksum field (offset BtreeChecksumOffset).
	// allBlocks[0] is the first leaf. Flip a byte in the checksum without
	// touching the checksummed region so the recomputed CRC no longer matches.
	leaf0 := allBlocks[0]
	buf := make([]byte, blockSize)
	if _, err := f.ReadAt(buf, int64(leaf0*blockSize)); err != nil {
		t.Fatalf("read leaf: %v", err)
	}
	buf[BtreeChecksumOffset] ^= 0xFF
	if _, err := f.WriteAt(buf, int64(leaf0*blockSize)); err != nil {
		t.Fatalf("write corrupt leaf: %v", err)
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}

	var got []Extent
	var nodes []uint64
	var faultBlocks []uint64
	err := WalkBtree(f, root, BtreeWalkOptions{
		BlockSize: blockSize,
		VerifyCRC: true,
		Tolerant:  true,
		OnFault:   func(block uint64, e error) { faultBlocks = append(faultBlocks, block) },
	}, BtreeNodeVisitor{
		VisitNode: func(info BtreeNodeInfo) error { nodes = append(nodes, info.Block); return nil },
		VisitLeaf: func(info BtreeNodeInfo, extents []Extent) error {
			got = append(got, extents...)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("WalkBtree tolerant: %v", err)
	}
	// The corrupt leaf must be reported as a fault.
	if len(faultBlocks) == 0 || faultBlocks[0] != leaf0 {
		t.Fatalf("expected fault on leaf %d, got %v", leaf0, faultBlocks)
	}
	// The corrupt leaf must NOT be in the visited node set (skipped on fault).
	for _, n := range nodes {
		if n == leaf0 {
			t.Fatalf("corrupt leaf %d was visited despite checksum fault", leaf0)
		}
	}
	// The surviving two leaves' extents (126 + 48 = 174) must still be collected.
	if len(got) != nExtents-126 {
		t.Fatalf("collected extents: got %d, want %d (first leaf lost)", len(got), nExtents-126)
	}
	// got must be a suffix of want (the corrupt leaf held offsets 0..250).
	for i, e := range got {
		if !reflect.DeepEqual(e, want[126+i]) {
			t.Fatalf("collected extent %d mismatch:\n got=%+v\nwant=%+v", i, e, want[126+i])
		}
	}
}

// TestWalkBtreeStrictAbortsOnBadMagic confirms a strict walk returns an error
// (rather than silently skipping) when the root itself has a bad magic, and
// that no node is visited past the fault.
func TestWalkBtreeStrictAbortsOnBadMagic(t *testing.T) {
	const blockSize = uint64(4096)
	f, err := os.OpenFile(filepath.Join(t.TempDir(), "bad.briefs"), os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { f.Close() })
	if err := f.Truncate(int64(2 * blockSize)); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	// Write a zero-filled (magic 0) node at block 1 (root 0 is the null-child
	// sentinel, so the bad node must live at a non-zero block).
	visited := false
	err = WalkBtree(f, 1, BtreeWalkOptions{BlockSize: blockSize, VerifyCRC: true}, BtreeNodeVisitor{
		VisitNode: func(BtreeNodeInfo) error { visited = true; return nil },
		VisitLeaf: func(BtreeNodeInfo, []Extent) error { visited = true; return nil },
	})
	if err == nil {
		t.Fatal("expected error from strict walk on bad-magic root")
	}
	if visited {
		t.Fatal("visitor fired on a bad-magic root under the strict policy")
	}
}

func sortUint64s(s []uint64) []uint64 {
	out := append([]uint64(nil), s...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}