package fuse

import (
	"syscall"
	"testing"

	"github.com/ctdk/briefs-utils/briefs"
)

// TestHardlink exercises link semantics and that unlinking the last link frees
// the data extents (freeInodeData) — fsck-clean afterward.
func TestHardlink(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)

	a, err := b.createInDir(1, "a", briefs.ModeFile|0o644, 1000, 1000, false)
	if err != nil {
		t.Fatalf("create a: %v", err)
	}
	data := makePattern(1, 300)
	if _, err := b.writeFileData(a.InodeNumber, data, 0); err != nil {
		t.Fatalf("write a: %v", err)
	}

	// Link b -> a.
	bn, err := b.linkInDir(1, "b", a.InodeNumber)
	if err != nil {
		t.Fatalf("link b: %v", err)
	}
	if bn.InodeNumber != a.InodeNumber {
		t.Fatalf("link: b ino %d != a ino %d", bn.InodeNumber, a.InodeNumber)
	}
	di, _ := b.inodes.ReadInode(a.InodeNumber)
	if di.Nlinks != 2 {
		t.Fatalf("after link: nlink want 2, got %d", di.Nlinks)
	}
	if got := lookupEntry(t, b, 1, "b"); got != a.InodeNumber {
		t.Fatalf("lookup b: want %d, got %d", a.InodeNumber, got)
	}
	// Data is visible through both names.
	if got := readFile(t, b, a.InodeNumber, 0, 300); !bytesEqual(got, data) {
		t.Fatalf("read via a after link: mismatch")
	}

	// Unlink a; b still resolves and the data survives.
	if err := b.unlinkInDir(1, "a", false); err != nil {
		t.Fatalf("unlink a: %v", err)
	}
	di, _ = b.inodes.ReadInode(a.InodeNumber)
	if di.Nlinks != 1 {
		t.Fatalf("after unlink a: nlink want 1, got %d", di.Nlinks)
	}
	if got := readFile(t, b, a.InodeNumber, 0, 300); !bytesEqual(got, data) {
		t.Fatalf("read via b after unlink a: mismatch")
	}

	// Unlink b (last link) -> inode + data freed.
	freeBefore := b.dataAlloc.FreeCount()
	if err := b.unlinkInDir(1, "b", false); err != nil {
		t.Fatalf("unlink b: %v", err)
	}
	if got := b.dataAlloc.FreeCount(); got <= freeBefore {
		t.Fatalf("data blocks not freed on last unlink: free %d -> %d", freeBefore, got)
	}

	fsckClean(t, b, img)
}

// TestSymlink covers inline (<=256B) and extent (>256B) symlink targets.
func TestSymlink(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)

	short := "/etc/passwd"
	s1, err := b.symlinkInDir(1, "short", short, 1000, 1000)
	if err != nil {
		t.Fatalf("symlink short: %v", err)
	}
	if !s1.IsSymlink() {
		t.Fatalf("short not a symlink: mode %o", s1.Filemode)
	}
	if got, _ := b.readSymlink(s1.InodeNumber); got != short {
		t.Fatalf("readSymlink short: want %q, got %q", short, got)
	}
	di, _ := b.inodes.ReadInode(s1.InodeNumber)
	if di.Flags&briefs.InodeFlagInlineData == 0 {
		t.Fatalf("short symlink should be inline-data")
	}

	long := "/this/is/a/very/long/symlink/target/" + string(makePattern(5, 300))
	s2, err := b.symlinkInDir(1, "long", long, 1000, 1000)
	if err != nil {
		t.Fatalf("symlink long: %v", err)
	}
	if got, _ := b.readSymlink(s2.InodeNumber); got != long {
		t.Fatalf("readSymlink long mismatch: got len %d want %d", len(got), len(long))
	}
	di, _ = b.inodes.ReadInode(s2.InodeNumber)
	if di.Flags&briefs.InodeFlagInlineData != 0 {
		t.Fatalf("long symlink should be extent-backed")
	}
	if di.NumExtentsTotal != 1 {
		t.Fatalf("long symlink extents: want 1, got %d", di.NumExtentsTotal)
	}

	fsckClean(t, b, img)
}

