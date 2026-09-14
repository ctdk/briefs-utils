package main

import (
	"fmt"
	"os"

	"github.com/ctdk/briefs-utils/briefs"
)

// fsckError tracks the total error count across all checks.
type fsckState struct {
	errors int
	file   *os.File
	sb     *briefs.SuperblockLayout
	// --verbose: lift the per-check report caps and print extra scan
	// detail. The default output is unchanged.
	verbose bool
	// Collected during inode table scan for cross-referencing
	inodes      map[uint64]*briefs.Inode // ino -> inode
	dirs        []dirInfo                // directories with trie roots
	usedBlocks  *blockSet                // all blocks referenced by extents or trie nodes (interval-backed)
	entryCounts map[uint64]int           // ino -> number of directory entries referencing it
	// inodeExtents holds every inode's walked extents (ascending offsets),
	// collected by the inode table scan's single extent walk; the overlap
	// pass consumes them instead of re-walking every tree. Inodes whose walk
	// failed (failedBtreeInos) may hold a partial list and are skipped there.
	inodeExtents map[uint64][]briefs.Extent
	// Tracks directories where trie walk had structural errors (bad magic, etc.)
	failedTrieDirs map[uint64]bool // ino -> true if trie walk had unrecoverable errors
	// Tracks tree-backed inodes whose B+ tree extent index walk had structural
	// errors (bad magic, bad CRC, cycle, unsorted keys, fanout overflow, etc.).
	// Such an inode's extents and B-tree node blocks are NOT recorded in
	// usedBlocks (the walk failed before reaching them), so an allocator
	// rebuild from usedBlocks would mark them free — permanent data loss.
	// The repair gate refuses --repair when this set is non-empty and the
	// allocator rebuild phase is active (repairOpts.RebuildAllocator).
	failedBtreeInos map[uint64]bool // ino -> true if B-tree extent index walk had unrecoverable errors
}

// repairPlan holds the intended state after repair/optimization. All changes
// are staged here before being written back to disk.
type repairPlan struct {
	// Allocator state rebuilt from the post-repair metadata.
	dataAlloc  *briefs.AllocBuilder
	inodeAlloc *briefs.AllocBuilder

	// Inodes that have been modified and need to be written back.
	inodes map[uint64]*briefs.Inode

	// Allocator and superblock free counts derived from the plan.
	freeDataBlks  uint64
	freeInodes    uint64
	checkpointSeq uint64
}

// repairOptions selects which repair/optimization phases run.
type repairOptions struct {
	RebuildAllocator   bool // rebuild allocator bitmaps from the scan (phase 2)
	RepairBtreeCRC     bool // rewrite torn B-tree node checksums (phase 3)
	RebuildBtree       bool // rebuild corrupt B+ tree extent indexes from recovered extents (phase 4)
	ReclaimOrphanBtree bool // free allocated-but-unreferenced B-tree node blocks (phase 5, default-off even in "all")
	CompactExtents     bool // rebuild tree-backed inodes' B+ tree extent indexes minimally packed (phase 4)
	CompactTries       bool // rebuild directory tries (phase 3)
	RepairLinks        bool // recompute inode nlink values (phase 5)
}

// dataRegionStart is the absolute block number of the first data block
// (the trie node pool ends there). The data allocator tracks data-relative
// block numbers from this point.
func (fs *fsckState) dataRegionStart() uint64 {
	return fs.sb.TrieNodePoolStart + fs.sb.TrieNodePoolSize
}

// allocDataBlock returns a fresh-block allocator over the repair plan's
// data allocator: each call returns one data-relative allocation shifted
// to its absolute block number.
func allocDataBlock(plan *repairPlan, dataRegionStart uint64) func() (uint64, error) {
	return func() (uint64, error) {
		rel, err := plan.dataAlloc.AllocateBlock()
		if err != nil {
			return 0, err
		}
		return rel + dataRegionStart, nil
	}
}

func (fs *fsckState) errorf(format string, args ...interface{}) {
	fs.errors++
	fmt.Fprintf(os.Stderr, "  ERROR: "+format+"\n", args...)
}

func (fs *fsckState) warnf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "  WARNING: "+format+"\n", args...)
}

// reportLimited emits one capped diagnostic.  Normally the first @limit
// occurrences of a check report via @report (fs.errorf or fs.warnf — or a
// closure choosing between them), then a single @suppressNotice goes out
// the same channel and the rest stay silent.  --verbose lifts the cap:
// every occurrence reports.  @counter tracks the caller's running total
// and is incremented even when the report is suppressed, so summary
// counts at the end of the check stay complete.
func (fs *fsckState) reportLimited(counter *int, limit int, report func(string, ...interface{}), suppressNotice, format string, args ...interface{}) {
	if fs.verbose || *counter < limit {
		report(format, args...)
	} else if *counter == limit {
		report("%s", suppressNotice)
	}
	*counter++
}

// verbosef prints an informational line only under --verbose.
func (fs *fsckState) verbosef(format string, args ...interface{}) {
	if fs.verbose {
		fmt.Fprintf(os.Stderr, "  "+format+"\n", args...)
	}
}
