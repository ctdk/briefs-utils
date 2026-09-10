// Package fuse implements a FUSE filesystem for BrieFS.
package fuse

import (
	"fmt"
	"math/bits"
	"sync"

	"github.com/ctdk/briefs-utils/briefs"
)

// Allocator is the runtime 3-level bitmap pyramid.
// Ported from kernel briefs_alloc.c.
type Allocator struct {
	mu sync.Mutex

	dev                       *BlockDevice
	poolStart                 uint64
	blockSize                 uint64
	l0Words, l1Words, l2Words uint64
	blockCount                uint64
	freeCount                 uint64
	l0, l1, l2                []uint64
	dirty                     bool
	// metaShield is the unwritten-extent metadata reserve (kernel
	// alloc->meta_shield, alloc.c:193-217): a COUNT of free blocks reserved
	// for the worst-case B+tree nodes that fragmenting the outstanding
	// unwritten extents could require.  Held as a count, never bitmap bits,
	// so data allocations stop at free_count == metaShield while
	// AllocBlockMeta (B+tree nodes) draws from the full free count.  Always
	// 0 on the inode allocator.  Adjusted by BrieFS.setUnwrittenRes
	// (meta_shield.go), which maintains the kernel invariant
	// meta_shield == sum over inodes of metaReserveSize(unwritten blocks).
	metaShield uint64
	// l2Dirty holds the indices (within the L2 level) of the on-disk blocks
	// whose words changed since the last Sync, so Sync rewrites only those.
	// L0/L1 are small summaries and are rewritten wholesale whenever dirty.
	l2Dirty map[uint64]bool
	// reclaim, when non-nil, is called once after a failed allocation scan
	// (BrieFS.reclaimPendingFrees on the data allocator): it commits the
	// journal so blocks freed earlier in this session — whose frees are
	// deferred until their records commit (BrieFS.deferBlockFree) — become
	// reusable, and reports whether a reclaim was attempted.  The kernel
	// needs no such hook: kjournald commits every few seconds and sync(2)
	// reaches the fs, so a deferred free never starves an allocation for
	// long.  The bridge syncs only on explicit fsync/umount (kernel parity,
	// dir.c:25) and a FUSE mount never receives SYNCFS (the 6.12 client
	// sets fc->sync_fs only for fuseblk, inode.c:1742), so delete-then-write
	// workloads would ENOSPC with thousands of blocks pending.  The hook
	// keeps the crash model intact: a block becomes reusable only after its
	// freeing record is durable, because the journal sync commits the
	// record BEFORE SyncMeta applies the free (cache.go).  nil on allocators
	// that never see deferred frees (the inode allocator, test instances).
	reclaim func() bool
}

// OpenAllocator reads the allocator pool from disk and initializes the in-memory bitmap.
func OpenAllocator(dev *BlockDevice, poolStart uint64) (*Allocator, error) {
	l0, l1, l2, hdr, err := briefs.ReadAllocatorBitmap(dev, poolStart, dev.BlockSize())
	if err != nil {
		return nil, err
	}

	if hdr.L0Words < 1 || hdr.L1Words < 1 || hdr.L2Words < 1 {
		return nil, fmt.Errorf("invalid allocator level sizes: l0=%d l1=%d l2=%d",
			hdr.L0Words, hdr.L1Words, hdr.L2Words)
	}

	return &Allocator{
		dev:        dev,
		blockSize:  dev.BlockSize(),
		poolStart:  poolStart,
		l0Words:    hdr.L0Words,
		l1Words:    hdr.L1Words,
		l2Words:    hdr.L2Words,
		blockCount: hdr.BlockCount,
		freeCount:  hdr.FreeCount,
		l0:         l0,
		l1:         l1,
		l2:         l2,
		l2Dirty:    make(map[uint64]bool),
	}, nil
}

// markL2Word records the on-disk L2 block containing word index w2 as
// changed, so Sync writes only the L2 blocks that actually differ (the L2
// leaf dominates the pool: on a 17TB volume it is over 130,000 blocks).
// Called with a.mu held, alongside dirty = true.
func (a *Allocator) markL2Word(w2 uint64) {
	if a.l2Dirty == nil {
		a.l2Dirty = make(map[uint64]bool)
	}
	a.l2Dirty[w2/(a.blockSize/8)] = true
}

// AllocBlock finds and allocates a single free block (a DATA allocation:
// it respects metaShield).  Returns the data-relative block number, or 0
// if out of space.
func (a *Allocator) AllocBlock() uint64 {
	return a.allocBlock(false)
}

