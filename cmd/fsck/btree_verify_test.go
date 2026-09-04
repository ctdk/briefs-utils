package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/ctdk/briefs-utils/briefs"
)

// buildMultiLevelTree builds a 300-extent B+ tree (3 leaves + 1 level-1 idx
// root), writes it to a temp image, and returns the open file, the root block,
// the leaf blocks, and the idx-root block. It is the fixture for the
// verifySubtree-via-WalkBtree structural tests.
func buildMultiLevelTree(t *testing.T) (f *os.File, root uint64, leafBlocks, idxBlocks []uint64) {
	t.Helper()
	const blockSize = uint64(4096)
	const dataRegionStart = uint64(1000)
	const nExtents = 300

	want := make([]briefs.Extent, nExtents)
	for i := 0; i < nExtents; i++ {
		want[i] = briefs.Extent{Offset: uint64(i) * 2, Phys: uint64(i) + 1, Len: 1}
	}
	alloc := briefs.NewAllocBuilder(5000)
	allocBlock := func() (uint64, error) {
		rel, err := alloc.AllocateBlock()
		if err != nil {
			return 0, err
		}
		return rel + dataRegionStart, nil
	}
	lb, leafFirst, leafBufs, err := briefs.BuildBtreeLeaves(want, blockSize, allocBlock)
	if err != nil {
		t.Fatalf("BuildBtreeLeaves: %v", err)
	}
	if len(lb) != 3 {
		t.Fatalf("expected 3 leaves, got %d", len(lb))
	}
	rootBlock, _, ib, idxBufs, err := briefs.BuildBtreeIndex(lb, leafFirst, blockSize, 1, allocBlock)
	if err != nil {
		t.Fatalf("BuildBtreeIndex: %v", err)
	}
	if len(ib) != 1 {
		t.Fatalf("expected 1 idx root, got %d", len(ib))
	}
	all := append(append([]uint64{}, lb...), ib...)
	allBufs := append(append([][]byte{}, leafBufs...), idxBufs...)

	imgPath := filepath.Join(t.TempDir(), "btree_verify.briefs")
	ff, err := os.OpenFile(imgPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		t.Fatalf("create image: %v", err)
	}
	t.Cleanup(func() { ff.Close() })
	if err := ff.Truncate(int64(5000 * blockSize)); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if err := briefs.WriteBtreeNodes(ff, all, allBufs, blockSize); err != nil {
		t.Fatalf("WriteBtreeNodes: %v", err)
	}
	if err := ff.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}
	return ff, rootBlock, lb, ib
}

// TestVerifyBtreeStructuresHealthy runs the structural-verify visitor over a
// healthy multi-level tree and confirms it accepts the tree (no error) and
// tallies all 300 extents. This is the parity check that the WalkBtree-based
// verify path handles a real internal-node + multi-leaf tree the same way the
// hand-rolled verifySubtree did.
func TestVerifyBtreeStructuresHealthy(t *testing.T) {
	const blockSize = uint64(4096)
	f, root, _, _ := buildMultiLevelTree(t)

	s := &btreeVerifyState{}
	err := briefs.WalkBtree(f, root, briefs.BtreeWalkOptions{
		BlockSize:        blockSize,
		NullChildIsFault: true,
	}, briefs.BtreeNodeVisitor{
		VisitNode: s.visitNode,
		VisitLeaf: s.visitLeaf,
	})
	if err != nil {
		t.Fatalf("verify healthy tree: %v", err)
	}
	if s.count != 300 {
		t.Fatalf("extent tally: got %d, want 300", s.count)
	}
}

