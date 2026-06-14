// Package fuse implements a FUSE filesystem for BrieFS.
package fuse

import (
	"encoding/binary"
	"fmt"

	"github.com/ctdk/briefs-utils/types"
)

// Trie page/slot layout constants (must match briefs.h).
const trieSlotSize = 36
const triePageHeaderSize = 20
const trieSlotCount = types.TrieSlotsPerBlock

// Trie page header offsets.
const (
	triePageMagicOff       = 0
	triePageVersionOff     = 4
	triePageLiveCountOff   = 8
	triePageFreeNameOffOff = 10
	triePageFreeSlotsOff   = 12
)

// Trie slot field offsets within a 36-byte slot.
const (
	trieSlotFirstChild  = 0
	trieSlotNextSibling = 8
	trieSlotInode       = 16
	trieSlotNameLen     = 24
	trieSlotNameOffset  = 26
	trieSlotDepth       = 28
	trieSlotNodeType    = 29
	trieSlotByteVal     = 30
	trieSlotFType       = 31
	trieSlotFlags       = 32
	trieSlotChildCount  = 34
)

// Trie node types (mirrors briefs.h).
const (
	trieNodeTypeFile    = 0x01
	trieNodeTypeDir     = 0x02
	trieNodeTypeInterm  = 0x04
	trieNodeStatusLeaf  = 0x08
)

