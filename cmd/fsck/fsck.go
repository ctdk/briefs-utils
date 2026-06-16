// fsck.briefs validates and repairs a BrieFS filesystem.
package main

import (
	"encoding/binary"
	"fmt"
	"math/bits"
	"os"
	"sort"

	"github.com/ctdk/briefs-utils/device"
	"github.com/ctdk/briefs-utils/types"
	"github.com/urfave/cli/v2"
)

var versionStr = fmt.Sprintf("v%d.%d.%d", types.BrieFSMajorVersion, types.BrieFSMinorVersion, types.BrieFSPatchVersion)

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

func (fs *fsckState) errorf(format string, args ...interface{}) {
	fs.errors++
	fmt.Fprintf(os.Stderr, "  ERROR: "+format+"\n", args...)
}

func (fs *fsckState) warnf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "  WARNING: "+format+"\n", args...)
}

func verifyAllocatorPool(file *os.File, poolBlock, blockSize uint64, label string) error {
	hdr, err := types.ReadAllocatorHeader(file, poolBlock, blockSize)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "  %s: pool at block %d, %d entries, %d free\n", label, poolBlock, hdr.BlockCount, hdr.FreeCount)
	fmt.Fprintf(os.Stderr, "    levels: L0=%d words, L1=%d words, L2=%d words\n", hdr.L0Words, hdr.L1Words, hdr.L2Words)

	return nil
}

// readAllocatorHeader reads the allocator pool header and returns all fields.
func readAllocatorHeader(file *os.File, poolBlock, blockSize uint64) (l0w, l1w, l2w, blockCount, freeCount uint64, err error) {
	hdr, err := types.ReadAllocatorHeader(file, poolBlock, blockSize)
	if err != nil {
		return 0, 0, 0, 0, 0, err
	}
	return hdr.L0Words, hdr.L1Words, hdr.L2Words, hdr.BlockCount, hdr.FreeCount, nil
}

// verifyAllocatorBitmap reads and validates the full 3-level allocator bitmap.
// It checks:
//   - L0 bits correctly summarize L1 (a set L0 bit means at least one L1 word under it is non-zero)
//   - L1 bits correctly summarize L2 (a set L1 bit means at least one L2 word under it is non-zero)
//   - Trailing bits in the last L0/L1/L2 word are properly masked
//   - Computed free count from L2 matches the header's free count
func verifyAllocatorBitmap(fs *fsckState, poolBlock, blockSize uint64, l0w, l1w, l2w, blockCount, headerFree uint64, label string) {
	wordsPerBlock := blockSize / 8 // 512 u64 words per 4096-byte block

	// Compute expected level sizes
	expectedL2 := (blockCount + 63) / 64
	expectedL1 := (expectedL2 + 63) / 64
	expectedL0 := (expectedL1 + 63) / 64
	if expectedL0 < 1 {
		expectedL0 = 1
	}
	if expectedL1 < 1 {
		expectedL1 = 1
	}
	if expectedL2 < 1 {
		expectedL2 = 1
	}

	if l0w != expectedL0 {
		fs.errorf("%s: L0 word count mismatch: header says %d, expected %d", label, l0w, expectedL0)
	}
	if l1w != expectedL1 {
		fs.errorf("%s: L1 word count mismatch: header says %d, expected %d", label, l1w, expectedL1)
	}
	if l2w != expectedL2 {
		fs.errorf("%s: L2 word count mismatch: header says %d, expected %d", label, l2w, expectedL2)
	}

	// Read all L0 words
	l0Blocks := (l0w + wordsPerBlock - 1) / wordsPerBlock
	l0 := make([]uint64, l0w)
	for bi := uint64(0); bi < l0Blocks; bi++ {
		buf := make([]byte, blockSize)
		block := poolBlock + 1 + bi
		if _, err := fs.file.ReadAt(buf, int64(block*blockSize)); err != nil {
			fs.errorf("%s: read L0 block %d: %v", label, block, err)
			return
		}
		start := bi * wordsPerBlock
		for j := uint64(0); j < wordsPerBlock && start+j < l0w; j++ {
			l0[start+j] = binary.LittleEndian.Uint64(buf[j*8:])
		}
	}

	// Read all L1 words
	l1Start := poolBlock + 1 + l0Blocks
	l1Blocks := (l1w + wordsPerBlock - 1) / wordsPerBlock
	l1 := make([]uint64, l1w)
	for bi := uint64(0); bi < l1Blocks; bi++ {
		buf := make([]byte, blockSize)
		block := l1Start + bi
		if _, err := fs.file.ReadAt(buf, int64(block*blockSize)); err != nil {
			fs.errorf("%s: read L1 block %d: %v", label, block, err)
			return
		}
		start := bi * wordsPerBlock
		for j := uint64(0); j < wordsPerBlock && start+j < l1w; j++ {
			l1[start+j] = binary.LittleEndian.Uint64(buf[j*8:])
		}
	}

	// Read all L2 words
	l2Start := l1Start + l1Blocks
	l2Blocks := (l2w + wordsPerBlock - 1) / wordsPerBlock
	l2 := make([]uint64, l2w)
	for bi := uint64(0); bi < l2Blocks; bi++ {
		buf := make([]byte, blockSize)
		block := l2Start + bi
		if _, err := fs.file.ReadAt(buf, int64(block*blockSize)); err != nil {
			fs.errorf("%s: read L2 block %d: %v", label, block, err)
			return
		}
		start := bi * wordsPerBlock
		for j := uint64(0); j < wordsPerBlock && start+j < l2w; j++ {
			l2[start+j] = binary.LittleEndian.Uint64(buf[j*8:])
		}
	}

	// Verify trailing bits in last L2 word are properly masked
	if tail := blockCount % 64; tail != 0 {
		lastWord := l2[len(l2)-1]
		mask := (uint64(1) << tail) - 1
		if lastWord&^mask != 0 {
			fs.errorf("%s: trailing bits set in last L2 word (0x%016X, mask 0x%016X)", label, lastWord, mask)
		}
	}

	// Verify trailing bits in last L1 word
	if tail := l2w % 64; tail != 0 {
		lastWord := l1[len(l1)-1]
		mask := (uint64(1) << tail) - 1
		if lastWord&^mask != 0 {
			fs.errorf("%s: trailing bits set in last L1 word (0x%016X, mask 0x%016X)", label, lastWord, mask)
		}
	}

	// Verify trailing bits in last L0 word
	if tail := l1w % 64; tail != 0 {
		lastWord := l0[len(l0)-1]
		mask := (uint64(1) << tail) - 1
		if lastWord&^mask != 0 {
			fs.errorf("%s: trailing bits set in last L0 word (0x%016X, mask 0x%016X)", label, lastWord, mask)
		}
	}

	// Verify L1 -> L2 pyramid: for each L1 word, check its bits correctly
	// summarize the corresponding L2 words.
	l1Errors := 0
	for i := uint64(0); i < l1w; i++ {
		expected := uint64(0)
		start := i * 64
		for j := uint64(0); j < 64 && start+j < l2w; j++ {
			if l2[start+j] != 0 {
				expected |= 1 << j
			}
		}
		if l1[i] != expected {
			if l1Errors < 10 {
				fs.errorf("%s: L1 word %d mismatch: on-disk 0x%016X, computed 0x%016X", label, i, l1[i], expected)
			} else if l1Errors == 10 {
				fs.errorf("%s: (more L1 errors suppressed)", label)
			}
			l1Errors++
		}
	}

	// Verify L0 -> L1 pyramid
	l0Errors := 0
	for i := uint64(0); i < l0w; i++ {
		expected := uint64(0)
		start := i * 64
		for j := uint64(0); j < 64 && start+j < l1w; j++ {
			if l1[start+j] != 0 {
				expected |= 1 << j
			}
		}
		if l0[i] != expected {
			if l0Errors < 10 {
				fs.errorf("%s: L0 word %d mismatch: on-disk 0x%016X, computed 0x%016X", label, i, l0[i], expected)
			} else if l0Errors == 10 {
				fs.errorf("%s: (more L0 errors suppressed)", label)
			}
			l0Errors++
		}
	}

	// Compute actual free count from L2 bitmap
	computedFree := uint64(0)
	for i := uint64(0); i < l2w; i++ {
		computedFree += uint64(popcount64(l2[i]))
	}

	if computedFree != headerFree {
		fs.errorf("%s: free count mismatch: header says %d, bitmap scan says %d", label, headerFree, computedFree)
	}

	if l1Errors > 0 || l0Errors > 0 {
		return
	}

	fmt.Fprintf(os.Stderr, "  %s bitmap pyramid: consistent (%d L0, %d L1, %d L2 words, %d free)\n",
		label, l0w, l1w, l2w, computedFree)
}

// popcount64 returns the number of set bits in a 64-bit word.
func popcount64(x uint64) int {
	// simple parallel popcount
	x = x - ((x >> 1) & 0x5555555555555555)
	x = (x & 0x3333333333333333) + ((x >> 2) & 0x3333333333333333)
	x = (x + (x >> 4)) & 0x0F0F0F0F0F0F0F0F
	return int((x * 0x0101010101010101) >> 56)
}

func verifySuperblock(file *os.File, blockSize uint64) (*types.SuperblockLayout, error) {
	sb, err := types.ReadSuperblock(file, blockSize)
	if err != nil {
		return nil, err
	}

	return sb, nil
}

