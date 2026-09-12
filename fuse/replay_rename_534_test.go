package fuse

// generic/534 regression: create + write + fsync, truncate, same-dir rename,
// fsync, crash cut, replay.  The real test (dm-flakey power-fail simulation)
// observed the OLD name 'foo' reappearing next to the correctly-renamed
// 'bar' after remount, with both fsyncs having committed everything — so the
// on-disk state at the cut was already post-rename and the old name must be
// re-derivation, not a sync gap.  This test mirrors the exact op sequence
// through the same BrieFS methods the FUSE handlers call, then cuts without
// the unmount checkpoint (dm-flakey drops those writes) and replays.

import (
	"testing"

	"github.com/ctdk/briefs-utils/briefs"
)

// fsyncLike mirrors the FUSE Fsync handler (fuse.go): journal commit, deferred
// metadata drain, one device cache flush.
func fsyncLike(t *testing.T, b *BrieFS) {
	t.Helper()
	if err := b.journal.Sync(false); err != nil {
		t.Fatalf("fsync: journal sync: %v", err)
	}
	if err := b.flushDirtyMeta(); err != nil {
		t.Fatalf("fsync: dirty meta drain: %v", err)
	}
	if err := b.dev.Fdatasync(); err != nil {
		t.Fatalf("fsync: fdatasync: %v", err)
	}
}

func TestReplayRenameOldNameStaysGone534(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 1500)
	b := openBridge(t, img)

	// $XFS_IO_PROG -f -c "pwrite -S 0xab 0 8000" -c "fsync" -c "truncate 3000"
	foo, err := b.createInDir(1, "foo", briefs.ModeFile|0o644, 1000, 1000, false)
	if err != nil {
		t.Fatalf("create foo: %v", err)
	}
	data := make([]byte, 8000)
	for i := range data {
		data[i] = 0xab
	}
	if _, err := b.writeFileData(foo.InodeNumber, data, 0); err != nil {
		t.Fatalf("pwrite foo: %v", err)
	}
	fsyncLike(t, b)
	// ftruncate -> Setattr(FATTR_SIZE)
	if err := b.setattrOp(foo.InodeNumber, &fuseSetAttrIn{valid: fattrSize, size: 3000}); err != nil {
		t.Fatalf("truncate foo: %v", err)
	}

	// mv foo bar; $XFS_IO_PROG -c "fsync" bar
	if err := b.renameInDir(1, "foo", 1, "bar", 0); err != nil {
		t.Fatalf("rename foo->bar: %v", err)
	}
	fsyncLike(t, b)

	// Crash cut: both fsyncs committed everything; close without the
	// unmount checkpoint (dm-flakey drops those writes in the real test).
	b.dev.Close()

	// Remount + replay.
	b2 := openBridge(t, img)
	if err := b2.replayJournal(); err != nil {
		t.Fatalf("replay: %v", err)
	}
	di, err := b2.inodes.ReadInode(1)
	if err != nil {
		t.Fatalf("read root after replay: %v", err)
	}
	if _, _, err := TrieLookup(b2.dev, di.DirTrieRoot, "foo"); err == nil {
		t.Fatal("generic/534 regression: old name 'foo' reappeared after replay")
	}
	barIno, _, err := TrieLookup(b2.dev, di.DirTrieRoot, "bar")
	if err != nil {
		t.Fatalf("post-replay lookup of bar: %v", err)
	}
	bi, err := b2.inodes.ReadInode(barIno)
	if err != nil {
		t.Fatalf("read bar inode: %v", err)
	}
	if bi.FileSize != 3000 {
		t.Fatalf("bar size after replay: got %d, want 3000", bi.FileSize)
	}
	_ = b2.journal.Checkpoint()
	_ = b2.dev.Sync()
}
