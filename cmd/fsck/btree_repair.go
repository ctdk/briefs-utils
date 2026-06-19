package main

import (
	"fmt"
	"sort"

	"github.com/ctdk/briefs-utils/briefs"
)

// repairBtreeChecksums is the Phase 3 CRC-only repair. For each tree-backed
// inode whose basic walk failed (fs.failedBtreeInos — the verify-pass btreeWalk
// fails on a bad checksum, so bad-CRC inodes are exactly this set), it re-walks
// the tree tolerantly and rewrites a node's checksum when the node is
// structurally self-consistent but only the CRC is wrong.
//
// Scope is deliberately leaf-only:
//
//   - A leaf's "valid structure" is fully self-contained — magic, fanout, and
//     within-leaf key ordering can all be checked without following any pointer,
//     so a checksum mismatch on such a leaf is unambiguously a torn CRC and is
//     safe to recompute.
//
//   - An internal node's checksum covers its child pointers. A mismatch there
//     could mean the pointers themselves are corrupt; recomputing the checksum
//     would validate the corrupt pointers and mask the real fault. Internal-node
//     checksum failures are therefore left for Phase 4's full rebuild — the
//     tolerant walk stops at a bad-checksum internal node and does not descend.
//
// This is non-destructive: it allocates no blocks and mutates no inodes. It
// MarkAllocates each rewritten leaf block in plan.dataAlloc (idempotent — the
// block is already a live tree node) so writeAllocator keeps it even when the
// on-disk allocator is loaded rather than rebuilt (RebuildAllocator=false, as
// --repair-only=btrees does).
//
// Structural faults (bad magic, fanout overflow, unsorted extents within a leaf)
// cause the walk to stop at that node without rewriting it, deferring to Phase 4.
// The re-verification pass after runRepair re-walks and re-populates
// failedBtreeInos, so this function does not clear that set itself.
func repairBtreeChecksums(fs *fsckState, plan *repairPlan, blockSize, dataRegionStart uint64) error {
	for ino := range fs.failedBtreeInos {
		in := fs.inodes[ino]
		if in == nil || in.Flags&briefs.InodeFlagIndexed == 0 {
			continue
		}
		root := in.ExtentInlineBase
		if root == 0 {
			continue
		}
		repairBtreeChecksumSubtree(fs, plan, ino, root, blockSize, dataRegionStart, make(map[uint64]bool))
	}
	return nil
}

// repairBtreeChecksumSubtree descends the subtree at @block, rewriting leaf
// checksums where the leaf is structurally valid. It is best-effort: any
// structural fault or read error stops descent of that subtree (the node is
// left for Phase 4) without aborting sibling subtrees. @visited guards against
// cycles independent of the verify-pass walk.
func repairBtreeChecksumSubtree(fs *fsckState, plan *repairPlan, ino, block, blockSize, dataRegionStart uint64, visited map[uint64]bool) {
	if block == 0 || visited[block] {
		return
	}
	visited[block] = true

	buf := make([]byte, blockSize)
	if _, err := fs.file.ReadAt(buf, int64(block*blockSize)); err != nil {
		return // unreadable node; leave for Phase 4
	}
	hdr := briefs.UnmarshalBtreeHeader(buf)
	if hdr.Magic != briefs.BtreeMagic {
		return // structural: bad magic, defer to Phase 4
	}

	if hdr.IsLeaf() {
		if int(hdr.NumKeys) > briefs.BtreeLeafFanout {
			return // structural: fanout overflow, defer
		}
		// Within-leaf key ordering must hold before a checksum rewrite is safe.
		var prev uint64
		for i := uint16(0); i < hdr.NumKeys; i++ {
			ext := briefs.ReadBtreeLeafExtent(buf, int(i))
			if i > 0 && ext.Offset <= prev {
				return // structural: unsorted extents, defer to Phase 4
			}
			prev = ext.Offset
		}
		// Structure is self-consistent. If only the CRC is wrong, recompute it.
		if briefs.VerifyBtreeNodeChecksum(buf, blockSize) == nil {
			return // checksum already valid; nothing to do
		}
		briefs.SetBtreeNodeChecksum(buf, blockSize)
		if err := briefs.WriteBtreeNode(fs.file, block, blockSize, buf); err != nil {
			fs.warnf("ino %d: failed to rewrite leaf node %d checksum: %v", ino, block, err)
			return
		}
		fs.warnf("ino %d: rewrote torn checksum of leaf node %d", ino, block)
		if block >= dataRegionStart {
			plan.dataAlloc.MarkAllocated(block - dataRegionStart)
		}
		return
	}

	// Internal node.
	if int(hdr.NumKeys) > briefs.BtreeIdxFanout {
		return // structural: fanout overflow, defer
	}
	if briefs.VerifyBtreeNodeChecksum(buf, blockSize) != nil {
		// Bad checksum on an internal node: the child pointers it covers may be
		// corrupt, so do NOT recompute (that would mask the fault) and do NOT
		// descend (following untrusted pointers could touch unrelated blocks).
		// Defer the whole subtree to Phase 4.
		return
	}
	// Checksum valid: the child pointers are trustworthy. Descend.
	for i := uint16(0); i < hdr.NumKeys; i++ {
		e := briefs.ReadBtreeIdxEntry(buf, int(i))
		repairBtreeChecksumSubtree(fs, plan, ino, e.Child, blockSize, dataRegionStart, visited)
	}
	trailing := briefs.BtreeTrailingChild(buf)
	repairBtreeChecksumSubtree(fs, plan, ino, trailing, blockSize, dataRegionStart, visited)
}

