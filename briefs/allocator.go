// Package briefs defines the BrieFS on-disk format.
package briefs

// AllocBuilder builds a 3-level bitmap pyramid for free block tracking.
//
// Level 0 (summary):      1 bit covers 64 Level 1 words  (4096 blocks per bit)
// Level 1 (summary):      1 bit covers 64 Level 2 words  (64 blocks per bit)
// Level 2 (blocks):        1 bit covers 1 block
//
// Everything is a flat array of u64 words. Each word is 64 bits.
// A set bit = "has free blocks". A cleared bit = "fully allocated".
//
// The number of levels is determined at build time and is always 3 for the
// sizes we support. Going to 4 levels later is trivial: compute words per level
// the same way until words == 1 (the root single word).
import (
	"encoding/binary"
	"fmt"
	"io"
	"math/bits"
)

// AllocMagic is the magic number for the allocator pool header block.
const AllocMagic = 0x4249544D // "BITM"

// AllocHeader is the on-disk header for the allocator pool (first block of the
// pool). It is exactly 48 bytes, matching the kernel's struct
// alloc_pool_header.
//
//go:briefs-disk size=48
type AllocHeader struct {
	Magic      uint32 // "BITM"
	Version    uint32 // 1
	L0Words    uint64 // words in level 0
	L1Words    uint64 // words in level 1
	L2Words    uint64 // words in level 2
	BlockCount uint64 // total data blocks tracked
	FreeCount  uint64 // total free blocks
}

// AllocBuilder builds the 3-level bitmap allocator.
type AllocBuilder struct {
	L0         []uint64 // level 0 summary words
	L1         []uint64 // level 1 summary words
	L2         []uint64 // level 2 block bitmaps
	BlockCount uint64
	FreeCount  uint64
}

// NewAllocBuilder creates a builder for the given number of blocks.
// All blocks start free. This is used for inode allocators where no
// block reservation is needed.
func NewAllocBuilder(dataBlockCount uint64) *AllocBuilder {
	l2Words := (dataBlockCount + 63) / 64
	l1Words := (l2Words + 63) / 64
	l0Words := (l1Words + 63) / 64

	if l0Words < 1 {
		l0Words = 1
	}
	if l1Words < 1 {
		l1Words = 1
	}
	if l2Words < 1 {
		l2Words = 1
	}

	b := &AllocBuilder{
		L0:         make([]uint64, l0Words),
		L1:         make([]uint64, l1Words),
		L2:         make([]uint64, l2Words),
		BlockCount: dataBlockCount,
		FreeCount:  dataBlockCount,
	}

	// Initialize: all bits set (all blocks free)
	for i := range b.L2 {
		// Last word may have trailing bits beyond BlockCount
		b.L2[i] = ^uint64(0)
	}
	// Clear trailing bits in the last L2 word that don't correspond to real blocks
	if tail := dataBlockCount % 64; tail != 0 {
		b.L2[len(b.L2)-1] = (1 << tail) - 1
	}

	// All L1 words are all-ones because all L2 words are non-zero
	for i := range b.L1 {
		b.L1[i] = ^uint64(0)
	}
	// Clear trailing bits in the last L1 word for non-existent L2 words
	if tail := l2Words % 64; tail != 0 {
		b.L1[len(b.L1)-1] = (1 << tail) - 1
	}

	// All L0 words are all-ones because all L1 words are non-zero
	for i := range b.L0 {
		b.L0[i] = ^uint64(0)
	}
	if tail := l1Words % 64; tail != 0 {
		b.L0[len(b.L0)-1] = (1 << tail) - 1
	}

	return b
}

// NewDataAllocBuilder creates a builder for data blocks with block 0
// reserved as the ENOSPC sentinel (matching kernel behavior).
// Block 0 is never allocated for data; AllocateBlock returns >= 1 on success.
func NewDataAllocBuilder(dataBlockCount uint64) *AllocBuilder {
	b := NewAllocBuilder(dataBlockCount)

	// Reserve block 0 as the ENOSPC sentinel (matches kernel behavior).
	// This ensures the allocator never returns 0, which is the failure sentinel.
	if dataBlockCount > 0 {
		b.MarkAllocated(0)
	}

	return b
}

// NbBlocks returns the number of 4096-byte blocks needed for the allocator pool
// (header + L0 + L1 + L2).
func (b *AllocBuilder) NbBlocks() uint64 {
	header := uint64(1)
	total := header + b.wordBlocks(b.L0) + b.wordBlocks(b.L1) + b.wordBlocks(b.L2)
	return total
}

