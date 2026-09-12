package fuse

// generic/335 regression repro: mkdir a, mkdir a/b, mkdir c, touch a/b/foo,
// sync (a NO-OP on the FUSE mount), mv a/b/foo c/, touch a/bar, fsync a,
// power-cut (no unmount checkpoint), remount + replay.  The single fsync
// commits the ENTIRE window since mount (no checkpoint ever ran), so replay
// re-derives every create against already-applied on-disk state — and the
// mv empties a/b's trie, freeing its root mid-window (the generic/534
// trie-root churn class).  The freed page's deferred content must still be
// drained at the commit (deferBlockFree keeps it; SyncMeta drains before
// applying frees): replay's re-derived dir-add of a/b/foo walks that root,
// and a never-written page is garbage — bad trie page magic, which
// TrieInsert collapses into a spurious "no space left on device" that kills
// the remount (the original VM failure).

import (
	"testing"

	"github.com/ctdk/briefs-utils/briefs"
)

func TestReplay335FullWindowReplay(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 1500)
	b := openBridge(t, img)

	// mkdir -p $SCRATCH_MNT/a/b; mkdir $SCRATCH_MNT/c; touch $SCRATCH_MNT/a/b/foo
	a, err := b.createInDir(1, "a", briefs.ModeDir|0o755, 1000, 1000, false)
	if err != nil {
		t.Fatalf("mkdir a: %v", err)
	}
	ab, err := b.createInDir(a.InodeNumber, "b", briefs.ModeDir|0o755, 1000, 1000, false)
	if err != nil {
		t.Fatalf("mkdir a/b: %v", err)
	}
	c, err := b.createInDir(1, "c", briefs.ModeDir|0o755, 1000, 1000, false)
	if err != nil {
		t.Fatalf("mkdir c: %v", err)
	}
	if _, err := b.createInDir(ab.InodeNumber, "foo", briefs.ModeFile|0o644, 1000, 1000, false); err != nil {
		t.Fatalf("touch a/b/foo: %v", err)
	}

	// _scratch_sync: a silent no-op on the FUSE mount (sync(2) never reaches
	// the daemon) — the point of this repro.

	// mv $SCRATCH_MNT/a/b/foo $SCRATCH_MNT/c/
	if err := b.renameInDir(ab.InodeNumber, "foo", c.InodeNumber, "foo", 0); err != nil {
		t.Fatalf("mv a/b/foo c/: %v", err)
	}

	// touch $SCRATCH_MNT/a/bar; $XFS_IO_PROG -c "fsync" $SCRATCH_MNT/a
	if _, err := b.createInDir(a.InodeNumber, "bar", briefs.ModeFile|0o644, 1000, 1000, false); err != nil {
		t.Fatalf("touch a/bar: %v", err)
	}
	fsyncLike(t, b)

	// Power cut: dm-flakey drops the unmount checkpoint; everything the
	// fsync committed is on disk.
	b.dev.Close()

	b2 := openBridge(t, img)
	if err := b2.replayJournal(); err != nil {
		t.Fatalf("replay: %v", err)
	}

	// Filesystem content after power failure must match before-failure:
	// a: bar, b/; c: foo.
	di, err := b2.inodes.ReadInode(1)
	if err != nil {
		t.Fatalf("read root: %v", err)
	}
	for _, want := range []string{"a", "c"} {
		if _, _, err := TrieLookup(b2.dev, di.DirTrieRoot, want); err != nil {
			t.Errorf("root lost %q after replay: %v", want, err)
		}
	}
	ai, err := b2.inodes.ReadInode(a.InodeNumber)
	if err != nil {
		t.Fatalf("read a: %v", err)
	}
	for _, want := range []string{"bar", "b"} {
		if _, _, err := TrieLookup(b2.dev, ai.DirTrieRoot, want); err != nil {
			t.Errorf("a lost %q after replay: %v", want, err)
		}
	}
	abi, err := b2.inodes.ReadInode(ab.InodeNumber)
	if err != nil {
		t.Fatalf("read a/b: %v", err)
	}
	if abi.DirTrieRoot != 0 {
		if _, _, err := TrieLookup(b2.dev, abi.DirTrieRoot, "foo"); err == nil {
			t.Error("a/b still contains foo after replay")
		}
	}
	ci, err := b2.inodes.ReadInode(c.InodeNumber)
	if err != nil {
		t.Fatalf("read c: %v", err)
	}
	if _, _, err := TrieLookup(b2.dev, ci.DirTrieRoot, "foo"); err != nil {
		t.Errorf("c lost foo after replay: %v", err)
	}
	_ = b2.journal.Checkpoint()
	_ = b2.dev.Sync()
}