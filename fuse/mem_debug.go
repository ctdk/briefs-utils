package fuse

// SIGUSR1/SIGUSR2 memory diagnostics.  The daemon holds several
// deferred-state structures (the dirtyBlocks write-back map, the
// pendingFrees queue, the walked-tree cache) that only shrink at a journal
// sync, so a workload that never syncs can turn a leak into an OOM kill with
// nothing in the log (generic/299: the daemon died to oom-killer repeatedly
// at an identical ~3.2 GB anon-rss with zero daemon-side errors).
// `kill -USR1 <pid>` dumps the sizes of every candidate to stderr — the
// shared /tmp/fuse.briefs-<mnt>.log under systemd-run — so a live repro can
// show which structure grows.  `kill -USR2 <pid>` writes a full pprof heap
// profile to /tmp/briefs-heap-<pid>-<n>.pprof for when the counters line
// exonerates every tracked structure and the allocation sites must be
// named directly.

import (
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"runtime/pprof"
	"strconv"
	"syscall"
	"time"
)

// startMemDebug installs the SIGUSR1 dumper and the SIGUSR2 heap profiler.
// Cheap when unused: one goroutine parked in select.  Wired by Mount for the
// life of the mount.
//
// BRIEFS_MEM_TICK=<seconds> additionally self-dumps every tick from inside
// the daemon — external pollers have repeatedly failed to survive the very
// memory pressure they are trying to observe (two runs, zero ticks), and an
// OOM kill leaves nothing behind.  Each tick past a new heapAlloc threshold
// (1G .. 3G in 512M steps) also writes a heap profile, so the growth sites
// are captured before the kill instead of after it.
func (b *BrieFS) startMemDebug(stop <-chan struct{}) {
	ch := make(chan os.Signal, 4)
	signal.Notify(ch, syscall.SIGUSR1, syscall.SIGUSR2)
	go func() {
		defer signal.Stop(ch)
		n := 0
		for {
			select {
			case <-stop:
				return
			case sig := <-ch:
				if sig == syscall.SIGUSR2 {
					n++
					path := fmt.Sprintf("/tmp/briefs-heap-%d-%d.pprof", os.Getpid(), n)
					if f, err := os.Create(path); err == nil {
						pprof.WriteHeapProfile(f)
						f.Close()
					}
					continue
				}
				b.dumpMemStats()
			}
		}
	}()

	if secs := os.Getenv("BRIEFS_MEM_TICK"); secs != "" {
		interval, err := strconv.Atoi(secs)
		if err != nil || interval <= 0 {
			interval = 5
		}
		go b.memTickLoop(stop, time.Duration(interval)*time.Second)
	}
}

// memTickLoop self-dumps memstats on a fixed interval and writes a heap
// profile each time heapAlloc crosses a fresh 512M threshold above 1G —
// the 299 daemon dies at ~3.2G anon-rss, so the last profiles land just
// before the kill.
func (b *BrieFS) memTickLoop(stop <-chan struct{}, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	const step = 512 << 20
	nextThreshold := int64(2 * step) // first profile at 1G heapAlloc
	profiles := 0
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			b.dumpMemStats()
			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)
			for int64(ms.HeapAlloc) >= nextThreshold {
				profiles++
				path := fmt.Sprintf("/tmp/briefs-heap-%d-tick%d.pprof", os.Getpid(), profiles)
				if f, err := os.Create(path); err == nil {
					pprof.WriteHeapProfile(f)
					f.Close()
				}
				nextThreshold += step
			}
		}
	}
}

// dumpMemStats prints one line of live counter values.
func (b *BrieFS) dumpMemStats() {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	b.dirtyMu.Lock()
	dirtyBlocks := len(b.dirtyBlocks)
	dirtyBytes := 0
	for _, buf := range b.dirtyBlocks {
		dirtyBytes += len(buf)
	}
	frees := b.pendingFrees.count()
	freeRuns := len(b.pendingFrees.runs)
	b.dirtyMu.Unlock()

	b.extentTreesMu.Lock()
	trees := len(b.extentTrees)
	extEntries := 0
	leafEntries := 0
	for _, et := range b.extentTrees {
		extEntries += len(et.exts)
		leafEntries += len(et.leaves)
	}
	b.extentTreesMu.Unlock()

	jDirty := false
	if b.journal != nil {
		jDirty = b.journal.Dirty()
	}

	fmt.Fprintf(os.Stderr,
		"memstats: heapAlloc=%.0fMB heapInuse=%.0fMB heapSys=%.0fMB "+
			"dirtyBlocks=%d (%.0fMB) pendingFrees=%d in %d runs (%.0fMB) "+
			"extentTrees=%d extEntries=%d leafEntries=%d journalDirty=%v\n",
		float64(ms.HeapAlloc)/1e6, float64(ms.HeapInuse)/1e6,
		float64(ms.HeapSys)/1e6,
		dirtyBlocks, float64(dirtyBytes)/1e6,
		frees, freeRuns, float64(freeRuns*16)/1e6,
		trees, extEntries, leafEntries,
		jDirty)
}
