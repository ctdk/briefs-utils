package main

import (
	"encoding/binary"
	"fmt"
	"math/bits"
	"os"

	"github.com/ctdk/briefs-utils/briefs"
)

// runRepair rebuilds allocator state from the verified metadata and writes the
// repaired allocator, superblock free counts, and a fresh checkpoint back to disk.
// This covers Phase 2 of the fsck repair roadmap:
//   - rebuild data/inode allocator bitmaps from the structures fsck found;
//   - fix superblock free count mismatches;
//   - free blocks that are allocated but not referenced (no failed trie walks).
func runRepair(fs *fsckState, blockSize uint64, totalInodes int, opts *repairOptions) error {
	plan := &repairPlan{
		inodes: make(map[uint64]*briefs.Inode),
	}

	// 1. Set up data allocator.
	dataRegionStart := fs.sb.TrieNodePoolStart + fs.sb.TrieNodePoolSize
	if dataRegionStart > fs.sb.TotalBlocks {
		return fmt.Errorf("data region start %d exceeds total blocks %d", dataRegionStart, fs.sb.TotalBlocks)
	}
	dataBlockCount := fs.sb.TotalBlocks - dataRegionStart
	if fs.sb.JournalOffset > dataRegionStart {
		dataBlockCount = fs.sb.JournalOffset - dataRegionStart
	}

	if opts.RebuildAllocator {
		// Rebuild data allocator from the structures fsck found.
		plan.dataAlloc = briefs.NewAllocBuilder(dataBlockCount)
		for absBlk := range fs.usedBlocks {
			if absBlk >= dataRegionStart && absBlk < dataRegionStart+dataBlockCount {
				plan.dataAlloc.MarkAllocated(absBlk - dataRegionStart)
			}
		}
	} else {
		// Use the on-disk allocator so selective repairs only touch the requested
		// phases.
		var err error
		plan.dataAlloc, err = loadAllocatorFromDisk(fs.file, fs.sb.TrieNodePoolStart, blockSize, dataBlockCount)
		if err != nil {
			return fmt.Errorf("load data allocator: %w", err)
		}
	}

	// 2. Set up inode allocator.
	inodeHeader, err := briefs.ReadAllocatorHeader(fs.file, fs.sb.InodeBMOffset, blockSize)
	if err != nil {
		return fmt.Errorf("read inode allocator header: %w", err)
	}
	if opts.RebuildAllocator {
		plan.inodeAlloc = briefs.NewAllocBuilder(inodeHeader.BlockCount)
		for ino := range fs.inodes {
			if ino > 0 && ino <= inodeHeader.BlockCount {
				plan.inodeAlloc.MarkAllocated(ino - 1)
			}
		}
	} else {
		plan.inodeAlloc, err = loadAllocatorFromDisk(fs.file, fs.sb.InodeBMOffset, blockSize, inodeHeader.BlockCount)
		if err != nil {
			return fmt.Errorf("load inode allocator: %w", err)
		}
	}

	// 2b. Rewrite torn B-tree node checksums (Phase 3). Runs after the data
	// allocator is set up (it MarkAllocates rewritten leaf blocks in
	// plan.dataAlloc) and before any compaction/rewrite that might move blocks.
	if opts.RepairBtreeCRC {
		if err := repairBtreeChecksums(fs, plan, blockSize, dataRegionStart); err != nil {
			return fmt.Errorf("repair btree checksums: %w", err)
		}
	}

	// 2c. Rebuild corrupt B+ tree extent indexes from recovered extents
	// (Phase 4). Runs after Phase 3 (so torn-but-valid leaf checksums are already
	// rewritten) and before compaction. It allocates new node blocks and frees
	// old ones directly in plan.dataAlloc, and stages rebuilt inodes in
	// plan.inodes — both flushed later by writeModifiedInodes / writeAllocator.
	if opts.RebuildBtree {
		if err := rebuildBtreeIndex(fs, plan, blockSize, dataRegionStart); err != nil {
			return fmt.Errorf("rebuild btree indexes: %w", err)
		}
	}

	// 2d. Reclaim orphan B-tree node blocks (Phase 5). Runs after the rebuild
	// (so a rebuilt tree's fresh node blocks are already in fs.usedBlocks via the
	// rebuilt inodes' extents... note: usedBlocks was populated during the verify
	// pass before runRepair; rebuilt nodes are tracked via plan.dataAlloc
	// MarkAllocated, not usedBlocks). It scans for allocated, unreferenced blocks
	// carrying BtreeMagic and frees them when opts.ReclaimOrphanBtree is set.
	// Always runs (to surface orphans) but only frees when opted in; skipped when
	// any tree walk failed so unreached-but-live node blocks are not freed.
	if err := reclaimOrphanBtree(fs, plan, opts, blockSize, dataRegionStart, dataBlockCount); err != nil {
		return fmt.Errorf("reclaim orphan btree blocks: %w", err)
	}

	// 3. Compact file extents.
	if opts.CompactExtents {
		if err := compactFileExtents(fs, plan, blockSize); err != nil {
			return fmt.Errorf("compact file extents: %w", err)
		}
	}

	// 4. Compact directory tries.
	if opts.CompactTries {
		if err := compactDirectoryTries(fs, plan, blockSize); err != nil {
			return fmt.Errorf("compact directory tries: %w", err)
		}
	}

	// Directory inodes may have received a new trie root during compaction.
	// Keep the in-memory state in sync so the remaining repair steps walk the
	// current on-disk tries.
	if opts.CompactTries {
		for i := range fs.dirs {
			d := &fs.dirs[i]
			if updated, ok := plan.inodes[d.ino]; ok {
				d.trieRoot = updated.DirTrieRoot
				if orig, ok := fs.inodes[d.ino]; ok {
					orig.DirTrieRoot = updated.DirTrieRoot
				}
			}
		}
	}

	// 5. Repair link counts.
	if opts.RepairLinks {
		if err := repairLinkCounts(fs, plan, blockSize); err != nil {
			return fmt.Errorf("repair link counts: %w", err)
		}
	}

	// 6. Stage summary values for superblock and checkpoint.
	plan.freeDataBlks = plan.dataAlloc.FreeCount
	plan.freeInodes = plan.inodeAlloc.FreeCount
	plan.checkpointSeq = fs.sb.CheckpointSeq + 1

	// 7. Write modified inodes back to the inode table.
	if err := writeModifiedInodes(fs.file, fs.sb, plan, blockSize); err != nil {
		return fmt.Errorf("write modified inodes: %w", err)
	}

	// 8. Write allocators.
	if err := writeAllocator(fs.file, fs.sb.InodeBMOffset, blockSize, plan.inodeAlloc); err != nil {
		return fmt.Errorf("write inode allocator: %w", err)
	}
	if err := writeAllocator(fs.file, fs.sb.TrieNodePoolStart, blockSize, plan.dataAlloc); err != nil {
		return fmt.Errorf("write data allocator: %w", err)
	}

	// 9. Write superblock with corrected free counts.
	fs.sb.FreeDataBlks = plan.freeDataBlks
	fs.sb.FreeInodes = plan.freeInodes
	fs.sb.CheckpointSeq = plan.checkpointSeq
	fs.sb.JournalLogStart = fs.sb.JournalOffset
	fs.sb.JournalLogEnd = fs.sb.JournalOffset
	if err := writeSuperblock(fs.file, fs.sb, blockSize); err != nil {
		return fmt.Errorf("write superblock: %w", err)
	}

	// 10. Write fresh checkpoint block.
	if err := writeCheckpoint(fs.file, fs.sb, blockSize, plan); err != nil {
		return fmt.Errorf("write checkpoint: %w", err)
	}

	return nil
}