// rebuildBtreeIndex is the Phase 4 full rebuild. For each tree-backed inode in
// fs.failedBtreeInos, it tolerantly re-collects whatever extents survive in
// readable, structurally-valid, checksum-valid leaves; sorts/dedups them; and
// either drops the inode to an inline array (<=8 extents) or builds a fresh
// B+ tree from them. Old node blocks that were successfully read and validated
// are freed; lost subtrees' data blocks are left allocated (see the lost-extent
// policy below). The rebuilt inode is staged in plan.inodes; writeModifiedInodes
// and writeAllocator persist it and the updated allocation bitmap.
//
// Lost-extent policy: when a subtree is unreadable / bad-magic / structurally
// broken / bad-checksum, its extents cannot be trusted and are NOT collected.
// Their data blocks are left allocated (never freed) so the data is preserved
// for manual recovery — fsck never reconstructs Phys from FileSize heuristics.
// Only confirmed B-tree node blocks we read and validated (valid magic +
// checksum + structure) are freed; corrupt/unreadable node blocks and their
// un-validated descendants are left allocated too (Phase 5 orphan reclamation
// can reclaim leaked node blocks once all trees walk cleanly).
//
// This mutates plan.dataAlloc directly (AllocateBlock marks new nodes
// allocated; MarkFree frees old ones), so it is correct whether
// RebuildAllocator is true or false. It runs after repairBtreeChecksums (so
// Phase 3 has already rewritten any torn-but-valid leaf checksums) and before
// writeModifiedInodes / writeAllocator.
func rebuildBtreeIndex(fs *fsckState, plan *repairPlan, blockSize, dataRegionStart uint64) error {
	for ino := range fs.failedBtreeInos {
		in := fs.inodes[ino]
		if in == nil || in.Flags&briefs.InodeFlagIndexed == 0 {
			continue
		}
		root := in.ExtentInlineBase
		if root == 0 {
			continue
		}
		if err := rebuildOneBtree(fs, plan, ino, in, root, blockSize, dataRegionStart); err != nil {
			fs.warnf("ino %d: B-tree rebuild skipped: %v (inode left in failed state, data blocks left allocated)", ino, err)
			continue
		}
	}
	return nil
}

