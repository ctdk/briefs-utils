package main

import (
	"encoding/binary"
	"fmt"
	"math/bits"
	"os"

	"github.com/ctdk/briefs-utils/types"
)

// runRepair rebuilds allocator state from the verified metadata and writes the
// repaired allocator, superblock free counts, and a fresh checkpoint back to disk.
// This covers Phase 2 of the fsck repair roadmap:
//   - rebuild data/inode allocator bitmaps from the structures fsck found;
//   - fix superblock free count mismatches;
//   - free blocks that are allocated but not referenced (no failed trie walks).
func runRepair(fs *fsckState, blockSize uint64, totalInodes int, opts *repairOptions) error {
	plan := &repairPlan{
		inodes: make(map[uint64]*types.Inode),
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
		plan.dataAlloc = types.NewAllocBuilder(dataBlockCount)
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
	inodeHeader, err := types.ReadAllocatorHeader(fs.file, fs.sb.InodeBMOffset, blockSize)
	if err != nil {
		return fmt.Errorf("read inode allocator header: %w", err)
	}
	if opts.RebuildAllocator {
		plan.inodeAlloc = types.NewAllocBuilder(inodeHeader.BlockCount)
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
func recomputeAllocatorFreeCount(b *types.AllocBuilder) {
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
func loadAllocatorFromDisk(file *os.File, poolBlock, blockSize, blockCount uint64) (*types.AllocBuilder, error) {
	l0, l1, l2, hdr, err := types.ReadAllocatorBitmap(file, poolBlock, blockSize)
	if err != nil {
		return nil, err
	}
	_ = blockCount // kept for symmetry with NewAllocBuilder; hdr.BlockCount is authoritative
	b := &types.AllocBuilder{
		L0:         l0,
		L1:         l1,
		L2:         l2,
		BlockCount: hdr.BlockCount,
		FreeCount:  hdr.FreeCount,
	}
	recomputeAllocatorFreeCount(b)
	return b, nil
}

// compactFileExtents merges adjacent extents and moves extent lists back inline
// when possible. Modified inodes are staged in the repair plan; freed data
// blocks are returned to the plan's allocator.
func compactFileExtents(fs *fsckState, plan *repairPlan, blockSize uint64) error {
	for ino, in := range fs.inodes {
		if in.Flags&types.InodeFlagInlineData != 0 {
			continue
		}
		if !in.IsFile() && !in.IsSymlink() {
			continue
		}

		extents, err := collectInodeExtentsForRepair(fs, in, blockSize)
		if err != nil {
			return fmt.Errorf("ino %d: collect extents: %w", ino, err)
		}
		if len(extents) == 0 {
			continue
		}

		merged := mergeAdjacentExtents(extents)
		if len(merged) == len(extents) {
			// Nothing changed; skip this inode.
			continue
		}

		oldExtents := extents
		newIn := *in
		if err := rewriteInodeExtents(&newIn, merged, blockSize); err != nil {
			return fmt.Errorf("ino %d: rewrite extents: %w", ino, err)
		}

		// Free old chain blocks and any physical blocks that disappeared.
		if err := freeOldExtentStorage(fs, plan, &newIn, oldExtents, blockSize); err != nil {
			return fmt.Errorf("ino %d: free old extent storage: %w", ino, err)
		}

		plan.inodes[ino] = &newIn
	}
	return nil
}

// collectInodeExtentsForRepair returns all logical extents for an inode,
// including inline and chain extents, sorted by logical offset.
func collectInodeExtentsForRepair(fs *fsckState, in *types.Inode, blockSize uint64) ([]types.Extent, error) {
	var extents []types.Extent
	inline := in.InlineExtents()
	for i := uint32(0); i < in.NumExtentsInline; i++ {
		extents = append(extents, inline[i])
	}

	remaining := int(in.NumExtentsTotal) - int(in.NumExtentsInline)
	chainBlock := in.ExtentInlineBase
	for chainBlock != 0 && remaining > 0 {
		buf := make([]byte, blockSize)
		if _, err := fs.file.ReadAt(buf, int64(chainBlock*blockSize)); err != nil {
			return nil, fmt.Errorf("read chain block %d: %w", chainBlock, err)
		}
		if err := types.VerifyChainChecksum(buf, blockSize); err != nil {
			return nil, fmt.Errorf("chain block %d checksum: %w", chainBlock, err)
		}
		hdr := types.UnmarshalExtentChainHeader(buf)
		n := int(hdr.NumExtentsInBlock)
		if n > remaining {
			n = remaining
		}
		for i := 0; i < n; i++ {
			extents = append(extents, types.ReadChainExtent(buf, i))
		}
		remaining -= n
		chainBlock = hdr.NextOverflowBlock
	}

	// Sort by logical offset to make merging straightforward.
	sortExtentsByOffset(extents)
	return extents, nil
}

// sortExtentsByOffset sorts extents in place by logical offset.
func sortExtentsByOffset(extents []types.Extent) {
	// Simple insertion sort; extent lists are usually tiny.
	for i := 1; i < len(extents); i++ {
		j := i
		for j > 0 && extents[j-1].Offset > extents[j].Offset {
			extents[j-1], extents[j] = extents[j], extents[j-1]
			j--
		}
	}
}

// mergeAdjacentExtents merges extents that are logically and physically adjacent.
func mergeAdjacentExtents(extents []types.Extent) []types.Extent {
	if len(extents) == 0 {
		return nil
	}
	out := []types.Extent{extents[0]}
	for i := 1; i < len(extents); i++ {
		last := &out[len(out)-1]
		cur := extents[i]
		if cur.Flags != last.Flags {
			out = append(out, cur)
			continue
		}
		if last.Offset+last.Len == cur.Offset && last.Phys+last.Len == cur.Phys {
			last.Len += cur.Len
		} else {
			out = append(out, cur)
		}
	}
	return out
}

// rewriteInodeExtents stores the compacted extent list in the inode. If it fits
// inline, all extents go into the inline region and chain blocks are cleared.
// Otherwise it packs extents into the minimum number of chain blocks.
func rewriteInodeExtents(in *types.Inode, extents []types.Extent, blockSize uint64) error {
	// Zero the inline region first.
	var zeroInline [8]types.Extent
	in.SetInlineExtents(zeroInline)
	in.NumExtentsInline = 0
	in.NumExtentsTotal = uint64(len(extents))
	in.ExtentInlineBase = 0

	if len(extents) <= 8 {
		var inline [8]types.Extent
		copy(inline[:], extents)
		in.SetInlineExtents(inline)
		in.NumExtentsInline = uint32(len(extents))
		return nil
	}

	// Chain-backed: we need to allocate chain blocks. For now, preserve the
	// existing ExtentInlineBase by leaving a marker; the caller (freeOldExtentStorage)
	// will allocate fresh blocks and patch this field. This avoids needing an
	// allocator here.
	in.ExtentInlineBase = 0xFFFFFFFFFFFFFFFF // sentinel; will be overwritten
	in.NumExtentsInline = 0
	return nil
}

// freeOldExtentStorage frees the old chain blocks and any physical blocks that
// are no longer covered by the new extent list. It then allocates fresh chain
// blocks (if needed) and writes the compacted extents.
func freeOldExtentStorage(fs *fsckState, plan *repairPlan, newIn *types.Inode, oldExtents []types.Extent, blockSize uint64) error {
	ino := newIn.InodeNumber

	// Build sets of physical blocks used by old and new extents.
	oldPhys := make(map[uint64]bool)
	for _, ext := range oldExtents {
		if ext.Flags&types.ExtentFlagHole != 0 {
			continue
		}
		for b := uint64(0); b < ext.Len; b++ {
			oldPhys[ext.Phys+b] = true
		}
	}

	newExtents := newIn.InlineExtents()
	var newExtentsList []types.Extent
	for i := uint32(0); i < newIn.NumExtentsInline; i++ {
		newExtentsList = append(newExtentsList, newExtents[i])
	}
	// If the sentinel is set, the new extents are chain-backed and we need to
	// rebuild them below.
	if newIn.ExtentInlineBase == 0xFFFFFFFFFFFFFFFF {
		newExtentsList = newExtentsList[:0]
	}

	newPhys := make(map[uint64]bool)
	for _, ext := range newExtentsList {
		if ext.Flags&types.ExtentFlagHole != 0 {
			continue
		}
		for b := uint64(0); b < ext.Len; b++ {
			newPhys[ext.Phys+b] = true
		}
	}

	// Free old chain blocks.
	origIn, ok := fs.inodes[ino]
	if !ok {
		return fmt.Errorf("inode %d not found in fsck state", ino)
	}
	if origIn.NumExtentsTotal > uint64(origIn.NumExtentsInline) && origIn.ExtentInlineBase != 0 {
		chainBlock := origIn.ExtentInlineBase
		for chainBlock != 0 {
			buf := make([]byte, blockSize)
			if _, err := fs.file.ReadAt(buf, int64(chainBlock*blockSize)); err != nil {
				break
			}
			hdr := types.UnmarshalExtentChainHeader(buf)
			if chainBlock >= fs.sb.TrieNodePoolStart+fs.sb.TrieNodePoolSize &&
				chainBlock < fs.sb.JournalOffset {
				plan.dataAlloc.MarkFree(chainBlock - (fs.sb.TrieNodePoolStart + fs.sb.TrieNodePoolSize))
			}
			chainBlock = hdr.NextOverflowBlock
		}
	}

	// Free physical data blocks no longer referenced.
	dataRegionStart := fs.sb.TrieNodePoolStart + fs.sb.TrieNodePoolSize
	for phys := range oldPhys {
		if !newPhys[phys] && phys >= dataRegionStart && phys < fs.sb.JournalOffset {
			plan.dataAlloc.MarkFree(phys - dataRegionStart)
		}
	}

	// If the inode is chain-backed, allocate fresh chain blocks and write them.
	if newIn.ExtentInlineBase == 0xFFFFFFFFFFFFFFFF {
		chainBlocks, err := allocateChainBlocks(plan, fs.sb, newIn, blockSize)
		if err != nil {
			return err
		}
		if err := writeChainBlocks(fs.file, newIn, chainBlocks, blockSize, dataRegionStart); err != nil {
			return fmt.Errorf("write chain blocks for ino %d: %w", ino, err)
		}
	}

	return nil
}

// allocateChainBlocks reserves chain blocks for a chain-backed inode and
// returns the list of absolute chain block numbers. The caller must later call
// writeChainBlocks to serialize the extents into those blocks.
func allocateChainBlocks(plan *repairPlan, sb *types.SuperblockLayout, in *types.Inode, blockSize uint64) ([]uint64, error) {
	extentsPerBlock := types.ExtentsPerChainBlock(blockSize)
	numExtents := int(in.NumExtentsTotal)
	numChainBlocks := (numExtents + extentsPerBlock - 1) / extentsPerBlock
	if numChainBlocks == 0 {
		return nil, nil
	}

	dataRegionStart := sb.TrieNodePoolStart + sb.TrieNodePoolSize
	absBlocks := make([]uint64, 0, numChainBlocks)
	for i := 0; i < numChainBlocks; i++ {
		rel, err := plan.dataAlloc.AllocateBlock()
		if err != nil {
			return nil, fmt.Errorf("no space for chain block %d: %w", i, err)
		}
		absBlocks = append(absBlocks, rel+dataRegionStart)
	}
	in.ExtentInlineBase = absBlocks[0]
	return absBlocks, nil
}

// writeChainBlocks serializes the extents of a chain-backed inode into the
// allocated chain blocks and writes them to disk.
func writeChainBlocks(file *os.File, in *types.Inode, chainBlocks []uint64, blockSize, dataRegionStart uint64) error {
	extentsPerBlock := types.ExtentsPerChainBlock(blockSize)
	extents := in.InlineExtents()
	// For a chain-backed inode we store all extents in chain blocks.
	totalExtents := int(in.NumExtentsTotal)

	for bi, absBlock := range chainBlocks {
		buf := make([]byte, blockSize)
		next := uint64(0)
		if bi+1 < len(chainBlocks) {
			next = chainBlocks[bi+1]
		}
		binary.LittleEndian.PutUint64(buf[0:], next)

		start := bi * extentsPerBlock
		end := start + extentsPerBlock
		if end > totalExtents {
			end = totalExtents
		}
		numExtents := end - start
		binary.LittleEndian.PutUint32(buf[8:], uint32(numExtents))
		binary.LittleEndian.PutUint32(buf[12:], 0)

		for i := 0; i < numExtents; i++ {
			ext := extents[start+i]
			off := types.ExtentChainHeaderSize + i*32
			binary.LittleEndian.PutUint64(buf[off:], ext.Offset)
			binary.LittleEndian.PutUint64(buf[off+8:], ext.Phys)
			binary.LittleEndian.PutUint64(buf[off+16:], ext.Len)
			binary.LittleEndian.PutUint32(buf[off+24:], ext.Flags)
			binary.LittleEndian.PutUint32(buf[off+28:], ext.Pad)
		}

		checksum := types.ComputeChainChecksum(buf, blockSize)
		binary.LittleEndian.PutUint64(buf[types.ExtentChainChecksumOffset:], checksum)

		if _, err := file.WriteAt(buf, int64(absBlock*blockSize)); err != nil {
			return fmt.Errorf("write chain block %d: %w", absBlock, err)
		}
	}
	return nil
}

// writeAllocator writes a freshly built allocator pyramid to disk.
func writeAllocator(file *os.File, poolBlock, blockSize uint64, alloc *types.AllocBuilder) error {
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
func writeSuperblock(file *os.File, sb *types.SuperblockLayout, blockSize uint64) error {
	super := &types.Superblock{Lay: *sb}
	buf := make([]byte, blockSize)
	copy(buf, super.MarshalBinary())
	if _, err := file.WriteAt(buf, 0); err != nil {
		return fmt.Errorf("write superblock: %w", err)
	}
	return nil
}

// writeCheckpoint writes a fresh JRN_CHECKPOINT record to the last journal
// block, clearing the active log range so the kernel sees a clean journal.
func writeCheckpoint(file *os.File, sb *types.SuperblockLayout, blockSize uint64, plan *repairPlan) error {
	checkpointBlock := sb.JournalOffset + sb.JournalBlocks - 1
	buf := make([]byte, blockSize)

	// Checkpoint block header (16 bytes)
	binary.LittleEndian.PutUint32(buf[0:], types.MagicCheckpoint)
	binary.LittleEndian.PutUint32(buf[4:], 0) // block_seq (unused by fsck)
	binary.LittleEndian.PutUint32(buf[8:], 1) // record_count

	// Record header at offset 16
	const recOff = 16
	binary.LittleEndian.PutUint32(buf[recOff:], uint32(types.JRN_CHECKPOINT))
	binary.LittleEndian.PutUint32(buf[recOff+4:], 0) // flags
	binary.LittleEndian.PutUint32(buf[recOff+8:], types.CheckpointSize)

	cp := &types.Checkpoint{
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

	checksum := types.ComputeJournalRecordChecksum(uint32(types.JRN_CHECKPOINT), 0, cpData)
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
