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

	dev     *BlockDevice
	poolStart uint64
	blockSize  uint64
	l0Words, l1Words, l2Words uint64
	blockCount       uint64
	freeCount        uint64
	l0, l1, l2       []uint64
	dirty            bool
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
	}, nil
}

// AllocBlock finds and allocates a single free block.
// Returns the data-relative block number, or 0 if out of space.
func (a *Allocator) AllocBlock() uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.freeCount == 0 || a.l0 == nil {
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

	a.mu.Lock()
	defer a.mu.Unlock()

	if n > a.freeCount || n > a.blockCount {
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

// FreeBlock marks a data-relative block as free.
func (a *Allocator) FreeBlock(relBlock uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.l0 == nil {
		return
	}
	if briefs.AllocMarkFree(a.l0, a.l1, a.l2, &a.freeCount, a.blockCount, relBlock) {
		a.dirty = true
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
	}
}

// FreeCount returns the number of free blocks.
func (a *Allocator) FreeCount() uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.freeCount
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

	// Level 0
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

	// Level 2
	if err := writeDirty(pos, a.l2, a.l2Words); err != nil {
		return fmt.Errorf("sync L2: %w", err)
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

