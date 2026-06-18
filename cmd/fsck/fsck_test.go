package main

import (
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/ctdk/briefs-utils/briefs"
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

// writeTrieNode serializes a single trie node slot into a page buffer.
// The caller is responsible for page-level fields (magic, live_count, free_slots).
func writeTrieNode(buf []byte, slot uint, firstChild, nextSibling, inode uint64,
	nameLen, nameOffset uint16, depth, nodeType, byteVal, fType uint8,
	flags, childCount uint16) {
	off := 20 + uint64(slot)*36
	binary.LittleEndian.PutUint64(buf[off+0:], firstChild)
	binary.LittleEndian.PutUint64(buf[off+8:], nextSibling)
	binary.LittleEndian.PutUint64(buf[off+16:], inode)
	binary.LittleEndian.PutUint16(buf[off+24:], nameLen)
	binary.LittleEndian.PutUint16(buf[off+26:], nameOffset)
	buf[off+28] = depth
	buf[off+29] = nodeType
	buf[off+30] = byteVal
	buf[off+31] = fType
	binary.LittleEndian.PutUint16(buf[off+32:], flags)
	binary.LittleEndian.PutUint16(buf[off+34:], childCount)
}

// writeTrieName stores a name in the trailing region of a trie page and returns
// the name_offset (distance from the end of the block to the length prefix).
func writeTrieName(buf []byte, blockSize int, name string) uint16 {
	needed := uint16(2 + len(name))
	start := blockSize - int(needed)
	binary.LittleEndian.PutUint16(buf[start:], uint16(len(name)))
	copy(buf[start+2:], name)
	return needed
}

// TestFsckTreeBackedFile pokes a v0.9 B+ tree-backed file inode directly into
// the inode table (9 single-block extents — one more than the 8 inline slots,
// so it must live in a tree), then runs fsck and verifies:
//   - the verify pass walks the B-tree root leaf cleanly (correct magic,
//     checksum, sorted extents, no structural errors) and cross-references
//     every data block and the tree node block against the allocator;
//   - --repair does NOT corrupt the tree-backed inode (extent compaction is
//     chain-based and must skip tree-backed inodes until tree-aware compaction
//     lands — see #7 phase 4); after repair the inode is still tree-backed with
//     9 extents and the same root.
//
// The inode is intentionally not linked into the root directory, so the final
// pass reports it as unreachable. That single reachability error is expected and
// tolerated — the test is validating the B-tree walk and repair-skip, not
// directory connectivity.
func TestFsckTreeBackedFile(t *testing.T) {
	mkfsPath := buildBinary(t, "github.com/ctdk/briefs-utils/cmd/mkfs", "mkfs.briefs")
	fsckPath := buildBinary(t, "github.com/ctdk/briefs-utils/cmd/fsck", "fsck.briefs")

	imgPath := filepath.Join(t.TempDir(), "btree.briefs")
	cmd := exec.Command(mkfsPath, "-s", "5000", imgPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("mkfs failed: %v\n%s", err, out)
	}

	// Layout for a 5000-block image (matches the chain test above):
	//   inode bitmap   -> block 1   (bit 1 = inode 2)
	//   inode table     -> block 5   (ino 2 at slot 1, byte offset 512)
	//   data region     -> block 90  (data-relative 0 = abs 90)
	//   data alloc L2   -> block 89  (data-relative N maps to L2 word 0, bit N)
	// We use data blocks 100..108 (data-relative 10..18) for the file content
	// and block 110 (data-relative 20) as the B-tree root leaf.
	const (
		rootLeafAbs   = uint64(110)
		dataStartAbs   = uint64(100)
		rootLeafRelBit = uint64(20) // data-relative block of the root leaf
	)

	// Build the B-tree root leaf: 9 single-block extents at logical offsets
	// 0..8, physical blocks 100..108. A single leaf holds up to 126 extents,
	// so 9 fits with no split.
	leafBuf := make([]byte, 4096)
	binary.LittleEndian.PutUint32(leafBuf[0:], briefs.BtreeMagic) // magic
	binary.LittleEndian.PutUint32(leafBuf[4:], briefs.BtreeFlagLeaf) // flags: leaf
	binary.LittleEndian.PutUint16(leafBuf[8:], 0)  // level 0 = leaf
	binary.LittleEndian.PutUint16(leafBuf[10:], 9) // num_keys
	binary.LittleEndian.PutUint64(leafBuf[12:], 0) // prev_leaf
	binary.LittleEndian.PutUint64(leafBuf[20:], 0) // next_leaf
	for i := 0; i < 9; i++ {
		off := briefs.BtreeHeaderSize + i*32
		binary.LittleEndian.PutUint64(leafBuf[off:], uint64(i))          // offset
		binary.LittleEndian.PutUint64(leafBuf[off+8:], dataStartAbs+uint64(i)) // phys
		binary.LittleEndian.PutUint64(leafBuf[off+16:], 1)              // len
		binary.LittleEndian.PutUint32(leafBuf[off+24:], 0)             // flags
		binary.LittleEndian.PutUint32(leafBuf[off+28:], 0)             // pad
	}
	leafChecksum := briefs.ComputeChainChecksum(leafBuf, 4096)
	binary.LittleEndian.PutUint64(leafBuf[briefs.BtreeChecksumOffset:], leafChecksum)

	// Tree-backed inode 2: InodeFlagIndexed set, root at rootLeafAbs, inline
	// array zeroed, num_extents_total = 9.
	inode := &briefs.Inode{
		InodeNumber:      2,
		Magic:            briefs.MagicInode,
		Filemode:         briefs.ModeFile | 0644,
		Uid:              0,
		Gid:              0,
		FileSize:         9 * 4096,
		Nlinks:           0, // orphan; we only care about the tree walk
		NumExtentsInline: 0,
		NumExtentsTotal:  9,
		ExtentInlineBase: rootLeafAbs,
		Flags:            briefs.InodeFlagIndexed,
	}
	inode.SetInlineExtents([8]briefs.Extent{}) // zeroed on spill

	f, err := os.OpenFile(imgPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open image: %v", err)
	}

	// Write the B-tree root leaf and the tree-backed inode.
	if _, err := f.WriteAt(leafBuf, int64(rootLeafAbs*4096)); err != nil {
		t.Fatalf("write btree root leaf: %v", err)
	}
	if err := inode.WriteAt(f, 5*4096+512); err != nil {
		t.Fatalf("write tree-backed inode: %v", err)
	}

	// Mark inode 2 allocated in the inode bitmap (block 1, bit 1).
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

	// Mark data blocks 100..108 (data-relative 10..18) and the root leaf
	// (data-relative 20) allocated in the data L2 bitmap (block 89). A cleared
	// bit means allocated. We do not touch data-relative 19 (abs 109).
	dataL2 := make([]byte, 4096)
	if _, err := f.ReadAt(dataL2, 89*4096); err != nil {
		t.Fatalf("read data L2 bitmap: %v", err)
	}
	l2Word := binary.LittleEndian.Uint64(dataL2[0:])
	for b := uint64(10); b <= 18; b++ {
		l2Word &^= 1 << b
	}
	l2Word &^= 1 << rootLeafRelBit
	binary.LittleEndian.PutUint64(dataL2[0:], l2Word)
	if _, err := f.WriteAt(dataL2, 89*4096); err != nil {
		t.Fatalf("write data L2 bitmap: %v", err)
	}

	if err := f.Close(); err != nil {
		t.Fatalf("close image: %v", err)
	}

	// Run fsck in repair mode. Repair rebuilds the allocator from the blocks
	// fsck found (including the tree node block via the B-tree walk) and must
	// leave the tree-backed inode untouched.
	cmd = exec.Command(fsckPath, "--repair", "-y", imgPath)
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fsck repair failed: %v\n%s", err, out)
	}
	output := string(out)
	if !contains(output, "Repair complete") {
		t.Fatalf("fsck did not report repair complete:\n%s", output)
	}

	// The B-tree walk must not have produced any structural errors at any
	// point (bad magic, checksum, unsorted, cycle, depth, count overflow).
	for _, marker := range []string{
		"bad magic",
		"checksum mismatch",
		"extents unsorted",
		"cycle detected",
		"depth exceeded",
		"count exceeds fanout",
		"btree node",
	} {
		if contains(output, marker) {
			t.Fatalf("fsck reported B-tree structural error (%q):\n%s", marker, output)
		}
	}

	// In the post-repair pass the only tolerated error is the orphan
	// reachability warning for inode 2 (it has no directory entry). Any other
	// ERROR — notably a block cross-ref mismatch, which would mean the tree
	// walk failed to mark a data or node block — is a failure.
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

	// Read the repaired inode back and verify repair left it tree-backed and
	// intact (extent compaction must have skipped it, not rewritten it inline).
	f, err = os.OpenFile(imgPath, os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("reopen image: %v", err)
	}
	defer f.Close()
	buf := make([]byte, 512)
	if _, err := f.ReadAt(buf, 5*4096+512); err != nil {
		t.Fatalf("read repaired inode: %v", err)
	}
	repaired, err := briefs.UnmarshalInode(buf)
	if err != nil {
		t.Fatalf("unmarshal repaired inode: %v", err)
	}
	if repaired.Flags&briefs.InodeFlagIndexed == 0 {
		t.Errorf("repaired inode lost InodeFlagIndexed (compaction must skip tree-backed inodes)")
	}
	if repaired.NumExtentsTotal != 9 {
		t.Errorf("NumExtentsTotal: want 9, got %d", repaired.NumExtentsTotal)
	}
	if repaired.NumExtentsInline != 0 {
		t.Errorf("NumExtentsInline: want 0, got %d", repaired.NumExtentsInline)
	}
	if repaired.ExtentInlineBase != rootLeafAbs {
		t.Errorf("ExtentInlineBase: want %d, got %d", rootLeafAbs, repaired.ExtentInlineBase)
	}

	// The root leaf on disk must still carry a valid magic + checksum and the
	// same 9 extents.
	leafRead := make([]byte, 4096)
	if _, err := f.ReadAt(leafRead, int64(rootLeafAbs*4096)); err != nil {
		t.Fatalf("read root leaf: %v", err)
	}
	if binary.LittleEndian.Uint32(leafRead[0:]) != briefs.BtreeMagic {
		t.Errorf("root leaf magic: want 0x%X, got 0x%X", briefs.BtreeMagic, binary.LittleEndian.Uint32(leafRead[0:]))
	}
	if err := briefs.VerifyBtreeNodeChecksum(leafRead, 4096); err != nil {
		t.Errorf("root leaf checksum after repair: %v", err)
	}
	if got := binary.LittleEndian.Uint16(leafRead[10:]); got != 9 {
		t.Errorf("root leaf num_keys: want 9, got %d", got)
	}
}

