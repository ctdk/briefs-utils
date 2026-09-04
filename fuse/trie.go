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
// The walk is capped at briefs.TrieSiblingMax hops: a back-edge in
// next_sibling (corrupt/stale trie) aborts with an error instead of
// spinning forever (kernel trie.c briefs_trie_find_child).
func TrieFindChild(dev *BlockDevice, parentRef uint64, byteVal byte) (uint64, error) {
	_, pnode, err := trieReadNode(dev, parentRef)
	if err != nil {
		return 0, err
	}

	child := pnode.FirstChild
	visited := 0
	for !briefs.TrieRefIsNull(child) {
		visited++
		if visited > briefs.TrieSiblingMax {
			return 0, fmt.Errorf("trie sibling walk exceeded %d nodes from parent ref %d (corrupt/cyclic trie)",
				briefs.TrieSiblingMax, parentRef)
		}
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

// TrieIterator provides a depth-first walk of a directory trie for readdir.
//
// The walk stack is dynamic: a trie whose names approach the 255-byte
// limit pushes up to 256 siblings per level, which overflows any fixed
// array and used to silently drop entries from readdir.  The walk also
// caps every sibling chain at briefs.TrieSiblingMax and tracks visited
// nodes (fsck's cycle semantics: a node may legitimately reappear once,
// re-pushed with leafEmitted=true to emit its children), so a
// first_child/next_sibling back-edge in a stale trie ends the walk
// instead of spinning forever (kernel trie.c cap comments).
type TrieIterator struct {
	dev         *BlockDevice
	blockSize   uint64
	stack       []uint64
	leafEmitted []bool
	visited     map[uint64]bool
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
		visited:     make(map[uint64]bool),
	}
	if !briefs.TrieRefIsNull(dirTrieRoot) {
		ti.stack = append(ti.stack, dirTrieRoot)
		ti.leafEmitted = append(ti.leafEmitted, false)
	}
	return ti
}

// pop removes and returns the top of the walk stack.
func (ti *TrieIterator) pop() (ref uint64, emitted bool, ok bool) {
	if len(ti.stack) == 0 {
		return 0, false, false
	}
	n := len(ti.stack) - 1
	ref = ti.stack[n]
	emitted = ti.leafEmitted[n]
	ti.stack = ti.stack[:n]
	ti.leafEmitted = ti.leafEmitted[:n]
	return ref, emitted, true
}

// push pushes refs onto the walk stack in reverse so the first ref pops
// first.
func (ti *TrieIterator) push(refs []uint64, emitted bool) {
	for i := len(refs) - 1; i >= 0; i-- {
		ti.stack = append(ti.stack, refs[i])
		ti.leafEmitted = append(ti.leafEmitted, emitted)
	}
}

// collectChildren gathers a node's child refs in sibling-chain order. The
// walk is capped at briefs.TrieSiblingMax; on a corrupt/cyclic chain the
// refs gathered so far are returned.
func (ti *TrieIterator) collectChildren(node *briefs.TrieSlot) []uint64 {
	var children []uint64
	child := node.FirstChild
	visited := 0
	for !briefs.TrieRefIsNull(child) {
		visited++
		if visited > briefs.TrieSiblingMax {
			return children
		}
		children = append(children, child)
		_, cnode, err := trieReadNode(ti.dev, child)
		if err != nil {
			break
		}
		child = cnode.NextSibling
	}
	return children
}

// Next returns the next directory entry from the trie.
func (ti *TrieIterator) Next() (uint64, uint8, string, error) {
	if ti.pending {
		ti.pending = false
		return ti.pendingIno, ti.pendingType, ti.pendingName, nil
	}

	for {
		ref, emitted, ok := ti.pop()
		if !ok {
			break
		}

		if ti.visited[ref] && !emitted {
			// first_child back-edge: skip it and drain the rest of the
			// stack (fsck's cycle semantics).
			continue
		}
		if !emitted {
			ti.visited[ref] = true
		}

		buf, node, err := trieReadNode(ti.dev, ref)
		if err != nil {
			continue
		}

		if emitted {
			ti.push(ti.collectChildren(node), false)
			continue
		}

		if briefs.TrieIsLeaf(node.NodeType) {
			leafName, err := briefs.ReadTrieName(buf, node.NameLen, node.NameOffset)
			if err != nil {
				continue
			}
			ino := node.Inode
			ftype := node.FType

			if !briefs.TrieRefIsNull(node.FirstChild) {
				ti.push([]uint64{ref}, true)
			}

			return ino, ftype, leafName, nil
		}

		// Pure INTERM node: push children.
		ti.push(ti.collectChildren(node), false)
	}

	return 0, 0, "", nil
}
