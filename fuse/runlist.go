package fuse

// Run-encoded block lists.  The write/fallocate/truncate paths used to
// materialize every touched block as one []uint64 entry — the rollback list
// (`allocated`), the free list (`freedAbs`), and the pending-free queue
// (`pendingFrees`).  generic/299 cycles four whole-device files through
// falloc/truncate (~26M blocks each), so each list alone held ~200 MB and the
// three together, with append doubling and the GC goal, drove the daemon to
// an OOM kill at a ~3.2 GB anon-rss ceiling.  The kernel never materializes
// per-block lists: it journals per extent (btree.c briefs_journal_extent_alloc
// takes ext->len) and frees per extent (briefs_free_blocks_range takes a
// length).  blockRun/runAccum give the bridge the same shape — one entry per
// maximal contiguous run, tail-merged as per-block allocators hand out
// consecutive blocks.

// blockRun is one maximal contiguous run of blocks.  Whether `first` is
// absolute or data-relative follows the list it lives in (allocated is
// data-relative, freed/pending lists absolute or relative per their fields).
type blockRun struct {
	first uint64
	n     uint64
}

// runAccum accumulates a block list as contiguous runs.  addBlock tail-merges
// an adjacent block into the last run (O(1) amortized); addRun merges a whole
// adjacent run.  Non-adjacent appends just start a new run — the list is
// bounded by the number of fragmentation boundaries, not the block count.
type runAccum struct {
	runs []blockRun
}

// addBlock adds one block, extending the last run when contiguous.
func (r *runAccum) addBlock(x uint64) {
	if k := len(r.runs); k > 0 {
		last := &r.runs[k-1]
		if last.first+last.n == x {
			last.n++
			return
		}
	}
	r.runs = append(r.runs, blockRun{first: x, n: 1})
}

// addRun adds [first, first+n), extending the last run when adjacent.
// n == 0 is a no-op.  Runs must not overlap what is already accumulated.
func (r *runAccum) addRun(first, n uint64) {
	if n == 0 {
		return
	}
	if k := len(r.runs); k > 0 {
		last := &r.runs[k-1]
		if last.first+last.n == first {
			last.n += n
			return
		}
	}
	r.runs = append(r.runs, blockRun{first: first, n: n})
}

// count returns the total number of blocks across all runs.
func (r *runAccum) count() uint64 {
	var n uint64
	for _, run := range r.runs {
		n += run.n
	}
	return n
}

// forEach calls f for every run in order.
func (r *runAccum) forEach(f func(first, n uint64) error) error {
	for _, run := range r.runs {
		if err := f(run.first, run.n); err != nil {
			return err
		}
	}
	return nil
}