// TestVerifyBtreeStructuresBadHighKey corrupts one separator high_key of the
// idx root to 0 (separators must be > 0), recomputes the node checksum so the
// basic walk would still accept it, and confirms the verify visitor flags the
// structural fault (ErrBtreeBadHighKey) — the check WalkBtree does not perform.
func TestVerifyBtreeStructuresBadHighKey(t *testing.T) {
	const blockSize = uint64(4096)
	f, root, _, idxBlocks := buildMultiLevelTree(t)

	// Zero idx[0].HighKey of the idx root. The idx root is idxBlocks[0]; its
	// first entry's HighKey is at BtreeHeaderSize + 0*16 + 8.
	idxRoot := idxBlocks[0]
	buf := make([]byte, blockSize)
	if _, err := f.ReadAt(buf, int64(idxRoot*blockSize)); err != nil {
		t.Fatalf("read idx root: %v", err)
	}
	// HighKey sits 8 bytes into the 16-byte idx entry.
	off := briefs.BtreeHeaderSize + 8
	for i := off; i < off+8; i++ {
		buf[i] = 0
	}
	// Recompute the checksum so the structural fault is the only defect: the
	// basic extent walk (which verifies CRC) would otherwise reject the node
	// first and the structural verify path would never run.
	briefs.SetBtreeNodeChecksum(buf, blockSize)
	if _, err := f.WriteAt(buf, int64(idxRoot*blockSize)); err != nil {
		t.Fatalf("write idx root: %v", err)
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}

	s := &btreeVerifyState{}
	err := briefs.WalkBtree(f, root, briefs.BtreeWalkOptions{
		BlockSize:        blockSize,
		NullChildIsFault: true,
	}, briefs.BtreeNodeVisitor{
		VisitNode: s.visitNode,
		VisitLeaf: s.visitLeaf,
	})
	if err == nil {
		t.Fatal("expected ErrBtreeBadHighKey from zero separator, got nil")
	}
}

// TestVerifyBtreeNodeChecksumZeroLegacy pins the kernel-parity legacy
// exemption: a stored checksum of 0 verifies (briefs_verify_chain_checksum,
// briefs.h:631-638, used verbatim by the kernel's B-tree read path,
// btree.c:145) while a wrong non-zero checksum is a real fault.  The zero
// case is also exercised end-to-end through the leaf extent walk below.
func TestVerifyBtreeNodeChecksumZeroLegacy(t *testing.T) {
	const blockSize = uint64(4096)
	f, _, leafBlocks, _ := buildMultiLevelTree(t)

	buf := make([]byte, blockSize)
	if _, err := f.ReadAt(buf, int64(leafBlocks[0]*blockSize)); err != nil {
		t.Fatalf("read leaf0: %v", err)
	}
	if err := briefs.VerifyBtreeNodeChecksum(buf, blockSize); err != nil {
		t.Fatalf("healthy leaf: %v", err)
	}
	binary.LittleEndian.PutUint64(buf[briefs.BtreeChecksumOffset:], 0)
	if err := briefs.VerifyBtreeNodeChecksum(buf, blockSize); err != nil {
		t.Fatalf("zero checksum must verify as legacy: %v", err)
	}
	binary.LittleEndian.PutUint64(buf[briefs.BtreeChecksumOffset:], 0xDEADBEEF)
	if err := briefs.VerifyBtreeNodeChecksum(buf, blockSize); err == nil {
		t.Fatal("wrong non-zero checksum must not verify")
	}

	// The extent walk (VerifyCRC path) accepts the zero-checksum node the
	// same way the kernel accepts it.
	binary.LittleEndian.PutUint64(buf[briefs.BtreeChecksumOffset:], 0)
	if _, err := f.WriteAt(buf, int64(leafBlocks[0]*blockSize)); err != nil {
		t.Fatalf("write leaf0: %v", err)
	}
	if err := briefs.WalkBtree(f, leafBlocks[0], briefs.BtreeWalkOptions{
		BlockSize:        blockSize,
		VerifyCRC:        true,
		NullChildIsFault: true,
	}, briefs.BtreeNodeVisitor{}); err != nil {
		t.Fatalf("walk with legacy zero checksum: %v", err)
	}
}

