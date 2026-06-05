// Package types defines the BrieFS on-disk format.
package types

// directory entry related structs and methods

import (
	"encoding/binary"
)

// DirEntry is a compact on-disk directory entry (16 bytes).
// Names are stored in the trailing variable-length name region of the
// directory block, referenced by NameOff/NameLen.
// NameOff is the offset from the end of the block to the start of the
// name entry (including the 2-byte length prefix).
type DirEntry struct {
	Inode    uint64
	Type     uint8  // file type (S_IFMT bits)
	Flags    uint8
	Reserved [2]byte // padding to 16-byte alignment
	NameLen  uint16 // 1..BRIEFS_NAME_LEN (255)
	NameOff  uint16 // offset from block end into name region
}

// DirBlock is a directory block header (16 bytes).
// Followed by a variable-length array of DirEntry structs, then a
// packed name region growing downward from block_size.
//
// Names are stored with a 2-byte length prefix for forward scanning:
//   [len:2][name bytes...]
// The region grows downward so the newest name is closest to block end.
// NameOff = offset from block end to the start of the name entry
type DirBlock struct {
	Magic      uint32 // "DRYR" - 0x44525952
	NumEntries uint32
	DataSize   uint32 // bytes used by entries (header + entries)
	NamesSize  uint32 // bytes used by packed name region
}

// NewDirBlock creates a directory block buffer (4096 bytes) with the
// given entries and their variable-length names packed.
func NewDirBlock(entries []DirBlockEntry) []byte {
	buf := make([]byte, 4096)
	blockSize := 4096

	// Write header
	binary.LittleEndian.PutUint32(buf[0:], 0x44525952) // "DRYR"
	binary.LittleEndian.PutUint32(buf[4:], uint32(len(entries)))

	// Entry array starts at offset 16
	hdrEnd := 16
	entrySz := 16 // Go struct is padded to 16 bytes (12 bytes data + 4 padding)

	// Write entries and pack names
	namePos := blockSize // grows downward
	for i, e := range entries {
		nameLen := len(e.Name)
		if nameLen < 1 || nameLen > 255 {
			continue
		}

		// Pack name: [len:2][name:nameLen] prepended from end
		entryStart := namePos - (2 + nameLen)
		if entryStart < hdrEnd+(i+1)*entrySz {
			// Out of space — stop
			break
		}
		binary.LittleEndian.PutUint16(buf[entryStart:], uint16(nameLen))
		copy(buf[entryStart+2:], e.Name)

		// Write DirEntry at position hdrEnd + i*16
		off := hdrEnd + i*entrySz
		binary.LittleEndian.PutUint64(buf[off:], e.Inode)
		buf[off+8] = e.Type
		buf[off+9] = e.Flags
		buf[off+10] = 0 // Reserved
		buf[off+11] = 0 // Reserved
		binary.LittleEndian.PutUint16(buf[off+12:], uint16(nameLen))
		binary.LittleEndian.PutUint16(buf[off+14:], uint16(blockSize-entryStart))

		namePos = entryStart
	}

	dirDataSize := uint32(hdrEnd + len(entries)*entrySz)
	namesSize := uint32(blockSize - namePos)
	binary.LittleEndian.PutUint32(buf[8:], dirDataSize)
	binary.LittleEndian.PutUint32(buf[12:], namesSize)

	return buf
}

// DirBlockEntry is a logical directory entry (before serialization).
type DirBlockEntry struct {
	Inode uint64
	Type  uint8
	Flags uint8
	Name  string
}
