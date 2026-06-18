// Package briefs defines the BrieFS on-disk format.
package briefs

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// ErrChecksumMismatch is returned when a stored CRC32C checksum does not
// match the computed value.
var ErrChecksumMismatch = errors.New("CRC32C checksum mismatch")

// Magic numbers for our filesystem structures.
const (
	MagicSuperblock = 0x504C434E        // "PLCN"
	MagicInode      = 0x494E4F44        // "INOD"
	MagicTrieNode   = 0x54524E20        // "TRN "
	MagicTriePage   = 0x54524E50        // "TRNP"
	MagicDirEntry   = 0x44495245        // "DIRE"
	MagicJournal    = 0x4A4E4C5A        // "JNLZ"
	MagicCheckpoint = 0x43485053        // "CHPS"

	// File mode constants. Not using the normal Go ones from io/fs because
	// they aren't what we need for something this low level.
	ModeTypeMask = 0170000
	ModeDir      = 0040000
	ModeFile     = 0100000
	ModeSymlink  = 0120000

	// Default values.
	DefaultBlockSize   = 4096
	DefaultInodeSize   = 512
	DefaultJournalSize = 64 // blocks

	// Journal record types — mirrors briefs.h enum journal_record_type.
	// These are used by fsck and mkfs to avoid magic numbers.
	JRN_NONE = iota
	JRN_EXTENT_ALLOC
	JRN_EXTENT_FREE
	JRN_INODE_UPDATE
	JRN_INODE_ALLOC
	JRN_INODE_FREE
	JRN_TRIE_ALLOC
	JRN_DIR_UPDATE
	JRN_CHECKPOINT
	JRN_INODE_FULL
	JRN_SYMLINK_DATA

	// Trie node types — mirrors briefs.h NODE_TYPE_* / NODE_STATUS_*
	NodeTypeFile     = 0x01
	NodeTypeDir      = 0x02
	NodeTypeInterm   = 0x04
	NodeStatusLeaf   = 0x08

	// Trie page constants — packed node pages (BrieFS >= 0.7.0)
	TrieSlotsPerBlock = 64
	TrieSlotBits      = 6
	TrieSlotMask      = (1 << TrieSlotBits) - 1
	TriePageVersion   = 1

	// Trie node flags — mirrors briefs.h NODE_FLAG_*
	NodeFlagDeleted  = 0x00000004
	NodeFlagRoot     = 0x00000008

	// Inode flags
	InodeFlagReserved    = 0x00000001
	InodeFlagCompressed  = 0x00000002
	InodeFlagIndexed     = 0x00000004
	InodeFlagInlineData  = 0x00000008

	// Extent flags
	ExtentFlagHole = 0x00000001
	ExtentFlagEof  = 0x80000000

	// Superblock reserved area padding. Matches _BRIEFS_SUPER_RESERVED in
	// briefs.h in the kernel module.
	BrieFSSuperReserved = 648

	// Size of the superblock in bytes. This matches the expectation on the
	// C side of things. The golang side treating the superblock as being
	// 4096 bytes works and all, but it's better to have both sides of this
	// equation doing the same thing.
	BrieFSSuperSize = 1024

	// BrieFS max name length. Theoretically there's nothing keeping this
	// from being nearly infinite in length, but POSIX caps the name length
	// to 255.
	BrieFSMaxNameLen = 255

	// Volume labels are straight up allocated 64 bytes, though. I don't
	// know if that's for a reason or what, but I don't care that much. This
	// const at least helps keep the magic numbers down.
	BrieFSVolLabelLen = 64

	// BrieFS version numbers for this version of briefs-utils
	BrieFSMajorVersion = 0
	BrieFSMinorVersion = 8
	BrieFSPatchVersion = 0
)

var VersionStr = fmt.Sprintf("v%d.%d.%d", BrieFSMajorVersion, BrieFSMinorVersion, BrieFSPatchVersion)

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

// TrieMakeRef encodes a block number and slot index into a node reference.
func TrieMakeRef(block uint64, slot uint) uint64 {
	return (block << TrieSlotBits) | (uint64(slot) & TrieSlotMask)
}

// TrieRefBlock returns the block number encoded in a node reference.
func TrieRefBlock(ref uint64) uint64 {
	return ref >> TrieSlotBits
}

// TrieRefSlot returns the slot index encoded in a node reference.
func TrieRefSlot(ref uint64) uint {
	return uint(ref & TrieSlotMask)
}

// TrieRefIsNull reports whether a node reference is the null pointer.
func TrieRefIsNull(ref uint64) bool {
	return ref == 0
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

// Keeping extents in the main briefs.go file for the time being, until there's
// more going on.

// Extent represents a file extent (16 bytes).
type Extent struct {
	Offset uint64
	Phys   uint64
	Len    uint64
	Flags  uint32
	Pad    uint32
}

// ExtentChainHeaderSize is the size of the header in an extent chain block
// (next_overflow_block + num_extents_in_block + pad).
const ExtentChainHeaderSize = 16

// ExtentChainChecksumSize is the checksum field size.
const ExtentChainChecksumSize = 8

// ExtentChainChecksumOffset is the byte offset of the checksum field within a
// chain block. It follows the 16-byte header and 127 inline extents.
const ExtentChainChecksumOffset = ExtentChainHeaderSize + 127*32 // 4080 for 4KiB blocks

// ExtentsPerChainBlock returns how many extents fit in one chain block.
func ExtentsPerChainBlock(blockSize uint64) int {
	return int((blockSize - ExtentChainHeaderSize - ExtentChainChecksumSize) / 32)
}

// ExtentChainHeader is the on-disk header of an extent chain block.
// Matches struct briefs_extent_chain in the kernel module.
type ExtentChainHeader struct {
	NextOverflowBlock uint64 // block number of next chain block, 0 if last
	NumExtentsInBlock uint32 // number of extents stored in this block
	Pad               uint32
	// Followed by extents, then a u64 checksum at block_size - 8
}

// UnmarshalExtentChainHeader reads the chain header from a block buffer.
func UnmarshalExtentChainHeader(buf []byte) *ExtentChainHeader {
	return &ExtentChainHeader{
		NextOverflowBlock: binary.LittleEndian.Uint64(buf[0:]),
		NumExtentsInBlock: binary.LittleEndian.Uint32(buf[8:]),
		Pad:               binary.LittleEndian.Uint32(buf[12:]),
	}
}

// ReadChainExtent reads the i-th extent from a chain block buffer.
func ReadChainExtent(buf []byte, i int) Extent {
	offset := ExtentChainHeaderSize + i*32
	return Extent{
		Offset: binary.LittleEndian.Uint64(buf[offset:]),
		Phys:   binary.LittleEndian.Uint64(buf[offset+8:]),
		Len:    binary.LittleEndian.Uint64(buf[offset+16:]),
		Flags:  binary.LittleEndian.Uint32(buf[offset+24:]),
		Pad:    binary.LittleEndian.Uint32(buf[offset+28:]),
	}
}

// ReadChainChecksum reads the checksum field from a chain block buffer.
func ReadChainChecksum(buf []byte, blockSize uint64) uint64 {
	return binary.LittleEndian.Uint64(buf[ExtentChainChecksumOffset:])
}
