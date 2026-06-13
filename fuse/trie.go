// Package fuse implements a FUSE filesystem for BrieFS.
package fuse

import (
	"encoding/binary"
	"fmt"

	"github.com/ctdk/briefs-utils/types"
)

// TrieNode is the in-memory representation of a directory trie node.
// Matches struct briefs_trie_node in the kernel module, but we work with
// raw buffers read from disk rather than C struct overlays.
//
// Node layout (32 bytes at start of a 4096-byte block):
//
//	Offset  Size  Field
//	0       4     magic ("TRN ")
//	4       4     child_count
//	8       8     first_child (block number)
//	16      8     next_sibling (block number)
//	24      1     depth
//	25      1     node_type
//	26      1     byte_val (the byte this node represents)
//	27      1     f_type (file type, formerly reserved[0])
//	28      4     reserved
//	32      8     flags
//	40      8     inode (leaf entry)
//	48      2     name_len (leaf entry)
//	50      2     name_offset (from block end to name bytes)
//
// After the struct header, the block has (4096 - name_offset) bytes of
// trailing name data. The first 2 bytes of name data are the name length
// prefix, followed by the actual name bytes.
//
// A leaf name is stored at: block + blockSize - name_offset
// The first 2 bytes at that location are the name length (uint16 LE),
// followed by name_len bytes of the actual name.
const trieNodeHeaderSize = 52
const trieNodeTypeFile = 0x01
const trieNodeTypeDir = 0x02
const trieNodeTypeInterm = 0x04
const trieNodeStatusLeaf = 0x08

// TrieNodeData holds the parsed fields from a trie node block.
// This is a convenience struct for callers that need to inspect trie nodes
// directly (e.g., the FUSE bridge for readdir or lookup).
type TrieNodeData struct {
	Magic       uint32
	ChildCount  uint32
	FirstChild  uint64
	NextSibling uint64
	Depth       uint8
	NodeType    uint8
	ByteVal     uint8
	FType       uint8
	Flags       uint64
	Inode       uint64
	NameLen     uint16
	NameOffset  uint16
}

// ParseTrieNode parses a trie node from a raw block buffer.
func ParseTrieNode(buf []byte) (*TrieNodeData, error) {
	if len(buf) < trieNodeHeaderSize {
		return nil, fmt.Errorf("buffer too small for trie node: %d < %d", len(buf), trieNodeHeaderSize)
	}
	magic := readTrieMagic(buf)
	if magic != types.MagicTrieNode {
		return nil, fmt.Errorf("bad trie magic: 0x%08x (expected 0x%08x)", magic, types.MagicTrieNode)
	}
	return &TrieNodeData{
		Magic:       magic,
		ChildCount:  readTrieChildCount(buf),
		FirstChild:  readTrieFirstChild(buf),
		NextSibling: readTrieNextSibling(buf),
		Depth:       readTrieDepth(buf),
		NodeType:    readTrieNodeType(buf),
		ByteVal:     readTrieByteVal(buf),
		FType:       readTrieFType(buf),
		Flags:       readTrieFlags(buf),
		Inode:       readTrieInode(buf),
		NameLen:     readTrieNameLen(buf),
		NameOffset:  readTrieNameOffset(buf),
	}, nil
}

// TrieReadNode reads a trie node block from disk and validates the magic.
// Returns the parsed node data and the raw buffer.
func TrieReadNode(dev *BlockDevice, block uint64) (*TrieNodeData, []byte, error) {
	buf, err := dev.ReadBlock(block)
	if err != nil {
		return nil, nil, fmt.Errorf("read trie node block %d: %w", block, err)
	}
	node, err := ParseTrieNode(buf)
	if err != nil {
		return nil, buf, fmt.Errorf("parse trie node at block %d: %w", block, err)
	}
	return node, buf, nil
}

// TrieIsLeaf returns true if the node is a leaf (pure leaf or INTERM with STATUS_LEAF).
func TrieIsLeaf(buf []byte) bool {
	nt := readTrieNodeType(buf)
	return nt != trieNodeTypeInterm || (nt&trieNodeStatusLeaf != 0)
}

