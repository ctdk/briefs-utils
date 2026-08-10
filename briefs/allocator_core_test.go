package briefs

import (
	"math/rand"
	"testing"
)

// TestAllocCoreProperty runs a long random sequence of MarkAllocated/MarkFree
// against an AllocBuilder and a parallel reference bitset, asserting after
// every operation that the builder's free count matches the reference and, at
// the end, that every block's IsAllocated matches the reference. This is the
// safety net for the shared allocator bit-math core: AllocBuilder.MarkAllocated
// and MarkFree now delegate to AllocMarkAllocated/AllocMarkFree, so this
// validates the core's clear/set + L0/L1/L2 propagation against an independent
// model. Out-of-range blocks are mixed in to confirm the no-op guards.
func TestAllocCoreProperty(t *testing.T) {
	const blockCount = uint64(5000)
	const ops = 50000
	rng := rand.New(rand.NewSource(1))

	for _, ctor := range []string{"NewAllocBuilder", "NewDataAllocBuilder"} {
		var b *AllocBuilder
		free := make([]bool, blockCount)
		for i := range free {
			free[i] = true
		}
		refFree := blockCount
		if ctor == "NewAllocBuilder" {
			b = NewAllocBuilder(blockCount)
		} else {
			b = NewDataAllocBuilder(blockCount)
			// Block 0 is reserved as the ENOSPC sentinel.
			free[0] = false
			refFree--
		}

		for n := 0; n < ops; n++ {
			// Bias toward in-range blocks, but include out-of-range to exercise
			// the boundary guard.
			blk := rng.Uint64() % (blockCount + 50)
			if rng.Intn(2) == 0 {
				if blk < blockCount && free[blk] {
					free[blk] = false
					refFree--
				}
				b.MarkAllocated(blk)
			} else {
				if blk < blockCount && !free[blk] {
					free[blk] = true
					refFree++
				}
				b.MarkFree(blk)
			}
			if b.FreeCount != refFree {
				t.Fatalf("%s: op %d free_count drift: got %d, want %d", ctor, n, b.FreeCount, refFree)
			}
		}

		for i := uint64(0); i < blockCount; i++ {
			if got := b.IsAllocated(i); got == free[i] {
				t.Fatalf("%s: IsAllocated(%d)=%v, want allocated=%v (free=%v)",
					ctor, i, got, !free[i], free[i])
			}
		}
		// Out-of-range blocks are never allocated.
		if b.IsAllocated(blockCount + 5) {
			t.Fatalf("%s: out-of-range block reported allocated", ctor)
		}
		// FreeCount can never exceed blockCount or go negative (wrap).
		if b.FreeCount > blockCount {
			t.Fatalf("%s: free_count %d exceeds block count %d", ctor, b.FreeCount, blockCount)
		}
	}
}

// TestAllocLevelWords pins the per-level word-count computation, including the
// clamps at small block counts and the ceiling at the 64:1 fanout ratio.
func TestAllocLevelWords(t *testing.T) {
	cases := []struct {
		blockCount      uint64
		l0, l1, l2 uint64
	}{
		{0, 1, 1, 1},
		{1, 1, 1, 1},
		{64, 1, 1, 1},
		{65, 1, 1, 2},
		{4096, 1, 1, 64},  // 4096/64 = 64 L2 words, 1 L1, 1 L0
		{5000, 1, 2, 79},  // ceil(5000/64)=79 L2, ceil(79/64)=2 L1, 1 L0
		{1 << 18, 1, 64, 4096}, // 262144 blocks
	}
	for _, c := range cases {
		l0, l1, l2 := allocLevelWords(c.blockCount)
		if l0 != c.l0 || l1 != c.l1 || l2 != c.l2 {
			t.Fatalf("allocLevelWords(%d): got (%d,%d,%d), want (%d,%d,%d)",
				c.blockCount, l0, l1, l2, c.l0, c.l1, c.l2)
		}
	}
}