// AllocBlockMeta is the metadata-class single-block allocation: it ignores
// metaShield and draws from the full free count, mirroring the kernel's
// briefs_alloc_block_meta (alloc.c:316) whose only callers are the B+tree
// node allocations (btree.c splits/promote).  Without the bypass, a fs
// filled to free_count == metaShield would ENOSPC exactly when the shield
// exists to let the unwritten-extent conversion's node splits proceed.
func (a *Allocator) AllocBlockMeta() uint64 {
	return a.allocBlock(true)
}

// allocBlock is the shared single-block allocator, mirroring the kernel's
// __briefs_alloc_block (alloc.c:193).  forMeta selects whether the metadata
// shield applies.  A failed scan gets one reclaim retry (see Allocator.reclaim):
// with frees deferred to their records' commit, the in-memory bitmap can be
// exhausted while thousands of freed blocks are merely pending — the retry
// commits them and scans again.
func (a *Allocator) allocBlock(forMeta bool) uint64 {
	rel := a.tryAllocBlock(forMeta)
	if rel == 0 && a.reclaim != nil && a.reclaim() {
		rel = a.tryAllocBlock(forMeta)
	}
	return rel
}

// tryAllocBlock is allocBlock's single scan pass.
func (a *Allocator) tryAllocBlock(forMeta bool) uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Data allocations must respect the metadata shield: the effective
	// free count is free_count - meta_shield, so a fs filled to 100% stops
	// at free_count == meta_shield, leaving the shielded blocks free for
	// metadata.  Metadata allocations (forMeta) bypass the shield and draw
	// from the full free_count (ENOSPC only at true exhaustion).
	avail := a.freeCount
	if !forMeta {
		if avail > a.metaShield {
			avail -= a.metaShield
		} else {
			avail = 0
		}
	}
	if avail == 0 || a.l0 == nil {
		return 0
	}

	for w0 := uint64(0); w0 < a.l0Words; w0++ {
		if a.l0[w0] == 0 {
			continue
		}
		b0 := uint64(bits.TrailingZeros64(a.l0[w0]))

		w1Idx := w0*64 + b0
		if w1Idx >= a.l1Words {
			return 0
		}
		l1Word := a.l1[w1Idx]
		if l1Word == 0 {
			a.l0[w0] &^= 1 << b0
			continue
		}
		b1 := uint64(bits.TrailingZeros64(l1Word))

		w2Idx := w1Idx*64 + b1
		if w2Idx >= a.l2Words {
			return 0
		}
		l2Word := a.l2[w2Idx]
		if l2Word == 0 {
			a.l1[w1Idx] &^= 1 << b1
			if a.l1[w1Idx] == 0 {
				a.l0[w0] &^= 1 << b0
			}
			continue
		}
		b2 := uint64(bits.TrailingZeros64(l2Word))

		block := w2Idx*64 + b2
		if block >= a.blockCount {
			return 0
		}

		// Clear the bit
		a.l2[w2Idx] &^= 1 << b2
		a.freeCount--
		a.dirty = true
		a.markL2Word(w2Idx)

		// Propagate upward if word went to zero
		if a.l2[w2Idx] == 0 {
			a.l1[w1Idx] &^= 1 << b1
			if a.l1[w1Idx] == 0 {
				a.l0[w0] &^= 1 << b0
			}
		}

		return block
	}

	return 0
}

// AllocBlocks finds and allocates a contiguous run of n free blocks under one
// lock, returning the starting data-relative block, or 0 when no contiguous
// run of length n is free (or n == 0). Ported from the kernel's
// briefs_alloc_blocks (alloc.c:350): first-fit scan of the L2 words for a
// maximal run of set (free) bits at least n long, possibly spanning word
// boundaries, with the last word masked to blockCount bits so a run cannot
// run past the end of the device. The block-0 failure sentinel is reserved
// (idempotently) before the scan, so a run never starts at 0.
func (a *Allocator) AllocBlocks(n uint64) uint64 {
	if a.l0 == nil || n == 0 {
		return 0
	}
	rel := a.tryAllocBlocks(n)
	if rel == 0 && a.reclaim != nil && a.reclaim() {
		rel = a.tryAllocBlocks(n)
	}
	return rel
}