// TrieReadLeafName reads the leaf name from the trailing region of a trie block.
// Returns the name bytes (not including the 2-byte length prefix).
func TrieReadLeafName(buf []byte, blockSize uint64) ([]byte, error) {
	return readTrieLeafName(buf, blockSize)
}

// TrieReadLeafNameStr reads the leaf name as a string.
func TrieReadLeafNameStr(buf []byte, blockSize uint64) (string, error) {
	return readTrieLeafNameStr(buf, blockSize)
}

// readTrieMagic returns the magic from a trie node block.
func readTrieMagic(buf []byte) uint32 {
	return binary.LittleEndian.Uint32(buf[0:])
}

// readTrieChildCount returns the child_count field.
func readTrieChildCount(buf []byte) uint32 {
	return binary.LittleEndian.Uint32(buf[4:])
}

// readTrieFirstChild returns the first_child field.
func readTrieFirstChild(buf []byte) uint64 {
	return binary.LittleEndian.Uint64(buf[8:])
}

// readTrieNextSibling returns the next_sibling field.
func readTrieNextSibling(buf []byte) uint64 {
	return binary.LittleEndian.Uint64(buf[16:])
}

// readTrieDepth returns the depth field.
func readTrieDepth(buf []byte) uint8 {
	return buf[24]
}

// readTrieNodeType returns the node_type field.
func readTrieNodeType(buf []byte) uint8 {
	return buf[25]
}

// readTrieByteVal returns the byte_val field.
func readTrieByteVal(buf []byte) uint8 {
	return buf[26]
}

// readTrieFType returns the f_type field.
func readTrieFType(buf []byte) uint8 {
	return buf[27]
}

// readTrieFlags returns the flags field (offset 32, 8 bytes).
func readTrieFlags(buf []byte) uint64 {
	return binary.LittleEndian.Uint64(buf[32:])
}

// readTrieInode returns the inode field from a leaf entry.
func readTrieInode(buf []byte) uint64 {
	return binary.LittleEndian.Uint64(buf[40:])
}

// readTrieNameLen returns the name_len field from a leaf entry.
func readTrieNameLen(buf []byte) uint16 {
	return binary.LittleEndian.Uint16(buf[48:])
}

// readTrieNameOffset returns the name_offset field.
func readTrieNameOffset(buf []byte) uint16 {
	return binary.LittleEndian.Uint16(buf[50:])
}

// trieIsLeaf returns true if the node is a leaf (pure leaf or INTERM with STATUS_LEAF).
func trieIsLeaf(buf []byte) bool {
	nt := readTrieNodeType(buf)
	return nt != trieNodeTypeInterm || (nt&trieNodeStatusLeaf != 0)
}

// readTrieLeafName reads the leaf name from the trailing region of a trie block.
// Returns the name bytes (not including the 2-byte length prefix).
func readTrieLeafName(buf []byte, blockSize uint64) ([]byte, error) {
	nameOff := readTrieNameOffset(buf)
	if nameOff == 0 || uint64(nameOff) > blockSize {
		return nil, fmt.Errorf("invalid name_offset %d", nameOff)
	}
	nameStart := blockSize - uint64(nameOff)
	// First 2 bytes at nameStart are the name length (little-endian uint16)
	prefixLen := int(binary.LittleEndian.Uint16(buf[nameStart:]))
	if prefixLen < 1 || prefixLen > types.BrieFSMaxNameLen || nameStart+2+uint64(prefixLen) > blockSize {
		return nil, fmt.Errorf("invalid name length %d", prefixLen)
	}
	name := make([]byte, prefixLen)
	copy(name, buf[nameStart+2:nameStart+2+uint64(prefixLen)])
	return name, nil
}

// readTrieLeafNameStr reads the leaf name as a string (for comparisons).
func readTrieLeafNameStr(buf []byte, blockSize uint64) (string, error) {
	name, err := readTrieLeafName(buf, blockSize)
	if err != nil {
		return "", err
	}
	return string(name), nil
}

