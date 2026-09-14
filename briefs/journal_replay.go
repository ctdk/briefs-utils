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
	"errors"
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

// JournalRingGeometry returns the absolute first journal block and the
// number of blocks in the ring, for callers that drive WalkJournalRing with
// a Journal's reader.
func (j *Journal) JournalRingGeometry() (offset, blocks uint64) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.journalStart, j.journalEnd - j.journalStart
}

// NextJournalBlock advances a journal block number, wrapping at journalEnd.
func (j *Journal) NextJournalBlock(cur uint64) uint64 {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.nextBlock(cur)
}

// NextRingBlock advances a journal block number, wrapping at
// journalStart+journalBlocks. It is the geometry-free form of
// Journal.NextJournalBlock for callers (fsck) that hold the raw superblock
// geometry rather than a Journal instance.
func NextRingBlock(cur, journalStart, journalBlocks uint64) uint64 {
	next := cur + 1
	if next >= journalStart+journalBlocks {
		return journalStart
	}
	return next
}

// ErrJournalBadMagic reports a walked journal block whose magic is neither
// MagicJournal nor MagicCheckpoint. Callers decide whether that is fatal:
// replay treats it as the end of the live log (the torn tail of a crash),
// while fsck reports it as an error.
var ErrJournalBadMagic = errors.New("journal block has bad magic")

// ErrJournalRecordOverflow reports a journal record whose header+payload
// would extend past the end of its block; the record is not visited.
var ErrJournalRecordOverflow = errors.New("journal record overflows block")

// WalkJournalRing walks journal blocks around the ring starting at start and
// stopping when end is reached (end is not visited). When start == end the
// single block at start is visited exactly once -- fsck's clean-journal
// checkpoint check; replay callers skip that block in their visitor. The
// walk never visits more than journalBlocks blocks, so a corrupt log range
// cannot spin. readBlock fetches one block's bytes; visit runs for each
// block whose header carries a journal or checkpoint magic, and typically
// iterates the block's records with IterateJournalBlockRecords.
func WalkJournalRing(readBlock func(block uint64) ([]byte, error),
	journalOffset, journalBlocks, start, end uint64,
	visit func(cur uint64, buf []byte) error) error {

	cur := start
	for iter := uint64(0); iter < journalBlocks; iter++ {
		buf, err := readBlock(cur)
		if err != nil {
			return fmt.Errorf("read journal block %d: %w", cur, err)
		}
		bh := ParseJournalBlockHeader(buf)
		if bh.Magic != MagicJournal && bh.Magic != MagicCheckpoint {
			return fmt.Errorf("%w at block %d (0x%08X)", ErrJournalBadMagic, cur, bh.Magic)
		}
		if err := visit(cur, buf); err != nil {
			return err
		}
		if start == end {
			return nil
		}
		cur = NextRingBlock(cur, journalOffset, journalBlocks)
		if cur == end {
			return nil
		}
	}
	return nil
}

// IterateJournalBlockRecords walks the records of one journal block, handing
// each well-formed record's index, header and payload to visit; a non-nil
// return from visit stops the walk and is returned as-is. The first record
// whose header+payload would extend past the block ends the walk with
// ErrJournalRecordOverflow (wrapping the record index and data_len) -- the
// caller decides whether that is fatal.
func IterateJournalBlockRecords(buf []byte, blockSize uint64, recordCount uint32,
	visit func(idx uint32, hdr RecordHeader, payload []byte) error) error {

	off := uint64(JournalBlockHdrSize)
	for i := uint32(0); i < recordCount && off+JournalRecordHdrSize <= blockSize; i++ {
		hdr := ParseRecordHeader(buf[off:])
		dlen := uint64(hdr.DataLen)
		if off+JournalRecordHdrSize+dlen > blockSize {
			return fmt.Errorf("%w: record %d (data_len=%d)", ErrJournalRecordOverflow, i, hdr.DataLen)
		}
		payload := buf[off+JournalRecordHdrSize : off+JournalRecordHdrSize+dlen]
		if err := visit(i, hdr, payload); err != nil {
			return err
		}
		off += JournalRecordHdrSize + dlen
	}
	return nil
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
	j.blocksSinceCheckpoint = 0
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
//
//go:briefs-disk size=16
type RecordHeader struct {
	Type     uint32
	Flags    uint32
	DataLen  uint32
	Checksum uint32
}

// ParseRecordHeader reads a 16-byte record header from buf.
func ParseRecordHeader(buf []byte) RecordHeader {
	var h RecordHeader
	_ = h.UnmarshalBinary(buf)
	return h
}

// JournalBlockHeader is the 16-byte on-disk journal block header (struct
// journal_block_header): magic, block_seq, record_count, reserved.
//
//go:briefs-disk size=16
type JournalBlockHeader struct {
	Magic       uint32
	BlockSeq    uint32
	RecordCount uint32
	Reserved    uint32
}

// ParseJournalBlockHeader reads a 16-byte journal block header from buf.
func ParseJournalBlockHeader(buf []byte) JournalBlockHeader {
	var h JournalBlockHeader
	_ = h.UnmarshalBinary(buf)
	return h
}

// MarshalJournalBlockHeader writes a journal block header into the first
// JournalBlockHdrSize bytes of buf.
func MarshalJournalBlockHeader(buf []byte, h JournalBlockHeader) {
	data, _ := h.MarshalBinary()
	copy(buf[:JournalBlockHdrSize], data)
}
