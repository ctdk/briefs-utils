package briefs

import (
	"encoding/binary"
	"os"
	"testing"
)

// newJournalImage creates a minimal valid BrieFS image with a journal region
// and returns the open read-write file plus the superblock layout.  It does
// not write allocators (the journal test uses a nil AllocatorSyncer).
func newJournalImage(t *testing.T, totalBlocks, journalBlocks uint64) (*os.File, *SuperblockLayout) {
	t.Helper()
	f, err := os.CreateTemp("", "briefs-journal-*.img")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	t.Cleanup(func() { os.Remove(f.Name()) })

	sb, err := NewSuperblock(totalBlocks, 4096, 512, journalBlocks, "test", "")
	if err != nil {
		f.Close()
		t.Fatalf("NewSuperblock: %v", err)
	}
	if err := sb.Write(f.Name()); err != nil {
		f.Close()
		t.Fatalf("Superblock.Write: %v", err)
	}
	f.Close()

	rw, err := os.OpenFile(f.Name(), os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	t.Cleanup(func() { rw.Close() })
	return rw, &sb.Lay
}

func readBlock(t *testing.T, f *os.File, blockSize, block uint64) []byte {
	t.Helper()
	buf := make([]byte, blockSize)
	if _, err := f.ReadAt(buf, int64(block*blockSize)); err != nil {
		t.Fatalf("ReadAt block %d: %v", block, err)
	}
	return buf
}

func TestJournalWriteAndSync(t *testing.T) {
	const totalBlocks, journalBlocks = 4096, 64
	f, sb := newJournalImage(t, totalBlocks, journalBlocks)

	j, err := NewJournal(sb, f, 4096)
	if err != nil {
		t.Fatalf("NewJournal: %v", err)
	}

	// Write a handful of JRN_INODE_ALLOC records into the current block.
	for i := uint64(1); i <= 5; i++ {
		rec := &JrnInodeAlloc{Ino: i, Mode: 0o100644, Nlink: 1, Uid: 1000, Gid: 1000}
		if err := j.WriteRecord(JRN_INODE_ALLOC, rec.Marshal()); err != nil {
			t.Fatalf("WriteRecord %d: %v", i, err)
		}
	}
	if !j.Dirty() {
		t.Fatal("journal should be dirty after writes")
	}

	initialLogEnd := sb.JournalLogEnd
	if err := j.Sync(false); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if j.Dirty() {
		t.Fatal("journal should not be dirty after Sync")
	}

	// The first journal block (at JournalOffset) must hold a valid block
	// header with MagicJournal and 5 records, followed by the first record.
	blk := readBlock(t, f, 4096, sb.JournalOffset)
	if got := binary.LittleEndian.Uint32(blk[0:]); got != MagicJournal {
		t.Fatalf("block magic: want 0x%x, got 0x%x", MagicJournal, got)
	}
	if got := binary.LittleEndian.Uint32(blk[8:]); got != 5 {
		t.Fatalf("record_count: want 5, got %d", got)
	}

	// First record header: type == JRN_INODE_ALLOC, checksum matches the
	// kernel's compute_record_checksum over its own payload.
	recType := binary.LittleEndian.Uint32(blk[JournalBlockHdrSize+0:])
	if recType != JRN_INODE_ALLOC {
		t.Fatalf("record type: want %d, got %d", JRN_INODE_ALLOC, recType)
	}
	dataLen := binary.LittleEndian.Uint32(blk[JournalBlockHdrSize+8:])
	if dataLen != JrnInodeAllocSize {
		t.Fatalf("data_len: want %d, got %d", JrnInodeAllocSize, dataLen)
	}
	payload := blk[JournalBlockHdrSize+JournalRecordHdrSize : JournalBlockHdrSize+JournalRecordHdrSize+dataLen]
	stored := binary.LittleEndian.Uint32(blk[JournalBlockHdrSize+12:])
	if got := ComputeJournalRecordChecksum(recType, 0, payload); got != stored {
		t.Fatalf("checksum mismatch: want 0x%08x, got 0x%08x", got, stored)
	}

	// After Sync, the superblock's log_end must have advanced past the
	// written block (commit point persisted).  On a fresh mount where
	// write_pos == log_start and records are pending, the kernel (and this
	// port) fires a back-pressure checkpoint at sync start, so log_start
	// also advances to log_end — the durable invariant is that the commit
	// happened, i.e. log_end moved past its initial value.
	sb2 := &SuperblockLayout{}
	sbBuf := readBlock(t, f, 4096, 0)
	if err := sb2.UnmarshalBinary(sbBuf); err != nil {
		t.Fatalf("UnmarshalBinary sb: %v", err)
	}
	if sb2.JournalLogEnd <= initialLogEnd {
		t.Fatalf("log_end (%d) should be past initial log_end (%d) after Sync",
			sb2.JournalLogEnd, initialLogEnd)
	}
	if sb2.JournalLogStart != sb2.JournalLogEnd {
		t.Fatalf("after first-sync back-pressure checkpoint, log_start (%d) should equal log_end (%d)",
			sb2.JournalLogStart, sb2.JournalLogEnd)
	}
}

func TestJournalCheckpointClearsLog(t *testing.T) {
	const totalBlocks, journalBlocks = 4096, 64
	f, sb := newJournalImage(t, totalBlocks, journalBlocks)

	j, err := NewJournal(sb, f, 4096)
	if err != nil {
		t.Fatalf("NewJournal: %v", err)
	}
	oldSeq := sb.CheckpointSeq

	for i := uint64(1); i <= 3; i++ {
		rec := &JrnInodeAlloc{Ino: i, Mode: 0o100644, Nlink: 1}
		if err := j.WriteRecord(JRN_INODE_ALLOC, rec.Marshal()); err != nil {
			t.Fatalf("WriteRecord: %v", err)
		}
	}
	if err := j.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}

	// A checkpoint must leave log_start == log_end (nothing to replay on a
	// clean unmount) and bump checkpoint_seq.
	sb2 := &SuperblockLayout{}
	if err := sb2.UnmarshalBinary(readBlock(t, f, 4096, 0)); err != nil {
		t.Fatalf("UnmarshalBinary sb: %v", err)
	}
	if sb2.JournalLogStart != sb2.JournalLogEnd {
		t.Fatalf("checkpoint should clear log: start=%d end=%d",
			sb2.JournalLogStart, sb2.JournalLogEnd)
	}
	if sb2.CheckpointSeq <= oldSeq {
		t.Fatalf("checkpoint_seq: want > %d, got %d", oldSeq, sb2.CheckpointSeq)
	}

	// The checkpoint block (last journal block) must carry MagicCheckpoint.
	cpBlk := readBlock(t, f, 4096, sb.JournalOffset+journalBlocks-1)
	if got := binary.LittleEndian.Uint32(cpBlk[0:]); got != MagicCheckpoint {
		t.Fatalf("checkpoint block magic: want 0x%x, got 0x%x", MagicCheckpoint, got)
	}
	// Its first record must be JRN_CHECKPOINT with a 56-byte payload.
	if got := binary.LittleEndian.Uint32(cpBlk[JournalBlockHdrSize+0:]); got != JRN_CHECKPOINT {
		t.Fatalf("checkpoint record type: want %d, got %d", JRN_CHECKPOINT, got)
	}
	if got := binary.LittleEndian.Uint32(cpBlk[JournalBlockHdrSize+8:]); got != CheckpointSize {
		t.Fatalf("checkpoint data_len: want %d, got %d", CheckpointSize, got)
	}
}

