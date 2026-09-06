// Package fuse: the unwritten-extent metadata reservation (meta_shield).
//
// Port of the kernel's count-shield (alloc.c:843-940, "Unwritten-extent
// metadata reservation"): for every unwritten (fallocate) block, a worst-case
// count of B+tree metadata blocks the eventual per-block fragmentation of
// those blocks could require is reserved as a COUNT in the data allocator's
// shield (Allocator.metaShield), never as bitmap bits.  A filesystem later
// filled to 100% therefore stops at free_count == meta_shield, leaving the
// shielded blocks free for the node allocations (Allocator.AllocBlockMeta)
// that splitting the converting extents needs — without the shield, a write
// converting an unwritten block ENOSPCs on a full fs where the kernel
// succeeds.  Reserved blocks are marked allocated only when they become
// referenced B+tree nodes, so fsck never sees allocated-but-unreferenced
// blocks.
//
// The kernel tracks the per-inode unwritten count incrementally
// (binfo->unwritten_res_blocks; briefs_raise/release/drop_unwritten_reserve
// at every conversion / free site).  The bridge instead recomputes the
// inode's total unwritten block count from the extent list at the end of
// each extent-mutating op and adjusts the shield by the reserve-size delta:
// the same invariant (shield == sum over inodes of metaReserveSize(count)),
// derived from the actual extent list, so a missed site self-corrects on
// the inode's next op instead of silently drifting (the kernel release path
// clamps its L for exactly this stale-tracking class, alloc.c:905-907).
//
// Like the kernel's binfo counters, the per-inode state is in-memory only
// and starts empty at mount: unwritten extents created by a previous mount
// are not shielded (kernel parity — binfo is zeroed at iget).

package fuse

import (
	"github.com/ctdk/briefs-utils/briefs"
)

// metaReserveSize ports briefs_meta_reserve_size (alloc.c:877): the
// worst-case B+tree node blocks needed to index @n unwritten data blocks
// when every block is written individually (one extent per block):
// ceil(n/leaf fanout) leaves + ceil(leaves/idx fanout) internal nodes + 1
// safety slop.  Over-reserves for coarser write patterns; the surplus
// returns as the unwritten region converts.
func metaReserveSize(n uint64) uint64 {
	if n == 0 {
		return 0
	}
	leaves := (n + briefs.BtreeLeafFanout - 1) / briefs.BtreeLeafFanout
	internals := (leaves + briefs.BtreeIdxFanout - 1) / briefs.BtreeIdxFanout
	return leaves + internals + 1
}

// countUnwrittenBlocks sums the lengths of an inode's unwritten extents.
func countUnwrittenBlocks(exts []briefs.Extent) uint64 {
	n := uint64(0)
	for _, e := range exts {
		if e.Flags&briefs.ExtentFlagUnwritten != 0 {
			n += e.Len
		}
	}
	return n
}

// setUnwrittenRes sets the inode's tracked unwritten block count and adjusts
// the data allocator's shield by the reserve-size delta.  Called with the
// inode's final extent list at the end of every extent-mutating op (the
// caller holds the inodeBlockLock) and with 0 when an inode is freed
// (freeInodeData — the kernel's briefs_drop_unwritten_reserve).
//
// shieldMu — not b.mu — protects the map: file ops run under inodeBlockLock
// only, so a map behind the global dir lock would serialize unrelated
// directory ops behind file writes.  shieldMu is a leaf lock (it never
// nests inodeBlockLock or b.mu inside), so it cannot deadlock against
// either.  A failed commit after the adjustment marks the FS read-only
// (failWrite in commitExtentChange), and the entry self-corrects from the
// extent list on the inode's next op regardless.
func (b *BrieFS) setUnwrittenRes(ino uint64, n uint64) {
	b.shieldMu.Lock()
	if b.unwrittenRes == nil {
		b.unwrittenRes = make(map[uint64]uint64)
	}
	old := b.unwrittenRes[ino]
	if n == old {
		b.shieldMu.Unlock()
		return
	}
	if n == 0 {
		delete(b.unwrittenRes, ino)
	} else {
		b.unwrittenRes[ino] = n
	}
	b.shieldMu.Unlock()
	b.dataAlloc.AdjustShield(int64(metaReserveSize(n)) - int64(metaReserveSize(old)))
}

// updateUnwrittenRes recomputes the inode's shield contribution from its
// current extent list.
func (b *BrieFS) updateUnwrittenRes(ino uint64, exts []briefs.Extent) {
	b.setUnwrittenRes(ino, countUnwrittenBlocks(exts))
}
