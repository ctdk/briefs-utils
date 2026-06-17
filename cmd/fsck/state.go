package main

import (
	"fmt"
	"os"

	"github.com/ctdk/briefs-utils/types"
)

// fsckError tracks the total error count across all checks.
type fsckState struct {
	errors int
	file   *os.File
	sb     *types.SuperblockLayout
	// Collected during inode table scan for cross-referencing
	inodes      map[uint64]*types.Inode // ino -> inode
	dirs        []dirInfo               // directories with trie roots
	usedBlocks  map[uint64]bool         // all blocks referenced by extents or trie nodes
	entryCounts map[uint64]int          // ino -> number of directory entries referencing it
	// Tracks directories where trie walk had structural errors (bad magic, etc.)
	failedTrieDirs map[uint64]bool // ino -> true if trie walk had unrecoverable errors
	// Set when the caller requested repair/optimization.
	repair bool
}

// repairPlan holds the intended state after repair/optimization. All changes
// are staged here before being written back to disk.
type repairPlan struct {
	// Allocator state rebuilt from the post-repair metadata.
	dataAlloc  *types.AllocBuilder
	inodeAlloc *types.AllocBuilder

	// Inodes that have been modified and need to be written back.
	inodes map[uint64]*types.Inode

	// Allocator and superblock free counts derived from the plan.
	freeDataBlks  uint64
	freeInodes    uint64
	checkpointSeq uint64
}

// repairOptions selects which repair/optimization phases run.
type repairOptions struct {
	RebuildAllocator bool // rebuild allocator bitmaps from the scan (phase 2)
	CompactExtents   bool // merge adjacent file extents (phase 4)
	CompactTries     bool // rebuild directory tries (phase 3)
	RepairLinks      bool // recompute inode nlink values (phase 5)
}

func (fs *fsckState) errorf(format string, args ...interface{}) {
	fs.errors++
	fmt.Fprintf(os.Stderr, "  ERROR: "+format+"\n", args...)
}

func (fs *fsckState) warnf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "  WARNING: "+format+"\n", args...)
}
