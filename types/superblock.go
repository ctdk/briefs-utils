package types

// Superblock structs and methods

import (
	"encoding/binary"
	"fmt"
	"io"

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
	InodeBMOffset  uint64  // offset 216
	InodeBMBlocks  uint64  // offset 224
	InodeTableOffset uint64  // offset 232 (replaces data_bitmap_offset + data_bitmap_blocks)
	JournalOffset  uint64  // offset 240
	JournalBlocks  uint64  // offset 248
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
	sb.Lay.MajorVer = BrieFSMajorVersion
	sb.Lay.MinorVer = BrieFSMinorVersion
	sb.Lay.PatchVer = BrieFSPatchVersion
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
	binary.LittleEndian.PutUint64(data[pos:], sb.Lay.InodeTableOffset); pos += 8
	// skipped: old data_bitmap_offset + data_bitmap_blocks area (now replaced by InodeTableOffset)
	// pos += 8
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

// UnmarshalBinary deserializes a SuperblockLayout from a byte slice.
// The data should be at least BrieFSSuperSize bytes.
func (sb *SuperblockLayout) UnmarshalBinary(data []byte) error {
	if len(data) < BrieFSSuperSize {
		return fmt.Errorf("superblock data too short: %d < %d", len(data), BrieFSSuperSize)
	}
	pos := 0
	sb.Magic = binary.LittleEndian.Uint64(data[pos:]); pos += 8
	sb.MajorVer = binary.LittleEndian.Uint64(data[pos:]); pos += 8
	sb.MinorVer = binary.LittleEndian.Uint64(data[pos:]); pos += 8
	sb.PatchVer = binary.LittleEndian.Uint64(data[pos:]); pos += 8
	sb.TotalBlocks = binary.LittleEndian.Uint64(data[pos:]); pos += 8
	sb.DataBlocks = binary.LittleEndian.Uint64(data[pos:]); pos += 8
	sb.BlockSize = binary.LittleEndian.Uint64(data[pos:]); pos += 8
	sb.InodeSize = binary.LittleEndian.Uint64(data[pos:]); pos += 8
	sb.BlocksGrp = binary.LittleEndian.Uint64(data[pos:]); pos += 8
	sb.InodesGrp = binary.LittleEndian.Uint64(data[pos:]); pos += 8
	sb.FSCreated = binary.LittleEndian.Uint64(data[pos:]); pos += 8
	sb.FSLastMount = binary.LittleEndian.Uint64(data[pos:]); pos += 8
	sb.FSLastChkpt = binary.LittleEndian.Uint64(data[pos:]); pos += 8
	sb.FreeDataBlks = binary.LittleEndian.Uint64(data[pos:]); pos += 8
	sb.FreeInodes = binary.LittleEndian.Uint64(data[pos:]); pos += 8
	sb.RootIno = binary.LittleEndian.Uint64(data[pos:]); pos += 8
	sb.FeatCompat = binary.LittleEndian.Uint64(data[pos:]); pos += 8
	sb.FeatROCompat = binary.LittleEndian.Uint64(data[pos:]); pos += 8
	sb.FeatIncompat = binary.LittleEndian.Uint64(data[pos:]); pos += 8

	copy(sb.UUID[:], data[pos:pos+16]); pos += 16

	sb.EATOffset = binary.LittleEndian.Uint64(data[pos:]); pos += 8
	sb.EATBlocks = binary.LittleEndian.Uint64(data[pos:]); pos += 8
	sb.TrieRootBlock = binary.LittleEndian.Uint64(data[pos:]); pos += 8
	sb.TrieBlocksUsed = binary.LittleEndian.Uint64(data[pos:]); pos += 8
	sb.TrieNodePoolStart = binary.LittleEndian.Uint64(data[pos:]); pos += 8
	sb.TrieNodePoolSize = binary.LittleEndian.Uint64(data[pos:]); pos += 8
	sb.InodeBMOffset = binary.LittleEndian.Uint64(data[pos:]); pos += 8
	sb.InodeBMBlocks = binary.LittleEndian.Uint64(data[pos:]); pos += 8
	sb.InodeTableOffset = binary.LittleEndian.Uint64(data[pos:]); pos += 8
	sb.JournalOffset = binary.LittleEndian.Uint64(data[pos:]); pos += 8
	sb.JournalBlocks = binary.LittleEndian.Uint64(data[pos:]); pos += 8
	sb.CheckpointSeq = binary.LittleEndian.Uint64(data[pos:]); pos += 8
	sb.JournalLogStart = binary.LittleEndian.Uint64(data[pos:]); pos += 8
	sb.JournalLogEnd = binary.LittleEndian.Uint64(data[pos:]); pos += 8

	for i := 0; i < 4; i++ {
		sb.ReservedJournal[i] = binary.LittleEndian.Uint64(data[pos:]); pos += 8
	}

	copy(sb.Label[:], data[pos:pos+BrieFSVolLabelLen]); pos += BrieFSVolLabelLen

	return nil
}

// ReadSuperblock reads and parses the superblock from an io.ReaderAt.
func ReadSuperblock(r io.ReaderAt, blockSize uint64) (*SuperblockLayout, error) {
	buf := make([]byte, blockSize)
	if _, err := r.ReadAt(buf, 0); err != nil {
		return nil, fmt.Errorf("read superblock: %w", err)
	}
	sb := &SuperblockLayout{}
	if err := sb.UnmarshalBinary(buf); err != nil {
		return nil, fmt.Errorf("unmarshal superblock: %w", err)
	}
	if sb.Magic != MagicSuperblock {
		return nil, fmt.Errorf("bad superblock magic: 0x%016X (expected 0x%016X)", sb.Magic, MagicSuperblock)
	}
	return sb, nil
}