// verifyInode checks a single inode from an already-read buffer.
// Returns the parsed inode if valid, or an error.
func verifyInode(buf []byte, ino, byteOffset, inodeSize uint64) (*types.Inode, error) {
	inodeBuf := buf[byteOffset : byteOffset+inodeSize]
	magic := binary.LittleEndian.Uint64(inodeBuf[8:])
	if magic == 0 {
		return nil, nil // unallocated inode
	}
	if magic != types.MagicInode {
		return nil, fmt.Errorf("ino %d: bad magic 0x%016X", ino, magic)
	}

	// Use the existing UnmarshalInode for full parsing
	in, err := types.UnmarshalInode(inodeBuf)
	if err != nil {
		return nil, fmt.Errorf("ino %d: unmarshal failed: %w", ino, err)
	}

	// Validate inode number matches
	if in.InodeNumber != ino {
		return nil, fmt.Errorf("ino %d: stored inode number mismatch (%d)", ino, in.InodeNumber)
	}

	// Validate inline-data inodes
	if in.Flags&types.InodeFlagInlineData != 0 {
		if in.FileSize > 256 {
			return nil, fmt.Errorf("ino %d: inline-data file size %d > 256", ino, in.FileSize)
		}
		if in.NumExtentsTotal != 0 {
			return nil, fmt.Errorf("ino %d: inline-data inode has %d extents", ino, in.NumExtentsTotal)
		}
		if in.NumExtentsInline != 0 {
			return nil, fmt.Errorf("ino %d: inline-data inode has %d inline extents", ino, in.NumExtentsInline)
		}
		if in.ExtentInlineBase != 0 {
			return nil, fmt.Errorf("ino %d: inline-data inode has extent_inline_base %d", ino, in.ExtentInlineBase)
		}
		if in.IsDir() {
			return nil, fmt.Errorf("ino %d: directory cannot use inline data", ino)
		}
	} else {
		// Validate extent counts
		if in.NumExtentsInline > 8 {
			return nil, fmt.Errorf("ino %d: too many inline extents %d", ino, in.NumExtentsInline)
		}
		if in.NumExtentsTotal < uint64(in.NumExtentsInline) {
			return nil, fmt.Errorf("ino %d: total extents %d < inline extents %d", ino, in.NumExtentsTotal, in.NumExtentsInline)
		}
	}

	// Validate file mode
	mode := in.Filemode
	if mode == 0 {
		return nil, fmt.Errorf("ino %d: zero file mode", ino)
	}
	if mode&types.ModeDir == 0 && mode&types.ModeFile == 0 && mode&types.ModeSymlink == 0 {
		// Not a dir, file, or symlink — could be a special device, which is fine
	}

	// Validate xattr fields (no BrieFS code writes xattrs yet, so these
	// should always be zero on a healthy filesystem).
	if in.XattrOffset != 0 || in.XattrSize != 0 {
		// Record the xattr offset for later bitmap cross-referencing
		// (the caller will track used blocks, but we just flag it here)
		return in, fmt.Errorf("ino %d: unexpected xattr_offset=%d, xattr_size=%d (xattr not yet implemented)",
			ino, in.XattrOffset, in.XattrSize)
	}

	return in, nil
}

func verifyInodeTable(fs *fsckState, inodeTableBlock, inodeTableBlocks, blockSize, inodeSize uint64) (totalInodes int) {
	inodesPerBlock := blockSize / inodeSize
	ino := uint64(1)

	// Initialize maps for cross-referencing
	fs.inodes = make(map[uint64]*types.Inode)
	fs.dirs = nil
	fs.usedBlocks = make(map[uint64]bool)
	fs.entryCounts = make(map[uint64]int)
	fs.failedTrieDirs = make(map[uint64]bool)

	fmt.Fprintf(os.Stderr, "  inodes per block: %d\n", inodesPerBlock)

	for bi := uint64(0); bi < inodeTableBlocks; bi++ {
		buf := make([]byte, blockSize)
		if _, err := fs.file.ReadAt(buf, int64((inodeTableBlock+bi)*blockSize)); err != nil {
			fs.errorf("read inode table block %d: %v", inodeTableBlock+bi, err)
			ino += inodesPerBlock
			continue
		}

		for j := uint64(0); j < inodesPerBlock; j++ {
			offset := j * inodeSize
			magic := binary.LittleEndian.Uint64(buf[offset+8:])
			if magic == 0 {
				ino++
				continue
			}

			totalInodes++
			in, err := verifyInode(buf, ino, offset, inodeSize)
			if err != nil {
				fs.errorf("%v", err)
			}
			if in != nil {
				// Even if verifyInode returned warnings (xattr, etc.),
				// we still record the inode data for cross-referencing.
				fs.inodes[ino] = in

				// Collect extents for block cross-reference
				collectInodeExtents(fs, ino, in, blockSize)

				// Collect trie root for directory trie walking
				if in.IsDir() {
					if in.DirTrieRoot == 0 {
						fs.errorf("ino %d: directory with no trie root", ino)
					} else {
						fs.dirs = append(fs.dirs, dirInfo{ino: ino, trieRoot: in.DirTrieRoot})
						fs.usedBlocks[types.TrieRefBlock(in.DirTrieRoot)] = true
					}
				}

				// File with zero size but extents
				if in.IsFile() && in.FileSize == 0 && in.NumExtentsTotal > 0 {
					fs.warnf("ino %d: file with zero size but %d extents", ino, in.NumExtentsTotal)
				}
				// Non-inline file with non-zero size but no extents
				if in.IsFile() && in.FileSize > 0 && in.NumExtentsTotal == 0 && in.Flags&types.InodeFlagInlineData == 0 {
					fs.warnf("ino %d: file with size %d but no extents (not inline)", ino, in.FileSize)
				}
			}
			ino++
		}
	}

	return
}

// collectInodeExtents collects all blocks referenced by an inode's extents,
// including both inline extents and overflow chain blocks.
func collectInodeExtents(fs *fsckState, ino uint64, in *types.Inode, blockSize uint64) {
	// Inline-data inodes reference no data blocks.
	if in.Flags&types.InodeFlagInlineData != 0 {
		return
	}

	// Helper to record the blocks from a single extent.
	// Skips hole extents (ExtentFlagHole) which have no physical backing.
	addExtentBlocks := func(ext types.Extent) {
		// Validate extent flags
		if ext.Flags&types.ExtentFlagHole != 0 {
			// Hole extent — no physical blocks, skip
			return
		}
		if ext.Flags&types.ExtentFlagEof != 0 {
			// EOF marker — should only appear on the last extent
			// (we can't easily verify that here, but it's valid)
		}
		if ext.Flags & ^(uint32(types.ExtentFlagHole|types.ExtentFlagEof)) != 0 {
			fs.warnf("ino %d: extent with unknown flags 0x%08X (phys=%d, len=%d)",
				ino, ext.Flags, ext.Phys, ext.Len)
		}

		if ext.Len > 0 && ext.Phys > 0 {
			for bk := uint64(0); bk < ext.Len; bk++ {
				fs.usedBlocks[ext.Phys+bk] = true
			}
		}
	}

	// Collect inline extents
	inlineExtents := in.InlineExtents()
	for ei := uint32(0); ei < in.NumExtentsInline; ei++ {
		addExtentBlocks(inlineExtents[ei])
	}

	// Collect overflow extents from chain blocks
	if in.NumExtentsTotal > uint64(in.NumExtentsInline) && in.ExtentInlineBase != 0 {
		extentsPerBlock := types.ExtentsPerChainBlock(blockSize)
		remaining := int(in.NumExtentsTotal) - int(in.NumExtentsInline)
		chainBlock := in.ExtentInlineBase

		for chainBlock != 0 && remaining > 0 {
			buf := make([]byte, blockSize)
			if _, err := fs.file.ReadAt(buf, int64(chainBlock*blockSize)); err != nil {
				fs.errorf("ino %d: read extent chain block %d: %v", ino, chainBlock, err)
				break
			}

			// Verify extent chain block checksum
			if err := types.VerifyChainChecksum(buf, blockSize); err != nil {
				fs.errorf("ino %d: extent chain block %d: checksum mismatch (stored=0x%08X computed=0x%08X)", ino, chainBlock, types.ReadChainChecksum(buf, blockSize), types.ComputeChainChecksum(buf, blockSize))
				break
			}

			hdr := types.UnmarshalExtentChainHeader(buf)

			// Validate extent count vs capacity
			if hdr.NumExtentsInBlock > uint32(extentsPerBlock) {
				fs.errorf("ino %d: extent chain block %d: %d extents exceeds block capacity %d",
					ino, chainBlock, hdr.NumExtentsInBlock, extentsPerBlock)
				break
			}

			// Record chain block itself as used (it's metadata)
			fs.usedBlocks[chainBlock] = true

			// Process extents in this chain block
			n := int(hdr.NumExtentsInBlock)
			if n > remaining {
				n = remaining
			}
			for i := 0; i < n; i++ {
				ext := types.ReadChainExtent(buf, i)
				addExtentBlocks(ext)
			}

			remaining -= n
			chainBlock = hdr.NextOverflowBlock
		}

		if remaining > 0 {
			fs.errorf("ino %d: extent chain ended early: %d extents left unreachable (total=%d, inline=%d)",
				ino, remaining, in.NumExtentsTotal, in.NumExtentsInline)
		}
	}
}
// trieSlot mirrors the kernel's packed trie node slot.
type trieSlot struct {
	FirstChild  uint64
	NextSibling uint64
	Inode       uint64
	NameLen     uint16
	NameOffset  uint16
	Depth       uint8
	NodeType    uint8
	ByteVal     uint8
	FType       uint8
	Flags       uint16
	ChildCount  uint16
}

