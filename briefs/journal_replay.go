// Package briefs: journal replay support.
//
// These exported helpers let the FUSE bridge (package fuse) walk and replay
// the on-disk journal at mount, mirroring the kernel's briefs_journal_replay()
// (journal.c).  The Journal owns the ring geometry and the backing file; the
// bridge owns the allocators, inode manager, and trie/xattr/symlink code, so
// the orchestration lives in the bridge while these methods provide the
// ring-level primitives the bridge needs.
//
// The replay is byte-for-byte compatible with the kernel's on-disk format, so
// a volume written by the FUSE bridge replays identically under the kernel
// module and vice versa.

package briefs

import (
	"encoding/binary"
	"fmt"
)

// SetInReplay toggles replay mode. While true, WriteRecord is a no-op so the
// re-derivation paths (trie_page_init etc.) do not append records into the
// range being replayed.
func (j *Journal) SetInReplay(b bool) {
	if j == nil {
		return
	}
	j.mu.Lock()
	j.inReplay = b
	j.mu.Unlock()
}

// ReplayLogRange returns the live record range [start, end) and the reserved
// checkpoint block, as the bridge needs them to walk the journal.
func (j *Journal) ReplayLogRange() (start, end, checkpointBlk uint64) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.sb.JournalLogStart, j.sb.JournalLogEnd, j.checkpointBlk
}

// JournalBlockSize returns the journal block size (4096).
func (j *Journal) JournalBlockSize() uint64 {
	return j.blockSize
}

// NextJournalBlock advances a journal block number, wrapping at journalEnd.
func (j *Journal) NextJournalBlock(cur uint64) uint64 {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.nextBlock(cur)
}

// ReadJournalBlock reads a 4096-byte journal block at the given absolute block
// number. It range-checks against [journalStart, journalEnd). Returns the raw
// block (a fresh copy the caller may inspect).
func (j *Journal) ReadJournalBlock(block uint64) ([]byte, error) {
	if j == nil {
		return nil, fmt.Errorf("briefs: nil journal")
	}
	j.mu.Lock()
	if block < j.journalStart || block >= j.journalEnd {
		j.mu.Unlock()
		return nil, fmt.Errorf("briefs: journal read out of range (block=%d, [%d,%d))",
			block, j.journalStart, j.journalEnd)
	}
	off := int64(block * j.blockSize)
	j.mu.Unlock()

	buf := make([]byte, j.blockSize)
	n, err := j.file.ReadAt(buf, off)
	if err != nil {
		return nil, fmt.Errorf("briefs: read journal block %d: %w", block, err)
	}
	if n != int(j.blockSize) {
		return nil, fmt.Errorf("briefs: short read journal block %d (%d/%d)", block, n, j.blockSize)
	}
	return buf, nil
}

// MarkCleanAfterReplay advances log_start to log_end (nothing left to replay),
// bumps the checkpoint sequence, re-points the write cursors at the cleared
// tail, refreshes the superblock free counts from the allocators, and persists
// the superblock. Mirrors the kernel's post-replay cleanup (journal.c:1784+).
// The caller must have already synced the allocator bitmaps and flushed
// replay-dirty metadata to disk.
func (j *Journal) MarkCleanAfterReplay() error {
	if j == nil {
		return fmt.Errorf("briefs: nil journal")
	}
	j.mu.Lock()
	defer j.mu.Unlock()

	j.sb.JournalLogStart = j.sb.JournalLogEnd
	j.sb.CheckpointSeq = j.checkpointSeq + 1
	j.writePos = j.sb.JournalLogEnd
	j.syncedPos = j.writePos
	j.dirty = false
	j.recordsSinceCheckpoint = 0
	j.checkpointSeq = j.sb.CheckpointSeq

	// Refresh free counts from the authoritative allocators before persisting.
	if j.allocSyncer != nil {
		j.allocSyncer.RefreshFreeCounts()
	}
	return j.syncSuperblock()
}

// VerifyJournalRecordChecksum returns true if the record header's checksum
// field matches a recomputed CRC over type+flags+data_len+data. A zero
// checksum field means "legacy/no checksum" and is treated as valid (the
// caller decides whether to warn).
func VerifyJournalRecordChecksum(typ, flags uint32, data []byte, stored uint32) bool {
	if stored == 0 {
		return true
	}
	return ComputeJournalRecordChecksum(typ, flags, data) == stored
}

// RecordHeader is the 16-byte on-disk journal record header.
type RecordHeader struct {
	Type     uint32
	Flags    uint32
	DataLen  uint32
	Checksum uint32
}

// ParseRecordHeader reads a 16-byte record header from buf.
func ParseRecordHeader(buf []byte) RecordHeader {
	return RecordHeader{
		Type:     binary.LittleEndian.Uint32(buf[0:]),
		Flags:    binary.LittleEndian.Uint32(buf[4:]),
		DataLen:  binary.LittleEndian.Uint32(buf[8:]),
		Checksum: binary.LittleEndian.Uint32(buf[12:]),
	}
}