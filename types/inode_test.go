package types

import (
	"encoding/binary"
	"os"
	"testing"
)

func TestInodeUnmarshal(t *testing.T) {
	data := make([]byte, 512)

	// Manually construct a valid inode
	binary.LittleEndian.PutUint64(data[0:], 1)                    // InodeNumber
	binary.LittleEndian.PutUint64(data[8:], MagicInode)           // Magic
	binary.LittleEndian.PutUint32(data[16:], 0x41ED)             // Filemode (dir + 0755)
	binary.LittleEndian.PutUint32(data[20:], 1000)               // Uid
	binary.LittleEndian.PutUint32(data[24:], 100)                // Gid
	binary.LittleEndian.PutUint32(data[28:], 0)                  // _Pad0
	binary.LittleEndian.PutUint64(data[32:], 4096)               // FileSize
	binary.LittleEndian.PutUint64(data[40:], 1000000)            // CtimeSec
	binary.LittleEndian.PutUint64(data[48:], 0)                  // CtimeNsec
	binary.LittleEndian.PutUint64(data[56:], 1000000)            // AtimeSec
	binary.LittleEndian.PutUint64(data[64:], 0)                  // AtimeNsec
	binary.LittleEndian.PutUint64(data[72:], 1000000)            // MtimeSec
	binary.LittleEndian.PutUint64(data[80:], 0)                  // MtimeNsec
	binary.LittleEndian.PutUint64(data[88:], 1000000)            // CreationTimeSec
	binary.LittleEndian.PutUint64(data[96:], 0)                  // CreationTimeNsec
	binary.LittleEndian.PutUint32(data[104:], 2)                 // Nlinks
	binary.LittleEndian.PutUint32(data[108:], 1)                 // NumExtentsInline
	binary.LittleEndian.PutUint64(data[112:], 0)                 // ExtentInlineBase
	binary.LittleEndian.PutUint64(data[120:], 1)                 // NumExtentsTotal

	// Inline extent 0: offset=0, phys=100, len=1, flags=0, pad=0
	binary.LittleEndian.PutUint64(data[128:], 0)
	binary.LittleEndian.PutUint64(data[136:], 100)
	binary.LittleEndian.PutUint64(data[144:], 1)
	binary.LittleEndian.PutUint32(data[152:], 0)
	binary.LittleEndian.PutUint32(data[156:], 0)

	// XattrOffset, XattrSize
	binary.LittleEndian.PutUint64(data[384:], 0)
	binary.LittleEndian.PutUint64(data[392:], 0)

	// ParentInode
	binary.LittleEndian.PutUint64(data[400:], 0)

	// Unused, Flags
	binary.LittleEndian.PutUint32(data[408:], 0)
	binary.LittleEndian.PutUint32(data[412:], 0)

	// DirTrieRoot
	binary.LittleEndian.PutUint64(data[416:], 91)

	// Rdev
	binary.LittleEndian.PutUint64(data[424:], 0)

	in, err := UnmarshalInode(data)
	if err != nil {
		t.Fatalf("UnmarshalInode: %v", err)
	}

	if in.InodeNumber != 1 {
		t.Errorf("InodeNumber: want 1, got %d", in.InodeNumber)
	}
	if in.Magic != MagicInode {
		t.Errorf("Magic: want 0x%X, got 0x%X", MagicInode, in.Magic)
	}
	if in.Filemode != 0x41ED {
		t.Errorf("Filemode: want 0x41ED, got 0x%X", in.Filemode)
	}
	if in.Uid != 1000 {
		t.Errorf("Uid: want 1000, got %d", in.Uid)
	}
	if in.Gid != 100 {
		t.Errorf("Gid: want 100, got %d", in.Gid)
	}
	if in.FileSize != 4096 {
		t.Errorf("FileSize: want 4096, got %d", in.FileSize)
	}
	if in.Nlinks != 2 {
		t.Errorf("Nlinks: want 2, got %d", in.Nlinks)
	}
	if in.NumExtentsInline != 1 {
		t.Errorf("NumExtentsInline: want 1, got %d", in.NumExtentsInline)
	}
	if in.NumExtentsTotal != 1 {
		t.Errorf("NumExtentsTotal: want 1, got %d", in.NumExtentsTotal)
	}
	if in.InlineExtents[0].Offset != 0 {
		t.Errorf("Extent[0].Offset: want 0, got %d", in.InlineExtents[0].Offset)
	}
	if in.InlineExtents[0].Phys != 100 {
		t.Errorf("Extent[0].Phys: want 100, got %d", in.InlineExtents[0].Phys)
	}
	if in.InlineExtents[0].Len != 1 {
		t.Errorf("Extent[0].Len: want 1, got %d", in.InlineExtents[0].Len)
	}
	if in.DirTrieRoot != 91 {
		t.Errorf("DirTrieRoot: want 91, got %d", in.DirTrieRoot)
	}
	if !in.IsDir() {
		t.Error("expected IsDir() = true")
	}
}

