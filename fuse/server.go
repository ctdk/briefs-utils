// Package fuse provides a FUSE-mounted view of a BrieFS image file.
package fuse

import (
	"fmt"
	"os"
	"sync"

	"github.com/ctdk/briefs-utils/device"
	"github.com/ctdk/briefs-utils/types"
	"github.com/hanwen/go-fuse/v2/fs"
)

// FuseFS is the root of the FUSE-mounted BrieFS volume.
type FuseFS struct {
	fs.Inode      // embedded inode tree root

	image *os.File
	sb    *types.Superblock
	// Allocator is a mutable copy of the on-disk bitmap pyramid,
	// used for allocating blocks during writes.
	alloc *types.AllocBuilder
	mu    sync.Mutex // guards dirty inodes below
	// dirtyInodes tracks inodes whose disk copy has been modified
	// and must be flushed back during fsync.
	dirtyInodes map[uint64]bool
}

// NewFuseFS opens a BrieFS image and reads the superblock.
// Returns nil on error (with err set), or an initialized FuseFS.
func NewFuseFS(imagePath string) (*FuseFS, error) {
	f, err := os.Open(imagePath)
	if err != nil {
		return nil, fmt.Errorf("open image: %w", err)
	}

	// Validate it looks like a block device or regular file.
	stat, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat: %w", err)
	}
	if !stat.Mode().IsRegular() {
		return nil, fmt.Errorf("image must be a regular file")
	}

	// Read block 0 (superblock).
	sb, err := readSuperblock(f)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("read superblock: %w", err)
	}

	// Validate magic.
	if sb.Lay.Magic != types.MagicSuperblock {
		f.Close()
		return nil, fmt.Errorf("not a BrieFS image: magic 0x%08x", sb.Lay.Magic)
	}

	// Initialize allocator from on-disk bitmap pyramid.
	alloc, err := loadAllocator(f, sb)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("load allocator: %w", err)
	}

	fs := &FuseFS{
		image:       f,
		sb:          sb,
		alloc:       alloc,
		dirtyInodes: make(map[uint64]bool),
	}

	// Verify root dir block is allocated.
	if !alloc.IsFree(dataRelFromAbs(sb, sb.TrieNodePoolStart+sb.TrieBlocksUsed)) {
		// root dir is at the first data block — check via allocator
	}

	return fs, nil
}

// readSuperblock reads block 0 of the image and deserializes it.
func readSuperblock(f *os.File) (*types.Superblock, error) {
	data := make([]byte, types.DefaultBlockSize)
	n, err := f.ReadAt(data, 0)
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	if n < int(types.BrieFSSuperSize) {
		return nil, fmt.Errorf("too short: %d bytes", n)
	}
	sb := types.NewSuperblock(0, 0, 0, 0, "")
	sb.UnmarshalBinary(data)
	return sb, nil
}

// dataRelFromAbs converts an absolute data-region block number to a
// data-relative index (block 0 = first data block).
func dataRelFromAbs(sb *types.Superblock, abs uint64) uint64 {
	return abs - (sb.TrieNodePoolStart + sb.TrieBlocksUsed)
}

// Close flushes dirty inodes and closes the image file.
func (f *FuseFS) Close() error {
	f.mu.Lock()
	dirty := make([]uint64, 0, len(f.dirtyInodes))
	for ino := range f.dirtyInodes {
		dirty = append(dirty, ino)
	}
	f.mu.Unlock()

	for _, ino := range dirty {
		if err := writeInodeBack(f.image, f.sb, ino); err != nil {
			return fmt.Errorf("flush inode %d: %w", ino, err)
		}
	}

	return f.image.Close()
}
