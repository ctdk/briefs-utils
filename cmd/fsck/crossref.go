package main

import (
	"fmt"
	"os"

	"github.com/ctdk/briefs-utils/briefs"
)

// verifyBlockCrossReference checks that every block referenced by inode extents
// and trie nodes is marked allocated in the data allocator bitmap, and that
// every allocated block in the bitmap is referenced by something.
//
// The data allocator tracks data-relative block numbers (0 = first block of
// the data region). The absolute block number of the first data block is
// TrieNodePoolStart + TrieNodePoolSize.
func verifyBlockCrossReference(fs *fsckState, blockSize uint64) {
	dataRegionStart := fs.sb.TrieNodePoolStart + fs.sb.TrieNodePoolSize

	l2, dataL2w, dataBlockCount, err := readAllocatorL2(fs.file, fs.sb.TrieNodePoolStart, blockSize)
	if err != nil {
		fs.errorf("block cross-ref: %v", err)
		return
	}

	// Build a set of data-relative blocks that the allocator says are allocated
	allocAllocated := make(map[uint64]bool)
	for i := uint64(0); i < dataBlockCount; i++ {
		w := i / 64
		b := i % 64
		if w < dataL2w && (l2[w]&(1<<b)) == 0 {
			allocAllocated[i] = true
		}
	}

	// Check 1: Blocks used by inodes/tries that are NOT marked allocated
	orphans := 0
	for absBlk := range fs.usedBlocks {
		if absBlk < dataRegionStart || absBlk >= dataRegionStart+dataBlockCount {
			// Block is outside the data region — metadata, which is fine
			continue
		}
		relBlk := absBlk - dataRegionStart
		if !allocAllocated[relBlk] {
			if orphans < 20 {
				fs.errorf("block %d (data-relative %d): used by inode/trie but NOT marked allocated in bitmap",
					absBlk, relBlk)
			} else if orphans == 20 {
				fs.errorf("(more orphan block errors suppressed)")
			}
			orphans++
		}
	}
	if orphans > 0 {
		fmt.Fprintf(os.Stderr, "  block cross-ref: %d block(s) used but not marked allocated\n", orphans)
	}

	// Check 2: Blocks marked allocated in bitmap but NOT referenced by any inode/trie.
	// If any directory trie or B+ tree had structural errors, leaked blocks could be
	// legitimate blocks from unreadable subtrees (or unreached B-tree node/data
	// blocks of a torn tree), so we downgrade to WARNING in that case.
	leaked := 0
	hasFailedTries := len(fs.failedTrieDirs) > 0
	hasFailedBtrees := len(fs.failedBtreeInos) > 0
	for relBlk := range allocAllocated {
		absBlk := dataRegionStart + relBlk
		if !fs.usedBlocks[absBlk] {
			if leaked < 20 {
				if hasFailedTries || hasFailedBtrees {
					fs.warnf("block %d (data-relative %d): marked allocated but not found during trie/btree walk (may be from a failed traversal)",
						absBlk, relBlk)
				} else {
					fs.errorf("block %d (data-relative %d): marked allocated in bitmap but NOT referenced by any inode/trie",
						absBlk, relBlk)
				}
			} else if leaked == 20 {
				if hasFailedTries || hasFailedBtrees {
					fs.warnf("(more unverifiable block warnings suppressed)")
				} else {
					fs.errorf("(more leaked block errors suppressed)")
				}
			}
			leaked++
		}
	}
	if leaked > 0 {
		if hasFailedTries || hasFailedBtrees {
			fmt.Fprintf(os.Stderr, "  block cross-ref: %d block(s) allocated but not verified (trie/btree errors may explain these)\n", leaked)
		} else {
			fmt.Fprintf(os.Stderr, "  block cross-ref: %d block(s) allocated but not referenced\n", leaked)
		}
	}

	if orphans == 0 && leaked == 0 {
		fmt.Fprintf(os.Stderr, "  block cross-ref: all used blocks match allocator bitmap\n")
	}
}

// verifyLinkCounts checks that each inode's nlink matches the number of
// directory entries referencing it.
func verifyLinkCounts(fs *fsckState) {
	mismatches := 0
	for ino, in := range fs.inodes {
		expected := fs.entryCounts[ino]
		// For directories, nlink includes . and .. which are not stored in the trie.
		// nlink for a dir = 2 (., ..) + number of subdirectories
		// We can't easily count subdirectories from here, so we only check files.
		if in.IsFile() {
			if int(in.Nlinks) != expected {
				if mismatches < 20 {
					fs.errorf("ino %d: nlink=%d but %d directory entries reference it", ino, in.Nlinks, expected)
				} else if mismatches == 20 {
					fs.errorf("(more link count errors suppressed)")
				}
				mismatches++
			}
		}
	}
	if mismatches == 0 {
		fmt.Fprintf(os.Stderr, "  link counts: all file link counts match directory entries\n")
	}
}

