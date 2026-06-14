package device

import (
	"os"
	"testing"
)

func TestGetDevice(t *testing.T) {
	f, err := os.CreateTemp("", "briefs-test-*.img")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	path := f.Name()
	if err := f.Truncate(1024 * 1024); err != nil { // 1MB
		f.Close()
		os.Remove(path)
		t.Fatalf("Truncate: %v", err)
	}
	f.Close()
	defer os.Remove(path)

	bd, err := GetDevice(path, 4096)
	if err != nil {
		t.Fatalf("GetDevice: %v", err)
	}

	if bd.Bytes() != 1024*1024 {
		t.Errorf("Bytes: want %d, got %d", 1024*1024, bd.Bytes())
	}
	if bd.Sectors() != 2048 {
		t.Errorf("Sectors: want 2048, got %d", bd.Sectors())
	}
	if bd.KiloBytes() != 1024 {
		t.Errorf("KiloBytes: want 1024, got %d", bd.KiloBytes())
	}
	if bd.MegaBytes() != 1 {
		t.Errorf("MegaBytes: want 1, got %d", bd.MegaBytes())
	}
	if bd.Blocks() != 256 {
		t.Errorf("Blocks: want 256, got %d", bd.Blocks())
	}
}

func TestGetDeviceNonexistent(t *testing.T) {
	_, err := GetDevice("/nonexistent/path", 4096)
	if err == nil {
		t.Fatal("expected error for nonexistent path")
	}
}

func TestCheckMounted(t *testing.T) {
	f, err := os.CreateTemp("", "briefs-test-*.img")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	path := f.Name()
	f.Close()
	defer os.Remove(path)

	// A temp file should not be mounted
	err = CheckMounted(path)
	if err != nil {
		t.Errorf("CheckMounted: expected nil for temp file, got %v", err)
	}
}
