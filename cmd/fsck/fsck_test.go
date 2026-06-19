package main

import (
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	binary.LittleEndian.PutUint64(leafBuf[16:], 0) // next_leaf (offset 16; 0 = no next leaf; bytes 12-15 are header padding)
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

// TestFsckRepairRefusesCorruptBtree verifies the failedBtreeInos safety guard:
// when a tree-backed inode's B+ tree walk fails, default --repair (which rebuilds
// the allocator from fs.usedBlocks) must REFUSE rather than free the inode's
// unreached data/node blocks. A non-allocator phase (--repair-only=links) must
// still proceed, since it loads the on-disk allocator instead of rebuilding it.
//
// We reuse the TestFsckTreeBackedFile fixture (9-extent single-leaf tree for
// inode 2) and corrupt the root leaf's magic so btreeWalk returns ErrBtreeBadMagic
// during collectInodeExtents. Because btreeWalk calls VisitNode (recording the
// root node block into usedBlocks) before the magic check, but only calls
// VisitExtent (recording the data blocks) after passing magic+checksum+ordering,
// the 9 data blocks (abs 100..108) never enter usedBlocks — exactly the
// data-loss vector the guard must prevent.
func TestFsckRepairRefusesCorruptBtree(t *testing.T) {
	mkfsPath := buildBinary(t, "github.com/ctdk/briefs-utils/cmd/mkfs", "mkfs.briefs")
	fsckPath := buildBinary(t, "github.com/ctdk/briefs-utils/cmd/fsck", "fsck.briefs")

	imgPath := filepath.Join(t.TempDir(), "corrupt-btree.briefs")
	cmd := exec.Command(mkfsPath, "-s", "5000", imgPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("mkfs failed: %v\n%s", err, out)
	}

	// Same layout as TestFsckTreeBackedFile (see that test for the rationale).
	const (
		rootLeafAbs   = uint64(110)
		dataStartAbs   = uint64(100)
		rootLeafRelBit = uint64(20)
	)

	leafBuf := make([]byte, 4096)
	binary.LittleEndian.PutUint32(leafBuf[0:], briefs.BtreeMagic)
	binary.LittleEndian.PutUint32(leafBuf[4:], briefs.BtreeFlagLeaf)
	binary.LittleEndian.PutUint16(leafBuf[8:], 0)  // level 0 = leaf
	binary.LittleEndian.PutUint16(leafBuf[10:], 9) // num_keys
	binary.LittleEndian.PutUint64(leafBuf[16:], 0) // next_leaf (offset 16; 0 = no next leaf; bytes 12-15 are header padding)
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

	inode := &briefs.Inode{
		InodeNumber:      2,
		Magic:            briefs.MagicInode,
		Filemode:         briefs.ModeFile | 0644,
		FileSize:         9 * 4096,
		Nlinks:           0, // orphan; we only care about the tree walk
		NumExtentsInline: 0,
		NumExtentsTotal:  9,
		ExtentInlineBase: rootLeafAbs,
		Flags:            briefs.InodeFlagIndexed,
	}
	inode.SetInlineExtents([8]briefs.Extent{})

	f, err := os.OpenFile(imgPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open image: %v", err)
	}
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
	word &^= 1 << 1
	binary.LittleEndian.PutUint64(inodeBM[0:], word)
	if _, err := f.WriteAt(inodeBM, 4096); err != nil {
		t.Fatalf("write inode bitmap: %v", err)
	}

	// Mark data blocks 100..108 and the root leaf allocated in the data L2 (block 89).
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

	// Corrupt the root leaf's magic so the B-tree walk fails with
	// ErrBtreeBadMagic (a structural, non-CRC failure).
	binary.LittleEndian.PutUint32(leafBuf[0:], 0xDEADBEEF)
	if _, err := f.WriteAt(leafBuf, int64(rootLeafAbs*4096)); err != nil {
		t.Fatalf("corrupt root leaf magic: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close image: %v", err)
	}

	// Snapshot the entire image before repair so we can prove the refusal
	// touched nothing.
	before, err := os.ReadFile(imgPath)
	if err != nil {
		t.Fatalf("read image before: %v", err)
	}

	// (1) Default --repair must REFUSE: failedBtreeInos is non-empty and the
	// allocator rebuild phase (RebuildAllocator) is active.
	cmd = exec.Command(fsckPath, "--repair", "-y", imgPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fsck --repair failed: %v\n%s", err, out)
	}
	output := string(out)
	if !contains(output, "Refusing repair") || !contains(output, "B-tree extent-index errors") {
		t.Fatalf("fsck did not refuse repair for corrupt B-tree:\n%s", output)
	}
	if contains(output, "Repair complete") {
		t.Fatalf("fsck proceeded to repair despite corrupt B-tree:\n%s", output)
	}

	// The refusal must not have modified the image at all.
	after, err := os.ReadFile(imgPath)
	if err != nil {
		t.Fatalf("read image after: %v", err)
	}
	if !bytesEqual(before, after) {
		t.Fatalf("fsck --repair refusal modified the image (%d -> %d bytes)", len(before), len(after))
	}

	// (2) A read-only fsck must report the failed B-tree inode in its summary
	// WARNING, confirming failedBtreeInos was populated.
	cmd = exec.Command(fsckPath, imgPath)
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("read-only fsck failed: %v\n%s", err, out)
	}
	output = string(out)
	if !contains(output, "unrecoverable B-tree extent-index errors") || !contains(output, "ino 2") {
		t.Fatalf("read-only fsck did not report failed B-tree inode:\n%s", output)
	}

	// (3) A non-allocator phase must proceed past the guard: --repair-only=links
	// sets RebuildAllocator=false, so the guard does not fire.
	cmd = exec.Command(fsckPath, "--repair-only=links", "-y", imgPath)
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fsck --repair-only=links failed: %v\n%s", err, out)
	}
	output = string(out)
	if !contains(output, "Repair complete") {
		t.Fatalf("fsck --repair-only=links did not proceed past the B-tree guard:\n%s", output)
	}
}

