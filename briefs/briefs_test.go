package briefs

import (
	"testing"
	"unsafe"
)

// TestJournalRecordEnumValues verifies that the Go journal record enum matches
// the kernel's enum journal_record_type in briefs.h.
func TestJournalRecordEnumValues(t *testing.T) {
	cases := []struct {
		name string
		got  uint32
		want uint32
	}{
		{"JRN_NONE", JRN_NONE, 0},
		{"JRN_EXTENT_ALLOC", JRN_EXTENT_ALLOC, 1},
		{"JRN_EXTENT_FREE", JRN_EXTENT_FREE, 2},
		{"JRN_INODE_UPDATE", JRN_INODE_UPDATE, 3},
		{"JRN_INODE_ALLOC", JRN_INODE_ALLOC, 4},
		{"JRN_INODE_FREE", JRN_INODE_FREE, 5},
		{"JRN_TRIE_ALLOC", JRN_TRIE_ALLOC, 6},
		{"JRN_DIR_UPDATE", JRN_DIR_UPDATE, 7},
		{"JRN_CHECKPOINT", JRN_CHECKPOINT, 8},
		{"JRN_INODE_FULL", JRN_INODE_FULL, 9},
		{"JRN_SYMLINK_DATA", JRN_SYMLINK_DATA, 10},
		{"JRN_XATTR_DATA", JRN_XATTR_DATA, 11},
		{"JRN_END", JRN_END, 12},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}
}

// TestExtentFlagUnwritten verifies the Go constant matches the kernel's
// BRIEFS_EXT_UNWRITTEN bit.
func TestExtentFlagUnwritten(t *testing.T) {
	if ExtentFlagUnwritten != 0x00000001 {
		t.Errorf("ExtentFlagUnwritten = 0x%08X, want 0x%08X", ExtentFlagUnwritten, 0x00000001)
	}
}

// TestExtentSize verifies the on-disk extent is 32 bytes.
func TestExtentSize(t *testing.T) {
	if got := unsafe.Sizeof(Extent{}); got != 32 {
		t.Errorf("sizeof(Extent) = %d, want 32", got)
	}
}

// TestXattrVersion verifies the Go xattr block version matches the kernel.
func TestXattrVersion(t *testing.T) {
	if BrieFSXattrVersion != 2 {
		t.Errorf("BrieFSXattrVersion = %d, want 2", BrieFSXattrVersion)
	}
}

// TestAllocHeaderSize verifies the Go allocator header is exactly 48 bytes,
// matching the kernel's struct alloc_pool_header.
func TestAllocHeaderSize(t *testing.T) {
	if got := unsafe.Sizeof(AllocHeader{}); got != 48 {
		t.Errorf("sizeof(AllocHeader) = %d, want 48", got)
	}
}
