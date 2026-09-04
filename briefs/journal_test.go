package briefs

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

// TestInodeUpdateRecordLayout pins jrn_inode_update to the kernel's 96-byte
// layout with the trailing generation field (kernel commit 33e4019,
// BUILD_BUG_ON briefs.h:1910): 88 bytes of pre-existing fields, generation at
// offset 88.
func TestInodeUpdateRecordLayout(t *testing.T) {
	if JrnInodeUpdateSize != 96 {
		t.Fatalf("JrnInodeUpdateSize: got %d, want 96", JrnInodeUpdateSize)
	}
	if JrnInodeUpdateLegacySize != 88 {
		t.Fatalf("JrnInodeUpdateLegacySize: got %d, want 88", JrnInodeUpdateLegacySize)
	}
	r := &JrnInodeUpdate{
		Ino:        0x0102030405060708,
		Mode:       0x11121314,
		Nlink:      0x15161718,
		Uid:        0x191a1b1c,
		Gid:        0x1d1e1f20,
		FileSize:   0x2122232425262728,
		ATimeSec:   0x3132333435363738,
		ATimeNsec:  0x4142434445464748,
		MTimeSec:   0x5152535455565758,
		MTimeNsec:  0x6162636465666768,
		CTimeSec:   0x7172737475767778,
		CTimeNsec:  0x8182838485868788,
		Flags:      0x91929394,
		Reserved:   0x95969798,
		Generation: 0xa1a2a3a4a5a6a7a8,
	}
	data, err := r.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary: %v", err)
	}
	if len(data) != 96 {
		t.Fatalf("marshal size: got %d, want 96", len(data))
	}
	if got := data[88:96]; !bytes.Equal(got, []byte{0xa8, 0xa7, 0xa6, 0xa5, 0xa4, 0xa3, 0xa2, 0xa1}) {
		t.Errorf("generation not at offset 88: %x", got)
	}

	var back JrnInodeUpdate
	if err := back.UnmarshalBinary(data); err != nil {
		t.Fatalf("UnmarshalBinary: %v", err)
	}
	if back != *r {
		t.Errorf("round-trip mismatch: got %+v, want %+v", back, *r)
	}
}

// TestUnmarshalInodeUpdateLegacy verifies the legacy 88-byte payload (no
// generation field, pre-33e4019 records) parses with Generation == 0 so the
// replay caller can gate the generation guard on payload length, mirroring
// the kernel's rec_data_len check (journal.c:1047).
func TestUnmarshalInodeUpdateLegacy(t *testing.T) {
	r := &JrnInodeUpdate{
		Ino:      42,
		Mode:     0o644,
		Nlink:    1,
		FileSize: 100,
	}
	data, err := r.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary: %v", err)
	}
	legacy := data[:JrnInodeUpdateLegacySize]
	got := UnmarshalInodeUpdate(legacy)
	if got == nil {
		t.Fatal("UnmarshalInodeUpdate(legacy): nil")
	}
	if got.Ino != 42 || got.Mode != 0o644 || got.Nlink != 1 || got.FileSize != 100 {
		t.Errorf("legacy parse mismatch: %+v", got)
	}
	if got.Generation != 0 {
		t.Errorf("legacy parse: Generation = %d, want 0", got.Generation)
	}

	// The full 96-byte payload still parses with its generation.
	full := UnmarshalInodeUpdate(data)
	if full == nil || full.Generation != 0 {
		t.Fatalf("full parse: %+v", full)
	}
}
