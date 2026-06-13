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

	return in, nil
}

func verifyInodeTable(fs *fsckState, inodeTableBlock, inodeTableBlocks, blockSize, inodeSize uint64) (totalInodes int) {
	inodesPerBlock := blockSize / inodeSize
	ino := uint64(1)

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
			} else if in != nil {
				// Basic sanity: directory must have a trie root
				if in.IsDir() && in.DirTrieRoot == 0 {
					fs.errorf("ino %d: directory with no trie root", ino)
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

// trieNode is the on-disk format of a BrieFS directory trie node (32 bytes header).
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
			continue
		}

		// Validate node type
		if node.NodeType == 0 {
			fs.errorf("ino %d dir trie: block %d: zero node type", parentIno, block)
			continue
		}
		if node.NodeType&types.NodeTypeInterm != 0 && node.NodeType != types.NodeTypeInterm &&
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
	buf := make([]byte, blockSize)
	if _, err := file.ReadAt(buf, int64(journalOffset*blockSize)); err != nil {
		return fmt.Errorf("read journal block %d: %w", journalOffset, err)
	}
	magic := binary.LittleEndian.Uint32(buf[0:])
	if magic != types.MagicJournal {
		return fmt.Errorf("bad journal magic at block %d: 0x%08X", journalOffset, magic)
	}
	return nil
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

			fi, err := file.Stat()
			if err != nil {
				return fmt.Errorf("stat device: %w", err)
			}
			deviceSize := fi.Size()

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
			if err := verifyJournal(file, sb.JournalOffset, sb.JournalBlocks, blockSize); err != nil {
				fs.warnf("journal check: %v", err)
			} else {
				fmt.Fprintf(os.Stderr, "  journal magic OK\n")
			}

			// 5. Summary
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
