// Package fuse implements a FUSE filesystem for BrieFS.
package fuse

import (
	"fmt"
	"os"
	"syscall"

	"github.com/ctdk/briefs-utils/briefs"
)

// BlockDevice provides random-access block I/O at the filesystem's block size.
type BlockDevice struct {
	file      *os.File
	blockSize uint64

	// dirtyView, when set, serves reads of blocks the FUSE bridge is holding
	// in daemon memory (the deferred-metadata map, cache.go): it returns a
	// private copy of the deferred content so every reader sees the latest
	// metadata without waiting for the next journal sync to drain it.  Set
	// once in Mount before serving; nil for standalone users of BlockDevice.
	dirtyView func(block uint64) ([]byte, bool)
}

// SetDirtyView wires the deferred-block read hook (see the dirtyView field).
func (bd *BlockDevice) SetDirtyView(fn func(block uint64) ([]byte, bool)) {
	bd.dirtyView = fn
}

// OpenBlockDevice opens a BrieFS image or block device for block-level access.
// The blockSize is determined by reading the superblock from block 0.
// This function reads the first 4KB to probe the block size, then returns
// a BlockDevice configured for that size.
//
// The device is opened read-write so the FUSE bridge can mutate the volume.
// Callers that only need read access (none in the read-write bridge) may
// open the path themselves.
func OpenBlockDevice(path string) (*BlockDevice, uint64, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, 0, fmt.Errorf("open device: %w", err)
	}

	// Read the first 4KB — enough to get the superblock magic and block size.
	probe := make([]byte, 4096)
	if _, err := f.ReadAt(probe, 0); err != nil {
		f.Close()
		return nil, 0, fmt.Errorf("read superblock probe: %w", err)
	}

	// Parse the probe through the generated superblock codec instead of a
	// hand-rolled offset read; the full magic/version validation happens in
	// Mount's readSuperblock right after this.
	probeSb := &briefs.SuperblockLayout{}
	if err := probeSb.UnmarshalBinary(probe); err != nil {
		f.Close()
		return nil, 0, fmt.Errorf("parse superblock probe: %w", err)
	}
	blockSize := probeSb.BlockSize
	if blockSize == 0 || blockSize > 4096 || (blockSize&(blockSize-1)) != 0 {
		// Invalid or non-power-of-2 block size; fall back to default 4096.
		// This handles older images or corrupted superblocks gracefully.
		blockSize = 4096
	}

	bd := &BlockDevice{
		file:      f,
		blockSize: blockSize,
	}

	return bd, blockSize, nil
}

// File returns the underlying *os.File, so callers can use helpers that read
// the device directly (e.g. briefs.IterateInodeExtents).
func (bd *BlockDevice) File() *os.File { return bd.file }

// ReadBlock reads a single block into a newly allocated []byte.
// blockNum is 0-based.  Blocks the bridge is deferring in daemon memory are
// served from the dirty view (as a private copy), so readers see the latest
// metadata even though the on-disk block is only written at the next journal
// sync.
func (bd *BlockDevice) ReadBlock(blockNum uint64) ([]byte, error) {
	if bd.dirtyView != nil {
		if buf, ok := bd.dirtyView(blockNum); ok {
			return buf, nil
		}
	}
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

// Sync flushes the device file's kernel page cache to durable storage.
// The FUSE bridge has no buffer cache of its own, but WriteBlock writes go
// through the host kernel's page cache for the backing file/device. The
// journal commit and checkpoint paths call Sync before declaring a record
// committed, mirroring the kernel's sync_blockdev() / drain-before-snapshot
// discipline: metadata must be on disk before the JRN_INODE_FULL record that
// references it is itself committed.
func (bd *BlockDevice) Sync() error {
	if err := bd.file.Sync(); err != nil {
		return fmt.Errorf("sync device: %w", err)
	}
	return nil
}

// Fdatasync flushes the device's data to durable storage without the
// metadata a full Sync also writes (the data-drain half of the kernel's
// briefs_btree_drain discipline). Platforms or file types without
// fdatasync fall back to a full Sync, which flushes data too.
func (bd *BlockDevice) Fdatasync() error {
	if err := syscall.Fdatasync(int(bd.file.Fd())); err != nil {
		if err != syscall.ENOSYS && err != syscall.EINVAL {
			return fmt.Errorf("fdatasync device: %w", err)
		}
		if serr := bd.file.Sync(); serr != nil {
			return fmt.Errorf("fdatasync device (sync fallback): %w", serr)
		}
	}
	return nil
}

// ReadAt implements io.ReaderAt, allowing briefs.ReadSuperblock and
// briefs.ReadAllocatorHeader to work with a BlockDevice.
func (bd *BlockDevice) ReadAt(p []byte, off int64) (int, error) {
	return bd.file.ReadAt(p, off)
}

// Close closes the underlying file.
func (bd *BlockDevice) Close() error {
	return bd.file.Close()
}
