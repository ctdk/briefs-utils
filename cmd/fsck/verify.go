package main

import (
	"fmt"
	"os"
)

// runVerificationPass runs the read-only checks and populates fsckState.
// It is used both for the initial pass and for the post-repair verification.
// It returns the number of inodes found, but callers typically only need the
// side effects on fs.
func runVerificationPass(fs *fsckState, blockSize, inodeSize uint64) int {
	file := fs.file
	sb := fs.sb

	// Drop any cached allocator pools: the post-repair pass must re-read the
	// pools repair rewrote (see fsckState.allocPools).
	fs.allocPools = nil

	// 1. Superblock fields have already been read and validated.

	// 2. Allocator pools
	fmt.Fprintf(os.Stderr, "\nInode bitmap:\n")
	if err := verifyAllocatorPool(fs, sb.InodeBMOffset, "inode bitmap"); err != nil {
		fs.errorf("%v", err)
	}

	fmt.Fprintf(os.Stderr, "\nData block allocator:\n")
	if err := verifyAllocatorPool(fs, sb.TrieNodePoolStart, "data allocator"); err != nil {
		fs.errorf("%v", err)
	}

	verifyAllocatorBitmap(fs, sb.InodeBMOffset, sb.FreeInodes, "inode")

	verifyAllocatorBitmap(fs, sb.TrieNodePoolStart, sb.FreeDataBlks, "data")

	// 3. Inode table
	inodeTableStart := sb.InodeTableOffset
	var inodeTableBlocks uint64
	if p := fs.allocatorPool(sb.InodeBMOffset); p.err != nil {
		fs.errorf("read inode allocator header: %v", p.err)
	} else {
		inodeTableBlocks = (p.hdr.BlockCount*sb.InodeSize + blockSize - 1) / blockSize
	}
	fmt.Fprintf(os.Stderr, "\nInode table:\n")
	fmt.Fprintf(os.Stderr, "  start block: %d\n", inodeTableStart)
	fmt.Fprintf(os.Stderr, "  blocks:      %d\n", inodeTableBlocks)

	totalInodes := verifyInodeTable(fs, inodeTableStart, inodeTableBlocks, blockSize, inodeSize)
	fmt.Fprintf(os.Stderr, "  inodes found: %d\n", totalInodes)

	// 4. Journal
	fmt.Fprintf(os.Stderr, "\nJournal:\n")
	fmt.Fprintf(os.Stderr, "  start block: %d\n", sb.JournalOffset)
	fmt.Fprintf(os.Stderr, "  blocks:      %d\n", sb.JournalBlocks)
	fmt.Fprintf(os.Stderr, "  checkpoint:  %d\n", sb.CheckpointSeq)
	if err := verifyJournal(file, sb.JournalOffset, sb.JournalBlocks, sb.CheckpointSeq, sb.JournalLogStart, sb.JournalLogEnd, blockSize); err != nil {
		fs.warnf("journal check: %v", err)
	} else {
		fmt.Fprintf(os.Stderr, "  journal magic OK\n")
	}
	verifyJournalRecords(fs, sb.JournalOffset, sb.JournalBlocks, sb.JournalLogStart, sb.JournalLogEnd, blockSize)

	// 5. Directory trie walk
	fmt.Fprintf(os.Stderr, "\nDirectory tries:\n")
	allEntries := verifyAllDirTries(fs, blockSize, fs.dirs)
	fmt.Fprintf(os.Stderr, "  directories scanned: %d\n", len(fs.dirs))
	fmt.Fprintf(os.Stderr, "  total entries found: %d\n", len(allEntries))
	if len(fs.failedTrieDirs) > 0 {
		fmt.Fprintf(os.Stderr, "  WARNING: %d director(ies) had unrecoverable trie errors:\n", len(fs.failedTrieDirs))
		for ino := range fs.failedTrieDirs {
			fmt.Fprintf(os.Stderr, "    ino %d\n", ino)
		}
	}
	if len(fs.failedBtreeInos) > 0 {
		fmt.Fprintf(os.Stderr, "  WARNING: %d inode(s) had unrecoverable B-tree extent-index errors:\n", len(fs.failedBtreeInos))
		for ino := range fs.failedBtreeInos {
			fmt.Fprintf(os.Stderr, "    ino %d\n", ino)
		}
	}

	// 6. Cross-referencing checks
	// (The inode bitmap cross-reference runs inside verifyInodeTable: the
	// scan already visits every bitmap-allocated slot, so a second full
	// inode-table read would buy nothing.)
	fmt.Fprintf(os.Stderr, "\nCross-referencing:\n")
	verifyBlockCrossReference(fs)
	verifySuperblockFreeCounts(fs, totalInodes)
	verifyDirEntryCrossReference(fs, allEntries)
	verifyDuplicateNames(fs, allEntries)
	verifyLinkCounts(fs, allEntries)
	verifyOrphanedInodes(fs)
	verifyExtentOverlaps(fs)
	verifyReachability(fs, allEntries)

	return totalInodes
}