// tryAllocBlocks is AllocBlocks' single scan pass (the body under the lock).
func (a *Allocator) tryAllocBlocks(n uint64) uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Data run allocation respects metaShield like allocBlock (kernel
	// briefs_alloc_blocks, alloc.c:365-371): a run is a data allocation, so
	// it must leave the shielded blocks for metadata.
	dataAvail := a.freeCount
	if dataAvail > a.metaShield {
		dataAvail -= a.metaShield
	} else {
		dataAvail = 0
	}
	if n > dataAvail || n > a.blockCount {
		return 0
	}

	// Reserve the data-relative block-0 sentinel so a run never starts there
	// (briefs_alloc_block's convention; mkfs normally did this already).
	if a.l2[0]&1 != 0 {
		a.l2[0] &^= 1
		a.freeCount--
		if a.l2[0] == 0 {
			a.l1[0] &^= 1
			if a.l1[0] == 0 {
				a.l0[0] &^= 1
			}
		}
		a.dirty = true
		a.markL2Word(0)
	}

	runStart, runLen := uint64(0), uint64(0)
	for w2 := uint64(0); w2 < a.l2Words; w2++ {
		word := a.l2[w2]
		base := w2 * 64

		// Mask trailing bits beyond blockCount in the last word.
		if w2 == a.l2Words-1 {
			if rem := a.blockCount % 64; rem != 0 {
				word &= (1 << rem) - 1
			}
		}
		if word == 0 {
			runLen = 0
			continue
		}

		// Walk each maximal run of set bits within this word.
		// (Named wbits: `bits` is the math/bits package.)
		wbits := word
		for wbits != 0 {
			b := uint64(bits.TrailingZeros64(wbits))
			s := base + b
			// Count consecutive set bits from b within this word.
			// TrailingZeros64(0) == 64, so an all-ones-from-b run
			// (only possible at b == 0) yields cnt == 64.
			cnt := uint64(bits.TrailingZeros64(^(wbits >> b)))
			if runLen > 0 && s == runStart+runLen {
				runLen += cnt // contiguous with the previous word's run
			} else {
				runStart, runLen = s, cnt
			}
			if runLen >= n {
				goto found
			}
			// Clear the consumed run bits. When cnt == 64 the whole
			// word is one run from bit b (bits below b are already
			// 0), so clearing the entire word is equivalent and
			// avoids the (1 << 64) that would otherwise loop forever.
			if cnt >= 64 {
				wbits = 0
			} else {
				wbits &^= ((1 << cnt) - 1) << b
			}
		}
	}

	// No contiguous run of length n.
	return 0

found:
	// Clear the n bits.
	for i := uint64(0); i < n; i++ {
		blk := runStart + i
		a.l2[blk/64] &^= 1 << (blk % 64)
	}
	a.freeCount -= n

	// Propagate L2 -> L1 -> L0 for every L2 word that became all-zero.
	w2First := runStart / 64
	w2Last := (runStart + n - 1) / 64
	for w2 := w2First; w2 <= w2Last; w2++ {
		a.markL2Word(w2)
		if a.l2[w2] == 0 {
			w1, b1 := w2/64, w2%64
			a.l1[w1] &^= 1 << b1
			if a.l1[w1] == 0 {
				w0, b0 := w1/64, w1%64
				a.l0[w0] &^= 1 << b0
			}
		}
	}

	a.dirty = true
	return runStart
}

// TrimFreeRuns walks the L2 leaf bitmap read-only for maximal free runs,
// invoking visit for each (runStart, runLen) pair in data-relative blocks,
// possibly spanning L2 word boundaries. The allocator mutex is held across
// the whole walk, so a concurrent allocation cannot hand out a block the
// visitor is about to discard (kernel briefs_trim_fs holds alloc->lock the
// same way, alloc.c:534); visit must not call back into the Allocator.
// The clamping / minlen policy lives in the caller (the kernel's
// briefs_trim_flush, alloc.c:474). Ported from briefs_trim_fs (alloc.c:512).
func (a *Allocator) TrimFreeRuns(visit func(runStart, runLen uint64) error) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.l0 == nil {
		return nil
	}

	runStart, runLen := uint64(0), uint64(0)
	flush := func() error {
		if runLen == 0 {
			return nil
		}
		if err := visit(runStart, runLen); err != nil {
			return err
		}
		runLen = 0
		return nil
	}

	for w2 := uint64(0); w2 < a.l2Words; w2++ {
		word := a.l2[w2]
		base := w2 * 64

		// Mask trailing bits beyond blockCount in the last word.
		if w2 == a.l2Words-1 {
			if rem := a.blockCount % 64; rem != 0 {
				word &= (1 << rem) - 1
			}
		}
		if word == 0 {
			if err := flush(); err != nil {
				return err
			}
			continue
		}

		// Walk each maximal run of set bits within this word
		// (named wbits: `bits` is the math/bits package).
		wbits := word
		for wbits != 0 {
			b := uint64(bits.TrailingZeros64(wbits))
			s := base + b
			// TrailingZeros64(0) == 64, so an all-ones-from-b run
			// (only possible at b == 0) yields cnt == 64.
			cnt := uint64(bits.TrailingZeros64(^(wbits >> b)))
			if runLen > 0 && s == runStart+runLen {
				runLen += cnt // contiguous with the previous word's run
			} else {
				if err := flush(); err != nil {
					return err
				}
				runStart, runLen = s, cnt
			}
			if cnt >= 64 {
				wbits = 0
			} else {
				wbits &^= ((1 << cnt) - 1) << b
			}
		}
	}
	return flush()
}

