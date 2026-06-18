package types

import (
	"bytes"
	"testing"
	"unsafe"

	"github.com/google/uuid"
)

func TestSuperblockLayoutSize(t *testing.T) {
	if got := int(unsafe.Sizeof(SuperblockLayout{})); got != BrieFSSuperSize {
		t.Fatalf("SuperblockLayout size: want %d, got %d", BrieFSSuperSize, got)
	}
	sb, err := NewSuperblock(100, 4096, 512, 4, "test", "")
	if err != nil {
		t.Fatalf("Error creating superblock: %v", err)
	}
	data := sb.MarshalBinary()
	if len(data) != BrieFSSuperSize {
		t.Fatalf("MarshalBinary length: want %d, got %d", BrieFSSuperSize, len(data))
	}
}

func TestSuperblockLayoutMarshalRoundTrip(t *testing.T) {
	sb, err := NewSuperblock(10000, 4096, 512, 64, "test-label", "")
	if err != nil {
		t.Fatalf("Error creating superblock: %v", err)
	}

	// Set fields that NewSuperblock doesn't set (normally done by mkfs)
	sb.Lay.DataBlocks = 9845
	sb.Lay.FreeDataBlks = 9844
	sb.Lay.FreeInodes = 639
	sb.Lay.EATOffset = 86
	sb.Lay.EATBlocks = 1
	sb.Lay.TrieRootBlock = 91
	sb.Lay.TrieBlocksUsed = 1
	sb.Lay.TrieNodePoolStart = 87
	sb.Lay.TrieNodePoolSize = 4
	sb.Lay.InodeBMOffset = 1
	sb.Lay.InodeBMBlocks = 4
	sb.Lay.InodeTableOffset = 6
	sb.Lay.CheckpointSeq = 0
	sb.Lay.JournalLogStart = 9936
	sb.Lay.JournalLogEnd = 9936

	data := sb.MarshalBinary()

	var got SuperblockLayout
	if err := got.UnmarshalBinary(data); err != nil {
		t.Fatalf("UnmarshalBinary: %v", err)
	}

	// Compare all fields
	fields := []struct {
		name string
		want, got interface{}
	}{
		{"Magic", sb.Lay.Magic, got.Magic},
		{"MajorVer", sb.Lay.MajorVer, got.MajorVer},
		{"MinorVer", sb.Lay.MinorVer, got.MinorVer},
		{"PatchVer", sb.Lay.PatchVer, got.PatchVer},
		{"TotalBlocks", sb.Lay.TotalBlocks, got.TotalBlocks},
		{"DataBlocks", sb.Lay.DataBlocks, got.DataBlocks},
		{"BlockSize", sb.Lay.BlockSize, got.BlockSize},
		{"InodeSize", sb.Lay.InodeSize, got.InodeSize},
		{"BlocksGrp", sb.Lay.BlocksGrp, got.BlocksGrp},
		{"InodesGrp", sb.Lay.InodesGrp, got.InodesGrp},
		{"FSCreated", sb.Lay.FSCreated, got.FSCreated},
		{"FSLastMount", sb.Lay.FSLastMount, got.FSLastMount},
		{"FSLastChkpt", sb.Lay.FSLastChkpt, got.FSLastChkpt},
		{"FreeDataBlks", sb.Lay.FreeDataBlks, got.FreeDataBlks},
		{"FreeInodes", sb.Lay.FreeInodes, got.FreeInodes},
		{"RootIno", sb.Lay.RootIno, got.RootIno},
		{"FeatCompat", sb.Lay.FeatCompat, got.FeatCompat},
		{"FeatROCompat", sb.Lay.FeatROCompat, got.FeatROCompat},
		{"FeatIncompat", sb.Lay.FeatIncompat, got.FeatIncompat},
		{"EATOffset", sb.Lay.EATOffset, got.EATOffset},
		{"EATBlocks", sb.Lay.EATBlocks, got.EATBlocks},
		{"TrieRootBlock", sb.Lay.TrieRootBlock, got.TrieRootBlock},
		{"TrieBlocksUsed", sb.Lay.TrieBlocksUsed, got.TrieBlocksUsed},
		{"TrieNodePoolStart", sb.Lay.TrieNodePoolStart, got.TrieNodePoolStart},
		{"TrieNodePoolSize", sb.Lay.TrieNodePoolSize, got.TrieNodePoolSize},
		{"InodeBMOffset", sb.Lay.InodeBMOffset, got.InodeBMOffset},
		{"InodeBMBlocks", sb.Lay.InodeBMBlocks, got.InodeBMBlocks},
		{"InodeTableOffset", sb.Lay.InodeTableOffset, got.InodeTableOffset},
		{"JournalOffset", sb.Lay.JournalOffset, got.JournalOffset},
		{"JournalBlocks", sb.Lay.JournalBlocks, got.JournalBlocks},
		{"CheckpointSeq", sb.Lay.CheckpointSeq, got.CheckpointSeq},
		{"JournalLogStart", sb.Lay.JournalLogStart, got.JournalLogStart},
		{"JournalLogEnd", sb.Lay.JournalLogEnd, got.JournalLogEnd},
	}

	for _, f := range fields {
		if f.want != f.got {
			t.Errorf("field %s: want %v, got %v", f.name, f.want, f.got)
		}
	}

	if !bytes.Equal(sb.Lay.UUID[:], got.UUID[:]) {
		t.Errorf("UUID mismatch")
	}
	if !bytes.Equal(sb.Lay.Label[:], got.Label[:]) {
		t.Errorf("Label mismatch")
	}
	for i := 0; i < 4; i++ {
		if sb.Lay.ReservedJournal[i] != got.ReservedJournal[i] {
			t.Errorf("ReservedJournal[%d]: want %d, got %d", i, sb.Lay.ReservedJournal[i], got.ReservedJournal[i])
		}
	}
}

