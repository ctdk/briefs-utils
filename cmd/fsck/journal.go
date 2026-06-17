package main

import (
	"encoding/binary"
	"fmt"
	"os"

	"github.com/ctdk/briefs-utils/types"
)

// readJournalMagic reads the first 4 bytes of the given journal block and
// returns the magic value, or an error if the block cannot be read.
func readJournalMagic(file *os.File, block, blockSize uint64) (uint32, error) {
	buf := make([]byte, blockSize)
	if _, err := file.ReadAt(buf, int64(block*blockSize)); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(buf[0:]), nil
}

// verifyJournal checks the journal checkpoint block and detects dirty
// filesystems (un-replayed journal records). It first checks the last journal
// block (where the kernel and current mkfs write the checkpoint). If that is
// not a valid checkpoint, it falls back to the first journal block for
// compatibility with older mkfs.briefs images.
func verifyJournal(file *os.File, journalOffset, journalBlocks, checkpointSeq, logStart, logEnd uint64, blockSize uint64) error {
	checkpointBlock := journalOffset + journalBlocks - 1
	magic, err := readJournalMagic(file, checkpointBlock, blockSize)
	if err != nil {
		return fmt.Errorf("read journal checkpoint block %d: %w", checkpointBlock, err)
	}
	if magic != types.MagicJournal && magic != types.MagicCheckpoint {
		// Fallback: older mkfs.briefs wrote the initial checkpoint to the
		// first journal block with JOURNAL_MAGIC.
		fallbackMagic, fallbackErr := readJournalMagic(file, journalOffset, blockSize)
		if fallbackErr != nil {
			return fmt.Errorf("read fallback journal block %d: %w", journalOffset, fallbackErr)
		}
		if fallbackMagic != types.MagicJournal && fallbackMagic != types.MagicCheckpoint {
			return fmt.Errorf("bad journal magic at checkpoint block %d (0x%08X) and fallback block %d (0x%08X)",
				checkpointBlock, magic, journalOffset, fallbackMagic)
		}
	}

	// Check if the filesystem is dirty (has un-replayed journal records).
	// The journal uses a log-structured layout where logStart/logEnd track
	// the range of blocks in use. If logStart != logEnd or checkpointSeq
	// is low, there may be un-replayed entries.
	if logStart != logEnd {
		logRange := logEnd
		if logEnd >= logStart {
			logRange = logEnd - logStart
		} else {
			// Wrapped around
			logRange = logEnd + journalBlocks - logStart
		}
		return fmt.Errorf("filesystem has un-replayed journal records (log range %d blocks, checkpoint seq %d)\n      journal replay required before fsck",
			logRange, checkpointSeq)
	}

	return nil
}

// nextJournalBlock returns the next block in the circular journal.
func nextJournalBlock(block, journalOffset, journalBlocks uint64) uint64 {
	next := block + 1
	if next >= journalOffset+journalBlocks {
		return journalOffset
	}
	return next
}