// FreeBlock marks a data-relative block as free.
func (a *Allocator) FreeBlock(relBlock uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.l0 == nil {
		return
	}
	if briefs.AllocMarkFree(a.l0, a.l1, a.l2, &a.freeCount, a.blockCount, relBlock) {
		a.dirty = true
		a.markL2Word(relBlock / 64)
	}
}

// ReserveBlock marks a specific block as allocated (used during journal replay).
func (a *Allocator) ReserveBlock(relBlock uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.l0 == nil {
		return
	}
	if briefs.AllocMarkAllocated(a.l0, a.l1, a.l2, &a.freeCount, a.blockCount, relBlock) {
		a.dirty = true
		a.markL2Word(relBlock / 64)
	}
}

// FreeCount returns the number of free blocks.
func (a *Allocator) FreeCount() uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.freeCount
}

// AdjustShield raises (delta > 0) or lowers (delta < 0) the unwritten-extent
// metadata reserve.  Callers derive deltas from metaReserveSize so the sum
// over inodes exactly matches the shield (kernel alloc.c:897/920).
func (a *Allocator) AdjustShield(delta int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if delta >= 0 {
		a.metaShield += uint64(delta)
	} else {
		a.metaShield -= uint64(-delta)
	}
}

// Shield returns the current metadata reserve count.
func (a *Allocator) Shield() uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.metaShield
}

// FreeCountData returns the free blocks available to data allocations:
// freeCount minus metaShield, clamped at zero.  Statfs reports this so df
// shows the data-allocatable free space (kernel super.c:799-804).
func (a *Allocator) FreeCountData() uint64 {
	return a.FreeCountDataPlus(0)
}

// FreeCountDataPlus is FreeCountData counting extra additional blocks as
// free — the data blocks whose freeing records are journal-committed but
// whose frees are still pending (BrieFS.pendingFrees).  They are reusable
// (an allocation reclaims them by committing the journal first), so df must
// report them; the kernel gets the same effect from its commit thread and
// wired sync(2), neither of which exists on a FUSE mount (inode.c:1742).
func (a *Allocator) FreeCountDataPlus(extra uint64) uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	avail := a.freeCount + extra
	if avail <= a.metaShield {
		return 0
	}
	return avail - a.metaShield
}

// Allocated reports whether the given data-relative block (or inode, for the
// inode allocator) is marked allocated (bit clear in the L2 bitmap). Used by
// journal replay's nlink reconciliation to walk only live inodes. Mirrors the
// kernel's replay_inode_allocated() (journal.c:1092).
func (a *Allocator) Allocated(rel uint64) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.l0 == nil {
		return false
	}
	return briefs.AllocIsAllocated(a.l2, a.blockCount, rel)
}

// FreeBlocksRange frees [phys, phys+length) absolute data blocks, converting
// each to data-relative. Used by journal replay's JRN_EXTENT_FREE / trie-free
// handlers. Mirrors the kernel's briefs_free_blocks_range().
func (a *Allocator) FreeBlocksRange(dataRegionStart, phys, length uint64) {
	for i := uint64(0); i < length; i++ {
		a.FreeBlock(phys + i - dataRegionStart)
	}
}

// TotalBlocks returns the total number of blocks tracked by this allocator.
func (a *Allocator) TotalBlocks() uint64 {
	return a.blockCount
}

