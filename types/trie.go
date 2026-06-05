package types

// trie structs and methods

import (
	"encoding/binary"
)

// TrieNode is the on-disk bitwise trie node (32 bytes).
// Matches `struct trie_node` in the kernel module.
type TrieNode struct {
	RangeStart uint64
	RangeLen   uint32
	FreeCount  uint32
	LeftChild  uint64
	RightChild uint64
}

// TrieRoot is the trie root block header.
// Matches `struct briefs_trie_root` in the kernel module.
type TrieRoot struct {
	Magic     uint32 // "TRIE" - 0x54524945
	Version   uint32 // 1
	RootNode  uint64 // block offset of root trie node (relative to trie pool start)
	FreeList  uint64 // next free trie node block
	NodeCount uint32 // total trie nodes in use
	Reserved  [7]uint32
}

// MarshalBinary serializes a TrieNode to its 32-byte on-disk format.
func (tn *TrieNode) MarshalBinary() []byte {
	data := make([]byte, 32)
	binary.LittleEndian.PutUint64(data[0:], tn.RangeStart)
	binary.LittleEndian.PutUint32(data[8:], tn.RangeLen)
	binary.LittleEndian.PutUint32(data[12:], tn.FreeCount)
	binary.LittleEndian.PutUint64(data[16:], tn.LeftChild)
	binary.LittleEndian.PutUint64(data[24:], tn.RightChild)
	return data
}

// MarshalBinary serializes a TrieRoot to its 32-byte on-disk format.
func (tr *TrieRoot) MarshalBinary() []byte {
	data := make([]byte, 32)
	binary.LittleEndian.PutUint32(data[0:], tr.Magic)
	binary.LittleEndian.PutUint32(data[4:], tr.Version)
	binary.LittleEndian.PutUint64(data[8:], tr.RootNode)
	binary.LittleEndian.PutUint64(data[16:], tr.FreeList)
	binary.LittleEndian.PutUint32(data[24:], tr.NodeCount)
	return data
}
