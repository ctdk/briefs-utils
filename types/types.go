// Package types defines the BrieFS on-disk format.
package types

import (
	"encoding/binary"
	"fmt"
	"github.com/google/uuid"
	"os"
	"time"
	"unsafe"
)

func init() {
	var s SuperblockLayout
	fmt.Printf("DEBUG: SuperblockLayout size = %d\n", unsafe.Sizeof(s))
	fmt.Printf("DEBUG: UUID offset = %d\n", unsafe.Offsetof(s.UUID))
	fmt.Printf("DEBUG: EATOffset offset = %d\n", unsafe.Offsetof(s.EATOffset))
	fmt.Printf("DEBUG: EATBlocks offset = %d\n", unsafe.Offsetof(s.EATBlocks))
	fmt.Printf("DEBUG: InodeBMOffset offset = %d\n", unsafe.Offsetof(s.InodeBMOffset))
	fmt.Printf("DEBUG: DataBitmapOffset offset = %d\n", unsafe.Offsetof(s.DataBitmapOffset))
}

// Magic numbers for our filesystem structures.
const (
	MagicSuperblock = 0x504C434E        // "PLCN"
	MagicInode      = 0x494E4F44        // "INOD"
	MagicTrieNode   = 0x54524945        // "TRIE"
	MagicDirEntry   = 0x44495245        // "DIRE"
	MagicJournal    = 0x4A4E4C5A        // "JNLZ"
	MagicCheckpoint = 0x43485053        // "CHPS"

	// File mode constants.
	ModeDir   = 0040000
	ModeFile  = 0100000
	ModeSymlink = 0120000

	// Default values.
	DefaultBlockSize   = 4096
	DefaultInodeSize   = 512
	DefaultJournalSize = 64 // blocks

	// Inode magic
	InodeMagic = 0x494E4F44 // "INOD"

	// Inode flags
	InodeFlagReserved    = 0x00000001
	InodeFlagCompressed  = 0x00000002
	InodeFlagIndexed     = 0x00000004

	// Extent flags
	ExtentFlagHole = 0x00000001
	ExtentFlagEof  = 0x80000000

	// Superblock reserved area padding. Matches _BRIEFS_SUPER_RESERVED in
	// briefs.h in the kernel module.
	BrieFsSuperReserved = 640

	// BrieFS version numbers for this version of briefs-utils
	BrieFSMajorVersion = 0
	BrieFSMinorVersion = 1
	BrieFSPatchVersion = 0
)

// SuperblockLayout is the on-disk format (first 4KB block).
type SuperblockLayout struct {
	// Block 0: core fields
	Magic       uint64
	MajorVer    uint64  // Changed from uint32 to match C struct (8 bytes)
	MinorVer    uint64  // Changed from uint32 to match C struct (8 bytes)
	PatchVer    uint64  // Changed from uint32 to match C struct (8 bytes)
	TotalBlocks uint64
	DataBlocks  uint64
	BlockSize   uint64
	InodeSize   uint64
	BlocksGrp   uint64
	InodesGrp   uint64
	FSCreated   uint64
	FSLastMount uint64
	FSLastChkpt uint64
	FreeDataBlks uint64
	FreeInodes  uint64
	RootIno     uint64
	FeatCompat  uint64
	FeatROCompat uint64
	FeatIncompat uint64
	UUID        [16]byte
	// C struct: __u8 uuid[16] ends at byte 168. __u64 eat_offset follows
	// at byte 168 (8-byte aligned, no padding needed). The comments in
	// the C header (/* 176 */) are incorrect.
	// No _Padding field needed.

	// Block 0: metadata pointers
	EATOffset   uint64  // offset 168
	EATBlocks      uint64  // offset 184
	TrieRootBlock  uint64  // offset 192
	TrieBlocksUsed uint64  // offset 200
	TrieNodePoolStart uint64  // offset 208
	TrieNodePoolSize  uint64  // offset 216
	InodeBMOffset  uint64  // offset 224
	InodeBMBlocks  uint64  // offset 232
	DataBitmapOffset uint64  // offset 240
	DataBitmapBlocks uint64  // offset 248
	JournalOffset  uint64  // offset 256
	JournalBlocks  uint64  // offset 264
	CheckpointSeq  uint64
	JournalLogStart uint64
	JournalLogEnd  uint64
	ReservedJournal [4]uint64

	// utf8, null padded
	Label [64]byte

	Reserved [BrieFsSuperReserved]uint8
}

