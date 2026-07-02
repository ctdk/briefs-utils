package main

import (
	"os"
	"testing"

	"github.com/ctdk/briefs-utils/briefs"
)

// TestCollectInodeExtentsUnwrittenAndHole verifies that collectInodeExtents:
//   - treats Phys == 0 as a hole and records no used block;
//   - treats ExtentFlagUnwritten as ordinary allocated backing and records it;
//   - tolerates no other extent flags (warns but still records the block).
func TestCollectInodeExtentsUnwrittenAndHole(t *testing.T) {
	tmp, err := os.CreateTemp(t.TempDir(), "extent-flags-*.briefs")
	if err != nil {
		t.Fatalf("create temp: %v", err)
	}
	tmp.Close()

	fs := &fsckState{
		file:        mustOpen(tmp.Name()),
		sb:          &briefs.SuperblockLayout{BlockSize: 4096},
		usedBlocks:  make(map[uint64]bool),
		inodes:      make(map[uint64]*briefs.Inode),
		entryCounts: make(map[uint64]int),
	}
	defer fs.file.Close()

	tests := []struct {
		name      string
		ext       briefs.Extent
		wantUsed  uint64
		wantSet   bool
	}{
		{"hole", briefs.Extent{Offset: 0, Phys: 0, Len: 1}, 0, false},
		{"unwritten", briefs.Extent{Offset: 1, Phys: 42, Len: 1, Flags: briefs.ExtentFlagUnwritten}, 42, true},
		{"unknown-flag", briefs.Extent{Offset: 2, Phys: 43, Len: 1, Flags: 0x2}, 43, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ino := uint64(2)
			in := &briefs.Inode{
				InodeNumber:     ino,
				Magic:           briefs.MagicInode,
				Filemode:        briefs.ModeFile | 0644,
				NumExtentsInline: 1,
				NumExtentsTotal:  1,
			}
			in.SetInlineExtent(0, tc.ext.Offset, tc.ext.Phys, tc.ext.Len, uint64(tc.ext.Flags))
			fs.inodes[ino] = in
			fs.usedBlocks = make(map[uint64]bool)
			collectInodeExtents(fs, ino, in, 4096)
			if got := fs.usedBlocks[tc.wantUsed]; got != tc.wantSet {
				t.Errorf("usedBlocks[%d] = %v, want %v", tc.wantUsed, got, tc.wantSet)
			}
		})
	}
}

func mustOpen(path string) *os.File {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		panic(err)
	}
	return f
}
