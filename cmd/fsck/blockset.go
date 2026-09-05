package main

import "sort"

// blockSet is the set of absolute blocks referenced by inode extents,
// B+tree node blocks, trie pages, and xattr blocks. Blocks arrive in long
// contiguous runs (extents are contiguous), so the set is stored as sorted,
// coalesced [start, end) intervals instead of one map entry per block: a
// fully-populated large volume would otherwise need billions of map
// entries just to record what fsck has seen. Intervals are appended
// unsorted and folded lazily, so the mark path stays O(1) amortized.
type blockSet struct {
	pending []blockInterval // appended by mark/markRange; folded by fold()
	merged  []blockInterval // sorted by start, non-overlapping, coalesced
	blocks  uint64          // distinct blocks covered by merged
}

type blockInterval struct {
	start, end uint64 // [start, end)
}

func newBlockSet() *blockSet { return &blockSet{} }

// mark records a single block.
func (s *blockSet) mark(b uint64) {
	s.pending = append(s.pending, blockInterval{b, b + 1})
}

// markRange records the n blocks starting at start (extent runs).
func (s *blockSet) markRange(start, n uint64) {
	if n == 0 {
		return
	}
	s.pending = append(s.pending, blockInterval{start, start + n})
}

// fold sorts and coalesces pending intervals into merged. Idempotent.
func (s *blockSet) fold() {
	if len(s.pending) == 0 {
		return
	}
	all := append(s.merged, s.pending...)
	sort.Slice(all, func(i, j int) bool { return all[i].start < all[j].start })

	// Coalesce in place: dst is a prefix of all, so extending dst[n-1].end
	// only touches already-consumed intervals.
	dst := all[:0]
	blocks := uint64(0)
	for _, iv := range all {
		if n := len(dst); n > 0 && iv.start <= dst[n-1].end {
			if iv.end > dst[n-1].end {
				dst[n-1].end = iv.end
			}
			continue
		}
		dst = append(dst, iv)
	}
	for _, iv := range dst {
		blocks += iv.end - iv.start
	}

	s.merged, s.pending, s.blocks = dst, nil, blocks
}

// has reports whether b is in the set.
func (s *blockSet) has(b uint64) bool {
	s.fold()
	// Find the first interval that could contain b (end > b); b is in the
	// set iff that interval starts at or before it.
	i := sort.Search(len(s.merged), func(i int) bool { return s.merged[i].end > b })
	return i < len(s.merged) && s.merged[i].start <= b
}

// count returns the number of distinct blocks in the set.
func (s *blockSet) count() uint64 {
	s.fold()
	return s.blocks
}

// each calls f for every distinct block, in ascending order.
func (s *blockSet) each(f func(b uint64)) {
	s.fold()
	for _, iv := range s.merged {
		for b := iv.start; b < iv.end; b++ {
			f(b)
		}
	}
}

// eachRange calls f for each maximal [start, end) run of blocks in the set,
// in ascending order. Callers that rebuild per-block state (the allocator
// rebuild) can loop the range themselves.
func (s *blockSet) eachRange(f func(start, end uint64)) {
	s.fold()
	for _, iv := range s.merged {
		f(iv.start, iv.end)
	}
}
