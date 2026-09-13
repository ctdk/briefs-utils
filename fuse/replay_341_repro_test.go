package fuse

// generic/341 regression: mkdir a, mkdir a/x, write a/x/{foo,bar},
// _scratch_sync (a NO-OP on the FUSE mount), mv a/x a/y, mkdir a/x (a NEW
// empty dir reusing the old name), fsync the new x, power cut, remount +
// replay.  Post-replay a/ must be exactly {x, y}.  The 2026-09-12 full run
// failed the real test with every entry DOUBLED (a/ = {x, x, y, y}): the
// rename that emptied a/ freed its trie root and the add of "y" re-created
// one, but an early-window INODE_FULL snapshot of a (restored onto the
// inode block before the first DIR_UPDATE touched a) still named the OLD
// root — so replay anchored a's whole re-derivation at the stale root, and
// the re-derived adds linked a second copy of the entries into a root slot
// the live path had freed and reused for the live "y" leaf.  Fixed by
// stashing the pre-replay slot content on the first restore
// (replayParentsPristine) so first-touch anchors at the live path's final
// state, where every drained add EEXISTs and del ENOENTs as a no-op.

import (
	"context"
	"sort"
	"testing"

	"github.com/ctdk/briefs-utils/briefs"
)

// listDirNames walks a directory trie and returns all entry names, duplicates
// included (the point of the repro is to catch them).
func listDirNames(t *testing.T, b *BrieFS, di *briefs.Inode) []string {
	t.Helper()
	var names []string
	it := NewTrieIterator(b.dev, di.DirTrieRoot)
	for {
		ino, _, name, err := it.Next()
		if ino == 0 && name == "" {
			break
		}
		if err != nil {
			break
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func TestReplay341RenameDirRecreateName(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 1500)
	b := openBridge(t, img)

	// mkdir -p $SCRATCH_MNT/a/x
	a, err := b.createInDir(1, "a", briefs.ModeDir|0o755, 1000, 1000, false, 0)
	if err != nil {
		t.Fatalf("mkdir a: %v", err)
	}
	x, err := b.createInDir(a.InodeNumber, "x", briefs.ModeDir|0o755, 1000, 1000, false, 0)
	if err != nil {
		t.Fatalf("mkdir a/x: %v", err)
	}
	// pwrite 32K foo + 32K bar into a/x
	for _, n := range []string{"foo", "bar"} {
		f, err := b.createInDir(x.InodeNumber, n, briefs.ModeFile|0o644, 1000, 1000, false, 0)
		if err != nil {
			t.Fatalf("create a/x/%s: %v", n, err)
		}
		data := make([]byte, 32*1024)
		if _, err := b.writeFileData(context.Background(), f.InodeNumber, data, 0); err != nil {
			t.Fatalf("pwrite a/x/%s: %v", n, err)
		}
	}

	// _scratch_sync: NO-OP on the FUSE mount — the point of the repro.

	// mv $SCRATCH_MNT/a/x $SCRATCH_MNT/a/y ; mkdir $SCRATCH_MNT/a/x
	if err := b.renameInDir(a.InodeNumber, "x", a.InodeNumber, "y", 0); err != nil {
		t.Fatalf("mv a/x a/y: %v", err)
	}
	if _, err := b.createInDir(a.InodeNumber, "x", briefs.ModeDir|0o755, 1000, 1000, false, 0); err != nil {
		t.Fatalf("mkdir new a/x: %v", err)
	}
	// $XFS_IO_PROG -c "fsync" $SCRATCH_MNT/a/x  — a dir fsync reduces to the
	// journal sync + drain + device flush.
	fsyncLike(t, b)

	// Live-path sanity: a/ must be exactly {x, y} BEFORE the cut.
	ai, err := b.inodes.ReadInode(a.InodeNumber)
	if err != nil {
		t.Fatalf("read a before cut: %v", err)
	}
	if got := listDirNames(t, b, ai); len(got) != 2 || got[0] != "x" || got[1] != "y" {
		t.Errorf("LIVE-PATH regression (before cut): a/ contains %v, want [x y]", got)
	}

	// Power cut (dm-flakey drops the unmount checkpoint).
	b.dev.Close()

	// Remount + replay.
	b2 := openBridge(t, img)
	if err := b2.replayJournal(); err != nil {
		t.Fatalf("replay: %v", err)
	}

	di, err := b2.inodes.ReadInode(a.InodeNumber)
	if err != nil {
		t.Fatalf("read a after replay: %v", err)
	}
	names := listDirNames(t, b2, di)
	if len(names) != 2 || names[0] != "x" || names[1] != "y" {
		t.Errorf("generic/341 regression: a/ contains %v after replay, want [x y]", names)
		// Diagnostic: dump every trie node reachable from a's root.
		t.Logf("a DirTrieRoot=%d", di.DirTrieRoot)
		w := briefs.NewTrieWalker(b2.dev.ReadBlock, di.DirTrieRoot)
		for {
			ref, emitted, buf, _, node, ok := w.Next()
			if !ok {
				break
			}
			nm := "<unreadable>"
			if name, err := briefs.ReadTrieName(buf, node.NameLen, node.NameOffset); err == nil {
				nm = name
			}
			t.Logf("ref=%d (blk %d slot %d) emitted=%v type=%d name=%q ino=%d firstChild=%d next=%d",
				ref, briefs.TrieRefBlock(ref), briefs.TrieRefSlot(ref), emitted, node.NodeType, nm, node.Inode, node.FirstChild, node.NextSibling)
		}
	}

	// y keeps foo+bar (single copies); the new x is empty.
	yIno, _, err := TrieLookup(b2.dev, di.DirTrieRoot, "y")
	if err != nil {
		t.Fatalf("lookup y after replay: %v", err)
	}
	yi, err := b2.inodes.ReadInode(yIno)
	if err != nil {
		t.Fatalf("read y: %v", err)
	}
	if got := listDirNames(t, b2, yi); len(got) != 2 || got[0] != "bar" || got[1] != "foo" {
		t.Errorf("generic/341 regression: a/y contains %v after replay, want [bar foo]", got)
	}
	xIno, _, err := TrieLookup(b2.dev, di.DirTrieRoot, "x")
	if err != nil {
		t.Fatalf("lookup x after replay: %v", err)
	}
	xi, err := b2.inodes.ReadInode(xIno)
	if err != nil {
		t.Fatalf("read x: %v", err)
	}
	if got := listDirNames(t, b2, xi); len(got) != 0 {
		t.Errorf("generic/341 regression: new a/x contains %v after replay, want empty", got)
	}
	_ = b2.journal.Checkpoint()
	_ = b2.dev.Sync()
}
