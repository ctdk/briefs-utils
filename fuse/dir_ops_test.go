package fuse

import (
	"os/exec"
	"syscall"
	"testing"

	"github.com/ctdk/briefs-utils/briefs"
)

// lookupEntry reads the parent dir inode from disk and looks up name in its
// trie, returning the child ino (0 if absent).
func lookupEntry(t *testing.T, b *BrieFS, parentIno uint64, name string) uint64 {
	t.Helper()
	parent, err := b.inodes.ReadInode(parentIno)
	if err != nil {
		t.Fatalf("ReadInode parent %d: %v", parentIno, err)
	}
	ino, _, err := TrieLookup(b.dev, parent.DirTrieRoot, name)
	if err != nil {
		return 0
	}
	return ino
}

// dirEntryCount returns the number of real entries in a directory trie.
func dirEntryCount(t *testing.T, b *BrieFS, parentIno uint64) int {
	t.Helper()
	parent, err := b.inodes.ReadInode(parentIno)
	if err != nil {
		t.Fatalf("ReadInode %d: %v", parentIno, err)
	}
	return len(allEntries(t, b, parent.DirTrieRoot))
}

func TestDirCreateMkdirUnlinkRmdir(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)

	const rootIno = 1

	// --- create files in root ---
	files := []string{"alpha", "beta", "gamma", "delta"}
	inoByName := map[string]uint64{}
	for _, name := range files {
		child, err := b.createInDir(rootIno, name, briefs.ModeFile|0o644, 1000, 1000, false)
		if err != nil {
			t.Fatalf("createInDir %q: %v", name, err)
		}
		inoByName[name] = child.InodeNumber
		if child.InodeNumber <= 1 {
			t.Fatalf("created ino %d should be > 1", child.InodeNumber)
		}
		if !child.IsFile() {
			t.Fatalf("created %q not a regular file: mode %o", name, child.Filemode)
		}
		if got := lookupEntry(t, b, rootIno, name); got != child.InodeNumber {
			t.Fatalf("lookup %q after create: want %d, got %d", name, child.InodeNumber, got)
		}
	}

	// Readdir reflects all created files.
	if got := dirEntryCount(t, b, rootIno); got != len(files) {
		t.Fatalf("root entry count: want %d, got %d", len(files), got)
	}

	// Root size grew by the entry prefix + name length for each file.
	root, err := b.inodes.ReadInode(rootIno)
	if err != nil {
		t.Fatalf("ReadInode root: %v", err)
	}
	wantSize := uint64(0)
	for _, n := range files {
		wantSize += uint64(dirEntryPrefixLen + len(n))
	}
	if root.FileSize != wantSize {
		t.Fatalf("root size: want %d, got %d", wantSize, root.FileSize)
	}

	// --- mkdir subdirs ---
	dirs := []string{"sub1", "sub2"}
	inoByDir := map[string]uint64{}
	for _, name := range dirs {
		child, err := b.createInDir(rootIno, name, briefs.ModeDir|0o755, 1000, 1000, false)
		if err != nil {
			t.Fatalf("mkdir %q: %v", name, err)
		}
		inoByDir[name] = child.InodeNumber
		if !child.IsDir() {
			t.Fatalf("mkdir %q not a directory: mode %o", name, child.Filemode)
		}
		if child.Nlinks != 2 {
			t.Fatalf("new dir nlink: want 2, got %d", child.Nlinks)
		}
		if child.DirTrieRoot == 0 {
			t.Fatalf("mkdir %q: dir trie root not created", name)
		}
		if child.ParentInode != rootIno {
			t.Fatalf("mkdir %q parent: want %d, got %d", name, rootIno, child.ParentInode)
		}
	}
	// Each subdir added one to root's nlink ("." + ".." => subdir nlink 2,
	// parent gains one ".." link per subdir).
	root, _ = b.inodes.ReadInode(rootIno)
	if root.Nlinks != uint32(2+len(dirs)) {
		t.Fatalf("root nlink after mkdirs: want %d, got %d", 2+len(dirs), root.Nlinks)
	}

	// --- create a file inside a subdir (nested trie) ---
	sub1 := inoByDir["sub1"]
	nested, err := b.createInDir(sub1, "inside", briefs.ModeFile|0o600, 1000, 1000, false)
	if err != nil {
		t.Fatalf("createInDir nested: %v", err)
	}
	if got := lookupEntry(t, b, sub1, "inside"); got != nested.InodeNumber {
		t.Fatalf("nested lookup: want %d, got %d", nested.InodeNumber, got)
	}
	if got := dirEntryCount(t, b, sub1); got != 1 {
		t.Fatalf("sub1 entry count: want 1, got %d", got)
	}

	// --- duplicate create fails with EEXIST and does not corrupt ---
	if _, err := b.createInDir(rootIno, "alpha", briefs.ModeFile|0o644, 1000, 1000, true); err != syscall.EEXIST {
		t.Fatalf("duplicate create: want EEXIST, got %v", err)
	}
	if got := dirEntryCount(t, b, rootIno); got != len(files)+len(dirs) {
		t.Fatalf("root count after dup: want %d, got %d", len(files)+len(dirs), got)
	}

	// --- rmdir non-empty fails with ENOTEMPTY ---
	if err := b.unlinkInDir(rootIno, "sub1", true); err != syscall.ENOTEMPTY {
		t.Fatalf("rmdir non-empty: want ENOTEMPTY, got %v", err)
	}
	// --- unlink on a directory fails with EISDIR ---
	if err := b.unlinkInDir(rootIno, "sub2", false); err != syscall.EISDIR {
		t.Fatalf("unlink dir: want EISDIR, got %v", err)
	}
	// --- rmdir on a file fails with ENOTDIR ---
	if err := b.unlinkInDir(rootIno, "alpha", true); err != syscall.ENOTDIR {
		t.Fatalf("rmdir file: want ENOTDIR, got %v", err)
	}
	// --- unlink a nonexistent name fails with ENOENT ---
	if err := b.unlinkInDir(rootIno, "nope", false); err != syscall.ENOENT {
		t.Fatalf("unlink missing: want ENOENT, got %v", err)
	}

	// --- unlink the nested file, then rmdir the now-empty subdir ---
	if err := b.unlinkInDir(sub1, "inside", false); err != nil {
		t.Fatalf("unlink nested: %v", err)
	}
	if got := dirEntryCount(t, b, sub1); got != 0 {
		t.Fatalf("sub1 after unlink nested: want 0, got %d", got)
	}
	if err := b.unlinkInDir(rootIno, "sub1", true); err != nil {
		t.Fatalf("rmdir sub1: %v", err)
	}
	if got := lookupEntry(t, b, rootIno, "sub1"); got != 0 {
		t.Fatalf("sub1 still present after rmdir: ino %d", got)
	}
	// Root nlink dropped back by one.
	root, _ = b.inodes.ReadInode(rootIno)
	if root.Nlinks != uint32(2+len(dirs)-1) {
		t.Fatalf("root nlink after rmdir: want %d, got %d", 2+len(dirs)-1, root.Nlinks)
	}

	// --- unlink the remaining files ---
	for _, name := range files {
		if err := b.unlinkInDir(rootIno, name, false); err != nil {
			t.Fatalf("unlink %q: %v", name, err)
		}
		if got := lookupEntry(t, b, rootIno, name); got != 0 {
			t.Fatalf("%q still present after unlink: ino %d", name, got)
		}
	}
	// rmdir the other subdir.
	if err := b.unlinkInDir(rootIno, "sub2", true); err != nil {
		t.Fatalf("rmdir sub2: %v", err)
	}
	if got := dirEntryCount(t, b, rootIno); got != 0 {
		t.Fatalf("root after all removals: want 0, got %d", got)
	}

	// The freed inode numbers should be reusable (bitmap recycled).
	freeBefore := b.inodeAlloc.FreeCount()
	reused, err := b.createInDir(rootIno, "reused", briefs.ModeFile|0o644, 1000, 1000, false)
	if err != nil {
		t.Fatalf("createInDir reused: %v", err)
	}
	_ = reused
	_ = b.unlinkInDir(rootIno, "reused", false)
	if got := b.inodeAlloc.FreeCount(); got != freeBefore {
		t.Fatalf("free count after alloc+free reuse: want %d, got %d", freeBefore, got)
	}

	// Checkpoint a clean journal and run fsck.
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
		t.Fatalf("fsck not clean after dir ops:\n%s", out)
	}
}