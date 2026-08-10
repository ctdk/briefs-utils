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