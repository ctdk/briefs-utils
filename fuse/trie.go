// Package fuse implements a FUSE filesystem for BrieFS.
package fuse

import (
	"fmt"

	"github.com/ctdk/briefs-utils/briefs"
)

// TrieLookup finds an entry by name in a directory trie.
func TrieLookup(dev *BlockDevice, dirTrieRoot uint64, name string) (ino uint64, ftype uint8, err error) {
	if briefs.TrieRefIsNull(dirTrieRoot) {
		return 0, 0, fmt.Errorf("no trie root")
	}

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
			if briefs.TrieRefIsNull(child) {
				return 0, 0, fmt.Errorf("not found")
			}

			cbuf, node, err := trieReadNode(dev, child)
			if err != nil {
				return 0, 0, err
			}

			if !briefs.TrieIsLeaf(node.NodeType) {
				return 0, 0, fmt.Errorf("not found")
			}

			leafName, err := briefs.ReadTrieName(cbuf, node.NameLen, node.NameOffset)
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
		if briefs.TrieRefIsNull(child) {
			return 0, 0, fmt.Errorf("not found")
		}

		cbuf, node, err := trieReadNode(dev, child)
		if err != nil {
			return 0, 0, err
		}

		if node.NodeType&briefs.NodeTypeInterm == 0 {
			// Pure leaf where we need an INTERM: check full name.
			leafName, err := briefs.ReadTrieName(cbuf, node.NameLen, node.NameOffset)
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
func trieReadNode(dev *BlockDevice, nodeRef uint64) ([]byte, *briefs.TrieSlot, error) {
	block := briefs.TrieRefBlock(nodeRef)
	slot := briefs.TrieRefSlot(nodeRef)
	buf, err := dev.ReadBlock(block)
	if err != nil {
		return nil, nil, fmt.Errorf("read trie page %d: %w", block, err)
	}
	if _, err := briefs.ReadTriePage(buf); err != nil {
		return nil, nil, fmt.Errorf("parse trie page %d: %w", block, err)
	}
	node, err := briefs.ReadTrieSlot(buf, slot)
	if err != nil {
		return nil, nil, fmt.Errorf("read trie slot %d/%d: %w", block, slot, err)
	}
	return buf, node, nil
}

// TrieFindChild finds a child node by byte value in the sibling chain.
func TrieFindChild(dev *BlockDevice, parentRef uint64, byteVal byte) (uint64, error) {
	_, pnode, err := trieReadNode(dev, parentRef)
	if err != nil {
		return 0, err
	}

	child := pnode.FirstChild
	for !briefs.TrieRefIsNull(child) {
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

	return 0, nil
}

// TrieGetChildren returns all children of a trie node.
func TrieGetChildren(dev *BlockDevice, parentRef uint64) ([]uint64, error) {
	_, pnode, err := trieReadNode(dev, parentRef)
	if err != nil {
		return nil, err
	}

	var children []uint64
	child := pnode.FirstChild
	for !briefs.TrieRefIsNull(child) {
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
	if !briefs.TrieRefIsNull(dirTrieRoot) {
		ti.stack[0] = dirTrieRoot
		ti.sp = 1
		ti.leafEmitted[0] = false
	}
	return ti
}

// Next returns the next directory entry from the trie.
func (ti *TrieIterator) Next() (uint64, uint8, string, error) {
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
			for !briefs.TrieRefIsNull(child) {
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

		if briefs.TrieIsLeaf(node.NodeType) {
			leafName, err := briefs.ReadTrieName(buf, node.NameLen, node.NameOffset)
			if err != nil {
				continue
			}
			ino := node.Inode
			ftype := node.FType

			if !briefs.TrieRefIsNull(node.FirstChild) && ti.sp < 256 {
				ti.stack[ti.sp] = ref
				ti.leafEmitted[ti.sp] = true
				ti.sp++
			}

			return ino, ftype, leafName, nil
		}

		// Pure INTERM node: push children.
		child := node.FirstChild
		var children []uint64
		for !briefs.TrieRefIsNull(child) {
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