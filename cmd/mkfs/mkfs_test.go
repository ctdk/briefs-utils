// Tests for mkfs.briefs.
package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/ctdk/briefs-utils/types"
)

// buildBinary builds a Go binary and returns its path.
func buildBinary(t *testing.T, pkg, name string) string {
	t.Helper()
	dir := t.TempDir()
	binPath := filepath.Join(dir, name)
	cmd := exec.Command("go", "build", "-o", binPath, pkg)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build %s: %v\n%s", pkg, err, out)
	}
	return binPath
}

// contains reports whether s contains substr.
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// TestMkfsCreatesCleanFilesystem checks that a default mkfs produces an image
// that passes fsck with no errors.
func TestMkfsCreatesCleanFilesystem(t *testing.T) {
	mkfsPath := buildBinary(t, "github.com/ctdk/briefs-utils/cmd/mkfs", "mkfs.briefs")
	fsckPath := buildBinary(t, "github.com/ctdk/briefs-utils/cmd/fsck", "fsck.briefs")

	imgPath := filepath.Join(t.TempDir(), "clean.briefs")
	cmd := exec.Command(mkfsPath, "-s", "5000", imgPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("mkfs failed: %v\n%s", err, out)
	}

	cmd = exec.Command(fsckPath, imgPath)
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fsck failed: %v\n%s", err, out)
	}
	output := string(out)
	if contains(output, "ERROR") {
		t.Errorf("fsck found errors on clean image:\n%s", output)
	}
	if !contains(output, "FSCK COMPLETE: no errors found") {
		t.Errorf("fsck didn't report clean:\n%s", output)
	}
}

// TestMkfsDefaultGeometry verifies the default on-disk layout and root inode.
func TestMkfsDefaultGeometry(t *testing.T) {
	mkfsPath := buildBinary(t, "github.com/ctdk/briefs-utils/cmd/mkfs", "mkfs.briefs")

	imgPath := filepath.Join(t.TempDir(), "default.briefs")
	cmd := exec.Command(mkfsPath, "-s", "5000", imgPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("mkfs failed: %v\n%s", err, out)
	}

	f, err := os.Open(imgPath)
	if err != nil {
		t.Fatalf("open image: %v", err)
	}
	defer f.Close()

	sb, err := types.ReadSuperblock(f, 4096)
	if err != nil {
		t.Fatalf("read superblock: %v", err)
	}
	if sb.Magic != types.MagicSuperblock {
		t.Errorf("Magic: want 0x%X, got 0x%X", types.MagicSuperblock, sb.Magic)
	}
	if sb.BlockSize != 4096 {
		t.Errorf("BlockSize: want 4096, got %d", sb.BlockSize)
	}
	if sb.InodeSize != 512 {
		t.Errorf("InodeSize: want 512, got %d", sb.InodeSize)
	}
	if sb.TotalBlocks != 5000 {
		t.Errorf("TotalBlocks: want 5000, got %d", sb.TotalBlocks)
	}
	if sb.FreeInodes == 0 {
		t.Errorf("FreeInodes should be non-zero")
	}
	if sb.FreeDataBlks == 0 {
		t.Errorf("FreeDataBlks should be non-zero")
	}
	if sb.RootIno != 1 {
		t.Errorf("RootIno: want 1, got %d", sb.RootIno)
	}

	// Root inode is the first slot in the inode table.
	buf := make([]byte, 512)
	if _, err := f.ReadAt(buf, int64(sb.InodeTableOffset*sb.BlockSize)); err != nil {
		t.Fatalf("read root inode: %v", err)
	}
	root, err := types.UnmarshalInode(buf)
	if err != nil {
		t.Fatalf("unmarshal root inode: %v", err)
	}
	if root.InodeNumber != 1 {
		t.Errorf("root InodeNumber: want 1, got %d", root.InodeNumber)
	}
	if !root.IsDir() {
		t.Errorf("root inode should be a directory, got mode 0x%X", root.Filemode)
	}
	if root.Nlinks != 2 {
		t.Errorf("root Nlinks: want 2, got %d", root.Nlinks)
	}
	if root.ParentInode != 1 {
		t.Errorf("root ParentInode: want 1, got %d", root.ParentInode)
	}
	if root.DirTrieRoot == 0 {
		t.Errorf("root DirTrieRoot should be non-zero")
	}

	// Root directory trie page should have the TRNP magic.
	page := make([]byte, 4096)
	rootBlock := types.TrieRefBlock(root.DirTrieRoot)
	if _, err := f.ReadAt(page, int64(rootBlock*sb.BlockSize)); err != nil {
		t.Fatalf("read root trie page: %v", err)
	}
	magic := binary.LittleEndian.Uint32(page[0:])
	if magic != types.MagicTriePage {
		t.Errorf("root trie page magic: want 0x%X, got 0x%X", types.MagicTriePage, magic)
	}
	liveCount := binary.LittleEndian.Uint16(page[8:])
	if liveCount == 0 {
		t.Errorf("root trie live_count should be non-zero")
	}
}