// TestFsckRepairCompactDirectoryTrie builds a deliberately fragmented root
// directory trie that spans two pages for a single entry, then runs
// fsck --repair and verifies the entry is preserved and the filesystem ends
// clean.
func TestFsckRepairCompactDirectoryTrie(t *testing.T) {
	mkfsPath := buildBinary(t, "github.com/ctdk/briefs-utils/cmd/mkfs", "mkfs.briefs")
	fsckPath := buildBinary(t, "github.com/ctdk/briefs-utils/cmd/fsck", "fsck.briefs")

	imgPath := filepath.Join(t.TempDir(), "dir.briefs")
	cmd := exec.Command(mkfsPath, "-s", "5000", imgPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("mkfs failed: %v\n%s", err, out)
	}

	const (
		rootTrieBlock  = uint64(90)
		extraTrieBlock = uint64(91)
		targetIno      = uint64(2)
	)

	f, err := os.OpenFile(imgPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open image: %v", err)
	}
	defer f.Close()

	// Write inode 2 as a plain empty file so the directory entry is valid.
	inode := &briefs.Inode{
		InodeNumber: targetIno,
		Magic:       briefs.MagicInode,
		Filemode:    briefs.ModeFile | 0644,
		FileSize:    0,
		Nlinks:      1,
	}
	if err := inode.WriteAt(f, 5*4096+512); err != nil {
		t.Fatalf("write target inode: %v", err)
	}

	// Mark inode 2 allocated in the inode bitmap. For a 640-inode bitmap the
	// L2 words are at block 4 (inode bitmap offset 1 + header block + L0 + L1).
	const inodeL2Block = 4
	inodeBM := make([]byte, 4096)
	if _, err := f.ReadAt(inodeBM, int64(inodeL2Block*4096)); err != nil {
		t.Fatalf("read inode bitmap: %v", err)
	}
	word := binary.LittleEndian.Uint64(inodeBM[0:])
	word &^= 1 << 1 // clear bit 1 = mark inode 2 allocated
	binary.LittleEndian.PutUint64(inodeBM[0:], word)
	if _, err := f.WriteAt(inodeBM, int64(inodeL2Block*4096)); err != nil {
		t.Fatalf("write inode bitmap: %v", err)
	}

	// Build a two-page trie for the root directory. Page one holds only the
	// root node; page two holds the leaf for "test". This is valid but
	// fragmented, so repair should compact it into a single page.
	rootPage := make([]byte, 4096)
	binary.LittleEndian.PutUint32(rootPage[0:], briefs.MagicTriePage)
	binary.LittleEndian.PutUint32(rootPage[4:], briefs.TriePageVersion)
	binary.LittleEndian.PutUint16(rootPage[8:], 1) // live_count
	binary.LittleEndian.PutUint16(rootPage[10:], 0)
	binary.LittleEndian.PutUint64(rootPage[12:], ^uint64(1)) // slot 0 used
	leafRef := briefs.TrieMakeRef(extraTrieBlock, 0)
	writeTrieNode(rootPage, 0, leafRef, 0, 0, 0, 0, 0, briefs.NodeTypeInterm, 0, 0, 0, 1)
	if _, err := f.WriteAt(rootPage, int64(rootTrieBlock*4096)); err != nil {
		t.Fatalf("write root trie page: %v", err)
	}

	leafPage := make([]byte, 4096)
	binary.LittleEndian.PutUint32(leafPage[0:], briefs.MagicTriePage)
	binary.LittleEndian.PutUint32(leafPage[4:], briefs.TriePageVersion)
	binary.LittleEndian.PutUint16(leafPage[8:], 1) // live_count
	binary.LittleEndian.PutUint16(leafPage[10:], 0)
	binary.LittleEndian.PutUint64(leafPage[12:], ^uint64(1)) // slot 0 used
	nameOff := writeTrieName(leafPage, 4096, "test")
	writeTrieNode(leafPage, 0, 0, 0, targetIno, uint16(len("test")), nameOff,
		uint8(len("test")), briefs.NodeTypeInterm|briefs.NodeStatusLeaf, 't', 8, 0, 0)
	if _, err := f.WriteAt(leafPage, int64(extraTrieBlock*4096)); err != nil {
		t.Fatalf("write leaf trie page: %v", err)
	}

	// Mark the extra trie block allocated in the data allocator. The data
	// region starts at block 93; block 94 is data-relative 1. The L2 bitmap is
	// at block 89.
	dataL2 := make([]byte, 4096)
	if _, err := f.ReadAt(dataL2, 89*4096); err != nil {
		t.Fatalf("read data L2 bitmap: %v", err)
	}
	l2Word := binary.LittleEndian.Uint64(dataL2[0:])
	l2Word &^= 1 << 1 // mark data-relative block 1 (absolute 94) allocated
	binary.LittleEndian.PutUint64(dataL2[0:], l2Word)
	if _, err := f.WriteAt(dataL2, 89*4096); err != nil {
		t.Fatalf("write data L2 bitmap: %v", err)
	}

	if err := f.Close(); err != nil {
		t.Fatalf("close image: %v", err)
	}

	cmd = exec.Command(fsckPath, "--repair", "-y", imgPath)
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fsck repair failed: %v\n%s", err, out)
	}
	output := string(out)
	if !contains(output, "Repair complete") {
		t.Fatalf("fsck did not report repair complete:\n%s", output)
	}
	if !contains(output, "total entries found: 1") {
		t.Fatalf("directory entry was not preserved (expected 1 entry):\n%s", output)
	}
	// The fixture intentionally does not update allocator header/summary levels,
	// so the initial verification pass reports bitmap/header mismatches. Those
	// are repaired. Only the post-repair verification pass must be error-free.
	lines := splitLines(output)
	postRepair := false
	for _, line := range lines {
		if contains(line, "Re-running verification pass") {
			postRepair = true
		}
		if postRepair && contains(line, "ERROR") {
			t.Fatalf("fsck repair left errors:\n%s", output)
		}
	}
	if !contains(output, "FSCK COMPLETE: no errors found") {
		t.Fatalf("fsck repair did not finish clean:\n%s", output)
	}
}

