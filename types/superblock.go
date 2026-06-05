package types

// Superblock structs and methods

import (
	"encoding/binary"
	"fmt"
	"github.com/google/uuid"
	"os"
	"time"
)

// SuperblockLayout is the on-disk format (first 1KB block).
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
	Label [BrieFSVolLabelLen]byte

	Reserved [BrieFSSuperReserved]uint8
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

	// Generate a UUID for this volume. TODO: Allow passing in a UUID for
	// this volume.
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

	return nil
}

// MarshalBinary converts the superblock to binary format.
func (sb *Superblock) MarshalBinary() []byte {
	data := make([]byte, BrieFSSuperSize)
	pos := 0

	// There are definitely more elegant ways to do this. Is it worth doing
	// this more elegantly?

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

	copy(data[pos:pos+BrieFSVolLabelLen], sb.Lay.Label[:]); pos += BrieFSVolLabelLen

	// Pad to 4096 bytes
	for i := pos; i < BrieFSSuperSize; i++ {
		data[i] = 0
	}

	return data
}
