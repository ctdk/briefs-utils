// Package fuse implements a FUSE filesystem for BrieFS.
package fuse

import (
	"encoding/binary"
	"fmt"
	"math/bits"
	"sync"

	"github.com/ctdk/briefs-utils/types"
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

const wordsPerBlock = 4096 / 8 // 512

// OpenAllocator reads the allocator pool from disk and initializes the in-memory bitmap.
func OpenAllocator(dev *BlockDevice, poolStart uint64) (*Allocator, error) {
	l0, l1, l2, hdr, err := types.ReadAllocatorBitmap(dev, poolStart, dev.BlockSize())
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

// FreeBlock marks a data-relative block as free.
func (a *Allocator) FreeBlock(relBlock uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.l0 == nil || relBlock >= a.blockCount {
		return
	}

	w2 := relBlock / 64
	b2 := relBlock % 64
	w1 := w2 / 64
	b1 := w2 % 64
	w0 := w1 / 64
	b0 := w1 % 64

	// Already free?
	if a.l2[w2]&(1<<b2) != 0 {
		return
	}

	a.l2[w2] |= 1 << b2
	a.freeCount++
	a.dirty = true

	// Propagate upward if word was all-zero
	if a.l2[w2] == (1 << b2) {
		a.l1[w1] |= 1 << b1
		if a.l1[w1] == (1 << b1) {
			a.l0[w0] |= 1 << b0
		}
	}
}

// ReserveBlock marks a specific block as allocated (used during journal replay).
func (a *Allocator) ReserveBlock(relBlock uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.l0 == nil || relBlock >= a.blockCount {
		return
	}

	w2 := relBlock / 64
	b2 := relBlock % 64
	w1 := w2 / 64
	b1 := w2 % 64
	w0 := w1 / 64
	b0 := w1 % 64

	// Already allocated?
	if a.l2[w2]&(1<<b2) == 0 {
		return
	}

	a.l2[w2] &^= 1 << b2
	a.freeCount--
	a.dirty = true

	// Propagate upward if word becomes zero
	if a.l2[w2] == 0 {
		a.l1[w1] &^= 1 << b1
		if a.l1[w1] == 0 {
			a.l0[w0] &^= 1 << b0
		}
	}
}

// FreeCount returns the number of free blocks.
func (a *Allocator) FreeCount() uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.freeCount
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

	l0Blocks := (a.l0Words + wordsPerBlock - 1) / wordsPerBlock
	l1Blocks := (a.l1Words + wordsPerBlock - 1) / wordsPerBlock
	l2Blocks := (a.l2Words + wordsPerBlock - 1) / wordsPerBlock
	_ = l2Blocks // used for clarity

	writeDirty := func(offset uint64, words []uint64, nWords uint64) error {
		nBlks := (nWords + wordsPerBlock - 1) / wordsPerBlock
		for i := uint64(0); i < nBlks; i++ {
			buf := make([]byte, blockSize)
			start := i * wordsPerBlock
			n := nWords - start
			if n > wordsPerBlock {
				n = wordsPerBlock
			}
			for j := uint64(0); j < n; j++ {
				binary.LittleEndian.PutUint64(buf[j*8:], words[start+j])
			}
			if err := a.dev.WriteBlock(offset+i, buf); err != nil {
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
	binary.LittleEndian.PutUint32(hdr[0:], types.AllocMagic)
	binary.LittleEndian.PutUint32(hdr[4:], 1) // version
	binary.LittleEndian.PutUint64(hdr[8:], a.l0Words)
	binary.LittleEndian.PutUint64(hdr[16:], a.l1Words)
	binary.LittleEndian.PutUint64(hdr[24:], a.l2Words)
	binary.LittleEndian.PutUint64(hdr[32:], a.blockCount)
	binary.LittleEndian.PutUint64(hdr[40:], a.freeCount)
	if err := a.dev.WriteBlock(a.poolStart, hdr); err != nil {
		return fmt.Errorf("sync allocator header: %w", err)
	}

	a.dirty = false
	return nil
}

// InodeManager handles on-disk inode read/write.
type InodeManager struct {
	dev          *BlockDevice
	sb           *types.SuperblockLayout
	inodesPerBlk uint64
	tableStart   uint64
}

// NewInodeManager creates an InodeManager from the superblock.
func NewInodeManager(dev *BlockDevice, sb *types.SuperblockLayout) *InodeManager {
	return &InodeManager{
		dev:          dev,
		sb:           sb,
		inodesPerBlk: sb.BlockSize / sb.InodeSize,
		tableStart:   sb.InodeTableOffset,
	}
}

// inodeLocation computes the block and byte offset for a given inode number.
func (im *InodeManager) inodeLocation(ino uint64) (blockOffset uint64, byteOffset uint64) {
	idx := ino - 1
	blockOffset = im.tableStart + idx/im.inodesPerBlk
	byteOffset = (idx % im.inodesPerBlk) * im.sb.InodeSize
	return
}

// ReadInode reads and unmarshals an inode from disk.
func (im *InodeManager) ReadInode(ino uint64) (*types.Inode, error) {
	blk, off := im.inodeLocation(ino)
	buf, err := im.dev.ReadBlock(blk)
	if err != nil {
		return nil, fmt.Errorf("read inode %d block %d: %w", ino, blk, err)
	}
	inodeData := buf[off : off+im.sb.InodeSize]
	return types.UnmarshalInode(inodeData)
}

// WriteInode marshals and writes an inode to disk.
func (im *InodeManager) WriteInode(inode *types.Inode) error {
	blk, off := im.inodeLocation(inode.InodeNumber)
	buf, err := im.dev.ReadBlock(blk)
	if err != nil {
		return fmt.Errorf("read-modify-write inode %d block %d: %w", inode.InodeNumber, blk, err)
	}
	// Marshal inode data into the buffer at the correct offset
	{
		data := make([]byte, im.sb.InodeSize)
		pos := 0
		binary.LittleEndian.PutUint64(data[pos:], inode.InodeNumber); pos += 8
		binary.LittleEndian.PutUint64(data[pos:], inode.Magic); pos += 8
		binary.LittleEndian.PutUint32(data[pos:], inode.Filemode); pos += 4
		binary.LittleEndian.PutUint32(data[pos:], inode.Uid); pos += 4
		binary.LittleEndian.PutUint32(data[pos:], inode.Gid); pos += 4
		binary.LittleEndian.PutUint32(data[pos:], 0); pos += 4 // _Pad0
		binary.LittleEndian.PutUint64(data[pos:], inode.FileSize); pos += 8
		binary.LittleEndian.PutUint64(data[pos:], inode.CtimeSec); pos += 8
		binary.LittleEndian.PutUint64(data[pos:], inode.CtimeNsec); pos += 8
		binary.LittleEndian.PutUint64(data[pos:], inode.AtimeSec); pos += 8
		binary.LittleEndian.PutUint64(data[pos:], inode.AtimeNsec); pos += 8
		binary.LittleEndian.PutUint64(data[pos:], inode.MtimeSec); pos += 8
		binary.LittleEndian.PutUint64(data[pos:], inode.MtimeNsec); pos += 8
		binary.LittleEndian.PutUint64(data[pos:], inode.CreationTimeSec); pos += 8
		binary.LittleEndian.PutUint64(data[pos:], inode.CreationTimeNsec); pos += 8
		binary.LittleEndian.PutUint32(data[pos:], inode.Nlinks); pos += 4
		binary.LittleEndian.PutUint32(data[pos:], inode.NumExtentsInline); pos += 4
		binary.LittleEndian.PutUint64(data[pos:], inode.ExtentInlineBase); pos += 8
		binary.LittleEndian.PutUint64(data[pos:], inode.NumExtentsTotal); pos += 8

		for i := 0; i < 8; i++ {
			binary.LittleEndian.PutUint64(data[pos:], inode.InlineExtents[i].Offset); pos += 8
			binary.LittleEndian.PutUint64(data[pos:], inode.InlineExtents[i].Phys); pos += 8
			binary.LittleEndian.PutUint64(data[pos:], inode.InlineExtents[i].Len); pos += 8
			binary.LittleEndian.PutUint32(data[pos:], inode.InlineExtents[i].Flags); pos += 4
			binary.LittleEndian.PutUint32(data[pos:], inode.InlineExtents[i].Pad); pos += 4
		}

		binary.LittleEndian.PutUint64(data[pos:], inode.XattrOffset); pos += 8
		binary.LittleEndian.PutUint64(data[pos:], inode.XattrSize); pos += 8
		binary.LittleEndian.PutUint64(data[pos:], inode.ParentInode); pos += 8
		binary.LittleEndian.PutUint32(data[pos:], inode.Unused); pos += 4
		binary.LittleEndian.PutUint32(data[pos:], inode.Flags); pos += 4
		binary.LittleEndian.PutUint64(data[pos:], inode.DirTrieRoot); pos += 8
		binary.LittleEndian.PutUint64(data[pos:], inode.Rdev); pos += 8

		copy(data[pos:pos+80], inode.Reserved[:])

		copy(buf[off:], data)
	}

	return im.dev.WriteBlock(blk, buf)
}