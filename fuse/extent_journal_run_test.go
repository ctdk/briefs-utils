package fuse

// generic/299 regression coverage for run-encoded extent journaling.
//
// commitExtentChange used to write one JRN_EXTENT_ALLOC per allocated
// block and one JRN_EXTENT_FREE per freed block.  generic/299's
// `falloc 0 $FILE_SIZE` over the whole ~100 GB scratch device allocated
// ~26M blocks in a single op: ~26M journal records filled the ~2800-record
// ring ~9300 times (a back-pressure checkpoint per fill), and the
// transient record buffers grew the daemon's heap until the OOM killer
// took it at ~3.2 GB anon-rss — with zero daemon-side errors to log.
//
// The kernel journals these records run-encoded (btree.c: one
// briefs_journal_extent_alloc(..., ext->len, ...) per extent), so the fix
// coalesces the block lists into contiguous runs (journalContigRuns).
// These tests pin both halves: the producer must emit O(runs) records,
// and the replay handlers (which always understood Length > 1) must
// reserve/free the same allocator bits the live path did.

import (
	"context"
	"testing"

	"github.com/ctdk/briefs-utils/briefs"
)

func TestJournalContigRuns(t *testing.T) {
	cases := []struct {
		name   string
		blocks []uint64
		want   [][2]uint64
	}{
		{"empty", nil, nil},
		{"single", []uint64{5}, [][2]uint64{{5, 1}}},
		{"one run", []uint64{10, 11, 12}, [][2]uint64{{10, 3}}},
		{"three runs", []uint64{10, 11, 12, 20, 21, 30}, [][2]uint64{{10, 3}, {20, 2}, {30, 1}}},
		{"descending stays split", []uint64{7, 6, 5}, [][2]uint64{{7, 1}, {6, 1}, {5, 1}}},
		{"duplicate splits", []uint64{3, 3, 4}, [][2]uint64{{3, 1}, {3, 2}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got [][2]uint64
			if err := journalContigRuns(tc.blocks, func(first, length uint64) error {
				got = append(got, [2]uint64{first, length})
				return nil
			}); err != nil {
				t.Fatalf("journalContigRuns: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %d runs %v, want %d runs %v", len(got), got, len(tc.want), tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("run %d: got %v, want %v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// countExtentRecords walks the journal's replay range and counts
// JRN_EXTENT_ALLOC / JRN_EXTENT_FREE records — the reader half of
// walkJournal, without applying anything.
func countExtentRecords(t *testing.T, b *BrieFS) (allocs, frees int) {
	t.Helper()
	start, end, checkpointBlk := b.journal.ReplayLogRange()
	blockSize := b.journal.JournalBlockSize()
	cur := start
	for cur != end {
		if cur == checkpointBlk {
			cur = b.journal.NextJournalBlock(cur)
			continue
		}
		buf, err := b.journal.ReadJournalBlock(cur)
		if err != nil {
			t.Fatalf("read journal block %d: %v", cur, err)
		}
		bh := briefs.ParseJournalBlockHeader(buf)
		if bh.Magic != briefs.MagicJournal && bh.Magic != briefs.MagicCheckpoint {
			break
		}
		off := uint64(briefs.JournalBlockHdrSize)
		for i := uint32(0); i < bh.RecordCount && off+briefs.JournalRecordHdrSize <= blockSize; i++ {
			hdr := briefs.ParseRecordHeader(buf[off:])
			switch hdr.Type {
			case briefs.JRN_EXTENT_ALLOC:
				allocs++
			case briefs.JRN_EXTENT_FREE:
				frees++
			}
			off += briefs.JournalRecordHdrSize + uint64(hdr.DataLen)
		}
		cur = b.journal.NextJournalBlock(cur)
	}
	return allocs, frees
}

func TestExtentJournalRunEncodedAndReplay(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 20000)
	b := openBridge(t, img)

	// Half the data region in one contiguous falloc — the shape of
	// generic/299's whole-device falloc, scaled to a unit-test image.
	dataBlocks := b.sb.TotalBlocks - b.dataRegionStart
	fallocBlocks := dataBlocks / 2
	size := fallocBlocks * uint64(b.blockSize)
	freeBefore := b.dataAlloc.FreeCount()

	f, err := b.createInDir(1, "big", briefs.ModeFile|0o644, 1000, 1000, false, 0)
	if err != nil {
		t.Fatalf("create big: %v", err)
	}
	if err := b.fallocateOp(context.Background(), f.InodeNumber, 0, size, 0); err != nil {
		t.Fatalf("falloc %d blocks: %v", fallocBlocks, err)
	}
	freeAfterFalloc := b.dataAlloc.FreeCount()

	// Commit without checkpointing: the records flush from the in-memory
	// curBlock to the device and stay replayable (an uncheckpointed Sync
	// does not retire the ring tail), but the journal is now committed.
	if err := b.journal.Sync(false); err != nil {
		t.Fatalf("journal sync: %v", err)
	}

	// THE regression pin: a contiguous falloc must journal a handful of
	// run-encoded records, not one per block (per-block records here
	// would number ~fallocBlocks).
	allocs, _ := countExtentRecords(t, b)
	if allocs == 0 {
		t.Fatal("no JRN_EXTENT_ALLOC records after falloc")
	}
	if allocs > 8 {
		t.Errorf("falloc of %d contiguous blocks journaled %d EXTENT_ALLOC records; run-encoding regressed?",
			fallocBlocks, allocs)
	}

	// "Crash" (close without the unmount checkpoint): the journal carries
	// committed-but-uncheckpointed run-encoded records.
	b.dev.Close()

	// Replay must reserve exactly the allocator bits the live path did.
	b2 := openBridge(t, img)
	if err := b2.replayJournal(); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if got := b2.dataAlloc.FreeCount(); got != freeAfterFalloc {
		t.Errorf("post-replay free count: got %d, want %d (live path had %d)", got, freeAfterFalloc, freeAfterFalloc)
	}
	di, err := b2.inodes.ReadInode(f.InodeNumber)
	if err != nil {
		t.Fatalf("read inode after replay: %v", err)
	}
	if di.FileSize != size {
		t.Errorf("post-replay file size: got %d, want %d", di.FileSize, size)
	}

	// Free side: truncate to 0 journals run-encoded EXTENT_FREE records,
	// and replaying them frees the same bits the live path freed.
	if err := b2.truncateInode(context.Background(), f.InodeNumber, 0); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if err := b2.journal.Sync(false); err != nil {
		t.Fatalf("journal sync: %v", err)
	}
	// Free counts must be read AFTER the sync: truncate's frees are
	// deferred (pendingFrees) and only enter the allocator at the sync's
	// SyncMeta drain — the deferred-free design the replay mirrors.
	freeAfterTrunc := b2.dataAlloc.FreeCount()
	_, frees := countExtentRecords(t, b2)
	if frees == 0 {
		t.Fatal("no JRN_EXTENT_FREE records after truncate")
	}
	if frees > 8 {
		t.Errorf("truncate of %d blocks journaled %d EXTENT_FREE records; run-encoding regressed?",
			fallocBlocks, frees)
	}
	b2.dev.Close()

	b3 := openBridge(t, img)
	if err := b3.replayJournal(); err != nil {
		t.Fatalf("replay after truncate: %v", err)
	}
	if got := b3.dataAlloc.FreeCount(); got != freeAfterTrunc {
		t.Errorf("post-truncate-replay free count: got %d, want %d", got, freeAfterTrunc)
	}
	// All but the directory-trie page (and any sentinel) must be back.
	if got := b3.dataAlloc.FreeCount(); freeBefore-got > 4 {
		t.Errorf("truncate did not return the blocks: free went %d -> %d", freeBefore, got)
	}
}
