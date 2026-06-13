// fsck.briefs validates and repairs a BrieFS filesystem.
package main

import (
	"encoding/binary"
	"fmt"
	"os"

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
}

func (fs *fsckState) errorf(format string, args ...interface{}) {
	fs.errors++
	fmt.Fprintf(os.Stderr, "  ERROR: "+format+"\n", args...)
}

func (fs *fsckState) warnf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "  WARNING: "+format+"\n", args...)
}

func verifyAllocatorPool(file *os.File, poolBlock, blockSize uint64, label string) error {
	buf := make([]byte, blockSize)
	if _, err := file.ReadAt(buf, int64(poolBlock*blockSize)); err != nil {
		return fmt.Errorf("%s: read header block at %d: %w", label, poolBlock, err)
	}

	magic := binary.LittleEndian.Uint32(buf[0:])
	if magic != types.AllocMagic {
		return fmt.Errorf("%s: bad magic at block %d: expected 0x%08X, got 0x%08X", label, poolBlock, types.AllocMagic, magic)
	}

	ver := binary.LittleEndian.Uint32(buf[4:])
	if ver != 1 {
		return fmt.Errorf("%s: unsupported version %d at block %d", label, ver, poolBlock)
	}

	l0w := binary.LittleEndian.Uint64(buf[8:])
	l1w := binary.LittleEndian.Uint64(buf[16:])
	l2w := binary.LittleEndian.Uint64(buf[24:])
	blockCount := binary.LittleEndian.Uint64(buf[32:])
	freeCount := binary.LittleEndian.Uint64(buf[40:])

	fmt.Fprintf(os.Stderr, "  %s: pool at block %d, %d entries, %d free\n", label, poolBlock, blockCount, freeCount)
	fmt.Fprintf(os.Stderr, "    levels: L0=%d words, L1=%d words, L2=%d words\n", l0w, l1w, l2w)

	return nil
}