// TestMkfsCustomBlockSize checks that valid non-default block sizes are accepted.
// The packed directory trie format requires at least a 4096-byte block, so only
// 4096 and 8192 are exercised here.
func TestMkfsCustomBlockSize(t *testing.T) {
	mkfsPath := buildBinary(t, "github.com/ctdk/briefs-utils/cmd/mkfs", "mkfs.briefs")
	fsckPath := buildBinary(t, "github.com/ctdk/briefs-utils/cmd/fsck", "fsck.briefs")

	for _, bs := range []string{"4096", "8192"} {
		imgPath := filepath.Join(t.TempDir(), "bs-"+bs+".briefs")
		cmd := exec.Command(mkfsPath, "-s", "5000", "-b", bs, imgPath)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("mkfs with block-size %s failed: %v\n%s", bs, err, out)
		}

		cmd = exec.Command(fsckPath, imgPath)
		out, err = cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("fsck with block-size %s failed: %v\n%s", bs, err, out)
		}
		if !contains(string(out), "FSCK COMPLETE: no errors found") {
			t.Errorf("fsck with block-size %s did not report clean:\n%s", bs, out)
		}
	}
}

// TestMkfsInvalidBlockSize verifies rejection of non-power-of-two block sizes.
func TestMkfsInvalidBlockSize(t *testing.T) {
	mkfsPath := buildBinary(t, "github.com/ctdk/briefs-utils/cmd/mkfs", "mkfs.briefs")

	imgPath := filepath.Join(t.TempDir(), "invalid.briefs")
	cmd := exec.Command(mkfsPath, "-s", "5000", "-b", "3000", imgPath)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("mkfs should have failed with non-power-of-two block size")
	}
	if !contains(string(out), "power of two") {
		t.Errorf("mkfs did not report power-of-two error:\n%s", out)
	}
}

// TestMkfsInvalidInodeSize verifies rejection of non-power-of-two inode sizes.
func TestMkfsInvalidInodeSize(t *testing.T) {
	mkfsPath := buildBinary(t, "github.com/ctdk/briefs-utils/cmd/mkfs", "mkfs.briefs")

	imgPath := filepath.Join(t.TempDir(), "invalid.briefs")
	cmd := exec.Command(mkfsPath, "-s", "5000", "--inode-size", "300", imgPath)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("mkfs should have failed with non-power-of-two inode size")
	}
	if !contains(string(out), "power of two") {
		t.Errorf("mkfs did not report power-of-two error:\n%s", out)
	}
}

// TestMkfsInvalidInodeRatio verifies rejection of inode-ratio < 1.
func TestMkfsInvalidInodeRatio(t *testing.T) {
	mkfsPath := buildBinary(t, "github.com/ctdk/briefs-utils/cmd/mkfs", "mkfs.briefs")

	imgPath := filepath.Join(t.TempDir(), "invalid.briefs")
	cmd := exec.Command(mkfsPath, "-s", "5000", "--inode-ratio", "0", imgPath)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("mkfs should have failed with inode-ratio 0")
	}
	if !contains(string(out), "at least 1") {
		t.Errorf("mkfs did not report inode-ratio error:\n%s", out)
	}
}

// TestMkfsTooSmall verifies that mkfs rejects volumes too small for metadata.
func TestMkfsTooSmall(t *testing.T) {
	mkfsPath := buildBinary(t, "github.com/ctdk/briefs-utils/cmd/mkfs", "mkfs.briefs")

	imgPath := filepath.Join(t.TempDir(), "small.briefs")
	cmd := exec.Command(mkfsPath, "-s", "10", imgPath)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("mkfs should have failed on tiny filesystem")
	}
	if !contains(string(out), "too small") {
		t.Errorf("mkfs did not report too-small error:\n%s", out)
	}
}