// rebuildOneBtree collects, decides inline-vs-tree, and stages the rebuilt
// inode for @ino. Returns an error only when the inode is unrecoverable (no
// readable extents) or the rebuild itself failed; in those cases the caller
// leaves the inode in failedBtreeInos and frees nothing.
func rebuildOneBtree(fs *fsckState, plan *repairPlan, ino uint64, in *briefs.Inode, root, blockSize, dataRegionStart uint64) error {
	extents, oldNodeBlocks, lost := btreeCollectExtents(fs, ino, root, blockSize)
	if lost {
		fs.warnf("ino %d: B-tree rebuild recovered %d extent(s); some unreadable subtrees were lost — their data blocks are left allocated for manual recovery", ino, len(extents))
	}
	if len(extents) == 0 {
		// Nothing recoverable. Leave the inode failed; free/write nothing so the
		// on-disk state is unchanged and the re-verify pass re-flags it.
		return fmt.Errorf("no readable extents (tree unrecoverable)")
	}
	extents = sortDedupExtents(extents)

	clone := *in // shallow copy: arrays (inlineRegion, Reserved) are value-copied

	if len(extents) <= 8 {
		// Drop to inline: the file now fits the inline array. Clear the tree flag,
		// zero the root, pack the surviving extents, and free the old node blocks.
		var inline [8]briefs.Extent
		copy(inline[:], extents)
		clone.Flags &^= briefs.InodeFlagIndexed
		clone.ExtentInlineBase = 0
		clone.NumExtentsInline = uint32(len(extents))
		clone.NumExtentsTotal = uint64(len(extents))
		clone.SetInlineExtents(inline)
		freeNodeBlocks(plan, oldNodeBlocks, dataRegionStart)
		plan.inodes[ino] = &clone
		fs.warnf("ino %d: rebuilt extent index → inline (%d extent(s))", ino, len(extents))
		return nil
	}

	// Rebuild a fresh tree. Allocate new node blocks first (the old blocks are
	// still marked allocated, so AllocateBlock will not hand them back), then
	// free the old blocks after the new tree is written — matches the
	// trie-compaction template.
	allocBlock := func() (uint64, error) {
		rel, err := plan.dataAlloc.AllocateBlock()
		if err != nil {
			return 0, err
		}
		return rel + dataRegionStart, nil
	}
	leafBlocks, leafFirstOffsets, leafBufs, err := briefs.BuildBtreeLeaves(extents, blockSize, allocBlock)
	if err != nil {
		freeNodeBlocks(plan, leafBlocks, dataRegionStart) // free the partial allocation
		return fmt.Errorf("build leaves: %w", err)
	}
	rootBlock, _, idxBlocks, idxBufs, err := briefs.BuildBtreeIndex(leafBlocks, leafFirstOffsets, blockSize, 1, allocBlock)
	if err != nil {
		freeNodeBlocks(plan, append(append([]uint64{}, leafBlocks...), idxBlocks...), dataRegionStart)
		return fmt.Errorf("build index: %w", err)
	}
	allBlocks := append(append([]uint64{}, leafBlocks...), idxBlocks...)
	allBufs := append(append([][]byte{}, leafBufs...), idxBufs...)
	if err := briefs.WriteBtreeNodes(fs.file, allBlocks, allBufs, blockSize); err != nil {
		freeNodeBlocks(plan, allBlocks, dataRegionStart)
		return fmt.Errorf("write nodes: %w", err)
	}

	clone.ExtentInlineBase = rootBlock
	clone.NumExtentsInline = 0
	clone.NumExtentsTotal = uint64(len(extents))
	clone.SetInlineExtents([8]briefs.Extent{}) // zero the inline array (tree-backed)
	// InodeFlagIndexed remains set; other flags untouched.
	plan.inodes[ino] = &clone
	freeNodeBlocks(plan, oldNodeBlocks, dataRegionStart)
	fs.warnf("ino %d: rebuilt extent index → %d node(s), root=%d, %d extent(s)", ino, len(allBlocks), rootBlock, len(extents))
	return nil
}

// btreeCollectExtents walks the subtree at @root tolerantly, collecting extents
// from leaves that are readable, have valid magic, valid fanout, sorted keys,
// and a valid checksum. It returns the collected extents, the confirmed node
// blocks to free (valid leaves + descended internal nodes), and whether any
// subtree was skipped (lost). Lost subtrees warnf and contribute no extents;
// their node blocks are NOT returned for freeing (left allocated).
func btreeCollectExtents(fs *fsckState, ino, root, blockSize uint64) (extents []briefs.Extent, oldNodeBlocks []uint64, lost bool) {
	visited := make(map[uint64]bool)
	btreeCollectSubtree(fs, ino, root, blockSize, visited, &extents, &oldNodeBlocks, &lost)
	return
}