// TrieNodeData holds the parsed fields from a trie node slot.
type TrieNodeData struct {
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

// TriePageData holds the parsed header of a packed trie page.
type TriePageData struct {
	Magic       uint32
	Version     uint32
	LiveCount   uint16
	FreeNameOff uint16
	FreeSlots   uint64
}

// slotOffset returns the byte offset of slot `slot` within a page buffer.
func slotOffset(slot uint) uint64 {
	return triePageHeaderSize + uint64(slot)*trieSlotSize
}

// ReadTriePage reads and validates a packed trie page header.
func ReadTriePage(buf []byte) (*TriePageData, error) {
	if uint64(len(buf)) < triePageHeaderSize {
		return nil, fmt.Errorf("buffer too small for trie page header: %d", len(buf))
	}
	magic := binary.LittleEndian.Uint32(buf[triePageMagicOff:])
	if magic != types.MagicTriePage {
		return nil, fmt.Errorf("bad trie page magic: 0x%08x (expected 0x%08x)", magic, types.MagicTriePage)
	}
	return &TriePageData{
		Magic:       magic,
		Version:     binary.LittleEndian.Uint32(buf[triePageVersionOff:]),
		LiveCount:   binary.LittleEndian.Uint16(buf[triePageLiveCountOff:]),
		FreeNameOff: binary.LittleEndian.Uint16(buf[triePageFreeNameOffOff:]),
		FreeSlots:   binary.LittleEndian.Uint64(buf[triePageFreeSlotsOff:]),
	}, nil
}

// ReadTrieSlot reads a single node slot from a page buffer.
func ReadTrieSlot(buf []byte, slot uint) (*TrieNodeData, error) {
	off := slotOffset(slot)
	if off+trieSlotSize > uint64(len(buf)) {
		return nil, fmt.Errorf("slot %d out of range in %d-byte buffer", slot, len(buf))
	}
	return &TrieNodeData{
		FirstChild:  binary.LittleEndian.Uint64(buf[off+trieSlotFirstChild:]),
		NextSibling: binary.LittleEndian.Uint64(buf[off+trieSlotNextSibling:]),
		Inode:       binary.LittleEndian.Uint64(buf[off+trieSlotInode:]),
		NameLen:     binary.LittleEndian.Uint16(buf[off+trieSlotNameLen:]),
		NameOffset:  binary.LittleEndian.Uint16(buf[off+trieSlotNameOffset:]),
		Depth:       buf[off+trieSlotDepth],
		NodeType:    buf[off+trieSlotNodeType],
		ByteVal:     buf[off+trieSlotByteVal],
		FType:       buf[off+trieSlotFType],
		Flags:       binary.LittleEndian.Uint16(buf[off+trieSlotFlags:]),
		ChildCount:  binary.LittleEndian.Uint16(buf[off+trieSlotChildCount:]),
	}, nil
}

// trieIsLeaf returns true if the node is a leaf (pure leaf or INTERM with STATUS_LEAF).
func trieIsLeaf(node *TrieNodeData) bool {
	return node.NodeType != trieNodeTypeInterm || (node.NodeType&trieNodeStatusLeaf != 0)
}

// readTrieLeafName reads the leaf name from the trailing region of a trie page buffer.
// Returns the name bytes (not including the 2-byte length prefix).
func readTrieLeafName(buf []byte, blockSize uint64, node *TrieNodeData) ([]byte, error) {
	nameOff := node.NameOffset
	if nameOff == 0 || uint64(nameOff) > blockSize {
		return nil, fmt.Errorf("invalid name_offset %d", nameOff)
	}
	nameStart := blockSize - uint64(nameOff)
	if nameStart+2 > uint64(len(buf)) {
		return nil, fmt.Errorf("name start out of range: %d", nameStart)
	}
	prefixLen := int(binary.LittleEndian.Uint16(buf[nameStart:]))
	if prefixLen < 1 || prefixLen > types.BrieFSMaxNameLen || nameStart+2+uint64(prefixLen) > blockSize {
		return nil, fmt.Errorf("invalid name length %d", prefixLen)
	}
	name := make([]byte, prefixLen)
	copy(name, buf[nameStart+2:nameStart+2+uint64(prefixLen)])
	return name, nil
}

// readTrieLeafNameStr reads the leaf name as a string (for comparisons).
func readTrieLeafNameStr(buf []byte, blockSize uint64, node *TrieNodeData) (string, error) {
	name, err := readTrieLeafName(buf, blockSize, node)
	if err != nil {
		return "", err
	}
	return string(name), nil
}

// TrieLookup finds an entry by name in a directory trie.
func TrieLookup(dev *BlockDevice, dirTrieRoot uint64, name string) (ino uint64, ftype uint8, err error) {
	if types.TrieRefIsNull(dirTrieRoot) {
		return 0, 0, fmt.Errorf("no trie root")
	}

	blockSize := dev.BlockSize()
	cur := dirTrieRoot
	nameBytes := []byte(name)
	nameLen := len(nameBytes)

	for pos := 0; pos < nameLen; pos++ {
		bval := byte(nameBytes[pos])

		if pos == nameLen-1 {
			child, err := TrieFindChild(dev, cur, bval)
			if err != nil {
				return 0, 0, err
			}
			if types.TrieRefIsNull(child) {
				return 0, 0, fmt.Errorf("not found")
			}

			cbuf, node, err := trieReadNode(dev, child)
			if err != nil {
				return 0, 0, err
			}

			if !trieIsLeaf(node) {
				return 0, 0, fmt.Errorf("not found")
			}

			leafName, err := readTrieLeafNameStr(cbuf, blockSize, node)
			if err != nil {
				return 0, 0, fmt.Errorf("corrupt trie leaf at ref %d: %w", child, err)
			}
			if leafName != name {
				return 0, 0, fmt.Errorf("not found")
			}

			return node.Inode, node.FType, nil
		}

		child, err := TrieFindChild(dev, cur, bval)
		if err != nil {
			return 0, 0, err
			}
		if types.TrieRefIsNull(child) {
			return 0, 0, fmt.Errorf("not found")
		}

		cbuf, node, err := trieReadNode(dev, child)
		if err != nil {
			return 0, 0, err
		}

		if node.NodeType&trieNodeTypeInterm == 0 {
			// Pure leaf where we need an INTERM: check full name.
			leafName, err := readTrieLeafNameStr(cbuf, blockSize, node)
			if err != nil {
				return 0, 0, fmt.Errorf("corrupt trie leaf at ref %d: %w", child, err)
			}
			if leafName == name {
				return node.Inode, node.FType, nil
			}
			return 0, 0, fmt.Errorf("not found")
		}
		cur = child
	}

	return 0, 0, fmt.Errorf("not found")
}

// trieReadNode reads the page containing a node reference and returns the
// raw page buffer plus the parsed slot.
func trieReadNode(dev *BlockDevice, nodeRef uint64) ([]byte, *TrieNodeData, error) {
	block := types.TrieRefBlock(nodeRef)
	slot := types.TrieRefSlot(nodeRef)
	buf, err := dev.ReadBlock(block)
	if err != nil {
		return nil, nil, fmt.Errorf("read trie page %d: %w", block, err)
	}
	if _, err := ReadTriePage(buf); err != nil {
		return nil, nil, fmt.Errorf("parse trie page %d: %w", block, err)
	}
	node, err := ReadTrieSlot(buf, slot)
	if err != nil {
		return nil, nil, fmt.Errorf("read trie slot %d/%d: %w", block, slot, err)
	}
	return buf, node, nil
}

// TrieFindChild finds a child node by byte value in the sibling chain.
func TrieFindChild(dev *BlockDevice, parentRef uint64, byteVal byte) (uint64, error) {
	pbuf, pnode, err := trieReadNode(dev, parentRef)
	if err != nil {
		return 0, err
	}

	child := pnode.FirstChild
	for !types.TrieRefIsNull(child) {
		cbuf, cnode, err := trieReadNode(dev, child)
		if err != nil {
			return 0, err
		}
		if cnode.ByteVal == byteVal {
			return child, nil
		}
		child = cnode.NextSibling
		_ = cbuf
	}

	_ = pbuf
	return 0, nil
}

// TrieGetChildren returns all children of a trie node.
func TrieGetChildren(dev *BlockDevice, parentRef uint64) ([]uint64, error) {
	pbuf, pnode, err := trieReadNode(dev, parentRef)
	if err != nil {
		return nil, err
	}
	_ = pbuf

	var children []uint64
	child := pnode.FirstChild
	for !types.TrieRefIsNull(child) {
		children = append(children, child)
		cbuf, cnode, err := trieReadNode(dev, child)
		if err != nil {
			return nil, err
		}
		child = cnode.NextSibling
		_ = cbuf
	}
	return children, nil
}

// TrieIterator provides a depth-first walk of a directory trie for readdir.
type TrieIterator struct {
	dev         *BlockDevice
	blockSize   uint64
	stack       [256]uint64
	leafEmitted [256]bool
	sp          int
	pending     bool
	pendingIno  uint64
	pendingType uint8
	pendingName string
	dirTrieRoot uint64
}

// NewTrieIterator creates a new iterator for the given directory.
func NewTrieIterator(dev *BlockDevice, dirTrieRoot uint64) *TrieIterator {
	ti := &TrieIterator{
		dev:         dev,
		blockSize:   dev.BlockSize(),
		dirTrieRoot: dirTrieRoot,
	}
	if !types.TrieRefIsNull(dirTrieRoot) {
		ti.stack[0] = dirTrieRoot
		ti.sp = 1
		ti.leafEmitted[0] = false
	}
	return ti
}

// Next returns the next directory entry from the trie.
func (ti *TrieIterator) Next() (uint64, uint8, string, error) {
	blockSize := ti.blockSize

	if ti.pending {
		ti.pending = false
		return ti.pendingIno, ti.pendingType, ti.pendingName, nil
	}

	for ti.sp > 0 {
		ref := ti.stack[ti.sp-1]
		emitted := ti.leafEmitted[ti.sp-1]
		ti.sp--

		buf, node, err := trieReadNode(ti.dev, ref)
		if err != nil {
			continue
		}

		if emitted {
			child := node.FirstChild
			var pushed []uint64
			for !types.TrieRefIsNull(child) {
				pushed = append(pushed, child)
				cbuf, cnode, err := trieReadNode(ti.dev, child)
				if err != nil {
					break
				}
				child = cnode.NextSibling
				_ = cbuf
			}
			for i := len(pushed) - 1; i >= 0; i-- {
				if ti.sp < 256 {
					ti.stack[ti.sp] = pushed[i]
					ti.leafEmitted[ti.sp] = false
					ti.sp++
				}
			}
			continue
		}

		if trieIsLeaf(node) {
			leafName, err := readTrieLeafNameStr(buf, blockSize, node)
			if err != nil {
				continue
			}
			ino := node.Inode
			ftype := node.FType

			if !types.TrieRefIsNull(node.FirstChild) && ti.sp < 256 {
				ti.stack[ti.sp] = ref
				ti.leafEmitted[ti.sp] = true
				ti.sp++
			}

			return ino, ftype, leafName, nil
		}

		// Pure INTERM node: push children.
		child := node.FirstChild
		var children []uint64
		for !types.TrieRefIsNull(child) {
			children = append(children, child)
			cbuf, cnode, err := trieReadNode(ti.dev, child)
			if err != nil {
				break
			}
			child = cnode.NextSibling
			_ = cbuf
		}
		for i := len(children) - 1; i >= 0; i-- {
			if ti.sp < 256 {
				ti.stack[ti.sp] = children[i]
				ti.leafEmitted[ti.sp] = false
				ti.sp++
			}
		}
	}

	return 0, 0, "", nil
}
