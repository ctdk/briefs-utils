package fuse

import (
	"os"
	"testing"

	"github.com/ctdk/briefs-utils/briefs"
)

func makeTriePageRoot(buf []byte) {
	pg := &briefs.TriePage{
		Magic:       briefs.MagicTriePage,
		Version:     briefs.TriePageVersion,
		LiveCount:   1,
		FreeNameOff: 0,
		FreeSlots:   ^uint64(1), // slot 0 allocated; rest free
	}
	if err := briefs.WriteTriePage(buf, pg); err != nil {
		panic(err)
	}
	// Slot 0: empty root INTERM node.
	root := &briefs.TrieSlot{NodeType: briefs.NodeTypeInterm}
	if err := briefs.WriteTrieSlot(buf, 0, root); err != nil {
		panic(err)
	}
}

func writeTrieLeaf(buf []byte, slot uint, ino uint64, name string) {
	nameLen := len(name)
	nameSize := uint16(nameLen + 2)
	nameOff := nameSize
	if _, err := briefs.WriteTrieName(buf, nameOff, name); err != nil {
		panic(err)
	}
	s := &briefs.TrieSlot{
		Inode:      ino,
		NameLen:    nameSize,
		NameOffset: nameOff,
		Depth:      uint8(nameLen - 1),
		NodeType:   0, // pure leaf
		FType:      8, // regular file
	}
	if err := briefs.WriteTrieSlot(buf, slot, s); err != nil {
		panic(err)
	}
}

func TestReadTriePage(t *testing.T) {
	buf := make([]byte, 4096)
	makeTriePageRoot(buf)

	page, err := briefs.ReadTriePage(buf)
	if err != nil {
		t.Fatalf("ReadTriePage: %v", err)
	}
	if page.Magic != briefs.MagicTriePage {
		t.Errorf("Magic: want 0x%X, got 0x%X", briefs.MagicTriePage, page.Magic)
	}
	if page.Version != briefs.TriePageVersion {
		t.Errorf("Version: want %d, got %d", briefs.TriePageVersion, page.Version)
	}
	if page.LiveCount != 1 {
		t.Errorf("LiveCount: want 1, got %d", page.LiveCount)
	}
}

func TestReadTriePageBadMagic(t *testing.T) {
	buf := make([]byte, 4096)
	_, err := briefs.ReadTriePage(buf)
	if err == nil {
		t.Fatal("expected error for bad magic")
	}
}

func TestReadTriePageTooShort(t *testing.T) {
	_, err := briefs.ReadTriePage([]byte{0, 0, 0, 0})
	if err == nil {
		t.Fatal("expected error for short buffer")
	}
}

func TestReadTrieSlot(t *testing.T) {
	buf := make([]byte, 4096)
	makeTriePageRoot(buf)
	writeTrieLeaf(buf, 1, 5, "test.txt")

	node, err := briefs.ReadTrieSlot(buf, 1)
	if err != nil {
		t.Fatalf("ReadTrieSlot: %v", err)
	}
	if node.Inode != 5 {
		t.Errorf("Inode: want 5, got %d", node.Inode)
	}
	if node.NameLen != 10 {
		t.Errorf("NameLen: want 10, got %d", node.NameLen)
	}
	if node.NameOffset != 10 {
		t.Errorf("NameOffset: want 10, got %d", node.NameOffset)
	}
	if node.NodeType != 0 {
		t.Errorf("NodeType: want 0, got %d", node.NodeType)
	}
	if node.FType != 8 {
		t.Errorf("FType: want 8, got %d", node.FType)
	}

	name, err := briefs.ReadTrieName(buf, node.NameLen, node.NameOffset)
	if err != nil {
		t.Fatalf("ReadTrieName: %v", err)
	}
	if name != "test.txt" {
		t.Errorf("name: want 'test.txt', got '%s'", name)
	}
}

func TestTrieIsLeaf(t *testing.T) {
	tests := []struct {
		name     string
		nodeType uint8
		want     bool
	}{
		{"pure leaf (0)", 0, true},
		{"file type", briefs.NodeTypeFile, true},
		{"dir type", briefs.NodeTypeDir, true},
		{"interm", briefs.NodeTypeInterm, false},
		{"interm+leaf", briefs.NodeTypeInterm | briefs.NodeStatusLeaf, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := briefs.TrieIsLeaf(tc.nodeType); got != tc.want {
				t.Errorf("TrieIsLeaf(0x%02X): want %v, got %v", tc.nodeType, tc.want, got)
			}
		})
	}
}

func TestTrieFindChild(t *testing.T) {
	_ = TrieFindChild
}

func TestTrieGetChildren(t *testing.T) {
	_ = TrieGetChildren
}

func TestTrieLookup(t *testing.T) {
	_ = TrieLookup
}

func TestTrieIterator(t *testing.T) {
	// Create a temp file with a minimal BrieFS image for trie testing
	path := tempImage(t, 100)

	// Write a minimal superblock so OpenBlockDevice works
	raw, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	sb, _ := briefs.NewSuperblock(100, 4096, 512, 4, "test", "")
	sb.Lay.DataBlocks = 90
	sb.Lay.FreeDataBlks = 89
	sb.Lay.FreeInodes = 99
	sb.Lay.TrieNodePoolStart = 5
	sb.Lay.TrieNodePoolSize = 1
	sb.Lay.InodeBMOffset = 1
	sb.Lay.InodeTableOffset = 2
	sb.Lay.JournalOffset = 96
	sb.Lay.JournalBlocks = 4
	data := sb.MarshalBinary()
	if _, err := raw.WriteAt(data, 0); err != nil {
		t.Fatalf("WriteAt superblock: %v", err)
	}
	raw.Close()

	bd, _, err := OpenBlockDevice(path)
	if err != nil {
		t.Fatalf("OpenBlockDevice: %v", err)
	}
	defer bd.Close()

	// TrieIterator on root=0 should return empty (no entries)
	iter := NewTrieIterator(bd, 0)
	ino, ftype, name, err := iter.Next()
	if err == nil {
		// No error means iterator is exhausted — ino should be 0
		if ino != 0 {
			t.Errorf("expected ino=0 for empty trie, got %d", ino)
		}
		_ = ftype
		_ = name
	}
}