// TrieLookup finds an entry by name in a directory trie.
// Returns the inode number and file type, or 0 and -ENOENT equivalent.
func TrieLookup(dev *BlockDevice, dirTrieRoot uint64, name string) (ino uint64, ftype uint8, err error) {
	if dirTrieRoot == 0 {
		return 0, 0, fmt.Errorf("no trie root")
	}

	blockSize := dev.BlockSize()
	cur := dirTrieRoot
	nameBytes := []byte(name)
	nameLen := len(nameBytes)

	for pos := 0; pos < nameLen; pos++ {
		bval := byte(nameBytes[pos])

		if pos == nameLen-1 {
			// Last byte — the target node should be a child of cur.
			child, err := TrieFindChild(dev, cur, bval)
			if err != nil {
				return 0, 0, err
			}
			if child == 0 {
				return 0, 0, fmt.Errorf("not found")
			}

			cbuf, err := dev.ReadBlock(child)
			if err != nil {
				return 0, 0, err
			}

			if !trieIsLeaf(cbuf) {
				return 0, 0, fmt.Errorf("not found")
			}

			// For INTERM+STATUS_LEAF nodes, verify the name matches
			// (could be a prefix collision)
			if readTrieNodeType(cbuf)&trieNodeTypeInterm != 0 &&
				readTrieNodeType(cbuf)&trieNodeStatusLeaf != 0 {
				leafName, err := readTrieLeafNameStr(cbuf, blockSize)
				if err != nil {
					return 0, 0, fmt.Errorf("corrupt trie leaf at block %d: %w", child, err)
				}
				if leafName == name {
					return readTrieInode(cbuf), readTrieFType(cbuf), nil
				}
				// Prefix collision — this INTERM node's leaf is a different name
				// that happens to share a prefix. The actual target must be
				// a deeper child. Continue the loop... but this is the last byte,
				// so we need to check children of this INTERM node.
				// This can't happen at the last byte — if we're at the last byte
				// and the node is INTERM+STATUS_LEAF, and the names differ,
				// then the target name doesn't exist in the trie.
				return 0, 0, fmt.Errorf("not found")
			}

			// Pure leaf — verify exact name match
			leafName, err := readTrieLeafNameStr(cbuf, blockSize)
			if err != nil {
				return 0, 0, fmt.Errorf("corrupt trie leaf at block %d: %w", child, err)
			}
			if leafName != name {
				return 0, 0, fmt.Errorf("not found")
			}

			return readTrieInode(cbuf), readTrieFType(cbuf), nil
		}

		// Not the last byte — find or traverse INTERM child
		child, err := TrieFindChild(dev, cur, bval)
		if err != nil {
			return 0, 0, err
		}
		if child == 0 {
			return 0, 0, fmt.Errorf("not found")
		}
		cur = child
	}

	return 0, 0, fmt.Errorf("not found")
}

// TrieFindChild finds a child node by byte value in the sibling chain.
// Returns 0 if not found.
func TrieFindChild(dev *BlockDevice, parentBlock uint64, byteVal byte) (uint64, error) {
	buf, err := dev.ReadBlock(parentBlock)
	if err != nil {
		return 0, err
	}

	magic := readTrieMagic(buf)
	if magic != types.MagicTrieNode {
		return 0, fmt.Errorf("bad trie magic at block %d: 0x%08x", parentBlock, magic)
	}

	child := readTrieFirstChild(buf)
	for child != 0 {
		cbuf, err := dev.ReadBlock(child)
		if err != nil {
			return 0, err
		}
		cmagic := readTrieMagic(cbuf)
		if cmagic != types.MagicTrieNode {
			return 0, fmt.Errorf("bad trie magic at child block %d: 0x%08x", child, cmagic)
		}
		if readTrieByteVal(cbuf) == byteVal {
			return child, nil
		}
		child = readTrieNextSibling(cbuf)
	}

	return 0, nil
}