// recomputeAllocatorFreeCount recalculates FreeCount from the L2 bitmap, masking
// off bits beyond BlockCount. In the BrieFS allocator a set bit means "free",
// so FreeCount is the number of set bits in the valid range. This is used after
// loading an allocator from disk so any written header count matches the bitmap.
func recomputeAllocatorFreeCount(b *briefs.AllocBuilder) {
	if b.BlockCount == 0 {
		b.FreeCount = 0
		return
	}
	fullWords := b.BlockCount / 64
	rem := b.BlockCount % 64
	free := uint64(0)
	for i := uint64(0); i < fullWords && i < uint64(len(b.L2)); i++ {
		free += uint64(bits.OnesCount64(b.L2[i]))
	}
	if rem > 0 && fullWords < uint64(len(b.L2)) {
		mask := (uint64(1) << rem) - 1
		free += uint64(bits.OnesCount64(b.L2[fullWords] & mask))
	}
	if free > b.BlockCount {
		free = b.BlockCount
	}
	b.FreeCount = free
}

// loadAllocatorFromDisk reads an allocator pool from disk into an AllocBuilder.
// This is used for selective repairs that must allocate/free blocks without
// rebuilding the allocator bitmaps from scratch.
func loadAllocatorFromDisk(file *os.File, poolBlock, blockSize, blockCount uint64) (*briefs.AllocBuilder, error) {
	l0, l1, l2, hdr, err := briefs.ReadAllocatorBitmap(file, poolBlock, blockSize)
	if err != nil {
		return nil, err
	}
	_ = blockCount // kept for symmetry with NewAllocBuilder; hdr.BlockCount is authoritative
	b := &briefs.AllocBuilder{
		L0:         l0,
		L1:         l1,
		L2:         l2,
		BlockCount: hdr.BlockCount,
		FreeCount:  hdr.FreeCount,
	}
	recomputeAllocatorFreeCount(b)
	return b, nil
}

