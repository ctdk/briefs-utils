package briefs

import (
	"encoding/binary"
	"fmt"
)

// On-disk sizes of the packed trie page header and node slot. These mirror the
// __attribute__((packed)) struct briefs_trie_page / briefs_trie_node in the
// kernel (briefs.h): Go has no packed structs, so the types below use gendisk's
// "packed" mode (Size() returns the declared size, verified against the
// field-width sum at generation time). The named constants are for offset
// arithmetic in the helpers below.
const (
	TriePageHeaderSize = 20
	TrieSlotSize       = 36
)

// TriePage is the 20-byte packed header of a directory-trie node page
// (struct briefs_trie_page). It is immediately followed by TrieSlotsPerBlock
// TrieSlot entries; a variable-length name heap grows downward from the end of
// the block.
//
//go:briefs-disk packed size=20
type TriePage struct {
	Magic       uint32
	Version     uint32
	LiveCount   uint16
	FreeNameOff uint16
	FreeSlots   uint64
}

// TrieSlot is one 36-byte packed directory-trie node slot
// (struct briefs_trie_node). FirstChild and NextSibling are node references
// (block<<TrieSlotBits | slot), not block numbers. NameLen is the full name
// entry size (2-byte length prefix + name bytes); NameOffset is measured from
// the end of the block.
//
//go:briefs-disk packed size=36
type TrieSlot struct {
	FirstChild  uint64
	NextSibling uint64
	Inode       uint64
	NameLen     uint16
	NameOffset  uint16
	Depth       uint8
	NodeType    uint8
	ByteVal     uint8
	FType       uint8
	Flags       uint16
	ChildCount  uint16
}

// SlotOffset returns the byte offset of slot `slot` within a trie page buffer.
func SlotOffset(slot uint) int {
	return TriePageHeaderSize + int(slot)*TrieSlotSize
}

// ReadTriePage reads and validates a packed trie page header. Only the magic is
// checked (the kernel does not mandate a particular version); the full header
// is returned so every caller sees live_count, free_name_off and free_slots
// uniformly -- previously fsck read a subset and fuse read them all.
func ReadTriePage(buf []byte) (*TriePage, error) {
	if uint64(len(buf)) < TriePageHeaderSize {
		return nil, fmt.Errorf("buffer too small for trie page header: %d", len(buf))
	}
	magic := binary.LittleEndian.Uint32(buf[0:])
	if magic != MagicTriePage {
		return nil, fmt.Errorf("bad trie page magic 0x%08x (expected 0x%08x)", magic, MagicTriePage)
	}
	p := &TriePage{}
	if err := p.UnmarshalBinary(buf[:TriePageHeaderSize]); err != nil {
		return nil, err
	}
	return p, nil
}

// ReadTrieSlot reads a single node slot from a page buffer.
func ReadTrieSlot(buf []byte, slot uint) (*TrieSlot, error) {
	off := SlotOffset(slot)
	if off+TrieSlotSize > len(buf) {
		return nil, fmt.Errorf("slot %d out of range in %d-byte buffer", slot, len(buf))
	}
	s := &TrieSlot{}
	if err := s.UnmarshalBinary(buf[off : off+TrieSlotSize]); err != nil {
		return nil, err
	}
	return s, nil
}

// WriteTriePage writes a trie page header into the first TriePageHeaderSize
// bytes of buf, leaving the slot array and name heap untouched.
func WriteTriePage(buf []byte, p *TriePage) error {
	if uint64(len(buf)) < TriePageHeaderSize {
		return fmt.Errorf("buffer too small for trie page header: %d", len(buf))
	}
	data, err := p.MarshalBinary()
	if err != nil {
		return err
	}
	copy(buf[:TriePageHeaderSize], data)
	return nil
}

// WriteTrieSlot writes a node slot into buf at the given slot index.
func WriteTrieSlot(buf []byte, slot uint, s *TrieSlot) error {
	off := SlotOffset(slot)
	if off+TrieSlotSize > len(buf) {
		return fmt.Errorf("slot %d out of range in %d-byte buffer", slot, len(buf))
	}
	data, err := s.MarshalBinary()
	if err != nil {
		return err
	}
	copy(buf[off:off+TrieSlotSize], data)
	return nil
}

// ReadTrieName reads a leaf name from the trailing name heap of a trie page.
// The kernel stores name_len = 2 (length prefix) + actual name length, with the
// 2-byte little-endian length prefix at (len(buf) - name_offset) followed by the
// name bytes. This enforces that invariant and returns the name without the
// prefix. Previously fsck enforced name_len == stored_len + 2 and fuse did not;
// both now share this single check.
func ReadTrieName(buf []byte, nameLen, nameOffset uint16) (string, error) {
	const maxNameLen = uint16(BrieFSMaxNameLen + 2)
	if nameLen < 2 || nameLen > maxNameLen || nameOffset == 0 {
		return "", fmt.Errorf("invalid name_len %d / name_offset %d", nameLen, nameOffset)
	}
	if int(nameOffset) > len(buf) {
		return "", fmt.Errorf("name_offset %d out of range (buf %d)", nameOffset, len(buf))
	}
	nameStart := len(buf) - int(nameOffset)
	if nameStart < 0 || nameStart+2 > len(buf) {
		return "", fmt.Errorf("name start %d out of range", nameStart)
	}
	storedLen := int(binary.LittleEndian.Uint16(buf[nameStart:]))
	if storedLen < 1 || storedLen > BrieFSMaxNameLen {
		return "", fmt.Errorf("invalid stored name length %d", storedLen)
	}
	if uint16(storedLen)+2 != nameLen {
		return "", fmt.Errorf("name_len %d != stored length %d + 2", nameLen, storedLen)
	}
	if nameStart+2+storedLen > len(buf) {
		return "", fmt.Errorf("name extends past buffer")
	}
	return string(buf[nameStart+2 : nameStart+2+storedLen]), nil
}

// WriteTrieName writes a name entry (2-byte little-endian length prefix + name
// bytes) into buf at the heap position (len(buf) - nameOffset) and returns the
// on-disk name_len value (len(name)+2, or 0 for a nameless/intermediate node).
// It does not allocate name-heap space; the caller supplies nameOffset
// (the live FUSE mutator derives it from the page's free_name_off, fsck
// compaction from its own packing cursor).
func WriteTrieName(buf []byte, nameOffset uint16, name string) (uint16, error) {
	if len(name) == 0 {
		return 0, nil
	}
	if nameOffset == 0 {
		return 0, fmt.Errorf("name offset not allocated")
	}
	nameStart := len(buf) - int(nameOffset)
	if nameStart < 0 || nameStart+2+len(name) > len(buf) {
		return 0, fmt.Errorf("name write out of range")
	}
	binary.LittleEndian.PutUint16(buf[nameStart:], uint16(len(name)))
	copy(buf[nameStart+2:], name)
	return uint16(len(name)) + 2, nil
}

// TrieIsLeaf reports whether a node carries a leaf entry: a pure leaf (no
// NODE_TYPE_INTERM bit) or an intermediate node that also has NODE_STATUS_LEAF.
// Mirrors the kernel's TRIE_IS_LEAF.
func TrieIsLeaf(nodeType uint8) bool {
	return (nodeType&NodeTypeInterm) == 0 || (nodeType&NodeStatusLeaf) != 0
}