func TestInodeUnmarshalBadMagic(t *testing.T) {
	data := make([]byte, 512)
	// UnmarshalInode doesn't validate magic — it just parses.
	// A zero magic is valid as far as the parser is concerned.
	in, err := UnmarshalInode(data)
	if err != nil {
		t.Fatalf("UnmarshalInode: %v", err)
	}
	if in.Magic != 0 {
		t.Errorf("Magic: want 0, got 0x%X", in.Magic)
	}
}

func TestInodeUnmarshalZeroMagic(t *testing.T) {
	data := make([]byte, 512)
	// Magic at offset 8 is 0 — should be treated as unallocated
	// But UnmarshalInode doesn't check for zero magic, it just parses
	// The caller (verifyInode in fsck) checks for zero magic.
	// This test just verifies UnmarshalInode doesn't crash on zero magic.
	in, err := UnmarshalInode(data)
	if err != nil {
		t.Fatalf("UnmarshalInode: %v", err)
	}
	if in.Magic != 0 {
		t.Errorf("Magic: want 0, got 0x%X", in.Magic)
	}
}

func TestInodeIsDir(t *testing.T) {
	dir := &Inode{Filemode: ModeDir | 0755}
	file := &Inode{Filemode: ModeFile | 0644}

	if !dir.IsDir() {
		t.Error("expected IsDir() = true for directory")
	}
	if file.IsDir() {
		t.Error("expected IsDir() = false for file")
	}
}

func TestInodeIsFile(t *testing.T) {
	dir := &Inode{Filemode: ModeDir | 0755}
	file := &Inode{Filemode: ModeFile | 0644}

	if dir.IsFile() {
		t.Error("expected IsFile() = false for directory")
	}
	if !file.IsFile() {
		t.Error("expected IsFile() = true for file")
	}
}

func TestInodeSetInlineExtent(t *testing.T) {
	in := &Inode{}
	err := in.SetInlineExtent(0, 10, 200, 5, 0)
	if err != nil {
		t.Fatalf("SetInlineExtent: %v", err)
	}
	if in.NumExtentsInline != 1 {
		t.Errorf("NumExtentsInline: want 1, got %d", in.NumExtentsInline)
	}
	if in.NumExtentsTotal != 1 {
		t.Errorf("NumExtentsTotal: want 1, got %d", in.NumExtentsTotal)
	}
	if in.InlineExtents[0].Offset != 10 {
		t.Errorf("Offset: want 10, got %d", in.InlineExtents[0].Offset)
	}
	if in.InlineExtents[0].Phys != 200 {
		t.Errorf("Phys: want 200, got %d", in.InlineExtents[0].Phys)
	}
	if in.InlineExtents[0].Len != 5 {
		t.Errorf("Len: want 5, got %d", in.InlineExtents[0].Len)
	}
}

func TestInodeSetInlineExtentOutOfRange(t *testing.T) {
	in := &Inode{}
	err := in.SetInlineExtent(8, 0, 0, 0, 0)
	if err == nil {
		t.Fatal("expected error for index 8")
	}
}


func TestInodeInlineDataRoundTrip(t *testing.T) {
	in := NewInode(2, ModeFile|0644)
	in.Flags = InodeFlagInlineData
	in.FileSize = 50
	for i := 0; i < 50; i++ {
		in.InlineData[i] = byte('a' + i%26)
	}

	// Extent fields should be ignored when the inline flag is set.
	in.NumExtentsInline = 0
	in.NumExtentsTotal = 0

	f, err := os.CreateTemp("", "briefs-inline-inode-*.img")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	defer os.Remove(f.Name())
	defer f.Close()

	if err := in.WriteAt(f, 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}

	data := make([]byte, DefaultInodeSize)
	if _, err := f.ReadAt(data, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}

	got, err := UnmarshalInode(data)
	if err != nil {
		t.Fatalf("UnmarshalInode: %v", err)
	}

	if got.Flags != InodeFlagInlineData {
		t.Errorf("Flags: want 0x%X, got 0x%X", InodeFlagInlineData, got.Flags)
	}
	if got.FileSize != 50 {
		t.Errorf("FileSize: want 50, got %d", got.FileSize)
	}
	if got.NumExtentsTotal != 0 {
		t.Errorf("NumExtentsTotal: want 0, got %d", got.NumExtentsTotal)
	}
	for i := 0; i < 50; i++ {
		if got.InlineData[i] != byte('a'+i%26) {
			t.Errorf("InlineData[%d]: want %c, got %c", i, byte('a'+i%26), got.InlineData[i])
		}
	}
}