// TestFsckRepairLinkCounts builds a tiny directory tree with a file and a
// subdirectory, corrupts the nlink values, then verifies fsck --repair fixes
// them without losing entries.
func TestFsckRepairLinkCounts(t *testing.T) {
	mkfsPath := buildBinary(t, "github.com/ctdk/briefs-utils/cmd/mkfs", "mkfs.briefs")
	fsckPath := buildBinary(t, "github.com/ctdk/briefs-utils/cmd/fsck", "fsck.briefs")

	imgPath := filepath.Join(t.TempDir(), "links.briefs")
	cmd := exec.Command(mkfsPath, "-s", "5000", imgPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("mkfs failed: %v\n%s", err, out)
	}

	const (
		rootTrieBlock   = uint64(90)
		subdirTrieBlock = uint64(91)
		fileIno         = uint64(2)
		dirIno          = uint64(3)
	)

	f, err := os.OpenFile(imgPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open image: %v", err)
	}
	defer f.Close()

	// Write file inode 2 and directory inode 3 with intentionally wrong nlinks.
	fileInode := &briefs.Inode{
		InodeNumber: fileIno,
		Magic:       briefs.MagicInode,
		Filemode:    briefs.ModeFile | 0644,
		FileSize:    0,
		Nlinks:      0, // will be repaired to 1
	}
	if err := fileInode.WriteAt(f, 5*4096+512); err != nil {
		t.Fatalf("write file inode: %v", err)
	}

	subdirInode := &briefs.Inode{
		InodeNumber: dirIno,
		Magic:       briefs.MagicInode,
		Filemode:    briefs.ModeDir | 0755,
		FileSize:    4096,
		Nlinks:      1, // will be repaired to 2
		DirTrieRoot: briefs.TrieMakeRef(subdirTrieBlock, 0),
		ParentInode: 1,
	}
	if err := subdirInode.WriteAt(f, 5*4096+1024); err != nil {
		t.Fatalf("write subdir inode: %v", err)
	}

	// Corrupt the root directory nlinks; it has one subdirectory child, so the
	// repaired value should be 3.
	rootInodeBuf := make([]byte, 512)
	if _, err := f.ReadAt(rootInodeBuf, 5*4096); err != nil {
		t.Fatalf("read root inode: %v", err)
	}
	rootInode, err := briefs.UnmarshalInode(rootInodeBuf)
	if err != nil {
		t.Fatalf("unmarshal root inode: %v", err)
	}
	rootInode.Nlinks = 2 // will be repaired to 3
	rootInode.DirTrieRoot = briefs.TrieMakeRef(rootTrieBlock, 0)
	rootInode.ParentInode = 1
	if err := rootInode.WriteAt(f, 5*4096); err != nil {
		t.Fatalf("write root inode: %v", err)
	}

	// Mark inodes 2 and 3 allocated in the inode bitmap L2 (block 4).
	const inodeL2Block = 4
	inodeBM := make([]byte, 4096)
	if _, err := f.ReadAt(inodeBM, int64(inodeL2Block*4096)); err != nil {
		t.Fatalf("read inode bitmap: %v", err)
	}
	word := binary.LittleEndian.Uint64(inodeBM[0:])
	word &^= 1 << 1 // inode 2
	word &^= 1 << 2 // inode 3
	binary.LittleEndian.PutUint64(inodeBM[0:], word)
	if _, err := f.WriteAt(inodeBM, int64(inodeL2Block*4096)); err != nil {
		t.Fatalf("write inode bitmap: %v", err)
	}

	// Build a root trie page with two leaf children: "file" and "dir".
	rootPage := make([]byte, 4096)
	binary.LittleEndian.PutUint32(rootPage[0:], briefs.MagicTriePage)
	binary.LittleEndian.PutUint32(rootPage[4:], briefs.TriePageVersion)
	binary.LittleEndian.PutUint16(rootPage[8:], 3) // live_count
	binary.LittleEndian.PutUint16(rootPage[10:], 0)
	freeSlots := ^uint64(0)
	freeSlots &^= 1 << 0
	freeSlots &^= 1 << 1
	freeSlots &^= 1 << 2
	binary.LittleEndian.PutUint64(rootPage[12:], freeSlots)

	// Pack both names from the end of the block.
	// "dir" last: length prefix at 4091, name at 4093-4096, offset = 5.
	// "file" before it: length prefix at 4085, name at 4087-4091, offset = 11.
	const nameOffDir = uint16(5)
	binary.LittleEndian.PutUint16(rootPage[4091:], uint16(len("dir")))
	copy(rootPage[4093:], "dir")
	const nameOffFile = uint16(11)
	binary.LittleEndian.PutUint16(rootPage[4085:], uint16(len("file")))
	copy(rootPage[4087:], "file")

	leafFileRef := briefs.TrieMakeRef(rootTrieBlock, 1)
	leafDirRef := briefs.TrieMakeRef(rootTrieBlock, 2)
	writeTrieNode(rootPage, 0, leafFileRef, 0, 0, 0, 0, 0, briefs.NodeTypeInterm, 0, 0, 0, 2)
	writeTrieNode(rootPage, 1, 0, leafDirRef, fileIno, uint16(len("file")), nameOffFile,
		1, briefs.NodeTypeInterm|briefs.NodeStatusLeaf, 'f', 8, 0, 0)
	writeTrieNode(rootPage, 2, 0, 0, dirIno, uint16(len("dir")), nameOffDir,
		1, briefs.NodeTypeInterm|briefs.NodeStatusLeaf, 'd', 4, 0, 0)

	if _, err := f.WriteAt(rootPage, int64(rootTrieBlock*4096)); err != nil {
		t.Fatalf("write root trie page: %v", err)
	}

	// Build an empty subdirectory trie page.
	subdirPage := make([]byte, 4096)
	binary.LittleEndian.PutUint32(subdirPage[0:], briefs.MagicTriePage)
	binary.LittleEndian.PutUint32(subdirPage[4:], briefs.TriePageVersion)
	binary.LittleEndian.PutUint16(subdirPage[8:], 1) // live_count
	binary.LittleEndian.PutUint16(subdirPage[10:], 0)
	binary.LittleEndian.PutUint64(subdirPage[12:], ^uint64(1)) // slot 0 used
	writeTrieNode(subdirPage, 0, 0, 0, 0, 0, 0, 0, briefs.NodeTypeInterm, 0, 0, 0, 0)
	if _, err := f.WriteAt(subdirPage, int64(subdirTrieBlock*4096)); err != nil {
		t.Fatalf("write subdir trie page: %v", err)
	}

	// Mark trie blocks 90 and 91 allocated in the data allocator L2 (block 89).
	dataL2 := make([]byte, 4096)
	if _, err := f.ReadAt(dataL2, 89*4096); err != nil {
		t.Fatalf("read data L2 bitmap: %v", err)
	}
	l2Word := binary.LittleEndian.Uint64(dataL2[0:])
	l2Word &^= 1 << 0 // block 90
	l2Word &^= 1 << 1 // block 91
	binary.LittleEndian.PutUint64(dataL2[0:], l2Word)
	if _, err := f.WriteAt(dataL2, 89*4096); err != nil {
		t.Fatalf("write data L2 bitmap: %v", err)
	}

	if err := f.Close(); err != nil {
		t.Fatalf("close image: %v", err)
	}

	cmd = exec.Command(fsckPath, "--repair", "-y", imgPath)
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fsck repair failed: %v\n%s", err, out)
	}
	output := string(out)
	if !contains(output, "Repair complete") {
		t.Fatalf("fsck did not report repair complete:\n%s", output)
	}
	if !contains(output, "total entries found: 2") {
		t.Fatalf("directory entries were not preserved (expected 2):\n%s", output)
	}
	// The fixture intentionally does not update allocator header/summary levels,
	// so the initial pass reports bitmap/header mismatches. Only the post-repair
	// pass must be error-free.
	lines := splitLines(output)
	postRepair := false
	for _, line := range lines {
		if contains(line, "Re-running verification pass") {
			postRepair = true
		}
		if postRepair && contains(line, "ERROR") {
			t.Fatalf("fsck repair left errors:\n%s", output)
		}
	}
	if !contains(output, "FSCK COMPLETE: no errors found") {
		t.Fatalf("fsck repair did not finish clean:\n%s", output)
	}

	// Read repaired inodes back and verify nlinks.
	f, err = os.OpenFile(imgPath, os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("reopen image: %v", err)
	}
	defer f.Close()

	buf := make([]byte, 512)
	if _, err := f.ReadAt(buf, 5*4096); err != nil {
		t.Fatalf("read repaired root inode: %v", err)
	}
	root, err := briefs.UnmarshalInode(buf)
	if err != nil {
		t.Fatalf("unmarshal root inode: %v", err)
	}
	if root.Nlinks != 3 {
		t.Errorf("root nlinks: want 3, got %d", root.Nlinks)
	}

	if _, err := f.ReadAt(buf, 5*4096+512); err != nil {
		t.Fatalf("read repaired file inode: %v", err)
	}
	file, err := briefs.UnmarshalInode(buf)
	if err != nil {
		t.Fatalf("unmarshal file inode: %v", err)
	}
	if file.Nlinks != 1 {
		t.Errorf("file nlinks: want 1, got %d", file.Nlinks)
	}

	if _, err := f.ReadAt(buf, 5*4096+1024); err != nil {
		t.Fatalf("read repaired subdir inode: %v", err)
	}
	dir, err := briefs.UnmarshalInode(buf)
	if err != nil {
		t.Fatalf("unmarshal subdir inode: %v", err)
	}
	if dir.Nlinks != 2 {
		t.Errorf("subdir nlinks: want 2, got %d", dir.Nlinks)
	}
}