// Superblock represents the filesystem superblock.
type Superblock struct {
	Lay SuperblockLayout
}

// Block layout constants - defines the order of metadata on disk
// Block 0: Superblock
// Block 1+: Inode bitmap
// Next:    Data bitmap
// Next:    Inode table
// Next:    EAT (extent allocation table) - reserved space
// Next:    Trie root header (first block of trie node pool)
// Next:    Trie node data blocks
// Next:    Data region
// Last:    Journal

// TrieNode is the on-disk bitwise trie node (32 bytes).
// Matches `struct trie_node` in the kernel module.
type TrieNode struct {
	RangeStart uint64
	RangeLen   uint32
	FreeCount  uint32
	LeftChild  uint64
	RightChild uint64
}

// TrieRoot is the trie root block header.
// Matches `struct briefs_trie_root` in the kernel module.
type TrieRoot struct {
	Magic     uint32 // "TRIE" - 0x54524945
	Version   uint32 // 1
	RootNode  uint64 // block offset of root trie node (relative to trie pool start)
	FreeList  uint64 // next free trie node block
	NodeCount uint32 // total trie nodes in use
	Reserved  [7]uint32
}

// MarshalBinary serializes a TrieNode to its 32-byte on-disk format.
func (tn *TrieNode) MarshalBinary() []byte {
	data := make([]byte, 32)
	binary.LittleEndian.PutUint64(data[0:], tn.RangeStart)
	binary.LittleEndian.PutUint32(data[8:], tn.RangeLen)
	binary.LittleEndian.PutUint32(data[12:], tn.FreeCount)
	binary.LittleEndian.PutUint64(data[16:], tn.LeftChild)
	binary.LittleEndian.PutUint64(data[24:], tn.RightChild)
	return data
}

// MarshalBinary serializes a TrieRoot to its 32-byte on-disk format.
func (tr *TrieRoot) MarshalBinary() []byte {
	data := make([]byte, 32)
	binary.LittleEndian.PutUint32(data[0:], tr.Magic)
	binary.LittleEndian.PutUint32(data[4:], tr.Version)
	binary.LittleEndian.PutUint64(data[8:], tr.RootNode)
	binary.LittleEndian.PutUint64(data[16:], tr.FreeList)
	binary.LittleEndian.PutUint32(data[24:], tr.NodeCount)
	return data
}

// DirEntry is an on-disk directory entry (80 bytes).
// Matches `struct briefs_dir_entry` in the kernel module.
type DirEntry struct {
	Inode    uint64
	NameLen  uint32
	Type     uint32 // file type (S_IFMT bits)
	Name     [64]byte
}

// DirBlock is a directory block on disk (4096 bytes).
// Matches `struct briefs_dir_block` in the kernel module.
type DirBlock struct {
	Magic      uint32 // "DRYR" - 0x44525952
	EntryCount uint32
	Flags      uint32
	Reserved   uint32
	Entries    [4]DirEntry
}

// NewDirBlock creates a directory block with the given entries.
func NewDirBlock(entries []DirEntry) DirBlock {
	db := DirBlock{
		Magic:      0x44525952, // "DRYR"
		EntryCount: uint32(len(entries)),
	}
	for i, e := range entries {
		if i < 4 {
			db.Entries[i] = e
		}
	}
	return db
}

