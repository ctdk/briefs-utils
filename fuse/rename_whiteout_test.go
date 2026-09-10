package fuse

// Regression test for generic/585: a RENAME_WHITEOUT whiteout is a chardev
// (S_IFCHR | 0600, like the kernel's dir.c:1036 S_IFCHR|WHITEOUT_MODE) whose
// trie ftype is 2.  A Lookup bug treated ftype 2 as a directory (colliding
// with the trie node-type bit briefs.NodeTypeDir == 0x02), so whiteouts
// appeared as un-removable directories and generic/585 died in
// _require_renameat2's cleanup with "Directory not empty".

import (
	"testing"

	"github.com/ctdk/briefs-utils/briefs"
)

func TestRenameWhiteoutChardev(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)

	const rootIno = 1
	if _, err := b.createInDir(rootIno, "foo", briefs.ModeFile|0o644, 1000, 1000, false); err != nil {
		t.Fatalf("create foo: %v", err)
	}
	if _, err := b.createInDir(rootIno, "bar", briefs.ModeFile|0o644, 1000, 1000, false); err != nil {
		t.Fatalf("create bar: %v", err)
	}

	if err := b.renameInDir(rootIno, "foo", rootIno, "bar", renameWhiteout); err != nil {
		t.Fatalf("renameWhiteout: %v", err)
	}

	root, err := b.inodes.ReadInode(rootIno)
	if err != nil {
		t.Fatal(err)
	}

	// The whiteout occupies the old name: a chardev ino with
	// S_IFCHR|0600, nlink 1 (kernel parity, dir.c:1036).
	woIno, woFtype, err := TrieLookup(b.dev, root.DirTrieRoot, "foo")
	if err != nil {
		t.Fatalf("lookup foo after rename: %v", err)
	}
	if woFtype != 2 {
		t.Errorf("whiteout trie ftype = %d, want 2 (S_IFCHR >> 12)", woFtype)
	}
	wo, err := b.inodes.ReadInode(woIno)
	if err != nil {
		t.Fatalf("read whiteout inode: %v", err)
	}
	if wo.Filemode != modeWhiteout {
		t.Errorf("whiteout inode mode = 0%o, want 0%o (S_IFCHR|0600)", wo.Filemode, modeWhiteout)
	}
	if wo.Nlinks != 1 {
		t.Errorf("whiteout nlink = %d, want 1", wo.Nlinks)
	}

	// The moved file keeps its regular-file type at the new name.
	barIno, barFtype, err := TrieLookup(b.dev, root.DirTrieRoot, "bar")
	if err != nil {
		t.Fatalf("lookup bar after rename: %v", err)
	}
	if barFtype != uint8(briefs.ModeFile>>12) {
		t.Errorf("bar trie ftype = %d, want %d (S_IFREG >> 12)", barFtype, briefs.ModeFile>>12)
	}
	if barIno == woIno {
		t.Errorf("bar and whiteout share ino %d", barIno)
	}

	// The whiteout must be removable by an ordinary unlink, and the
	// directory must then be empty of it (the generic/585 failure mode:
	// leftover entries that rm -rf can never clear).
	if err := b.unlinkInDir(rootIno, "foo", false); err != nil {
		t.Fatalf("unlink whiteout: %v", err)
	}
	if _, _, err := TrieLookup(b.dev, root.DirTrieRoot, "foo"); err == nil {
		t.Error("foo still resolvable after unlink")
	}
}