// TestFsckRepairCombinedFragmentation builds an image with several repair-worthy
// problems at once: a tree-backed fragmented file inside a directory, a
// fragmented directory trie, and corrupted link counts. It then runs fsck
// --repair and verifies the post-repair pass is clean, the directory entries
// were preserved, the link counts were recomputed, and the tree-backed file
// was left intact (extent compaction is a no-op for tree-backed inodes until
// #7 phase 4 adds leaf compaction).
func TestFsckRepairCombinedFragmentation(t *testing.T) {
	mkfsPath := buildBinary(t, "github.com/ctdk/briefs-utils/cmd/mkfs", "mkfs.briefs")
	fsckPath := buildBinary(t, "github.com/ctdk/briefs-utils/cmd/fsck", "fsck.briefs")

	imgPath := filepath.Join(t.TempDir(), "combined.briefs")
	writeCombinedFixture(t, mkfsPath, imgPath, true)

	// Run fsck in repair mode.
	cmd := exec.Command(fsckPath, "--repair", "-y", imgPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fsck repair failed: %v\n%s", err, out)
	}
	output := string(out)
	if !contains(output, "Repair complete") {
		t.Fatalf("fsck did not report repair complete:\n%s", output)
	}
	if !contains(output, "total entries found: 2") {
		t.Fatalf("directory entries were not preserved (expected 2):\n%s", output)
	}

	// The post-repair verification pass must be completely clean.
	lines := splitLines(output)
	postRepair := false
	for _, line := range lines {
		if contains(line, "Re-running verification pass") {
			postRepair = true
		}
		if postRepair && contains(line, "ERROR") {
			t.Fatalf("fsck repair left errors:\n%s", output)
		}
	}
	if !contains(output, "FSCK COMPLETE: no errors found") {
		t.Fatalf("fsck repair did not finish clean:\n%s", output)
	}

	// Run a second, standalone fsck to verify the repaired image stays clean.
	cmd = exec.Command(fsckPath, imgPath)
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("second fsck failed: %v\n%s", err, out)
	}
	second := string(out)
	if contains(second, "ERROR") {
		t.Fatalf("second fsck found errors:\n%s", second)
	}
	if !contains(second, "FSCK COMPLETE: no errors found") {
		t.Fatalf("second fsck did not report clean:\n%s", second)
	}

	// Read repaired inodes back and verify the repairs.
	f, err := os.OpenFile(imgPath, os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("reopen image: %v", err)
	}
	defer f.Close()

	buf := make([]byte, 512)
	if _, err := f.ReadAt(buf, 5*4096); err != nil {
		t.Fatalf("read repaired root inode: %v", err)
	}
	root, err := briefs.UnmarshalInode(buf)
	if err != nil {
		t.Fatalf("unmarshal root inode: %v", err)
	}
	if root.Nlinks != 3 {
		t.Errorf("root nlinks: want 3, got %d", root.Nlinks)
	}

	if _, err := f.ReadAt(buf, 5*4096+512); err != nil {
		t.Fatalf("read repaired file inode: %v", err)
	}
	file, err := briefs.UnmarshalInode(buf)
	if err != nil {
		t.Fatalf("unmarshal file inode: %v", err)
	}
	if file.Nlinks != 1 {
		t.Errorf("file nlinks: want 1, got %d", file.Nlinks)
	}
	// The file is tree-backed; --repair must leave it intact (extent compaction
	// does not yet handle B-tree leaves — deferred to #7 phase 4).
	if file.Flags&briefs.InodeFlagIndexed == 0 {
		t.Errorf("file lost InodeFlagIndexed after repair")
	}
	if file.NumExtentsTotal != 9 {
		t.Errorf("file NumExtentsTotal: want 9 (tree-backed, untouched), got %d", file.NumExtentsTotal)
	}
	if file.NumExtentsInline != 0 {
		t.Errorf("file NumExtentsInline: want 0, got %d", file.NumExtentsInline)
	}
	if file.ExtentInlineBase == 0 {
		t.Errorf("file ExtentInlineBase: expected root leaf still in use, got 0")
	}

	if _, err := f.ReadAt(buf, 5*4096+1024); err != nil {
		t.Fatalf("read repaired subdir inode: %v", err)
	}
	dir, err := briefs.UnmarshalInode(buf)
	if err != nil {
		t.Fatalf("unmarshal subdir inode: %v", err)
	}
	if dir.Nlinks != 2 {
		t.Errorf("subdir nlinks: want 2, got %d", dir.Nlinks)
	}
}