// TestMknod creates special files and checks their mode + rdev.
func TestMknod(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)

	cases := []struct {
		name string
		mode uint32
		rdev uint64
	}{
		{"chr", modeChr | 0o660, 0x0501},
		{"blk", modeBlk | 0o660, 0x0802},
		{"fifo", modeFifo | 0o666, 0},
		{"sock", modeSock | 0o666, 0},
	}
	for _, c := range cases {
		in, err := b.mknodInDir(1, c.name, c.mode, 1000, 1000, c.rdev)
		if err != nil {
			t.Fatalf("mknod %q: %v", c.name, err)
		}
		di, _ := b.inodes.ReadInode(in.InodeNumber)
		if di.Filemode&briefs.ModeTypeMask != c.mode&briefs.ModeTypeMask {
			t.Fatalf("mknod %q mode: want %o, got %o", c.name, c.mode, di.Filemode)
		}
		if di.Rdev != c.rdev {
			t.Fatalf("mknod %q rdev: want %d, got %d", c.name, c.rdev, di.Rdev)
		}
		if got := lookupEntry(t, b, 1, c.name); got != in.InodeNumber {
			t.Fatalf("mknod %q lookup: want %d, got %d", c.name, in.InodeNumber, got)
		}
	}
	fsckClean(t, b, img)
}

// TestRename covers same-dir, cross-dir, replace, dir-move, ENOTEMPTY.
func TestRename(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)

	// Files in root.
	a, _ := b.createInDir(1, "a", briefs.ModeFile|0o644, 1000, 1000, false)
	b.writeFileData(a.InodeNumber, makePattern(1, 100), 0)
	// Same-dir rename a -> a2.
	if err := b.renameInDir(1, "a", 1, "a2", 0); err != nil {
		t.Fatalf("rename a->a2: %v", err)
	}
	if got := lookupEntry(t, b, 1, "a"); got != 0 {
		t.Fatalf("a still present after rename: %d", got)
	}
	if got := lookupEntry(t, b, 1, "a2"); got != a.InodeNumber {
		t.Fatalf("a2 lookup: want %d, got %d", a.InodeNumber, got)
	}

	// A second dir + cross-dir rename a2 -> sub/a2 (file move).
	sub, _ := b.createInDir(1, "sub", briefs.ModeDir|0o755, 1000, 1000, false)
	if err := b.renameInDir(1, "a2", sub.InodeNumber, "a2", 0); err != nil {
		t.Fatalf("rename a2 -> sub/a2: %v", err)
	}
	if got := lookupEntry(t, b, sub.InodeNumber, "a2"); got != a.InodeNumber {
		t.Fatalf("sub/a2 lookup: want %d, got %d", a.InodeNumber, got)
	}

	// Cross-dir DIRECTORY move updates parent_inode + parent nlinks. Create a
	// dir "d" in root, move it into sub, check d.parent_inode == sub and the
	// parent nlink adjustments.
	d, _ := b.createInDir(1, "d", briefs.ModeDir|0o755, 1000, 1000, false)
	rootBefore, _ := b.inodes.ReadInode(1)
	subBefore, _ := b.inodes.ReadInode(sub.InodeNumber)
	if err := b.renameInDir(1, "d", sub.InodeNumber, "d", 0); err != nil {
		t.Fatalf("rename d -> sub/d (dir move): %v", err)
	}
	moved, _ := b.inodes.ReadInode(d.InodeNumber)
	if moved.ParentInode != sub.InodeNumber {
		t.Fatalf("moved dir parent_inode: want %d, got %d", sub.InodeNumber, moved.ParentInode)
	}
	rootAfter, _ := b.inodes.ReadInode(1)
	subAfter, _ := b.inodes.ReadInode(sub.InodeNumber)
	if rootAfter.Nlinks != rootBefore.Nlinks-1 {
		t.Fatalf("root nlink after dir move: want %d, got %d", rootBefore.Nlinks-1, rootAfter.Nlinks)
	}
	if subAfter.Nlinks != subBefore.Nlinks+1 {
		t.Fatalf("sub nlink after dir move: want %d, got %d", subBefore.Nlinks+1, subAfter.Nlinks)
	}

	// Rename over an existing target replaces it (and frees the old target).
	tgt, _ := b.createInDir(1, "tgt", briefs.ModeFile|0o644, 1000, 1000, false)
	b.writeFileData(tgt.InodeNumber, makePattern(2, 200), 0)
	tgtIno := tgt.InodeNumber
	src, _ := b.createInDir(1, "src", briefs.ModeFile|0o644, 1000, 1000, false)
	if err := b.renameInDir(1, "src", 1, "tgt", 0); err != nil {
		t.Fatalf("rename src -> tgt (replace): %v", err)
	}
	if got := lookupEntry(t, b, 1, "tgt"); got != src.InodeNumber {
		t.Fatalf("tgt after replace: want %d, got %d", src.InodeNumber, got)
	}
	// The old target inode should be freed (nlink 0).
	oldTgt, err := b.inodes.ReadInode(tgtIno)
	if err == nil && oldTgt.Magic == briefs.MagicInode {
		t.Fatalf("old target inode not freed after rename-over")
	}

	// Rename over a non-empty directory target -> ENOTEMPTY.
	ne, _ := b.createInDir(1, "ne", briefs.ModeDir|0o755, 1000, 1000, false)
	if _, err := b.createInDir(ne.InodeNumber, "kid", briefs.ModeFile|0o644, 1000, 1000, false); err != nil {
		t.Fatalf("create ne/kid: %v", err)
	}
	if _, err := b.createInDir(1, "src2", briefs.ModeFile|0o644, 1000, 1000, false); err != nil {
		t.Fatalf("create src2: %v", err)
	}
	if err := b.renameInDir(1, "src2", 1, "ne", 0); err != syscall.ENOTEMPTY {
		t.Fatalf("rename over non-empty dir: want ENOTEMPTY, got %v", err)
	}
	// Renaming over an EMPTY directory replaces it (rmdir-style).
	empty, _ := b.createInDir(1, "empty", briefs.ModeDir|0o755, 1000, 1000, false)
	src3, _ := b.createInDir(1, "src3", briefs.ModeFile|0o644, 1000, 1000, false)
	if err := b.renameInDir(1, "src3", 1, "empty", 0); err != nil {
		t.Fatalf("rename over empty dir: %v", err)
	}
	// "empty" now points at src3's inode; the old empty dir inode is freed.
	if got := lookupEntry(t, b, 1, "empty"); got != src3.InodeNumber {
		t.Fatalf("empty after replace: want %d, got %d", src3.InodeNumber, got)
	}
	if oldEmpty, err := b.inodes.ReadInode(empty.InodeNumber); err == nil && oldEmpty.Magic == briefs.MagicInode {
		t.Fatalf("old empty dir inode not freed after rename-over")
	}

	fsckClean(t, b, img)
}

