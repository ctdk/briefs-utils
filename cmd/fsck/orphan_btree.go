package main

import (
	"encoding/binary"
	"fmt"

	"github.com/ctdk/briefs-utils/briefs"
)

// reclaimOrphanBtree is the Phase 5 orphan tree-block reclamation. It scans the
// data region for blocks that still carry BtreeMagic in their first 4 bytes but
// are not referenced by any inode's extent index (not in fs.usedBlocks) — the
// classic leftover of a B-tree node split that crashed partway through: the new
// node was allocated and written but never linked into any tree, so no walk ever
// reaches it, and it stays allocated forever.
//
// Such blocks are reported with a warning. When opts.ReclaimOrphanBtree is set,
// each confirmed orphan is freed in plan.dataAlloc (MarkFree is idempotent), so
// writeAllocator reclaims the space.
//
// Only blocks the allocator currently says are allocated are considered orphans.
// A block that is free but happens to carry a stale BtreeMagic (left over from a
// block that was once a node and has since been freed and reused as scratch) is
// NOT reported: it is already free, so there is nothing to reclaim, and warning
// about it would be noise.
//
// This phase is deliberately default-off even in "all" (reclamation frees a block
// the on-disk metadata still claims, which is destructive if the scan is wrong),
// and it only runs when every B+ tree walked cleanly (len(fs.failedBtreeInos) ==
// 0). If any tree failed, its unreached node blocks are legitimately allocated
// (the walk simply never reached them), and freeing them would lose data.
func reclaimOrphanBtree(fs *fsckState, plan *repairPlan, opts *repairOptions, blockSize, dataRegionStart, dataBlockCount uint64) error {
	if len(fs.failedBtreeInos) > 0 {
		// Do not reclaim while any tree is torn: unreached node blocks of the
		// failed trees would be indistinguishable from orphans and would be freed.
		fs.warnf("orphan B-tree reclamation skipped: %d inode(s) have failed B-tree walks; re-run after repairing them",
			len(fs.failedBtreeInos))
		return nil
	}

	hdr := make([]byte, 4)
	reclaimed := 0
	reported := 0
	const reportLimit = 20

	for relBlk := uint64(0); relBlk < dataBlockCount; relBlk++ {
		absBlk := dataRegionStart + relBlk
		// Only blocks the allocator marks allocated can be orphans. A free block
		// carrying a stale magic is not an orphan (nothing to reclaim).
		if !plan.dataAlloc.IsAllocated(relBlk) {
			continue
		}
		// Reachable by some inode's tree or data extents: not an orphan.
		if fs.usedBlocks[absBlk] {
			continue
		}

		// Read the first 4 bytes and check for BtreeMagic. A non-B-tree allocated
		// block (data extent, etc.) that is somehow not in usedBlocks is a
		// leaked-block concern handled by verifyBlockCrossReference, not this pass.
		if _, err := fs.file.ReadAt(hdr, int64(absBlk*blockSize)); err != nil {
			// Unreadable: leave allocated, don't reclaim — could be a real block on
			// a bad medium. Report once-ish and move on.
			if reported < reportLimit {
				fs.warnf("orphan B-tree scan: block %d unreadable (%v), left allocated", absBlk, err)
			}
			reported++
			continue
		}
		if binary.LittleEndian.Uint32(hdr) != briefs.BtreeMagic {
			continue
		}

		// Confirmed orphan: allocated, unreferenced, and still looks like a B-tree
		// node. Free it when asked; always warn so a read-only run surfaces them.
		fs.warnf("orphan B-tree node block %d (data-relative %d): carries B-tree magic but no inode references it",
			absBlk, relBlk)
		if opts.ReclaimOrphanBtree {
			plan.dataAlloc.MarkFree(relBlk)
			reclaimed++
		}
	}

	if reclaimed > 0 {
		fmt.Printf("  orphan B-tree reclamation: freed %d block(s)\n", reclaimed)
	}
	return nil
}