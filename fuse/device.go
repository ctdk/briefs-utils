// Package fuse implements a FUSE filesystem for BrieFS.
package fuse

import (
	"fmt"
	"os"
	"sort"
	"sync"
	"syscall"

	"github.com/ctdk/briefs-utils/briefs"
	"golang.org/x/sys/unix"
)

// pendingWritebackCap bounds the tracked-writeback set under workloads that
// never fsync (generic/069 raw-write throughput): once the set exceeds this
// many distinct blocks, the next tracked write drains it inline via
// sync_file_range so the map cannot grow without bound.  8192 blocks is 32MB
// of 4K blocks — enough headroom that steady fsync-driven loads never hit it.
const pendingWritebackCap = 8192

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

	// pendingWB tracks block numbers written since the last FlushPendingWB,
	// so flushes can target just the dirty ranges (sync_file_range) instead
	// of whole-device fsyncs — the 2026-09 fuse 63-HANG family was exactly
	// this: every commit issued a device-wide cache flush that took seconds
	// on the VM's virtio disk.  Guarded by wbMu.
	wbMu      sync.Mutex
	pendingWB map[uint64]struct{}
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
	bd.noteWB(blockNum)
	return nil
}

// WriteBlockSlot writes a sub-block range: len(data) bytes at offsetInBlock
// within the given block.  Inode-table blocks pack 8 slots per 4096-byte
// block; the write-through of a fresh inode's slot must not read-modify-
// write the whole block, because sibling slots can hold uncommitted deferred
// state that must not reach the page cache ahead of its journal records
// (writeThroughFreshInodeSlot).
func (bd *BlockDevice) WriteBlockSlot(blockNum uint64, offsetInBlock uint64, data []byte) error {
	if offsetInBlock+uint64(len(data)) > bd.blockSize {
		return fmt.Errorf("write slot in block %d: range [%d+%d) exceeds block size %d",
			blockNum, offsetInBlock, len(data), bd.blockSize)
	}
	offset := int64(blockNum*bd.blockSize + offsetInBlock)
	if _, err := bd.file.WriteAt(data, offset); err != nil {
		return fmt.Errorf("write slot in block %d: %w", blockNum, err)
	}
	bd.noteWB(blockNum)
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

// noteWB records that blockNum was written and now holds unflushed page-cache
// data.  WriteBlock and WriteBlockSlot call it on every write.  Once the
// tracked set exceeds pendingWritebackCap, the excess is kicked out inline so
// a write-heavy caller that never fsyncs cannot grow the set without bound.
// The kick only STARTS writeback (SYNC_FILE_RANGE_WRITE, no WAIT flags): the
// write path never stalls on disk — back-pressure is left to the kernel's own
// dirty-page throttling, which is where the kernel module's write path gets
// its back-pressure too.  Correctness is unaffected: a kicked block dropped
// from the set is still flushed by the next commit-time FlushPendingWB's end
// Fdatasync, and kill -9 never loses kernel page cache.
func (bd *BlockDevice) noteWB(blockNum uint64) {
	bd.wbMu.Lock()
	if bd.pendingWB == nil {
		bd.pendingWB = make(map[uint64]struct{})
	}
	bd.pendingWB[blockNum] = struct{}{}
	over := len(bd.pendingWB) > pendingWritebackCap
	bd.wbMu.Unlock()
	if over {
		// Best-effort: an error here (e.g. writeback EIO) surfaces at the
		// next explicit flush, where callers already handle it.
		_ = bd.kickPendingWB()
	}
}

// kickPendingWB starts writeback for every tracked block without waiting for
// it (noteWB's over-cap path): snapshot+clear the set, then SYNC_FILE_RANGE_
// WRITE over the coalesced runs.  This bounds the tracked set while keeping
// the write path off the disk-queue critical path; FlushPendingWB is the
// variant that waits.
func (bd *BlockDevice) kickPendingWB() error {
	blocks := bd.takePendingWB()
	if blocks == nil {
		return nil
	}
	return forEachWBRun(blocks, bd.kickWBRange)
}

// forEachWBRun coalesces a sorted block snapshot into maximal contiguous
// runs and calls range on each [first,last] — a burst of adjacent writes
// (extent data, allocator pool rewrites) then drains in one
// sync_file_range call per run instead of one per block.
func forEachWBRun(blocks []uint64, rng func(first, last uint64) error) error {
	var first, last uint64
	for i, b := range blocks {
		if i > 0 && b == last+1 {
			last = b
			continue
		}
		if i > 0 {
			if err := rng(first, last); err != nil {
				return err
			}
		}
		first, last = b, b
	}
	return rng(first, last)
}

// kickWBRange starts (does not wait for) writeback of blocks [first,last].
func (bd *BlockDevice) kickWBRange(first, last uint64) error {
	off := int64(first * bd.blockSize)
	length := int64((last - first + 1) * bd.blockSize)
	err := unix.SyncFileRange(int(bd.file.Fd()), off, length, unix.SYNC_FILE_RANGE_WRITE)
	if err == nil {
		return nil
	}
	if err != syscall.ENOSYS && err != syscall.EINVAL {
		return fmt.Errorf("kick writeback blocks [%d,%d]: %w", first, last, err)
	}
	// No sync_file_range: nothing to fall back to that starts-without-waiting;
	// the tracked set was already cleared, so the blocks are simply untracked
	// and the next commit-time flush's device-level sync covers them.
	return nil
}

// FlushPendingWB starts writeback for every block written since the last flush
// and waits for it to complete: sync_file_range with WAIT_BEFORE|WRITE|
// WAIT_AFTER over the coalesced contiguous runs.  This is the targeted
// equivalent of the kernel's per-buffer sync_dirty_buffer() — writeback
// complete, but without the device cache flush the old Sync()/Fdatasync()
// whole-device barriers issued.  The FUSE crash model is kill -9 (page cache
// lost, device cache intact), so writeback-complete is durable; the single
// remaining device flush per fsync and at unmount covers power-fail parity.
func (bd *BlockDevice) FlushPendingWB() error {
	blocks := bd.takePendingWB()
	if blocks == nil {
		return nil
	}
	return forEachWBRun(blocks, bd.syncWBRange)
}

// syncWBRange waits for writeback of blocks [first,last] (inclusive) to
// complete.  Platforms or files without sync_file_range fall back to a full
// device Sync, which flushes data too.
func (bd *BlockDevice) syncWBRange(first, last uint64) error {
	off := int64(first * bd.blockSize)
	length := int64((last - first + 1) * bd.blockSize)
	err := unix.SyncFileRange(int(bd.file.Fd()), off, length,
		unix.SYNC_FILE_RANGE_WAIT_BEFORE|unix.SYNC_FILE_RANGE_WRITE|unix.SYNC_FILE_RANGE_WAIT_AFTER)
	if err == nil {
		return nil
	}
	if err != syscall.ENOSYS && err != syscall.EINVAL {
		return fmt.Errorf("sync file range blocks [%d,%d]: %w", first, last, err)
	}
	if serr := bd.file.Sync(); serr != nil {
		return fmt.Errorf("sync file range (sync fallback): %w", serr)
	}
	return nil
}

// takePendingWB removes and returns the sorted snapshot of tracked blocks, or
// nil when nothing is tracked.
func (bd *BlockDevice) takePendingWB() []uint64 {
	bd.wbMu.Lock()
	defer bd.wbMu.Unlock()
	if len(bd.pendingWB) == 0 {
		return nil
	}
	blocks := make([]uint64, 0, len(bd.pendingWB))
	for b := range bd.pendingWB {
		blocks = append(blocks, b)
	}
	bd.pendingWB = make(map[uint64]struct{})
	sort.Slice(blocks, func(i, j int) bool { return blocks[i] < blocks[j] })
	return blocks
}

// FlushWB implements briefs.WBFlusher (the journal's targeted writeback-flush
// hook, set with SetWBFlusher in Mount).
func (bd *BlockDevice) FlushWB() error { return bd.FlushPendingWB() }

// KickWB implements briefs.WBFlusher's no-wait variant: start writeback of
// the tracked blocks (the journal's checkpoint path, kernel parity for the
// checkpoint never waiting for user data).
func (bd *BlockDevice) KickWB() error { return bd.kickPendingWB() }

// ReadAt implements io.ReaderAt, allowing briefs.ReadSuperblock and
// briefs.ReadAllocatorHeader to work with a BlockDevice.
func (bd *BlockDevice) ReadAt(p []byte, off int64) (int, error) {
	return bd.file.ReadAt(p, off)
}

// Close closes the underlying file.
func (bd *BlockDevice) Close() error {
	return bd.file.Close()
}
