package briefs

// Superblock structs and methods

import (
	"fmt"
	"io"

	"github.com/google/uuid"
	"os"
	"time"
)

//go:briefs-disk size=1024
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
func NewSuperblock(totalBlocks, blockSize, inodeSize, journalBlocks uint64, label string, uuidStr string) (*Superblock, error) {
	sb := &Superblock{}

	sb.Lay.Magic = MagicSuperblock
	sb.Lay.MajorVer = BrieFSMajorVersion
	sb.Lay.MinorVer = BrieFSMinorVersion
	sb.Lay.PatchVer = BrieFSPatchVersion
	sb.Lay.FeatIncompat = FeatureIncompatBtree
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

	// Generate a UUID for this volume, or use one passed into this
	// function. Return an error if it fails.
	var fsUuid uuid.UUID
	var err error
	if uuidStr != "" {
		if fsUuid, err = uuid.Parse(uuidStr); err != nil {
			return nil, err
		}
	} else {
		fsUuid = uuid.New()
	}

	for i, v := range fsUuid {
		sb.Lay.UUID[i] = v
	}

	return sb, nil
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
	data, err := sb.Lay.MarshalBinary()
	if err != nil {
		panic(err)
	}
	return data
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
	// On-disk format gate (clean break, v0.9.0 B+ tree extent index).
	if sb.MinorVer != BrieFSMinorVersion {
		return nil, fmt.Errorf("incompatible on-disk minor version %d (need %d)", sb.MinorVer, BrieFSMinorVersion)
	}
	if sb.FeatIncompat&FeatureIncompatBtree == 0 {
		return nil, fmt.Errorf("image is not B-tree formatted (feature_incompat 0x%016X)", sb.FeatIncompat)
	}
	if sb.FeatIncompat&^FeatureIncompatBtree != 0 {
		return nil, fmt.Errorf("unknown incompatible feature bits 0x%016X", sb.FeatIncompat&^FeatureIncompatBtree)
	}
	return sb, nil
}
