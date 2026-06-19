package main

import (
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