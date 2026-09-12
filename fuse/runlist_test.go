package fuse

import (
	"errors"
	"testing"
)

// TestRunAccum covers the tail-merge invariants the run-encoded block lists
// rely on: adjacent blocks/runs merge, gaps start new runs, and the block
// sequence round-trips.
func TestRunAccum(t *testing.T) {
	var r runAccum

	if got := r.count(); got != 0 {
		t.Fatalf("empty: count=%d, want 0", got)
	}

	// addRun of a whole run, then adjacent blocks extending it.
	r.addRun(100, 3) // [100,103)
	r.addBlock(103)
	r.addBlock(104)
	if got := r.count(); got != 5 {
		t.Fatalf("after extend: count=%d, want 5", got)
	}
	if len(r.runs) != 1 {
		t.Fatalf("after extend: runs=%d, want 1 (merged)", len(r.runs))
	}

	// A gap starts a new run; an adjacent run merges into it.
	r.addRun(200, 4) // [200,204)
	r.addRun(204, 2) // adjacent: extends to [200,206)
	if len(r.runs) != 2 {
		t.Fatalf("after gap+merge: runs=%d, want 2", len(r.runs))
	}
	if r.runs[1].n != 6 {
		t.Fatalf("merged run length=%d, want 6", r.runs[1].n)
	}

	// Non-adjacent block: third run.
	r.addBlock(300)
	if len(r.runs) != 3 {
		t.Fatalf("after isolated block: runs=%d, want 3", len(r.runs))
	}

	// forEach visits every (first, n) pair in order; the block sequence
	// round-trips exactly.
	var blocks []uint64
	if err := r.forEach(func(first, n uint64) error {
		for i := uint64(0); i < n; i++ {
			blocks = append(blocks, first+i)
		}
		return nil
	}); err != nil {
		t.Fatalf("forEach: %v", err)
	}
	want := []uint64{100, 101, 102, 103, 104, 200, 201, 202, 203, 204, 205, 300}
	if len(blocks) != len(want) {
		t.Fatalf("round-trip: got %d blocks, want %d", len(blocks), len(want))
	}
	for i := range want {
		if blocks[i] != want[i] {
			t.Fatalf("round-trip: blocks[%d]=%d, want %d", i, blocks[i], want[i])
		}
	}

	// Duplicate block (offset by one) must NOT merge into the previous run
	// — addBlock only extends a run whose last block is x-1.
	var d runAccum
	d.addBlock(10)
	d.addBlock(10)
	if len(d.runs) != 2 || d.count() != 2 {
		t.Fatalf("duplicate: runs=%d count=%d, want 2/2", len(d.runs), d.count())
	}

	// Zero-length run is a no-op.
	var z runAccum
	z.addRun(5, 0)
	if z.count() != 0 || len(z.runs) != 0 {
		t.Fatalf("zero-length run: count=%d runs=%d, want 0/0", z.count(), len(z.runs))
	}

	// forEach error propagation.
	var e runAccum
	e.addRun(1, 2)
	sentinel := errors.New("stop")
	if err := e.forEach(func(first, n uint64) error { return sentinel }); err != sentinel {
		t.Fatalf("forEach error: got %v, want sentinel", err)
	}
}
