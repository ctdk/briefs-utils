package main

import (
	"fmt"
	"math/bits"
	"os"
	"sort"

	"github.com/ctdk/briefs-utils/briefs"
)

// verifyBlockCrossReference checks that every block referenced by inode extents
// and trie nodes is marked allocated in the data allocator bitmap, and that
// every allocated block in the bitmap is referenced by something.
//
// The data allocator tracks data-relative block numbers (0 = first block of
// the data region). The absolute block number of the first data block is
// TrieNodePoolStart + TrieNodePoolSize.
func verifyBlockCrossReference(fs *fsckState) {
	dataRegionStart := fs.dataRegionStart()

	pool := fs.allocatorPool(fs.sb.TrieNodePoolStart)
	if pool.err != nil {
		fs.errorf("block cross-ref: %v", pool.err)
		return
	}
	l2 := pool.l2
	dataBlockCount := pool.hdr.BlockCount

	// Check 1: Blocks used by inodes/tries that are NOT marked allocated.
	// The allocator bitmap itself is the membership structure (allocated =
	// bit clear in the L2 leaf), so test it directly instead of building a
	// per-block map first.
	orphans := 0
	fs.usedBlocks.each(func(absBlk uint64) {
		if absBlk < dataRegionStart || absBlk >= dataRegionStart+dataBlockCount {
			// Block is outside the data region — metadata, which is fine
			return
		}
		relBlk := absBlk - dataRegionStart
		if !briefs.AllocIsAllocated(l2, dataBlockCount, relBlk) {
			fs.reportLimited(&orphans, 20, fs.errorf, "(more orphan block errors suppressed)",
				"block %d (data-relative %d): used by inode/trie but NOT marked allocated in bitmap",
				absBlk, relBlk)
		}
	})
	if orphans > 0 {
		fmt.Fprintf(os.Stderr, "  block cross-ref: %d block(s) used but not marked allocated\n", orphans)
	}

	// Check 2: Blocks marked allocated in bitmap but NOT referenced by any inode/trie.
	// If any directory trie or B+ tree had structural errors, leaked blocks could be
	// legitimate blocks from unreadable subtrees (or unreached B-tree node/data
	// blocks of a torn tree), so we downgrade to WARNING in that case.
	//
	// Data-relative block 0 is reserved as the ENOSPC sentinel (matching kernel
	// behavior), so it's expected to be allocated but unused. Skip it.
	// The allocated (clear) bits are iterated word-at-a-time: on a large
	// volume this is the pass fsck spends most of its cross-reference time
	// in, and a per-block loop over billions of bits does not scale.
	leaked := 0
	allocatedCount := 0
	hasFailedTries := len(fs.failedTrieDirs) > 0
	hasFailedBtrees := len(fs.failedBtreeInos) > 0
	for w, word := range l2 {
		base := uint64(w) * 64
		if base >= dataBlockCount {
			break
		}
		inv := ^word // set bit = allocated block
		if rem := dataBlockCount - base; rem < 64 {
			inv &= (1 << rem) - 1
		}
		for inv != 0 {
			b := bits.TrailingZeros64(inv)
			inv &^= 1 << b
			relBlk := base + uint64(b)
			allocatedCount++
			if relBlk == 0 {
				// Block 0 is the ENOSPC sentinel; expected to be unused.
				continue
			}
			absBlk := dataRegionStart + relBlk
			if !fs.usedBlocks.has(absBlk) {
				if hasFailedTries || hasFailedBtrees {
					fs.reportLimited(&leaked, 20, fs.warnf,
						"(more unverifiable block warnings suppressed)",
						"block %d (data-relative %d): marked allocated but not found during trie/btree walk (may be from a failed traversal)",
						absBlk, relBlk)
				} else {
					fs.reportLimited(&leaked, 20, fs.errorf,
						"(more leaked block errors suppressed)",
						"block %d (data-relative %d): marked allocated in bitmap but NOT referenced by any inode/trie",
						absBlk, relBlk)
				}
			}
		}
	}
	if leaked > 0 {
		if hasFailedTries || hasFailedBtrees {
			fmt.Fprintf(os.Stderr, "  block cross-ref: %d block(s) allocated but not verified (trie/btree errors may explain these)\n", leaked)
		} else {
			fmt.Fprintf(os.Stderr, "  block cross-ref: %d block(s) allocated but not referenced\n", leaked)
		}
	}

	fs.verbosef("block cross-ref: %d block(s) marked allocated, %d block(s) referenced by inodes/tries",
		allocatedCount, fs.usedBlocks.count())

	if orphans == 0 && leaked == 0 {
		fmt.Fprintf(os.Stderr, "  block cross-ref: all used blocks match allocator bitmap\n")
	}
}

