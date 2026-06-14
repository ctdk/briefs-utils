package types

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestAllocHeaderUnmarshalBinary(t *testing.T) {
	buf := make([]byte, 4096)
	binary.LittleEndian.PutUint32(buf[0:], AllocMagic)
	binary.LittleEndian.PutUint32(buf[4:], 1)
	binary.LittleEndian.PutUint64(buf[8:], 3)   // L0Words
	binary.LittleEndian.PutUint64(buf[16:], 5)  // L1Words
	binary.LittleEndian.PutUint64(buf[24:], 154) // L2Words
	binary.LittleEndian.PutUint64(buf[32:], 9845)
	binary.LittleEndian.PutUint64(buf[40:], 9815)

	var h AllocHeader
	if err := h.UnmarshalBinary(buf); err != nil {
		t.Fatalf("UnmarshalBinary: %v", err)
	}

	if h.Magic != AllocMagic {
		t.Errorf("Magic: want 0x%X, got 0x%X", AllocMagic, h.Magic)
	}
	if h.Version != 1 {
		t.Errorf("Version: want 1, got %d", h.Version)
	}
	if h.L0Words != 3 {
		t.Errorf("L0Words: want 3, got %d", h.L0Words)
	}
	if h.L1Words != 5 {
		t.Errorf("L1Words: want 5, got %d", h.L1Words)
	}
	if h.L2Words != 154 {
		t.Errorf("L2Words: want 154, got %d", h.L2Words)
	}
	if h.BlockCount != 9845 {
		t.Errorf("BlockCount: want 9845, got %d", h.BlockCount)
	}
	if h.FreeCount != 9815 {
		t.Errorf("FreeCount: want 9815, got %d", h.FreeCount)
	}
}

func TestAllocHeaderUnmarshalTooShort(t *testing.T) {
	var h AllocHeader
	err := h.UnmarshalBinary([]byte{0, 0, 0, 0})
	if err == nil {
		t.Fatal("expected error for short data")
	}
}

func TestAllocBuilderBasic(t *testing.T) {
	b := NewAllocBuilder(1000)

	// All blocks should start free
	if b.FreeCount != 1000 {
		t.Errorf("FreeCount: want 1000, got %d", b.FreeCount)
	}

	// Mark block 0 as allocated (bit goes from 1 to 0)
	b.MarkAllocated(0)
	if b.FreeCount != 999 {
		t.Errorf("FreeCount after alloc: want 999, got %d", b.FreeCount)
	}

	// Mark block 999 as allocated
	b.MarkAllocated(999)
	if b.FreeCount != 998 {
		t.Errorf("FreeCount after second alloc: want 998, got %d", b.FreeCount)
	}

	// Mark block 0 free again (bit goes from 0 to 1)
	b.MarkFree(0)
	if b.FreeCount != 999 {
		t.Errorf("FreeCount after free: want 999, got %d", b.FreeCount)
	}

	// Verify L2 bit for block 0 is set (1 = free)
	w2 := uint64(0) / 64
	b2 := uint64(0) % 64
	if b.L2[w2]&(1<<b2) == 0 {
		t.Error("block 0 should be free after MarkFree (bit should be 1)")
	}

	// Verify L2 bit for block 999 is clear (0 = allocated)
	w2 = 999 / 64
	b2 = 999 % 64
	if b.L2[w2]&(1<<b2) != 0 {
		t.Error("block 999 should be allocated (bit should be 0)")
	}
}

func TestAllocBuilderMarkAllocatedTwice(t *testing.T) {
	b := NewAllocBuilder(100)
	b.MarkAllocated(5)
	b.MarkAllocated(5) // should be a no-op
	if b.FreeCount != 99 {
		t.Errorf("FreeCount: want 99, got %d", b.FreeCount)
	}
}

func TestAllocBuilderMarkFreeTwice(t *testing.T) {
	b := NewAllocBuilder(100)
	b.MarkAllocated(5)
	b.MarkFree(5)
	b.MarkFree(5) // should be a no-op
	if b.FreeCount != 100 {
		t.Errorf("FreeCount: want 100, got %d", b.FreeCount)
	}
}