// MarshalBinary serializes the directory block to 4096 bytes.
func (db *DirBlock) MarshalBinary() []byte {
	buf := make([]byte, 4096)
	pos := 0
	binary.LittleEndian.PutUint32(buf[pos:], db.Magic); pos += 4
	binary.LittleEndian.PutUint32(buf[pos:], db.EntryCount); pos += 4
	binary.LittleEndian.PutUint32(buf[pos:], db.Flags); pos += 4
	binary.LittleEndian.PutUint32(buf[pos:], db.Reserved); pos += 4
	for i := 0; i < 4; i++ {
		binary.LittleEndian.PutUint64(buf[pos:], db.Entries[i].Inode); pos += 8
		binary.LittleEndian.PutUint32(buf[pos:], db.Entries[i].NameLen); pos += 4
		binary.LittleEndian.PutUint32(buf[pos:], db.Entries[i].Type); pos += 4
		copy(buf[pos:pos+64], db.Entries[i].Name[:]); pos += 64
	}
	return buf
}

// Extent helpers

// SetInlineExtent sets one of the 8 inline extents on an inode.
func (in *Inode) SetInlineExtent(index int, offset, phys, length, flags uint64) {
	if index < 0 || index >= 8 {
		return
	}
	in.InlineExtents[index] = Extent{
		Offset: offset,
		Phys:   phys,
		Len:    length,
		Flags:  uint32(flags),
		Pad:    0,
	}
	// Update inline extent count if this is the last one
	if index+1 > int(in.NumExtentsInline) {
		in.NumExtentsInline = uint32(index + 1)
	}
	if index+1 > int(in.NumExtentsTotal) {
		in.NumExtentsTotal = uint64(index + 1)
	}
}

// nextPowerOf2 returns the smallest power of 2 >= n.
func nextPowerOf2(n uint64) uint64 {
	if n <= 1 {
		return 1
	}
	n--
	n |= n >> 1
	n |= n >> 2
	n |= n >> 4
	n |= n >> 8
	n |= n >> 16
	n |= n >> 32
	return n + 1
}

// AllocTreeBuilder builds a bitwise trie for free block tracking.
type AllocTreeBuilder struct {
	// Flat array of trie nodes, indexed by node index.
	Nodes     []TrieNode
	NextIndex uint64
}

// NewAllocTreeBuilder creates a tree builder for the given number of data blocks.
// The tree will be a complete binary trie where leaves represent individual blocks.
func NewAllocTreeBuilder(dataBlockCount uint64) *AllocTreeBuilder {
	padded := nextPowerOf2(dataBlockCount)
	// Total nodes in a complete binary trie covering `padded` leaf blocks:
	// root (covers `padded`), 2 children (cover `padded/2`), 4 (cover `padded/4`), ..., padded leaves
	// = 1 + 2 + 4 + ... + padded = 2 * padded - 1
	totalNodes := 2*padded - 1
	if totalNodes < 1 {
		totalNodes = 1
	}
	return &AllocTreeBuilder{
		Nodes:     make([]TrieNode, totalNodes),
		NextIndex: 0,
	}
}

// Build builds the trie. Returns a map of block-number-in-pool -> node data.
// The returned trieNodeBlocks includes only the node data blocks (no header block).
func (tb *AllocTreeBuilder) Build(dataBlockCount uint64) []TrieNode {
	padded := nextPowerOf2(dataBlockCount)
	tb.buildNode(0, padded, dataBlockCount)
	return tb.Nodes
}