// verifyDirEntryCrossReference checks that every directory entry's inode
// exists in the inode table and that the file type matches.
func verifyDirEntryCrossReference(fs *fsckState, entries []trieEntry) {
	badInos := 0
	badTypes := 0

	for _, e := range entries {
		// Check inode exists
		in, ok := fs.inodes[e.Inode]
		if !ok {
			if badInos < 20 {
				fs.errorf("dir entry '%s' in ino %d: references ino %d which does not exist",
					e.Name, e.Parent, e.Inode)
			} else if badInos == 20 {
				fs.errorf("(more bad inode reference errors suppressed)")
			}
			badInos++
			continue
		}

		// Check file type matches.
		// ftype is stored as (S_IFMT >> 12): 4 for directories, 8 for regular
		// files, 10 for symbolic links.
		var expectedFType uint8
		switch in.Filemode & briefs.ModeTypeMask {
		case briefs.ModeDir:
			expectedFType = 4 // S_IFDIR >> 12
		case briefs.ModeFile:
			expectedFType = 8 // S_IFREG >> 12
		case briefs.ModeSymlink:
			expectedFType = 10 // S_IFLNK >> 12
		}
		if expectedFType != 0 && e.FType != expectedFType {
			if badTypes < 20 {
				fs.errorf("dir entry '%s' in ino %d: ftype=%d but inode %d has mode 0x%04X (expected %d)",
					e.Name, e.Parent, e.FType, e.Inode, in.Filemode, expectedFType)
			} else if badTypes == 20 {
				fs.errorf("(more type mismatch errors suppressed)")
			}
			badTypes++
		}
	}

	if badInos > 0 || badTypes > 0 {
		return
	}
	fmt.Fprintf(os.Stderr, "  dir entry cross-ref: all entries reference valid inodes\n")
}

// verifyOrphanedInodes checks for inodes that have nlink > 0 but no
// directory entries referencing them (orphaned).
func verifyOrphanedInodes(fs *fsckState) {
	orphans := 0
	for ino, in := range fs.inodes {
		if ino == fs.sb.RootIno {
			continue // root is special
		}
		if in.Nlinks > 0 && fs.entryCounts[ino] == 0 {
			if orphans < 20 {
				fs.errorf("ino %d: nlink=%d but no directory entries reference it (orphaned)", ino, in.Nlinks)
			} else if orphans == 20 {
				fs.errorf("(more orphaned inode errors suppressed)")
			}
			orphans++
		}
	}
	if orphans == 0 {
		fmt.Fprintf(os.Stderr, "  orphan check: no orphaned inodes found\n")
	}
}

