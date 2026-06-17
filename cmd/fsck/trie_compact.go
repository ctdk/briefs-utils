package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"sort"

	"github.com/ctdk/briefs-utils/types"
)

// compactTrieNode is an in-memory node used while rebuilding a directory trie.
type compactTrieNode struct {
	Depth     uint8
	ByteVal   uint8
	NodeType  uint8
	FType     uint8
	Inode     uint64
	Name      string
	NameOff   uint16
	Parent    *compactTrieNode
	Children  []*compactTrieNode
	Block     uint64
	Slot      uint
}

// compactTriePage is a freshly packed trie page under construction.
type compactTriePage struct {
	Block     uint64
	SlotsUsed uint
	NextSlot  uint
	NameOff   uint16
	Nodes     []*compactTrieNode
}

// compactDirectoryTries rebuilds every directory trie from its collected entries,
// packing nodes and names tightly into fresh pages and freeing any old pages that
// are no longer needed. This covers step 4 of the fsck repair roadmap.
func compactDirectoryTries(fs *fsckState, plan *repairPlan, blockSize uint64) error {
	dataRegionStart := fs.sb.TrieNodePoolStart + fs.sb.TrieNodePoolSize
	allocBlock := func() (uint64, error) {
		rel, err := plan.dataAlloc.AllocateBlock()
		if err != nil {
			return 0, err
		}
		return rel + dataRegionStart, nil
	}

	for _, d := range fs.dirs {
		entries, err := collectDirectoryEntries(fs, d.ino, d.trieRoot, blockSize)
		if err != nil {
			return fmt.Errorf("ino %d: collect directory entries: %w", d.ino, err)
		}

		oldBlocks, err := collectDirectoryTrieBlocks(fs, d.ino, d.trieRoot, blockSize)
		if err != nil {
			return fmt.Errorf("ino %d: collect old trie blocks: %w", d.ino, err)
		}

		root := buildCompactTrie(entries)
		pages, err := packCompactTrie(root, blockSize, allocBlock)
		if err != nil {
			return fmt.Errorf("ino %d: pack trie: %w", d.ino, err)
		}

		if err := writeCompactTriePages(fs.file, pages, blockSize); err != nil {
			return fmt.Errorf("ino %d: write compacted trie: %w", d.ino, err)
		}

		// Update directory inode with new trie root.
		dirIno, ok := plan.inodes[d.ino]
		if !ok {
			orig := fs.inodes[d.ino]
			if orig == nil {
				return fmt.Errorf("ino %d: directory inode missing from fsck state", d.ino)
			}
			clone := *orig
			dirIno = &clone
		}
		dirIno.DirTrieRoot = types.TrieMakeRef(root.Block, root.Slot)
		plan.inodes[d.ino] = dirIno

		// Free old trie blocks.
		for absBlk := range oldBlocks {
			if absBlk >= dataRegionStart && absBlk < fs.sb.JournalOffset {
				plan.dataAlloc.MarkFree(absBlk - dataRegionStart)
			}
		}
	}
	return nil
}

// buildCompactTrie builds a fresh prefix trie from a directory's entries.
func buildCompactTrie(entries []trieEntry) *compactTrieNode {
	root := &compactTrieNode{
		Depth:    0,
		NodeType: types.NodeTypeInterm,
	}
	for _, e := range entries {
		insertCompactTrieEntry(root, e.Name, e.Inode, e.FType)
	}
	return root
}

// insertCompactTrieEntry inserts one directory entry into the compact trie.
func insertCompactTrieEntry(root *compactTrieNode, name string, inode uint64, ftype uint8) {
	cur := root
	nameBytes := []byte(name)
	for i, b := range nameBytes {
		depth := uint8(i + 1)
		var child *compactTrieNode
		for _, c := range cur.Children {
			if c.ByteVal == b {
				child = c
				break
			}
		}
		if child == nil {
			child = &compactTrieNode{
				Depth:    depth,
				ByteVal:  b,
				NodeType: types.NodeTypeInterm,
				Parent:   cur,
			}
			cur.Children = append(cur.Children, child)
		}
		cur = child
	}
	cur.Name = name
	cur.Inode = inode
	cur.FType = ftype
	if cur.NodeType&types.NodeTypeInterm != 0 {
		cur.NodeType |= types.NodeStatusLeaf
	} else {
		cur.NodeType = types.NodeTypeInterm | types.NodeStatusLeaf
	}
}

