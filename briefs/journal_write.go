// Package briefs: journal write path.
//
// This is a Go port of the kernel journal.c write/sync/checkpoint paths
// (briefs_journal_init, __briefs_journal_write_record_locked,
// __briefs_journal_sync_locked, __briefs_journal_checkpoint_locked,
// briefs_journal_sync_superblock).  It produces journal records byte-for-byte
// compatible with the kernel's on-disk format so a volume written by the FUSE
// bridge can be mounted and replayed by the kernel module after a crash.
//
// Durability model: the FUSE bridge has no buffer cache, so the kernel's
// "mark_buffer_dirty + lazy flush" becomes direct file.WriteAt into the
// backing file's page cache, and "sync_blockdev" becomes file.Sync()
// (fdatasync of the whole backing file).  The commit-before-flush ordering is
// preserved: a journal block is written and log_end advanced + the superblock
// persisted BEFORE the metadata referenced by the records is flushed, so
// replay can re-derive any metadata that did not reach disk.

package briefs

import (
	"encoding/binary"
	"fmt"
	"os"
	"sync"
)

// Journal ring geometry bounds (match briefs_journal.h).
const (
	JournalBlockSize      = 4096
	JournalMinBlocks      = 4
	JournalMaxBlocks      = 65536
	JrnCheckpointInterval = 4096
)

// AllocatorSyncer is implemented by the FUSE bridge (and later fsck) to give
// the journal access to the in-memory allocators without creating an import
// cycle (briefs cannot import fuse).  It is optional: a nil syncer leaves the
// superblock free counts unchanged and skips the bitmap flush, which is fine
// for pure-writer tests that do not allocate.
type AllocatorSyncer interface {
	// RefreshFreeCounts updates the in-memory superblock's FreeDataBlks and
	// FreeInodes from the authoritative allocator free counts.  Cheap; no
	// disk I/O.  Called before persisting the superblock in Sync and
	// Checkpoint, mirroring briefs_journal_sync_superblock().
	RefreshFreeCounts()

	// SyncAllocators writes both (data and inode) allocator bitmap pools to
	// disk.  Expensive; called only at checkpoint, mirroring the kernel's
	// briefs_alloc_sync() pair.  Without this, a back-pressure checkpoint
	// mid-burst advances log_start past the JRN_TRIE_ALLOC/JRN_INODE_ALLOC
	// records while the on-disk bitmap still fails to mark those blocks
	// allocated (generic/040/041 hardlink-storm family).
	SyncAllocators() error
}

// Journal mirrors struct briefs_journal (briefs_journal.h:24).  All ring
// state is protected by mu (j->write_lock).
type Journal struct {
	mu sync.Mutex

	sb        *SuperblockLayout
	file      *os.File
	blockSize uint64

	// Ring buffer state.
	curBlock     []byte // current 4096-byte journal block under construction
	blockSeq     uint32 // monotonic per-block sequence (cur_hdr->block_seq)
	writeOffset  uint64 // byte offset of next record within curBlock
	writePos     uint64 // journal block number curBlock will be written to
	syncedPos    uint64 // journal blocks before this are durably on disk
	journalStart uint64
	journalEnd   uint64
	checkpointBlk uint64
	checkpointSeq uint64
	recordsSinceCheckpoint uint32
	dirty        bool
	inCheckpoint bool

	// inReplay is set during journal replay at mount. While set, WriteRecord
	// is a no-op: replay re-derives metadata from existing records and must
	// not append fresh records (e.g. the JRN_TRIE_ALLOC that trie_page_init
	// would otherwise log), which would advance write_pos into the range
	// still being replayed and clobber unprocessed records. Mirrors the
	// kernel's j->in_replay gating in briefs_trie_page_init/trie_free_node.
	inReplay bool

	allocSyncer AllocatorSyncer
}

// NewJournal initializes a Journal from an already-loaded superblock and an
// open, read-write device file.  It reads journal geometry from the
// superblock; it does NOT replay (replay is the kernel's / fsck's job).  The
// caller owns sb and file; the Journal mutates sb in place and writes through
// file.
func NewJournal(sb *SuperblockLayout, file *os.File, blockSize uint64) (*Journal, error) {
	if sb == nil || file == nil {
		return nil, fmt.Errorf("briefs: new journal: nil sb or file")
	}
	if blockSize == 0 {
		blockSize = JournalBlockSize
	}
	j := &Journal{
		sb:          sb,
		file:        file,
		blockSize:   blockSize,
		journalStart: sb.JournalOffset,
		journalEnd:   sb.JournalOffset + sb.JournalBlocks,
		checkpointBlk: sb.JournalOffset + sb.JournalBlocks - 1,
		checkpointSeq: sb.CheckpointSeq,
		writePos:    sb.JournalLogStart,
		syncedPos:   sb.JournalLogStart,
		curBlock:    make([]byte, JournalBlockSize),
	}
	if err := j.validate(); err != nil {
		return nil, err
	}
	j.initCurBlock(0)
	return j, nil
}

