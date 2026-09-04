package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ctdk/briefs-utils/briefs"
)

// TestFsckVerboseLiftsReportCaps pins the --verbose contract: the per-check
// report caps (here the L1-summary cap of 10) apply by default — the first
// 10 occurrences report, then a single "(more ... suppressed)" notice — and
// --verbose lifts the cap so every occurrence reports. Corrupting an L1
// summary word (toggle one bit) always mismatches its recomputed value but
// leaves the free counts and L2 bitmap intact.
func TestFsckVerboseLiftsReportCaps(t *testing.T) {
	fsckPath := buildBinary(t, "github.com/ctdk/briefs-utils/cmd/fsck", "fsck.briefs")
	mkfsPath := buildBinary(t, "github.com/ctdk/briefs-utils/cmd/mkfs", "mkfs.briefs")

	// The data allocator's L1 level needs more than 10 words for the cap to
	// bite: L1Words = ceil(L2Words/64), L2Words = ceil(blocks/64), so a
	// 60000-block image gives L1Words ~ 15.
	imgPath := filepath.Join(t.TempDir(), "verbose.briefs")
	if out, err := exec.Command(mkfsPath, "-s", "60000", imgPath).CombinedOutput(); err != nil {
		t.Fatalf("mkfs: %v\n%s", err, out)
	}

	f, err := os.OpenFile(imgPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open image: %v", err)
	}
	sb, err := briefs.ReadSuperblock(f, 4096)
	if err != nil {
		t.Fatalf("read superblock: %v", err)
	}
	hdr, err := briefs.ReadAllocatorHeader(f, sb.TrieNodePoolStart, sb.BlockSize)
	if err != nil {
		t.Fatalf("read data allocator header: %v", err)
	}
	if hdr.L1Words < 12 {
		t.Fatalf("fixture too small: data allocator L1 has %d words, want >= 12", hdr.L1Words)
	}
	// The pool layout is header block, then whole blocks of L0, then L1.
	l0Blocks := (hdr.L0Words*8 + sb.BlockSize - 1) / sb.BlockSize
	l1ByteStart := (sb.TrieNodePoolStart + 1 + l0Blocks) * sb.BlockSize

	const nCorrupt = 12
	buf := make([]byte, nCorrupt*8)
	if _, err := f.ReadAt(buf, int64(l1ByteStart)); err != nil {
		t.Fatalf("read L1 region: %v", err)
	}
	for i := 0; i < nCorrupt; i++ {
		buf[i*8] ^= 0x01 // toggle one bit of L1 word i
	}
	if _, err := f.WriteAt(buf, int64(l1ByteStart)); err != nil {
		t.Fatalf("write L1 region: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	run := func(args ...string) string {
		t.Helper()
		out, _ := exec.Command(fsckPath, append(args, imgPath)...).CombinedOutput()
		return string(out)
	}

	// Default: first 10 report, then one suppression notice.
	deflt := run()
	if got := strings.Count(deflt, "L1 word"); got != 10 {
		t.Errorf("default run: want 10 L1 mismatch reports, got %d:\n%s", got, deflt)
	}
	if !strings.Contains(deflt, "(more L1 errors suppressed)") {
		t.Errorf("default run: missing suppression notice:\n%s", deflt)
	}

	// --verbose: every occurrence reports, no suppression notice.
	verbose := run("--verbose")
	if got := strings.Count(verbose, "L1 word"); got != nCorrupt {
		t.Errorf("verbose run: want %d L1 mismatch reports, got %d:\n%s", nCorrupt, got, verbose)
	}
	if strings.Contains(verbose, "suppressed)") {
		t.Errorf("verbose run: cap must be lifted, found a suppression notice:\n%s", verbose)
	}
}
