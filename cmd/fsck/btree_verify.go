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

	s := &btreeVerifyState{
		fs:        fs,
		ino:       ino,
		blockSize: blockSize,
		visited:   make(map[uint64]bool),
	}

	count, err := s.verifySubtree(root, 0, true)
	if err != nil {
		fs.errorf("ino %d: %v", ino, err)
		fs.failedBtreeInos[ino] = true
		return
	}
	if uint64(count) != in.NumExtentsTotal {
		fs.errorf("ino %d: %w (walked %d, num_extents_total %d)",
			ino, briefs.ErrBtreeCountMismatch, count, in.NumExtentsTotal)
		fs.failedBtreeInos[ino] = true
	}
}

// btreeVerifyState carries the cross-leaf ordering cursor and cycle guard
// through the recursive descent.
type btreeVerifyState struct {
	fs        *fsckState
	ino       uint64
	blockSize uint64
	visited   map[uint64]bool

	// prevLeafMax is the max extent offset of the most recently visited leaf;
	// havePrev is false until the first leaf is visited. Used to enforce
	// cross-leaf key ordering as leaves are visited left-to-right.
	prevLeafMax uint64
	havePrev     bool
}

// verifySubtree walks the subtree rooted at @block and returns the number of
// extents in it. @expectedLevel is the level the node should have (ignored when
// @isRoot, since the root's level depends on tree height). It returns an error
// (wrapped with the offending block) on any structural fault; the caller then
// records the inode as failed.
func (s *btreeVerifyState) verifySubtree(block uint64, expectedLevel uint16, isRoot bool) (int, error) {
	if block == 0 {
		return 0, fmt.Errorf("btree node %d: %w (null child pointer)", block, briefs.ErrBtreeBadChild)
	}
	if s.visited[block] {
		return 0, fmt.Errorf("btree node %d: %w", block, briefs.ErrBtreeCycle)
	}
	s.visited[block] = true

	buf := make([]byte, s.blockSize)
	if _, err := s.fs.file.ReadAt(buf, int64(block*s.blockSize)); err != nil {
		return 0, fmt.Errorf("btree node %d: read: %w", block, err)
	}
	hdr := briefs.UnmarshalBtreeHeader(buf)
	if hdr.Magic != briefs.BtreeMagic {
		return 0, fmt.Errorf("btree node %d: %w", block, briefs.ErrBtreeBadMagic)
	}
	// The checksum was already verified by the collectInodeExtents walk (which
	// succeeded for this inode), so it is not re-verified here.

	if hdr.IsLeaf() {
		if int(hdr.NumKeys) > briefs.BtreeLeafFanout {
			return 0, fmt.Errorf("btree node %d: %w (leaf %d > %d)",
				block, briefs.ErrBtreeCountOverflow, hdr.NumKeys, briefs.BtreeLeafFanout)
		}
		// Leaves are level 0.
		if hdr.Level != 0 {
			return 0, fmt.Errorf("btree node %d: %w (leaf with level %d, want 0)",
				block, briefs.ErrBtreeBadChild, hdr.Level)
		}

		var count int
		var prevOffset uint64
		var leafMax uint64
		for i := uint16(0); i < hdr.NumKeys; i++ {
			ext := briefs.ReadBtreeLeafExtent(buf, int(i))
			if i > 0 && ext.Offset <= prevOffset {
				return 0, fmt.Errorf("btree node %d: %w (offset %d after %d)",
					block, briefs.ErrBtreeUnsorted, ext.Offset, prevOffset)
			}
			prevOffset = ext.Offset
			leafMax = ext.Offset
			count++
		}

		// Cross-leaf ordering: this leaf's first offset must exceed the
		// previous leaf's max offset. Empty leaves don't advance the cursor.
		if hdr.NumKeys > 0 {
			first := briefs.ReadBtreeLeafExtent(buf, 0).Offset
			if s.havePrev && first <= s.prevLeafMax {
				return 0, fmt.Errorf("btree node %d: %w (leaf first offset %d <= previous leaf max %d)",
					block, briefs.ErrBtreeCrossLeafUnsorted, first, s.prevLeafMax)
			}
			s.prevLeafMax = leafMax
			s.havePrev = true
		}
		return count, nil
	}

	// Internal node.
	if int(hdr.NumKeys) > briefs.BtreeIdxFanout {
		return 0, fmt.Errorf("btree node %d: %w (internal %d > %d)",
			block, briefs.ErrBtreeCountOverflow, hdr.NumKeys, briefs.BtreeIdxFanout)
	}
	// An internal node sits above leaves, so its level must be >= 1.
	if hdr.Level == 0 {
		return 0, fmt.Errorf("btree node %d: %w (internal node with level 0)", block, briefs.ErrBtreeBadChild)
	}
	// A non-root internal node must be exactly one level below its parent.
	if !isRoot && hdr.Level != expectedLevel {
		return 0, fmt.Errorf("btree node %d: %w (level %d, want %d)",
			block, briefs.ErrBtreeBadChild, hdr.Level, expectedLevel)
	}

	// Separator high_keys must be strictly ascending and > 0. Separators are
	// real extent offsets (the first offset of each right sibling), so a
	// non-ascending or zero separator is corruption.
	var prevHigh uint64
	for i := uint16(0); i < hdr.NumKeys; i++ {
		e := briefs.ReadBtreeIdxEntry(buf, int(i))
		if e.HighKey == 0 {
			return 0, fmt.Errorf("btree node %d: %w (idx[%d].high_key=0; separators must be > 0)",
				block, briefs.ErrBtreeBadHighKey, i)
		}
		if i > 0 && e.HighKey <= prevHigh {
			return 0, fmt.Errorf("btree node %d: %w (idx[%d].high_key=%d after %d)",
				block, briefs.ErrBtreeBadHighKey, i, e.HighKey, prevHigh)
		}
		prevHigh = e.HighKey
	}

	// Recurse into children left-to-right (idx children then the trailing
	// child), which yields leaves in ascending key order. Each child must be
	// one level below this node.
	childLevel := hdr.Level - 1
	total := 0
	for i := uint16(0); i < hdr.NumKeys; i++ {
		e := briefs.ReadBtreeIdxEntry(buf, int(i))
		c, err := s.verifySubtree(e.Child, childLevel, false)
		if err != nil {
			return 0, err
		}
		total += c
	}
	trailing := briefs.BtreeTrailingChild(buf)
	c, err := s.verifySubtree(trailing, childLevel, false)
	if err != nil {
		return 0, err
	}
	total += c
	return total, nil
}