// computeDirSubdirCounts returns a map of ino -> number of subdirectory
// entries the directory contains, derived from the entries the verify pass
// already collected (fs.dirEntries) instead of walking every trie a second
// time. A directory's nlink is 2 (., ..) plus its subdirectory count, so
// this is the computation behind repairLinkCounts (fix). The repair path is
// gated on fs.failedTrieDirs being empty, so the cached lists are complete;
// a missing cache means repair ran without a verification pass, which the
// caller must treat as an error rather than repairing from empty counts.
func computeDirSubdirCounts(fs *fsckState) (map[uint64]int, error) {
	if fs.dirEntries == nil {
		return nil, fmt.Errorf("directory entries not collected (repair before verify)")
	}
	subdirCount := make(map[uint64]int)
	for _, d := range fs.dirs {
		for _, e := range fs.dirEntries[d.ino] {
			if target, ok := fs.inodes[e.Inode]; ok && target.IsDir() {
				subdirCount[d.ino]++
			}
		}
	}
	return subdirCount, nil
}

// verifyLinkCounts checks that each inode's nlink matches the count derived
// from the directory tries: directories are 2 (., ..) + subdirectory count,
// files and symlinks are the number of directory entries referencing them.
// This matches what repairLinkCounts would fix and what the kernel maintains,
// so a wrong nlink is flagged instead of passing silently.
//
// Subdirectory counts are derived from the entries the trie walk
// (verifyAllDirTries) already collected — every entry carries its parent —
// instead of walking each directory trie a second time. A directory whose
// trie walk had structural errors is in fs.failedTrieDirs and its entries
// may be incomplete, so the directory nlink check is skipped in that case
// (the trie error itself is already reported); files and symlinks can still
// be checked against entryCounts.
func verifyLinkCounts(fs *fsckState, entries []trieEntry) {
	var subdirCount map[uint64]int
	if len(fs.failedTrieDirs) > 0 {
		fs.errorf("link counts: skipping directory nlink check: %d director(ies) with failed trie walks",
			len(fs.failedTrieDirs))
	} else {
		subdirCount = make(map[uint64]int)
		for _, e := range entries {
			if target, ok := fs.inodes[e.Inode]; ok && target.IsDir() {
				subdirCount[e.Parent]++
			}
		}
	}

	mismatches := 0
	for ino, in := range fs.inodes {
		var expected int
		switch {
		case in.IsDir():
			if subdirCount == nil {
				continue
			}
			// nlink = 2 (., ..) + number of subdirectories.
			expected = 2 + subdirCount[ino]
		case in.IsFile() || in.IsSymlink():
			expected = fs.entryCounts[ino]
		default:
			continue
		}
		if int(in.Nlinks) != expected {
			fs.reportLimited(&mismatches, 20, fs.errorf,
				"(more link count errors suppressed)",
				"ino %d: nlink=%d but expected %d", ino, in.Nlinks, expected)
		}
	}
	if mismatches == 0 {
		if subdirCount != nil {
			fmt.Fprintf(os.Stderr, "  link counts: all link counts match (dirs, files, symlinks)\n")
		} else {
			fmt.Fprintf(os.Stderr, "  link counts: all file/symlink link counts match (dir check skipped)\n")
		}
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
			fs.reportLimited(&badInos, 20, fs.errorf,
				"(more bad inode reference errors suppressed)",
				"dir entry '%s' in ino %d: references ino %d which does not exist",
				e.Name, e.Parent, e.Inode)
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
			fs.reportLimited(&badTypes, 20, fs.errorf,
				"(more type mismatch errors suppressed)",
				"dir entry '%s' in ino %d: ftype=%d but inode %d has mode 0x%04X (expected %d)",
				e.Name, e.Parent, e.FType, e.Inode, in.Filemode, expectedFType)
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
			fs.reportLimited(&orphans, 20, fs.errorf,
				"(more orphaned inode errors suppressed)",
				"ino %d: nlink=%d but no directory entries reference it (orphaned)", ino, in.Nlinks)
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
		if phys, length, ok := checkExtent(fs, ino, ext); ok {
			allExtents = append(allExtents, extentRef{ino: ino, phys: phys, len: length})
		}
	}

	// The per-inode extent lists were already collected in ascending offset
	// order by the inode table scan's single walk (collectInodeExtents);
	// consume them instead of re-walking every tree. Skip inodes whose walk
	// failed: they are in fs.failedBtreeInos, the structural error was already
	// reported there, and their lists may be partial.
	for ino, extents := range fs.inodeExtents {
		if fs.failedBtreeInos[ino] {
			continue
		}
		for _, ext := range extents {
			addExtent(ino, ext)
		}
	}

	// Metadata regions an extent must not touch. Bounds come from the
	// superblock (and, for the inode table, the inode allocator header) —
	// NOT from the number of inodes found during the scan: an extent
	// squatting in the tail of the inode table beyond the last in-use slot
	// would otherwise go unflagged.
	type region struct {
		name  string
		start uint64
		end   uint64
	}
	regions := []region{
		{"superblock", 0, 1},
		{"inode bitmap", fs.sb.InodeBMOffset, fs.sb.InodeBMOffset + fs.sb.InodeBMBlocks},
		{"extent allocation table", fs.sb.EATOffset, fs.sb.EATOffset + fs.sb.EATBlocks},
		{"trie node pool / allocator pool", fs.sb.TrieNodePoolStart, fs.sb.TrieNodePoolStart + fs.sb.TrieNodePoolSize},
		{"journal", fs.sb.JournalOffset, fs.sb.JournalOffset + fs.sb.JournalBlocks},
	}
	// Inode table: capacity from the inode allocator header (BlockCount is
	// the number of inode slots the table was built for), the same bound
	// verifyInodeTable scans with.
	itStart := fs.sb.InodeTableOffset
	itEnd := itStart
	if p := fs.allocatorPool(fs.sb.InodeBMOffset); p.err == nil {
		itEnd = itStart + (p.hdr.BlockCount*fs.sb.InodeSize+fs.sb.BlockSize-1)/fs.sb.BlockSize
	}
	regions = append(regions, region{"inode table", itStart, itEnd})

	overlaps := 0
	for i := range allExtents {
		ei := allExtents[i]
		eiEnd := ei.phys + ei.len

		for _, r := range regions {
			if r.end <= r.start {
				continue // degenerate/unknown bound: nothing to check
			}
			if ei.phys < r.end && eiEnd > r.start {
				fs.reportLimited(&overlaps, 20, fs.errorf,
					"(more extent overlap errors suppressed)",
					"ino %d: extent at phys=%d len=%d overlaps with %s (blocks %d-%d)",
					ei.ino, ei.phys, ei.len, r.name, r.start, r.end-1)
			}
		}
	}

	// Extent-vs-extent overlap: sort by physical start and scan with the
	// running maximum end. A pairwise O(n²) scan does not scale to large
	// volumes; after sorting, an interval overlaps an earlier one iff its
	// start is below the maximum end seen so far, and the interval holding
	// that maximum is a witness — every overlapping pair flags its later
	// member at least once (no false negatives), though a pair is
	// attributed to the furthest-reaching earlier interval rather than to
	// each earlier interval it overlaps.
	sort.Slice(allExtents, func(i, j int) bool { return allExtents[i].phys < allExtents[j].phys })
	maxEnd, maxIdx := uint64(0), -1
	for i := range allExtents {
		ei := allExtents[i]
		if maxIdx >= 0 && ei.phys < maxEnd {
			w := allExtents[maxIdx]
			fs.reportLimited(&overlaps, 20, fs.errorf,
				"(more extent overlap errors suppressed)",
				"ino %d extent and ino %d extent overlap: [%d,%d) vs [%d,%d)",
				ei.ino, w.ino, ei.phys, ei.phys+ei.len, w.phys, w.phys+w.len)
		}
		if end := ei.phys + ei.len; end > maxEnd {
			maxEnd, maxIdx = end, i
		}
	}

	fs.verbosef("extent overlap: checked %d extent(s) across %d inode(s)", len(allExtents), len(fs.inodes))

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
			fs.reportLimited(&unreachable, 20, fs.errorf,
				"(more unreachable inode errors suppressed)",
				"ino %d: not reachable from root directory", ino)
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
					fs.reportLimited(&dups, 20, fs.errorf,
						"(more duplicate name errors suppressed)",
						"ino %d: duplicate name '%s' (inodes %d and %d)",
						parent, e.Name, firstIno, e.Inode)
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
