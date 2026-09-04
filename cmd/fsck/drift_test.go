package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/ctdk/briefs-utils/briefs"
)

// mkfsDriftImage runs mkfs on a 5000-block image and returns its path plus
// the parsed superblock. Fixture for the kernel-drift regression tests.
func mkfsDriftImage(t *testing.T) (imgPath string, sb *briefs.SuperblockLayout) {
	t.Helper()
	mkfsPath := buildBinary(t, "github.com/ctdk/briefs-utils/cmd/mkfs", "mkfs.briefs")
	imgPath = filepath.Join(t.TempDir(), "drift.briefs")
	if out, err := exec.Command(mkfsPath, "-s", "5000", imgPath).CombinedOutput(); err != nil {
		t.Fatalf("mkfs: %v\n%s", err, out)
	}
	f, err := os.Open(imgPath)
	if err != nil {
		t.Fatalf("open image: %v", err)
	}
	defer f.Close()
	sb, err = briefs.ReadSuperblock(f, 4096)
	if err != nil {
		t.Fatalf("read superblock: %v", err)
	}
	return imgPath, sb
}

// updateRootInode reads, mutates, and writes back the root inode (ino 1).
func updateRootInode(t *testing.T, imgPath string, sb *briefs.SuperblockLayout, mut func(in *briefs.Inode)) {
	t.Helper()
	f, err := os.OpenFile(imgPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open image: %v", err)
	}
	defer f.Close()

	blk, off := briefs.InodeLocation(sb, 1)
	buf := make([]byte, sb.BlockSize)
	if _, err := f.ReadAt(buf, int64(blk*sb.BlockSize)); err != nil {
		t.Fatalf("read inode block %d: %v", blk, err)
	}
	in, err := briefs.UnmarshalInode(buf[off : off+sb.InodeSize])
	if err != nil {
		t.Fatalf("unmarshal root inode: %v", err)
	}
	mut(in)
	if err := in.WriteAt(f, int64(blk*sb.BlockSize+off)); err != nil {
		t.Fatalf("write root inode: %v", err)
	}
}

// TestFsckInodeTableOverlapBound pins the E7 fix: the inode-table overlap
// bound is the table's CAPACITY (inode allocator header BlockCount), not the
// number of inodes found. A fresh image has one in-use inode (the root), so
// the old bound covered a single block; an extent squatting in the table's
// tail went unflagged.
func TestFsckInodeTableOverlapBound(t *testing.T) {
	fsckPath := buildBinary(t, "github.com/ctdk/briefs-utils/cmd/fsck", "fsck.briefs")
	imgPath, sb := mkfsDriftImage(t)

	f, err := os.Open(imgPath)
	if err != nil {
		t.Fatalf("open image: %v", err)
	}
	ah, err := briefs.ReadAllocatorHeader(f, sb.InodeBMOffset, sb.BlockSize)
	f.Close()
	if err != nil {
		t.Fatalf("read inode allocator header: %v", err)
	}
	tableBlocks := (ah.BlockCount*sb.InodeSize + sb.BlockSize - 1) / sb.BlockSize
	if tableBlocks < 2 {
		t.Fatalf("fixture too small: inode table is %d blocks", tableBlocks)
	}
	tailBlock := sb.InodeTableOffset + tableBlocks - 1

	// Point a (bogus) inline extent of the root inode at the table tail.
	// verifyExtentOverlaps must flag it regardless of how many inodes the
	// scan found (here: exactly one).
	updateRootInode(t, imgPath, sb, func(in *briefs.Inode) {
		exts := in.InlineExtents()
		exts[0] = briefs.Extent{Offset: 0, Phys: tailBlock, Len: 1}
		in.SetInlineExtents(exts)
		in.NumExtentsInline = 1
		in.NumExtentsTotal = 1
	})

	// fsck exits 0 even with errors (they are reported in output).
	out, err := exec.Command(fsckPath, imgPath).CombinedOutput()
	_ = err
	output := string(out)
	if !contains(output, "overlaps with inode table") {
		t.Errorf("fsck did not flag the inode-table-tail overlap (capacity bound):\n%s", output)
	}
}

// TestFsckUnknownUserFlagsAndGeneration pins the E8 checks: bits in
// user_flags outside the kernel's BRIEFS_USER_FLAG_ALL are reported, and a
// generation wider than 32 bits is reported (the kernel truncates it to
// u32 for NFS file handles and the 536 replay guard).
func TestFsckUnknownUserFlagsAndGeneration(t *testing.T) {
	fsckPath := buildBinary(t, "github.com/ctdk/briefs-utils/cmd/fsck", "fsck.briefs")
	imgPath, sb := mkfsDriftImage(t)

	updateRootInode(t, imgPath, sb, func(in *briefs.Inode) {
		in.UserFlags = briefs.UserFlagSync | 0x80000000 // one valid, one unknown
		in.Generation = 1 << 40
	})

	out, err := exec.Command(fsckPath, imgPath).CombinedOutput()
	if err != nil {
		t.Fatalf("fsck failed on user_flags/generation warnings:\n%s", out)
	}
	output := string(out)
	if !contains(output, "unknown bits 0x80000000") {
		t.Errorf("fsck did not report unknown user_flags bits:\n%s", output)
	}
	if !contains(output, "does not fit the kernel's 32-bit i_generation") {
		t.Errorf("fsck did not report an out-of-range generation:\n%s", output)
	}
}
