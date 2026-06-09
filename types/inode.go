package types

// inode structs and methods

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"time"
)

var InlineExtentRangeErr = errors.New("index out of range for inode inline extents")

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
	Unused            uint32
	Flags             uint32
	DirTrieRoot       uint64
	Rdev              uint64
	Reserved          [80]byte
}

// NewInode creates a new inode with default values.
func NewInode(ino uint64, mode uint32) *Inode {
	// Let's set the timestamps on new inodes.
	t := time.Now()
	sec := uint64(t.Unix()) // cast to uint64
	nsec := uint64(t.Nanosecond())
	return &Inode{
		InodeNumber: ino,
		Magic:       MagicInode,
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

	// Same thing as with the superblock: There are definitely more elegant
	// ways to do this. Is it worth pursuing that?
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
	binary.LittleEndian.PutUint64(block[pos:], in.DirTrieRoot); pos += 8
	binary.LittleEndian.PutUint64(block[pos:], in.Rdev); pos += 8

	copy(block[pos:pos+80], in.Reserved[:])

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
	return in.Magic == MagicInode
}

// Extent helpers

// SetInlineExtent sets one of the 8 inline extents on an inode.
func (in *Inode) SetInlineExtent(index int, offset, phys, length, flags uint64) error {
	if index < 0 || index >= 8 {
		return InlineExtentRangeErr
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

	return nil
}
