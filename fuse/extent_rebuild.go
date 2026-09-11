package fuse

// Localized extent-index rebuild for the write hot path.
//
// rebuildExtentIndex re-emits the ENTIRE extent B+tree on every
// extent-adding op ("the incremental insert is deferred" — its own
// comment).  A fragmented bulk write therefore rewrites every leaf
// block on every FUSE WRITE request: generic/074's fstest builds
// 10-30 MB files in 512 B fragments (~7.7K extents = ~61 leaf blocks
// per file), and the amplified device writes (34.5 GB at a steady
// 45 MB/s for a test whose logical volume is a few GB) push the run
// past any test timeout.  The kernel module never rebuilds — its
// extent chain appends in O(1) — which is why 074 passes there.
//
// The localized variant below keeps the rebuild model (fresh node
// blocks, old ones freed after the commit, everything durable under
// the same drain-before-snapshot ordering) but only re-emits the
// leaves whose contents actually changed:
//
//   - The new extent list is chunked positionally (126 extents per
//     leaf, exactly like BuildBtreeLeaves) and compared against the
//     old tree's per-leaf chunks.  A chunk that is byte-identical to
//     its same-position old leaf may keep that old block: it was
//     allocated and journaled when it was first built, and its
//     content is at least as durable as a freshly written block (the
//     device page cache survives the kill-9 crash model, and any
//     fsync since covered it via the tracked-writeback flush).
//   - Reuse is restricted to an equal-content PREFIX, minus its
//     boundary leaf.  A reused leaf block embeds its next_leaf
//     pointer, so it may only be kept when its successor in the new
//     tree is the same old block it already points at.  The chunk
//     where the equal prefix ends is re-emitted fresh (its content or
//     its successor changed), which re-links the chain.
//   - The index levels are always rebuilt fresh on top of the mixed
//     (reused + fresh) leaf list.  For the fragmented writes this
//     optimization targets, the index is a handful of blocks against
//     tens of leaf blocks.
//   - Only the actually-replaced old leaf blocks plus the old index
//     blocks are returned to the caller to journal (JRN_EXTENT_FREE)
//     and free after the commit.  Reused blocks are neither
//     allocated, journaled, nor freed — they keep the records and
//     bitmap bits from the ops that created them.
//
// Crash safety is unchanged.  An aborted op leaves the old tree fully
// intact (reused blocks were never rewritten, replaced blocks were
// never freed), so the last committed INODE_FULL root still resolves.
// A committed op publishes the new root via INODE_FULL as before;
// replay walks the reused blocks like any other, and the fresh leaf
// and index blocks are covered by the pre-commit data drain exactly as
// the full rebuild's were.

import (
	"slices"
	"syscall"

	"github.com/ctdk/briefs-utils/briefs"
)

// extentLeaf is one leaf block of the on-disk extent tree with its
// extent chunk, as captured by collectExtentTree.
type extentLeaf struct {
	block uint64
	chunk []briefs.Extent
}

// extentTree is the inode's on-disk extent index as walked: every
// extent in ascending offset order, the per-leaf chunks (the reuse
// candidates for the localized rebuild), and the internal node blocks.
// For inline-data and inline-only inodes leaves and idx are empty.
// root and total are the inode fields the tree was walked under; they
// validate cache entries against a freshly-read inode (see
// collectExtentTree).  A cached entry is SHARED read-only: callers that
// mutate the extent list must clone it first (writeExtentData does).
type extentTree struct {
	exts   []briefs.Extent
	leaves []extentLeaf
	idx    []uint64
	root   uint64
	total  uint64
}

// extentTreeCacheMax bounds the walked-tree cache (BrieFS.extentTrees).
// An entry for a heavily fragmented file is the full extent list (24
// bytes per extent; ~720 KB for a 30 MB sparse file), so the cache is
// capped rather than unbounded.  Eviction drops an arbitrary entry —
// the hot file re-walks on its next op, an O(tree) blip, not a
// correctness event.
const extentTreeCacheMax = 64

// lookupExtentTree returns the cached walked tree for @ino, or nil.
func (b *BrieFS) lookupExtentTree(ino uint64) *extentTree {
	b.extentTreesMu.Lock()
	defer b.extentTreesMu.Unlock()
	return b.extentTrees[ino]
}

// storeExtentTree caches @t as @ino's walked tree.  The caller must
// have filled t.root/t.total from the inode the tree describes.
func (b *BrieFS) storeExtentTree(ino uint64, t *extentTree) {
	b.extentTreesMu.Lock()
	defer b.extentTreesMu.Unlock()
	if b.extentTrees == nil {
		b.extentTrees = make(map[uint64]*extentTree)
	}
	if len(b.extentTrees) >= extentTreeCacheMax && b.extentTrees[ino] == nil {
		for k := range b.extentTrees { // arbitrary victim; map order is random
			delete(b.extentTrees, k)
			break
		}
	}
	b.extentTrees[ino] = t
}