func (b *AllocBuilder) wordBlocks(words []uint64) uint64 {
	bytes := len(words) * 8
	blk := uint64(bytes + 4095) / 4096
	if blk < 1 {
		return 1
	}
	return blk
}

// MarkAllocated marks a block (data-relative, 0-based) as allocated.
func (b *AllocBuilder) MarkAllocated(relBlock uint64) {
	if relBlock >= b.BlockCount {
		return
	}

	w2 := relBlock / 64
	b2 := relBlock % 64
	w1 := w2 / 64
	b1 := w2 % 64
	w0 := w1 / 64
	b0 := w1 % 64

	// Already allocated?
	if b.L2[w2]&(1<<b2) == 0 {
		return
	}

	wasNonZero := b.L2[w2] != 0

	b.L2[w2] &^= 1 << b2
	b.FreeCount--

	if wasNonZero && b.L2[w2] == 0 {
		b.L1[w1] &^= 1 << b1
		if b.L1[w1] == 0 {
			b.L0[w0] &^= 1 << b0
		}
	}
}

// MarkFree marks a block (data-relative, 0-based) as free.
func (b *AllocBuilder) MarkFree(relBlock uint64) {
	if relBlock >= b.BlockCount {
		return
	}

	w2 := relBlock / 64
	b2 := relBlock % 64
	w1 := w2 / 64
	b1 := w2 % 64
	w0 := w1 / 64
	b0 := w1 % 64

	// Already free?
	if b.L2[w2]&(1<<b2) != 0 {
		return
	}

	wasAllZero := b.L2[w2] == 0

	b.L2[w2] |= 1 << b2
	b.FreeCount++

	if wasAllZero {
		b.L1[w1] |= 1 << b1
		if b.L0[w0]&(1<<b0) == 0 {
			b.L0[w0] |= 1 << b0
		}
	}
}

// IsAllocated reports whether a block (data-relative, 0-based) is currently
// marked allocated. A set L2 bit means "free"; a cleared bit means "allocated",
// so the block is allocated when the bit is zero. Out-of-range blocks are
// reported as not allocated.
func (b *AllocBuilder) IsAllocated(relBlock uint64) bool {
	if relBlock >= b.BlockCount {
		return false
	}
	w2 := relBlock / 64
	b2 := relBlock % 64
	return b.L2[w2]&(1<<b2) == 0
}

// AllocateBlock finds a single free data-relative block and marks it allocated.
// It returns the relative block number, or an error if no blocks are free.
func (b *AllocBuilder) AllocateBlock() (uint64, error) {
	if b.FreeCount == 0 {
		return 0, fmt.Errorf("allocator has no free blocks")
	}

	for w0 := uint64(0); w0 < uint64(len(b.L0)); w0++ {
		if b.L0[w0] == 0 {
			continue
		}
		b0 := bits.TrailingZeros64(b.L0[w0])

		w1 := w0*64 + uint64(b0)
		if w1 >= uint64(len(b.L1)) {
			continue
		}
		l1Word := b.L1[w1]
		if l1Word == 0 {
			continue
		}
		b1 := bits.TrailingZeros64(l1Word)

		w2 := w1*64 + uint64(b1)
		if w2 >= uint64(len(b.L2)) {
			continue
		}
		l2Word := b.L2[w2]
		if l2Word == 0 {
			continue
		}
		b2 := bits.TrailingZeros64(l2Word)

		block := w2*64 + uint64(b2)
		if block >= b.BlockCount {
			continue
		}

		b.MarkAllocated(block)
		return block, nil
	}

	return 0, fmt.Errorf("allocator search exhausted despite free_count=%d", b.FreeCount)
}

// WriteBlocks packs the allocator into blocks. Returns one []byte per block.
// Block 0 is the header. Blocks 1.. are L0, L1, L2 packed.
func (b *AllocBuilder) WriteBlocks() [][]byte {
	nb := b.NbBlocks()
	blocks := make([][]byte, nb)

	// Block 0: header
	hdr := &AllocHeader{
		Magic:      AllocMagic,
		Version:    1,
		L0Words:    uint64(len(b.L0)),
		L1Words:    uint64(len(b.L1)),
		L2Words:    uint64(len(b.L2)),
		BlockCount: b.BlockCount,
		FreeCount:  b.FreeCount,
	}
	hdrBuf := make([]byte, 4096)
	hdrData, _ := hdr.MarshalBinary()
	copy(hdrBuf[:48], hdrData)
	blocks[0] = hdrBuf

	// Pack words into blocks
	b.packWords(blocks, b.L0, 1)
	l0Blocks := b.wordBlocks(b.L0)
	b.packWords(blocks, b.L1, 1+l0Blocks)
	l1Blocks := b.wordBlocks(b.L1)
	b.packWords(blocks, b.L2, 1+l0Blocks+l1Blocks)

	return blocks
}