// readAllocatorHeader reads the allocator pool header and returns all fields.
func readAllocatorHeader(file *os.File, poolBlock, blockSize uint64) (l0w, l1w, l2w, blockCount, freeCount uint64, err error) {
	buf := make([]byte, blockSize)
	if _, err := file.ReadAt(buf, int64(poolBlock*blockSize)); err != nil {
		return 0, 0, 0, 0, 0, fmt.Errorf("read allocator header at %d: %w", poolBlock, err)
	}
	l0w = binary.LittleEndian.Uint64(buf[8:])
	l1w = binary.LittleEndian.Uint64(buf[16:])
	l2w = binary.LittleEndian.Uint64(buf[24:])
	blockCount = binary.LittleEndian.Uint64(buf[32:])
	freeCount = binary.LittleEndian.Uint64(buf[40:])
	return
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
	buf := make([]byte, blockSize)
	if _, err := file.ReadAt(buf, 0); err != nil {
		return nil, fmt.Errorf("read superblock: %w", err)
	}

	sb := &types.SuperblockLayout{}
	magic := binary.LittleEndian.Uint64(buf[0:])
	if magic != types.MagicSuperblock {
		return nil, fmt.Errorf("bad superblock magic: 0x%016X (expected 0x%016X)", magic, types.MagicSuperblock)
	}

	sb.Magic = magic
	sb.MajorVer = binary.LittleEndian.Uint64(buf[8:])
	sb.MinorVer = binary.LittleEndian.Uint64(buf[16:])
	sb.PatchVer = binary.LittleEndian.Uint64(buf[24:])
	sb.TotalBlocks = binary.LittleEndian.Uint64(buf[32:])
	sb.DataBlocks = binary.LittleEndian.Uint64(buf[40:])
	sb.BlockSize = binary.LittleEndian.Uint64(buf[48:])
	sb.InodeSize = binary.LittleEndian.Uint64(buf[56:])
	sb.BlocksGrp = binary.LittleEndian.Uint64(buf[64:])
	sb.InodesGrp = binary.LittleEndian.Uint64(buf[72:])
	sb.FSCreated = binary.LittleEndian.Uint64(buf[80:])
	sb.FSLastMount = binary.LittleEndian.Uint64(buf[88:])
	sb.FSLastChkpt = binary.LittleEndian.Uint64(buf[96:])
	sb.FreeDataBlks = binary.LittleEndian.Uint64(buf[104:])
	sb.FreeInodes = binary.LittleEndian.Uint64(buf[112:])
	sb.RootIno = binary.LittleEndian.Uint64(buf[120:])
	sb.FeatCompat = binary.LittleEndian.Uint64(buf[128:])
	sb.FeatROCompat = binary.LittleEndian.Uint64(buf[136:])
	sb.FeatIncompat = binary.LittleEndian.Uint64(buf[144:])
	copy(sb.UUID[:], buf[152:168])
	sb.EATOffset = binary.LittleEndian.Uint64(buf[168:])
	sb.EATBlocks = binary.LittleEndian.Uint64(buf[176:])
	sb.TrieRootBlock = binary.LittleEndian.Uint64(buf[184:])
	sb.TrieBlocksUsed = binary.LittleEndian.Uint64(buf[192:])
	sb.TrieNodePoolStart = binary.LittleEndian.Uint64(buf[200:])
	sb.TrieNodePoolSize = binary.LittleEndian.Uint64(buf[208:])
	sb.InodeBMOffset = binary.LittleEndian.Uint64(buf[216:])
	sb.InodeBMBlocks = binary.LittleEndian.Uint64(buf[224:])
	sb.InodeTableOffset = binary.LittleEndian.Uint64(buf[232:])
	sb.JournalOffset = binary.LittleEndian.Uint64(buf[240:])
	sb.JournalBlocks = binary.LittleEndian.Uint64(buf[248:])
	sb.CheckpointSeq = binary.LittleEndian.Uint64(buf[256:])
	sb.JournalLogStart = binary.LittleEndian.Uint64(buf[264:])
	sb.JournalLogEnd = binary.LittleEndian.Uint64(buf[272:])
	for i := 0; i < 4; i++ {
		sb.ReservedJournal[i] = binary.LittleEndian.Uint64(buf[280+i*8:])
	}
	copy(sb.Label[:], buf[312:312+64])

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

	// Validate extent counts
	if in.NumExtentsInline > 8 {
		return nil, fmt.Errorf("ino %d: too many inline extents %d", ino, in.NumExtentsInline)
	}
	if in.NumExtentsTotal < uint64(in.NumExtentsInline) {
		return nil, fmt.Errorf("ino %d: total extents %d < inline extents %d", ino, in.NumExtentsTotal, in.NumExtentsInline)
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
						fs.usedBlocks[in.DirTrieRoot] = true
					}
				}

				// File with zero size but extents
				if in.IsFile() && in.FileSize == 0 && in.NumExtentsTotal > 0 {
					fs.warnf("ino %d: file with zero size but %d extents", ino, in.NumExtentsTotal)
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
	// Helper to record the blocks from a single extent.
	addExtentBlocks := func(ext types.Extent) {
		if ext.Len > 0 && ext.Phys > 0 {
			for bk := uint64(0); bk < ext.Len; bk++ {
				fs.usedBlocks[ext.Phys+bk] = true
			}
		}
	}

	// Collect inline extents
	for ei := uint32(0); ei < in.NumExtentsInline; ei++ {
		ext := in.InlineExtents[ei]
		addExtentBlocks(ext)
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
// Each node occupies one 4096-byte block. Names are stored in the trailing bytes.
type trieNode struct {
	Magic       uint32 // "TRN " - 0x54524E20
	ChildCount  uint32
	FirstChild  uint64 // block number of first child
	NextSibling uint64 // block number of next sibling at same depth
	Depth       uint8
	NodeType    uint8 // NODE_TYPE_* | NODE_STATUS_LEAF
	ByteVal     uint8
	FType       uint8 // file type (S_IFMT >> 12)
	Reserved    [4]byte
	Flags       uint64 // NODE_FLAG_*

	// Leaf entry data (valid when TRIE_IS_LEAF is true)
	Inode       uint64
	NameLen     uint16 // full name length (including 2-byte prefix)
	NameOffset  uint16 // offset from block end to name bytes
}

const trieNodeHeaderSize = 32

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

// verifyDirectoryTrie walks a directory's trie, validating structure and collecting entries.
// Returns the list of entries found, or nil if the trie is empty.
// Also records trie node blocks in fs.usedBlocks and counts entries per inode.
func verifyDirectoryTrie(fs *fsckState, parentIno uint64, rootBlock uint64, blockSize uint64) []trieEntry {
	if rootBlock == 0 {
		return nil
	}

	// Track visited blocks to detect cycles
	visited := make(map[uint64]bool)
	var entries []trieEntry

	// Iterative depth-first walk using a stack of block numbers
	stack := []uint64{rootBlock}
	// Parallel stack: whether we've already emitted this node's leaf
	leafEmitted := []bool{false}

	for len(stack) > 0 {
		block := stack[len(stack)-1]
		emitted := leafEmitted[len(leafEmitted)-1]
		stack = stack[:len(stack)-1]
		leafEmitted = leafEmitted[:len(leafEmitted)-1]

		if visited[block] {
			fs.errorf("ino %d dir trie: cycle detected at block %d", parentIno, block)
			continue
		}
		visited[block] = true

		// Record this trie node block as used
		fs.usedBlocks[block] = true

		buf := make([]byte, blockSize)
		if _, err := fs.file.ReadAt(buf, int64(block*blockSize)); err != nil {
			fs.errorf("ino %d dir trie: read block %d: %v", parentIno, block, err)
			continue
		}

		node := parseTrieNode(buf)

		// Validate magic
		if node.Magic != types.MagicTrieNode {
			fs.errorf("ino %d dir trie: block %d: bad magic 0x%08X (expected 0x%08X)",
				parentIno, block, node.Magic, types.MagicTrieNode)
			fs.failedTrieDirs[parentIno] = true
			continue
		}

		// Validate node type
		// node_type == 0 is valid for pure leaf nodes (the file type is in f_type).
		// node_type can be NODE_TYPE_INTERM (0x04) for internal nodes,
		// or NODE_TYPE_INTERM | NODE_STATUS_LEAF (0x0C) for internal nodes
		// that also store a leaf entry.
		if node.NodeType != 0 && node.NodeType != types.NodeTypeInterm &&
			node.NodeType != (types.NodeTypeInterm|types.NodeStatusLeaf) {
			fs.errorf("ino %d dir trie: block %d: invalid node type 0x%02X", parentIno, block, node.NodeType)
		}

		// Validate depth
		if block == rootBlock && node.Depth != 0 {
			fs.errorf("ino %d dir trie: root block %d: depth is %d, expected 0", parentIno, block, node.Depth)
		}

		// Validate root block byte_val
		if block == rootBlock && node.ByteVal != 0 {
			fs.errorf("ino %d dir trie: root block %d: byte_val is %d, expected 0", parentIno, block, node.ByteVal)
		}

		// Validate child_count vs first_child
		if node.ChildCount == 0 && node.FirstChild != 0 {
			fs.errorf("ino %d dir trie: block %d: child_count=0 but first_child=%d", parentIno, block, node.FirstChild)
		}
		if node.ChildCount > 0 && node.FirstChild == 0 {
			fs.errorf("ino %d dir trie: block %d: child_count=%d but first_child=0", parentIno, block, node.ChildCount)
		}

		// Validate block number ranges
		if node.FirstChild > 0 && node.FirstChild >= fs.sb.TotalBlocks {
			fs.errorf("ino %d dir trie: block %d: first_child %d exceeds total blocks %d",
				parentIno, block, node.FirstChild, fs.sb.TotalBlocks)
		}
		if node.NextSibling > 0 && node.NextSibling >= fs.sb.TotalBlocks {
			fs.errorf("ino %d dir trie: block %d: next_sibling %d exceeds total blocks %d",
				parentIno, block, node.NextSibling, fs.sb.TotalBlocks)
		}

		// If we've already emitted this node's leaf (re-visit for children), skip leaf
		if emitted {
			goto pushChildren
		}

		// Extract leaf entry if this node has one
		if trieIsLeaf(node.NodeType) {
			name := extractTrieNodeName(buf, node)
			if name == "" {
				fs.errorf("ino %d dir trie: block %d: empty or invalid name (name_len=%d, name_offset=%d)",
					parentIno, block, node.NameLen, node.NameOffset)
			} else {
				entries = append(entries, trieEntry{
					Inode:  node.Inode,
					FType:  node.FType,
					Name:   name,
					Parent: parentIno,
				})
				// Count this entry for link count cross-referencing
				fs.entryCounts[node.Inode]++
			}

			// If this node also has children, push it back with emitted=true
			if node.FirstChild != 0 {
				stack = append(stack, block)
				leafEmitted = append(leafEmitted, true)
			}
		}

	pushChildren:
		// Push children (siblings in reverse order so they're processed in order)
		if node.FirstChild != 0 {
			// Collect all siblings first
			var siblings []uint64
			child := node.FirstChild
			for child != 0 {
				siblings = append(siblings, child)
				cbuf := make([]byte, blockSize)
				if _, err := fs.file.ReadAt(cbuf, int64(child*blockSize)); err != nil {
					fs.errorf("ino %d dir trie: read sibling block %d: %v", parentIno, child, err)
					break
				}
				cn := parseTrieNode(cbuf)
				child = cn.NextSibling
			}
			// Push in reverse order so first sibling is processed first
			for i := len(siblings) - 1; i >= 0; i-- {
				stack = append(stack, siblings[i])
				leafEmitted = append(leafEmitted, false)
			}
		}
	}

	return entries
}

// parseTrieNode reads a trie node from a block buffer.
func parseTrieNode(buf []byte) trieNode {
	return trieNode{
		Magic:       binary.LittleEndian.Uint32(buf[0:]),
		ChildCount:  binary.LittleEndian.Uint32(buf[4:]),
		FirstChild:  binary.LittleEndian.Uint64(buf[8:]),
		NextSibling: binary.LittleEndian.Uint64(buf[16:]),
		Depth:       buf[24],
		NodeType:    buf[25],
		ByteVal:     buf[26],
		FType:       buf[27],
		// reserved[4] at buf[28:32]
		Flags:      binary.LittleEndian.Uint64(buf[32:]),
		Inode:      binary.LittleEndian.Uint64(buf[40:]),
		NameLen:    binary.LittleEndian.Uint16(buf[48:]),
		NameOffset: binary.LittleEndian.Uint16(buf[50:]),
	}
}

// extractTrieNodeName reads the name from the trailing bytes of a trie node block.
// Names are stored with a 2-byte length prefix, starting at block_size - name_offset.
func extractTrieNodeName(buf []byte, node trieNode) string {
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
	// First 2 bytes at nameStart are the length prefix
	storedLen := int(binary.LittleEndian.Uint16(buf[nameStart:]))
	if storedLen < 1 || storedLen > types.BrieFSMaxNameLen {
		return ""
	}
	if nameStart+2+storedLen > len(buf) {
		return ""
	}
	return string(buf[nameStart+2 : nameStart+2+storedLen])
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

// verifyJournal checks the journal header block and detects dirty filesystems
// (un-replayed journal records).
func verifyJournal(file *os.File, journalOffset, journalBlocks, checkpointSeq, logStart, logEnd uint64, blockSize uint64) error {
	buf := make([]byte, blockSize)
	if _, err := file.ReadAt(buf, int64(journalOffset*blockSize)); err != nil {
		return fmt.Errorf("read journal block %d: %w", journalOffset, err)
	}
	magic := binary.LittleEndian.Uint32(buf[0:])
	if magic != types.MagicJournal {
		return fmt.Errorf("bad journal magic at block %d: 0x%08X", journalOffset, magic)
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

		// Check file type matches
		// ftype is stored as (S_IFMT >> 12): 4 for directories, 8 for regular files.
		var expectedFType uint8
		if in.IsDir() {
			expectedFType = 4 // S_IFDIR >> 12
		} else if in.IsFile() {
			expectedFType = 8 // S_IFREG >> 12
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

	for ino, in := range fs.inodes {
		// Walk inline extents
		for ei := uint32(0); ei < in.NumExtentsInline; ei++ {
			ext := in.InlineExtents[ei]
			if ext.Len > 0 && ext.Phys > 0 {
				allExtents = append(allExtents, extentRef{ino: ino, phys: ext.Phys, len: ext.Len})
			}
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
				hdr := types.UnmarshalExtentChainHeader(buf)
				for i := uint32(0); i < hdr.NumExtentsInBlock && i < uint32(extentsPerBlock); i++ {
					ext := types.ReadChainExtent(buf, int(i))
					if ext.Len > 0 && ext.Phys > 0 {
						allExtents = append(allExtents, extentRef{ino: ino, phys: ext.Phys, len: ext.Len})
					}
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
	buf := make([]byte, blockSize)
	if _, err := file.ReadAt(buf, int64(poolBlock*blockSize)); err != nil {
		return nil, 0, 0, fmt.Errorf("read allocator header at %d: %w", poolBlock, err)
	}
	l0w := binary.LittleEndian.Uint64(buf[8:])
	l1w := binary.LittleEndian.Uint64(buf[16:])
	l2w = binary.LittleEndian.Uint64(buf[24:])
	blockCount = binary.LittleEndian.Uint64(buf[32:])

	l0Blocks := (l0w + 511) / 512
	l1Blocks := (l1w + 511) / 512
	l2Start := poolBlock + 1 + l0Blocks + l1Blocks
	l2Blocks := (l2w + 511) / 512

	l2 = make([]uint64, l2w)
	for bi := uint64(0); bi < l2Blocks; bi++ {
		b := make([]byte, blockSize)
		if _, err := file.ReadAt(b, int64((l2Start+bi)*blockSize)); err != nil {
			return nil, 0, 0, fmt.Errorf("read L2 block %d: %w", l2Start+bi, err)
		}
		start := bi * 512
		for j := uint64(0); j < 512 && start+j < l2w; j++ {
			l2[start+j] = binary.LittleEndian.Uint64(b[j*8:])
		}
	}
	return
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
		Name:    "fsck.briefs",
		Usage:   "Check and repair a BrieFS filesystem",
		Version: versionStr,
		Before: func(c *cli.Context) error {
			path := c.String("device")
			if err := device.CheckMounted(path); err != nil {
				return fmt.Errorf("refusing to check filesystem: %w\n", err)
			}
			return nil
		},
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:     "device",
				Aliases:  []string{"d"},
				Required: true,
				Usage:    "filesystem device or image file",
			},
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
			path := c.String("device")

			file, err := os.Open(path)
			if err != nil {
				return fmt.Errorf("open device: %w", err)
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
				file: file,
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

			// 2. Allocator pools
			fmt.Fprintf(os.Stderr, "\nInode bitmap:\n")
			if err := verifyAllocatorPool(file, sb.InodeBMOffset, blockSize, "inode bitmap"); err != nil {
				fs.errorf("%v", err)
			}

			fmt.Fprintf(os.Stderr, "\nData block allocator:\n")
			if err := verifyAllocatorPool(file, sb.TrieNodePoolStart, blockSize, "data allocator"); err != nil {
				fs.errorf("%v", err)
			}

			// Cross-check allocator free counts against superblock and scan bitmap pyramid
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
				// Read inode allocator header to get actual inode count
				inodeHeader := make([]byte, blockSize)
				if _, err := file.ReadAt(inodeHeader, int64(sb.InodeBMOffset*blockSize)); err != nil {
					return fmt.Errorf("read inode allocator header: %w", err)
				}
				numInodes := binary.LittleEndian.Uint64(inodeHeader[32:])
				inodeTableBlocks = (numInodes*sb.InodeSize + blockSize - 1) / blockSize
			}
			fmt.Fprintf(os.Stderr, "\nInode table:\n")
			fmt.Fprintf(os.Stderr, "  start block: %d\n", inodeTableStart)
			fmt.Fprintf(os.Stderr, "  blocks:      %d\n", inodeTableBlocks)

			totalInodes := verifyInodeTable(fs, inodeTableStart, inodeTableBlocks, blockSize, sb.InodeSize)
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
			verifyInodeBitmapCrossReference(fs, blockSize, sb.InodeSize)
			verifyBlockCrossReference(fs, blockSize)
			verifySuperblockFreeCounts(fs, totalInodes)
			verifyDirEntryCrossReference(fs, allEntries)
			verifyDuplicateNames(fs, allEntries)
			verifyLinkCounts(fs)
			verifyOrphanedInodes(fs)
			verifyExtentOverlaps(fs)
			verifyReachability(fs, allEntries)

			// 7. Summary
			fmt.Fprintf(os.Stderr, "\n")
			if fs.errors > 0 {
				fmt.Fprintf(os.Stderr, "FSCK COMPLETE: %d error(s) found\n", fs.errors)
				if c.Bool("repair") {
					fmt.Fprintf(os.Stderr, "Repair not yet implemented\n")
				}
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
