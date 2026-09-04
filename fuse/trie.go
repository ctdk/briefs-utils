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

// trieScanSiblings walks a parent's child chain looking for the child with
// byte_val == byteVal, returning that child and its previous sibling (0 if
// it is the first child). The walk is capped at briefs.TrieSiblingMax hops:
// a next_sibling back-edge (corrupt/stale trie) errors out instead of
// spinning forever, mirroring kernel trie.c's briefs_trie_find_child and
// trie_find_child_with_prev. readNode fetches and parses one child slot;
// the uncached (device) and cached (BrieFS) sibling scans share this core
// through it.
func trieScanSiblings(parentRef, firstChild uint64, byteVal uint8, readNode func(ref uint64) (*briefs.TrieSlot, error)) (child, prev uint64, err error) {
	cur := firstChild
	for hops := 0; !briefs.TrieRefIsNull(cur); hops++ {
		if hops >= briefs.TrieSiblingMax {
			return 0, 0, fmt.Errorf("trie sibling walk exceeded %d nodes from parent ref %d (corrupt/cyclic trie)",
				briefs.TrieSiblingMax, parentRef)
		}
		cnode, rerr := readNode(cur)
		if rerr != nil {
			return 0, 0, rerr
		}
		if cnode.ByteVal == byteVal {
			return cur, prev, nil
		}
		prev = cur
		cur = cnode.NextSibling
	}
	return 0, 0, nil
}

// TrieFindChild finds a child node by byte value in the sibling chain.
func TrieFindChild(dev *BlockDevice, parentRef uint64, byteVal byte) (uint64, error) {
	_, pnode, err := trieReadNode(dev, parentRef)
	if err != nil {
		return 0, err
	}
	child, _, err := trieScanSiblings(parentRef, pnode.FirstChild, byteVal, func(ref uint64) (*briefs.TrieSlot, error) {
		_, cnode, rerr := trieReadNode(dev, ref)
		return cnode, rerr
	})
	return child, err
}

// TrieIterator provides a depth-first walk of a directory trie for
// readdir, delegating the traversal mechanics (dynamic visit stack,
// visited-node cycle check, leaf re-visit for a leaf that also branches,
// and the briefs.TrieSiblingMax cap on sibling chains) to
// briefs.TrieWalker, which fsck's walks share as well.  Problems are
// skipped silently: a FUSE readdir must keep serving whatever part of the
// trie is readable (fsck reports the same conditions through its Note
// hook instead).
type TrieIterator struct {
	w *briefs.TrieWalker
}

// NewTrieIterator creates a new iterator for the given directory.
func NewTrieIterator(dev *BlockDevice, dirTrieRoot uint64) *TrieIterator {
	return &TrieIterator{w: briefs.NewTrieWalker(dev.ReadBlock, dirTrieRoot)}
}

// Next returns the next directory entry from the trie, or a zero inode
// when the walk is finished.  A leaf whose name cannot be read is skipped
// but its subtree is still walked.
func (ti *TrieIterator) Next() (uint64, uint8, string, error) {
	for {
		_, emitted, buf, _, node, ok := ti.w.Next()
		if !ok {
			return 0, 0, "", nil
		}
		if emitted || !briefs.TrieIsLeaf(node.NodeType) {
			continue
		}
		name, err := briefs.ReadTrieName(buf, node.NameLen, node.NameOffset)
		if err != nil {
			continue
		}
		return node.Inode, node.FType, name, nil
	}
}
