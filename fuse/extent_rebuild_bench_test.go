package fuse

// Perf probe for the 074 fragmented-write workload (512-byte fragments
// at 1 KB stride into a 30 MB file, ~7.7K extents).  Not a correctness
// test; it logs the per-op cost curve so write-path regressions show up
// as a slope change.  Skipped unless BRIEFS_PERF_PROBES=1 (and under
// -short).
//
// Measured on this code, 2026-09-11 (dev machine, in-process writes):
//   full rebuild per op        ~4500 us/op and up with tree depth
//   localized rebuild (8643303)  8-9 us/op pass 1 (extent inserts)
//   + walked-tree cache           7-9 us/op pass 1, ~5 us/op pass 2
// Pass 2 rewrites the same fragments (within-extent hits, no insert),
// isolating the per-op walk cost from the rebuild cost.

import (
	"os"
	"testing"
	"time"

	"github.com/ctdk/briefs-utils/briefs"
)

func TestFragmentedWritePerf(t *testing.T) {
	if testing.Short() || os.Getenv("BRIEFS_PERF_PROBES") == "" {
		t.Skip("perf probe (set BRIEFS_PERF_PROBES=1 to run)")
	}
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 60000)
	b := openBridge(t, img)

	in, err := b.createInDir(1, "frag", briefs.ModeFile|0o644, 1000, 1000, false, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ino := in.InodeNumber

	const bs = 4096
	const frag = 512
	const stride = 1024
	const size = 30 << 20
	data := makePattern(1, frag)

	nOps := 0
	start := time.Now()
	lastMark := start
	var marks []string
	for off := 0; off < size; off += stride {
		writeFile(t, b, ino, data, int64(off))
		nOps++
		if nOps%5000 == 0 {
			now := time.Now()
			marks = append(marks, time.Since(lastMark).String())
			lastMark = now
		}
	}
	total := time.Since(start)
	t.Logf("pass 1 (fragmented writes): %d ops in %s (%.1f us/op avg)", nOps, total, float64(total.Microseconds())/float64(nOps))
	t.Logf("per-5K-op marks: %v", marks)

	// Pass 2: same writes again — full-block-overwrite / within-extent hits
	// (no extent insert), isolating the per-op walk cost from the rebuild.
	start = time.Now()
	for off := 0; off < size; off += stride {
		writeFile(t, b, ino, data, int64(off))
	}
	total = time.Since(start)
	t.Logf("pass 2 (overwrites, no rebuild): %d ops in %s (%.1f us/op avg)", nOps, total, float64(total.Microseconds())/float64(nOps))

	_ = b.journal.Checkpoint()
	_ = b.dev.Sync()
}
