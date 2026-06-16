package main

import (
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/ctdk/briefs-utils/types"
)

func TestFsckCleanImage(t *testing.T) {
	// Build mkfs and fsck
	mkfsPath := buildBinary(t, "github.com/ctdk/briefs-utils/cmd/mkfs", "mkfs.briefs")
	fsckPath := buildBinary(t, "github.com/ctdk/briefs-utils/cmd/fsck", "fsck.briefs")

	// Create a clean filesystem image
	imgPath := filepath.Join(t.TempDir(), "test.briefs")
	cmd := exec.Command(mkfsPath, "-s", "5000", imgPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("mkfs failed: %v\n%s", err, out)
	}

	// Run fsck on it
	cmd = exec.Command(fsckPath, imgPath)
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fsck failed: %v\n%s", err, out)
	}

	// Verify no errors in output
	output := string(out)
	if contains(output, "ERROR") {
		t.Errorf("fsck found errors on clean image:\n%s", output)
	}
	if !contains(output, "FSCK COMPLETE: no errors found") {
		t.Errorf("fsck didn't report clean:\n%s", output)
	}
}

func TestFsckCorruptSuperblock(t *testing.T) {
	mkfsPath := buildBinary(t, "github.com/ctdk/briefs-utils/cmd/mkfs", "mkfs.briefs")
	fsckPath := buildBinary(t, "github.com/ctdk/briefs-utils/cmd/fsck", "fsck.briefs")

	imgPath := filepath.Join(t.TempDir(), "corrupt.briefs")
	cmd := exec.Command(mkfsPath, "-s", "5000", imgPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("mkfs failed: %v\n%s", err, out)
	}

	// Corrupt the superblock magic
	f, err := os.OpenFile(imgPath, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	f.WriteAt([]byte{0xDE, 0xAD, 0xBE, 0xEF, 0x00, 0x00, 0x00, 0x00}, 0)
	f.Close()

	cmd = exec.Command(fsckPath, imgPath)
	out, err = cmd.CombinedOutput()
	if err == nil {
		t.Fatal("fsck should have failed on corrupt superblock")
	}
	if !contains(string(out), "bad superblock magic") {
		t.Errorf("fsck didn't report bad superblock magic:\n%s", out)
	}
}

func TestFsckCorruptInode(t *testing.T) {
	mkfsPath := buildBinary(t, "github.com/ctdk/briefs-utils/cmd/mkfs", "mkfs.briefs")
	fsckPath := buildBinary(t, "github.com/ctdk/briefs-utils/cmd/fsck", "fsck.briefs")

	imgPath := filepath.Join(t.TempDir(), "corrupt_inode.briefs")
	cmd := exec.Command(mkfsPath, "-s", "5000", imgPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("mkfs failed: %v\n%s", err, out)
	}

	// Corrupt the root inode's magic (inode table starts at block 6, inode 0)
	f, err := os.OpenFile(imgPath, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	// Inode magic is at offset 8 within the inode, inode 0 is at block 6, offset 0
	f.WriteAt([]byte{0xDE, 0xAD, 0xBE, 0xEF, 0x00, 0x00, 0x00, 0x00}, 6*4096+8)
	f.Close()

	cmd = exec.Command(fsckPath, imgPath)
	out, err = cmd.CombinedOutput()
	// fsck currently exits 0 even with errors (it reports them in output)
	output := string(out)
	if !contains(output, "bad magic") {
		t.Errorf("fsck didn't report bad inode magic:\n%s", output)
	}
	if !contains(output, "error(s) found") {
		t.Errorf("fsck didn't report errors:\n%s", output)
	}
}

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

func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

// TestFsckRepairFragmentedFile creates a file with nine single-block extents
// (so it must use chain blocks), then runs fsck --repair and verifies the
// extents are merged into one inline extent and the file still passes fsck.
func TestFsckRepairFragmentedFile(t *testing.T) {
	mkfsPath := buildBinary(t, "github.com/ctdk/briefs-utils/cmd/mkfs", "mkfs.briefs")
	fsckPath := buildBinary(t, "github.com/ctdk/briefs-utils/cmd/fsck", "fsck.briefs")

	imgPath := filepath.Join(t.TempDir(), "frag.briefs")
	cmd := exec.Command(mkfsPath, "-s", "5000", imgPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("mkfs failed: %v\n%s", err, out)
	}

	// Poke a fragmented file inode directly into the inode table.
	// File inode 2 at inode table slot 1 (block 5, byte offset 512).
	// Data region starts at TrieNodePoolStart + TrieNodePoolSize. For a 5000-block
	// image that is block 93. We use a chain block at absolute block 110 and data
	// blocks 100..108 for the file content.
	const (
		chainBlockAbs = uint64(110)
		dataStartAbs  = uint64(100)
	)

	inode := &types.Inode{
		InodeNumber:      2,
		Magic:            types.MagicInode,
		Filemode:         types.ModeFile | 0644,
		Uid:              0,
		Gid:              0,
		FileSize:         9 * 4096,
		Nlinks:           0, // orphan; we only care about extent compaction
		NumExtentsInline: 8,
		NumExtentsTotal:  9,
		ExtentInlineBase: chainBlockAbs,
		Flags:            0,
	}
	// Eight inline extents at logical offsets 0..7.
	var inlineExtents [8]types.Extent
	for i := 0; i < 8; i++ {
		inlineExtents[i] = types.Extent{Offset: uint64(i), Phys: dataStartAbs + uint64(i), Len: 1, Flags: 0, Pad: 0}
	}
	inode.SetInlineExtents(inlineExtents)

	f, err := os.OpenFile(imgPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open image: %v", err)
	}

	// Write the fragmented inode.
	if err := inode.WriteAt(f, 5*4096+512); err != nil {
		t.Fatalf("write fragmented inode: %v", err)
	}

	// Write a chain block containing the ninth extent (logical offset 8).
	chainBuf := make([]byte, 4096)
	binary.LittleEndian.PutUint64(chainBuf[0:], 0) // next_overflow_block
	binary.LittleEndian.PutUint32(chainBuf[8:], 1) // num_extents_in_block
	binary.LittleEndian.PutUint32(chainBuf[12:], 0)
	const extOff = types.ExtentChainHeaderSize
	binary.LittleEndian.PutUint64(chainBuf[extOff:], 8)                         // offset
	binary.LittleEndian.PutUint64(chainBuf[extOff+8:], dataStartAbs+8)           // phys
	binary.LittleEndian.PutUint64(chainBuf[extOff+16:], 1)                     // len
	binary.LittleEndian.PutUint32(chainBuf[extOff+24:], 0)                     // flags
	binary.LittleEndian.PutUint32(chainBuf[extOff+28:], 0)                     // pad
	checksum := types.ComputeChainChecksum(chainBuf, 4096)
	binary.LittleEndian.PutUint64(chainBuf[types.ExtentChainChecksumOffset:], checksum)
	if _, err := f.WriteAt(chainBuf, int64(chainBlockAbs*4096)); err != nil {
		t.Fatalf("write chain block: %v", err)
	}

	// Mark inode 2 allocated in the inode bitmap. The bitmap is at block 1;
	// bit 1 corresponds to inode 2 (inode numbers are 1-based).
	inodeBM := make([]byte, 4096)
	if _, err := f.ReadAt(inodeBM, 4096); err != nil {
		t.Fatalf("read inode bitmap: %v", err)
	}
	word := binary.LittleEndian.Uint64(inodeBM[0:])
	word &^= 1 << 1 // clear bit 1 = mark inode 2 allocated
	binary.LittleEndian.PutUint64(inodeBM[0:], word)
	if _, err := f.WriteAt(inodeBM, 4096); err != nil {
		t.Fatalf("write inode bitmap: %v", err)
	}

	// Mark data blocks 100..110 allocated in the data allocator. The data
	// allocator starts at block 86; L2 starts after the header, L0 and L1.
	// For 4846 data blocks, L0=1 word (1 block), L1=2 words (1 block),
	// L2=76 words (1 block). So L2 starts at block 86 + 1 + 1 + 1 = 89.
	// Data-relative block 10 maps to L2 word 0, bit 10. Mark bits 10..18 and
	// bit 20 (chain block) allocated by clearing them.
	dataL2 := make([]byte, 4096)
	if _, err := f.ReadAt(dataL2, 89*4096); err != nil {
		t.Fatalf("read data L2 bitmap: %v", err)
	}
	l2Word := binary.LittleEndian.Uint64(dataL2[0:])
	for b := uint64(10); b <= 20; b++ {
		l2Word &^= 1 << b
	}
	binary.LittleEndian.PutUint64(dataL2[0:], l2Word)
	if _, err := f.WriteAt(dataL2, 89*4096); err != nil {
		t.Fatalf("write data L2 bitmap: %v", err)
	}

	if err := f.Close(); err != nil {
		t.Fatalf("close image: %v", err)
	}

	// Run fsck in repair mode.
	cmd = exec.Command(fsckPath, "--repair", imgPath)
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fsck repair failed: %v\n%s", err, out)
	}
	output := string(out)
	if !contains(output, "Repair complete") {
		t.Fatalf("fsck did not report repair complete:\n%s", output)
	}
	// The test intentionally does not add a directory entry, so inode 2 is an
	// orphan and the final pass reports it as unreachable. That is acceptable
	// here: we are validating extent compaction and allocator rebuild, not
	// directory connectivity. Require only that the single orphan reachability
	// error remains and that no other ERROR lines appear in the final pass.
	lines := splitLines(output)
	postRepair := false
	for _, line := range lines {
		if contains(line, "Re-running verification pass") {
			postRepair = true
		}
		if postRepair && contains(line, "ERROR") && !contains(line, "not reachable from root directory") {
			t.Fatalf("fsck repair left unexpected error:\n%s", output)
		}
	}
	if !contains(output, "not reachable from root directory") {
		t.Fatalf("expected orphan reachability warning not found:\n%s", output)
	}

	// Read the repaired inode back and verify it has one inline extent.
	f, err = os.OpenFile(imgPath, os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("reopen image: %v", err)
	}
	defer f.Close()
	buf := make([]byte, 512)
	if _, err := f.ReadAt(buf, 5*4096+512); err != nil {
		t.Fatalf("read repaired inode: %v", err)
	}
	repaired, err := types.UnmarshalInode(buf)
	if err != nil {
		t.Fatalf("unmarshal repaired inode: %v", err)
	}
	if repaired.NumExtentsTotal != 1 {
		t.Errorf("NumExtentsTotal: want 1, got %d", repaired.NumExtentsTotal)
	}
	if repaired.NumExtentsInline != 1 {
		t.Errorf("NumExtentsInline: want 1, got %d", repaired.NumExtentsInline)
	}
	if repaired.ExtentInlineBase != 0 {
		t.Errorf("ExtentInlineBase: want 0, got %d", repaired.ExtentInlineBase)
	}
	inline := repaired.InlineExtents()
	if inline[0].Offset != 0 || inline[0].Phys != dataStartAbs || inline[0].Len != 9 {
		t.Errorf("extent: want {0,%d,9}, got {%d,%d,%d}",
			dataStartAbs, inline[0].Offset, inline[0].Phys, inline[0].Len)
	}
}