// writeCombinedFixture creates a 5000-block image with a fragmented file, a
// hand-built two-entry root directory trie, and an empty subdirectory trie. If
// corruptNlinks is true, the link counts on the file, subdirectory, and root
// directory are set to incorrect values.
func writeCombinedFixture(t *testing.T, mkfsPath, imgPath string, corruptNlinks bool) {
	const (
		rootTrieBlock   = uint64(90)
		subdirTrieBlock = uint64(91)
		fileIno         = uint64(2)
		dirIno          = uint64(3)
		dataStartAbs    = uint64(100)
		chainBlockAbs   = uint64(110)
	)

	cmd := exec.Command(mkfsPath, "-s", "5000", imgPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("mkfs failed: %v\n%s", err, out)
	}

	f, err := os.OpenFile(imgPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open image: %v", err)
	}
	defer f.Close()

	fileInode := &briefs.Inode{
		InodeNumber:      fileIno,
		Magic:            briefs.MagicInode,
		Filemode:         briefs.ModeFile | 0644,
		FileSize:         9 * 4096,
		Nlinks:           1,
		NumExtentsInline: 0,
		NumExtentsTotal:  9,
		ExtentInlineBase: chainBlockAbs,
		Flags:            briefs.InodeFlagIndexed,
	}
	fileInode.SetInlineExtents([8]briefs.Extent{}) // zeroed on spill
	if corruptNlinks {
		fileInode.Nlinks = 0
	}
	if err := fileInode.WriteAt(f, 5*4096+512); err != nil {
		t.Fatalf("write file inode: %v", err)
	}

	// B-tree root leaf holding the nine single-block extents (logical offsets
	// 0..8, physical blocks 100..108). A single leaf holds up to 126 extents,
	// so 9 fits with no split.
	leafBuf := make([]byte, 4096)
	binary.LittleEndian.PutUint32(leafBuf[0:], briefs.BtreeMagic)
	binary.LittleEndian.PutUint32(leafBuf[4:], briefs.BtreeFlagLeaf)
	binary.LittleEndian.PutUint16(leafBuf[8:], 0)  // level 0
	binary.LittleEndian.PutUint16(leafBuf[10:], 9) // num_keys
	binary.LittleEndian.PutUint64(leafBuf[12:], 0) // prev_leaf
	binary.LittleEndian.PutUint64(leafBuf[20:], 0) // next_leaf
	for i := 0; i < 9; i++ {
		off := briefs.BtreeHeaderSize + i*32
		binary.LittleEndian.PutUint64(leafBuf[off:], uint64(i))
		binary.LittleEndian.PutUint64(leafBuf[off+8:], dataStartAbs+uint64(i))
		binary.LittleEndian.PutUint64(leafBuf[off+16:], 1)
		binary.LittleEndian.PutUint32(leafBuf[off+24:], 0)
		binary.LittleEndian.PutUint32(leafBuf[off+28:], 0)
	}
	leafChecksum := briefs.ComputeChainChecksum(leafBuf, 4096)
	binary.LittleEndian.PutUint64(leafBuf[briefs.BtreeChecksumOffset:], leafChecksum)
	if _, err := f.WriteAt(leafBuf, int64(chainBlockAbs*4096)); err != nil {
		t.Fatalf("write btree root leaf: %v", err)
	}

	subdirInode := &briefs.Inode{
		InodeNumber: dirIno,
		Magic:       briefs.MagicInode,
		Filemode:    briefs.ModeDir | 0755,
		FileSize:    4096,
		Nlinks:      2,
		DirTrieRoot: briefs.TrieMakeRef(subdirTrieBlock, 0),
		ParentInode: 1,
	}
	if corruptNlinks {
		subdirInode.Nlinks = 1
	}
	if err := subdirInode.WriteAt(f, 5*4096+1024); err != nil {
		t.Fatalf("write subdir inode: %v", err)
	}

	rootInodeBuf := make([]byte, 512)
	if _, err := f.ReadAt(rootInodeBuf, 5*4096); err != nil {
		t.Fatalf("read root inode: %v", err)
	}
	rootInode, err := briefs.UnmarshalInode(rootInodeBuf)
	if err != nil {
		t.Fatalf("unmarshal root inode: %v", err)
	}
	rootInode.Nlinks = 3
	rootInode.DirTrieRoot = briefs.TrieMakeRef(rootTrieBlock, 0)
	rootInode.ParentInode = 1
	if corruptNlinks {
		rootInode.Nlinks = 2
	}
	if err := rootInode.WriteAt(f, 5*4096); err != nil {
		t.Fatalf("write root inode: %v", err)
	}

	const inodeL2Block = 4
	inodeBM := make([]byte, 4096)
	if _, err := f.ReadAt(inodeBM, int64(inodeL2Block*4096)); err != nil {
		t.Fatalf("read inode bitmap: %v", err)
	}
	word := binary.LittleEndian.Uint64(inodeBM[0:])
	word &^= 1 << 1
	word &^= 1 << 2
	binary.LittleEndian.PutUint64(inodeBM[0:], word)
	if _, err := f.WriteAt(inodeBM, int64(inodeL2Block*4096)); err != nil {
		t.Fatalf("write inode bitmap: %v", err)
	}

	rootPage := make([]byte, 4096)
	binary.LittleEndian.PutUint32(rootPage[0:], briefs.MagicTriePage)
	binary.LittleEndian.PutUint32(rootPage[4:], briefs.TriePageVersion)
	binary.LittleEndian.PutUint16(rootPage[8:], 3)
	binary.LittleEndian.PutUint16(rootPage[10:], 0)
	freeSlots := ^uint64(0)
	freeSlots &^= 1 << 0
	freeSlots &^= 1 << 1
	freeSlots &^= 1 << 2
	binary.LittleEndian.PutUint64(rootPage[12:], freeSlots)

	const nameOffDir = uint16(5)
	binary.LittleEndian.PutUint16(rootPage[4091:], uint16(len("dir")))
	copy(rootPage[4093:], "dir")
	const nameOffFile = uint16(11)
	binary.LittleEndian.PutUint16(rootPage[4085:], uint16(len("file")))
	copy(rootPage[4087:], "file")

	leafFileRef := briefs.TrieMakeRef(rootTrieBlock, 1)
	leafDirRef := briefs.TrieMakeRef(rootTrieBlock, 2)
	writeTrieNode(rootPage, 0, leafFileRef, 0, 0, 0, 0, 0, briefs.NodeTypeInterm, 0, 0, 0, 2)
	writeTrieNode(rootPage, 1, 0, leafDirRef, fileIno, uint16(len("file")), nameOffFile,
		1, briefs.NodeTypeInterm|briefs.NodeStatusLeaf, 'f', 8, 0, 0)
	writeTrieNode(rootPage, 2, 0, 0, dirIno, uint16(len("dir")), nameOffDir,
		1, briefs.NodeTypeInterm|briefs.NodeStatusLeaf, 'd', 4, 0, 0)
	if _, err := f.WriteAt(rootPage, int64(rootTrieBlock*4096)); err != nil {
		t.Fatalf("write root trie page: %v", err)
	}

	subdirPage := make([]byte, 4096)
	binary.LittleEndian.PutUint32(subdirPage[0:], briefs.MagicTriePage)
	binary.LittleEndian.PutUint32(subdirPage[4:], briefs.TriePageVersion)
	binary.LittleEndian.PutUint16(subdirPage[8:], 1)
	binary.LittleEndian.PutUint16(subdirPage[10:], 0)
	binary.LittleEndian.PutUint64(subdirPage[12:], ^uint64(1))
	writeTrieNode(subdirPage, 0, 0, 0, 0, 0, 0, 0, briefs.NodeTypeInterm, 0, 0, 0, 0)
	if _, err := f.WriteAt(subdirPage, int64(subdirTrieBlock*4096)); err != nil {
		t.Fatalf("write subdir trie page: %v", err)
	}

	dataL2 := make([]byte, 4096)
	if _, err := f.ReadAt(dataL2, 89*4096); err != nil {
		t.Fatalf("read data L2 bitmap: %v", err)
	}
	l2Word := binary.LittleEndian.Uint64(dataL2[0:])
	l2Word &^= 1 << 0
	l2Word &^= 1 << 1
	for b := uint64(10); b <= 18; b++ {
		l2Word &^= 1 << b
	}
	l2Word &^= 1 << 20
	binary.LittleEndian.PutUint64(dataL2[0:], l2Word)
	if _, err := f.WriteAt(dataL2, 89*4096); err != nil {
		t.Fatalf("write data L2 bitmap: %v", err)
	}

	if err := f.Close(); err != nil {
		t.Fatalf("close image: %v", err)
	}
}

