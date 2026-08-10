package briefs

// inode structs and methods

import (
	"errors"
	"fmt"
	"os"
	"time"
)

var InlineExtentRangeErr = errors.New("index out of range for inode inline extents")

//go:briefs-disk size=512
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
	inlineRegion      [256]byte
	XattrOffset       uint64
	XattrSize         uint64
	ParentInode       uint64
	Unused            uint32
	Flags             uint32
	DirTrieRoot       uint64
	Rdev              uint64
	Generation        uint64 // inode generation for stable NFS file handles
	UserFlags         uint32
	Reserved          [68]byte
}

// InlineData returns the 256-byte raw inline data region of the inode.
func (in *Inode) InlineData() [256]byte {
	return in.inlineRegion
}

// SetInlineData copies the given 256-byte slice into the inode's inline data region.
func (in *Inode) SetInlineData(data [256]byte) {
	in.inlineRegion = data
}

// InlineExtents parses and returns the 8 inline extents stored in the raw region.
func (in *Inode) InlineExtents() [8]Extent {
	var ext [8]Extent
	for i := 0; i < 8; i++ {
		_ = ext[i].UnmarshalBinary(in.inlineRegion[i*32 : (i+1)*32])
	}
	return ext
}

// SetInlineExtents serializes the 8 inline extents into the raw region.
func (in *Inode) SetInlineExtents(ext [8]Extent) {
	for i := 0; i < 8; i++ {
		b, _ := ext[i].MarshalBinary()
		copy(in.inlineRegion[i*32:], b)
	}
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
	return in.Filemode&ModeTypeMask == ModeDir
}

// IsFile returns true if the inode is a regular file.
func (in *Inode) IsFile() bool {
	return in.Filemode&ModeTypeMask == ModeFile
}

// IsSymlink returns true if the inode is a symbolic link.
func (in *Inode) IsSymlink() bool {
	return in.Filemode&ModeTypeMask == ModeSymlink
}

// WriteAt writes the inode to a file at the given byte offset.
func (in *Inode) WriteAt(file *os.File, offset int64) error {
	block, err := in.MarshalBinary()
	if err != nil {
		return err
	}

	written, err := file.WriteAt(block, offset)
	if err != nil {
		return err
	}
	if written != len(block) {
		return fmt.Errorf("short write: expected %d, got %d", len(block), written)
	}
	return nil
}

// UnmarshalInode deserializes a 512-byte buffer into an Inode.
func UnmarshalInode(data []byte) (*Inode, error) {
	in := &Inode{}
	if err := in.UnmarshalBinary(data); err != nil {
		return nil, err
	}
	return in, nil
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
	e := Extent{Offset: offset, Phys: phys, Len: length, Flags: uint32(flags)}
	b, _ := e.MarshalBinary()
	copy(in.inlineRegion[index*32:], b)
	// Update inline extent count if this is the last one
	if index+1 > int(in.NumExtentsInline) {
		in.NumExtentsInline = uint32(index + 1)
	}
	if index+1 > int(in.NumExtentsTotal) {
		in.NumExtentsTotal = uint64(index + 1)
	}

	return nil
}
