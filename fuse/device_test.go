package fuse

import (
	"os"
	"testing"
)

func TestBlockDeviceReadWrite(t *testing.T) {
	path := tempImage(t, 100)

	bd, blockSize, err := OpenBlockDevice(path)
	if err != nil {
		t.Fatalf("OpenBlockDevice: %v", err)
	}
	defer bd.Close()

	if blockSize != 4096 {
		t.Errorf("blockSize: want 4096, got %d", blockSize)
	}
	if bd.BlockSize() != 4096 {
		t.Errorf("BlockSize(): want 4096, got %d", bd.BlockSize())
	}

	// Write a block via raw file I/O (OpenBlockDevice opens read-only)
	raw, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	data := make([]byte, 4096)
	copy(data, "BRIEFS TEST DATA")
	if _, err := raw.WriteAt(data, 4096*5); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	raw.Close()

	// Read it back via BlockDevice
	read, err := bd.ReadBlock(5)
	if err != nil {
		t.Fatalf("ReadBlock: %v", err)
	}
	if string(read[:16]) != "BRIEFS TEST DATA" {
		t.Errorf("ReadBlock: want 'BRIEFS TEST DATA', got '%s'", string(read[:16]))
	}

	// ReadBlocks
	blocks, err := bd.ReadBlocks(5, 3)
	if err != nil {
		t.Fatalf("ReadBlocks: %v", err)
	}
	if len(blocks) != 3 {
		t.Fatalf("ReadBlocks: want 3 blocks, got %d", len(blocks))
	}
	if string(blocks[0][:16]) != "BRIEFS TEST DATA" {
		t.Errorf("blocks[0]: want 'BRIEFS TEST DATA', got '%s'", string(blocks[0][:16]))
	}
}

func TestBlockDeviceReadAt(t *testing.T) {
	path := tempImage(t, 10)

	bd, _, err := OpenBlockDevice(path)
	if err != nil {
		t.Fatalf("OpenBlockDevice: %v", err)
	}
	defer bd.Close()

	// Read via ReadAt interface
	p := make([]byte, 16)
	n, err := bd.ReadAt(p, 4096*3)
	if err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if n != 16 {
		t.Errorf("ReadAt: want 16 bytes, got %d", n)
	}
}

func TestBlockDeviceWriteBlockWrongSize(t *testing.T) {
	path := tempImage(t, 10)

	bd, _, err := OpenBlockDevice(path)
	if err != nil {
		t.Fatalf("OpenBlockDevice: %v", err)
	}
	defer bd.Close()

	err = bd.WriteBlock(0, []byte("too short"))
	if err == nil {
		t.Fatal("expected error for wrong-sized write")
	}
}

// tempImage creates a temporary file of the given number of 4096-byte blocks.
func tempImage(t *testing.T, blocks int) string {
	t.Helper()
	f, err := os.CreateTemp("", "briefs-test-*.img")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	path := f.Name()
	if err := f.Truncate(int64(blocks) * 4096); err != nil {
		f.Close()
		os.Remove(path)
		t.Fatalf("Truncate: %v", err)
	}
	f.Close()
	t.Cleanup(func() { os.Remove(path) })
	return path
}
