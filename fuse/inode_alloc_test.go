package fuse

import (
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/ctdk/briefs-utils/briefs"
)

func buildMkfs(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "mkfs.briefs")
	out, err := exec.Command("go", "build", "-o", bin, "github.com/ctdk/briefs-utils/cmd/mkfs").CombinedOutput()
	if err != nil {
		t.Fatalf("build mkfs: %v\n%s", err, out)
	}
	return bin
}

// openBridge builds a BrieFS over an existing image (the same construction
// Mount does, minus the FUSE server) so tests can exercise the write path
// directly.
func openBridge(t *testing.T, imgPath string) *BrieFS {
	t.Helper()
	dev, blockSize, err := OpenBlockDevice(imgPath)
	if err != nil {
		t.Fatalf("OpenBlockDevice: %v", err)
	}
	t.Cleanup(func() { dev.Close() })

	sb, err := readSuperblock(dev)
	if err != nil {
		t.Fatalf("readSuperblock: %v", err)
	}
	dataAlloc, err := OpenAllocator(dev, sb.TrieNodePoolStart)
	if err != nil {
		t.Fatalf("OpenAllocator data: %v", err)
	}
	inodeAlloc, err := OpenAllocator(dev, sb.InodeBMOffset)
	if err != nil {
		t.Fatalf("OpenAllocator inode: %v", err)
	}
	bfs := &BrieFS{
		dev:             dev,
		sb:              sb,
		inodes:          NewInodeManager(dev, sb),
		dataAlloc:       dataAlloc,
		inodeAlloc:      inodeAlloc,
		blockSize:       blockSize,
		dataRegionStart: sb.TrieNodePoolStart + sb.TrieNodePoolSize,
	}
	j, err := briefs.NewJournal(sb, dev.File(), blockSize)
	if err != nil {
		t.Fatalf("NewJournal: %v", err)
	}
	j.SetAllocatorSyncer(bfs)
	bfs.journal = j
	return bfs
}

func mkfsImage(t *testing.T, mkfsPath string, sizeBlocks int) string {
	t.Helper()
	img := filepath.Join(t.TempDir(), "test.briefs")
	out, err := exec.Command(mkfsPath, "-s", strconv.Itoa(sizeBlocks), img).CombinedOutput()
	if err != nil {
		t.Fatalf("mkfs: %v\n%s", err, out)
	}
	return img
}

func TestInodeAllocFree(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)

	bfs := openBridge(t, img)
	freeBefore := bfs.inodeAlloc.FreeCount()

	// Root inode is 1; the first allocation must be a fresh inode number > 1.
	in1, err := bfs.AllocInode(briefs.ModeFile|0o644, 1000, 1000, 1)
	if err != nil {
		t.Fatalf("AllocInode 1: %v", err)
	}
	if in1.InodeNumber <= 1 {
		t.Fatalf("allocated ino %d should be > 1", in1.InodeNumber)
	}
	in2, err := bfs.AllocInode(briefs.ModeDir|0o755, 1000, 1000, 1)
	if err != nil {
		t.Fatalf("AllocInode 2: %v", err)
	}
	if in2.InodeNumber == in1.InodeNumber {
		t.Fatalf("allocs must be distinct, both %d", in1.InodeNumber)
	}
	if in2.Nlinks != 2 {
		t.Fatalf("dir nlink: want 2, got %d", in2.Nlinks)
	}
	if in1.Nlinks != 1 {
		t.Fatalf("file nlink: want 1, got %d", in1.Nlinks)
	}
	if got := bfs.inodeAlloc.FreeCount(); got != freeBefore-2 {
		t.Fatalf("free count after 2 allocs: want %d, got %d", freeBefore-2, got)
	}

	// The on-disk inode must round-trip with the magic and mode we set.
	rt, err := bfs.inodes.ReadInode(in1.InodeNumber)
	if err != nil {
		t.Fatalf("ReadInode: %v", err)
	}
	if rt.Magic != briefs.MagicInode {
		t.Fatalf("round-trip magic: want 0x%x, got 0x%x", briefs.MagicInode, rt.Magic)
	}
	if rt.Filemode != briefs.ModeFile|0o644 {
		t.Fatalf("round-trip mode: want 0%o, got 0%o", briefs.ModeFile|0o644, rt.Filemode)
	}

	// Free both and confirm the bitmap is restored.
	if err := bfs.FreeInode(in1.InodeNumber); err != nil {
		t.Fatalf("FreeInode 1: %v", err)
	}
	if err := bfs.FreeInode(in2.InodeNumber); err != nil {
		t.Fatalf("FreeInode 2: %v", err)
	}
	if got := bfs.inodeAlloc.FreeCount(); got != freeBefore {
		t.Fatalf("free count after frees: want %d, got %d", freeBefore, got)
	}
	// A freed inode must read back with magic 0 (ZeroInode).
	rt2, err := bfs.inodes.ReadInode(in1.InodeNumber)
	if err != nil {
		t.Fatalf("ReadInode after free: %v", err)
	}
	if rt2.Magic != 0 {
		t.Fatalf("freed inode magic: want 0, got 0x%x", rt2.Magic)
	}

	// Checkpoint to leave a clean journal (log_start == log_end) so fsck can
	// run, then close the device before fsck opens the image.
	if err := bfs.journal.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	bfs.dev.Close()

	fsck := buildBinary(t, "github.com/ctdk/briefs-utils/cmd/fsck", "fsck.briefs")
	out, err := exec.Command(fsck, img).CombinedOutput()
	if err != nil {
		t.Fatalf("fsck failed: %v\n%s", err, out)
	}
	if !contains(string(out), "FSCK COMPLETE: no errors found") {
		t.Fatalf("fsck not clean after alloc/free round-trip:\n%s", out)
	}
}

// buildBinary mirrors cmd/fsck's helper: build a package binary into a temp dir.
func buildBinary(t *testing.T, pkg, name string) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, name)
	out, err := exec.Command("go", "build", "-o", bin, pkg).CombinedOutput()
	if err != nil {
		t.Fatalf("build %s: %v\n%s", name, err, out)
	}
	return bin
}

// contains is a substring test (kept local to avoid pulling strings.Contains
// into a package-level helper accidentally shadowing cmd/fsck's).
func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return len(substr) == 0
}