// packCompactTrie assigns every trie node to a slot in a freshly allocated page,
// linking parents and children via absolute node references.
func packCompactTrie(root *compactTrieNode, blockSize uint64, allocBlock func() (uint64, error)) ([]*compactTriePage, error) {
	var pages []*compactTriePage

	assignNode := func(node *compactTrieNode) error {
		nameLen := uint16(len(node.Name))
		needed := nameLen + 2

		for _, p := range pages {
			if p.NextSlot >= trieSlotCount {
				continue
			}
			slotEnd := uint16(triePageHeaderSize + (p.NextSlot+1)*trieSlotSize)
			// The slot must not overlap with existing names at the page end.
			if slotEnd > uint16(blockSize)-p.NameOff {
				continue
			}
			// For leaf nodes, the name must also fit without overlapping slots.
			if nameLen > 0 && p.NameOff+needed > uint16(blockSize)-slotEnd {
				continue
			}
			if nameLen > 0 {
				node.NameOff = p.NameOff + needed
				p.NameOff += needed
			}
			node.Block = p.Block
			node.Slot = p.NextSlot
			p.NextSlot++
			p.SlotsUsed++
			p.Nodes = append(p.Nodes, node)
			return nil
		}

		// Need a new page.
		block, err := allocBlock()
		if err != nil {
			return fmt.Errorf("allocate trie page: %w", err)
		}
		p := &compactTriePage{
			Block:    block,
			NextSlot: 0,
		}
		slotEnd := uint16(triePageHeaderSize + trieSlotSize)
		// The slot must fit and, for leaves, the name must fit too.
		if slotEnd > uint16(blockSize)-p.NameOff {
			return fmt.Errorf("trie slot does not fit in a fresh page")
		}
		if nameLen > 0 {
			if p.NameOff+needed > uint16(blockSize)-slotEnd {
				return fmt.Errorf("name '%s' (%d bytes) too long for a trie page", node.Name, nameLen)
			}
			node.NameOff = needed
			p.NameOff = needed
		}
		node.Block = block
		node.Slot = 0
		p.NextSlot = 1
		p.SlotsUsed = 1
		p.Nodes = append(p.Nodes, node)
		pages = append(pages, p)
		return nil
	}

	var walk func(node *compactTrieNode) error
	walk = func(node *compactTrieNode) error {
		if err := assignNode(node); err != nil {
			return err
		}
		for _, child := range node.Children {
			if err := walk(child); err != nil {
				return err
			}
		}
		return nil
	}

	if err := walk(root); err != nil {
		return nil, err
	}

	// Sort children by byte value so sibling chains are deterministic.
	var sortChildren func(node *compactTrieNode)
	sortChildren = func(node *compactTrieNode) {
		sort.Slice(node.Children, func(i, j int) bool {
			return node.Children[i].ByteVal < node.Children[j].ByteVal
		})
		for _, c := range node.Children {
			sortChildren(c)
		}
	}
	sortChildren(root)

	return pages, nil
}

// writeCompactTriePages serializes freshly packed trie pages to disk.
func writeCompactTriePages(file *os.File, pages []*compactTriePage, blockSize uint64) error {
	for _, p := range pages {
		buf := make([]byte, blockSize)
		binary.LittleEndian.PutUint32(buf[0:], types.MagicTriePage)
		binary.LittleEndian.PutUint32(buf[4:], types.TriePageVersion)
		binary.LittleEndian.PutUint16(buf[8:], uint16(p.SlotsUsed))
		binary.LittleEndian.PutUint16(buf[10:], p.NameOff)
		freeSlots := ^uint64(0)
		for i := uint(0); i < p.SlotsUsed; i++ {
			freeSlots &^= 1 << i
		}
		binary.LittleEndian.PutUint64(buf[12:], freeSlots)

		for _, node := range p.Nodes {
			off := uint64(triePageHeaderSize + node.Slot*trieSlotSize)

			var firstChild uint64
			if len(node.Children) > 0 {
				c := node.Children[0]
				firstChild = types.TrieMakeRef(c.Block, c.Slot)
			}

			var nextSibling uint64
			if node.Parent != nil {
				for i, sibling := range node.Parent.Children {
					if sibling == node && i+1 < len(node.Parent.Children) {
						next := node.Parent.Children[i+1]
						nextSibling = types.TrieMakeRef(next.Block, next.Slot)
						break
					}
				}
			}

			binary.LittleEndian.PutUint64(buf[off+0:], firstChild)
			binary.LittleEndian.PutUint64(buf[off+8:], nextSibling)
			binary.LittleEndian.PutUint64(buf[off+16:], node.Inode)
			binary.LittleEndian.PutUint16(buf[off+24:], uint16(len(node.Name)))
			binary.LittleEndian.PutUint16(buf[off+26:], node.NameOff)
			buf[off+28] = node.Depth
			buf[off+29] = node.NodeType
			buf[off+30] = node.ByteVal
			buf[off+31] = node.FType
			binary.LittleEndian.PutUint16(buf[off+32:], 0) // flags
			binary.LittleEndian.PutUint16(buf[off+34:], uint16(len(node.Children)))
		}

		for _, node := range p.Nodes {
			if node.NameOff == 0 {
				continue
			}
			nameStart := uint64(blockSize) - uint64(node.NameOff)
			binary.LittleEndian.PutUint16(buf[nameStart:], uint16(len(node.Name)))
			copy(buf[nameStart+2:], node.Name)
		}

		if _, err := file.WriteAt(buf, int64(p.Block*blockSize)); err != nil {
			return fmt.Errorf("write trie page %d: %w", p.Block, err)
		}
	}
	return nil
}