// SetAllocatorSyncer wires the allocator interface after construction (the
// FUSE bridge builds the Journal and allocators separately during Mount).
func (j *Journal) SetAllocatorSyncer(s AllocatorSyncer) { j.allocSyncer = s }

func (j *Journal) validate() error {
	if j.journalEnd <= j.journalStart {
		return fmt.Errorf("briefs: invalid journal geometry [start=%d end=%d)",
			j.journalStart, j.journalEnd)
	}
	n := j.journalEnd - j.journalStart
	if n < JournalMinBlocks {
		return fmt.Errorf("briefs: journal too small (%d blocks, min %d)", n, JournalMinBlocks)
	}
	if n > JournalMaxBlocks {
		return fmt.Errorf("briefs: journal too large (%d blocks, max %d)", n, JournalMaxBlocks)
	}
	return nil
}

// initCurBlock resets curBlock to a fresh journal block header with the given
// sequence number.
func (j *Journal) initCurBlock(seq uint32) {
	for i := range j.curBlock {
		j.curBlock[i] = 0
	}
	binary.LittleEndian.PutUint32(j.curBlock[0:], MagicJournal) // magic
	binary.LittleEndian.PutUint32(j.curBlock[4:], seq)          // block_seq
	// record_count at 8 = 0, reserved at 12 = 0
	j.blockSeq = seq
	j.writeOffset = JournalBlockHdrSize
}

// nextBlock advances a journal block number, wrapping at journalEnd.
func (j *Journal) nextBlock(cur uint64) uint64 {
	next := cur + 1
	if next >= j.journalEnd {
		return j.journalStart
	}
	return next
}

// writeBlock writes a 4096-byte block at the given journal block number
// (absolute).  The write goes into the backing file's page cache; durability
// is provided by the caller's subsequent Sync/Checkpoint.
func (j *Journal) writeBlock(block uint64, data []byte) error {
	if block < j.journalStart || block >= j.journalEnd {
		return fmt.Errorf("briefs: journal write out of range (block=%d, [%d,%d))",
			block, j.journalStart, j.journalEnd)
	}
	off := int64(block * j.blockSize)
	if _, err := j.file.WriteAt(data, off); err != nil {
		return fmt.Errorf("briefs: write journal block %d: %w", block, err)
	}
	return nil
}

// writeRecordLocked appends a record to curBlock, flushing and advancing the
// ring when it does not fit.  Caller must hold j.mu.  Mirrors
// __briefs_journal_write_record_locked (journal.c:226).
func (j *Journal) writeRecordLocked(typ uint32, data []byte) error {
	if typ <= JRN_NONE || typ >= JRN_END {
		return fmt.Errorf("briefs: invalid journal record type %d", typ)
	}
	if len(data) == 0 {
		return fmt.Errorf("briefs: empty journal record data")
	}
	total := uint32(JournalRecordHdrSize) + uint32(len(data))

	if j.writeOffset+uint64(total) > JournalBlockSize {
		// Flush current block, advance write position.
		if err := j.writeBlock(j.writePos, j.curBlock); err != nil {
			return err
		}
		j.writePos = j.nextBlock(j.writePos)
		// Never clobber the checkpoint block.
		if j.writePos == j.checkpointBlk {
			j.writePos = j.nextBlock(j.writePos)
		}
		j.initCurBlock(j.blockSeq + 1)
		// curBlock is now empty; the flushed block lives only in the page
		// cache until the next Sync.  Mirror the kernel's post-flush state so
		// the back-pressure checkpoint below sees nothing pending.
		j.dirty = false

		// Back-pressure: ring completely full.  write_pos == log_start only
		// holds when the ring has wrapped full (an empty ring has
		// next_block(write_pos) != write_pos).  Force a checkpoint to retire
		// the tail instead of overwriting live records.
		if j.writePos == j.sb.JournalLogStart {
			if err := j.checkpointLocked(); err != nil {
				return fmt.Errorf("briefs: back-pressure checkpoint: %w", err)
			}
			// A ring with >=2 usable blocks is now clear.  A degenerate
			// 1-block ring stays collidable; refuse rather than corrupt.
			if j.nextBlock(j.writePos) == j.sb.JournalLogStart {
				return fmt.Errorf("briefs: journal ring exhausted (size=%d blocks)",
					j.journalEnd-j.journalStart)
			}
		}
	}

	// Append record header + data.
	off := j.writeOffset
	binary.LittleEndian.PutUint32(j.curBlock[off+0:], typ)
	binary.LittleEndian.PutUint32(j.curBlock[off+4:], 0) // flags
	binary.LittleEndian.PutUint32(j.curBlock[off+8:], uint32(len(data)))
	binary.LittleEndian.PutUint32(j.curBlock[off+12:],
		ComputeJournalRecordChecksum(typ, 0, data))
	copy(j.curBlock[off+JournalRecordHdrSize:], data)

	j.writeOffset += uint64(total)
	// Increment the block's record_count.
	rc := binary.LittleEndian.Uint32(j.curBlock[8:])
	binary.LittleEndian.PutUint32(j.curBlock[8:], rc+1)
	j.dirty = true
	j.recordsSinceCheckpoint++
	return nil
}

