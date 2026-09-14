package main

import (
	"fmt"

	"github.com/ctdk/briefs-utils/briefs"
)

// btreeVerifyState carries the structural-check accumulators through the
// single WalkBtree pass that collectInodeExtents runs per tree-backed inode.
// It supplies only the checks WalkBtree itself does not perform:
//
//   - separator high_keys are strictly ascending and > 0 (per internal node),
//   - no null child pointer (the walk options leave NullChildIsFault unset so
//     the engine skips one silently; the visitor scans the parent and flags
//     it instead, keeping usedBlocks complete),
//   - every child sits one level below its parent (leaf == 0; internal == parent-1),
//   - extents are ordered across leaf boundaries (last offset of leaf N <
//     first offset of leaf N+1, visiting leaves in idx-descent order),
//   - the walked extent count equals the inode's num_extents_total.
//
// Child-pointer range and checksum/magic validity are NOT re-checked here:
// WalkBtree (run with VerifyCRC) already validated every reachable child
// (magic, CRC, read) before the visitor runs, so any child we descend to is a
// valid in-range node. Re-checking range against dataBlockCount here would
// only fire if the allocator header itself were corrupt — flagging a healthy
// tree as failed.
//
// The descent goes down the idx tree exactly the way the kernel iterates
// extents (briefs_btree.c: btree_walk_descend), NOT via the next_leaf chain:
// the kernel tolerates dangling next_leaf links to freed-and-dropped leaves
// after range deletes, so the leaf chain is not a reliable invariant and is
// deliberately not checked here.
//
// Structural faults are recorded here (first one only) instead of returned,
// so the walk completes and the block cross-reference keeps a full usedBlocks
// set; collectInodeExtents then fails the inode, so the Phase 1 repair guard
// refuses --repair (the tree is corrupt and the unreached-or-misrecorded
// blocks must not be freed).
type btreeVerifyState struct {
	fs  *fsckState
	ino uint64

	// fault holds the first structural fault recorded during the walk.
	fault error
	// count tallies extents across every visited leaf.
	count int
	// prevLeafMax is the max extent offset of the most recently visited leaf;
	// havePrev is false until the first non-empty leaf is visited. Used to
	// enforce cross-leaf key ordering as leaves are visited left-to-right.
	prevLeafMax uint64
	havePrev    bool
}

// recordFault keeps the first structural fault and drops later ones: the
// first is what collectInodeExtents reports and fails the inode with.
func (s *btreeVerifyState) recordFault(err error) {
	if s.fault == nil {
		s.fault = err
	}
}

// visitNode applies the per-node structural checks WalkBtree does not: level
// ordering (for both leaves and internal nodes), null child pointers, and,
// for internal nodes, strictly-ascending non-zero separator high_keys. It is
// called for every node after WalkBtree has already validated magic, fanout,
// and (for leaves) within-leaf ordering.
func (s *btreeVerifyState) visitNode(info briefs.BtreeNodeInfo) error {
	hdr := info.Hdr
	if hdr.IsLeaf() {
		// Leaves are level 0.
		if hdr.Level != 0 {
			return fmt.Errorf("btree node %d: %w (leaf with level %d, want 0)",
				info.Block, briefs.ErrBtreeBadChild, hdr.Level)
		}
		return nil
	}

	// Internal node.
	if hdr.Level == 0 {
		return fmt.Errorf("btree node %d: %w (internal node with level 0)",
			info.Block, briefs.ErrBtreeBadChild)
	}
	// A non-root internal node must be exactly one level below its parent.
	if !info.IsRoot && hdr.Level != info.ExpectedLevel {
		return fmt.Errorf("btree node %d: %w (level %d, want %d)",
			info.Block, briefs.ErrBtreeBadChild, hdr.Level, info.ExpectedLevel)
	}

	// Null child pointers. The engine skips a null child silently (the kernel
	// reader does too), so the walk is run without NullChildIsFault and this
	// scan flags the parent instead — the fault is recorded while the rest of
	// the tree still gets walked and recorded into usedBlocks.
	for i := uint16(0); i < hdr.NumKeys; i++ {
		e := briefs.ReadBtreeIdxEntry(info.Buf, int(i))
		if e.Child == 0 {
			return fmt.Errorf("btree node %d: %w (null child pointer, idx[%d])",
				info.Block, briefs.ErrBtreeBadChild, i)
		}
	}
	if briefs.BtreeTrailingChild(info.Buf) == 0 {
		return fmt.Errorf("btree node %d: %w (null child pointer, trailing child)",
			info.Block, briefs.ErrBtreeBadChild)
	}

	// Separator high_keys must be strictly ascending. Separators are real
	// extent offsets (the first offset of each right sibling), so a
	// non-ascending separator is corruption. A high_key of 0 is documented
	// on disk as +inf for the rightmost separator (kernel briefs.h struct
	// briefs_btree_idx_entry: "0 => +inf (rightmost)"), so it is accepted as
	// the final entry, ending the ascending chain; anywhere else it is a
	// fault (the kernel's lookup compares key < high_key, so a mid-array
	// zero separator makes that child unreachable).
	var prevHigh uint64
	for i := uint16(0); i < hdr.NumKeys; i++ {
		e := briefs.ReadBtreeIdxEntry(info.Buf, int(i))
		if e.HighKey == 0 {
			if i != hdr.NumKeys-1 {
				return fmt.Errorf("btree node %d: %w (idx[%d].high_key=0 before the last separator; +inf is only valid rightmost)",
					info.Block, briefs.ErrBtreeBadHighKey, i)
			}
			break // +inf rightmost separator; ascending chain ends here
		}
		if i > 0 && e.HighKey <= prevHigh {
			return fmt.Errorf("btree node %d: %w (idx[%d].high_key=%d after %d)",
				info.Block, briefs.ErrBtreeBadHighKey, i, e.HighKey, prevHigh)
		}
		prevHigh = e.HighKey
	}
	return nil
}

// visitLeaf enforces cross-leaf key ordering (this leaf's first offset must
// exceed the previous leaf's max offset) and adds the leaf's extent count to
// the running tally. Empty leaves do not advance the cursor.
func (s *btreeVerifyState) visitLeaf(info briefs.BtreeNodeInfo, extents []briefs.Extent) error {
	if len(extents) > 0 {
		first := extents[0].Offset
		if s.havePrev && first <= s.prevLeafMax {
			return fmt.Errorf("btree node %d: %w (leaf first offset %d <= previous leaf max %d)",
				info.Block, briefs.ErrBtreeCrossLeafUnsorted, first, s.prevLeafMax)
		}
		s.prevLeafMax = extents[len(extents)-1].Offset
		s.havePrev = true
	}
	s.count += len(extents)
	return nil
}
