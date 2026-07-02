package main

import (
	"encoding/binary"
	"os"
	"testing"

	"github.com/ctdk/briefs-utils/briefs"
)

// writeXattrBlock writes a synthetic xattr block at the given absolute block.
// A zero checksum is used so VerifyChainChecksum treats it as a legacy block.
func writeXattrBlock(t *testing.T, path string, absBlock, version, usedSize, entryCount uint32) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	buf := make([]byte, 4096)
	binary.LittleEndian.PutUint32(buf[0:], briefs.MagicXattr)
	binary.LittleEndian.PutUint32(buf[4:], version)
	binary.LittleEndian.PutUint32(buf[8:], usedSize)
	binary.LittleEndian.PutUint32(buf[12:], entryCount)
	if _, err := f.WriteAt(buf, int64(absBlock*4096)); err != nil {
		t.Fatalf("write xattr block: %v", err)
	}
}

// writeXattrBlockV2 writes a synthetic v2 xattr block with optional chain link.
func writeXattrBlockV2(t *testing.T, path string, absBlock, usedSize, entryCount uint32,
	nextBlock uint64, flags uint32) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	buf := make([]byte, 4096)
	binary.LittleEndian.PutUint32(buf[0:], briefs.MagicXattr)
	binary.LittleEndian.PutUint32(buf[4:], 2)
	binary.LittleEndian.PutUint32(buf[8:], usedSize)
	binary.LittleEndian.PutUint32(buf[12:], entryCount)
	binary.LittleEndian.PutUint64(buf[16:], nextBlock)
	binary.LittleEndian.PutUint32(buf[24:], flags)
	if _, err := f.WriteAt(buf, int64(absBlock*4096)); err != nil {
		t.Fatalf("write xattr block: %v", err)
	}
}

// TestVerifyXattrBlockVersion verifies that fsck accepts v1 and v2 xattr blocks
// and rejects unsupported versions.
func TestVerifyXattrBlockVersion(t *testing.T) {
	tmp, err := os.CreateTemp(t.TempDir(), "xattr-version-*.briefs")
	if err != nil {
		t.Fatalf("create temp: %v", err)
	}
	tmp.Close()

	path := tmp.Name()

	t.Run("bad-version", func(t *testing.T) {
		writeXattrBlock(t, path, 10, 3, 16, 0)
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer f.Close()
		fs := &fsckState{
			file:       f,
			sb:         &briefs.SuperblockLayout{BlockSize: 4096, TotalBlocks: 100},
			usedBlocks: make(map[uint64]bool),
		}
		in := &briefs.Inode{InodeNumber: 2, Magic: briefs.MagicInode, XattrOffset: 10, XattrSize: 16}
		verifyXattrBlock(fs, 2, in, 4096)
		if fs.errors == 0 {
			t.Errorf("expected error for xattr version 3, got none")
		}
	})

	t.Run("good-version-v1", func(t *testing.T) {
		writeXattrBlock(t, path, 11, 1, 16, 0)
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer f.Close()
		fs := &fsckState{
			file:       f,
			sb:         &briefs.SuperblockLayout{BlockSize: 4096, TotalBlocks: 100},
			usedBlocks: make(map[uint64]bool),
		}
		in := &briefs.Inode{InodeNumber: 3, Magic: briefs.MagicInode, XattrOffset: 11, XattrSize: 16}
		verifyXattrBlock(fs, 3, in, 4096)
		if fs.errors != 0 {
			t.Errorf("expected no errors for xattr version 1, got %d", fs.errors)
		}
		if !fs.usedBlocks[11] {
			t.Errorf("expected xattr block 11 to be marked used")
		}
	})

	t.Run("good-version-v2", func(t *testing.T) {
		writeXattrBlockV2(t, path, 12, 32, 0, 0, 0)
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer f.Close()
		fs := &fsckState{
			file:       f,
			sb:         &briefs.SuperblockLayout{BlockSize: 4096, TotalBlocks: 100},
			usedBlocks: make(map[uint64]bool),
		}
		in := &briefs.Inode{InodeNumber: 4, Magic: briefs.MagicInode, XattrOffset: 12, XattrSize: 32}
		verifyXattrBlock(fs, 4, in, 4096)
		if fs.errors != 0 {
			t.Errorf("expected no errors for xattr version 2, got %d", fs.errors)
		}
		if !fs.usedBlocks[12] {
			t.Errorf("expected xattr block 12 to be marked used")
		}
	})

	t.Run("chain-v2", func(t *testing.T) {
		writeXattrBlockV2(t, path, 20, 32, 0, 21, briefs.BrieFSXattrFlagCont)
		writeXattrBlockV2(t, path, 21, 32, 0, 0, 0)
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer f.Close()
		fs := &fsckState{
			file:       f,
			sb:         &briefs.SuperblockLayout{BlockSize: 4096, TotalBlocks: 100},
			usedBlocks: make(map[uint64]bool),
		}
		in := &briefs.Inode{InodeNumber: 5, Magic: briefs.MagicInode, XattrOffset: 20, XattrSize: 32}
		verifyXattrBlock(fs, 5, in, 4096)
		if fs.errors != 0 {
			t.Errorf("expected no errors for v2 chain, got %d", fs.errors)
		}
		if !fs.usedBlocks[20] || !fs.usedBlocks[21] {
			t.Errorf("expected xattr chain blocks 20 and 21 to be marked used")
		}
	})

	t.Run("chain-loop", func(t *testing.T) {
		writeXattrBlockV2(t, path, 30, 32, 0, 31, 0)
		writeXattrBlockV2(t, path, 31, 32, 0, 30, 0)
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer f.Close()
		fs := &fsckState{
			file:       f,
			sb:         &briefs.SuperblockLayout{BlockSize: 4096, TotalBlocks: 100},
			usedBlocks: make(map[uint64]bool),
		}
		in := &briefs.Inode{InodeNumber: 6, Magic: briefs.MagicInode, XattrOffset: 30, XattrSize: 32}
		verifyXattrBlock(fs, 6, in, 4096)
		if fs.errors == 0 {
			t.Errorf("expected error for xattr chain loop, got none")
		}
	})
}