// WriteRecord is the public entry: it serializes writers on j.mu then appends
// the record.  Mirrors briefs_journal_write_record (journal.c:347).
func (j *Journal) WriteRecord(typ uint32, data []byte) error {
	if j == nil {
		return fmt.Errorf("briefs: write record on nil journal")
	}
	// Replay re-derives metadata from existing records; it must not append
	// new ones (see the inReplay field comment).
	if j.inReplay {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.writeRecordLocked(typ, data)
}

// Sync flushes the current journal block to disk, commits log_end in the
// superblock, then flushes metadata.  If checkpoint is true and enough records
// have accumulated, a periodic checkpoint follows.  Mirrors
// __briefs_journal_sync_locked (journal.c:2048) with commit-before-flush.
func (j *Journal) Sync(checkpoint bool) error {
	if j == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.syncLocked(checkpoint)
}

func (j *Journal) syncLocked(checkpoint bool) error {
	if !j.dirty {
		return nil
	}

	// Back-pressure: if the current block is also the oldest uncheckpointed
	// block, retire it first (journal.c:2063).
	if !j.inCheckpoint && j.writePos == j.sb.JournalLogStart {
		j.inCheckpoint = true
		err := j.checkpointLocked()
		j.inCheckpoint = false
		if err != nil {
			return err
		}
		if !j.dirty {
			return nil
		}
	}

	// Write the current block and advance.
	if err := j.writeBlock(j.writePos, j.curBlock); err != nil {
		return err
	}
	j.writePos = j.nextBlock(j.writePos)
	if j.writePos == j.checkpointBlk {
		j.writePos = j.nextBlock(j.writePos)
	}
	j.initCurBlock(j.blockSeq + 1)
	j.dirty = false

	// Commit point: advance log_end and persist the superblock BEFORE the
	// metadata flush.  Replay is idempotent, so committing before the flush
	// is safe (journal.c:2105-2118).
	j.sb.JournalLogEnd = j.writePos
	if err := j.syncSuperblock(); err != nil {
		return fmt.Errorf("briefs: persist journal tail: %w", err)
	}

	// Metadata flush (= sync_blockdev): the FUSE bridge has no buffer cache,
	// so the handler's metadata WriteAt calls are already in the page cache;
	// a single fdatasync flushes both them and the journal block above.
	if err := j.file.Sync(); err != nil {
		return fmt.Errorf("briefs: metadata flush: %w", err)
	}
	j.syncedPos = j.writePos

	// Periodic checkpoint (journal.c:2189).
	if checkpoint && j.recordsSinceCheckpoint >= JrnCheckpointInterval {
		if err := j.checkpointLocked(); err != nil {
			return fmt.Errorf("briefs: periodic checkpoint: %w", err)
		}
	}
	// Back-pressure after advance (journal.c:2202).
	if !j.inCheckpoint && j.nextBlock(j.writePos) == j.sb.JournalLogStart {
		j.inCheckpoint = true
		err := j.checkpointLocked()
		j.inCheckpoint = false
		if err != nil {
			return fmt.Errorf("briefs: back-pressure checkpoint: %w", err)
		}
	}
	return nil
}

// Checkpoint flushes all pending records, syncs the allocator bitmaps, writes
// a JRN_CHECKPOINT record, and advances log_start to write_pos so the retired
// records are no longer needed for replay.  Mirrors
// __briefs_journal_checkpoint_locked (journal.c:366).
func (j *Journal) Checkpoint() error {
	if j == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.checkpointLocked()
}

func (j *Journal) checkpointLocked() error {
	// Flush any pending records first (journal.c:374).
	if j.dirty {
		if err := j.syncLocked(true); err != nil {
			return err
		}
	}

	// Flush all dirty metadata buffers before discarding the journal records
	// that reference them, then persist the allocator bitmaps (journal.c:394).
	// The kernel releases write_lock here for concurrency and to avoid an
	// AB-BA with alloc->lock.  The FUSE bridge holds the global fs mutex
	// across each op, so the journal mu is uncontended; we keep it held
	// (the release/re-acquire would be a no-op for correctness).
	if err := j.file.Sync(); err != nil {
		return fmt.Errorf("briefs: checkpoint metadata flush: %w", err)
	}
	if j.allocSyncer != nil {
		if err := j.allocSyncer.SyncAllocators(); err != nil {
			return fmt.Errorf("briefs: sync allocators: %w", err)
		}
	}

	// Refresh superblock free counts from the authoritative allocators
	// (journal.c:442).
	if j.allocSyncer != nil {
		j.allocSyncer.RefreshFreeCounts()
	}

	// Build the checkpoint record in a fresh block.
	cpBuf := make([]byte, JournalBlockSize)
	binary.LittleEndian.PutUint32(cpBuf[0:], MagicCheckpoint) // header magic
	binary.LittleEndian.PutUint32(cpBuf[4:], j.blockSeq)      // block_seq
	binary.LittleEndian.PutUint32(cpBuf[8:], 1)               // record_count = 1

	cp := &Checkpoint{
		Seq:            j.checkpointSeq + 1,
		RecordCount:    binary.LittleEndian.Uint32(j.curBlock[8:]),
		LogSequenceEnd: j.writePos,
		TrieRootNode:   j.sb.TrieRootBlock,
		FreeDataCount:  j.sb.FreeDataBlks,
		FreeInodeCount: j.sb.FreeInodes,
	}
	cpBytes, _ := cp.MarshalBinary()

	recOff := uint64(JournalBlockHdrSize)
	binary.LittleEndian.PutUint32(cpBuf[recOff+0:], JRN_CHECKPOINT)
	binary.LittleEndian.PutUint32(cpBuf[recOff+4:], 0)
	binary.LittleEndian.PutUint32(cpBuf[recOff+8:], uint32(len(cpBytes)))
	binary.LittleEndian.PutUint32(cpBuf[recOff+12:],
		ComputeJournalRecordChecksum(JRN_CHECKPOINT, 0, cpBytes))
	copy(cpBuf[recOff+JournalRecordHdrSize:], cpBytes)

	if err := j.writeBlock(j.checkpointBlk, cpBuf); err != nil {
		return fmt.Errorf("briefs: write checkpoint block: %w", err)
	}

	j.checkpointSeq = cp.Seq
	j.sb.CheckpointSeq = j.checkpointSeq
	j.sb.JournalLogStart = j.writePos
	j.sb.JournalLogEnd = j.writePos
	j.dirty = false
	j.recordsSinceCheckpoint = 0

	// Persist the updated superblock (free counts + new log boundaries).
	if err := j.file.Sync(); err != nil {
		return fmt.Errorf("briefs: checkpoint block flush: %w", err)
	}
	return j.syncSuperblock()
}

// syncSuperblock persists the in-memory superblock back to block 0, after
// refreshing free counts from the allocators.  Mirrors
// briefs_journal_sync_superblock (journal.c:799).
func (j *Journal) syncSuperblock() error {
	if j.allocSyncer != nil {
		j.allocSyncer.RefreshFreeCounts()
	}
	buf := make([]byte, j.blockSize)
	data, err := j.sb.MarshalBinary()
	if err != nil {
		return fmt.Errorf("briefs: marshal superblock: %w", err)
	}
	copy(buf, data)
	if _, err := j.file.WriteAt(buf, 0); err != nil {
		return fmt.Errorf("briefs: write superblock: %w", err)
	}
	return j.file.Sync()
}

// Close flushes any pending records.  Callers that want a clean unmount
// (log_start == log_end, nothing to replay on next mount) must call
// Checkpoint before Close — mirroring the kernel's put_super
// always-checkpoint-at-unmount (commit f8ef293).
func (j *Journal) Close() error {
	if j == nil {
		return nil
	}
	if j.dirty {
		if err := j.Sync(false); err != nil {
			return err
		}
	}
	return nil
}

// Dirty reports whether the journal has uncommitted records.  Test helper.
func (j *Journal) Dirty() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.dirty
}