// bytesEqual reports whether two byte slices are equal (avoiding a dependency on
// bytes.Equal to keep the test file's import set unchanged).
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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
	binary.LittleEndian.PutUint64(leafBuf[16:], 0) // next_leaf (offset 16; 0 = no next leaf; bytes 12-15 are header padding)
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

// --- Phase 2: B+ tree structural-detection tests ----------------------------
//
// These build a hand-rolled 2-level B+ tree (3 leaves of 30 extents each under
// one idx root) for inode 2, then inject structurally-bad-but-checksum-valid
// faults and assert verifyBtreeStructures catches them. "Checksum-valid" is the
// key: btreeWalk (the basic walk) only checks magic/checksum/fanout/within-leaf
// order, so to reach the deeper checks in verifyBtreeStructures the corrupted
// node must still pass btreeWalk — hence each fault recomputes the node checksum
// after mutation.
//
// Layout (5000-block image, superblock fields dumped empirically):
//   TrieNodePoolStart=86, TrieNodePoolSize=4 -> data region starts at abs 90
//   data allocator: header@86, L0@87, L1@88, L2@89  (data-rel N -> blk 89 word N/64 bit N%64)
//   inode allocator: header@1, L0@2, L1@3, L2@4      (inode 2 -> blk 4 word 0 bit 1)
//   inode table @ block 5; inode 2 at byte 5*4096+512
//   extents: logical 0..89, phys abs 120..209 (data-rel 30..119)
//   leaves:  data-rel 120/121/122 -> abs 210/211/212 (offsets 0..29 / 30..59 / 60..89)
//   idx root: data-rel 123 -> abs 213 (level 1; children=[210,211] high_keys=[30,60] trailing=212)

const (
	btree2DataRegionAbs = uint64(90)
	btree2DataL2Block   = uint64(89)
	btree2InodeL2Block  = uint64(4)
	btree2InodeByteOff  = int64(5*4096 + 512)
	btree2RootAbs       = uint64(213)
)

// buildBtree2Leaf serializes a leaf with numKeys single-block extents starting
// at logical startOffset and physical physBase, threading nextLeaf. The caller
// supplies a zeroed 4096-byte buffer.
func buildBtree2Leaf(buf []byte, numKeys uint16, startOffset, physBase, nextLeaf uint64) {
	binary.LittleEndian.PutUint32(buf[0:], briefs.BtreeMagic)
	binary.LittleEndian.PutUint32(buf[4:], briefs.BtreeFlagLeaf)
	binary.LittleEndian.PutUint16(buf[8:], 0) // level 0
	binary.LittleEndian.PutUint16(buf[10:], numKeys)
	binary.LittleEndian.PutUint64(buf[16:], nextLeaf)
	for i := uint16(0); i < numKeys; i++ {
		off := briefs.BtreeHeaderSize + int(i)*32
		binary.LittleEndian.PutUint64(buf[off:], startOffset+uint64(i))
		binary.LittleEndian.PutUint64(buf[off+8:], physBase+uint64(i))
		binary.LittleEndian.PutUint64(buf[off+16:], 1) // len 1
		binary.LittleEndian.PutUint64(buf[off+24:], 0) // flags+pad
	}
	binary.LittleEndian.PutUint64(buf[briefs.BtreeChecksumOffset:], briefs.ComputeChainChecksum(buf, 4096))
}

// buildBtree2Idx serializes an internal node: children[i] is the subtree left of
// separator i (high_keys[i]); trailing is the rightmost child.
func buildBtree2Idx(buf []byte, level uint16, children, highKeys []uint64, trailing uint64) {
	binary.LittleEndian.PutUint32(buf[0:], briefs.BtreeMagic)
	binary.LittleEndian.PutUint32(buf[4:], 0) // internal (no leaf flag)
	binary.LittleEndian.PutUint16(buf[8:], level)
	binary.LittleEndian.PutUint16(buf[10:], uint16(len(children)))
	binary.LittleEndian.PutUint64(buf[16:], 0) // next_leaf 0 for internal
	for i := range children {
		off := briefs.BtreeHeaderSize + i*16
		binary.LittleEndian.PutUint64(buf[off:], children[i])
		binary.LittleEndian.PutUint64(buf[off+8:], highKeys[i])
	}
	binary.LittleEndian.PutUint64(buf[briefs.BtreeTrailingChildOffset:], trailing)
	binary.LittleEndian.PutUint64(buf[briefs.BtreeChecksumOffset:], briefs.ComputeChainChecksum(buf, 4096))
}

// markDataAllocated clears bits [relStart, relEnd] (data-relative block numbers)
// in the data L2 bitmap at block dataL2Block, marking them allocated (a cleared
// bit = allocated). Handles multiple 64-bit words.
func markDataAllocated(f *os.File, dataL2Block, relStart, relEnd uint64) error {
	buf := make([]byte, 4096)
	if _, err := f.ReadAt(buf, int64(dataL2Block*4096)); err != nil {
		return err
	}
	for b := relStart; b <= relEnd; b++ {
		wordIdx := b / 64
		bit := b % 64
		off := int(wordIdx) * 8
		w := binary.LittleEndian.Uint64(buf[off:])
		w &^= 1 << bit
		binary.LittleEndian.PutUint64(buf[off:], w)
	}
	_, err := f.WriteAt(buf, int64(dataL2Block*4096))
	return err
}

