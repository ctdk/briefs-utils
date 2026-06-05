// Package types defines the BrieFS on-disk format.
package types

import (
	"fmt"
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

	// File mode constants. Not using the normal Go ones from io/fs because
	// they aren't what we need for something this low level.
	ModeDir   = 0040000
	ModeFile  = 0100000
	ModeSymlink = 0120000

	// Default values.
	DefaultBlockSize   = 4096
	DefaultInodeSize   = 512
	DefaultJournalSize = 64 // blocks

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

// Keeping extents in the main types.go file for the time being, until there's
// more going on.

// Extent represents a file extent (16 bytes).
type Extent struct {
	Offset uint64
	Phys   uint64
	Len    uint64
	Flags  uint32
	Pad    uint32
}