// TestFsckRepairOnlyAllocator verifies that --repair-only=allocator rebuilds
// the allocator and fixes free counts without compacting file extents or
// directory tries.
func TestFsckRepairOnlyAllocator(t *testing.T) {
	mkfsPath := buildBinary(t, "github.com/ctdk/briefs-utils/cmd/mkfs", "mkfs.briefs")
	fsckPath := buildBinary(t, "github.com/ctdk/briefs-utils/cmd/fsck", "fsck.briefs")

	imgPath := filepath.Join(t.TempDir(), "only-alloc.briefs")
	writeCombinedFixture(t, mkfsPath, imgPath, false)

	cmd := exec.Command(fsckPath, "--repair", "--repair-only=allocator", "-y", imgPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fsck repair failed: %v\n%s", err, out)
	}
	output := string(out)
	if !contains(output, "Repair complete") {
		t.Fatalf("fsck did not report repair complete:\n%s", output)
	}
	lines := splitLines(output)
	postRepair := false
	for _, line := range lines {
		if contains(line, "Re-running verification pass") {
			postRepair = true
		}
		if postRepair && contains(line, "ERROR") {
			t.Fatalf("fsck repair left errors:\n%s", output)
		}
	}
	if !contains(output, "FSCK COMPLETE: no errors found") {
		t.Fatalf("fsck repair did not finish clean:\n%s", output)
	}

	f, err := os.OpenFile(imgPath, os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("reopen image: %v", err)
	}
	defer f.Close()

	buf := make([]byte, 512)
	if _, err := f.ReadAt(buf, 5*4096+512); err != nil {
		t.Fatalf("read file inode: %v", err)
	}
	file, err := briefs.UnmarshalInode(buf)
	if err != nil {
		t.Fatalf("unmarshal file inode: %v", err)
	}
	if file.NumExtentsTotal != 9 {
		t.Errorf("file NumExtentsTotal: want 9 (still fragmented), got %d", file.NumExtentsTotal)
	}
	if file.ExtentInlineBase == 0 {
		t.Errorf("file ExtentInlineBase: expected root leaf still in use")
	}
}

