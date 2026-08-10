// Package briefs: shared allocator bit-math core.
//
// AllocBuilder (build-time/batch: mkfs, fsck repair) and fuse.Allocator
// (runtime: mutex + dev + dirty + search-with-repair) both maintain the same
// 3-level free-block bitmap pyramid but differ in everything around it. This
// file holds only the divergence-free bit math they share — clear/set an L2
// bit, propagate the change up through the L1/L0 summary words, test a bit,
// and compute the per-level word counts — so the two consumers cannot drift on
// the core invariant. The divergent search (AllocBlock's stale-summary repair
// vs AllocateBlock's plain scan), FreeBlock/Sync, and mutex/dirty handling
// stay on their respective types.
package briefs

// AllocMarkAllocated clears the L2 bit for relBlock, decrementing *freeCount,
// and propagates a now-empty word up through the L1 and L0 summaries. It is
// the bit math behind AllocBuilder.MarkAllocated and fuse.Allocator.ReserveBlock.
// It returns whether the block was newly allocated (its bit was free); a block
// already allocated, or one outside [0,blockCount), is a no-op returning false.
// The caller owns concurrency (AllocBuilder has no lock; fuse.Allocator holds
// its mutex) and any dirty tracking (fuse sets dirty when this returns true).
func AllocMarkAllocated(l0, l1, l2 []uint64, freeCount *uint64, blockCount, relBlock uint64) bool {
	if relBlock >= blockCount {
		return false
	}
	w2 := relBlock / 64
	b2 := relBlock % 64
	// Already allocated (bit clear)?
	if l2[w2]&(1<<b2) == 0 {
		return false
	}
	l2[w2] &^= 1 << b2
	*freeCount--

	// Propagate upward if the L2 word became fully allocated.
	if l2[w2] == 0 {
		w1 := w2 / 64
		b1 := w2 % 64
		l1[w1] &^= 1 << b1
		if l1[w1] == 0 {
			w0 := w1 / 64
			b0 := w1 % 64
			l0[w0] &^= 1 << b0
		}
	}
	return true
}

// AllocMarkFree sets the L2 bit for relBlock, incrementing *freeCount, and
// propagates a previously-empty word up through the L1 and L0 summaries. It is
// the bit math behind AllocBuilder.MarkFree and fuse.Allocator.FreeBlock. It
// returns whether the block was newly freed (its bit was clear); a block
// already free, or one outside [0,blockCount), is a no-op returning false.
func AllocMarkFree(l0, l1, l2 []uint64, freeCount *uint64, blockCount, relBlock uint64) bool {
	if relBlock >= blockCount {
		return false
	}
	w2 := relBlock / 64
	b2 := relBlock % 64
	// Already free (bit set)?
	if l2[w2]&(1<<b2) != 0 {
		return false
	}
	l2[w2] |= 1 << b2
	*freeCount++

	// Propagate upward if the L2 word went from empty to exactly this bit,
	// i.e. it was empty before. The same "did the word just become non-empty"
	// check applies to the L1 → L0 step.
	if l2[w2] == 1<<b2 {
		w1 := w2 / 64
		b1 := w2 % 64
		l1[w1] |= 1 << b1
		if l1[w1] == 1<<b1 {
			w0 := w1 / 64
			b0 := w1 % 64
			l0[w0] |= 1 << b0
		}
	}
	return true
}

// AllocIsAllocated reports whether relBlock is marked allocated (its L2 bit is
// clear). Out-of-range blocks report false. It is the bit test behind
// AllocBuilder.IsAllocated and fuse.Allocator.Allocated.
func AllocIsAllocated(l2 []uint64, blockCount, rel uint64) bool {
	if rel >= blockCount {
		return false
	}
	return l2[rel/64]&(1<<(rel%64)) == 0
}

// allocLevelWords returns the L0/L1/L2 word counts for a 3-level bitmap pyramid
// tracking blockCount blocks (64 blocks per L2 word, 64 L2 words per L1 word,
// 64 L1 words per L0 word), each level clamped to at least one word. It is used
// by NewAllocBuilder; the runtime Allocator reads its counts from the on-disk
// header instead.
func allocLevelWords(blockCount uint64) (l0, l1, l2 uint64) {
	l2 = (blockCount + 63) / 64
	l1 = (l2 + 63) / 64
	l0 = (l1 + 63) / 64
	if l0 < 1 {
		l0 = 1
	}
	if l1 < 1 {
		l1 = 1
	}
	if l2 < 1 {
		l2 = 1
	}
	return
}