// invalidateExtentTree drops @ino's cached walked tree.  Called by every
// full rebuild (the tree it describes is about to be replaced) and by
// paths that change an inode's index state without a localized rebuild.
func (b *BrieFS) invalidateExtentTree(ino uint64) {
	b.extentTreesMu.Lock()
	defer b.extentTreesMu.Unlock()
	delete(b.extentTrees, ino)
}

// collectExtentTree walks @in's extent index with the same structure
// checks collectExtentsAndNodes goes through (WalkBtree behind
// IterateInodeExtents, checksums on), capturing the per-leaf chunks
// the localized rebuild diffs against.  Tree-backed inodes are served
// from the walked-tree cache when the entry validates against @in:
// every rebuild allocates a fresh root block, so (root, total)
// matching the freshly-read inode means the cached tree IS the
// on-disk tree (a mutation by punch/truncate/collapse or this path
// itself always moved the root).
func (b *BrieFS) collectExtentTree(in *briefs.Inode) (*extentTree, error) {
	t := &extentTree{}
	if in.Flags&briefs.InodeFlagInlineData != 0 {
		return t, nil
	}
	if in.Flags&briefs.InodeFlagIndexed == 0 {
		// Inline-only: mirror IterateInodeExtents's array walk.
		inlineExtents := in.InlineExtents()
		maxExtents := in.NumExtentsInline
		if maxExtents > 8 {
			maxExtents = 8
		}
		for ei := uint32(0); ei < maxExtents; ei++ {
			t.exts = append(t.exts, inlineExtents[ei])
		}
		return t, nil
	}
	root := in.ExtentInlineBase
	if root == 0 {
		return t, nil
	}
	if cached := b.lookupExtentTree(in.InodeNumber); cached != nil &&
		cached.root == root && cached.total == in.NumExtentsTotal {
		return cached, nil
	}
	err := briefs.WalkBtree(b.dev.File(), root, briefs.BtreeWalkOptions{
		BlockSize: b.blockSize,
		VerifyCRC: true,
	}, briefs.BtreeNodeVisitor{
		VisitNode: func(info briefs.BtreeNodeInfo) error {
			if !info.Hdr.IsLeaf() {
				t.idx = append(t.idx, info.Block)
			}
			return nil
		},
		VisitLeaf: func(info briefs.BtreeNodeInfo, extents []briefs.Extent) error {
			// WalkBtree allocates a fresh slice per leaf, so the chunk
			// may be kept without copying.
			t.leaves = append(t.leaves, extentLeaf{block: info.Block, chunk: extents})
			t.exts = append(t.exts, extents...)
			return nil
		},
	})
	if err != nil {
		return nil, err
	}
	t.root = root
	t.total = in.NumExtentsTotal
	b.storeExtentTree(in.InodeNumber, t)
	return t, nil
}

// extentBlocksOf returns @in's allocated data-block count (the sum of
// its extents' lengths), the value the FUSE attr fillers report as
// st_blocks.  Tree-backed inodes are served from the walked-tree cache
// like collectExtentTree's own callers: GETATTR/LOOKUP fire on every
// step of a fragmented workload, and a full CRC-verified walk per
// request was the CPU sink that hung generic/074 after the write-path
// amplification was fixed.
func (b *BrieFS) extentBlocksOf(in *briefs.Inode) (uint64, error) {
	tree, err := b.collectExtentTree(in)
	if err != nil {
		return 0, err
	}
	var total uint64
	for _, ext := range tree.exts {
		total += ext.Len
	}
	return total, nil
}

// allNodes returns every node block of the walked tree (leaves and
// index), the free set for the full-rebuild and dismantle cases.
func (t *extentTree) allNodes() []uint64 {
	nodes := make([]uint64, 0, len(t.leaves)+len(t.idx))
	for _, lf := range t.leaves {
		nodes = append(nodes, lf.block)
	}
	return append(nodes, t.idx...)
}

// chunkExtents splits exts into positional chunks of at most
// briefs.BtreeLeafFanout, the exact chunking BuildBtreeLeaves uses.
func chunkExtents(exts []briefs.Extent) [][]briefs.Extent {
	var chunks [][]briefs.Extent
	for start := 0; start < len(exts); start += briefs.BtreeLeafFanout {
		end := start + briefs.BtreeLeafFanout
		if end > len(exts) {
			end = len(exts)
		}
		chunks = append(chunks, exts[start:end])
	}
	return chunks
}

// buildLeafBuf marshals one leaf chunk into a fresh block buffer wired
// to @next (0 terminates the chain), checksummed like BuildBtreeLeaves
// does.
func (b *BrieFS) buildLeafBuf(chunk []briefs.Extent, next uint64) []byte {
	buf := make([]byte, b.blockSize)
	briefs.MarshalBtreeHeader(buf, briefs.BtreeNodeHeader{
		Magic:    briefs.BtreeMagic,
		Flags:    briefs.BtreeFlagLeaf,
		Level:    0,
		NumKeys:  uint16(len(chunk)),
		NextLeaf: next,
	})
	for i, ext := range chunk {
		briefs.PutBtreeLeafExtent(buf, i, ext)
	}
	briefs.SetBtreeNodeChecksum(buf, b.blockSize)
	return buf
}

