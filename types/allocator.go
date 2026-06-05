package types

// allocator structs and methods. It was up in the air if this should have been
// in types/trie.go, but at least for now it's in its own file.

import ( )

// AllocTreeBuilder builds a bitwise trie for free block tracking.
type AllocTreeBuilder struct {
	// Flat array of trie nodes, indexed by node index.
	Nodes     []TrieNode
	NextIndex uint64
}

// NewAllocTreeBuilder creates a tree builder for the given number of data blocks.
// The tree will be a complete binary trie where leaves represent individual blocks.
func NewAllocTreeBuilder(dataBlockCount uint64) *AllocTreeBuilder {
	padded := nextPowerOf2(dataBlockCount)
	// Total nodes in a complete binary trie covering `padded` leaf blocks:
	// root (covers `padded`), 2 children (cover `padded/2`), 4 (cover `padded/4`), ..., padded leaves
	// = 1 + 2 + 4 + ... + padded = 2 * padded - 1
	totalNodes := 2*padded - 1
	if totalNodes < 1 {
		totalNodes = 1
	}
	return &AllocTreeBuilder{
		Nodes:     make([]TrieNode, totalNodes),
		NextIndex: 0,
	}
}

// Build builds the trie. Returns a map of block-number-in-pool -> node data.
// The returned trieNodeBlocks includes only the node data blocks (no header block).
func (tb *AllocTreeBuilder) Build(dataBlockCount uint64) []TrieNode {
	padded := nextPowerOf2(dataBlockCount)
	tb.buildNode(0, padded, dataBlockCount)
	return tb.Nodes
}

// buildNode recursively builds the trie starting at the current NextIndex.
func (tb *AllocTreeBuilder) buildNode(rangeStart uint64, paddedRangeLen uint64, maxValid uint64) uint64 {
	idx := tb.NextIndex
	tb.NextIndex++

	node := &tb.Nodes[idx]
	node.RangeStart = rangeStart
	node.RangeLen = uint32(paddedRangeLen)

	if paddedRangeLen == 1 {
		// Leaf node: free if this block is within the valid data region
		if rangeStart < maxValid {
			node.FreeCount = 1
		} else {
			node.FreeCount = 0
		}
		return idx
	}

	// Internal node: recurse into children
	half := paddedRangeLen / 2

	// Left child covers [rangeStart, rangeStart + half)
	leftIdx := tb.buildNode(rangeStart, half, maxValid)
	node.LeftChild = leftIdx

	// Right child covers [rangeStart + half, rangeStart + paddedRangeLen)
	rightIdx := tb.buildNode(rangeStart+half, half, maxValid)
	node.RightChild = rightIdx

	// Free count = sum of children
	node.FreeCount = tb.Nodes[leftIdx].FreeCount + tb.Nodes[rightIdx].FreeCount

	return idx
}

// NbBlocks returns the number of 4096-byte blocks needed for all trie nodes.
// Each block holds 128 trie nodes (4096 / 32 = 128).
func (tb *AllocTreeBuilder) NbBlocks() uint64 {
	nodesPerBlock := uint64(4096 / 32)
	nodeCount := uint64(len(tb.Nodes))
	if nodeCount == 0 {
		return 0
	}
	return (nodeCount + nodesPerBlock - 1) / nodesPerBlock
}

// recountInternal recalculates the free_count for an internal node from its children.
func (tb *AllocTreeBuilder) recountInternal(nodeIdx uint64) {
	node := &tb.Nodes[nodeIdx]
	if node.RangeLen == 1 {
		return
	}
	node.FreeCount = tb.Nodes[node.LeftChild].FreeCount + tb.Nodes[node.RightChild].FreeCount
}

// MarkRangeAllocated marks a range of blocks (data-relative) as allocated in the trie.
func (tb *AllocTreeBuilder) MarkRangeAllocated(offset, count uint64) {
	if count == 0 {
		return
	}
	tb.markAllocRec(0, offset, count)
}

func (tb *AllocTreeBuilder) markAllocRec(nodeIdx, offset, count uint64) {
	if count == 0 {
		return
	}
	node := &tb.Nodes[nodeIdx]
	if node.RangeLen == 1 {
		// Leaf: mark allocated
		node.FreeCount = 0
		return
	}
	half := uint64(node.RangeLen) / 2
	if offset+count <= half {
		// Entirely in left child
		tb.markAllocRec(node.LeftChild, offset, count)
	} else if offset >= half {
		// Entirely in right child
		tb.markAllocRec(node.RightChild, offset-half, count)
	} else {
		// Spans both children
		leftCount := half - offset
		rightCount := count - leftCount
		tb.markAllocRec(node.LeftChild, offset, leftCount)
		tb.markAllocRec(node.RightChild, 0, rightCount)
	}
	tb.recountInternal(nodeIdx)
}

// WriteNodes packs the trie nodes into blocks. Returns one []byte per block.
// The caller can write these blocks starting at (poolStartBlock + 1) since
// block 0 of the pool is the trie root header.
func (tb *AllocTreeBuilder) WriteNodes() [][]byte {
	nb := tb.NbBlocks()
	if nb == 0 {
		return nil
	}
	blocks := make([][]byte, nb)
	nodesPerBlock := uint64(4096 / 32)
	for bi := uint64(0); bi < nb; bi++ {
		buf := make([]byte, 4096)
		start := bi * nodesPerBlock
		end := start + nodesPerBlock
		if end > uint64(len(tb.Nodes)) {
			end = uint64(len(tb.Nodes))
		}
		for i := start; i < end; i++ {
			nodeData := tb.Nodes[i].MarshalBinary()
			copy(buf[(i-start)*32:], nodeData)
		}
		blocks[bi] = buf
	}
	return blocks
}