// verifyExtentOverlaps checks that extents don't overlap with each other
// or with metadata regions.
func verifyExtentOverlaps(fs *fsckState) {
	type extentRef struct {
		ino  uint64
		phys uint64
		len  uint64
	}
	var allExtents []extentRef

	addExtent := func(ino uint64, ext briefs.Extent) {
		// Skip hole extents — no physical backing
		if ext.Flags&briefs.ExtentFlagHole != 0 {
			return
		}
		if ext.Flags&^(uint32(briefs.ExtentFlagHole|briefs.ExtentFlagEof)) != 0 {
			fs.warnf("ino %d: extent with unknown flags 0x%08X (phys=%d, len=%d)",
				ino, ext.Flags, ext.Phys, ext.Len)
		}
		if ext.Len > 0 && ext.Phys > 0 {
			allExtents = append(allExtents, extentRef{ino: ino, phys: ext.Phys, len: ext.Len})
		}
	}

	for ino, in := range fs.inodes {
		if in.Flags&briefs.InodeFlagInlineData != 0 {
			continue
		}
		// Skip inodes whose B+ tree walk already failed in collectInodeExtents;
		// the structural error was already reported there, and re-walking would
		// just emit a duplicate error (the tree is still torn).
		if fs.failedBtreeInos[ino] {
			continue
		}
		// Walk every extent (inline array or B+ tree) in ascending offset order.
		visit := func(ext briefs.Extent) error {
			addExtent(ino, ext)
			return nil
		}
		if err := briefs.IterateInodeExtents(fs.file, in, fs.sb.BlockSize, briefs.InodeExtentVisitor{
			VisitExtent: visit,
		}); err != nil {
			fs.errorf("ino %d: %v", ino, err)
		}
	}

	overlaps := 0
	for i := 0; i < len(allExtents); i++ {
		ei := allExtents[i]
		eiEnd := ei.phys + ei.len

		// Check against metadata regions
		// Superblock: block 0
		if ei.phys == 0 || (ei.phys < 1 && eiEnd > 0) {
			if overlaps < 20 {
				fs.errorf("ino %d: extent at phys=%d len=%d overlaps with superblock (block 0)",
					ei.ino, ei.phys, ei.len)
			}
			overlaps++
		}

		// Check against journal
		journalStart := fs.sb.JournalOffset
		journalEnd := journalStart + fs.sb.JournalBlocks
		if ei.phys < journalEnd && eiEnd > journalStart {
			if overlaps < 20 {
				fs.errorf("ino %d: extent at phys=%d len=%d overlaps with journal (blocks %d-%d)",
					ei.ino, ei.phys, ei.len, journalStart, journalEnd-1)
			}
			overlaps++
		}

		// Check against inode table
		itStart := fs.sb.InodeTableOffset
		itEnd := itStart + (uint64(len(fs.inodes))*fs.sb.InodeSize+fs.sb.BlockSize-1)/fs.sb.BlockSize
		if ei.phys < itEnd && eiEnd > itStart {
			if overlaps < 20 {
				fs.errorf("ino %d: extent at phys=%d len=%d overlaps with inode table (blocks %d-%d)",
					ei.ino, ei.phys, ei.len, itStart, itEnd-1)
			}
			overlaps++
		}

		// Check against other extents
		for j := i + 1; j < len(allExtents); j++ {
			ej := allExtents[j]
			ejEnd := ej.phys + ej.len
			if ei.phys < ejEnd && eiEnd > ej.phys {
				if overlaps < 20 {
					fs.errorf("ino %d extent and ino %d extent overlap: [%d,%d) vs [%d,%d)",
						ei.ino, ej.ino, ei.phys, eiEnd, ej.phys, ejEnd)
				} else if overlaps == 20 {
					fs.errorf("(more extent overlap errors suppressed)")
				}
				overlaps++
			}
		}
	}

	if overlaps == 0 {
		fmt.Fprintf(os.Stderr, "  extent overlap: no overlapping extents found\n")
	}
}

// verifyReachability walks from the root inode through the directory tree
// and reports any inodes that are not reachable.
func verifyReachability(fs *fsckState, entries []trieEntry) {
	// Build a map of parent -> children from directory entries
	children := make(map[uint64]map[uint64]bool) // parent ino -> set of child inos
	for _, e := range entries {
		if children[e.Parent] == nil {
			children[e.Parent] = make(map[uint64]bool)
		}
		children[e.Parent][e.Inode] = true
	}

	// BFS from root
	reachable := make(map[uint64]bool)
	queue := []uint64{fs.sb.RootIno}
	reachable[fs.sb.RootIno] = true

	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]

		for child := range children[parent] {
			if !reachable[child] {
				reachable[child] = true
				queue = append(queue, child)
			}
		}
	}

	// Report unreachable inodes
	unreachable := 0
	for ino := range fs.inodes {
		if ino == fs.sb.RootIno {
			continue
		}
		if !reachable[ino] {
			if unreachable < 20 {
				fs.errorf("ino %d: not reachable from root directory", ino)
			} else if unreachable == 20 {
				fs.errorf("(more unreachable inode errors suppressed)")
			}
			unreachable++
		}
	}

	if unreachable == 0 {
		fmt.Fprintf(os.Stderr, "  reachability: all inodes reachable from root\n")
	}
}

// verifyDuplicateNames checks for duplicate directory entries within the same
// directory (same name, different inode).
func verifyDuplicateNames(fs *fsckState, entries []trieEntry) {
	// Group entries by parent directory
	byParent := make(map[uint64][]trieEntry)
	for _, e := range entries {
		byParent[e.Parent] = append(byParent[e.Parent], e)
	}

	dups := 0
	for parent, ents := range byParent {
		seen := make(map[string]uint64) // name -> first ino seen
		for _, e := range ents {
			if firstIno, ok := seen[e.Name]; ok {
				if firstIno != e.Inode {
					if dups < 20 {
						fs.errorf("ino %d: duplicate name '%s' (inodes %d and %d)",
							parent, e.Name, firstIno, e.Inode)
					} else if dups == 20 {
						fs.errorf("(more duplicate name errors suppressed)")
					}
					dups++
				}
			} else {
				seen[e.Name] = e.Inode
			}
		}
	}

	if dups == 0 {
		fmt.Fprintf(os.Stderr, "  duplicate names: no duplicate directory entries found\n")
	}
}