// buildNode recursively builds the trie starting at the current NextIndex.
func (tb *AllocTreeBuilder) buildNode(rangeStart uint64, paddedRangeLen uint64, maxValid uint64) uint64 {
	idx := tb.NextIndex
	tb.NextIndex++

	node := &tb.Nodes[idx]
	node.RangeStart = rangeStart
	node.RangeLen = uint32(paddedRangeLen)

	if paddedRangeLen == 1 {
		// Leaf node: free if this block is within the valid data region
		if rangeStart < maxValid {
			node.FreeCount = 1
		} else {
			node.FreeCount = 0
		}
		return idx
	}

	// Internal node: recurse into children
	half := paddedRangeLen / 2

	// Left child covers [rangeStart, rangeStart + half)
	leftIdx := tb.buildNode(rangeStart, half, maxValid)
	node.LeftChild = leftIdx

	// Right child covers [rangeStart + half, rangeStart + paddedRangeLen)
	rightIdx := tb.buildNode(rangeStart+half, half, maxValid)
	node.RightChild = rightIdx

	// Free count = sum of children
	node.FreeCount = tb.Nodes[leftIdx].FreeCount + tb.Nodes[rightIdx].FreeCount

	return idx
}

// NbBlocks returns the number of 4096-byte blocks needed for all trie nodes.
// Each block holds 128 trie nodes (4096 / 32 = 128).
func (tb *AllocTreeBuilder) NbBlocks() uint64 {
	nodesPerBlock := uint64(4096 / 32)
	nodeCount := uint64(len(tb.Nodes))
	if nodeCount == 0 {
		return 0
	}
	return (nodeCount + nodesPerBlock - 1) / nodesPerBlock
}

// WriteNodes packs the trie nodes into blocks. Returns one []byte per block.
// The caller can write these blocks starting at (poolStartBlock + 1) since
// block 0 of the pool is the trie root header.
func (tb *AllocTreeBuilder) WriteNodes() [][]byte {
	nb := tb.NbBlocks()
	if nb == 0 {
		return nil
	}
	blocks := make([][]byte, nb)
	nodesPerBlock := uint64(4096 / 32)
	for bi := uint64(0); bi < nb; bi++ {
		buf := make([]byte, 4096)
		start := bi * nodesPerBlock
		end := start + nodesPerBlock
		if end > uint64(len(tb.Nodes)) {
			end = uint64(len(tb.Nodes))
		}
		for i := start; i < end; i++ {
			nodeData := tb.Nodes[i].MarshalBinary()
			copy(buf[(i-start)*32:], nodeData)
		}
		blocks[bi] = buf
	}
	return blocks
}

// Getters for easy access to superblock fields
func (sb *Superblock) TotalBlocks() uint64 { return sb.Lay.TotalBlocks }
func (sb *Superblock) BlockSize() uint64 { return sb.Lay.BlockSize }
func (sb *Superblock) InodeSize() uint64 { return sb.Lay.InodeSize }
func (sb *Superblock) JournalBlocks() uint64 { return sb.Lay.JournalBlocks }
func (sb *Superblock) DataBlocks() uint64 { return sb.Lay.DataBlocks }
func (sb *Superblock) TotalInodes() uint64 { return sb.Lay.FreeInodes + 100 } // rough estimate

// NewSuperblock creates a new superblock with the given metadata.
// mkfs.briefs will set the layout fields (bitmap offsets, etc.) after creation.
func NewSuperblock(totalBlocks, blockSize, inodeSize, journalBlocks uint64, label string) *Superblock {
	sb := &Superblock{}

	sb.Lay.Magic = MagicSuperblock
	sb.Lay.MajorVer = 0
	sb.Lay.MinorVer = 1
	sb.Lay.PatchVer = 0
	sb.Lay.TotalBlocks = totalBlocks
	sb.Lay.BlockSize = blockSize
	sb.Lay.InodeSize = inodeSize
	sb.Lay.BlocksGrp = 1024  // TODO
	sb.Lay.InodesGrp = 256   // TODO
	sb.Lay.FSCreated = uint64(time.Now().Unix())
	sb.Lay.FSLastMount = 0
	sb.Lay.FSLastChkpt = 0
	sb.Lay.FreeInodes = 0 // set by mkfs
	sb.Lay.FreeDataBlks = 0 // set by mkfs
	sb.Lay.RootIno = 1

	sb.Lay.JournalOffset = totalBlocks - journalBlocks
	sb.Lay.JournalBlocks = journalBlocks
	sb.Lay.JournalLogStart = sb.Lay.JournalOffset
	sb.Lay.JournalLogEnd = sb.Lay.JournalOffset

	// Set label
	copy(sb.Lay.Label[:], []byte(label))

	// Generate a UUID for this volume
	fsUuid := uuid.New()
	for i, v := range fsUuid {
		sb.Lay.UUID[i] = v
	}

	return sb
}