// prepareBtree2Fixture mkfs's a 5000-block image and writes a correct 3-leaf
// B+ tree for inode 2 with all data/node blocks and inode 2 marked allocated.
// Returns the image path. Each subtest calls this fresh (corruption is destructive).
func prepareBtree2Fixture(t *testing.T) string {
	t.Helper()
	mkfsPath := buildBinary(t, "github.com/ctdk/briefs-utils/cmd/mkfs", "mkfs.briefs")
	imgPath := filepath.Join(t.TempDir(), "btree2.briefs")
	if out, err := exec.Command(mkfsPath, "-s", "5000", imgPath).CombinedOutput(); err != nil {
		t.Fatalf("mkfs: %v\n%s", err, out)
	}

	leaf0 := make([]byte, 4096)
	buildBtree2Leaf(leaf0, 30, 0, 120, 211)  // offsets 0..29, phys 120..149, next=leaf1
	leaf1 := make([]byte, 4096)
	buildBtree2Leaf(leaf1, 30, 30, 150, 212) // offsets 30..59, phys 150..179, next=leaf2
	leaf2 := make([]byte, 4096)
	buildBtree2Leaf(leaf2, 30, 60, 180, 0)   // offsets 60..89, phys 180..209, next=0
	idx := make([]byte, 4096)
	buildBtree2Idx(idx, 1, []uint64{210, 211}, []uint64{30, 60}, 212)

	inode := &briefs.Inode{
		InodeNumber:      2,
		Magic:            briefs.MagicInode,
		Filemode:         briefs.ModeFile | 0644,
		FileSize:         90 * 4096,
		Nlinks:           0, // orphan; we only care about the tree walk
		NumExtentsInline: 0,
		NumExtentsTotal:  90,
		ExtentInlineBase: btree2RootAbs,
		Flags:            briefs.InodeFlagIndexed,
	}
	inode.SetInlineExtents([8]briefs.Extent{})

	f, err := os.OpenFile(imgPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open image: %v", err)
	}
	for _, w := range []struct {
		buf []byte
		abs uint64
	}{{leaf0, 210}, {leaf1, 211}, {leaf2, 212}, {idx, 213}} {
		if _, err := f.WriteAt(w.buf, int64(w.abs*4096)); err != nil {
			t.Fatalf("write node %d: %v", w.abs, err)
		}
	}
	if err := inode.WriteAt(f, btree2InodeByteOff); err != nil {
		t.Fatalf("write inode: %v", err)
	}

	// Mark inode 2 allocated in the inode L2 bitmap (block 4, word 0, bit 1).
	inodeL2 := make([]byte, 4096)
	if _, err := f.ReadAt(inodeL2, int64(btree2InodeL2Block*4096)); err != nil {
		t.Fatalf("read inode L2: %v", err)
	}
	w := binary.LittleEndian.Uint64(inodeL2[0:])
	w &^= 1 << 1
	binary.LittleEndian.PutUint64(inodeL2[0:], w)
	if _, err := f.WriteAt(inodeL2, int64(btree2InodeL2Block*4096)); err != nil {
		t.Fatalf("write inode L2: %v", err)
	}

	// Mark data-rel 30..123 allocated (90 data blocks + 3 leaves + 1 idx root).
	if err := markDataAllocated(f, btree2DataL2Block, 30, 123); err != nil {
		t.Fatalf("mark data: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close image: %v", err)
	}
	return imgPath
}