// btreeCollectSubtree is the recursive worker for btreeCollectExtents.
func btreeCollectSubtree(fs *fsckState, ino, block, blockSize uint64, visited map[uint64]bool, extents *[]briefs.Extent, oldNodeBlocks *[]uint64, lost *bool) {
	if block == 0 || visited[block] {
		return
	}
	visited[block] = true

	buf := make([]byte, blockSize)
	if _, err := fs.file.ReadAt(buf, int64(block*blockSize)); err != nil {
		*lost = true
		fs.warnf("ino %d: B-tree node %d unreadable (%v); extents in this subtree lost (data blocks left allocated)", ino, block, err)
		return
	}
	hdr := briefs.UnmarshalBtreeHeader(buf)
	if hdr.Magic != briefs.BtreeMagic {
		*lost = true
		fs.warnf("ino %d: B-tree node %d bad magic 0x%08X; extents in this subtree lost (data blocks left allocated)", ino, block, hdr.Magic)
		return
	}

	if hdr.IsLeaf() {
		if int(hdr.NumKeys) > briefs.BtreeLeafFanout {
			*lost = true
			fs.warnf("ino %d: leaf node %d fanout overflow (%d > %d); subtree lost", ino, block, hdr.NumKeys, briefs.BtreeLeafFanout)
			return
		}
		var prev uint64
		for i := uint16(0); i < hdr.NumKeys; i++ {
			ext := briefs.ReadBtreeLeafExtent(buf, int(i))
			if i > 0 && ext.Offset <= prev {
				*lost = true
				fs.warnf("ino %d: leaf node %d extents unsorted (offset %d after %d); subtree lost", ino, block, ext.Offset, prev)
				return
			}
			prev = ext.Offset
		}
		if briefs.VerifyBtreeNodeChecksum(buf, blockSize) != nil {
			*lost = true
			fs.warnf("ino %d: leaf node %d checksum mismatch; extents in this subtree lost (run --repair-only=btrees first, or data blocks are left allocated)", ino, block)
			return
		}
		for i := uint16(0); i < hdr.NumKeys; i++ {
			*extents = append(*extents, briefs.ReadBtreeLeafExtent(buf, int(i)))
		}
		*oldNodeBlocks = append(*oldNodeBlocks, block)
		return
	}

	// Internal node.
	if int(hdr.NumKeys) > briefs.BtreeIdxFanout {
		*lost = true
		fs.warnf("ino %d: internal node %d fanout overflow (%d > %d); subtree lost", ino, block, hdr.NumKeys, briefs.BtreeIdxFanout)
		return
	}
	if briefs.VerifyBtreeNodeChecksum(buf, blockSize) != nil {
		// Untrusted child pointers: do not descend or free this node.
		*lost = true
		fs.warnf("ino %d: internal node %d checksum mismatch; not descending (child pointers untrusted); subtree lost (data blocks left allocated)", ino, block)
		return
	}
	*oldNodeBlocks = append(*oldNodeBlocks, block)
	for i := uint16(0); i < hdr.NumKeys; i++ {
		e := briefs.ReadBtreeIdxEntry(buf, int(i))
		btreeCollectSubtree(fs, ino, e.Child, blockSize, visited, extents, oldNodeBlocks, lost)
	}
	trailing := briefs.BtreeTrailingChild(buf)
	btreeCollectSubtree(fs, ino, trailing, blockSize, visited, extents, oldNodeBlocks, lost)
}

// sortDedupExtents sorts extents ascending by Offset and drops any duplicate
// offsets (keeping the first). Hole extents (ExtentFlagHole) are ordinary
// extents with a unique offset and are preserved with their Flags/Phys/Len.
func sortDedupExtents(extents []briefs.Extent) []briefs.Extent {
	sort.Slice(extents, func(i, j int) bool { return extents[i].Offset < extents[j].Offset })
	out := make([]briefs.Extent, 0, len(extents))
	var last uint64
	for i, e := range extents {
		if i > 0 && e.Offset == last {
			continue
		}
		out = append(out, e)
		last = e.Offset
	}
	return out
}

// freeNodeBlocks MarkFrees each absolute block (relative to the data region
// start) in the builder. Blocks outside the data region are ignored.
func freeNodeBlocks(plan *repairPlan, blocks []uint64, dataRegionStart uint64) {
	for _, b := range blocks {
		if b >= dataRegionStart {
			plan.dataAlloc.MarkFree(b - dataRegionStart)
		}
	}
}