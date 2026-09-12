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

// openTestAllocator builds an allocator pool from b, writes it to a fresh
// temp image, and opens it (the hand-rolled sequence every other allocator
// test repeats, parameterized once for the AllocBlocks tests).
func openTestAllocator(t *testing.T, b *briefs.AllocBuilder) *Allocator {
	t.Helper()
	path := tempImage(t, 100)
	raw, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	for i, blk := range b.WriteBlocks() {
		if _, err := raw.WriteAt(blk, int64(i)*4096); err != nil {
			raw.Close()
			t.Fatalf("WriteAt block %d: %v", i, err)
		}
	}
	raw.Close()

	bd, _, err := OpenBlockDevice(path)
	if err != nil {
		t.Fatalf("OpenBlockDevice: %v", err)
	}
	t.Cleanup(func() { bd.Close() })

	a, err := OpenAllocator(bd, 0)
	if err != nil {
		t.Fatalf("OpenAllocator: %v", err)
	}
	return a
}

func TestAllocatorAllocBlocks(t *testing.T) {
	// Fresh 1000-block pool: AllocBlocks(5) takes one 5-block run starting
	// at 1 -- block 0 is reserved as the ENOSPC sentinel on first use, so a
	// run never starts there.
	a := openTestAllocator(t, briefs.NewAllocBuilder(1000))
	if got := a.AllocBlocks(5); got != 1 {
		t.Fatalf("AllocBlocks(5) = %d, want 1", got)
	}
	if fc := a.FreeCount(); fc != 1000-6 {
		t.Errorf("FreeCount = %d, want %d (5-block run + sentinel)", fc, 1000-6)
	}
	if !a.Allocated(1) || !a.Allocated(5) || a.Allocated(6) {
		t.Errorf("run [1,6) not marked allocated in the bitmap")
	}
	// The next run is contiguous with the first.
	if got := a.AllocBlocks(3); got != 6 {
		t.Fatalf("AllocBlocks(3) = %d, want 6", got)
	}
	// Zero / over-count requests fail without side effects.
	before := a.FreeCount()
	if got := a.AllocBlocks(0); got != 0 {
		t.Errorf("AllocBlocks(0) = %d, want 0", got)
	}
	if got := a.AllocBlocks(1001); got != 0 {
		t.Errorf("AllocBlocks(1001) = %d, want 0 (exceeds block count)", got)
	}
	if a.FreeCount() != before {
		t.Errorf("failed AllocBlocks changed FreeCount: %d -> %d", before, a.FreeCount())
	}
}

func TestAllocatorAllocBlocksCrossWord(t *testing.T) {
	// 200-block pool with only blocks 63..65 free: the run spans the L2
	// word boundary at block 64 and must still be found.
	b := briefs.NewAllocBuilder(200)
	for blk := uint64(0); blk < 200; blk++ {
		if blk < 63 || blk > 65 {
			b.MarkAllocated(blk)
		}
	}
	a := openTestAllocator(t, b)
	if got := a.AllocBlocks(3); got != 63 {
		t.Fatalf("AllocBlocks(3) = %d, want 63 (cross-word run)", got)
	}
	if fc := a.FreeCount(); fc != 0 {
		t.Errorf("FreeCount = %d, want 0", fc)
	}
	if got := a.AllocBlocks(1); got != 0 {
		t.Errorf("AllocBlocks(1) = %d, want 0 (exhausted)", got)
	}
}

func TestAllocatorAllocBlocksFragmented(t *testing.T) {
	// Every other block free: no contiguous run of 2 exists anywhere.
	b := briefs.NewAllocBuilder(200)
	for blk := uint64(0); blk < 200; blk += 2 {
		b.MarkAllocated(blk)
	}
	a := openTestAllocator(t, b)
	if got := a.AllocBlocks(2); got != 0 {
		t.Fatalf("AllocBlocks(2) = %d, want 0 (bitmap fragmented)", got)
	}
	// A single-block run still fits.
	if got := a.AllocBlocks(1); got != 1 {
		t.Fatalf("AllocBlocks(1) = %d, want 1 (first free odd block)", got)
	}
}

