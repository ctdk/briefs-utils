package fuse

import (
	"context"
	"sync"
	"testing"

	"github.com/ctdk/briefs-utils/briefs"
)

// TestSyncMetaDrainKeepsSiblingSlots pins the generic/127 fix.  SyncMeta once
// removed blocks from the deferred-metadata map before writing them to the
// device, opening a window in which the dirty view no longer served a block
// whose content was not yet in the page cache.  A concurrent op on a SIBLING
// inode in the same 4K inode-table block (file writes serialize on the
// inode-block shard lock, which the drain does not hold) then based its
// whole-block read-modify-write on the stale page-cache copy and regressed
// the other file's slot — observed in generic/127 as a file's size reverting
// to an older drain's value mid-run ("Size error: expected 0x28342 stat
// 0x26a1d").  The drain now keeps the map entries in place until every block
// is written, deleting afterwards only entries no concurrent op re-stored,
// so the dirty view covers the block for the whole window.
func TestSyncMetaDrainKeepsSiblingSlots(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)

	// Two files whose inode slots are siblings in one inode-table block —
	// the generic/127 shape (two fsx files created consecutively).
	a, err := b.createInDir(1, "a", briefs.ModeFile|0o644, 1000, 1000, false)
	if err != nil {
		t.Fatalf("create a: %v", err)
	}
	c, err := b.createInDir(1, "c", briefs.ModeFile|0o644, 1000, 1000, false)
	if err != nil {
		t.Fatalf("create c: %v", err)
	}
	blkA, offA := briefs.InodeLocation(b.sb, a.InodeNumber)
	blkC, _ := briefs.InodeLocation(b.sb, c.InodeNumber)
	if blkA != blkC {
		t.Fatalf("inodes %d and %d landed in different table blocks %d/%d",
			a.InodeNumber, c.InodeNumber, blkA, blkC)
	}

	// Write both files and drain, so the device page cache holds this state —
	// the stale base a regression would reintroduce.
	writeFile(t, b, a.InodeNumber, makePattern(1, 100), 0)
	writeFile(t, b, c.InodeNumber, makePattern(2, 100), 0)
	if err := b.journal.Sync(false); err != nil {
		t.Fatalf("initial sync: %v", err)
	}

	// Truncate a WITHOUT syncing: the deferred map now holds a whole-block
	// copy with a truncated to 50 that the device page cache does not have.
	if err := b.setattrOp(context.Background(), a.InodeNumber, setattrReq(fattrSize, withSize(50))); err != nil {
		t.Fatalf("truncate a: %v", err)
	}

	// Park a drain in its window: snapshot taken, no block written yet.
	// One-shot: later drains in this test run unpaused.
	parked := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	syncMetaPause = func() {
		once.Do(func() { close(parked); <-release })
	}
	defer func() { syncMetaPause = nil }()
	drained := make(chan error, 1)
	go func() { drained <- b.SyncMeta() }()
	<-parked

	// A write to the SIBLING while the drain is parked: whole-block RMW of
	// the shared inode block.  Its base must come from the dirty view —
	// with the old swap-out drain it came from the stale page cache and
	// regressed a's slot back to size 100.
	writeFile(t, b, c.InodeNumber, makePattern(3, 150), 0)

	close(release)
	if err := <-drained; err != nil {
		t.Fatalf("SyncMeta: %v", err)
	}

	// The truncate survived the sibling's concurrent write.
	in, err := b.inodes.ReadInode(a.InodeNumber)
	if err != nil {
		t.Fatalf("re-read a: %v", err)
	}
	if in.FileSize != 50 {
		t.Fatalf("sibling write during drain regressed a: size %d, want 50", in.FileSize)
	}
	// ... and the sibling write survived too.
	inC, err := b.inodes.ReadInode(c.InodeNumber)
	if err != nil {
		t.Fatalf("re-read c: %v", err)
	}
	if inC.FileSize != 150 {
		t.Fatalf("drain dropped c's write: size %d, want 150", inC.FileSize)
	}

	// Drain the rest and verify the DEVICE copy (bypassing the dirty view)
	// carries a's truncated slot — the page cache must converge, not just
	// the in-memory state.
	if err := b.SyncMeta(); err != nil {
		t.Fatalf("final SyncMeta: %v", err)
	}
	raw := make([]byte, b.blockSize)
	if _, err := b.dev.File().ReadAt(raw, int64(blkA*b.blockSize)); err != nil {
		t.Fatalf("read table block %d: %v", blkA, err)
	}
	inDev, err := briefs.UnmarshalInode(raw[offA : offA+b.sb.InodeSize])
	if err != nil {
		t.Fatalf("unmarshal a from device: %v", err)
	}
	if inDev.FileSize != 50 {
		t.Fatalf("device copy of a regressed: size %d, want 50", inDev.FileSize)
	}

	fsckClean(t, b, img)
}