// TestMkfsMissingDevice verifies that mkfs errors when no device is provided.
func TestMkfsMissingDevice(t *testing.T) {
	mkfsPath := buildBinary(t, "github.com/ctdk/briefs-utils/cmd/mkfs", "mkfs.briefs")

	cmd := exec.Command(mkfsPath, "-s", "5000")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("mkfs should have failed with missing device")
	}
	if !contains(string(out), "missing required argument") {
		t.Errorf("mkfs did not report missing device error:\n%s", out)
	}
}

// TestMkfsLabel checks that the label is written into the superblock.
func TestMkfsLabel(t *testing.T) {
	mkfsPath := buildBinary(t, "github.com/ctdk/briefs-utils/cmd/mkfs", "mkfs.briefs")

	imgPath := filepath.Join(t.TempDir(), "label.briefs")
	cmd := exec.Command(mkfsPath, "-s", "5000", "--label", "TESTVOL", imgPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("mkfs failed: %v\n%s", err, out)
	}

	f, err := os.Open(imgPath)
	if err != nil {
		t.Fatalf("open image: %v", err)
	}
	defer f.Close()

	sb, err := types.ReadSuperblock(f, 4096)
	if err != nil {
		t.Fatalf("read superblock: %v", err)
	}
	label := string(bytes.TrimRight(sb.Label[:], "\x00"))
	if label != "TESTVOL" {
		t.Errorf("label: want TESTVOL, got %q", label)
	}
}

// TestMkfsJournalSize verifies the journal size flag.
func TestMkfsJournalSize(t *testing.T) {
	mkfsPath := buildBinary(t, "github.com/ctdk/briefs-utils/cmd/mkfs", "mkfs.briefs")
	fsckPath := buildBinary(t, "github.com/ctdk/briefs-utils/cmd/fsck", "fsck.briefs")

	imgPath := filepath.Join(t.TempDir(), "journal.briefs")
	cmd := exec.Command(mkfsPath, "-s", "5000", "-j", "128", imgPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("mkfs failed: %v\n%s", err, out)
	}

	f, err := os.Open(imgPath)
	if err != nil {
		t.Fatalf("open image: %v", err)
	}
	defer f.Close()

	sb, err := types.ReadSuperblock(f, 4096)
	if err != nil {
		t.Fatalf("read superblock: %v", err)
	}
	if sb.JournalBlocks != 128 {
		t.Errorf("JournalBlocks: want 128, got %d", sb.JournalBlocks)
	}
	if sb.JournalOffset+sb.JournalBlocks != sb.TotalBlocks {
		t.Errorf("journal should end at total blocks: offset %d + blocks %d != total %d",
			sb.JournalOffset, sb.JournalBlocks, sb.TotalBlocks)
	}

	cmd = exec.Command(fsckPath, imgPath)
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fsck failed: %v\n%s", err, out)
	}
	if !contains(string(out), "FSCK COMPLETE: no errors found") {
		t.Errorf("fsck did not report clean:\n%s", out)
	}
}

// TestMkfsInodeRatio checks that --inode-ratio affects the free inode count.
func TestMkfsInodeRatio(t *testing.T) {
	mkfsPath := buildBinary(t, "github.com/ctdk/briefs-utils/cmd/mkfs", "mkfs.briefs")

	imgPath := filepath.Join(t.TempDir(), "ratio.briefs")
	cmd := exec.Command(mkfsPath, "-s", "5000", "--inode-ratio", "16", imgPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("mkfs failed: %v\n%s", err, out)
	}

	f, err := os.Open(imgPath)
	if err != nil {
		t.Fatalf("open image: %v", err)
	}
	defer f.Close()

	sb, err := types.ReadSuperblock(f, 4096)
	if err != nil {
		t.Fatalf("read superblock: %v", err)
	}

	// mkfs computes estInodes = max(roundUp(totalBlocks/ratio, 32), 100) and
	// subtracts one for the root inode.
	estInodes := uint64(5000) / 16
	if estInodes < 100 {
		estInodes = 100
	}
	estInodes = ((estInodes + 31) / 32) * 32
	wantFree := estInodes - 1
	if sb.FreeInodes != wantFree {
		t.Errorf("FreeInodes: want %d, got %d", wantFree, sb.FreeInodes)
	}
}