// TrieGetChildren returns all children of a trie node.
func TrieGetChildren(dev *BlockDevice, parentBlock uint64) ([]uint64, error) {
	buf, err := dev.ReadBlock(parentBlock)
	if err != nil {
		return nil, err
	}
	magic := readTrieMagic(buf)
	if magic != types.MagicTrieNode {
		return nil, fmt.Errorf("bad trie magic at block %d: 0x%08x", parentBlock, magic)
	}

	var children []uint64
	child := readTrieFirstChild(buf)
	for child != 0 {
		children = append(children, child)
		cbuf, err := dev.ReadBlock(child)
		if err != nil {
			return nil, err
		}
		child = readTrieNextSibling(cbuf)
	}
	return children, nil
}

// TrieIterator provides a depth-first walk of a directory trie for readdir.
type TrieIterator struct {
	dev          *BlockDevice
	blockSize    uint64
	stack        [256]uint64
	leafEmitted  [256]bool
	sp           int
	pending      bool
	pendingIno   uint64
	pendingType  uint8
	pendingName  string
	dirTrieRoot  uint64
}

// NewTrieIterator creates a new iterator for the given directory.
func NewTrieIterator(dev *BlockDevice, dirTrieRoot uint64) *TrieIterator {
	ti := &TrieIterator{
		dev:         dev,
		blockSize:   dev.BlockSize(),
		dirTrieRoot: dirTrieRoot,
	}
	if dirTrieRoot != 0 {
		ti.stack[0] = dirTrieRoot
		ti.sp = 1
		ti.leafEmitted[0] = false
	}
	return ti
}

// Next returns the next directory entry from the trie.
// Returns (ino, ftype, name, nil) on success.
// Returns ("", , nil) when iteration is complete.
func (ti *TrieIterator) Next() (uint64, uint8, string, error) {
	blockSize := ti.blockSize

	// Return pending entry from previous failed dir_emit (not used in FUSE,
	// but kept for API compatibility)
	if ti.pending {
		ti.pending = false
		return ti.pendingIno, ti.pendingType, ti.pendingName, nil
	}

	for ti.sp > 0 {
		block := ti.stack[ti.sp-1]
		emitted := ti.leafEmitted[ti.sp-1]
		ti.sp--

		buf, err := ti.dev.ReadBlock(block)
		if err != nil {
			continue
		}
		if readTrieMagic(buf) != types.MagicTrieNode {
			continue
		}

		if emitted {
			// Already emitted this node's leaf — push children
			child := readTrieFirstChild(buf)
			var pushed []uint64
			for child != 0 {
				pushed = append(pushed, child)
				cbuf, err := ti.dev.ReadBlock(child)
				if err != nil {
					break
				}
				child = readTrieNextSibling(cbuf)
			}
			// Push in reverse order for alphabetical readdir
			for i := len(pushed) - 1; i >= 0; i-- {
				if ti.sp < 256 {
					ti.stack[ti.sp] = pushed[i]
					ti.leafEmitted[ti.sp] = false
					ti.sp++
				}
			}
			continue
		}

		// Check if this node has leaf data
		nt := readTrieNodeType(buf)
		if (nt&trieNodeStatusLeaf != 0) || (nt != trieNodeTypeInterm) {
			// Has leaf data
			leafName, err := readTrieLeafNameStr(buf, blockSize)
			if err != nil {
				continue
			}
			ino := readTrieInode(buf)
			ftype := readTrieFType(buf)

			// If this node also has children, push it back with leaf_emitted=true
			if readTrieFirstChild(buf) != 0 && ti.sp < 256 {
				ti.stack[ti.sp] = block
				ti.leafEmitted[ti.sp] = true
				ti.sp++
			}

			return ino, ftype, leafName, nil
		}

		// Pure INTERM node — push children
		child := readTrieFirstChild(buf)
		var children []uint64
		for child != 0 {
			children = append(children, child)
			cbuf, err := ti.dev.ReadBlock(child)
			if err != nil {
				break
			}
			child = readTrieNextSibling(cbuf)
		}
		for i := len(children) - 1; i >= 0; i-- {
			if ti.sp < 256 {
				ti.stack[ti.sp] = children[i]
				ti.leafEmitted[ti.sp] = false
				ti.sp++
			}
		}
	}

	return 0, 0, "", nil // iteration complete
}