// Write writes the superblock to a file and initializes the full filesystem image.
func (sb *Superblock) Write(path string) error {
	// Calculate total size: superblock + bitmaps + inode table + journal + data
	totalSize := sb.Lay.TotalBlocks * sb.Lay.BlockSize

	// Create the file and truncate to full size
	file, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}
	defer file.Close()

	if err := file.Truncate(int64(totalSize)); err != nil {
		return fmt.Errorf("truncate file: %w", err)
	}

	// Write superblock to first block
	block := make([]byte, sb.Lay.BlockSize)
	copy(block[:], sb.MarshalBinary())

	if _, err := file.WriteAt(block, 0); err != nil {
		return fmt.Errorf("write superblock: %w", err)
	}

	// TODO: Initialize bitmap blocks
	// TODO: Initialize inode table
	// TODO: Initialize journal with checkpoint record
	// TODO: Initialize trie node pool

	// For now, just zeros for remaining blocks
	// (This is sufficient for testing - the C fsck can validate the format)

	return nil
}

// MarshalBinary converts the superblock to binary format.
func (sb *Superblock) MarshalBinary() []byte {
	data := make([]byte, 4096)
	pos := 0

	// Write fields in order
	binary.LittleEndian.PutUint64(data[pos:], sb.Lay.Magic); pos += 8
	binary.LittleEndian.PutUint64(data[pos:], sb.Lay.MajorVer); pos += 8
	binary.LittleEndian.PutUint64(data[pos:], sb.Lay.MinorVer); pos += 8
	binary.LittleEndian.PutUint64(data[pos:], sb.Lay.PatchVer); pos += 8
	binary.LittleEndian.PutUint64(data[pos:], sb.Lay.TotalBlocks); pos += 8
	binary.LittleEndian.PutUint64(data[pos:], sb.Lay.DataBlocks); pos += 8
	binary.LittleEndian.PutUint64(data[pos:], sb.Lay.BlockSize); pos += 8
	binary.LittleEndian.PutUint64(data[pos:], sb.Lay.InodeSize); pos += 8
	binary.LittleEndian.PutUint64(data[pos:], sb.Lay.BlocksGrp); pos += 8
	binary.LittleEndian.PutUint64(data[pos:], sb.Lay.InodesGrp); pos += 8
	binary.LittleEndian.PutUint64(data[pos:], sb.Lay.FSCreated); pos += 8
	binary.LittleEndian.PutUint64(data[pos:], sb.Lay.FSLastMount); pos += 8
	binary.LittleEndian.PutUint64(data[pos:], sb.Lay.FSLastChkpt); pos += 8
	binary.LittleEndian.PutUint64(data[pos:], sb.Lay.FreeDataBlks); pos += 8
	binary.LittleEndian.PutUint64(data[pos:], sb.Lay.FreeInodes); pos += 8
	binary.LittleEndian.PutUint64(data[pos:], sb.Lay.RootIno); pos += 8
	binary.LittleEndian.PutUint64(data[pos:], sb.Lay.FeatCompat); pos += 8
	binary.LittleEndian.PutUint64(data[pos:], sb.Lay.FeatROCompat); pos += 8
	binary.LittleEndian.PutUint64(data[pos:], sb.Lay.FeatIncompat); pos += 8

	copy(data[pos:pos+16], sb.Lay.UUID[:]); pos += 16

	binary.LittleEndian.PutUint64(data[pos:], sb.Lay.EATOffset); pos += 8
	binary.LittleEndian.PutUint64(data[pos:], sb.Lay.EATBlocks); pos += 8
	binary.LittleEndian.PutUint64(data[pos:], sb.Lay.TrieRootBlock); pos += 8
	binary.LittleEndian.PutUint64(data[pos:], sb.Lay.TrieBlocksUsed); pos += 8
	binary.LittleEndian.PutUint64(data[pos:], sb.Lay.TrieNodePoolStart); pos += 8
	binary.LittleEndian.PutUint64(data[pos:], sb.Lay.TrieNodePoolSize); pos += 8
	binary.LittleEndian.PutUint64(data[pos:], sb.Lay.InodeBMOffset); pos += 8
	binary.LittleEndian.PutUint64(data[pos:], sb.Lay.InodeBMBlocks); pos += 8
	binary.LittleEndian.PutUint64(data[pos:], sb.Lay.DataBitmapOffset); pos += 8
	binary.LittleEndian.PutUint64(data[pos:], sb.Lay.DataBitmapBlocks); pos += 8
	binary.LittleEndian.PutUint64(data[pos:], sb.Lay.JournalOffset); pos += 8
	binary.LittleEndian.PutUint64(data[pos:], sb.Lay.JournalBlocks); pos += 8
	binary.LittleEndian.PutUint64(data[pos:], sb.Lay.CheckpointSeq); pos += 8
	binary.LittleEndian.PutUint64(data[pos:], sb.Lay.JournalLogStart); pos += 8
	binary.LittleEndian.PutUint64(data[pos:], sb.Lay.JournalLogEnd); pos += 8

	copy(data[pos:pos+32], []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}); pos += 32

	copy(data[pos:pos+64], sb.Lay.Label[:]); pos += 64

	// Pad to 4096 bytes
	for i := pos; i < 4096; i++ {
		data[i] = 0
	}

	return data
}

