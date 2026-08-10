package main

import (
	"fmt"

	"github.com/ctdk/briefs-utils/briefs"
)

// verifyBtreeStructures runs the read-only B+ tree structural checks that the
// basic extent walk (briefs.IterateInodeExtents / btreeWalk) does NOT perform:
//
//   - separator high_keys are strictly ascending and > 0 (per internal node),
//   - no null child pointer (btreeWalk silently skips a 0 child; we flag it),
//   - every child sits one level below its parent (leaf == 0; internal == parent-1),
//   - extents are ordered across leaf boundaries (last offset of leaf N <
//     first offset of leaf N+1, visiting leaves in idx-descent order),
//   - the walked extent count equals the inode's num_extents_total.
//
// Child-pointer range and checksum/magic validity are NOT re-checked here:
// btreeWalk already validated every reachable child (magic, CRC, read) before
// this function runs, so any child we descend to is a valid in-range node.
// Re-checking range against dataBlockCount here would only fire if the
// allocator header itself were corrupt — flagging a healthy tree as failed.
//
// It descends the idx tree exactly the way the kernel iterates extents
// (briefs_btree.c: btree_walk_descend), NOT via the next_leaf chain: the kernel
// tolerates dangling next_leaf links to freed-and-dropped leaves after range
// deletes, so the leaf chain is not a reliable invariant and is deliberately
// not checked here.
//
// The descent skeleton (read/magic/fanout/within-leaf sort + null-child and
// cycle guards) is shared with the extent-collection walk via briefs.WalkBtree;
// this caller supplies only the structural checks WalkBtree does not: level
// ordering, separator high-key ordering, the cross-leaf cursor, and the
// extent-count tally. Checksum is not re-verified (VerifyCRC is false) because
// the prior collect pass already validated the tree.
//
// This runs only for tree-backed inodes (InodeFlagIndexed) whose basic walk
// already succeeded (not in fs.failedBtreeInos). A structural failure sets
// fs.failedBtreeInos[ino] so the Phase 1 repair guard refuses --repair (the
// tree is corrupt and the unreached-or-misrecorded blocks must not be freed).
func verifyBtreeStructures(fs *fsckState, ino uint64, in *briefs.Inode, blockSize uint64) {
	if in.Flags&briefs.InodeFlagIndexed == 0 {
		return // inline-only / inline-data: no B-tree to check
	}
	if fs.failedBtreeInos[ino] {
		return // basic walk already failed; tree is torn, nothing deeper to check
	}
	root := in.ExtentInlineBase
	if root == 0 {
		return // verifyInode rejects this, but guard anyway
	}

	s := &btreeVerifyState{fs: fs, ino: ino}
	err := briefs.WalkBtree(fs.file, root, briefs.BtreeWalkOptions{
		BlockSize:        blockSize,
		NullChildIsFault: true,
		// Tolerant stays false: the first structural fault aborts the walk and
		// marks the inode failed, exactly as the hand-rolled descent did.
	}, briefs.BtreeNodeVisitor{
		VisitNode: s.visitNode,
		VisitLeaf: s.visitLeaf,
	})
	if err != nil {
		fs.errorf("ino %d: %v", ino, err)
		fs.failedBtreeInos[ino] = true
		return
	}
	if uint64(s.count) != in.NumExtentsTotal {
		fs.errorf("ino %d: %v (walked %d, num_extents_total %d)",
			ino, briefs.ErrBtreeCountMismatch, s.count, in.NumExtentsTotal)
		fs.failedBtreeInos[ino] = true
	}
}

// btreeVerifyState carries the cross-leaf ordering cursor and the extent-count
// tally through the WalkBtree callbacks. The cycle guard lives in WalkBtree,
// so this state holds only the verify-specific accumulators.
type btreeVerifyState struct {
	fs  *fsckState
	ino uint64

	// count tallies extents across every visited leaf.
	count int
	// prevLeafMax is the max extent offset of the most recently visited leaf;
	// havePrev is false until the first non-empty leaf is visited. Used to
	// enforce cross-leaf key ordering as leaves are visited left-to-right.
	prevLeafMax uint64
	havePrev     bool
}

// visitNode applies the per-node structural checks WalkBtree does not: level
// ordering (for both leaves and internal nodes) and, for internal nodes,
// strictly-ascending non-zero separator high_keys. It is called for every node
// after WalkBtree has already validated magic, fanout, and (for leaves)
// within-leaf ordering.
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

	// Separator high_keys must be strictly ascending and > 0. Separators are
	// real extent offsets (the first offset of each right sibling), so a
	// non-ascending or zero separator is corruption.
	var prevHigh uint64
	for i := uint16(0); i < hdr.NumKeys; i++ {
		e := briefs.ReadBtreeIdxEntry(info.Buf, int(i))
		if e.HighKey == 0 {
			return fmt.Errorf("btree node %d: %w (idx[%d].high_key=0; separators must be > 0)",
				info.Block, briefs.ErrBtreeBadHighKey, i)
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