// TestRenameExchange swaps two entries via RENAME_EXCHANGE.
func TestRenameExchange(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)

	x, _ := b.createInDir(1, "x", briefs.ModeFile|0o644, 1000, 1000, false)
	y, _ := b.createInDir(1, "y", briefs.ModeFile|0o644, 1000, 1000, false)

	if err := b.renameInDir(1, "x", 1, "y", renameExchange); err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if got := lookupEntry(t, b, 1, "x"); got != y.InodeNumber {
		t.Fatalf("after exchange x: want %d, got %d", y.InodeNumber, got)
	}
	if got := lookupEntry(t, b, 1, "y"); got != x.InodeNumber {
		t.Fatalf("after exchange y: want %d, got %d", x.InodeNumber, got)
	}

	// Cross-dir exchange.
	sub, _ := b.createInDir(1, "sub", briefs.ModeDir|0o755, 1000, 1000, false)
	z, _ := b.createInDir(sub.InodeNumber, "z", briefs.ModeFile|0o644, 1000, 1000, false)
	if err := b.renameInDir(1, "x", sub.InodeNumber, "z", renameExchange); err != nil {
		t.Fatalf("cross-dir exchange: %v", err)
	}
	if got := lookupEntry(t, b, 1, "x"); got != z.InodeNumber {
		t.Fatalf("after cross exchange x: want %d, got %d", z.InodeNumber, got)
	}
	if got := lookupEntry(t, b, sub.InodeNumber, "z"); got != y.InodeNumber {
		t.Fatalf("after cross exchange sub/z: want %d, got %d", y.InodeNumber, got)
	}

	fsckClean(t, b, img)
}

// TestRenameWhiteout renames with RENAME_WHITEOUT and checks a chardev whiteout
// is left at the source.
func TestRenameWhiteout(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)

	src, _ := b.createInDir(1, "src", briefs.ModeFile|0o644, 1000, 1000, false)
	srcIno := src.InodeNumber

	if err := b.renameInDir(1, "src", 1, "dst", renameWhiteout); err != nil {
		t.Fatalf("whiteout rename: %v", err)
	}
	// dst points at the original source inode.
	if got := lookupEntry(t, b, 1, "dst"); got != srcIno {
		t.Fatalf("after whiteout dst: want %d, got %d", srcIno, got)
	}
	// src is now a chardev whiteout.
	wIno := lookupEntry(t, b, 1, "src")
	if wIno == 0 {
		t.Fatalf("whiteout not left at src")
	}
	wdi, err := b.inodes.ReadInode(wIno)
	if err != nil {
		t.Fatalf("read whiteout inode: %v", err)
	}
	if wdi.Filemode&briefs.ModeTypeMask != modeChr {
		t.Fatalf("whiteout mode: want chardev %o, got %o", modeChr, wdi.Filemode&briefs.ModeTypeMask)
	}

	fsckClean(t, b, img)
}