func TestAllocatorAllocRunsUpTo(t *testing.T) {
	// 200-block pool with free runs [10,15), [40,43), [100,107): no run of
	// 10 exists, so AllocBlocks fails and AllocRunsUpTo harvests 5 + 3 + a
	// 2-block prefix of the last run.
	b := briefs.NewAllocBuilder(200)
	for blk := uint64(0); blk < 200; blk++ {
		if !((blk >= 10 && blk < 15) || (blk >= 40 && blk < 43) || (blk >= 100 && blk < 107)) {
			b.MarkAllocated(blk)
		}
	}
	a := openTestAllocator(t, b)
	if got := a.AllocBlocks(10); got != 0 {
		t.Fatalf("AllocBlocks(10) = %d, want 0 (fragmented)", got)
	}
	runs := a.AllocRunsUpTo(10)
	want := []blockRun{{first: 10, n: 5}, {first: 40, n: 3}, {first: 100, n: 2}}
	if len(runs) != len(want) {
		t.Fatalf("AllocRunsUpTo(10) = %+v, want %+v", runs, want)
	}
	for i := range want {
		if runs[i] != want[i] {
			t.Fatalf("AllocRunsUpTo(10) = %+v, want %+v", runs, want)
		}
	}
	if fc := a.FreeCount(); fc != 5 {
		t.Errorf("FreeCount = %d, want 5 (15 free - 10 harvested)", fc)
	}
	for _, r := range want {
		for i := uint64(0); i < r.n; i++ {
			if !a.Allocated(r.first + i) {
				t.Errorf("harvested block %d not marked allocated", r.first+i)
			}
		}
	}
	// The un-taken tail of [100,107) stays free.
	if a.Allocated(102) || !a.Allocated(100) {
		t.Errorf("prefix truncation wrong: 100 allocated=%v, 102 allocated=%v",
			a.Allocated(100), a.Allocated(102))
	}
	// Re-harvest takes the remainder, then the pool is exhausted.
	runs = a.AllocRunsUpTo(10)
	if len(runs) != 1 || runs[0] != (blockRun{first: 102, n: 5}) {
		t.Fatalf("re-harvest = %+v, want [{102 5}]", runs)
	}
	if runs = a.AllocRunsUpTo(1); len(runs) != 0 {
		t.Fatalf("exhausted AllocRunsUpTo(1) = %+v, want none", runs)
	}
}

func TestAllocatorAllocRunsUpToCrossWord(t *testing.T) {
	// Only blocks 62..65 free: one run spanning the L2 word boundary at
	// block 64 must be harvested as a single run.
	b := briefs.NewAllocBuilder(200)
	for blk := uint64(0); blk < 200; blk++ {
		if blk < 62 || blk > 65 {
			b.MarkAllocated(blk)
		}
	}
	a := openTestAllocator(t, b)
	runs := a.AllocRunsUpTo(4)
	if len(runs) != 1 || runs[0] != (blockRun{first: 62, n: 4}) {
		t.Fatalf("AllocRunsUpTo(4) = %+v, want [{62 4}] (cross-word run)", runs)
	}
	if fc := a.FreeCount(); fc != 0 {
		t.Errorf("FreeCount = %d, want 0", fc)
	}
}

func TestAllocatorAllocRunsUpToSentinel(t *testing.T) {
	// Fresh pool: block 0 is the ENOSPC sentinel and is reserved on first
	// use, so the first harvested run starts at 1.
	a := openTestAllocator(t, briefs.NewAllocBuilder(1000))
	runs := a.AllocRunsUpTo(3)
	if len(runs) != 1 || runs[0] != (blockRun{first: 1, n: 3}) {
		t.Fatalf("AllocRunsUpTo(3) = %+v, want [{1 3}]", runs)
	}
	if fc := a.FreeCount(); fc != 1000-4 {
		t.Errorf("FreeCount = %d, want %d (run + sentinel)", fc, 1000-4)
	}
}