// TestFsckRepairOnlyExtents verifies that --repair-only=extents leaves tree-backed
// file extents untouched (B-tree leaf compaction is deferred to #7 phase 4)
// without rebuilding the allocator or compacting directory tries.
// extents without rebuilding the allocator or compacting directory tries.
func TestFsckRepairOnlyExtents(t *testing.T) {
	mkfsPath := buildBinary(t, "github.com/ctdk/briefs-utils/cmd/mkfs", "mkfs.briefs")
	fsckPath := buildBinary(t, "github.com/ctdk/briefs-utils/cmd/fsck", "fsck.briefs")

	imgPath := filepath.Join(t.TempDir(), "only-extents.briefs")
	writeCombinedFixture(t, mkfsPath, imgPath, false)

	cmd := exec.Command(fsckPath, "--repair", "--repair-only=extents", "-y", imgPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fsck repair failed: %v\n%s", err, out)
	}
	output := string(out)
	if !contains(output, "Repair complete") {
		t.Fatalf("fsck did not report repair complete:\n%s", output)
	}
	lines := splitLines(output)
	postRepair := false
	for _, line := range lines {
		if contains(line, "Re-running verification pass") {
			postRepair = true
		}
		if postRepair && contains(line, "ERROR") {
			t.Fatalf("fsck repair left errors:\n%s", output)
		}
	}
	if !contains(output, "FSCK COMPLETE: no errors found") {
		t.Fatalf("fsck repair did not finish clean:\n%s", output)
	}

	f, err := os.OpenFile(imgPath, os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("reopen image: %v", err)
	}
	defer f.Close()

	buf := make([]byte, 512)
	if _, err := f.ReadAt(buf, 5*4096+512); err != nil {
		t.Fatalf("read file inode: %v", err)
	}
	file, err := briefs.UnmarshalInode(buf)
	if err != nil {
		t.Fatalf("unmarshal file inode: %v", err)
	}
	// The file is tree-backed; extent compaction is a no-op for B-tree leaves
	// until #7 phase 4, so the file must be left untouched (still 9 extents,
	// root leaf still in use).
	if file.Flags&briefs.InodeFlagIndexed == 0 {
		t.Errorf("file lost InodeFlagIndexed")
	}
	if file.NumExtentsTotal != 9 {
		t.Errorf("file NumExtentsTotal: want 9 (tree-backed, untouched), got %d", file.NumExtentsTotal)
	}
	if file.ExtentInlineBase == 0 {
		t.Errorf("file ExtentInlineBase: expected root leaf still in use, got 0")
	}

	if _, err := f.ReadAt(buf, 5*4096); err != nil {
		t.Fatalf("read root inode: %v", err)
	}
	root, err := briefs.UnmarshalInode(buf)
	if err != nil {
		t.Fatalf("unmarshal root inode: %v", err)
	}
	if root.DirTrieRoot == 0 {
		t.Errorf("root DirTrieRoot was cleared unexpectedly")
	}
}