// Extent represents a file extent (16 bytes).
type Extent struct {
	Offset uint64
	Phys   uint64
	Len    uint64
	Flags  uint32
	Pad    uint32
}

// Inode represents a filesystem inode (512 bytes).
type Inode struct {
	InodeNumber       uint64
	Magic             uint64   // C struct has __u64 magic
	Filemode          uint32
	Uid               uint32
	Gid               uint32
	_Pad0             uint32   // explicit padding for u64 alignment
	FileSize          uint64
	CtimeSec          uint64
	CtimeNsec         uint64
	AtimeSec          uint64
	AtimeNsec         uint64
	MtimeSec          uint64
	MtimeNsec         uint64
	CreationTimeSec   uint64
	CreationTimeNsec  uint64
	Nlinks            uint32
	NumExtentsInline  uint32
	ExtentInlineBase  uint64
	NumExtentsTotal   uint64
	InlineExtents     [8]Extent
	XattrOffset       uint64
	XattrSize         uint64
	ParentInode       uint64
	LinkCount         uint32
	Flags             uint32
	Reserved          [96]byte
}

// NewInode creates a new inode with default values.
func NewInode(ino uint64, mode uint32) *Inode {
	// Let's set the timestamps on new inodes.
	t := time.Now()
	sec := uint64(t.Unix()) // cast to uint64
	nsec := uint64(t.Nanosecond())
	return &Inode{
		InodeNumber: ino,
		Magic:       InodeMagic,
		Filemode:    mode,
		Uid:         0, // root
		Gid:         0, // root
		FileSize:    0,
		Nlinks:      1,
		CtimeSec:    sec,
		CtimeNsec:   nsec,
		AtimeSec:    sec,
		AtimeNsec:   nsec,
		MtimeSec:    sec,
		MtimeNsec:   nsec,
		CreationTimeSec: sec,
		CreationTimeNsec: nsec,
	}
}

// IsDir returns true if the inode is a directory.
func (in *Inode) IsDir() bool {
	return in.Filemode&ModeDir != 0
}