func TestAllocBuilderWriteBlocks(t *testing.T) {
	b := NewAllocBuilder(1000)
	b.MarkAllocated(0)
	b.MarkAllocated(5)
	b.MarkAllocated(999)

	blocks := b.WriteBlocks()

	// First block should be the header
	if len(blocks) < 1 {
		t.Fatal("WriteBlocks returned no blocks")
	}

	magic := binary.LittleEndian.Uint32(blocks[0][0:])
	if magic != AllocMagic {
		t.Errorf("header magic: want 0x%X, got 0x%X", AllocMagic, magic)
	}

	// L2 starts after header + L0 blocks + L1 blocks
	l0Words := uint64(len(b.L0))
	l1Words := uint64(len(b.L1))
	l0Blocks := int((l0Words + 511) / 512)
	l1Blocks := int((l1Words + 511) / 512)
	l2Start := 1 + l0Blocks + l1Blocks

	if l2Start >= len(blocks) {
		t.Fatalf("L2 start %d beyond blocks %d", l2Start, len(blocks))
	}

	// Block 0 should be allocated (bit 0 = 0)
	l2Block := blocks[l2Start]
	bit0 := l2Block[0] & 1
	if bit0 != 0 {
		t.Error("block 0 should be allocated (bit 0 should be 0)")
	}

	// Block 5 should be allocated
	word5 := uint64(5) / 64
	bit5 := uint64(5) % 64
	wordVal := binary.LittleEndian.Uint64(l2Block[word5*8:])
	if wordVal&(1<<bit5) != 0 {
		t.Error("block 5 should be allocated (bit should be 0)")
	}
}

func TestAllocBuilderNbBlocks(t *testing.T) {
	tests := []struct {
		blocks uint64
		want   uint64
	}{
		{100, 4},    // tiny: 1 L0, 1 L1, 1 L2 + header = 4 blocks
		{1000, 4},   // small: 1 L0, 1 L1, 1 L2 + header = 4 blocks
		{10000, 4},  // 10000 -> 157 L2 words, 3 L1 words, 1 L0 word = 1+1+1+1 = 4
		{100000, 7}, // 100000 -> 1563 L2 words, 25 L1 words, 1 L0 word
	}

	for _, tc := range tests {
		b := NewAllocBuilder(tc.blocks)
		got := b.NbBlocks()
		if got != tc.want {
			t.Errorf("NbBlocks(%d): want %d, got %d", tc.blocks, tc.want, got)
		}
	}
}

func TestReadAllocatorHeader(t *testing.T) {
	b := NewAllocBuilder(1000)
	blocks := b.WriteBlocks()

	var pool bytes.Buffer
	for _, blk := range blocks {
		pool.Write(blk)
	}

	r := bytes.NewReader(pool.Bytes())
	hdr, err := ReadAllocatorHeader(r, 0, 4096)
	if err != nil {
		t.Fatalf("ReadAllocatorHeader: %v", err)
	}
	if hdr.Magic != AllocMagic {
		t.Errorf("Magic: want 0x%X, got 0x%X", AllocMagic, hdr.Magic)
	}
	if hdr.BlockCount != 1000 {
		t.Errorf("BlockCount: want 1000, got %d", hdr.BlockCount)
	}
	if hdr.FreeCount != 1000 {
		t.Errorf("FreeCount: want 1000, got %d", hdr.FreeCount)
	}
}

func TestReadAllocatorBitmap(t *testing.T) {
	b := NewAllocBuilder(1000)
	b.MarkAllocated(0)
	b.MarkAllocated(5)
	b.MarkAllocated(999)
	blocks := b.WriteBlocks()

	var pool bytes.Buffer
	for _, blk := range blocks {
		pool.Write(blk)
	}

	r := bytes.NewReader(pool.Bytes())
	_, _, l2, hdr, err := ReadAllocatorBitmap(r, 0, 4096)
	if err != nil {
		t.Fatalf("ReadAllocatorBitmap: %v", err)
	}

	if hdr.BlockCount != 1000 {
		t.Errorf("BlockCount: want 1000, got %d", hdr.BlockCount)
	}
	if hdr.FreeCount != 997 {
		t.Errorf("FreeCount: want 997, got %d", hdr.FreeCount)
	}

	// Verify L2 has the right bits
	if len(l2) == 0 {
		t.Fatal("L2 is empty")
	}

	// Block 0 should be allocated (bit 0 = 0)
	if l2[0]&1 != 0 {
		t.Error("block 0 should be allocated (bit 0 should be 0)")
	}

	// Block 5 should be allocated (bit 5 = 0)
	if l2[0]&(1<<5) != 0 {
		t.Error("block 5 should be allocated (bit 5 should be 0)")
	}

	// Block 999 should be allocated (bit = 0)
	w2 := uint64(999) / 64
	b2 := uint64(999) % 64
	if l2[w2]&(1<<b2) != 0 {
		t.Error("block 999 should be allocated (bit should be 0)")
	}
}

func TestReadAllocatorHeaderBadMagic(t *testing.T) {
	data := make([]byte, 4096)
	r := bytes.NewReader(data)
	_, err := ReadAllocatorHeader(r, 0, 4096)
	if err == nil {
		t.Fatal("expected error for bad magic")
	}
}
