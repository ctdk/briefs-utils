package briefs

import (
	"fmt"
	"os"
)

// B-tree construction helpers (Phase 4 rebuild-from-extents). They are the
// inverse of the readers in btree.go: pack extents into leaves (126/leaf), build
// internal idx levels bottom-up (up to 254 children per node), checksum each
// node, and write them out. Holes (Phys == 0) are preserved verbatim.
//
// alloc is a caller-supplied block allocator returning ABSOLUTE block numbers
// (the fsck caller wraps plan.dataAlloc.AllocateBlock()+dataRegionStart so the
// briefs package stays free of fsck-specific geometry). AllocateBlock already
// marks the block allocated in the builder, so callers need not re-mark new
// blocks; they only MarkFree the old node blocks being replaced.

// BuildBtreeLeaves packs extents into leaf nodes (BtreeLeafFanout per leaf),
// allocating one block per leaf via alloc. Leaves are wired into a next_leaf
// chain left-to-right (last leaf's next_leaf = 0). Returns the absolute leaf
// block numbers, each leaf's first extent offset (the separator keys for the
// index level), and the checksummed leaf buffers.
//
// extents must be sorted ascending by Offset with no duplicate offsets. Hole
// extents are packed like any other.
func BuildBtreeLeaves(extents []Extent, blockSize uint64, alloc func() (uint64, error)) (leafBlocks []uint64, leafFirstOffsets []uint64, bufs [][]byte, err error) {
	if len(extents) == 0 {
		return nil, nil, nil, fmt.Errorf("BuildBtreeLeaves: no extents")
	}
	nLeaves := (len(extents) + BtreeLeafFanout - 1) / BtreeLeafFanout

	// Allocate all leaf blocks up front so the next_leaf chain can be wired.
	leafBlocks = make([]uint64, nLeaves)
	for i := 0; i < nLeaves; i++ {
		b, e := alloc()
		if e != nil {
			return leafBlocks[:i], nil, nil, fmt.Errorf("alloc leaf %d: %w", i, e)
		}
		leafBlocks[i] = b
	}

	bufs = make([][]byte, nLeaves)
	leafFirstOffsets = make([]uint64, nLeaves)
	for li := 0; li < nLeaves; li++ {
		start := li * BtreeLeafFanout
		end := start + BtreeLeafFanout
		if end > len(extents) {
			end = len(extents)
		}
		chunk := extents[start:end]
		buf := make([]byte, blockSize)
		var next uint64
		if li+1 < nLeaves {
			next = leafBlocks[li+1]
		}
		MarshalBtreeHeader(buf, BtreeNodeHeader{
			Magic:    BtreeMagic,
			Flags:    BtreeFlagLeaf,
			Level:    0,
			NumKeys:  uint16(len(chunk)),
			NextLeaf: next,
		})
		for i, ext := range chunk {
			PutBtreeLeafExtent(buf, i, ext)
		}
		SetBtreeNodeChecksum(buf, blockSize)
		bufs[li] = buf
		leafFirstOffsets[li] = chunk[0].Offset
	}
	return leafBlocks, leafFirstOffsets, bufs, nil
}

// BuildBtreeIndex builds the internal-node levels bottom-up over the given
// children. childMinKeys must give, for each child, the minimum extent offset in
// that child's subtree (for a leaf, its first extent offset; for an idx node, the
// minKey of its leftmost descendant leaf — which is the first child's minKey
// carried up). level is the level to assign to the new idx nodes (1 for nodes
// directly above leaves). alloc returns absolute block numbers.
//
// Each idx node holds up to BtreeIdxFanout separators (BtreeIdxFanout+1
// children). Children are distributed evenly across the nodes of each level
// (never producing a zero-key node). Returns the root block, the root's minKey,
// the absolute idx block numbers, and the checksummed idx buffers.
//
// A single child needs no idx node: it becomes the root at its existing level.
func BuildBtreeIndex(childBlocks, childMinKeys []uint64, blockSize uint64, level uint16, alloc func() (uint64, error)) (rootBlock, rootMinKey uint64, idxBlocks []uint64, bufs [][]byte, err error) {
	if len(childBlocks) != len(childMinKeys) {
		return 0, 0, nil, nil, fmt.Errorf("BuildBtreeIndex: childBlocks/childMinKeys length mismatch")
	}
	if len(childBlocks) == 0 {
		return 0, 0, nil, nil, fmt.Errorf("BuildBtreeIndex: no children")
	}
	if len(childBlocks) == 1 {
		return childBlocks[0], childMinKeys[0], nil, nil, nil
	}

	const maxChildren = BtreeIdxFanout + 1 // 254
	curBlocks := childBlocks
	curKeys := childMinKeys
	for {
		if len(curBlocks) == 1 {
			rootBlock = curBlocks[0]
			rootMinKey = curKeys[0]
			return rootBlock, rootMinKey, idxBlocks, bufs, nil
		}
		// Distribute curBlocks evenly across nNodes idx nodes (each <= maxChildren,
		// each holding at least 2 children so no zero-key node is produced).
		nNodes := (len(curBlocks) + maxChildren - 1) / maxChildren
		base := len(curBlocks) / nNodes
		rem := len(curBlocks) % nNodes
		nextBlocks := make([]uint64, 0, nNodes)
		nextKeys := make([]uint64, 0, nNodes)
		off := 0
		for n := 0; n < nNodes; n++ {
			k := base
			if n < rem {
				k++
			}
			bucket := curBlocks[off : off+k]
			bucketKeys := curKeys[off : off+k]
			off += k

			block, e := alloc()
			if e != nil {
				return 0, 0, idxBlocks, bufs, fmt.Errorf("alloc idx node: %w", e)
			}
			buf := make([]byte, blockSize)
			MarshalBtreeHeader(buf, BtreeNodeHeader{
				Magic:   BtreeMagic,
				Flags:   0, // internal
				Level:   level,
				NumKeys: uint16(k - 1),
			})
			for i := 0; i < k-1; i++ {
				// idx[i].child = bucket[i]; its high_key is the minKey of the next child.
				PutBtreeIdxEntry(buf, i, BtreeIdxEntry{Child: bucket[i], HighKey: bucketKeys[i+1]})
			}
			SetBtreeTrailingChild(buf, bucket[k-1])
			SetBtreeNodeChecksum(buf, blockSize)
			bufs = append(bufs, buf)
			idxBlocks = append(idxBlocks, block)
			nextBlocks = append(nextBlocks, block)
			nextKeys = append(nextKeys, bucketKeys[0]) // this node's minKey = first child's minKey
		}
		curBlocks = nextBlocks
		curKeys = nextKeys
		level++
	}
}

// WriteBtreeNodes writes each buffer to its corresponding absolute block.
func WriteBtreeNodes(file *os.File, blocks []uint64, bufs [][]byte, blockSize uint64) error {
	if len(blocks) != len(bufs) {
		return fmt.Errorf("WriteBtreeNodes: %d blocks vs %d buffers", len(blocks), len(bufs))
	}
	for i, block := range blocks {
		if err := WriteBtreeNode(file, block, blockSize, bufs[i]); err != nil {
			return fmt.Errorf("write btree node %d: %w", block, err)
		}
	}
	return nil
}