func TestJournalRingWrapSkipsCheckpointBlock(t *testing.T) {
	// A 4-block journal: blocks [0,1,2,3]; block 3 is the checkpoint block,
	// so only [0,1,2] are usable for records.  Writing enough records to wrap
	// must skip block 3 and never land a record there.
	const totalBlocks, journalBlocks = 4096, 4
	f, sb := newJournalImage(t, totalBlocks, journalBlocks)

	j, err := NewJournal(sb, f, 4096)
	if err != nil {
		t.Fatalf("NewJournal: %v", err)
	}

	// Each JRN_INODE_ALLOC is 40B + 16B hdr = 56B; a 4096B block holds ~73
	// records.  Write far more than 3 blocks' worth to force a wrap; the
	// back-pressure checkpoint will fire when the ring fills.  The point of
	// this test is that it does not error and the checkpoint block is never
	// overwritten with a JOURNAL_MAGIC record.
	for i := uint64(1); i <= 500; i++ {
		rec := &JrnInodeAlloc{Ino: i, Mode: 0o100644, Nlink: 1}
		if err := j.WriteRecord(JRN_INODE_ALLOC, rec.Marshal()); err != nil {
			t.Fatalf("WriteRecord %d: %v", i, err)
		}
	}
	if err := j.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}

	cpBlk := readBlock(t, f, 4096, sb.JournalOffset+journalBlocks-1)
	if got := binary.LittleEndian.Uint32(cpBlk[0:]); got != MagicCheckpoint {
		t.Fatalf("checkpoint block magic after wrap: want 0x%x, got 0x%x",
			MagicCheckpoint, got)
	}
}