// compactFileExtents is currently a no-op. On v0.9 the extent index is a B+ tree
// (InodeFlagIndexed) for files with >8 extents and a sorted, already-merged
// inline array for files with <=8 extents; the kernel's insert path merges
// adjacent extents, so there is nothing to compact on a consistent image. The
// old chain-block collect/rewrite/compaction logic was removed with the chain
// format. B-tree leaf compaction (reclaiming half-empty leaves after heavy
// delete churn) is a separate, deferred concern -- see #7 phase 4.
func compactFileExtents(fs *fsckState, plan *repairPlan, blockSize uint64) error {
	return nil
}

// writeAllocator writes a freshly built allocator pyramid to disk.
func writeAllocator(file *os.File, poolBlock, blockSize uint64, alloc *briefs.AllocBuilder) error {
	blocks := alloc.WriteBlocks()
	for i, buf := range blocks {
		off := int64((poolBlock + uint64(i)) * blockSize)
		if len(buf) < int(blockSize) {
			// WriteBlocks always returns full 4096-byte blocks; guard anyway.
			padded := make([]byte, blockSize)
			copy(padded, buf)
			buf = padded
		}
		if _, err := file.WriteAt(buf, off); err != nil {
			return fmt.Errorf("write allocator block %d at offset %d: %w", i, off, err)
		}
	}
	return nil
}

// writeSuperblock writes the updated superblock to block 0.
func writeSuperblock(file *os.File, sb *briefs.SuperblockLayout, blockSize uint64) error {
	super := &briefs.Superblock{Lay: *sb}
	buf := make([]byte, blockSize)
	copy(buf, super.MarshalBinary())
	if _, err := file.WriteAt(buf, 0); err != nil {
		return fmt.Errorf("write superblock: %w", err)
	}
	return nil
}

// writeCheckpoint writes a fresh JRN_CHECKPOINT record to the last journal
// block, clearing the active log range so the kernel sees a clean journal.
func writeCheckpoint(file *os.File, sb *briefs.SuperblockLayout, blockSize uint64, plan *repairPlan) error {
	checkpointBlock := sb.JournalOffset + sb.JournalBlocks - 1
	buf := make([]byte, blockSize)

	// Checkpoint block header (16 bytes)
	binary.LittleEndian.PutUint32(buf[0:], briefs.MagicCheckpoint)
	binary.LittleEndian.PutUint32(buf[4:], 0) // block_seq (unused by fsck)
	binary.LittleEndian.PutUint32(buf[8:], 1) // record_count

	// Record header at offset 16
	const recOff = 16
	binary.LittleEndian.PutUint32(buf[recOff:], uint32(briefs.JRN_CHECKPOINT))
	binary.LittleEndian.PutUint32(buf[recOff+4:], 0) // flags
	binary.LittleEndian.PutUint32(buf[recOff+8:], briefs.CheckpointSize)

	cp := &briefs.Checkpoint{
		Seq:            plan.checkpointSeq,
		RecordCount:    1,
		LogSequenceEnd: sb.JournalOffset,
		TrieRootNode:   sb.TrieRootBlock,
		FreeDataCount:  plan.freeDataBlks,
		FreeInodeCount: plan.freeInodes,
	}
	cpData, err := cp.MarshalBinary()
	if err != nil {
		return fmt.Errorf("marshal checkpoint: %w", err)
	}
	copy(buf[recOff+16:], cpData)

	checksum := briefs.ComputeJournalRecordChecksum(uint32(briefs.JRN_CHECKPOINT), 0, cpData)
	binary.LittleEndian.PutUint32(buf[recOff+12:], checksum)

	if _, err := file.WriteAt(buf, int64(checkpointBlock*blockSize)); err != nil {
		return fmt.Errorf("write checkpoint block %d: %w", checkpointBlock, err)
	}
	return nil
}

// repairLinkCounts recomputes and fixes inode nlink values. Files and symlinks
// should have nlinks equal to the number of directory entries that reference them;
// directories should have nlinks equal to 2 (for . and ..) plus the number of
// subdirectories they contain.
func repairLinkCounts(fs *fsckState, plan *repairPlan, blockSize uint64) error {
	// Count how many subdirectory entries each directory contains.
	subdirCount := make(map[uint64]int)
	for _, d := range fs.dirs {
		entries, err := collectDirectoryEntries(fs, d.ino, d.trieRoot, blockSize)
		if err != nil {
			return fmt.Errorf("ino %d: collect directory entries: %w", d.ino, err)
		}
		for _, e := range entries {
			target, ok := fs.inodes[e.Inode]
			if !ok {
				continue
			}
			if target.IsDir() {
				subdirCount[d.ino]++
			}
		}
	}

	for ino, in := range fs.inodes {
		var expected uint32
		switch {
		case in.IsDir():
			expected = uint32(2 + subdirCount[ino])
		case in.IsFile() || in.IsSymlink():
			expected = uint32(fs.entryCounts[ino])
		default:
			continue
		}

		if in.Nlinks != expected {
			clone, ok := plan.inodes[ino]
			if !ok {
				c := *in
				clone = &c
			}
			clone.Nlinks = expected
			plan.inodes[ino] = clone
		}
	}
	return nil
}