const trieSlotSize = 36
const triePageHeaderSize = 20
const trieSlotCount = types.TrieSlotsPerBlock

// trieIsLeaf returns true if the node has leaf data (pure leaf or INTERM+NODE_STATUS_LEAF).
func trieIsLeaf(nt uint8) bool {
	return (nt&types.NodeTypeInterm) == 0 || (nt&types.NodeStatusLeaf) != 0
}

// trieEntry represents a single directory entry found in the trie.
type trieEntry struct {
	Inode  uint64
	FType  uint8
	Name   string
	Parent uint64 // parent directory inode
}

// readTriePage reads and validates a packed trie page header.
func readTriePage(buf []byte) (magic uint32, liveCount uint16, freeSlots uint64, err error) {
	if uint64(len(buf)) < triePageHeaderSize {
		return 0, 0, 0, fmt.Errorf("buffer too small for trie page header")
	}
	magic = binary.LittleEndian.Uint32(buf[0:])
	if magic != types.MagicTriePage {
		return magic, 0, 0, fmt.Errorf("bad trie page magic 0x%08X (expected 0x%08X)", magic, types.MagicTriePage)
	}
	liveCount = binary.LittleEndian.Uint16(buf[8:])
	freeSlots = binary.LittleEndian.Uint64(buf[12:])
	return magic, liveCount, freeSlots, nil
}

// parseTrieSlot reads a single node slot from a page buffer.
func parseTrieSlot(buf []byte, slot uint) (trieSlot, error) {
	off := uint64(triePageHeaderSize + slot*trieSlotSize)
	if off+trieSlotSize > uint64(len(buf)) {
		return trieSlot{}, fmt.Errorf("slot %d out of range", slot)
	}
	return trieSlot{
		FirstChild:  binary.LittleEndian.Uint64(buf[off:]),
		NextSibling: binary.LittleEndian.Uint64(buf[off+8:]),
		Inode:       binary.LittleEndian.Uint64(buf[off+16:]),
		NameLen:     binary.LittleEndian.Uint16(buf[off+24:]),
		NameOffset:  binary.LittleEndian.Uint16(buf[off+26:]),
		Depth:       buf[off+28],
		NodeType:    buf[off+29],
		ByteVal:     buf[off+30],
		FType:       buf[off+31],
		Flags:       binary.LittleEndian.Uint16(buf[off+32:]),
		ChildCount:  binary.LittleEndian.Uint16(buf[off+34:]),
	}, nil
}

// extractTrieNodeName reads the name from the trailing bytes of a trie page buffer.
func extractTrieNodeName(buf []byte, node trieSlot) string {
	if node.NameLen < 2 || node.NameOffset == 0 {
		return ""
	}
	if int(node.NameOffset) > len(buf) {
		return ""
	}
	nameStart := len(buf) - int(node.NameOffset)
	if nameStart < 0 || nameStart+2 > len(buf) {
		return ""
	}
	storedLen := int(binary.LittleEndian.Uint16(buf[nameStart:]))
	if storedLen < 1 || storedLen > types.BrieFSMaxNameLen {
		return ""
	}
	if nameStart+2+storedLen > len(buf) {
		return ""
	}
	return string(buf[nameStart+2 : nameStart+2+storedLen])
}

// verifyDirectoryTrie walks a directory's packed trie, validating structure and collecting entries.
// Returns the list of entries found, or nil if the trie is empty.
func verifyDirectoryTrie(fs *fsckState, parentIno uint64, rootRef uint64, blockSize uint64) []trieEntry {
	if rootRef == 0 {
		return nil
	}

	// Track visited node references to detect cycles.
	visited := make(map[uint64]bool)
	var entries []trieEntry

	// Iterative depth-first walk using a stack of node references.
	stack := []uint64{rootRef}
	leafEmitted := []bool{false}

	for len(stack) > 0 {
		ref := stack[len(stack)-1]
		emitted := leafEmitted[len(leafEmitted)-1]
		stack = stack[:len(stack)-1]
		leafEmitted = leafEmitted[:len(leafEmitted)-1]

		if visited[ref] && !emitted {
			fs.errorf("ino %d dir trie: cycle detected at ref %d", parentIno, ref)
			continue
		}
		if !emitted {
			visited[ref] = true
		}

		block := types.TrieRefBlock(ref)
		slot := types.TrieRefSlot(ref)

		// Record the containing page as used.
		fs.usedBlocks[block] = true

		buf := make([]byte, blockSize)
		if _, err := fs.file.ReadAt(buf, int64(block*blockSize)); err != nil {
			fs.errorf("ino %d dir trie: read page %d: %v", parentIno, block, err)
			fs.failedTrieDirs[parentIno] = true
			continue
		}

		_, liveCount, freeSlots, err := readTriePage(buf)
		if err != nil {
			fs.errorf("ino %d dir trie: ref %d: %v", parentIno, ref, err)
			fs.failedTrieDirs[parentIno] = true
			continue
		}

		// Cross-check the page header's live_count against the free-slot bitmap.
		allocated := bits.OnesCount64(freeSlots)
		if allocated != int(trieSlotCount-liveCount) {
			fs.errorf("ino %d dir trie: page %d live_count=%d inconsistent with free_slots bitmap (%d allocated)",
				parentIno, block, liveCount, allocated)
		}

		if slot >= trieSlotCount {
			fs.errorf("ino %d dir trie: ref %d: slot %d out of range", parentIno, ref, slot)
			fs.failedTrieDirs[parentIno] = true
			continue
		}

		if freeSlots&(1<<slot) != 0 {
			fs.errorf("ino %d dir trie: ref %d: slot %d is marked free", parentIno, ref, slot)
			fs.failedTrieDirs[parentIno] = true
			continue
		}

		node, err := parseTrieSlot(buf, slot)
		if err != nil {
			fs.errorf("ino %d dir trie: ref %d: %v", parentIno, ref, err)
			fs.failedTrieDirs[parentIno] = true
			continue
		}

		// Validate node type.
		if node.NodeType != 0 && node.NodeType != types.NodeTypeInterm &&
			node.NodeType != (types.NodeTypeInterm|types.NodeStatusLeaf) {
			fs.errorf("ino %d dir trie: ref %d: invalid node type 0x%02X", parentIno, ref, node.NodeType)
		}

		// Validate node flags.
		if node.Flags&uint16(types.NodeFlagDeleted) != 0 {
			fs.warnf("ino %d dir trie: ref %d: NODE_FLAG_DELETED set (pending cleanup)", parentIno, ref)
		}
		if ref == rootRef && node.Flags&uint16(types.NodeFlagRoot) != 0 {
			// NODE_FLAG_ROOT defined but unused.
		}
		if ref != rootRef && node.Flags&uint16(types.NodeFlagRoot) != 0 {
			fs.errorf("ino %d dir trie: ref %d: NODE_FLAG_ROOT set on non-root node", parentIno, ref)
		}
		if node.Flags&^(uint16(types.NodeFlagDeleted|types.NodeFlagRoot)) != 0 {
			fs.warnf("ino %d dir trie: ref %d: unknown flags 0x%04X", parentIno, ref, node.Flags)
		}

		// Validate depth and byte_val for root.
		if ref == rootRef && node.Depth != 0 {
			fs.errorf("ino %d dir trie: root ref %d: depth is %d, expected 0", parentIno, ref, node.Depth)
		}
		if ref == rootRef && node.ByteVal != 0 {
			fs.errorf("ino %d dir trie: root ref %d: byte_val is %d, expected 0", parentIno, ref, node.ByteVal)
		}

		// Validate child_count vs first_child.
		if node.ChildCount == 0 && node.FirstChild != 0 {
			fs.errorf("ino %d dir trie: ref %d: child_count=0 but first_child=%d", parentIno, ref, node.FirstChild)
		}
		if node.ChildCount > 0 && node.FirstChild == 0 {
			fs.errorf("ino %d dir trie: ref %d: child_count=%d but first_child=0", parentIno, ref, node.ChildCount)
		}

		// Validate child/sibling ref ranges.
		if node.FirstChild > 0 && types.TrieRefBlock(node.FirstChild) >= fs.sb.TotalBlocks {
			fs.errorf("ino %d dir trie: ref %d: first_child block %d exceeds total blocks %d",
				parentIno, ref, types.TrieRefBlock(node.FirstChild), fs.sb.TotalBlocks)
		}
		if node.NextSibling > 0 && types.TrieRefBlock(node.NextSibling) >= fs.sb.TotalBlocks {
			fs.errorf("ino %d dir trie: ref %d: next_sibling block %d exceeds total blocks %d",
				parentIno, ref, types.TrieRefBlock(node.NextSibling), fs.sb.TotalBlocks)
		}

		// If already emitted, just push children.
		if emitted {
			goto pushChildren
		}

		// Extract leaf entry if this node has one.
		if trieIsLeaf(node.NodeType) {
			if node.Flags&uint16(types.NodeFlagDeleted) == 0 {
				name := extractTrieNodeName(buf, node)
				if name == "" {
					fs.errorf("ino %d dir trie: ref %d: empty or invalid name (name_len=%d, name_offset=%d)",
						parentIno, ref, node.NameLen, node.NameOffset)
				} else {
					entries = append(entries, trieEntry{
						Inode:  node.Inode,
						FType:  node.FType,
						Name:   name,
						Parent: parentIno,
					})
					fs.entryCounts[node.Inode]++
				}
			}

			if node.FirstChild != 0 {
				stack = append(stack, ref)
				leafEmitted = append(leafEmitted, true)
				continue
			}
		}

	pushChildren:
		if node.FirstChild != 0 {
			var siblings []uint64
			child := node.FirstChild
			for child != 0 {
				siblings = append(siblings, child)
				childBlock := types.TrieRefBlock(child)
				childSlot := types.TrieRefSlot(child)
				cbuf := make([]byte, blockSize)
				if _, err := fs.file.ReadAt(cbuf, int64(childBlock*blockSize)); err != nil {
					fs.errorf("ino %d dir trie: read child page %d: %v", parentIno, childBlock, err)
					break
				}
				if _, _, _, err := readTriePage(cbuf); err != nil {
					fs.errorf("ino %d dir trie: child ref %d: %v", parentIno, child, err)
					break
				}
				cn, err := parseTrieSlot(cbuf, childSlot)
				if err != nil {
					fs.errorf("ino %d dir trie: child ref %d: %v", parentIno, child, err)
					break
				}
				child = cn.NextSibling
			}
			for i := len(siblings) - 1; i >= 0; i-- {
				stack = append(stack, siblings[i])
				leafEmitted = append(leafEmitted, false)
			}
		}

	}

	return entries
}

