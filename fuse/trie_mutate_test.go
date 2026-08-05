package fuse

import (
	"os/exec"
	"syscall"
	"testing"

	"github.com/ctdk/briefs-utils/briefs"
)

// allEntries walks a directory trie via the read-side iterator and returns the
// set of (name -> ino) entries.
func allEntries(t *testing.T, b *BrieFS, rootRef uint64) map[string]uint64 {
	t.Helper()
	out := map[string]uint64{}
	if rootRef == 0 {
		return out
	}
	it := NewTrieIterator(b.dev, rootRef)
	for {
		ino, _, name, err := it.Next()
		if err != nil || ino == 0 {
			break
		}
		out[name] = ino
	}
	return out
}

func TestTrieInsertLookupRemove(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)

	root, err := b.inodes.ReadInode(1) // root directory
	if err != nil {
		t.Fatalf("ReadInode root: %v", err)
	}

	// Names chosen to exercise pure-leaf creation, INTERM+STATUS_LEAF prefix
	// promotion, and splits: shared prefixes of varying lengths.
	names := []string{
		"file", "file1", "file10", "file100", // prefix chain
		"a", "ab", "abc", "abcd", // growing prefixes
		"x", "xy", "xyz",
		"lonely",
		"z",
		"ZZ", "Za", "za", // byte-value ordering across siblings
	}
	inoByName := map[string]uint64{}
	b.cacheBegin()
	for _, name := range names {
		child, err := b.AllocInode(briefs.ModeFile|0o644, 1000, 1000, 1)
		if err != nil {
			t.Fatalf("AllocInode %q: %v", name, err)
		}
		if err := b.writeInodeCached(child); err != nil {
			t.Fatalf("writeInodeCached %q: %v", name, err)
		}
		ftype := uint8(briefs.ModeFile >> 12) // DT_REG
		if err := b.TrieInsert(root, name, child.InodeNumber, ftype); err != nil {
			t.Fatalf("TrieInsert %q: %v", name, err)
		}
		inoByName[name] = child.InodeNumber
	}
	if err := b.writeInodeCached(root); err != nil {
		t.Fatalf("writeInode root: %v", err)
	}
	if err := b.flushCache(); err != nil {
		t.Fatalf("flushCache: %v", err)
	}

	// Every inserted name must look up to its allocated inode.
	for _, name := range names {
		ino, _, err := TrieLookup(b.dev, root.DirTrieRoot, name)
		if err != nil {
			t.Fatalf("TrieLookup %q: %v", name, err)
		}
		if ino != inoByName[name] {
			t.Errorf("lookup %q: want ino %d, got %d", name, inoByName[name], ino)
		}
	}
	// A name not inserted must be absent.
	if _, _, err := TrieLookup(b.dev, root.DirTrieRoot, "nope"); err == nil {
		t.Error("TrieLookup nope: want error, got nil")
	}

	// The iterator must yield exactly the inserted set.
	got := allEntries(t, b, root.DirTrieRoot)
	if len(got) != len(names) {
		t.Fatalf("iterator count: want %d, got %d (%v)", len(names), len(got), got)
	}
	for _, name := range names {
		if got[name] != inoByName[name] {
			t.Errorf("iterator %q: want ino %d, got %d", name, inoByName[name], got[name])
		}
	}

	// Re-inserting a name must report EEXIST and not corrupt the trie.
	b.cacheBegin()
	dup, _ := b.AllocInode(briefs.ModeFile|0o644, 1000, 1000, 1)
	if err := b.TrieInsert(root, "file", dup.InodeNumber, uint8(briefs.ModeFile>>12)); err != syscall.EEXIST {
		t.Errorf("re-insert file: want EEXIST, got %v", err)
	}
	_ = b.FreeInode(dup.InodeNumber)
	if err := b.flushCache(); err != nil {
		t.Fatalf("flushCache: %v", err)
	}

	// Remove half the entries (and free their inodes so fsck stays clean),
	// then verify they are gone and the rest remain.
	remove := []string{"file1", "abc", "xyz", "lonely", "z", "Za"}
	b.cacheBegin()
	for _, name := range remove {
		if err := b.TrieRemove(root, name); err != nil {
			t.Fatalf("TrieRemove %q: %v", name, err)
		}
		if err := b.FreeInode(inoByName[name]); err != nil {
			t.Fatalf("FreeInode %q: %v", name, err)
		}
	}
	if err := b.writeInodeCached(root); err != nil {
		t.Fatalf("writeInode root after remove: %v", err)
	}
	if err := b.flushCache(); err != nil {
		t.Fatalf("flushCache: %v", err)
	}
	for _, name := range remove {
		if _, _, err := TrieLookup(b.dev, root.DirTrieRoot, name); err == nil {
			t.Errorf("lookup removed %q: want error, got nil", name)
		}
	}
	remaining := map[string]bool{}
	for _, n := range names {
		remaining[n] = true
	}
	for _, n := range remove {
		remaining[n] = false
	}
	got = allEntries(t, b, root.DirTrieRoot)
	for name, want := range remaining {
		if want && got[name] == 0 {
			t.Errorf("iterator lost surviving entry %q", name)
		}
		if !want && got[name] != 0 {
			t.Errorf("iterator still has removed entry %q", name)
		}
	}

	// Checkpoint and run fsck: the on-disk trie must be consistent.
	if err := b.journal.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	b.dev.Close()

	fsck := buildBinary(t, "github.com/ctdk/briefs-utils/cmd/fsck", "fsck.briefs")
	out, err := exec.Command(fsck, img).CombinedOutput()
	if err != nil {
		t.Fatalf("fsck failed: %v\n%s", err, out)
	}
	if !contains(string(out), "FSCK COMPLETE: no errors found") {
		t.Fatalf("fsck not clean after trie ops:\n%s", out)
	}
}