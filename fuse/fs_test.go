package fuse

import (
	"os"
	"testing"

	"github.com/ctdk/briefs-utils/briefs"
)

func TestAllocatorOpen(t *testing.T) {
	// Build an allocator pool in memory, write it to a temp file, open it
	path := tempImage(t, 100)

	b := briefs.NewAllocBuilder(1000)
	blocks := b.WriteBlocks()

	raw, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	for i, blk := range blocks {
		if _, err := raw.WriteAt(blk, int64(i)*4096); err != nil {
			t.Fatalf("WriteAt block %d: %v", i, err)
		}
	}
	raw.Close()

	bd, _, err := OpenBlockDevice(path)
	if err != nil {
		t.Fatalf("OpenBlockDevice: %v", err)
	}
	defer bd.Close()

	a, err := OpenAllocator(bd, 0)
	if err != nil {
		t.Fatalf("OpenAllocator: %v", err)
	}

	if a.FreeCount() != 1000 {
		t.Errorf("FreeCount: want 1000, got %d", a.FreeCount())
	}
	if a.TotalBlocks() != 1000 {
		t.Errorf("TotalBlocks: want 1000, got %d", a.TotalBlocks())
	}
}

func TestAllocatorAllocFree(t *testing.T) {
	path := tempImage(t, 100)

	b := briefs.NewAllocBuilder(100)
	blocks := b.WriteBlocks()

	raw, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	for i, blk := range blocks {
		if _, err := raw.WriteAt(blk, int64(i)*4096); err != nil {
			t.Fatalf("WriteAt block %d: %v", i, err)
		}
	}
	raw.Close()

	bd, _, err := OpenBlockDevice(path)
	if err != nil {
		t.Fatalf("OpenBlockDevice: %v", err)
	}
	defer bd.Close()

	a, err := OpenAllocator(bd, 0)
	if err != nil {
		t.Fatalf("OpenAllocator: %v", err)
	}

	// Allocate a block (block 0 is valid — 0 is the sentinel for out-of-space)
	blk := a.AllocBlock()
	if a.FreeCount() != 99 {
		t.Errorf("FreeCount after alloc: want 99, got %d", a.FreeCount())
	}

	// Free it
	a.FreeBlock(blk)
	if a.FreeCount() != 100 {
		t.Errorf("FreeCount after free: want 100, got %d", a.FreeCount())
	}

	// Free again should be a no-op
	a.FreeBlock(blk)
	if a.FreeCount() != 100 {
		t.Errorf("FreeCount after double free: want 100, got %d", a.FreeCount())
	}
}

func TestAllocatorReserve(t *testing.T) {
	path := tempImage(t, 100)

	b := briefs.NewAllocBuilder(100)
	blocks := b.WriteBlocks()

	raw, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	for i, blk := range blocks {
		if _, err := raw.WriteAt(blk, int64(i)*4096); err != nil {
			t.Fatalf("WriteAt block %d: %v", i, err)
		}
	}
	raw.Close()

	bd, _, err := OpenBlockDevice(path)
	if err != nil {
		t.Fatalf("OpenBlockDevice: %v", err)
	}
	defer bd.Close()

	a, err := OpenAllocator(bd, 0)
	if err != nil {
		t.Fatalf("OpenAllocator: %v", err)
	}

	// Reserve block 5
	a.ReserveBlock(5)
	if a.FreeCount() != 99 {
		t.Errorf("FreeCount after reserve: want 99, got %d", a.FreeCount())
	}

	// Reserve again should be a no-op
	a.ReserveBlock(5)
	if a.FreeCount() != 99 {
		t.Errorf("FreeCount after double reserve: want 99, got %d", a.FreeCount())
	}
}

func TestAllocatorExhaustion(t *testing.T) {
	path := tempImage(t, 100)

	b := briefs.NewAllocBuilder(10)
	blocks := b.WriteBlocks()

	raw, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	for i, blk := range blocks {
		if _, err := raw.WriteAt(blk, int64(i)*4096); err != nil {
			t.Fatalf("WriteAt block %d: %v", i, err)
		}
	}
	raw.Close()

	bd, _, err := OpenBlockDevice(path)
	if err != nil {
		t.Fatalf("OpenBlockDevice: %v", err)
	}
	defer bd.Close()

	a, err := OpenAllocator(bd, 0)
	if err != nil {
		t.Fatalf("OpenAllocator: %v", err)
	}

	// Allocate all 10 blocks (0 is a valid block, so we check free count)
	for i := 0; i < 10; i++ {
		blk := a.AllocBlock()
		if a.FreeCount() != uint64(9-i) {
			t.Fatalf("iteration %d: FreeCount want %d, got %d", i, 9-i, a.FreeCount())
		}
		_ = blk
	}

	// Next alloc should return 0 (out of space)
	blk := a.AllocBlock()
	if blk != 0 {
		t.Errorf("AllocBlock: want 0 (out of space), got %d", blk)
	}
	if a.FreeCount() != 0 {
		t.Errorf("FreeCount: want 0, got %d", a.FreeCount())
	}
}
