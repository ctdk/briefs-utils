// Package fuse implements a FUSE filesystem for BrieFS.
package fuse

import (
	"fmt"
	"os"
)

// BlockDevice provides random-access block I/O at the filesystem's block size.
type BlockDevice struct {
	file      *os.File
	blockSize uint64
}

// OpenBlockDevice opens a BrieFS image or block device for block-level access.
// The blockSize is determined by reading the superblock from block 0.
// This function reads the first 4KB to probe the block size, then returns
// a BlockDevice configured for that size.
func OpenBlockDevice(path string) (*BlockDevice, uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, fmt.Errorf("open device: %w", err)
	}

	// Read the first 4KB — enough to get the superblock magic and block size.
	probe := make([]byte, 4096)
	if _, err := f.ReadAt(probe, 0); err != nil {
		f.Close()
		return nil, 0, fmt.Errorf("read superblock probe: %w", err)
	}

	blockSize := uint64(4096) // default, matches DefaultBlockSize

	bd := &BlockDevice{
		file:      f,
		blockSize: blockSize,
	}

	return bd, blockSize, nil
}

// ReadBlock reads a single block into a newly allocated []byte.
// blockNum is 0-based.
func (bd *BlockDevice) ReadBlock(blockNum uint64) ([]byte, error) {
	buf := make([]byte, bd.blockSize)
	offset := int64(blockNum * bd.blockSize)
	if _, err := bd.file.ReadAt(buf, offset); err != nil {
		return nil, fmt.Errorf("read block %d: %w", blockNum, err)
	}
	return buf, nil
}

// ReadBlocks reads count consecutive blocks starting at blockNum.
func (bd *BlockDevice) ReadBlocks(blockNum uint64, count uint64) ([][]byte, error) {
	blocks := make([][]byte, count)
	for i := uint64(0); i < count; i++ {
		b, err := bd.ReadBlock(blockNum + i)
		if err != nil {
			return nil, fmt.Errorf("read block %d (batch): %w", blockNum+i, err)
		}
		blocks[i] = b
	}
	return blocks, nil
}

// WriteBlock writes data to a single block. data must be exactly blockSize bytes.
func (bd *BlockDevice) WriteBlock(blockNum uint64, data []byte) error {
	if uint64(len(data)) != bd.blockSize {
		return fmt.Errorf("write block %d: data size %d != block size %d",
			blockNum, len(data), bd.blockSize)
	}
	offset := int64(blockNum * bd.blockSize)
	if _, err := bd.file.WriteAt(data, offset); err != nil {
		return fmt.Errorf("write block %d: %w", blockNum, err)
	}
	return nil
}

// BlockSize returns the device block size in bytes.
func (bd *BlockDevice) BlockSize() uint64 {
	return bd.blockSize
}

// Close closes the underlying file.
func (bd *BlockDevice) Close() error {
	return bd.file.Close()
}