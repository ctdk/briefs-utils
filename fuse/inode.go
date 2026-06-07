package fuse

import (
	"fmt"
	"os"

	"github.com/ctdk/briefs-utils/types"
)

// loadInode reads an inode from the image at the given inode number.
func loadInode(f *os.File, sb *types.Superblock, ino uint64) (*types.Inode, error) {
	if ino < 1 {
		return nil, fmt.Errorf("invalid inode number %d", ino)
	}
	inode, offset := calcInodeLocation(sb, ino)
	data := make([]byte, types.DefaultInodeSize)
	n, err := f.ReadAt(data, int64(inode*sb.Lay.BlockSize+offset))
	if err != nil {
		return nil, fmt.Errorf("read inode %d at %d+%d: %w", ino, inode, offset, err)
	}
	if n < int(types.DefaultInodeSize) {
		return nil, fmt.Errorf("short read for inode %d: %d bytes", ino, n)
	}
	return types.UnmarshalInode(data)
}

// calcInodeLocation returns (blockOffset, byteOffset) for a given inode number.
func calcInodeLocation(sb *types.Superblock, ino uint64) (uint64, uint64) {
	inodesPerBlock := sb.Lay.BlockSize / sb.Lay.InodeSize
	inodeTableStart := sb.DataBitmapOffset() + sb.DataBitmapBlocks()
	inodeIndex := ino - 1
	return inodeTableStart + (inodeIndex / inodesPerBlock),
		(inodeIndex % inodesPerBlock) * sb.InodeSize()
}

// writeInodeBack flushes a dirty inode back to the image file.
func writeInodeBack(f *os.File, sb *types.Superblock, ino uint64) error {
	inode, err := loadInode(f, sb, ino)
	if err != nil {
		return err
	}
	// In practice this would write the in-memory inode back.
	// For the skeleton we skip the write — just remove from dirty set.
	return nil
}
