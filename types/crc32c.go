package types

import (
	"encoding/binary"
)

// CRC32C (Castagnoli polynomial) implementation.
// This matches the table-driven implementation in the kernel module's
// crc32c.c, used for journal record checksums and extent chain block
// checksums.

const crc32cPolynomial = 0x1EDC6F41

// crc32cTable is the lookup table for the Castagnoli polynomial.
var crc32cTable = func() [256]uint32 {
	var table [256]uint32
	for i := uint32(0); i < 256; i++ {
		crc := i
		for j := 0; j < 8; j++ {
			if crc&1 != 0 {
				crc = (crc >> 1) ^ crc32cPolynomial
			} else {
				crc >>= 1
			}
		}
		table[i] = crc
	}
	return table
}()

// Crc32c computes the CRC32C checksum of data, using crc as the initial
// value. Passing 0 as crc computes the checksum from scratch.
func Crc32c(crc uint32, data []byte) uint32 {
	if len(data) == 0 {
		return crc
	}
	crc = ^crc
	for _, b := range data {
		idx := (crc ^ uint32(b)) & 0xFF
		crc = (crc >> 8) ^ crc32cTable[idx]
	}
	return ^crc
}

// ComputeJournalRecordChecksum computes the CRC32C checksum for a journal
// record header. It matches the kernel's compute_record_checksum(), which
// chains a single CRC over type + flags + data_len + data rather than XORing
// four separate CRCs.
func ComputeJournalRecordChecksum(recordType uint32, flags uint32, data []byte) uint32 {
	var tmp [4]byte
	var c uint32

	binary.LittleEndian.PutUint32(tmp[:], recordType)
	c = Crc32c(0, tmp[:])
	binary.LittleEndian.PutUint32(tmp[:], flags)
	c = Crc32c(c, tmp[:])
	binary.LittleEndian.PutUint32(tmp[:], uint32(len(data)))
	c = Crc32c(c, tmp[:])
	c = Crc32c(c, data)

	return c
}

// ComputeChainChecksum computes the CRC32C checksum for an extent chain block.
// The checksum covers bytes [0, ExtentChainChecksumOffset), i.e. the header
// and extents but excluding the checksum field itself.
func ComputeChainChecksum(buf []byte, blockSize uint64) uint64 {
	if uint64(len(buf)) < blockSize || blockSize < ExtentChainChecksumOffset {
		return 0
	}
	return uint64(Crc32c(0, buf[:ExtentChainChecksumOffset]))
}

// VerifyChainChecksum checks the checksum field of an extent chain block.
// A zero checksum is treated as legacy (no checksum) and returns nil.
func VerifyChainChecksum(buf []byte, blockSize uint64) error {
	stored := ReadChainChecksum(buf, blockSize)
	if stored == 0 {
		return nil // legacy block with no checksum
	}
	computed := ComputeChainChecksum(buf, blockSize)
	if stored != computed {
		return ErrChecksumMismatch
	}
	return nil
}