func (b *AllocBuilder) packWords(blocks [][]byte, words []uint64, startBlock uint64) {
	wordsPerBlock := uint64(4096 / 8) // 512
	for i := uint64(0); i < uint64(len(words)); i += wordsPerBlock {
		end := i + wordsPerBlock
		if end > uint64(len(words)) {
			end = uint64(len(words))
		}
		buf := make([]byte, 4096)
		for j := i; j < end; j++ {
			binary.LittleEndian.PutUint64(buf[(j-i)*8:], words[j])
		}
		blocks[startBlock+i/wordsPerBlock] = buf
	}
}

// ReadAllocatorHeader reads and parses an allocator pool header from disk.
func ReadAllocatorHeader(r io.ReaderAt, poolBlock, blockSize uint64) (*AllocHeader, error) {
	buf := make([]byte, blockSize)
	if _, err := r.ReadAt(buf, int64(poolBlock*blockSize)); err != nil {
		return nil, fmt.Errorf("read allocator header at block %d: %w", poolBlock, err)
	}
	h := &AllocHeader{}
	if err := h.UnmarshalBinary(buf); err != nil {
		return nil, err
	}
	if h.Magic != AllocMagic {
		return nil, fmt.Errorf("bad allocator magic at block %d: 0x%08X (expected 0x%08X)",
			poolBlock, h.Magic, AllocMagic)
	}
	return h, nil
}

// ReadAllocatorBitmap reads all three levels of an allocator bitmap from disk.
// Returns the L0, L1, and L2 word arrays, and the pool header.
func ReadAllocatorBitmap(r io.ReaderAt, poolBlock, blockSize uint64) (l0, l1, l2 []uint64, hdr *AllocHeader, err error) {
	hdr, err = ReadAllocatorHeader(r, poolBlock, blockSize)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	wordsPerBlock := blockSize / 8

	// Read L0 blocks
	l0Blocks := (hdr.L0Words + wordsPerBlock - 1) / wordsPerBlock
	l0 = make([]uint64, hdr.L0Words)
	for i := uint64(0); i < l0Blocks; i++ {
		buf := make([]byte, blockSize)
		if _, err := r.ReadAt(buf, int64((poolBlock+1+i)*blockSize)); err != nil {
			return nil, nil, nil, nil, fmt.Errorf("read L0 block %d: %w", poolBlock+1+i, err)
		}
		start := i * wordsPerBlock
		for j := uint64(0); j < wordsPerBlock && start+j < hdr.L0Words; j++ {
			l0[start+j] = binary.LittleEndian.Uint64(buf[j*8:])
		}
	}

	// Read L1 blocks
	l1Start := poolBlock + 1 + l0Blocks
	l1Blocks := (hdr.L1Words + wordsPerBlock - 1) / wordsPerBlock
	l1 = make([]uint64, hdr.L1Words)
	for i := uint64(0); i < l1Blocks; i++ {
		buf := make([]byte, blockSize)
		if _, err := r.ReadAt(buf, int64((l1Start+i)*blockSize)); err != nil {
			return nil, nil, nil, nil, fmt.Errorf("read L1 block %d: %w", l1Start+i, err)
		}
		start := i * wordsPerBlock
		for j := uint64(0); j < wordsPerBlock && start+j < hdr.L1Words; j++ {
			l1[start+j] = binary.LittleEndian.Uint64(buf[j*8:])
		}
	}

	// Read L2 blocks
	l2Start := l1Start + l1Blocks
	l2Blocks := (hdr.L2Words + wordsPerBlock - 1) / wordsPerBlock
	l2 = make([]uint64, hdr.L2Words)
	for i := uint64(0); i < l2Blocks; i++ {
		buf := make([]byte, blockSize)
		if _, err := r.ReadAt(buf, int64((l2Start+i)*blockSize)); err != nil {
			return nil, nil, nil, nil, fmt.Errorf("read L2 block %d: %w", l2Start+i, err)
		}
		start := i * wordsPerBlock
		for j := uint64(0); j < wordsPerBlock && start+j < hdr.L2Words; j++ {
			l2[start+j] = binary.LittleEndian.Uint64(buf[j*8:])
		}
	}

	return
}