// TestVerifyBtreeStructuresHighKeyInfinity pins the documented high_key==0
// convention (kernel briefs.h struct briefs_btree_idx_entry: "0 => +inf
// (rightmost)"): the final separator may be +inf, a mid-array zero separator
// is still a fault (TestVerifyBtreeStructuresBadHighKey covers that one).
func TestVerifyBtreeStructuresHighKeyInfinity(t *testing.T) {
	const blockSize = uint64(4096)
	f, root, _, idxBlocks := buildMultiLevelTree(t)

	// The idx root has 2 separators (3 leaves); zero the LAST one's HighKey
	// (idx entry i: child at +0, high_key at +8).
	idxRoot := idxBlocks[0]
	buf := make([]byte, blockSize)
	if _, err := f.ReadAt(buf, int64(idxRoot*blockSize)); err != nil {
		t.Fatalf("read idx root: %v", err)
	}
	off := briefs.BtreeHeaderSize + 1*16 + 8
	binary.LittleEndian.PutUint64(buf[off:], 0)
	// Recompute the checksum so the structural accept/reject is the only
	// thing under test.
	briefs.SetBtreeNodeChecksum(buf, blockSize)
	if _, err := f.WriteAt(buf, int64(idxRoot*blockSize)); err != nil {
		t.Fatalf("write idx root: %v", err)
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}

	s := &btreeVerifyState{}
	err := briefs.WalkBtree(f, root, briefs.BtreeWalkOptions{
		BlockSize:        blockSize,
		VerifyCRC:        true,
		NullChildIsFault: true,
	}, briefs.BtreeNodeVisitor{
		VisitNode: s.visitNode,
		VisitLeaf: s.visitLeaf,
	})
	if err != nil {
		t.Fatalf("rightmost +inf separator must verify: %v", err)
	}
	if s.count != 300 {
		t.Fatalf("extent tally: got %d, want 300", s.count)
	}
}

// TestVerifyBtreeStructuresCrossLeafUnsorted corrupts the second leaf's first
// extent offset so it falls at or below the previous leaf's max offset, and
// confirms the verify visitor flags the cross-leaf ordering fault
// (ErrBtreeCrossLeafUnsorted) — the cursor check that depends on leaves being
// visited in ascending order, which WalkBtree guarantees.
func TestVerifyBtreeStructuresCrossLeafUnsorted(t *testing.T) {
	const blockSize = uint64(4096)
	f, root, leafBlocks, _ := buildMultiLevelTree(t)

	// Leaf 0 holds extents at offsets 0,2,...,250 (max 250). Leaf 1's first
	// extent is at offset 252. Rewrite leaf 1's first extent to offset 200
	// (<= 250) so the cross-leaf invariant fails while within-leaf order still
	// holds (200 < 252 < 254 < ...). VerifyCRC is false on the verify path, so
	// the stale checksum does not mask the structural fault.
	leaf1 := leafBlocks[1]
	buf := make([]byte, blockSize)
	if _, err := f.ReadAt(buf, int64(leaf1*blockSize)); err != nil {
		t.Fatalf("read leaf1: %v", err)
	}
	extOff := briefs.BtreeHeaderSize // first extent, offset field
	for i := extOff; i < extOff+8; i++ {
		buf[i] = 0
	}
	buf[extOff] = 200 // offset = 200 (little-endian)
	if _, err := f.WriteAt(buf, int64(leaf1*blockSize)); err != nil {
		t.Fatalf("write leaf1: %v", err)
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}

	s := &btreeVerifyState{}
	err := briefs.WalkBtree(f, root, briefs.BtreeWalkOptions{
		BlockSize:        blockSize,
		NullChildIsFault: true,
	}, briefs.BtreeNodeVisitor{
		VisitNode: s.visitNode,
		VisitLeaf: s.visitLeaf,
	})
	if err == nil {
		t.Fatal("expected ErrBtreeCrossLeafUnsorted, got nil")
	}
}