// verifyAllDirTries walks the trie of every directory inode found during the inode table scan.
// It collects all entries and returns them for cross-referencing.
func verifyAllDirTries(fs *fsckState, blockSize uint64, dirs []dirInfo) []trieEntry {
	var allEntries []trieEntry

	for _, d := range dirs {
		entries := verifyDirectoryTrie(fs, d.ino, d.trieRoot, blockSize)
		allEntries = append(allEntries, entries...)
	}

	return allEntries
}

// dirInfo stores info about a directory inode for later trie walking.
type dirInfo struct {
	ino      uint64
	trieRoot uint64
}

// readJournalMagic reads the first 4 bytes of the given journal block and
// returns the magic value, or an error if the block cannot be read.
func readJournalMagic(file *os.File, block, blockSize uint64) (uint32, error) {
	buf := make([]byte, blockSize)
	if _, err := file.ReadAt(buf, int64(block*blockSize)); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(buf[0:]), nil
}

// verifyJournal checks the journal checkpoint block and detects dirty
// filesystems (un-replayed journal records). It first checks the last journal
// block (where the kernel and current mkfs write the checkpoint). If that is
// not a valid checkpoint, it falls back to the first journal block for
// compatibility with older mkfs.briefs images.
func verifyJournal(file *os.File, journalOffset, journalBlocks, checkpointSeq, logStart, logEnd uint64, blockSize uint64) error {
	checkpointBlock := journalOffset + journalBlocks - 1
	magic, err := readJournalMagic(file, checkpointBlock, blockSize)
	if err != nil {
		return fmt.Errorf("read journal checkpoint block %d: %w", checkpointBlock, err)
	}
	if magic != types.MagicJournal && magic != types.MagicCheckpoint {
		// Fallback: older mkfs.briefs wrote the initial checkpoint to the
		// first journal block with JOURNAL_MAGIC.
		fallbackMagic, fallbackErr := readJournalMagic(file, journalOffset, blockSize)
		if fallbackErr != nil {
			return fmt.Errorf("read fallback journal block %d: %w", journalOffset, fallbackErr)
		}
		if fallbackMagic != types.MagicJournal && fallbackMagic != types.MagicCheckpoint {
			return fmt.Errorf("bad journal magic at checkpoint block %d (0x%08X) and fallback block %d (0x%08X)",
				checkpointBlock, magic, journalOffset, fallbackMagic)
		}
	}

	// Check if the filesystem is dirty (has un-replayed journal records).
	// The journal uses a log-structured layout where logStart/logEnd track
	// the range of blocks in use. If logStart != logEnd or checkpointSeq
	// is low, there may be un-replayed entries.
	if logStart != logEnd {
		logRange := logEnd
		if logEnd >= logStart {
			logRange = logEnd - logStart
		} else {
			// Wrapped around
			logRange = logEnd + journalBlocks - logStart
		}
		return fmt.Errorf("filesystem has un-replayed journal records (log range %d blocks, checkpoint seq %d)\n      journal replay required before fsck",
			logRange, checkpointSeq)
	}

	return nil
}

// nextJournalBlock returns the next block in the circular journal.
func nextJournalBlock(block, journalOffset, journalBlocks uint64) uint64 {
	next := block + 1
	if next >= journalOffset+journalBlocks {
		return journalOffset
	}
	return next
}

