package briefs

import (
	"encoding/binary"
	"testing"
)

func TestDirBlockNew(t *testing.T) {
	entries := []DirBlockEntry{
		{Inode: 2, Type: 4, Name: "."},
		{Inode: 1, Type: 4, Name: ".."},
		{Inode: 3, Type: 8, Name: "file.txt"},
	}

	buf := NewDirBlock(entries)

	// Verify magic
	magic := binary.LittleEndian.Uint32(buf[0:])
	if magic != 0x44525952 {
		t.Errorf("magic: want 0x44525952, got 0x%X", magic)
	}

	// Verify entry count
	numEntries := binary.LittleEndian.Uint32(buf[4:])
	if numEntries != 3 {
		t.Errorf("numEntries: want 3, got %d", numEntries)
	}

	// Verify data size
	dataSize := binary.LittleEndian.Uint32(buf[8:])
	expectedDataSize := uint32(16 + 3*16) // header + 3 entries
	if dataSize != expectedDataSize {
		t.Errorf("dataSize: want %d, got %d", expectedDataSize, dataSize)
	}

	// Verify names size
	namesSize := binary.LittleEndian.Uint32(buf[12:])
	if namesSize == 0 {
		t.Error("namesSize should be non-zero")
	}

	// Verify first entry
	entry0Inode := binary.LittleEndian.Uint64(buf[16:])
	if entry0Inode != 2 {
		t.Errorf("entry[0].Inode: want 2, got %d", entry0Inode)
	}
	if buf[24] != 4 {
		t.Errorf("entry[0].Type: want 4, got %d", buf[24])
	}

	// Verify second entry
	entry1Inode := binary.LittleEndian.Uint64(buf[32:])
	if entry1Inode != 1 {
		t.Errorf("entry[1].Inode: want 1, got %d", entry1Inode)
	}

	// Verify third entry
	entry2Inode := binary.LittleEndian.Uint64(buf[48:])
	if entry2Inode != 3 {
		t.Errorf("entry[2].Inode: want 3, got %d", entry2Inode)
	}
	entry2NameLen := binary.LittleEndian.Uint16(buf[60:])
	if entry2NameLen != 8 {
		t.Errorf("entry[2].NameLen: want 8, got %d", entry2NameLen)
	}
}

func TestDirBlockEmpty(t *testing.T) {
	buf := NewDirBlock(nil)

	numEntries := binary.LittleEndian.Uint32(buf[4:])
	if numEntries != 0 {
		t.Errorf("numEntries: want 0, got %d", numEntries)
	}

	dataSize := binary.LittleEndian.Uint32(buf[8:])
	if dataSize != 16 {
		t.Errorf("dataSize: want 16, got %d", dataSize)
	}

	namesSize := binary.LittleEndian.Uint32(buf[12:])
	if namesSize != 0 {
		t.Errorf("namesSize: want 0, got %d", namesSize)
	}
}

func TestDirBlockNamePacking(t *testing.T) {
	entries := []DirBlockEntry{
		{Inode: 5, Type: 8, Name: "hello"},
	}

	buf := NewDirBlock(entries)

	// The name should be packed at the end of the block
	// NameOff = offset from block end to name entry
	entry0NameOff := binary.LittleEndian.Uint16(buf[30:]) // offset 16+14
	blockSize := 4096
	nameStart := blockSize - int(entry0NameOff)

	// Name entry: [len:2][name:5]
	nameLen := binary.LittleEndian.Uint16(buf[nameStart:])
	if nameLen != 5 {
		t.Errorf("packed name length: want 5, got %d", nameLen)
	}
	name := string(buf[nameStart+2 : nameStart+2+5])
	if name != "hello" {
		t.Errorf("packed name: want 'hello', got '%s'", name)
	}
}