// rewriteBtree2Node reads block @abs, applies mutate, recomputes the B-tree
// checksum, and writes it back — producing a structurally-bad-but-checksum-valid
// node so btreeWalk passes and verifyBtreeStructures catches the structural fault.
func rewriteBtree2Node(t *testing.T, imgPath string, abs uint64, mutate func(buf []byte)) {
	t.Helper()
	f, err := os.OpenFile(imgPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	buf := make([]byte, 4096)
	if _, err := f.ReadAt(buf, int64(abs*4096)); err != nil {
		t.Fatalf("read node %d: %v", abs, err)
	}
	mutate(buf)
	binary.LittleEndian.PutUint64(buf[briefs.BtreeChecksumOffset:], briefs.ComputeChainChecksum(buf, 4096))
	if _, err := f.WriteAt(buf, int64(abs*4096)); err != nil {
		t.Fatalf("write node %d: %v", abs, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// runBtree2Fsck runs read-only fsck and returns its combined output.
func runBtree2Fsck(t *testing.T, imgPath string) string {
	t.Helper()
	fsckPath := buildBinary(t, "github.com/ctdk/briefs-utils/cmd/fsck", "fsck.briefs")
	out, err := exec.Command(fsckPath, imgPath).CombinedOutput()
	if err != nil {
		// read-only fsck exits non-zero when it finds errors; that's expected
		// for the fault cases. Only fail on execution errors, not exit codes.
		_ = err
	}
	return string(out)
}

func TestFsckBtreeStructuralChecks(t *testing.T) {
	// Each fault is (name, inject, wantSubstrings). wantSubstrings must all be
	// present in the read-only fsck output, and "unrecoverable B-tree
	// extent-index errors" + "ino 2" must confirm failedBtreeInos was set.
	//
	// Byte offsets within the idx root (block 213):
	//   idx[0].child    @ BtreeHeaderSize+0*16   = 24
	//   idx[0].high_key @ BtreeHeaderSize+0*16+8 = 32
	//   idx[1].child    @ BtreeHeaderSize+1*16   = 40
	//   idx[1].high_key @ BtreeHeaderSize+1*16+8 = 48
	// Leaf1 first extent offset @ BtreeHeaderSize+0*32 = 24.
	// Leaf level field @ 8 (uint16).
	type fault struct {
		name    string
		inject  func(t *testing.T, imgPath string)
		want    []string
	}
	faults := []fault{
		{
			name: "unsorted_high_key",
			inject: func(t *testing.T, p string) {
				rewriteBtree2Node(t, p, btree2RootAbs, func(buf []byte) {
					binary.LittleEndian.PutUint64(buf[48:], 25) // idx[1].high_key 60 -> 25 (< 30)
				})
			},
			want: []string{"separator high_key not strictly ascending"},
		},
		{
			name: "zero_high_key",
			inject: func(t *testing.T, p string) {
				rewriteBtree2Node(t, p, btree2RootAbs, func(buf []byte) {
					binary.LittleEndian.PutUint64(buf[32:], 0) // idx[0].high_key 30 -> 0
				})
			},
			want: []string{"separator high_key not strictly ascending", "high_key=0"},
		},
		{
			name: "null_child",
			inject: func(t *testing.T, p string) {
				rewriteBtree2Node(t, p, btree2RootAbs, func(buf []byte) {
					binary.LittleEndian.PutUint64(buf[24:], 0) // idx[0].child 210 -> 0
				})
			},
			want: []string{"bad child pointer", "null child pointer"},
		},
		{
			name: "wrong_leaf_level",
			inject: func(t *testing.T, p string) {
				rewriteBtree2Node(t, p, 211, func(buf []byte) {
					binary.LittleEndian.PutUint16(buf[8:], 2) // leaf1 level 0 -> 2
				})
			},
			want: []string{"bad child pointer", "leaf with level"},
		},
		{
			name: "cross_leaf_overlap",
			inject: func(t *testing.T, p string) {
				rewriteBtree2Node(t, p, 211, func(buf []byte) {
					// leaf1 first offset 30 -> 29 (== leaf0 max 29): within-leaf
					// order still holds (29 < 31), so btreeWalk passes, but the
					// cross-leaf check (29 <= prevLeafMax 29) fires.
					binary.LittleEndian.PutUint64(buf[24:], 29)
				})
			},
			want: []string{"cross-leaf extents unsorted"},
		},
		{
			name: "count_mismatch",
			inject: func(t *testing.T, p string) {
				// Rewrite the inode with a wrong num_extents_total (tree untouched).
				f, err := os.OpenFile(p, os.O_RDWR, 0)
				if err != nil {
					t.Fatalf("open: %v", err)
				}
				buf := make([]byte, 512)
				if _, err := f.ReadAt(buf, btree2InodeByteOff); err != nil {
					t.Fatalf("read inode: %v", err)
				}
				in, err := briefs.UnmarshalInode(buf)
				if err != nil {
					t.Fatalf("unmarshal inode: %v", err)
				}
				in.NumExtentsTotal = 89 // actual walked count is 90
				if err := in.WriteAt(f, btree2InodeByteOff); err != nil {
					t.Fatalf("write inode: %v", err)
				}
				if err := f.Close(); err != nil {
					t.Fatalf("close: %v", err)
				}
			},
			want: []string{"extent count != num_extents_total"},
		},
	}

	for _, ft := range faults {
		t.Run(ft.name, func(t *testing.T) {
			imgPath := prepareBtree2Fixture(t)
			ft.inject(t, imgPath)
			out := runBtree2Fsck(t, imgPath)
			for _, s := range ft.want {
				if !contains(out, s) {
					t.Errorf("expected output to contain %q\n---- output ----\n%s", s, out)
				}
			}
			if !contains(out, "unrecoverable B-tree extent-index errors") || !contains(out, "ino 2") {
				t.Errorf("expected failedBtreeInos WARNING for ino 2\n---- output ----\n%s", out)
			}
		})
	}

	// The correct tree must produce NO structural errors and must NOT populate
	// failedBtreeInos (no "unrecoverable B-tree extent-index errors" warning).
	t.Run("correct_tree_clean", func(t *testing.T) {
		imgPath := prepareBtree2Fixture(t)
		out := runBtree2Fsck(t, imgPath)
		for _, bad := range []string{
			"separator high_key",
			"bad child pointer",
			"cross-leaf extents unsorted",
			"extent count != num_extents_total",
			"leaf with level",
			"unrecoverable B-tree extent-index errors",
			"btree node",
		} {
			if contains(out, bad) {
				t.Errorf("correct tree produced B-tree marker %q\n---- output ----\n%s", bad, out)
			}
		}
	})
}

// --- Phase 3: B+ tree CRC-only repair tests ---------------------------------
//
// repairBtreeChecksums rewrites a leaf node's torn checksum when the leaf is
// structurally self-consistent (magic/fanout/within-leaf order all valid). It
// defers internal-node checksum failures and structurally-invalid leaves to
// Phase 4. These tests use the 3-leaf fixture (prepareBtree2Fixture): leaf0 at
// abs 210, root idx at abs 213.

func TestFsckRepairBtreeChecksums(t *testing.T) {
	fsckPath := buildBinary(t, "github.com/ctdk/briefs-utils/cmd/fsck", "fsck.briefs")

	// Positive: a torn leaf checksum (structure intact) is rewritten and the
	// tree re-verifies clean.
	t.Run("torn_leaf_checksum_fixed", func(t *testing.T) {
		imgPath := prepareBtree2Fixture(t)

		// Zero leaf0's checksum field -> torn CRC, structure intact. btreeWalk
		// fails on the bad checksum -> failedBtreeInos{2}. --repair-only=btrees
		// (RebuildAllocator=false) bypasses the Phase 1 guard and runs the CRC
		// repair.
		f, err := os.OpenFile(imgPath, os.O_RDWR, 0)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		buf := make([]byte, 4096)
		if _, err := f.ReadAt(buf, int64(210*4096)); err != nil {
			t.Fatalf("read leaf0: %v", err)
		}
		binary.LittleEndian.PutUint64(buf[briefs.BtreeChecksumOffset:], 0)
		if _, err := f.WriteAt(buf, int64(210*4096)); err != nil {
			t.Fatalf("write leaf0: %v", err)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}

		out, err := exec.Command(fsckPath, "--repair-only=btrees", "-y", imgPath).CombinedOutput()
		if err != nil {
			t.Fatalf("fsck repair: %v\n%s", err, out)
		}
		output := string(out)
		if !contains(output, "Repair complete") {
			t.Fatalf("repair did not complete:\n%s", output)
		}
		if !contains(output, "rewrote torn checksum") {
			t.Errorf("expected a 'rewrote torn checksum' notice:\n%s", output)
		}

		// The re-verify pass (after "Re-running verification pass") must show no
		// checksum mismatch and no failed B-tree inode. The first pass legitimately
		// reports the torn checksum, so only inspect the post-repair section.
		lines := splitLines(output)
		postRepair := false
		for _, line := range lines {
			if contains(line, "Re-running verification pass") {
				postRepair = true
			}
			if postRepair && contains(line, "checksum mismatch") {
				t.Errorf("checksum mismatch still reported after repair:\n%s", output)
			}
			if postRepair && contains(line, "unrecoverable B-tree extent-index errors") {
				t.Errorf("failedBtreeInos still set after repair:\n%s", output)
			}
		}

		// The leaf on disk must now carry a valid checksum, and its extents intact.
		f, err = os.Open(imgPath)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		defer f.Close()
		leaf := make([]byte, 4096)
		if _, err := f.ReadAt(leaf, int64(210*4096)); err != nil {
			t.Fatalf("read leaf0: %v", err)
		}
		if err := briefs.VerifyBtreeNodeChecksum(leaf, 4096); err != nil {
			t.Errorf("leaf checksum not restored: %v", err)
		}
		if got := binary.LittleEndian.Uint16(leaf[10:]); got != 30 {
			t.Errorf("leaf0 num_keys changed: want 30, got %d", got)
		}
	})

	// Negative: a structurally-invalid leaf (unsorted extents) is NOT rewritten,
	// even though its checksum is also torn. repairBtreeChecksums checks
	// within-leaf order before touching the checksum, so it defers to Phase 4.
	t.Run("structural_fault_not_fixed", func(t *testing.T) {
		imgPath := prepareBtree2Fixture(t)

		// Corrupt leaf0's first extent offset 0 -> 100 (makes [100,1,2,...,29]
		// unsorted) WITHOUT recomputing the checksum, so both structure and CRC
		// are bad.
		f, err := os.OpenFile(imgPath, os.O_RDWR, 0)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		buf := make([]byte, 4096)
		if _, err := f.ReadAt(buf, int64(210*4096)); err != nil {
			t.Fatalf("read leaf0: %v", err)
		}
		binary.LittleEndian.PutUint64(buf[briefs.BtreeHeaderSize:], 100) // offset[0] 0 -> 100
		if _, err := f.WriteAt(buf, int64(210*4096)); err != nil {
			t.Fatalf("write leaf0: %v", err)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}

		// Snapshot the leaf checksum field before repair.
		before, err := os.ReadFile(imgPath)
		if err != nil {
			t.Fatalf("read image: %v", err)
		}
		beforeCS := binary.LittleEndian.Uint64(before[210*4096+briefs.BtreeChecksumOffset:])

		out, err := exec.Command(fsckPath, "--repair-only=btrees", "-y", imgPath).CombinedOutput()
		if err != nil {
			t.Fatalf("fsck repair: %v\n%s", err, out)
		}
		output := string(out)
		if !contains(output, "Repair complete") {
			t.Fatalf("repair did not complete:\n%s", output)
		}
		if contains(output, "rewrote torn checksum") {
			t.Errorf("repair rewrote a structurally-invalid leaf (must defer to Phase 4):\n%s", output)
		}
		// The fault must still be flagged after repair (re-verify still fails).
		if !contains(output, "checksum mismatch") && !contains(output, "extents unsorted") {
			t.Errorf("expected persistent B-tree error after repair:\n%s", output)
		}

		// The leaf checksum must be unchanged: repair must not have rewritten it.
		after, err := os.ReadFile(imgPath)
		if err != nil {
			t.Fatalf("read image: %v", err)
		}
		afterCS := binary.LittleEndian.Uint64(after[210*4096+briefs.BtreeChecksumOffset:])
		if beforeCS != afterCS {
			t.Errorf("leaf checksum changed (must defer to Phase 4): was 0x%X now 0x%X", beforeCS, afterCS)
		}
	})
}

// --- Phase 4: B+ tree rebuild-from-extents tests ------------------------------
//
// rebuildBtreeIndex tolerantly re-collects surviving extents from a damaged
// tree and either drops to an inline array (<=8 extents) or builds a fresh
// B+ tree. These tests build a reader-valid tree-backed inode 2 with the
// briefs builders, inject a fault that lands the inode in failedBtreeInos, run
// --repair-only=btree-rebuild, and assert the rebuilt inode/tree.

// prepareIndexedBtreeFixtureExtents mkfs's a 5000-block image and writes a
// correct tree-backed inode 2 holding the given (already sorted) extents,
// marking all non-hole data blocks, node blocks, and inode 2 allocated. Hole
// extents (ExtentFlagHole, Phys=0) consume no data block. Non-hole extents must
// carry contiguous Phys starting at abs 120 (data-rel 30) so the data+node
// blocks form one contiguous allocated range. Returns the image path and the
// tree root absolute block. Uses the briefs builders so the tree is
// reader-valid by construction.
func prepareIndexedBtreeFixtureExtents(t *testing.T, extents []briefs.Extent) (imgPath string, rootAbs uint64, leafBlocks []uint64) {
	t.Helper()
	mkfsPath := buildBinary(t, "github.com/ctdk/briefs-utils/cmd/mkfs", "mkfs.briefs")
	imgPath = filepath.Join(t.TempDir(), "idxbtree.briefs")
	if out, err := exec.Command(mkfsPath, "-s", "5000", imgPath).CombinedOutput(); err != nil {
		t.Fatalf("mkfs: %v\n%s", err, out)
	}
	f, err := os.OpenFile(imgPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	// Non-hole extents' Phys are abs 120.. (data-rel 30..); node blocks follow.
	nNonHole := 0
	for _, e := range extents {
		if e.Flags&briefs.ExtentFlagHole == 0 && e.Len > 0 && e.Phys > 0 {
			nNonHole++
		}
	}
	nextRel := uint64(30 + nNonHole)
	allocBlock := func() (uint64, error) { abs := 90 + nextRel; nextRel++; return abs, nil }

	leafBlocks, leafFirst, leafBufs, err := briefs.BuildBtreeLeaves(extents, 4096, allocBlock)
	if err != nil {
		t.Fatalf("BuildBtreeLeaves: %v", err)
	}
	rootBlock, _, idxBlocks, idxBufs, err := briefs.BuildBtreeIndex(leafBlocks, leafFirst, 4096, 1, allocBlock)
	if err != nil {
		t.Fatalf("BuildBtreeIndex: %v", err)
	}
	rootAbs = rootBlock
	allBlocks := append(append([]uint64{}, leafBlocks...), idxBlocks...)
	allBufs := append(append([][]byte{}, leafBufs...), idxBufs...)
	for i, b := range allBlocks {
		if _, err := f.WriteAt(allBufs[i], int64(b*4096)); err != nil {
			t.Fatalf("write node %d: %v", b, err)
		}
	}

	inode := &briefs.Inode{
		InodeNumber:      2,
		Magic:            briefs.MagicInode,
		Filemode:         briefs.ModeFile | 0644,
		FileSize:         uint64(len(extents)) * 4096,
		Nlinks:           0, // orphan; only the tree walk matters here
		NumExtentsInline: 0,
		NumExtentsTotal:  uint64(len(extents)),
		ExtentInlineBase: rootBlock,
		Flags:            briefs.InodeFlagIndexed,
	}
	inode.SetInlineExtents([8]briefs.Extent{})
	if err := inode.WriteAt(f, btree2InodeByteOff); err != nil {
		t.Fatalf("write inode: %v", err)
	}

	// Mark inode 2 allocated in the inode L2 bitmap (block 4, word 0, bit 1).
	inodeL2 := make([]byte, 4096)
	if _, err := f.ReadAt(inodeL2, int64(btree2InodeL2Block*4096)); err != nil {
		t.Fatalf("read inode L2: %v", err)
	}
	w := binary.LittleEndian.Uint64(inodeL2[0:])
	w &^= 1 << 1
	binary.LittleEndian.PutUint64(inodeL2[0:], w)
	if _, err := f.WriteAt(inodeL2, int64(btree2InodeL2Block*4096)); err != nil {
		t.Fatalf("write inode L2: %v", err)
	}

	// Data blocks (data-rel 30..30+nNonHole-1) + node blocks (..nextRel-1) are
	// contiguous; mark the whole range allocated.
	if err := markDataAllocated(f, btree2DataL2Block, 30, nextRel-1); err != nil {
		t.Fatalf("mark data: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return imgPath, rootAbs, leafBlocks
}

// prepareIndexedBtreeFixture builds a tree-backed inode 2 with nExtents
// single-block, non-hole extents at offsets 0..nExtents-1 (phys abs 120..). For
// nExtents > BtreeLeafFanout (126) the tree has multiple leaves plus an idx root.
func prepareIndexedBtreeFixture(t *testing.T, nExtents int) (string, uint64, []uint64) {
	ext := make([]briefs.Extent, nExtents)
	for i := 0; i < nExtents; i++ {
		ext[i] = briefs.Extent{Offset: uint64(i), Phys: 120 + uint64(i), Len: 1}
	}
	return prepareIndexedBtreeFixtureExtents(t, ext)
}

// corruptInode2NumExtentsTotal rewrites inode 2's NumExtentsTotal to @wrong,
// producing a count-mismatch fault (tree valid, count wrong) that lands the
// inode in failedBtreeInos without making any node unreadable.
func corruptInode2NumExtentsTotal(t *testing.T, imgPath string, wrong uint64) {
	t.Helper()
	f, err := os.OpenFile(imgPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	buf := make([]byte, 512)
	if _, err := f.ReadAt(buf, btree2InodeByteOff); err != nil {
		t.Fatalf("read inode: %v", err)
	}
	in, err := briefs.UnmarshalInode(buf)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	in.NumExtentsTotal = wrong
	if err := in.WriteAt(f, btree2InodeByteOff); err != nil {
		t.Fatalf("write inode: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// corruptBtreeNodeMagic overwrites the magic field of block @abs with a bad
// value (and leaves the rest of the node, so the node is unreadable as a
// B-tree node — its extents are "lost" to the tolerant collector).
func corruptBtreeNodeMagic(t *testing.T, imgPath string, abs uint64) {
	t.Helper()
	f, err := os.OpenFile(imgPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	buf := make([]byte, 4096)
	if _, err := f.ReadAt(buf, int64(abs*4096)); err != nil {
		t.Fatalf("read node %d: %v", abs, err)
	}
	binary.LittleEndian.PutUint32(buf[0:], 0xDEADBEEF)
	if _, err := f.WriteAt(buf, int64(abs*4096)); err != nil {
		t.Fatalf("write node %d: %v", abs, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// runBtreeRebuild runs --repair-only=btree-rebuild -y and returns the output.
func runBtreeRebuild(t *testing.T, imgPath string) string {
	t.Helper()
	fsckPath := buildBinary(t, "github.com/ctdk/briefs-utils/cmd/fsck", "fsck.briefs")
	out, err := exec.Command(fsckPath, "--repair-only=btree-rebuild", "-y", imgPath).CombinedOutput()
	if err != nil {
		t.Fatalf("fsck repair: %v\n%s", err, out)
	}
	return string(out)
}

// postRepairSection returns the portion of fsck output after the
// "Re-running verification pass" marker (the post-repair re-verify).
func postRepairSection(output string) string {
	var sb strings.Builder
	post := false
	for _, l := range splitLines(output) {
		if contains(l, "Re-running verification pass") {
			post = true
		}
		if post {
			sb.WriteString(l)
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}

// readInode2 reads inode 2 from the image.
func readInode2(t *testing.T, imgPath string) *briefs.Inode {
	t.Helper()
	f, err := os.Open(imgPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	buf := make([]byte, 512)
	if _, err := f.ReadAt(buf, btree2InodeByteOff); err != nil {
		t.Fatalf("read inode: %v", err)
	}
	in, err := briefs.UnmarshalInode(buf)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return in
}

// readInode2Extents reads all of inode 2's extents via the production walker.
func readInode2Extents(t *testing.T, imgPath string, in *briefs.Inode) []briefs.Extent {
	t.Helper()
	f, err := os.Open(imgPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	var exts []briefs.Extent
	if err := briefs.IterateInodeExtents(f, in, 4096, briefs.InodeExtentVisitor{
		VisitExtent: func(e briefs.Extent) error { exts = append(exts, e); return nil },
	}); err != nil {
		t.Fatalf("iterate: %v", err)
	}
	return exts
}

// assertNoBtreeMarkers fails if the post-repair section contains any B-tree
// structural/CRC/count marker (the rebuilt tree must verify clean).
func assertNoBtreeMarkers(t *testing.T, postRepair string) {
	t.Helper()
	for _, bad := range []string{
		"btree node",
		"unrecoverable B-tree extent-index errors",
		"checksum mismatch",
		"extents unsorted",
		"extent count != num_extents_total",
		"separator high_key",
		"bad child pointer",
		"cross-leaf extents unsorted",
		"fanout overflow",
	} {
		if contains(postRepair, bad) {
			t.Errorf("post-repair section contains B-tree marker %q:\n%s", bad, postRepair)
		}
	}
}

func TestFsckRebuildBtreeIndex(t *testing.T) {
	// rebuild_gt8: a 300-extent (3-leaf + idx-root) tree with a count mismatch
	// is rebuilt into a fresh tree, the count is corrected, and the re-verify is
	// clean (old node blocks freed, no leaked blocks).
	t.Run("rebuild_gt8", func(t *testing.T) {
		imgPath, _, _ := prepareIndexedBtreeFixture(t, 300)
		corruptInode2NumExtentsTotal(t, imgPath, 299) // actual 300

		out := runBtreeRebuild(t, imgPath)
		if !contains(out, "Repair complete") {
			t.Fatalf("repair did not complete:\n%s", out)
		}
		if !contains(out, "rebuilt extent index") {
			t.Errorf("expected a rebuild notice:\n%s", out)
		}
		post := postRepairSection(out)
		assertNoBtreeMarkers(t, post)

		in := readInode2(t, imgPath)
		if in.Flags&briefs.InodeFlagIndexed == 0 {
			t.Errorf("expected tree-backed inode, got flags=0x%X", in.Flags)
		}
		if in.NumExtentsTotal != 300 {
			t.Errorf("NumExtentsTotal: want 300, got %d", in.NumExtentsTotal)
		}
		if in.ExtentInlineBase == 0 {
			t.Errorf("ExtentInlineBase still 0")
		}
		exts := readInode2Extents(t, imgPath, in)
		if len(exts) != 300 {
			t.Fatalf("rebuilt tree extents: want 300, got %d", len(exts))
		}
		for i, e := range exts {
			if e.Offset != uint64(i) || e.Phys != 120+uint64(i) {
				t.Errorf("extent %d: want offset=%d phys=%d, got offset=%d phys=%d", i, i, 120+i, e.Offset, e.Phys)
			}
		}
	})

	// rebuild_lost_extents: the middle leaf (offsets 126..251) of a 300-extent
	// 3-leaf tree gets a bad magic, so its 126 extents are lost. The rebuild
	// recovers the surviving 174 (leaf0 offsets 0..125 + leaf2 offsets 252..299),
	// builds a fresh 2-leaf + idx tree, and warnfs the loss. The corrupt middle
	// node is left allocated (safe policy) -> re-verify reports it as a leaked
	// block (an ERROR, not a B-tree error); Phase 5 reclaims it.
	t.Run("rebuild_lost_extents", func(t *testing.T) {
		imgPath, _, leafBlocks := prepareIndexedBtreeFixture(t, 300)
		if len(leafBlocks) != 3 {
			t.Fatalf("expected 3 leaves, got %d", len(leafBlocks))
		}
		corruptBtreeNodeMagic(t, imgPath, leafBlocks[1]) // middle leaf -> bad magic

		out := runBtreeRebuild(t, imgPath)
		if !contains(out, "Repair complete") {
			t.Fatalf("repair did not complete:\n%s", out)
		}
		if !contains(out, "rebuilt extent index") {
			t.Errorf("expected a rebuild notice:\n%s", out)
		}
		if !contains(out, "lost") {
			t.Errorf("expected a lost-extent warning:\n%s", out)
		}
		post := postRepairSection(out)
		assertNoBtreeMarkers(t, post)

		in := readInode2(t, imgPath)
		if in.Flags&briefs.InodeFlagIndexed == 0 {
			t.Errorf("expected tree-backed inode, got flags=0x%X", in.Flags)
		}
		if in.NumExtentsTotal != 174 {
			t.Errorf("NumExtentsTotal: want 174, got %d", in.NumExtentsTotal)
		}
		exts := readInode2Extents(t, imgPath, in)
		if len(exts) != 174 {
			t.Fatalf("rebuilt tree extents: want 174, got %d", len(exts))
		}
		// Offsets must be 0..125 then 252..299 (middle leaf's 126..251 lost).
		wantOff := append(append([]uint64{}, seqOffsets(0, 126)...), seqOffsets(252, 48)...)
		for i, e := range exts {
			if e.Offset != wantOff[i] {
				t.Errorf("extent %d offset: want %d, got %d", i, wantOff[i], e.Offset)
			}
		}
		// The corrupt middle-leaf node is left allocated and unreferenced -> leaked.
		if !contains(post, "marked allocated in bitmap but NOT referenced") {
			t.Errorf("expected a leaked-block notice for the corrupt middle leaf:\n%s", post)
		}
	})

	// rebuild_drop_to_inline: a 5-extent tree-backed inode (count mismatch) is
	// rebuilt to inline-only: InodeFlagIndexed cleared, ExtentInlineBase=0,
	// NumExtentsInline=5, 5 inline extents. Old leaf block freed.
	t.Run("rebuild_drop_to_inline", func(t *testing.T) {
		imgPath, _, _ := prepareIndexedBtreeFixture(t, 5)
		corruptInode2NumExtentsTotal(t, imgPath, 99) // actual 5

		out := runBtreeRebuild(t, imgPath)
		if !contains(out, "Repair complete") {
			t.Fatalf("repair did not complete:\n%s", out)
		}
		if !contains(out, "rebuilt extent index") {
			t.Errorf("expected a rebuild notice:\n%s", out)
		}
		if !contains(out, "inline") {
			t.Errorf("expected an inline-drop notice:\n%s", out)
		}
		post := postRepairSection(out)
		assertNoBtreeMarkers(t, post)

		in := readInode2(t, imgPath)
		if in.Flags&briefs.InodeFlagIndexed != 0 {
			t.Errorf("expected InodeFlagIndexed cleared, got flags=0x%X", in.Flags)
		}
		if in.ExtentInlineBase != 0 {
			t.Errorf("ExtentInlineBase: want 0, got %d", in.ExtentInlineBase)
		}
		if in.NumExtentsInline != 5 {
			t.Errorf("NumExtentsInline: want 5, got %d", in.NumExtentsInline)
		}
		if in.NumExtentsTotal != 5 {
			t.Errorf("NumExtentsTotal: want 5, got %d", in.NumExtentsTotal)
		}
		exts := readInode2Extents(t, imgPath, in)
		if len(exts) != 5 {
			t.Fatalf("inline extents: want 5, got %d", len(exts))
		}
		for i, e := range exts {
			if e.Offset != uint64(i) || e.Phys != 120+uint64(i) {
				t.Errorf("inline extent %d: want offset=%d phys=%d, got offset=%d phys=%d", i, i, 120+i, e.Offset, e.Phys)
			}
		}
	})

	// rebuild_hole_preservation: a 10-extent tree with holes at offsets 3 and 7
	// (ExtentFlagHole, Phys=0) is rebuilt (count mismatch) and the holes survive
	// verbatim in the fresh tree.
	t.Run("rebuild_hole_preservation", func(t *testing.T) {
		extents := make([]briefs.Extent, 10)
		phys := uint64(120)
		for i := 0; i < 10; i++ {
			if i == 3 || i == 7 {
				extents[i] = briefs.Extent{Offset: uint64(i), Phys: 0, Len: 1, Flags: briefs.ExtentFlagHole}
			} else {
				extents[i] = briefs.Extent{Offset: uint64(i), Phys: phys, Len: 1}
				phys++
			}
		}
		imgPath, _, _ := prepareIndexedBtreeFixtureExtents(t, extents)
		corruptInode2NumExtentsTotal(t, imgPath, 42) // actual 10

		out := runBtreeRebuild(t, imgPath)
		if !contains(out, "Repair complete") {
			t.Fatalf("repair did not complete:\n%s", out)
		}
		post := postRepairSection(out)
		assertNoBtreeMarkers(t, post)

		in := readInode2(t, imgPath)
		if in.Flags&briefs.InodeFlagIndexed == 0 {
			t.Errorf("expected tree-backed inode (10 > 8), got flags=0x%X", in.Flags)
		}
		if in.NumExtentsTotal != 10 {
			t.Errorf("NumExtentsTotal: want 10, got %d", in.NumExtentsTotal)
		}
		exts := readInode2Extents(t, imgPath, in)
		if len(exts) != 10 {
			t.Fatalf("rebuilt tree extents: want 10, got %d", len(exts))
		}
		for i, e := range exts {
			if e.Offset != uint64(i) {
				t.Errorf("extent %d offset: want %d, got %d", i, i, e.Offset)
				continue
			}
			isHole := i == 3 || i == 7
			if isHole {
				if e.Flags&briefs.ExtentFlagHole == 0 || e.Phys != 0 {
					t.Errorf("extent %d: expected hole (Phys=0, Hole flag), got phys=%d flags=0x%X", i, e.Phys, e.Flags)
				}
			} else {
				if e.Flags&briefs.ExtentFlagHole != 0 || e.Phys == 0 {
					t.Errorf("extent %d: expected non-hole, got phys=%d flags=0x%X", i, e.Phys, e.Flags)
				}
			}
		}
	})
}

// seqOffsets returns start, start+1, ..., start+n-1.
func seqOffsets(start uint64, n int) []uint64 {
	out := make([]uint64, n)
	for i := 0; i < n; i++ {
		out[i] = start + uint64(i)
	}
	return out
}