// verifyJournalRecords verifies the CRC32C checksum of journal records.
// For a clean filesystem only the checkpoint block is checked. For a dirty
// filesystem the recorded log range is walked. A zero checksum is treated
// as a legacy record and is allowed with a single warning.
func verifyJournalRecords(fs *fsckState, journalOffset, journalBlocks, logStart, logEnd, blockSize uint64) {
	legacyWarned := false
	badRecords := 0
	recordsChecked := 0

	start := logStart
	end := logEnd
	fallbackClean := false
	if logStart == logEnd {
		// Clean journal: only the checkpoint block should contain records.
		start = journalOffset + journalBlocks - 1
		end = start
	}

	cur := start
	for {
		buf := make([]byte, blockSize)
		if _, err := fs.file.ReadAt(buf, int64(cur*blockSize)); err != nil {
			fs.errorf("journal block %d: read error: %v", cur, err)
			break
		}

		magic := binary.LittleEndian.Uint32(buf[0:])
		if magic != types.MagicJournal && magic != types.MagicCheckpoint {
			if logStart == logEnd && !fallbackClean && cur == journalOffset+journalBlocks-1 {
				// Old mkfs.briefs images have the initial checkpoint in the
				// first journal block. Try that as a fallback once.
				fallbackClean = true
				cur = journalOffset
				continue
			}
			fs.errorf("journal block %d: bad magic 0x%08X", cur, magic)
			break
		}

		recordCount := binary.LittleEndian.Uint32(buf[8:])
		recOff := uint64(16)
		for i := uint32(0); i < recordCount && recOff+16 <= blockSize; i++ {
			recType := binary.LittleEndian.Uint32(buf[recOff:])
			recFlags := binary.LittleEndian.Uint32(buf[recOff+4:])
			dataLen := binary.LittleEndian.Uint32(buf[recOff+8:])
			storedChecksum := binary.LittleEndian.Uint32(buf[recOff+12:])

			if recOff+16+uint64(dataLen) > blockSize {
				fs.errorf("journal block %d record %d: record overflows block (data_len=%d)",
					cur, i, dataLen)
				badRecords++
				break
			}

			recordsChecked++
			recData := buf[recOff+16 : recOff+16+uint64(dataLen)]
			if storedChecksum == 0 {
				if !legacyWarned {
					fs.warnf("journal: legacy record with no checksum at block %d record %d; skipping CRC verification",
						cur, i)
					legacyWarned = true
				}
			} else {
				computed := types.ComputeJournalRecordChecksum(recType, recFlags, recData)
				if computed != storedChecksum {
					fs.errorf("journal block %d record %d: checksum mismatch (stored=0x%08X computed=0x%08X)",
						cur, i, storedChecksum, computed)
					badRecords++
				}
			}

			// Parse and validate checkpoint payloads when the checksum is good.
			if recType == uint32(types.JRN_CHECKPOINT) {
				if dataLen != types.CheckpointSize {
					fs.warnf("journal block %d record %d: checkpoint payload has unexpected length %d (want %d)",
						cur, i, dataLen, types.CheckpointSize)
				} else {
					var cp types.Checkpoint
					if err := cp.UnmarshalBinary(recData); err != nil {
						fs.warnf("journal block %d record %d: checkpoint parse error: %v", cur, i, err)
					} else {
						fmt.Fprintf(os.Stderr, "  checkpoint:    seq=%d records=%d log_end=%d free_data=%d free_inodes=%d\n",
							cp.Seq, cp.RecordCount, cp.LogSequenceEnd, cp.FreeDataCount, cp.FreeInodeCount)

						if fs.sb != nil {
							if cp.Seq != fs.sb.CheckpointSeq {
								fs.warnf("checkpoint seq mismatch: payload=%d, superblock=%d",
									cp.Seq, fs.sb.CheckpointSeq)
							}
							if cp.LogSequenceEnd != fs.sb.JournalLogEnd {
								fs.warnf("checkpoint log_sequence_end mismatch: payload=%d, superblock=%d",
									cp.LogSequenceEnd, fs.sb.JournalLogEnd)
							}
							if cp.FreeDataCount > fs.sb.TotalBlocks {
								fs.warnf("checkpoint free_data_count out of range: %d > total_blocks %d",
									cp.FreeDataCount, fs.sb.TotalBlocks)
							}
							if cp.FreeInodeCount > fs.sb.TotalBlocks {
								fs.warnf("checkpoint free_inode_count out of range: %d > total_blocks %d",
									cp.FreeInodeCount, fs.sb.TotalBlocks)
							}
							if cp.FreeDataCount != fs.sb.FreeDataBlks {
								fs.warnf("checkpoint free_data_count differs from superblock: %d vs %d",
									cp.FreeDataCount, fs.sb.FreeDataBlks)
							}
							if cp.FreeInodeCount != fs.sb.FreeInodes {
								fs.warnf("checkpoint free_inode_count differs from superblock: %d vs %d",
									cp.FreeInodeCount, fs.sb.FreeInodes)
							}
						}
					}
				}
			}

			recOff += 16 + uint64(dataLen)
		}

		if logStart == logEnd {
			break
		}
		cur = nextJournalBlock(cur, journalOffset, journalBlocks)
		if cur == end {
			break
		}
	}

	if badRecords == 0 && recordsChecked > 0 {
		fmt.Fprintf(os.Stderr, "  journal records: %d checked, no checksum errors\n", recordsChecked)
	} else if badRecords > 0 {
		fmt.Fprintf(os.Stderr, "  journal records: %d checked, %d checksum error(s)\n", recordsChecked, badRecords)
	}
}