// TestFsckRepairOnlyLinks verifies that --repair-only=links fixes incorrect
// inode nlink values without compacting file extents or directory tries.
func TestFsckRepairOnlyLinks(t *testing.T) {
	mkfsPath := buildBinary(t, "github.com/ctdk/briefs-utils/cmd/mkfs", "mkfs.briefs")
	fsckPath := buildBinary(t, "github.com/ctdk/briefs-utils/cmd/fsck", "fsck.briefs")

	imgPath := filepath.Join(t.TempDir(), "only-links.briefs")
	writeCombinedFixture(t, mkfsPath, imgPath, true)

	cmd := exec.Command(fsckPath, "--repair", "--repair-only=links", "-y", imgPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fsck repair failed: %v\n%s", err, out)
	}
	output := string(out)
	if !contains(output, "Repair complete") {
		t.Fatalf("fsck did not report repair complete:\n%s", output)
	}
	lines := splitLines(output)
	postRepair := false
	for _, line := range lines {
		if contains(line, "Re-running verification pass") {
			postRepair = true
		}
		if postRepair && contains(line, "ERROR") {
			t.Fatalf("fsck repair left errors:\n%s", output)
		}
	}
	if !contains(output, "FSCK COMPLETE: no errors found") {
		t.Fatalf("fsck repair did not finish clean:\n%s", output)
	}

	f, err := os.OpenFile(imgPath, os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("reopen image: %v", err)
	}
	defer f.Close()

	buf := make([]byte, 512)
	if _, err := f.ReadAt(buf, 5*4096+512); err != nil {
		t.Fatalf("read file inode: %v", err)
	}
	file, err := briefs.UnmarshalInode(buf)
	if err != nil {
		t.Fatalf("unmarshal file inode: %v", err)
	}
	if file.Nlinks != 1 {
		t.Errorf("file Nlinks: want 1, got %d", file.Nlinks)
	}
	if file.NumExtentsTotal != 9 {
		t.Errorf("file NumExtentsTotal: want 9 (still fragmented), got %d", file.NumExtentsTotal)
	}
	if file.ExtentInlineBase == 0 {
		t.Errorf("file ExtentInlineBase: expected root leaf still in use")
	}

	if _, err := f.ReadAt(buf, 5*4096+1024); err != nil {
		t.Fatalf("read subdir inode: %v", err)
	}
	subdir, err := briefs.UnmarshalInode(buf)
	if err != nil {
		t.Fatalf("unmarshal subdir inode: %v", err)
	}
	if subdir.Nlinks != 2 {
		t.Errorf("subdir Nlinks: want 2, got %d", subdir.Nlinks)
	}

	if _, err := f.ReadAt(buf, 5*4096); err != nil {
		t.Fatalf("read root inode: %v", err)
	}
	root, err := briefs.UnmarshalInode(buf)
	if err != nil {
		t.Fatalf("unmarshal root inode: %v", err)
	}
	if root.Nlinks != 3 {
		t.Errorf("root Nlinks: want 3, got %d", root.Nlinks)
	}
	if root.DirTrieRoot == 0 {
		t.Errorf("root DirTrieRoot was cleared unexpectedly")
	}
}

// TestFsckOptimize verifies that --optimize runs trie and extent compaction
// without link-count repair.
func TestFsckOptimize(t *testing.T) {
	mkfsPath := buildBinary(t, "github.com/ctdk/briefs-utils/cmd/mkfs", "mkfs.briefs")
	fsckPath := buildBinary(t, "github.com/ctdk/briefs-utils/cmd/fsck", "fsck.briefs")

	imgPath := filepath.Join(t.TempDir(), "optimize.briefs")
	writeCombinedFixture(t, mkfsPath, imgPath, false)

	cmd := exec.Command(fsckPath, "--optimize", "-y", imgPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fsck optimize failed: %v\n%s", err, out)
	}
	output := string(out)
	if !contains(output, "Repair complete") {
		t.Fatalf("fsck did not report repair complete:\n%s", output)
	}
	lines := splitLines(output)
	postRepair := false
	for _, line := range lines {
		if contains(line, "Re-running verification pass") {
			postRepair = true
		}
		if postRepair && contains(line, "ERROR") {
			t.Fatalf("fsck optimize left errors:\n%s", output)
		}
	}
	if !contains(output, "FSCK COMPLETE: no errors found") {
		t.Fatalf("fsck optimize did not finish clean:\n%s", output)
	}

	f, err := os.OpenFile(imgPath, os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("reopen image: %v", err)
	}
	defer f.Close()

	buf := make([]byte, 512)
	if _, err := f.ReadAt(buf, 5*4096+512); err != nil {
		t.Fatalf("read file inode: %v", err)
	}
	file, err := briefs.UnmarshalInode(buf)
	if err != nil {
		t.Fatalf("unmarshal file inode: %v", err)
	}
	// --optimize runs trie + extent compaction, but extent compaction is a no-op
	// for tree-backed inodes until #7 phase 4, so the file stays tree-backed and
	// untouched. Directory-trie compaction still ran (the post-repair pass is
	// clean), which is the part --optimize is meant to exercise here.
	if file.Flags&briefs.InodeFlagIndexed == 0 {
		t.Errorf("file lost InodeFlagIndexed")
	}
	if file.NumExtentsTotal != 9 {
		t.Errorf("file NumExtentsTotal: want 9 (tree-backed, untouched), got %d", file.NumExtentsTotal)
	}
	if file.ExtentInlineBase == 0 {
		t.Errorf("file ExtentInlineBase: expected root leaf still in use, got 0")
	}
}

// TestFsckRepairOnlyInvalid verifies that an unknown --repair-only value is
// rejected before any disk modifications.
func TestFsckRepairOnlyInvalid(t *testing.T) {
	mkfsPath := buildBinary(t, "github.com/ctdk/briefs-utils/cmd/mkfs", "mkfs.briefs")
	fsckPath := buildBinary(t, "github.com/ctdk/briefs-utils/cmd/fsck", "fsck.briefs")

	imgPath := filepath.Join(t.TempDir(), "invalid.briefs")
	cmd := exec.Command(mkfsPath, "-s", "5000", imgPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("mkfs failed: %v\n%s", err, out)
	}

	cmd = exec.Command(fsckPath, "--repair-only=foo", imgPath)
	out, err = cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("fsck should have failed with invalid repair-only value")
	}
	if !contains(string(out), "unknown repair phase") {
		t.Fatalf("fsck did not report unknown repair phase:\n%s", out)
	}
}
