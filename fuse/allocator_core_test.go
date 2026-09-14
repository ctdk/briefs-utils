package fuse

import (
	"math/rand"
	"reflect"
	"testing"

	"github.com/ctdk/briefs-utils/briefs"
)

// TestAllocatorCoreMatchesBuilder seeds a fuse Allocator and an AllocBuilder
// from the same initial bitmap and runs the identical MarkAllocated/MarkFree
// sequence through both, asserting the bitmaps and free counts stay identical
// at every step. This validates that the runtime Allocator (ReserveBlock/
// FreeBlock) routes through the shared briefs bit-math core exactly as the
// build-time AllocBuilder does — the dedup this core exists to enforce. The
// divergent search (AllocBlock's stale-summary repair) is not exercised here;
// only the shared mark/free/test path.
func TestAllocatorCoreMatchesBuilder(t *testing.T) {
	const blockCount = uint64(5000)
	rng := rand.New(rand.NewSource(2))

	// Build an initial partially-allocated bitmap via an AllocBuilder.
	b := briefs.NewAllocBuilder(blockCount)
	for i := 0; i < 1000; i++ {
		b.MarkAllocated(rng.Uint64() % blockCount)
	}

	// Construct a fuse Allocator with the same initial bitmap (independent
	// copies of every level so mutations do not alias the builder).
	a := &Allocator{
		l0:         append([]uint64(nil), b.L0...),
		l1:         append([]uint64(nil), b.L1...),
		l2:         append([]uint64(nil), b.L2...),
		l0Words:    uint64(len(b.L0)),
		l1Words:    uint64(len(b.L1)),
		l2Words:    uint64(len(b.L2)),
		blockCount: b.BlockCount,
		freeCount:  b.FreeCount,
		// markL2Word needs a real words-per-block divisor; Sync is not
		// exercised here, so any positive multiple of 8 works.
		blockSize: 4096,
	}

	const ops = 50000
	for n := 0; n < ops; n++ {
		blk := rng.Uint64() % (blockCount + 50)
		if rng.Intn(2) == 0 {
			b.MarkAllocated(blk)
			a.ReserveBlock(blk)
		} else {
			b.MarkFree(blk)
			a.FreeBlock(blk)
		}
		if b.FreeCount != a.freeCount {
			t.Fatalf("op %d (blk %d): free_count drift builder=%d allocator=%d",
				n, blk, b.FreeCount, a.freeCount)
		}
	}

	if !reflect.DeepEqual(b.L0, a.l0) {
		t.Fatalf("L0 bitmap drift:\n builder=%v\nallocator=%v", b.L0, a.l0)
	}
	if !reflect.DeepEqual(b.L1, a.l1) {
		t.Fatalf("L1 bitmap drift:\n builder=%v\nallocator=%v", b.L1, a.l1)
	}
	if !reflect.DeepEqual(b.L2, a.l2) {
		t.Fatalf("L2 bitmap drift:\n builder=%v\nallocator=%v", b.L2, a.l2)
	}

	// Allocated() and IsAllocated() must agree on every block.
	for i := uint64(0); i < blockCount; i++ {
		if a.Allocated(i) != b.IsAllocated(i) {
			t.Fatalf("block %d: allocator.Allocated=%v builder.IsAllocated=%v",
				i, a.Allocated(i), b.IsAllocated(i))
		}
	}
	if a.Allocated(blockCount + 5) {
		t.Fatal("out-of-range block reported allocated by Allocator")
	}
}

// TestAllocMarkFreeRunMatchesPerBlock drives random ranges through
// AllocMarkFreeRun and through the equivalent per-block AllocMarkFree
// sequence on identical copies of a seeded bitmap, asserting the levels,
// free counts, and returned newly-freed tallies stay identical at every
// step. Ranges deliberately overlap, re-free, run past blockCount, start
// out of range, and use zero lengths — the edge shapes the run form must
// fold to the same no-ops the per-block form produces.
func TestAllocMarkFreeRunMatchesPerBlock(t *testing.T) {
	const blockCount = uint64(5000)
	rng := rand.New(rand.NewSource(7))

	// A partially-allocated seed bitmap, mirrored into run/per-block views.
	seed := briefs.NewAllocBuilder(blockCount)
	for i := 0; i < 1000; i++ {
		seed.MarkAllocated(rng.Uint64() % blockCount)
	}
	clone := func() (l0, l1, l2 []uint64, free uint64) {
		return append([]uint64(nil), seed.L0...),
			append([]uint64(nil), seed.L1...),
			append([]uint64(nil), seed.L2...),
			seed.FreeCount
	}

	for op := 0; op < 20000; op++ {
		// Runs up to ~3 words, plus occasional monsters past blockCount.
		first := rng.Uint64() % (blockCount + 100)
		n := rng.Uint64() % 200
		if rng.Intn(50) == 0 {
			n = blockCount // clamp path
		}
		if rng.Intn(20) == 0 {
			n = 0
		}

		runL0, runL1, runL2, runFree := clone()
		perL0, perL1, perL2, perFree := clone()

		freedRun := briefs.AllocMarkFreeRun(runL0, runL1, runL2, &runFree, blockCount, first, n)

		freedPer := uint64(0)
		for i := uint64(0); i < n; i++ {
			if briefs.AllocMarkFree(perL0, perL1, perL2, &perFree, blockCount, first+i) {
				freedPer++
			}
		}

		if freedRun != freedPer {
			t.Fatalf("op %d (first=%d n=%d): newly-freed tally run=%d per-block=%d",
				op, first, n, freedRun, freedPer)
		}
		if runFree != perFree {
			t.Fatalf("op %d (first=%d n=%d): free_count drift run=%d per-block=%d",
				op, first, n, runFree, perFree)
		}
		if !reflect.DeepEqual(runL0, perL0) || !reflect.DeepEqual(runL1, perL1) ||
			!reflect.DeepEqual(runL2, perL2) {
			t.Fatalf("op %d (first=%d n=%d): bitmap drift", op, first, n)
		}

		// The mirrored seed must follow the run result for the next op.
		seed.L0, seed.L1, seed.L2, seed.FreeCount = runL0, runL1, runL2, runFree
	}
}