// verifyJournalRecords verifies the CRC32C checksum of journal records.
// For a clean filesystem only the checkpoint block is checked. For a dirty
// filesystem the recorded log range is walked. A zero checksum is treated
// as a legacy record and is allowed with a single warning.
func verifyJournalRecords(fs *fsckState, journalOffset, journalBlocks, logStart, logEnd, blockSize uint64) {
	legacyWarned := false
	badRecords := 0
	recordsChecked := 0

	start := logStart
	end := logEnd
	fallbackClean := false
	if logStart == logEnd {
		// Clean journal: only the checkpoint block should contain records.
		start = journalOffset + journalBlocks - 1
		end = start
	}

	cur := start
	for {
		buf := make([]byte, blockSize)
		if _, err := fs.file.ReadAt(buf, int64(cur*blockSize)); err != nil {
			fs.errorf("journal block %d: read error: %v", cur, err)
			break
		}

		magic := binary.LittleEndian.Uint32(buf[0:])
		if magic != types.MagicJournal && magic != types.MagicCheckpoint {
			if logStart == logEnd && !fallbackClean && cur == journalOffset+journalBlocks-1 {
				// Old mkfs.briefs images have the initial checkpoint in the
				// first journal block. Try that as a fallback once.
				fallbackClean = true
				cur = journalOffset
				continue
			}
			fs.errorf("journal block %d: bad magic 0x%08X", cur, magic)
			break
		}

		recordCount := binary.LittleEndian.Uint32(buf[8:])
		recOff := uint64(16)
		for i := uint32(0); i < recordCount && recOff+16 <= blockSize; i++ {
			recType := binary.LittleEndian.Uint32(buf[recOff:])
			recFlags := binary.LittleEndian.Uint32(buf[recOff+4:])
			dataLen := binary.LittleEndian.Uint32(buf[recOff+8:])
			storedChecksum := binary.LittleEndian.Uint32(buf[recOff+12:])

			if recOff+16+uint64(dataLen) > blockSize {
				fs.errorf("journal block %d record %d: record overflows block (data_len=%d)",
					cur, i, dataLen)
				badRecords++
				break
			}

			recordsChecked++
			recData := buf[recOff+16 : recOff+16+uint64(dataLen)]
			if storedChecksum == 0 {
				if !legacyWarned {
					fs.warnf("journal: legacy record with no checksum at block %d record %d; skipping CRC verification",
						cur, i)
					legacyWarned = true
				}
			} else {
				computed := types.ComputeJournalRecordChecksum(recType, recFlags, recData)
				if computed != storedChecksum {
					fs.errorf("journal block %d record %d: checksum mismatch (stored=0x%08X computed=0x%08X)",
						cur, i, storedChecksum, computed)
					badRecords++
				}
			}

			// Parse and validate checkpoint payloads when the checksum is good.
			if recType == uint32(types.JRN_CHECKPOINT) {
				if dataLen != types.CheckpointSize {
					fs.warnf("journal block %d record %d: checkpoint payload has unexpected length %d (want %d)",
						cur, i, dataLen, types.CheckpointSize)
				} else {
					var cp types.Checkpoint
					if err := cp.UnmarshalBinary(recData); err != nil {
						fs.warnf("journal block %d record %d: checkpoint parse error: %v", cur, i, err)
					} else {
						fmt.Fprintf(os.Stderr, "  checkpoint:    seq=%d records=%d log_end=%d free_data=%d free_inodes=%d\n",
							cp.Seq, cp.RecordCount, cp.LogSequenceEnd, cp.FreeDataCount, cp.FreeInodeCount)

						if fs.sb != nil {
							if cp.Seq != fs.sb.CheckpointSeq {
								fs.warnf("checkpoint seq mismatch: payload=%d, superblock=%d",
									cp.Seq, fs.sb.CheckpointSeq)
							}
							if cp.LogSequenceEnd != fs.sb.JournalLogEnd {
								fs.warnf("checkpoint log_sequence_end mismatch: payload=%d, superblock=%d",
									cp.LogSequenceEnd, fs.sb.JournalLogEnd)
							}
							if cp.FreeDataCount > fs.sb.TotalBlocks {
								fs.warnf("checkpoint free_data_count out of range: %d > total_blocks %d",
									cp.FreeDataCount, fs.sb.TotalBlocks)
							}
							if cp.FreeInodeCount > fs.sb.TotalBlocks {
								fs.warnf("checkpoint free_inode_count out of range: %d > total_blocks %d",
									cp.FreeInodeCount, fs.sb.TotalBlocks)
							}
							if cp.FreeDataCount != fs.sb.FreeDataBlks {
								fs.warnf("checkpoint free_data_count differs from superblock: %d vs %d",
									cp.FreeDataCount, fs.sb.FreeDataBlks)
							}
							if cp.FreeInodeCount != fs.sb.FreeInodes {
								fs.warnf("checkpoint free_inode_count differs from superblock: %d vs %d",
									cp.FreeInodeCount, fs.sb.FreeInodes)
							}
						}
					}
				}
			}

			recOff += 16 + uint64(dataLen)
		}

		if logStart == logEnd {
			break
		}
		cur = nextJournalBlock(cur, journalOffset, journalBlocks)
		if cur == end {
			break
		}
	}

	if badRecords == 0 && recordsChecked > 0 {
		fmt.Fprintf(os.Stderr, "  journal records: %d checked, no checksum errors\n", recordsChecked)
	} else if badRecords > 0 {
		fmt.Fprintf(os.Stderr, "  journal records: %d checked, %d checksum error(s)\n", recordsChecked, badRecords)
	}
}

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
	// If any directory trie had structural errors, leaked blocks could be
	// legitimate blocks from unreadable subtrees, so we downgrade to WARNING.
	leaked := 0
	hasFailedTries := len(fs.failedTrieDirs) > 0
	for relBlk := range allocAllocated {
		absBlk := dataRegionStart + relBlk
		if !fs.usedBlocks[absBlk] {
			if leaked < 20 {
				if hasFailedTries {
					fs.warnf("block %d (data-relative %d): marked allocated but not found during trie walk (may be from failed trie traversal)",
						absBlk, relBlk)
				} else {
					fs.errorf("block %d (data-relative %d): marked allocated in bitmap but NOT referenced by any inode/trie",
						absBlk, relBlk)
				}
			} else if leaked == 20 {
				if hasFailedTries {
					fs.warnf("(more unverifiable block warnings suppressed)")
				} else {
					fs.errorf("(more leaked block errors suppressed)")
				}
			}
			leaked++
		}
	}
	if leaked > 0 {
		if hasFailedTries {
			fmt.Fprintf(os.Stderr, "  block cross-ref: %d block(s) allocated but not verified (trie errors may explain these)\n", leaked)
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
		switch in.Filemode & types.ModeTypeMask {
		case types.ModeDir:
			expectedFType = 4 // S_IFDIR >> 12
		case types.ModeFile:
			expectedFType = 8 // S_IFREG >> 12
		case types.ModeSymlink:
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

	addExtent := func(ino uint64, ext types.Extent) {
		// Skip hole extents — no physical backing
		if ext.Flags&types.ExtentFlagHole != 0 {
			return
		}
		if ext.Flags&^(uint32(types.ExtentFlagHole|types.ExtentFlagEof)) != 0 {
			fs.warnf("ino %d: extent with unknown flags 0x%08X (phys=%d, len=%d)",
				ino, ext.Flags, ext.Phys, ext.Len)
		}
		if ext.Len > 0 && ext.Phys > 0 {
			allExtents = append(allExtents, extentRef{ino: ino, phys: ext.Phys, len: ext.Len})
		}
	}

	for ino, in := range fs.inodes {
		if in.Flags&types.InodeFlagInlineData != 0 {
			continue
		}
		// Walk inline extents
		inlineExtents := in.InlineExtents()
		for ei := uint32(0); ei < in.NumExtentsInline; ei++ {
			addExtent(ino, inlineExtents[ei])
		}

		// Walk overflow chain extents
		if in.NumExtentsTotal > uint64(in.NumExtentsInline) && in.ExtentInlineBase != 0 {
			extentsPerBlock := types.ExtentsPerChainBlock(fs.sb.BlockSize)
			chainBlock := in.ExtentInlineBase
			for chainBlock != 0 {
				buf := make([]byte, fs.sb.BlockSize)
				if _, err := fs.file.ReadAt(buf, int64(chainBlock*fs.sb.BlockSize)); err != nil {
					break
				}
				if err := types.VerifyChainChecksum(buf, fs.sb.BlockSize); err != nil {
					fs.errorf("ino %d: extent chain block %d: checksum mismatch", ino, chainBlock)
					break
				}
				hdr := types.UnmarshalExtentChainHeader(buf)
				for i := uint32(0); i < hdr.NumExtentsInBlock && i < uint32(extentsPerBlock); i++ {
					ext := types.ReadChainExtent(buf, int(i))
					addExtent(ino, ext)
				}
				chainBlock = hdr.NextOverflowBlock
			}
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

// readAllocatorL2 reads the L2 bitmap words from an allocator pool.
func readAllocatorL2(file *os.File, poolBlock, blockSize uint64) (l2 []uint64, l2w uint64, blockCount uint64, err error) {
	_, _, l2, hdr, err := types.ReadAllocatorBitmap(file, poolBlock, blockSize)
	if err != nil {
		return nil, 0, 0, err
	}
	return l2, hdr.L2Words, hdr.BlockCount, nil
}

// verifyInodeBitmapCrossReference checks that every allocated inode bitmap slot
// corresponds to an inode with valid magic on disk, and every unallocated slot
// truly lacks valid magic.
func verifyInodeBitmapCrossReference(fs *fsckState, blockSize, inodeSize uint64) {
	inodeTableStart := fs.sb.InodeTableOffset
	inodesPerBlock := blockSize / inodeSize

	l2, _, blockCount, err := readAllocatorL2(fs.file, fs.sb.InodeBMOffset, blockSize)
	if err != nil {
		fs.errorf("inode bitmap cross-ref: %v", err)
		return
	}

	// Check each inode slot
	badAllocated := 0 // bitmap says allocated, but no valid inode magic
	badFree := 0      // bitmap says free, but has valid inode magic
	ino := uint64(1)

	for bi := uint64(0); bi < (blockCount+inodesPerBlock-1)/inodesPerBlock; bi++ {
		absBlock := inodeTableStart + bi
		buf := make([]byte, blockSize)
		if _, err := fs.file.ReadAt(buf, int64(absBlock*blockSize)); err != nil {
			fs.errorf("inode bitmap cross-ref: read inode table block %d: %v", absBlock, err)
			ino += inodesPerBlock
			continue
		}

		for j := uint64(0); j < inodesPerBlock && ino <= blockCount; j++ {
			offset := j * inodeSize
			magic := binary.LittleEndian.Uint64(buf[offset+8:])

			w := (ino - 1) / 64
			b := (ino - 1) % 64
			allocated := w < uint64(len(l2)) && (l2[w]&(1<<b)) == 0

			hasMagic := magic == types.MagicInode

			if allocated && !hasMagic {
				if badAllocated < 20 {
					fs.errorf("ino %d: bitmap says allocated but inode has no valid magic (0x%016X)", ino, magic)
				} else if badAllocated == 20 {
					fs.errorf("(more inode bitmap/table mismatch errors suppressed)")
				}
				badAllocated++
			}
			if !allocated && hasMagic {
				if badFree < 20 {
					fs.errorf("ino %d: bitmap says free but inode has valid magic (0x%016X)", ino, magic)
				} else if badFree == 20 {
					fs.errorf("(more inode bitmap/table mismatch errors suppressed)")
				}
				badFree++
			}
			ino++
		}
	}

	if badAllocated == 0 && badFree == 0 {
		fmt.Fprintf(os.Stderr, "  inode bitmap cross-ref: all bitmap entries match inode table\n")
	}
}

// verifySuperblockFreeCounts cross-checks the superblock free counts against
// the allocator headers and the actual inode/found counts.
func verifySuperblockFreeCounts(fs *fsckState, totalInodesFound int) {
	// Read data allocator free count
	_, _, _, _, dataFree, err := readAllocatorHeader(fs.file, fs.sb.TrieNodePoolStart, fs.sb.BlockSize)
	if err == nil {
		if dataFree != fs.sb.FreeDataBlks {
			fs.errorf("superblock free data blocks mismatch: superblock says %d, allocator says %d",
				fs.sb.FreeDataBlks, dataFree)
		}
	}

	// Read inode allocator free count
	_, _, _, _, inodeFree, err := readAllocatorHeader(fs.file, fs.sb.InodeBMOffset, fs.sb.BlockSize)
	if err == nil {
		if inodeFree != fs.sb.FreeInodes {
			fs.errorf("superblock free inodes mismatch: superblock says %d, allocator says %d",
				fs.sb.FreeInodes, inodeFree)
		}
	}

	// Cross-check: total inodes = (blockCount - inodeFree), should be totalInodesFound
	inodeHeader := make([]byte, fs.sb.BlockSize)
	if _, err := fs.file.ReadAt(inodeHeader, int64(fs.sb.InodeBMOffset*fs.sb.BlockSize)); err == nil {
		inodeBlockCount := binary.LittleEndian.Uint64(inodeHeader[32:])
		expectedInodes := int(inodeBlockCount - inodeFree)
		if expectedInodes != totalInodesFound {
			fs.errorf("inode count mismatch: bitmap says %d in-use, inode table scan found %d",
				expectedInodes, totalInodesFound)
		}
	}
}

func main() {
	app := &cli.App{
		Name:     "fsck.briefs",
		Usage:    "Check and repair a BrieFS filesystem",
		ArgsUsage: "DEVICE",
		Version:  versionStr,
		Before: func(c *cli.Context) error {
			if c.Args().Len() < 1 {
				return fmt.Errorf("missing required argument: DEVICE")
			}
			path := c.Args().First()
			if err := device.CheckMounted(path); err != nil {
				return fmt.Errorf("refusing to check filesystem: %w\n", err)
			}
			return nil
		},
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:    "verbose",
				Aliases: []string{"V"},
				Usage:   "verbose output",
			},
			&cli.BoolFlag{
				Name:  "repair",
				Usage: "attempt to repair found errors (not yet implemented)",
			},
		},
		Action: func(c *cli.Context) error {
			path := c.Args().First()
			repair := c.Bool("repair")

			var file *os.File
			var err error
			if repair {
				file, err = os.OpenFile(path, os.O_RDWR, 0)
				if err != nil {
					return fmt.Errorf("open device read-write for repair: %w", err)
				}
			} else {
				file, err = os.Open(path)
				if err != nil {
					return fmt.Errorf("open device: %w", err)
				}
			}
			defer file.Close()

			// Probe the device size using seeking (works for both regular
			// files and block devices; os.Stat().Size() returns 0 for
			// block devices).
			bd, err := device.GetDevice(path, 4096)
			if err != nil {
				return fmt.Errorf("probe device size: %w", err)
			}
			deviceSize := bd.Bytes()

			fs := &fsckState{
				file:   file,
				repair: repair,
			}

			fmt.Fprintf(os.Stderr, "BrieFS filesystem check, version %s\n", versionStr)
			fmt.Fprintf(os.Stderr, "Device: %s (%d bytes)\n", path, deviceSize)

			// 1. Superblock
			sb, err := verifySuperblock(file, 4096)
			if err != nil {
				return fmt.Errorf("superblock check FAILED: %w", err)
			}
			fs.sb = sb
			blockSize := sb.BlockSize
			fmt.Fprintf(os.Stderr, "\nSuperblock:\n")
			fmt.Fprintf(os.Stderr, "  magic:       0x%016X\n", sb.Magic)
			fmt.Fprintf(os.Stderr, "  version:     %d.%d.%d\n", sb.MajorVer, sb.MinorVer, sb.PatchVer)
			fmt.Fprintf(os.Stderr, "  total blocks: %d\n", sb.TotalBlocks)
			fmt.Fprintf(os.Stderr, "  block size:  %d\n", blockSize)
			fmt.Fprintf(os.Stderr, "  data blocks: %d\n", sb.DataBlocks)
			fmt.Fprintf(os.Stderr, "  free data:   %d\n", sb.FreeDataBlks)
			fmt.Fprintf(os.Stderr, "  free inodes: %d\n", sb.FreeInodes)
			fmt.Fprintf(os.Stderr, "  root inode:  %d\n", sb.RootIno)
			fmt.Fprintf(os.Stderr, "  label:       %s\n", string(sb.Label[:]))

			if deviceSize < int64(sb.TotalBlocks*blockSize) {
				return fmt.Errorf("device too small: %d bytes needed, got %d", sb.TotalBlocks*blockSize, deviceSize)
			}

			// Validate superblock field sanity
			if sb.BlockSize < 512 || sb.BlockSize > 65536 || (sb.BlockSize&(sb.BlockSize-1)) != 0 {
				fs.errorf("superblock: invalid block size %d (must be power of 2, 512-65536)", sb.BlockSize)
			}
			if sb.InodeSize < 128 || sb.InodeSize > 4096 || (sb.InodeSize&(sb.InodeSize-1)) != 0 {
				fs.errorf("superblock: invalid inode size %d (must be power of 2, 128-4096)", sb.InodeSize)
			}
			if sb.TotalBlocks == 0 {
				fs.errorf("superblock: zero total blocks")
			}
			if sb.DataBlocks > sb.TotalBlocks {
				fs.errorf("superblock: data blocks (%d) > total blocks (%d)", sb.DataBlocks, sb.TotalBlocks)
			}
			if sb.FreeDataBlks > sb.DataBlocks {
				fs.errorf("superblock: free data blocks (%d) > data blocks (%d)", sb.FreeDataBlks, sb.DataBlocks)
			}
			if sb.RootIno != 1 {
				fs.errorf("superblock: root inode is %d, expected 1", sb.RootIno)
			}
			if sb.InodeTableOffset == 0 {
				fs.errorf("superblock: inode table offset is 0")
			}
			if sb.JournalOffset == 0 || sb.JournalBlocks == 0 {
				fs.errorf("superblock: invalid journal offset %d / blocks %d", sb.JournalOffset, sb.JournalBlocks)
			}
			if sb.JournalOffset+sb.JournalBlocks > sb.TotalBlocks {
				fs.errorf("superblock: journal extends past end of device (offset %d + blocks %d > total %d)",
					sb.JournalOffset, sb.JournalBlocks, sb.TotalBlocks)
			}

			totalInodes := runVerificationPass(fs, blockSize, sb.InodeSize)

			// 7. Repair / optimization phase
			if repair {
				if fs.errors > 0 && len(fs.failedTrieDirs) > 0 {
					fmt.Fprintf(os.Stderr, "Refusing repair: %d director(ies) have unrecoverable trie errors\n", len(fs.failedTrieDirs))
					fmt.Fprintf(os.Stderr, "FSCK COMPLETE: %d error(s) found, repair skipped\n", fs.errors)
					return nil
				}
				if sb.JournalLogStart != sb.JournalLogEnd {
					fmt.Fprintf(os.Stderr, "Refusing repair: journal has un-replayed records\n")
					fmt.Fprintf(os.Stderr, "FSCK COMPLETE: %d error(s) found, repair skipped\n", fs.errors)
					return nil
				}
				if err := runRepair(fs, blockSize, totalInodes); err != nil {
					fmt.Fprintf(os.Stderr, "Repair failed: %v\n", err)
					fmt.Fprintf(os.Stderr, "FSCK COMPLETE: %d error(s) found, repair failed\n", fs.errors)
					return nil
				}
				fmt.Fprintf(os.Stderr, "\nRepair complete. Re-running verification pass...\n")
				fs.errors = 0
				fs.inodes = make(map[uint64]*types.Inode)
				fs.dirs = nil
				fs.usedBlocks = make(map[uint64]bool)
				fs.entryCounts = make(map[uint64]int)
				fs.failedTrieDirs = make(map[uint64]bool)
				runVerificationPass(fs, blockSize, sb.InodeSize)
			}

			// 8. Summary
			fmt.Fprintf(os.Stderr, "\n")
			if fs.errors > 0 {
				fmt.Fprintf(os.Stderr, "FSCK COMPLETE: %d error(s) found\n", fs.errors)
			} else {
				fmt.Fprintf(os.Stderr, "FSCK COMPLETE: no errors found\n")
			}

			return nil
		},
	}

	if err := app.Run(os.Args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// runVerificationPass runs the read-only checks and populates fsckState.
// It is used both for the initial pass and for the post-repair verification.
// It returns the number of inodes found, but callers typically only need the
// side effects on fs.
func runVerificationPass(fs *fsckState, blockSize, inodeSize uint64) int {
	file := fs.file
	sb := fs.sb

	// 1. Superblock fields have already been read and validated.

	// 2. Allocator pools
	fmt.Fprintf(os.Stderr, "\nInode bitmap:\n")
	if err := verifyAllocatorPool(file, sb.InodeBMOffset, blockSize, "inode bitmap"); err != nil {
		fs.errorf("%v", err)
	}

	fmt.Fprintf(os.Stderr, "\nData block allocator:\n")
	if err := verifyAllocatorPool(file, sb.TrieNodePoolStart, blockSize, "data allocator"); err != nil {
		fs.errorf("%v", err)
	}

	inodeL0w, inodeL1w, inodeL2w, inodeBlockCount, inodeAllocFree, err := readAllocatorHeader(file, sb.InodeBMOffset, blockSize)
	if err != nil {
		fs.errorf("read inode allocator header: %v", err)
	} else {
		if inodeAllocFree != sb.FreeInodes {
			fs.errorf("inode free count mismatch: superblock says %d, allocator says %d",
				sb.FreeInodes, inodeAllocFree)
		}
		verifyAllocatorBitmap(fs, sb.InodeBMOffset, blockSize, inodeL0w, inodeL1w, inodeL2w, inodeBlockCount, inodeAllocFree, "inode")
	}

	dataL0w, dataL1w, dataL2w, dataBlockCount, dataAllocFree, err := readAllocatorHeader(file, sb.TrieNodePoolStart, blockSize)
	if err != nil {
		fs.errorf("read data allocator header: %v", err)
	} else {
		if dataAllocFree != sb.FreeDataBlks {
			fs.errorf("data block free count mismatch: superblock says %d, allocator says %d",
				sb.FreeDataBlks, dataAllocFree)
		}
		verifyAllocatorBitmap(fs, sb.TrieNodePoolStart, blockSize, dataL0w, dataL1w, dataL2w, dataBlockCount, dataAllocFree, "data")
	}

	// 3. Inode table
	inodeTableStart := sb.InodeTableOffset
	var inodeTableBlocks uint64
	{
		inodeHeader := make([]byte, blockSize)
		if _, err := file.ReadAt(inodeHeader, int64(sb.InodeBMOffset*blockSize)); err != nil {
			fs.errorf("read inode allocator header: %v", err)
		} else {
			numInodes := binary.LittleEndian.Uint64(inodeHeader[32:])
			inodeTableBlocks = (numInodes*sb.InodeSize + blockSize - 1) / blockSize
		}
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

	// 6. Cross-referencing checks
	fmt.Fprintf(os.Stderr, "\nCross-referencing:\n")
	verifyInodeBitmapCrossReference(fs, blockSize, inodeSize)
	verifyBlockCrossReference(fs, blockSize)
	verifySuperblockFreeCounts(fs, totalInodes)
	verifyDirEntryCrossReference(fs, allEntries)
	verifyDuplicateNames(fs, allEntries)
	verifyLinkCounts(fs)
	verifyOrphanedInodes(fs)
	verifyExtentOverlaps(fs)
	verifyReachability(fs, allEntries)

	return totalInodes
}

// runRepair rebuilds allocator state from the verified metadata and writes the
// repaired allocator, superblock free counts, and a fresh checkpoint back to disk.
// This covers Phase 2 of the fsck repair roadmap:
//   - rebuild data/inode allocator bitmaps from the structures fsck found;
//   - fix superblock free count mismatches;
//   - free blocks that are allocated but not referenced (no failed trie walks).
func runRepair(fs *fsckState, blockSize uint64, totalInodes int) error {
	plan := &repairPlan{
		inodes: make(map[uint64]*types.Inode),
	}

	// 1. Rebuild data allocator from the structures fsck found.
	dataRegionStart := fs.sb.TrieNodePoolStart + fs.sb.TrieNodePoolSize
	if dataRegionStart > fs.sb.TotalBlocks {
		return fmt.Errorf("data region start %d exceeds total blocks %d", dataRegionStart, fs.sb.TotalBlocks)
	}
	dataBlockCount := fs.sb.TotalBlocks - dataRegionStart
	if fs.sb.JournalOffset > dataRegionStart {
		dataBlockCount = fs.sb.JournalOffset - dataRegionStart
	}
	plan.dataAlloc = types.NewAllocBuilder(dataBlockCount)

	// Mark every block referenced by extents or trie pages as allocated.
	for absBlk := range fs.usedBlocks {
		if absBlk >= dataRegionStart && absBlk < dataRegionStart+dataBlockCount {
			plan.dataAlloc.MarkAllocated(absBlk - dataRegionStart)
		}
	}

	// 2. Rebuild inode allocator from the inode table scan.
	inodeHeader, err := types.ReadAllocatorHeader(fs.file, fs.sb.InodeBMOffset, blockSize)
	if err != nil {
		return fmt.Errorf("read inode allocator header: %w", err)
	}
	plan.inodeAlloc = types.NewAllocBuilder(inodeHeader.BlockCount)
	for ino := range fs.inodes {
		if ino > 0 && ino <= inodeHeader.BlockCount {
			plan.inodeAlloc.MarkAllocated(ino - 1)
		}
	}

	// 3. Compact file extents.
	if err := compactFileExtents(fs, plan, blockSize); err != nil {
		return fmt.Errorf("compact file extents: %w", err)
	}

	// 4. Compact directory tries.
	if err := compactDirectoryTries(fs, plan, blockSize); err != nil {
		return fmt.Errorf("compact directory tries: %w", err)
	}

	// Directory inodes may have received a new trie root during compaction.
	// Keep the in-memory state in sync so the remaining repair steps walk the
	// current on-disk tries.
	for i := range fs.dirs {
		d := &fs.dirs[i]
		if updated, ok := plan.inodes[d.ino]; ok {
			d.trieRoot = updated.DirTrieRoot
			if orig, ok := fs.inodes[d.ino]; ok {
				orig.DirTrieRoot = updated.DirTrieRoot
			}
		}
	}

	// 5. Repair link counts.
	if err := repairLinkCounts(fs, plan, blockSize); err != nil {
		return fmt.Errorf("repair link counts: %w", err)
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

// writeModifiedInodes writes any inodes staged in the repair plan back to
// their fixed slots in the inode table.
func writeModifiedInodes(file *os.File, sb *types.SuperblockLayout, plan *repairPlan, blockSize uint64) error {
	inodesPerBlock := blockSize / sb.InodeSize
	for ino, in := range plan.inodes {
		blockOffset := sb.InodeTableOffset + (ino-1)/inodesPerBlock
		byteOffset := ((ino - 1) % inodesPerBlock) * sb.InodeSize
		off := int64(blockOffset*blockSize + byteOffset)
		if err := in.WriteAt(file, off); err != nil {
			return fmt.Errorf("write inode %d at offset %d: %w", ino, off, err)
		}
	}
	return nil
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

// compactTrieNode is an in-memory node used while rebuilding a directory trie.
type compactTrieNode struct {
	Depth     uint8
	ByteVal   uint8
	NodeType  uint8
	FType     uint8
	Inode     uint64
	Name      string
	NameOff   uint16
	Parent    *compactTrieNode
	Children  []*compactTrieNode
	Block     uint64
	Slot      uint
}

// compactTriePage is a freshly packed trie page under construction.
type compactTriePage struct {
	Block     uint64
	SlotsUsed uint
	NextSlot  uint
	NameOff   uint16
	Nodes     []*compactTrieNode
}

// compactDirectoryTries rebuilds every directory trie from its collected entries,
// packing nodes and names tightly into fresh pages and freeing any old pages that
// are no longer needed. This covers step 4 of the fsck repair roadmap.
func compactDirectoryTries(fs *fsckState, plan *repairPlan, blockSize uint64) error {
	dataRegionStart := fs.sb.TrieNodePoolStart + fs.sb.TrieNodePoolSize
	allocBlock := func() (uint64, error) {
		rel, err := plan.dataAlloc.AllocateBlock()
		if err != nil {
			return 0, err
		}
		return rel + dataRegionStart, nil
	}

	for _, d := range fs.dirs {
		entries, err := collectDirectoryEntries(fs, d.ino, d.trieRoot, blockSize)
		if err != nil {
			return fmt.Errorf("ino %d: collect directory entries: %w", d.ino, err)
		}

		oldBlocks, err := collectDirectoryTrieBlocks(fs, d.ino, d.trieRoot, blockSize)
		if err != nil {
			return fmt.Errorf("ino %d: collect old trie blocks: %w", d.ino, err)
		}

		root := buildCompactTrie(entries)
		pages, err := packCompactTrie(root, blockSize, allocBlock)
		if err != nil {
			return fmt.Errorf("ino %d: pack trie: %w", d.ino, err)
		}

		if err := writeCompactTriePages(fs.file, pages, blockSize); err != nil {
			return fmt.Errorf("ino %d: write compacted trie: %w", d.ino, err)
		}

		// Update directory inode with new trie root.
		dirIno, ok := plan.inodes[d.ino]
		if !ok {
			orig := fs.inodes[d.ino]
			if orig == nil {
				return fmt.Errorf("ino %d: directory inode missing from fsck state", d.ino)
			}
			clone := *orig
			dirIno = &clone
		}
		dirIno.DirTrieRoot = types.TrieMakeRef(root.Block, root.Slot)
		plan.inodes[d.ino] = dirIno

		// Free old trie blocks.
		for absBlk := range oldBlocks {
			if absBlk >= dataRegionStart && absBlk < fs.sb.JournalOffset {
				plan.dataAlloc.MarkFree(absBlk - dataRegionStart)
			}
		}
	}
	return nil
}

// collectDirectoryEntries walks a directory trie and returns all live entries.
// Unlike verifyDirectoryTrie, it does not emit fsck errors; it returns an error
// only on structural problems that prevent collection.
func collectDirectoryEntries(fs *fsckState, parentIno uint64, rootRef uint64, blockSize uint64) ([]trieEntry, error) {
	if rootRef == 0 {
		return nil, nil
	}
	visited := make(map[uint64]bool)
	var entries []trieEntry
	stack := []uint64{rootRef}
	leafEmitted := []bool{false}

	for len(stack) > 0 {
		ref := stack[len(stack)-1]
		emitted := leafEmitted[len(leafEmitted)-1]
		stack = stack[:len(stack)-1]
		leafEmitted = leafEmitted[:len(leafEmitted)-1]

		if visited[ref] && !emitted {
			return nil, fmt.Errorf("cycle detected at ref %d", ref)
		}
		if !emitted {
			visited[ref] = true
		}

		block := types.TrieRefBlock(ref)
		slot := types.TrieRefSlot(ref)

		buf := make([]byte, blockSize)
		if _, err := fs.file.ReadAt(buf, int64(block*blockSize)); err != nil {
			return nil, fmt.Errorf("read page %d: %w", block, err)
		}
		if _, _, _, err := readTriePage(buf); err != nil {
			return nil, fmt.Errorf("page %d: %w", block, err)
		}
		if slot >= trieSlotCount {
			return nil, fmt.Errorf("slot %d out of range", slot)
		}

		node, err := parseTrieSlot(buf, slot)
		if err != nil {
			return nil, err
		}

		if emitted {
			stack, leafEmitted = pushChildren(stack, leafEmitted, fs.file, node, blockSize)
			continue
		}

		if trieIsLeaf(node.NodeType) {
			if node.Flags&uint16(types.NodeFlagDeleted) == 0 {
				name := extractTrieNodeName(buf, node)
				if name != "" {
					entries = append(entries, trieEntry{
						Inode:  node.Inode,
						FType:  node.FType,
						Name:   name,
						Parent: parentIno,
					})
				}
			}
			if node.FirstChild != 0 {
				stack = append(stack, ref)
				leafEmitted = append(leafEmitted, true)
				continue
			}
		}

		stack, leafEmitted = pushChildren(stack, leafEmitted, fs.file, node, blockSize)
	}

	return entries, nil
}

// pushChildren pushes a node's children onto the walk stack in reverse order so
// that they are processed first-child-first. It returns the updated slices.
func pushChildren(stack []uint64, leafEmitted []bool, file *os.File, node trieSlot, blockSize uint64) ([]uint64, []bool) {
	if node.FirstChild == 0 {
		return stack, leafEmitted
	}
	var siblings []uint64
	child := node.FirstChild
	for child != 0 {
		siblings = append(siblings, child)
		cbuf := make([]byte, blockSize)
		if _, err := file.ReadAt(cbuf, int64(types.TrieRefBlock(child)*blockSize)); err != nil {
			break
		}
		cn, err := parseTrieSlot(cbuf, types.TrieRefSlot(child))
		if err != nil {
			break
		}
		child = cn.NextSibling
	}
	for i := len(siblings) - 1; i >= 0; i-- {
		stack = append(stack, siblings[i])
		leafEmitted = append(leafEmitted, false)
	}
	return stack, leafEmitted
}

// collectDirectoryTrieBlocks returns the set of absolute block numbers used by a
// directory trie. This is used to free old pages after compaction.
func collectDirectoryTrieBlocks(fs *fsckState, parentIno uint64, rootRef uint64, blockSize uint64) (map[uint64]bool, error) {
	blocks := make(map[uint64]bool)
	if rootRef == 0 {
		return blocks, nil
	}
	visited := make(map[uint64]bool)
	stack := []uint64{rootRef}

	for len(stack) > 0 {
		ref := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if visited[ref] {
			continue
		}
		visited[ref] = true

		block := types.TrieRefBlock(ref)
		slot := types.TrieRefSlot(ref)
		blocks[block] = true

		buf := make([]byte, blockSize)
		if _, err := fs.file.ReadAt(buf, int64(block*blockSize)); err != nil {
			return nil, fmt.Errorf("read page %d: %w", block, err)
		}
		if _, _, _, err := readTriePage(buf); err != nil {
			return nil, fmt.Errorf("page %d: %w", block, err)
		}

		node, err := parseTrieSlot(buf, slot)
		if err != nil {
			return nil, err
		}

		child := node.FirstChild
		for child != 0 {
			if !visited[child] {
				stack = append(stack, child)
			}
			cbuf := make([]byte, blockSize)
			if _, err := fs.file.ReadAt(cbuf, int64(types.TrieRefBlock(child)*blockSize)); err != nil {
				break
			}
			cn, err := parseTrieSlot(cbuf, types.TrieRefSlot(child))
			if err != nil {
				break
			}
			child = cn.NextSibling
		}
	}

	return blocks, nil
}

// buildCompactTrie builds a fresh prefix trie from a directory's entries.
func buildCompactTrie(entries []trieEntry) *compactTrieNode {
	root := &compactTrieNode{
		Depth:    0,
		NodeType: types.NodeTypeInterm,
	}
	for _, e := range entries {
		insertCompactTrieEntry(root, e.Name, e.Inode, e.FType)
	}
	return root
}

// insertCompactTrieEntry inserts one directory entry into the compact trie.
func insertCompactTrieEntry(root *compactTrieNode, name string, inode uint64, ftype uint8) {
	cur := root
	nameBytes := []byte(name)
	for i, b := range nameBytes {
		depth := uint8(i + 1)
		var child *compactTrieNode
		for _, c := range cur.Children {
			if c.ByteVal == b {
				child = c
				break
			}
		}
		if child == nil {
			child = &compactTrieNode{
				Depth:    depth,
				ByteVal:  b,
				NodeType: types.NodeTypeInterm,
				Parent:   cur,
			}
			cur.Children = append(cur.Children, child)
		}
		cur = child
	}
	cur.Name = name
	cur.Inode = inode
	cur.FType = ftype
	if cur.NodeType&types.NodeTypeInterm != 0 {
		cur.NodeType |= types.NodeStatusLeaf
	} else {
		cur.NodeType = types.NodeTypeInterm | types.NodeStatusLeaf
	}
}

// packCompactTrie assigns every trie node to a slot in a freshly allocated page,
// linking parents and children via absolute node references.
func packCompactTrie(root *compactTrieNode, blockSize uint64, allocBlock func() (uint64, error)) ([]*compactTriePage, error) {
	var pages []*compactTriePage

	assignNode := func(node *compactTrieNode) error {
		nameLen := uint16(len(node.Name))
		needed := nameLen + 2

		for _, p := range pages {
			if p.NextSlot >= trieSlotCount {
				continue
			}
			slotEnd := uint16(triePageHeaderSize + (p.NextSlot+1)*trieSlotSize)
			// The slot must not overlap with existing names at the page end.
			if slotEnd > uint16(blockSize)-p.NameOff {
				continue
			}
			// For leaf nodes, the name must also fit without overlapping slots.
			if nameLen > 0 && p.NameOff+needed > uint16(blockSize)-slotEnd {
				continue
			}
			if nameLen > 0 {
				node.NameOff = p.NameOff + needed
				p.NameOff += needed
			}
			node.Block = p.Block
			node.Slot = p.NextSlot
			p.NextSlot++
			p.SlotsUsed++
			p.Nodes = append(p.Nodes, node)
			return nil
		}

		// Need a new page.
		block, err := allocBlock()
		if err != nil {
			return fmt.Errorf("allocate trie page: %w", err)
		}
		p := &compactTriePage{
			Block:    block,
			NextSlot: 0,
		}
		slotEnd := uint16(triePageHeaderSize + trieSlotSize)
		// The slot must fit and, for leaves, the name must fit too.
		if slotEnd > uint16(blockSize)-p.NameOff {
			return fmt.Errorf("trie slot does not fit in a fresh page")
		}
		if nameLen > 0 {
			if p.NameOff+needed > uint16(blockSize)-slotEnd {
				return fmt.Errorf("name '%s' (%d bytes) too long for a trie page", node.Name, nameLen)
			}
			node.NameOff = needed
			p.NameOff = needed
		}
		node.Block = block
		node.Slot = 0
		p.NextSlot = 1
		p.SlotsUsed = 1
		p.Nodes = append(p.Nodes, node)
		pages = append(pages, p)
		return nil
	}

	var walk func(node *compactTrieNode) error
	walk = func(node *compactTrieNode) error {
		if err := assignNode(node); err != nil {
			return err
		}
		for _, child := range node.Children {
			if err := walk(child); err != nil {
				return err
			}
		}
		return nil
	}

	if err := walk(root); err != nil {
		return nil, err
	}

	// Sort children by byte value so sibling chains are deterministic.
	var sortChildren func(node *compactTrieNode)
	sortChildren = func(node *compactTrieNode) {
		sort.Slice(node.Children, func(i, j int) bool {
			return node.Children[i].ByteVal < node.Children[j].ByteVal
		})
		for _, c := range node.Children {
			sortChildren(c)
		}
	}
	sortChildren(root)

	return pages, nil
}

// writeCompactTriePages serializes freshly packed trie pages to disk.
func writeCompactTriePages(file *os.File, pages []*compactTriePage, blockSize uint64) error {
	for _, p := range pages {
		buf := make([]byte, blockSize)
		binary.LittleEndian.PutUint32(buf[0:], types.MagicTriePage)
		binary.LittleEndian.PutUint32(buf[4:], types.TriePageVersion)
		binary.LittleEndian.PutUint16(buf[8:], uint16(p.SlotsUsed))
		binary.LittleEndian.PutUint16(buf[10:], p.NameOff)
		freeSlots := ^uint64(0)
		for i := uint(0); i < p.SlotsUsed; i++ {
			freeSlots &^= 1 << i
		}
		binary.LittleEndian.PutUint64(buf[12:], freeSlots)

		for _, node := range p.Nodes {
			off := uint64(triePageHeaderSize + node.Slot*trieSlotSize)

			var firstChild uint64
			if len(node.Children) > 0 {
				c := node.Children[0]
				firstChild = types.TrieMakeRef(c.Block, c.Slot)
			}

			var nextSibling uint64
			if node.Parent != nil {
				for i, sibling := range node.Parent.Children {
					if sibling == node && i+1 < len(node.Parent.Children) {
						next := node.Parent.Children[i+1]
						nextSibling = types.TrieMakeRef(next.Block, next.Slot)
						break
					}
				}
			}

			binary.LittleEndian.PutUint64(buf[off+0:], firstChild)
			binary.LittleEndian.PutUint64(buf[off+8:], nextSibling)
			binary.LittleEndian.PutUint64(buf[off+16:], node.Inode)
			binary.LittleEndian.PutUint16(buf[off+24:], uint16(len(node.Name)))
			binary.LittleEndian.PutUint16(buf[off+26:], node.NameOff)
			buf[off+28] = node.Depth
			buf[off+29] = node.NodeType
			buf[off+30] = node.ByteVal
			buf[off+31] = node.FType
			binary.LittleEndian.PutUint16(buf[off+32:], 0) // flags
			binary.LittleEndian.PutUint16(buf[off+34:], uint16(len(node.Children)))
		}

		for _, node := range p.Nodes {
			if node.NameOff == 0 {
				continue
			}
			nameStart := uint64(blockSize) - uint64(node.NameOff)
			binary.LittleEndian.PutUint16(buf[nameStart:], uint16(len(node.Name)))
			copy(buf[nameStart+2:], node.Name)
		}

		if _, err := file.WriteAt(buf, int64(p.Block*blockSize)); err != nil {
			return fmt.Errorf("write trie page %d: %w", p.Block, err)
		}
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