// TestWriteCheckpointBlockLayout asserts the shared checkpoint-block framer
// produces the exact on-disk layout the hand-rolled writers it replaced used:
// a 16-byte journal block header, a 16-byte JRN_CHECKPOINT record header with a
// validating CRC32C, then the 56-byte Checkpoint payload. This guards the
// journal/fsck/mkfs checkpoint path against framing drift.
func TestWriteCheckpointBlockLayout(t *testing.T) {
	cp := &Checkpoint{
		Seq:            42,
		RecordCount:    7,
		LogSequenceEnd: 1234,
		TrieRootNode:   56,
		FreeDataCount:  100,
		FreeInodeCount: 200,
	}
	cpBytes, err := cp.MarshalBinary()
	if err != nil {
		t.Fatalf("Checkpoint.MarshalBinary: %v", err)
	}
	if len(cpBytes) != CheckpointSize {
		t.Fatalf("Checkpoint payload size: want %d, got %d", CheckpointSize, len(cpBytes))
	}

	const blockSeq uint32 = 9
	buf := make([]byte, 4096)
	if err := WriteCheckpointBlock(buf, blockSeq, cp); err != nil {
		t.Fatalf("WriteCheckpointBlock: %v", err)
	}

	// Block header.
	bh := ParseJournalBlockHeader(buf)
	if bh.Magic != MagicCheckpoint {
		t.Errorf("block magic: want 0x%x, got 0x%x", MagicCheckpoint, bh.Magic)
	}
	if bh.BlockSeq != blockSeq {
		t.Errorf("block seq: want %d, got %d", blockSeq, bh.BlockSeq)
	}
	if bh.RecordCount != 1 {
		t.Errorf("block record_count: want 1, got %d", bh.RecordCount)
	}

	// Record header at offset JournalBlockHdrSize.
	recOff := uint64(JournalBlockHdrSize)
	rh := ParseRecordHeader(buf[recOff:])
	if rh.Type != JRN_CHECKPOINT {
		t.Errorf("record type: want %d, got %d", JRN_CHECKPOINT, rh.Type)
	}
	if rh.Flags != JRN_FLAGS_NONE {
		t.Errorf("record flags: want 0, got 0x%x", rh.Flags)
	}
	if rh.DataLen != uint32(CheckpointSize) {
		t.Errorf("record data_len: want %d, got %d", CheckpointSize, rh.DataLen)
	}
	wantCksum := ComputeJournalRecordChecksum(JRN_CHECKPOINT, JRN_FLAGS_NONE, cpBytes)
	if rh.Checksum != wantCksum {
		t.Errorf("record checksum: want 0x%x, got 0x%x", wantCksum, rh.Checksum)
	}
	if !VerifyJournalRecordChecksum(JRN_CHECKPOINT, rh.Flags, cpBytes, rh.Checksum) {
		t.Error("VerifyJournalRecordChecksum rejected the checkpoint record")
	}

	// Payload immediately after the record header must equal the marshaled
	// Checkpoint.
	gotPayload := buf[recOff+JournalRecordHdrSize : recOff+JournalRecordHdrSize+CheckpointSize]
	if string(gotPayload) != string(cpBytes) {
		t.Error("checkpoint payload bytes differ from Checkpoint.MarshalBinary")
	}

	// BlockSeq=0 path (fsck/mkfs) must differ only in the block-seq field.
	buf0 := make([]byte, 4096)
	if err := WriteCheckpointBlock(buf0, 0, cp); err != nil {
		t.Fatalf("WriteCheckpointBlock(0): %v", err)
	}
	bh0 := ParseJournalBlockHeader(buf0)
	if bh0.BlockSeq != 0 {
		t.Errorf("block seq(0): want 0, got %d", bh0.BlockSeq)
	}
	// Record header and payload must be identical between the two seqs.
	if string(buf0[recOff:recOff+JournalRecordHdrSize+CheckpointSize]) !=
		string(buf[recOff:recOff+JournalRecordHdrSize+CheckpointSize]) {
		t.Error("record header/payload changed with block_seq (should not)")
	}
}