func TestSuperblockLayoutUnmarshalErrors(t *testing.T) {
	t.Run("too short", func(t *testing.T) {
		var sb SuperblockLayout
		err := sb.UnmarshalBinary([]byte{0, 0, 0, 0})
		if err == nil {
			t.Fatal("expected error for short data")
		}
	})
}

func TestReadSuperblock(t *testing.T) {
	sb, err := NewSuperblock(10000, 4096, 512, 64, "test", "")
	if err != nil {
		t.Fatalf("Error creating superblock: %v", err)
	}
	sb.Lay.DataBlocks = 9845
	sb.Lay.FreeDataBlks = 9844
	sb.Lay.FreeInodes = 639
	sb.Lay.EATOffset = 86
	sb.Lay.EATBlocks = 1
	sb.Lay.TrieNodePoolStart = 87
	sb.Lay.TrieNodePoolSize = 4
	sb.Lay.InodeBMOffset = 1
	sb.Lay.InodeTableOffset = 6
	sb.Lay.JournalOffset = 9936
	sb.Lay.JournalBlocks = 64

	data := sb.MarshalBinary()
	// Pad to 4096 bytes (ReadSuperblock reads blockSize bytes)
	buf := make([]byte, 4096)
	copy(buf, data)
	r := bytes.NewReader(buf)

	got, err := ReadSuperblock(r, 4096)
	if err != nil {
		t.Fatalf("ReadSuperblock: %v", err)
	}
	if got.Magic != MagicSuperblock {
		t.Errorf("magic: want 0x%X, got 0x%X", MagicSuperblock, got.Magic)
	}
	if got.TotalBlocks != 10000 {
		t.Errorf("TotalBlocks: want 10000, got %d", got.TotalBlocks)
	}
}

func TestReadSuperblockBadMagic(t *testing.T) {
	data := make([]byte, 4096)
	r := bytes.NewReader(data)
	_, err := ReadSuperblock(r, 4096)
	if err == nil {
		t.Fatal("expected error for bad magic")
	}
}

func TestSuperblockUUID(t *testing.T) {
	uuidStr := "bee364c3-fff3-4967-9fb2-f03b5d29eecf"
	sb, err := NewSuperblock(100, 4096, 512, 4, "test", uuidStr)
	if err != nil {
		t.Fatalf("Error creating superblock with custom UUID: %v", err)
	}
	uuidP, _ := uuid.Parse(uuidStr)
	if len(uuidP) != len(sb.Lay.UUID) {
		t.Fatalf("original UUID and the superblock UUID do not have the same length: got %d and %d respectively", len(uuidP), len(sb.Lay.UUID))
	}
	for i := 0; i < len(uuidP); i++ {
		if uuidP[i] != sb.Lay.UUID[i] {
			t.Errorf("uuid bytes do not match at position %d! 0x%x vs. 0x%x", i, uuidP[i], sb.Lay.UUID[i])
		}
	}
}
