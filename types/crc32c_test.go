package types

import (
	"testing"
)

// TestCrc32cKnownVectors verifies the CRC32C implementation matches the
// kernel's table-driven algorithm in crc32c.c. The "123456789" value below
// is the value produced by that algorithm; it intentionally does not match
// the standard Castagnoli test vector because the kernel uses a non-reflected
// table built from the polynomial 0x1EDC6F41.
func TestCrc32cKnownVectors(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want uint32
	}{
		{"empty", nil, 0},
		{"empty slice", []byte{}, 0},
		{"123456789", []byte("123456789"), 0xF28417BE},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Crc32c(0, tc.data)
			if got != tc.want {
				t.Fatalf("Crc32c(0, %q) = 0x%08X, want 0x%08X", tc.data, got, tc.want)
			}
		})
	}
}

func TestComputeJournalRecordChecksum(t *testing.T) {
	// The kernel computes:
	//   crc32c(0, type) ^ crc32c(0, flags) ^ crc32c(0, data_len) ^ crc32c(0, data)
	// For type=1, flags=0, data_len=0, data=[] the result is:
	//   crc32c(0, [1,0,0,0]) ^ crc32c(0, [0,0,0,0]) ^ crc32c(0, [0,0,0,0]) ^ crc32c(0, [])
	var buf [4]byte
	buf[0] = 1
	typeCRC := Crc32c(0, buf[:])
	zeroCRC := Crc32c(0, []byte{0, 0, 0, 0})
	want := typeCRC ^ zeroCRC ^ zeroCRC ^ 0

	got := ComputeJournalRecordChecksum(1, 0, nil)
	if got != want {
		t.Fatalf("ComputeJournalRecordChecksum(1, 0, nil) = 0x%08X, want 0x%08X", got, want)
	}
}

func TestChainChecksumRoundTrip(t *testing.T) {
	blockSize := uint64(4096)
	buf := make([]byte, blockSize)

	// Set up a minimal extent chain block.
	// Header: next=0, num_extents=1, pad=0
	// Extent at offset 16: offset=1, phys=100, len=1, flags=0
	// Checksum field at offset ExtentChainChecksumOffset.
	buf[8] = 1 // num_extents_in_block = 1
	buf[16] = 1
	buf[24] = 100
	buf[32] = 1

	checksum := ComputeChainChecksum(buf, blockSize)
	if checksum == 0 {
		t.Fatalf("ComputeChainChecksum returned zero for non-empty block")
	}

	// Write the checksum in little-endian at the checksum offset.
	for i := uint64(0); i < 8; i++ {
		buf[ExtentChainChecksumOffset+i] = byte(checksum >> (i * 8))
	}

	if err := VerifyChainChecksum(buf, blockSize); err != nil {
		t.Fatalf("VerifyChainChecksum failed for valid block: %v", err)
	}

	// Corrupt a covered byte and verify that verification fails.
	buf[20] ^= 0xFF
	if err := VerifyChainChecksum(buf, blockSize); err == nil {
		t.Fatalf("VerifyChainChecksum passed for corrupted block")
	}
}
