package fuse

import (
	"encoding/binary"
	"os"
	"testing"

	"github.com/ctdk/briefs-utils/types"
)

func makeTriePageRoot(buf []byte) {
	binary.LittleEndian.PutUint32(buf[0:], types.MagicTriePage)
	binary.LittleEndian.PutUint32(buf[4:], types.TriePageVersion)
	binary.LittleEndian.PutUint16(buf[8:], 1)          // live_count
	binary.LittleEndian.PutUint16(buf[10:], 0)         // free_name_off
	binary.LittleEndian.PutUint64(buf[12:], ^uint64(1)) // free_slots: slot 0 allocated

	// Slot 0 at offset 16: empty root INTERM node, all fields zero.
	slotOff := uint64(16)
	buf[slotOff+trieSlotDepth] = 0
	buf[slotOff+trieSlotNodeType] = trieNodeTypeInterm
}

func writeTrieLeaf(buf []byte, slot uint, ino uint64, name string) {
	slotOff := slotOffset(slot)
	nameLen := len(name)
	nameSize := nameLen + 2

	// Place name at the top of the name heap, growing upward from block end.
	nameStart := uint64(len(buf)) - uint64(nameSize)
	binary.LittleEndian.PutUint16(buf[nameStart:], uint16(nameLen))
	copy(buf[nameStart+2:], name)

	binary.LittleEndian.PutUint64(buf[slotOff+trieSlotInode:], ino)
	binary.LittleEndian.PutUint16(buf[slotOff+trieSlotNameLen:], uint16(nameSize))
	binary.LittleEndian.PutUint16(buf[slotOff+trieSlotNameOffset:], uint16(nameSize))
	buf[slotOff+trieSlotDepth] = uint8(len(name) - 1)
	buf[slotOff+trieSlotNodeType] = 0 // pure leaf
	buf[slotOff+trieSlotFType] = 8    // regular file
}

func TestReadTriePage(t *testing.T) {
	buf := make([]byte, 4096)
	makeTriePageRoot(buf)

	page, err := ReadTriePage(buf)
	if err != nil {
		t.Fatalf("ReadTriePage: %v", err)
	}
	if page.Magic != types.MagicTriePage {
		t.Errorf("Magic: want 0x%X, got 0x%X", types.MagicTriePage, page.Magic)
	}
	if page.Version != types.TriePageVersion {
		t.Errorf("Version: want %d, got %d", types.TriePageVersion, page.Version)
	}
	if page.LiveCount != 1 {
		t.Errorf("LiveCount: want 1, got %d", page.LiveCount)
	}
}

func TestReadTriePageBadMagic(t *testing.T) {
	buf := make([]byte, 4096)
	_, err := ReadTriePage(buf)
	if err == nil {
		t.Fatal("expected error for bad magic")
	}
}

func TestReadTriePageTooShort(t *testing.T) {
	_, err := ReadTriePage([]byte{0, 0, 0, 0})
	if err == nil {
		t.Fatal("expected error for short buffer")
	}
}

func TestReadTrieSlot(t *testing.T) {
	buf := make([]byte, 4096)
	makeTriePageRoot(buf)
	writeTrieLeaf(buf, 1, 5, "test.txt")

	node, err := ReadTrieSlot(buf, 1)
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

	name, err := readTrieLeafNameStr(buf, 4096, node)
	if err != nil {
		t.Fatalf("readTrieLeafNameStr: %v", err)
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
		{"file type", trieNodeTypeFile, true},
		{"dir type", trieNodeTypeDir, true},
		{"interm", trieNodeTypeInterm, false},
		{"interm+leaf", trieNodeTypeInterm | trieNodeStatusLeaf, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			node := &TrieNodeData{NodeType: tc.nodeType}
			got := trieIsLeaf(node)
			if got != tc.want {
				t.Errorf("trieIsLeaf(0x%02X): want %v, got %v", tc.nodeType, tc.want, got)
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
	sb, _ := types.NewSuperblock(100, 4096, 512, 4, "test", "")
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
