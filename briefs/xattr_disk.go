package briefs

import (
	"encoding/binary"
	"fmt"
)

// On-disk xattr sizes and helpers. These mirror the kernel's
// struct briefs_xattr_header / briefs_xattr_entry (briefs.h:719) and
// briefs_xattr_hdr_size (briefs.h:736). Both structs are naturally aligned
// (next_block uint64 at offset 16 is 8-aligned; size 32 matches), so no
// packed mode is needed and the generated size assertions hold.
const (
	XattrEntrySize = 8
	XattrMaxUsed   = 4044 // header + entries + values <= 4044 (one JRN_XATTR_DATA record)
	XattrMaxChain  = 1024
)

// XattrHeader is the 32-byte v2 header of an xattr block (struct
// briefs_xattr_header). A v1 block uses only the first 16 bytes (magic,
// version, used_size, entry_count); the trailing next_block/flags/reserved
// fields are absent, see XattrHeaderSize.
//
//go:briefs-disk size=32
type XattrHeader struct {
	Magic      uint32
	Version    uint32
	UsedSize   uint32
	EntryCount uint32
	NextBlock  uint64
	Flags      uint32
	Reserved   uint32
}

// XattrEntry is one 8-byte xattr entry record (struct briefs_xattr_entry).
// NameLen includes the namespace prefix; ValueOffset is 0 when ValueLen is 0
// or the whole value is stored in continuation block(s).
//
//go:briefs-disk size=8
type XattrEntry struct {
	NameLen      uint16
	ValueLen     uint16
	NameOffset   uint16
	ValueOffset  uint16
}

// XattrHeaderSize returns the on-disk header size for an xattr block of the
// given version: 16 for v1 (legacy, read-only in fsck), 32 for v2. Mirrors the
// kernel's briefs_xattr_hdr_size.
func XattrHeaderSize(version uint32) int {
	if version == 1 {
		return 16
	}
	return 32
}

// ReadXattrHeader reads and validates an xattr block header. The magic and
// version are checked; the full header is returned so every caller sees
// used_size, entry_count, next_block and flags uniformly. For a v1 block only
// the 16-byte header is read -- the trailing next_block/flags/reserved read as
// zero (v1 has no chaining or flags), matching the kernel's
// briefs_xattr_hdr_size distinction. Previously fsck read a v1 subset and fuse
// read all v2 fields inline; both now share this single check.
func ReadXattrHeader(buf []byte) (*XattrHeader, error) {
	if len(buf) < XattrHeaderSize(1) {
		return nil, fmt.Errorf("buffer too small for xattr header: %d", len(buf))
	}
	magic := binary.LittleEndian.Uint32(buf[0:])
	if magic != MagicXattr {
		return nil, fmt.Errorf("bad xattr magic 0x%08x (expected 0x%08x)", magic, MagicXattr)
	}
	version := binary.LittleEndian.Uint32(buf[4:])
	if version != 1 && version != 2 {
		return nil, fmt.Errorf("unsupported xattr version %d", version)
	}
	hdrSize := XattrHeaderSize(version)
	if len(buf) < hdrSize {
		return nil, fmt.Errorf("buffer too small for v%d xattr header: %d", version, len(buf))
	}
	h := &XattrHeader{}
	if version == 2 {
		if err := h.UnmarshalBinary(buf[:hdrSize]); err != nil {
			return nil, err
		}
	} else {
		// v1: header is 16 bytes (magic, version, used_size, entry_count).
		// Zero-pad to 32 so the generated UnmarshalBinary parses it; the
		// trailing next_block/flags/reserved read as 0 (v1 has neither).
		tmp := make([]byte, XattrHeaderSize(2))
		copy(tmp[:XattrHeaderSize(1)], buf[:XattrHeaderSize(1)])
		if err := h.UnmarshalBinary(tmp); err != nil {
			return nil, err
		}
	}
	return h, nil
}

// WriteXattrHeader writes an xattr block header into buf. For v2 it writes all
// 32 bytes; for v1 it writes only the 16-byte prefix (next_block/flags/reserved
// are omitted), matching the on-disk header size.
func WriteXattrHeader(buf []byte, h *XattrHeader) error {
	hdrSize := XattrHeaderSize(h.Version)
	if len(buf) < hdrSize {
		return fmt.Errorf("buffer too small for xattr header: %d", len(buf))
	}
	data, err := h.MarshalBinary()
	if err != nil {
		return err
	}
	copy(buf[:hdrSize], data[:hdrSize])
	return nil
}

// ReadXattrEntry reads one 8-byte xattr entry at the given byte offset (the
// caller computes entryOff = headerSize + index*XattrEntrySize).
func ReadXattrEntry(buf []byte, entryOff int) (*XattrEntry, error) {
	if entryOff+XattrEntrySize > len(buf) {
		return nil, fmt.Errorf("xattr entry at %d out of range in %d-byte buffer", entryOff, len(buf))
	}
	e := &XattrEntry{}
	if err := e.UnmarshalBinary(buf[entryOff : entryOff+XattrEntrySize]); err != nil {
		return nil, err
	}
	return e, nil
}

// WriteXattrEntry writes one 8-byte xattr entry at the given byte offset.
func WriteXattrEntry(buf []byte, entryOff int, e *XattrEntry) error {
	if entryOff+XattrEntrySize > len(buf) {
		return fmt.Errorf("xattr entry at %d out of range in %d-byte buffer", entryOff, len(buf))
	}
	data, err := e.MarshalBinary()
	if err != nil {
		return err
	}
	copy(buf[entryOff:entryOff+XattrEntrySize], data)
	return nil
}