// IsFile returns true if the inode is a regular file.
func (in *Inode) IsFile() bool {
	return in.Filemode&ModeFile != 0
}

// WriteAt writes the inode to a file at the given byte offset.
func (in *Inode) WriteAt(file *os.File, offset int64) error {
	block := make([]byte, DefaultInodeSize)
	pos := 0

	binary.LittleEndian.PutUint64(block[pos:], in.InodeNumber); pos += 8
	binary.LittleEndian.PutUint64(block[pos:], in.Magic); pos += 8
	binary.LittleEndian.PutUint32(block[pos:], in.Filemode); pos += 4
	binary.LittleEndian.PutUint32(block[pos:], in.Uid); pos += 4
	binary.LittleEndian.PutUint32(block[pos:], in.Gid); pos += 4
	binary.LittleEndian.PutUint32(block[pos:], in._Pad0); pos += 4
	binary.LittleEndian.PutUint64(block[pos:], in.FileSize); pos += 8
	binary.LittleEndian.PutUint64(block[pos:], in.CtimeSec); pos += 8
	binary.LittleEndian.PutUint64(block[pos:], in.CtimeNsec); pos += 8
	binary.LittleEndian.PutUint64(block[pos:], in.AtimeSec); pos += 8
	binary.LittleEndian.PutUint64(block[pos:], in.AtimeNsec); pos += 8
	binary.LittleEndian.PutUint64(block[pos:], in.MtimeSec); pos += 8
	binary.LittleEndian.PutUint64(block[pos:], in.MtimeNsec); pos += 8
	binary.LittleEndian.PutUint64(block[pos:], in.CreationTimeSec); pos += 8
	binary.LittleEndian.PutUint64(block[pos:], in.CreationTimeNsec); pos += 8
	binary.LittleEndian.PutUint32(block[pos:], in.Nlinks); pos += 4
	binary.LittleEndian.PutUint32(block[pos:], in.NumExtentsInline); pos += 4
	binary.LittleEndian.PutUint64(block[pos:], in.ExtentInlineBase); pos += 8
	binary.LittleEndian.PutUint64(block[pos:], in.NumExtentsTotal); pos += 8

	// Write inline extents
	for i := 0; i < 8; i++ {
		binary.LittleEndian.PutUint64(block[pos:], in.InlineExtents[i].Offset); pos += 8
		binary.LittleEndian.PutUint64(block[pos:], in.InlineExtents[i].Phys); pos += 8
		binary.LittleEndian.PutUint64(block[pos:], in.InlineExtents[i].Len); pos += 8
		binary.LittleEndian.PutUint32(block[pos:], in.InlineExtents[i].Flags); pos += 4
		binary.LittleEndian.PutUint32(block[pos:], in.InlineExtents[i].Pad); pos += 4
	}

	binary.LittleEndian.PutUint64(block[pos:], in.XattrOffset); pos += 8
	binary.LittleEndian.PutUint64(block[pos:], in.XattrSize); pos += 8
	binary.LittleEndian.PutUint64(block[pos:], in.ParentInode); pos += 8
	binary.LittleEndian.PutUint32(block[pos:], in.LinkCount); pos += 4
	binary.LittleEndian.PutUint32(block[pos:], in.Flags); pos += 4

	copy(block[pos:pos+96], in.Reserved[:])

	written, err := file.WriteAt(block, offset)
	if err != nil {
		return err
	}
	if written != len(block) {
		return fmt.Errorf("short write: expected %d, got %d", len(block), written)
	}
	return nil
}

// Write writes the inode to a file at the given offset (in 512-byte units).
// Deprecated: Use WriteAt with byte offsets instead.
func (in *Inode) Write(file *os.File, offset uint64) error {
	return in.WriteAt(file, int64(offset*512))
}

// ValidateInode checks if the inode magic is correct.
func (in *Inode) ValidateInode() bool {
	return in.Magic == InodeMagic
}