// Sync writes the in-memory bitmap back to disk.
func (a *Allocator) Sync() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if !a.dirty {
		return nil
	}

	blockSize := a.dev.BlockSize()

	// The position advance must match the block count PackAllocWords
	// produces for the same level (blockSize-based, like the packer).
	wpb := blockSize / 8
	l0Blocks := (a.l0Words + wpb - 1) / wpb
	l1Blocks := (a.l1Words + wpb - 1) / wpb

	writeDirty := func(offset uint64, words []uint64, nWords uint64) error {
		if nWords > uint64(len(words)) {
			nWords = uint64(len(words))
		}
		for i, buf := range briefs.PackAllocWords(words[:nWords], blockSize) {
			if err := a.dev.WriteBlock(offset+uint64(i), buf); err != nil {
				return err
			}
		}
		return nil
	}

	// Level 0 and Level 1 are summaries of L2 and are small even on huge
	// volumes (on 17TB: ~33 and ~2100 blocks); tracking which of their words
	// crossed the zero boundary is not worth it, so they are rewritten
	// wholesale. The L2 leaf is where the bulk of the pool lives (over
	// 130,000 blocks on the same volume), so only the blocks whose words
	// changed are written.
	pos := a.poolStart + 1
	if err := writeDirty(pos, a.l0, a.l0Words); err != nil {
		return fmt.Errorf("sync L0: %w", err)
	}
	pos += l0Blocks

	// Level 1
	if err := writeDirty(pos, a.l1, a.l1Words); err != nil {
		return fmt.Errorf("sync L1: %w", err)
	}
	pos += l1Blocks

	// Level 2: only the changed blocks. PackAllocWords zero-pads the tail
	// of the last block, matching the wholesale write above. An empty set
	// with dirty set cannot happen (every mutation marks its L2 word); the
	// wholesale fallback keeps a forgotten mark from silently dropping a
	// changed block.
	if len(a.l2Dirty) == 0 {
		if err := writeDirty(pos, a.l2, a.l2Words); err != nil {
			return fmt.Errorf("sync L2: %w", err)
		}
	} else {
		for blk := range a.l2Dirty {
			first := blk * wpb
			if first >= a.l2Words {
				continue // stale index; cannot happen while markL2Word gates
			}
			last := min(first+wpb, a.l2Words)
			l2Blocks := briefs.PackAllocWords(a.l2[first:last], blockSize)
			if err := a.dev.WriteBlock(pos+blk, l2Blocks[0]); err != nil {
				return fmt.Errorf("sync L2 block %d: %w", blk, err)
			}
		}
	}

	// Update header with free_count
	hdr := make([]byte, blockSize)
	ah := &briefs.AllocHeader{
		Magic:      briefs.AllocMagic,
		Version:    1,
		L0Words:    a.l0Words,
		L1Words:    a.l1Words,
		L2Words:    a.l2Words,
		BlockCount: a.blockCount,
		FreeCount:  a.freeCount,
	}
	ahData, _ := ah.MarshalBinary()
	copy(hdr[:48], ahData)
	if err := a.dev.WriteBlock(a.poolStart, hdr); err != nil {
		return fmt.Errorf("sync allocator header: %w", err)
	}

	a.dirty = false
	a.l2Dirty = make(map[uint64]bool)
	return nil
}

// InodeManager handles on-disk inode read/write.
type InodeManager struct {
	dev *BlockDevice
	sb  *briefs.SuperblockLayout
}

// NewInodeManager creates an InodeManager from the superblock.
func NewInodeManager(dev *BlockDevice, sb *briefs.SuperblockLayout) *InodeManager {
	return &InodeManager{
		dev: dev,
		sb:  sb,
	}
}

// inodeLocation computes the block and byte offset for a given inode number,
// delegating to the shared briefs.InodeLocation formula.
func (im *InodeManager) inodeLocation(ino uint64) (blockOffset uint64, byteOffset uint64) {
	return briefs.InodeLocation(im.sb, ino)
}

// ReadInode reads and unmarshals an inode from disk.
func (im *InodeManager) ReadInode(ino uint64) (*briefs.Inode, error) {
	blk, off := im.inodeLocation(ino)
	buf, err := im.dev.ReadBlock(blk)
	if err != nil {
		return nil, fmt.Errorf("read inode %d block %d: %w", ino, blk, err)
	}
	inodeData := buf[off : off+im.sb.InodeSize]
	return briefs.UnmarshalInode(inodeData)
}

// WriteInode marshals and writes an inode to disk.
func (im *InodeManager) WriteInode(inode *briefs.Inode) error {
	blk, off := im.inodeLocation(inode.InodeNumber)
	buf, err := im.dev.ReadBlock(blk)
	if err != nil {
		return fmt.Errorf("read-modify-write inode %d block %d: %w", inode.InodeNumber, blk, err)
	}
	// Marshal inode data into the buffer at the correct offset
	{
		data, err := inode.MarshalBinary()
		if err != nil {
			return fmt.Errorf("marshal inode %d: %w", inode.InodeNumber, err)
		}
		copy(buf[off:], data)
	}

	return im.dev.WriteBlock(blk, buf)
}
