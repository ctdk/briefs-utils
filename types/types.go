// Package types defines the BrieFS on-disk format.
package types

import (
	"encoding/binary"
	"fmt"
	"os"
)

// Magic numbers for our filesystem structures.
const (
	MagicSuperblock = 0x50656c6963616e62 // "Pelicanb"
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
)

// SuperblockLayout is the on-disk format (first 4KB block).
type SuperblockLayout struct {
	// Block 0: core fields
	Magic       uint64
	MajorVer    uint32
	MinorVer    uint32
	PatchVer    uint32
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

	// Block 0: metadata pointers
	EATOffset      uint64
	EATBlocks      uint64
	TrieRootBlock  uint64
	TrieBlocksUsed uint64
	TrieNodePoolStart uint64
	TrieNodePoolSize  uint64
	InodeBMOffset  uint64
	InodeBMBlocks  uint64
	DSBMOffset     uint64
	DSBMBlocks     uint64
	JournalOffset  uint64
	JournalBlocks  uint64
	CheckpointSeq  uint64
	JournalLogStart uint64
	JournalLogEnd  uint64
	ReservedJournal [4]uint64

	// utf8, null padded
	Label [64]byte
}

// Superblock represents the filesystem superblock.
type Superblock struct {
	Lay SuperblockLayout
}

// Getters for easy access to superblock fields
func (sb *Superblock) TotalBlocks() uint64 { return sb.Lay.TotalBlocks }
func (sb *Superblock) BlockSize() uint64 { return sb.Lay.BlockSize }
func (sb *Superblock) InodeSize() uint64 { return sb.Lay.InodeSize }
func (sb *Superblock) JournalBlocks() uint64 { return sb.Lay.JournalBlocks }
func (sb *Superblock) DataBlocks() uint64 { return sb.Lay.DataBlocks }
func (sb *Superblock) TotalInodes() uint64 { return sb.Lay.FreeInodes + 100 } // rough estimate

// NewSuperblock creates a new superblock with the given parameters.
func NewSuperblock(totalBlocks, blockSize, inodeSize, journalBlocks uint64, label string) *Superblock {
	sb := &Superblock{}

	sb.Lay.Magic = MagicSuperblock
	sb.Lay.MajorVer = 0
	sb.Lay.MinorVer = 0
	sb.Lay.PatchVer = 1
	sb.Lay.TotalBlocks = totalBlocks
	sb.Lay.DataBlocks = totalBlocks - journalBlocks - 4  // superblock + bitmaps + journal
	sb.Lay.BlockSize = blockSize
	sb.Lay.InodeSize = inodeSize
	sb.Lay.BlocksGrp = 1024  // TODO
	sb.Lay.InodesGrp = 256   // TODO
	sb.Lay.FSLastMount = 0
	sb.Lay.FSLastChkpt = 0
	sb.Lay.FreeDataBlks = totalBlocks - journalBlocks - 4 // superblock + bitmaps + journal
	sb.Lay.FreeInodes = 100 // TODO
	sb.Lay.RootIno = 1

	sb.Lay.JournalOffset = totalBlocks - journalBlocks
	sb.Lay.JournalBlocks = journalBlocks
	sb.Lay.JournalLogStart = sb.Lay.JournalOffset
	sb.Lay.JournalLogEnd = sb.Lay.JournalOffset

	// Set label
	copy(sb.Lay.Label[:], []byte(label))

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
	binary.LittleEndian.PutUint32(data[pos:], sb.Lay.MajorVer); pos += 4
	binary.LittleEndian.PutUint32(data[pos:], sb.Lay.MinorVer); pos += 4
	binary.LittleEndian.PutUint32(data[pos:], sb.Lay.PatchVer); pos += 4
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
	binary.LittleEndian.PutUint64(data[pos:], sb.Lay.DSBMOffset); pos += 8
	binary.LittleEndian.PutUint64(data[pos:], sb.Lay.DSBMBlocks); pos += 8
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