// rebuildExtentIndexWrite is the write path's extent-index store: the
// localized (leaf-diff) rebuild for tree-backed inodes, falling back to
// the full rebuild for every other shape (no old tree, inline fits,
// truncate to zero).  Returns the blocks the caller must journal as
// JRN_EXTENT_FREE and free after the commit: for a localized rebuild
// the replaced leaf blocks plus the old index blocks; for the fallback
// shapes every old node block.
func (b *BrieFS) rebuildExtentIndexWrite(in *briefs.Inode, tree *extentTree, exts []briefs.Extent, drain, allocated *[]uint64) ([]uint64, error) {
	// The full rebuild's dismantle and inline cases, verbatim.
	if len(exts) == 0 || len(exts) <= 8 && in.Flags&briefs.InodeFlagIndexed == 0 {
		if err := b.rebuildExtentIndex(in, exts, tree.allNodes(), drain, allocated); err != nil {
			return nil, err
		}
		return tree.allNodes(), nil
	}

	// Tree-backed: only when there is an old tree with leaves to diff
	// against.  (An indexed inode whose walk found no leaves is
	// inconsistent — take the full rebuild.)
	if in.Flags&briefs.InodeFlagIndexed == 0 || len(tree.leaves) == 0 {
		if err := b.rebuildExtentIndex(in, exts, tree.allNodes(), drain, allocated); err != nil {
			return nil, err
		}
		return tree.allNodes(), nil
	}

	newChunks := chunkExtents(exts)
	oldLeaves := tree.leaves

	// Longest equal-content chunk prefix.  Reuse drops the boundary
	// chunk of the prefix (its next_leaf, or its content, changed).
	p := 0
	for p < len(newChunks) && p < len(oldLeaves) &&
		slices.Equal(newChunks[p], oldLeaves[p].chunk) {
		p++
	}
	reuseCount := 0
	if p >= 2 {
		reuseCount = p - 1
	}

	// Blocks for the fresh chunk range [reuseCount, len(newChunks)).
	// Allocated up front so next_leaf pointers can be wired.
	leafBlocks := make([]uint64, len(newChunks))
	for i := 0; i < reuseCount; i++ {
		leafBlocks[i] = oldLeaves[i].block
	}
	for i := reuseCount; i < len(newChunks); i++ {
		rel := b.dataAlloc.AllocBlockMeta()
		if rel == 0 {
			return nil, syscall.ENOSPC
		}
		*allocated = append(*allocated, rel)
		leafBlocks[i] = b.dataRegionStart + rel
	}

	// Write the fresh leaves; reused ones keep their blocks as-is.
	for i := reuseCount; i < len(newChunks); i++ {
		var next uint64
		if i+1 < len(newChunks) {
			next = leafBlocks[i+1]
		}
		if err := b.dev.WriteBlock(leafBlocks[i], b.buildLeafBuf(newChunks[i], next)); err != nil {
			return nil, err
		}
		*drain = append(*drain, leafBlocks[i])
	}

	// Index levels: always rebuilt fresh on top of the mixed leaf list.
	firstOffsets := make([]uint64, len(newChunks))
	for i, chunk := range newChunks {
		firstOffsets[i] = chunk[0].Offset
	}
	root, _, idxBlocks, idxBufs, err := briefs.BuildBtreeIndex(leafBlocks, firstOffsets, b.blockSize, 1,
		func() (uint64, error) {
			rel := b.dataAlloc.AllocBlockMeta()
			if rel == 0 {
				return 0, syscall.ENOSPC
			}
			*allocated = append(*allocated, rel)
			return b.dataRegionStart + rel, nil
		})
	if err != nil {
		return nil, err
	}
	for i, blk := range idxBlocks {
		if err := b.dev.WriteBlock(blk, idxBufs[i]); err != nil {
			return nil, err
		}
		*drain = append(*drain, blk)
	}

	in.Flags |= briefs.InodeFlagIndexed
	in.ExtentInlineBase = root
	in.NumExtentsInline = 0
	in.NumExtentsTotal = uint64(len(exts))

	// Cache the new tree: the next write op on this inode validates it
	// against the inode (root/total) and skips the walk.  exts and the
	// chunks alias one backing that no caller mutates (the write path
	// clones before its in-place inserts).
	newTree := &extentTree{exts: exts, idx: idxBlocks, root: root, total: uint64(len(exts))}
	for i, chunk := range newChunks {
		newTree.leaves = append(newTree.leaves, extentLeaf{block: leafBlocks[i], chunk: chunk})
	}
	b.storeExtentTree(in.InodeNumber, newTree)

	// Free set: the replaced old leaves plus every old index block.
	freed := make([]uint64, 0, len(oldLeaves)-reuseCount+len(tree.idx))
	for i := reuseCount; i < len(oldLeaves); i++ {
		freed = append(freed, oldLeaves[i].block)
	}
	return append(freed, tree.idx...), nil
}