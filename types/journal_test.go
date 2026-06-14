package types

import (
	"bytes"
	"testing"
)

func TestCheckpointMarshalRoundTrip(t *testing.T) {
	cp := &Checkpoint{
		Seq:            7,
		RecordCount:    42,
		Reserved1:      0,
		LogSequenceEnd: 99,
		TrieRootNode:   123,
		FreeDataCount:  9876,
		FreeInodeCount: 543,
		Reserved2:      0,
	}

	data, err := cp.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary: %v", err)
	}
	if len(data) != CheckpointSize {
		t.Fatalf("marshal size: got %d, want %d", len(data), CheckpointSize)
	}

	var got Checkpoint
	if err := got.UnmarshalBinary(data); err != nil {
		t.Fatalf("UnmarshalBinary: %v", err)
	}

	if got != *cp {
		t.Errorf("round-trip mismatch: got %+v, want %+v", got, *cp)
	}
}

func TestCheckpointUnmarshalTooShort(t *testing.T) {
	var cp Checkpoint
	if err := cp.UnmarshalBinary(make([]byte, CheckpointSize-1)); err == nil {
		t.Fatal("expected error for short payload")
	}
}

func TestCheckpointMarshalLayout(t *testing.T) {
	cp := &Checkpoint{
		Seq:            0x0102030405060708,
		RecordCount:    0x11121314,
		Reserved1:      0x15161718,
		LogSequenceEnd: 0x2122232425262728,
		TrieRootNode:   0x3132333435363738,
		FreeDataCount:  0x4142434445464748,
		FreeInodeCount: 0x5152535455565758,
		Reserved2:      0x6162636465666768,
	}
	data, err := cp.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary: %v", err)
	}

	want := []byte{
		0x08, 0x07, 0x06, 0x05, 0x04, 0x03, 0x02, 0x01, // Seq
		0x14, 0x13, 0x12, 0x11, // RecordCount
		0x18, 0x17, 0x16, 0x15, // Reserved1
		0x28, 0x27, 0x26, 0x25, 0x24, 0x23, 0x22, 0x21, // LogSequenceEnd
		0x38, 0x37, 0x36, 0x35, 0x34, 0x33, 0x32, 0x31, // TrieRootNode
		0x48, 0x47, 0x46, 0x45, 0x44, 0x43, 0x42, 0x41, // FreeDataCount
		0x58, 0x57, 0x56, 0x55, 0x54, 0x53, 0x52, 0x51, // FreeInodeCount
		0x68, 0x67, 0x66, 0x65, 0x64, 0x63, 0x62, 0x61, // Reserved2
	}
	if !bytes.Equal(data, want) {
		t.Errorf("layout mismatch:\n got %x\nwant %x", data, want)
	}
}
