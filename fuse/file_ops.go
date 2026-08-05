// Package fuse: file data reads and writes.
//
// This ports the kernel's file data path (file.c briefs_read_iter,
// briefs_write_iter, briefs_promote_inline_data, briefs_zero_eof_tail;
// iomap.c briefs_iomap_begin_common; btree.c btree_spill_inline) into Go,
// driving the data allocator and the journal through the per-op block cache.
//
// Extent index storage has three states (mirroring the kernel):
//   - inline-data: file data lives in the inode's 256-byte inline region
//     (InodeFlagInlineData set, no extents, no data blocks).
//   - inline-only: up to 8 extents packed in the inode (ExtentInlineBase == 0,
//     InodeFlagIndexed clear).
//   - tree-backed: extents live in a B+ tree rooted at ExtentInlineBase
//     (InodeFlagIndexed set, NumExtentsInline == 0). The 9th extent spills
//     inline -> tree.
//
// Durability ordering — drain-before-snapshot, the file-write analogue of the
// kernel's briefs_btree_drain + briefs_journal_inode_full.  Replay restores an
// inode verbatim from JRN_INODE_FULL (it trusts extent_inline_base) and only
// RESERVES data/btree-node blocks in the bitmap from JRN_EXTENT_ALLOC — it does
// NOT re-derive btree node contents (unlike trie pages, which replay re-derives
// from JRN_DIR_UPDATE).  So any block whose content is not carried by a journal
// record (data blocks, btree index nodes) must be on disk BEFORE the
// JRN_INODE_FULL snapshot that references it is committed.
//
// Concretely each extent-backed write runs in two phases:
//
//  1. Allocate + write (no journaling yet): per-block read-modify-write, hole
//     allocation, and a full B+ tree rebuild on spill/index change. Data and
//     btree-node blocks are written to the device page cache. On any error
//     here the allocated blocks are returned to the allocator and the cache is
//     aborted — nothing has been journaled, so the on-disk state is unchanged.
//
//  2. Journal + drain + commit: write JRN_EXTENT_ALLOC for every new block and
//     JRN_EXTENT_FREE for the replaced btree nodes, fdatasync (drain the data
//     and btree nodes to disk), write the JRN_INODE_FULL snapshot, commit the
//     journal (journal.Sync), then free the old btree nodes in memory and flush
//     the inode block. The inode block is flushed AFTER the commit because it
//     is snapshot-trusted: replay overwrites it from JRN_INODE_FULL, so a crash
//     before the flush leaves the old on-disk inode, which replay repairs.
//
// Inline-data writes keep their data in the inode block itself, which the
// JRN_INODE_FULL snapshot carries, so they use the simpler commit-before-flush
// order (journal.Sync then flushCache) — the inode block is snapshot-trusted,
// nothing else needs draining.

package fuse

import (
	"os"
	"sort"
	"syscall"

	"github.com/ctdk/briefs-utils/briefs"
)

// inlineDataMax is the inline-data capacity: the 256-byte inode inline region
// (BRIEFS_INODE_INLINE_DATA_SIZE in the kernel). Files at or below this size
// carry their data in the inode.
const inlineDataMax = 256

// readFileData reads up to len(dest) bytes starting at off, mirroring the
// kernel's briefs_read_iter (inline-data bypass + extent walk with zero-filled
// holes). Returns the (possibly short) read buffer, clamped to the file size.
func (b *BrieFS) readFileData(ino uint64, dest []byte, off int64) ([]byte, error) {
	diskInode, err := b.inodes.ReadInode(ino)
	if err != nil {
		return nil, err
	}
	if off >= int64(diskInode.FileSize) {
		return nil, nil
	}
	blockSize := int64(b.blockSize)
	endOff := off + int64(len(dest))
	if endOff > int64(diskInode.FileSize) {
		endOff = int64(diskInode.FileSize)
	}
	readBuf := make([]byte, endOff-off)

	// Inline data is stored directly in the inode.
	if diskInode.Flags&briefs.InodeFlagInlineData != 0 {
		start := off
		if start < 0 {
			start = 0
		}
		end := endOff
		if end > int64(diskInode.FileSize) {
			end = int64(diskInode.FileSize)
		}
		region := diskInode.InlineData()
		n := copy(readBuf, region[start:end])
		return readBuf[:n], nil
	}

	// Walk extents (inline array or B+ tree) in ascending offset order.
	exts, err := collectInodeExtents(b.dev.File(), diskInode, b.blockSize)
	if err != nil {
		return nil, err
	}
	readPos := int64(0)
	for _, ext := range exts {
		extStart := int64(ext.Offset) * blockSize
		extEnd := extStart + int64(ext.Len)*blockSize
		if off >= extEnd || endOff <= extStart {
			continue
		}
		readStart := off
		if readStart < extStart {
			readStart = extStart
		}
		readEnd := endOff
		if readEnd > extEnd {
			readEnd = extEnd
		}

		if ext.Phys == 0 {
			// Hole: zero the overlapping region.
			zeroStart := off
			if zeroStart < extStart {
				zeroStart = extStart
			}
			zeroEnd := endOff
			if zeroEnd > extEnd {
				zeroEnd = extEnd
			}
			bufPos := zeroStart - off
			bufLen := zeroEnd - zeroStart
			if bufPos >= 0 && bufLen > 0 && bufPos < int64(len(readBuf)) {
				for i := bufPos; i < bufPos+bufLen && i < int64(len(readBuf)); i++ {
					readBuf[i] = 0
				}
			}
			continue
		}

		for blkOff := readStart; blkOff < readEnd; blkOff += blockSize {
			absBlock := ext.Phys + uint64((blkOff-extStart)/blockSize)
			buf, err := b.dev.ReadBlock(absBlock)
			if err != nil {
				return nil, err
			}
			blkEnd := blkOff + blockSize
			copyStart := readStart
			if copyStart < blkOff {
				copyStart = blkOff
			}
			copyEnd := readEnd
			if copyEnd > blkEnd {
				copyEnd = blkEnd
			}
			nc := copy(readBuf[readPos:], buf[copyStart-blkOff:copyEnd-blkOff])
			readPos += int64(nc)
		}
	}
	return readBuf[:readPos], nil
}

// collectInodeExtents returns every extent of an inode in ascending offset
// order via briefs.IterateInodeExtents. (Shorthand for the read path, which
// does not need the node-block list.)
func collectInodeExtents(file *os.File, in *briefs.Inode, blockSize uint64) ([]briefs.Extent, error) {
	var exts []briefs.Extent
	err := briefs.IterateInodeExtents(file, in, blockSize, briefs.InodeExtentVisitor{
		VisitExtent: func(ext briefs.Extent) error {
			exts = append(exts, ext)
			return nil
		},
	})
	return exts, err
}

// writeFileData writes data at off, mirroring briefs_write_iter. It selects the
// inline-data path (small files) or the extent-backed path, journals the
// change, and makes it durable. The file-write path takes NO global dir lock:
// it touches only its own inode-table block (via direct RMW under
// inodeBlockLock) plus data/btree blocks exclusive to the inode, so writes to
// files in different inode blocks run concurrently with each other and with
// dir ops. The caller (go-fuse Write handler) need not lock.
func (b *BrieFS) writeFileData(ino uint64, data []byte, off int64) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	if b.readOnly {
		return 0, syscall.EROFS
	}
	// Lock the file's inode-table block for the whole op. The direct inode RMW
	// (writeInodeDirect) and the start-of-op ReadInode both touch this block;
	// same-block writes (siblings in the same 4K table block) serialize here.
	lock := b.inodeBlockLock(ino)
	lock.Lock()
	defer lock.Unlock()

	in, err := b.inodes.ReadInode(ino)
	if err != nil {
		return 0, err
	}
	oldSize := int64(in.FileSize)
	totalSize := off + int64(len(data))

	// Inline-data path: the file is inline (or empty) and the whole write fits
	// in the 256-byte inline region.
	if in.Flags&briefs.InodeFlagInlineData != 0 || oldSize == 0 {
		if totalSize <= inlineDataMax {
			b.writeInlineData(in, data, off, totalSize)
			// Inline data lives in the snapshot-trusted inode block: commit, then
			// write the inode block directly (replay overwrites it if a crash
			// preempts the write).
			if err := b.journalInodeFull(in); err != nil {
				b.failWrite()
				return 0, err
			}
			if err := b.journal.Sync(false); err != nil {
				b.failWrite()
				return 0, err
			}
			if err := b.writeInodeDirect(in); err != nil {
				b.failWrite()
				return 0, err
			}
			if err := b.dev.Fdatasync(); err != nil {
				b.failWrite()
				return 0, err
			}
			return len(data), nil
		}
		// Write exceeds inline capacity: promote to extent-backed first.
		var drain, allocated []uint64
		if err := b.promoteInlineData(in, &drain, &allocated); err != nil {
			b.rollbackAlloc(allocated)
			return 0, err
		}
		n, err := b.writeExtentData(in, data, off, oldSize, &drain, &allocated)
		if err != nil {
			return 0, err // writeExtentData cleaned up (rollback or read-only).
		}
		return n, nil
	}

	var drain, allocated []uint64
	n, err := b.writeExtentData(in, data, off, oldSize, &drain, &allocated)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// writeInlineData copies data into the inode's inline region, sets the inline
// flag, and advances the file size + mtime/ctime on the in-memory inode. It
// does not persist; the caller (writeFileData) writes the inode once at the end
// of the op. The caller must hold the inode's inodeBlockLock.
func (b *BrieFS) writeInlineData(in *briefs.Inode, data []byte, off, totalSize int64) {
	region := in.InlineData()
	copy(region[off:], data)
	in.SetInlineData(region)
	in.Flags |= briefs.InodeFlagInlineData
	if totalSize > int64(in.FileSize) {
		in.FileSize = uint64(totalSize)
	}
	sec, nsec := nowTime()
	in.MtimeSec, in.MtimeNsec = sec, nsec
	in.CtimeSec, in.CtimeNsec = sec, nsec
}

// promoteInlineData converts an inline-data inode to extent-backed, mirroring
// briefs_promote_inline_data (file.c:352). For a non-empty inline file it
// allocates one data block, copies the old inline content into it, and sets a
// single inline extent; for an empty file it just clears the flag. The
// promoted block is written to the page cache and recorded in *drain (drained
// before the commit by writeExtentData). The inode is mutated in memory only;
// the caller persists it once at the end of the op. The caller must hold the
// inode's inodeBlockLock.
func (b *BrieFS) promoteInlineData(in *briefs.Inode, drain, allocated *[]uint64) error {
	oldSize := in.FileSize
	// Capture the old inline content BEFORE clearing the region.
	oldRegion := in.InlineData()
	in.Flags &^= briefs.InodeFlagInlineData
	in.Flags &^= briefs.InodeFlagIndexed
	in.SetInlineData([256]byte{})

	if oldSize == 0 {
		// Empty: no content to carry; the extent write allocates the first block.
		in.NumExtentsInline = 0
		in.NumExtentsTotal = 0
		in.ExtentInlineBase = 0
		return nil
	}

	rel := b.dataAlloc.AllocBlock()
	if rel == 0 {
		return syscall.ENOSPC
	}
	*allocated = append(*allocated, rel)
	abs := b.dataRegionStart + rel

	// Copy the captured old inline content into a zeroed block.
	buf := make([]byte, b.blockSize)
	copy(buf, oldRegion[:oldSize])
	if err := b.dev.WriteBlock(abs, buf); err != nil {
		return err
	}
	*drain = append(*drain, abs)

	var exts [8]briefs.Extent
	exts[0] = briefs.Extent{Offset: 0, Phys: abs, Len: 1, Flags: 0}
	in.SetInlineExtents(exts)
	in.NumExtentsInline = 1
	in.NumExtentsTotal = 1
	in.ExtentInlineBase = 0
	return nil
}

// writeExtentData performs an extent-backed write: per-block RMW for mapped
// blocks, allocation for holes, and a full B+ tree rebuild when the extent list
// changes. It owns the *allocated list for rollback on phase-1 errors and marks
// the FS read-only on phase-2 (post-journal) errors. It uses no per-op cache
// (data, btree nodes, and the inode block are all written directly), so it
// needs only the file's inodeBlockLock, not the global dir lock. The caller
// must hold the inode's inodeBlockLock and pass the in-memory inode @in (mutated
// in place; persisted once at the end).
func (b *BrieFS) writeExtentData(in *briefs.Inode, data []byte, off, oldSize int64, drain, allocated *[]uint64) (int, error) {
	blockSize := int64(b.blockSize)

	// --- Phase 1: allocate + write (no journaling) ---
	exts, oldNodes, err := b.collectExtentsAndNodes(in)
	if err != nil {
		b.rollbackAlloc(*allocated)
		return 0, err
	}

	// Defensive EOF-tail zero (generic/363): a write starting past EOF must zero
	// the tail of the old EOF block so stale bytes do not leak when i_size grows
	// past it. BrieFS zeroes freshly allocated blocks, so this is usually a
	// no-op; it covers blocks written by paths that did not zero the tail.
	if off > oldSize && oldSize > 0 && oldSize%blockSize != 0 {
		if err := b.zeroEofTail(exts, oldSize, drain); err != nil {
			b.rollbackAlloc(*allocated)
			return 0, err
		}
	}

	end := off + int64(len(data))
	rebuildNeeded := false
	pos := int64(0)
	for cur := off; cur < end; {
		iblock := uint64(cur / blockSize)
		blockStart := int64(iblock) * blockSize
		blockEnd := blockStart + blockSize
		segStart := cur
		if segStart < blockStart {
			segStart = blockStart
		}
		segEnd := end
		if segEnd > blockEnd {
			segEnd = blockEnd
		}
		segLen := segEnd - segStart
		segData := data[pos : pos+segLen]

		ext, found := lookupExtent(exts, iblock)
		if found {
			abs := ext.Phys + (iblock - ext.Offset)
			var buf []byte
			if segStart == blockStart && segEnd == blockEnd {
				buf = make([]byte, blockSize) // full-block overwrite: no read needed
			} else {
				buf, err = b.dev.ReadBlock(abs)
				if err != nil {
					b.rollbackAlloc(*allocated)
					return 0, err
				}
			}
			copy(buf[segStart-blockStart:], segData)
			if err := b.dev.WriteBlock(abs, buf); err != nil {
				b.rollbackAlloc(*allocated)
				return 0, err
			}
			*drain = append(*drain, abs)
			// A write into an unwritten extent converts it to written (clear the
			// flag in the working list; the index rebuild persists the change).
			if ext.Flags&briefs.ExtentFlagUnwritten != 0 {
				for i := range exts {
					if exts[i].Offset == ext.Offset && exts[i].Phys == ext.Phys {
						exts[i].Flags &^= briefs.ExtentFlagUnwritten
						break
					}
				}
				rebuildNeeded = true
			}
		} else {
			// Hole: allocate a block, zero it, patch in the written bytes.
			rel := b.dataAlloc.AllocBlock()
			if rel == 0 {
				b.rollbackAlloc(*allocated)
				return 0, syscall.ENOSPC
			}
			*allocated = append(*allocated, rel)
			abs := b.dataRegionStart + rel
			buf := make([]byte, blockSize) // zeroed
			copy(buf[segStart-blockStart:], segData)
			if err := b.dev.WriteBlock(abs, buf); err != nil {
				b.rollbackAlloc(*allocated)
				return 0, err
			}
			*drain = append(*drain, abs)
			exts = insertExtentSorted(exts, briefs.Extent{Offset: iblock, Phys: abs, Len: 1, Flags: 0})
			rebuildNeeded = true
		}
		pos += segLen
		cur = segEnd
	}

	if rebuildNeeded {
		if err := b.rebuildExtentIndex(in, exts, oldNodes, drain, allocated); err != nil {
			b.rollbackAlloc(*allocated)
			return 0, err
		}
	}

	// Update size + mtime/ctime on the in-memory inode (persisted once below).
	if end > oldSize {
		in.FileSize = uint64(end)
	}
	sec, nsec := nowTime()
	in.MtimeSec, in.MtimeNsec = sec, nsec
	in.CtimeSec, in.CtimeNsec = sec, nsec

	// --- Phase 2: journal + drain + commit + free old + write inode ---
	// Journal JRN_EXTENT_ALLOC for every block allocated this op (data + btree
	// nodes) so replay reserves them in the bitmap.
	for _, rel := range *allocated {
		if err := b.journalExtentAlloc(in.InodeNumber, 0, b.dataRegionStart+rel); err != nil {
			b.failWrite()
			return 0, err
		}
	}
	// Drain data + btree nodes to disk BEFORE the snapshot commits (the
	// snapshot trusts the btree root pointer; replay does not re-derive nodes).
	if err := b.dev.Fdatasync(); err != nil {
		b.failWrite()
		return 0, err
	}
	if err := b.journalInodeFull(in); err != nil {
		b.failWrite()
		return 0, err
	}
	// Free replaced btree nodes AFTER the new-root snapshot is journaled. With
	// concurrent file writes sharing the journal, another op's Sync may commit
	// these records; ordering EXTENT_FREE after INODE_FULL means a partial commit
	// can never free old nodes without also committing the new root (which would
	// leave the on-disk inode referencing freed blocks). When the write was a
	// pure RMW (no rebuild) oldNodes is the LIVE tree and must not be freed.
	if rebuildNeeded {
		for _, blk := range oldNodes {
			if err := b.journalExtentFree(in.InodeNumber, blk); err != nil {
				b.failWrite()
				return 0, err
			}
		}
	}
	if err := b.journal.Sync(false); err != nil {
		b.failWrite()
		return 0, err
	}
	// The new root is committed; the replaced old btree nodes are no longer
	// referenced. Free them in memory (their JRN_EXTENT_FREE is committed).
	if rebuildNeeded {
		for _, blk := range oldNodes {
			b.dataAlloc.FreeBlock(blk - b.dataRegionStart)
		}
	}
	// Write the inode block directly (snapshot-trusted; safe after the commit,
	// since replay overwrites it from JRN_INODE_FULL if a crash preempts the
	// write). Under the inodeBlockLock, no other op touches this block.
	if err := b.writeInodeDirect(in); err != nil {
		b.failWrite()
		return 0, err
	}
	if err := b.dev.Fdatasync(); err != nil {
		b.failWrite()
		return 0, err
	}
	return len(data), nil
}

// writeInodeDirect reads the inode-table block, patches the inode's slot with
// the marshaled inode, and writes the whole 4K block back atomically. The
// caller must hold the inode's inodeBlockLock so no other op touches the same
// block concurrently. Used by the file-write path, which bypasses the per-op
// cache so writes to files in different inode blocks run concurrently.
func (b *BrieFS) writeInodeDirect(in *briefs.Inode) error {
	blk, off := b.inodes.inodeLocation(in.InodeNumber)
	buf, err := b.dev.ReadBlock(blk)
	if err != nil {
		return err
	}
	data, err := in.MarshalBinary()
	if err != nil {
		return err
	}
	copy(buf[off:], data)
	return b.dev.WriteBlock(blk, buf)
}

// collectExtentsAndNodes returns the inode's current extents (ascending offset)
// and, for a tree-backed inode, every B+ tree node block it owns (so the rebuild
// can free them). For inline-only/inline-data inodes the node list is empty.
func (b *BrieFS) collectExtentsAndNodes(in *briefs.Inode) (exts []briefs.Extent, nodes []uint64, err error) {
	err = briefs.IterateInodeExtents(b.dev.File(), in, b.blockSize, briefs.InodeExtentVisitor{
		VisitNode: func(block uint64) error {
			nodes = append(nodes, block)
			return nil
		},
		VisitExtent: func(ext briefs.Extent) error {
			exts = append(exts, ext)
			return nil
		},
	})
	return
}

// zeroEofTail zeroes [oldSize, block_end) of the block containing oldSize,
// mirroring briefs_zero_eof_tail (file.c:61). Defensive: BrieFS zeroes freshly
// allocated blocks, so the tail is already zero unless a prior writer left it.
func (b *BrieFS) zeroEofTail(exts []briefs.Extent, oldSize int64, drain *[]uint64) error {
	blockSize := int64(b.blockSize)
	eofBlock := uint64((oldSize - 1) / blockSize)
	ext, found := lookupExtent(exts, eofBlock)
	if !found {
		return nil // nothing mapped at EOF
	}
	abs := ext.Phys + (eofBlock - ext.Offset)
	buf, err := b.dev.ReadBlock(abs)
	if err != nil {
		return err
	}
	tailStart := int(oldSize % blockSize)
	for i := tailStart; i < int(blockSize); i++ {
		buf[i] = 0
	}
	if err := b.dev.WriteBlock(abs, buf); err != nil {
		return err
	}
	*drain = append(*drain, abs)
	return nil
}

// rebuildExtentIndex stores the (possibly changed) extent list back into the
// inode: inline if it fits in 8 extents and the inode is not already
// tree-backed, otherwise a full B+ tree rebuild. The rebuild allocates fresh
// node blocks (the old ones are freed by the caller after the commit), writes
// them to the page cache (recorded in *drain), and records new allocations in
// *allocated for journaling/rollback. Mirrors btree_spill_inline (btree.c:861)
// generalized to a full rebuild on every index change (valid under
// drain-before-snapshot; the incremental insert is deferred).
func (b *BrieFS) rebuildExtentIndex(in *briefs.Inode, exts []briefs.Extent, oldNodes []uint64, drain, allocated *[]uint64) error {
	// Inline-only stays inline if it fits and the inode is not tree-backed.
	if len(exts) <= 8 && in.Flags&briefs.InodeFlagIndexed == 0 {
		var arr [8]briefs.Extent
		copy(arr[:], exts)
		in.SetInlineExtents(arr)
		in.NumExtentsInline = uint32(len(exts))
		in.NumExtentsTotal = uint64(len(exts))
		in.ExtentInlineBase = 0
		in.Flags &^= briefs.InodeFlagIndexed
		return nil
	}

	allocFn := func() (uint64, error) {
		rel := b.dataAlloc.AllocBlock()
		if rel == 0 {
			return 0, syscall.ENOSPC
		}
		*allocated = append(*allocated, rel)
		return b.dataRegionStart + rel, nil
	}

	leafBlocks, leafFirstOffsets, leafBufs, err := briefs.BuildBtreeLeaves(exts, b.blockSize, allocFn)
	if err != nil {
		return err
	}
	root, _, idxBlocks, idxBufs, err := briefs.BuildBtreeIndex(leafBlocks, leafFirstOffsets, b.blockSize, 1, allocFn)
	if err != nil {
		return err
	}

	// Write the new node blocks to the page cache; drained before the commit.
	for i, blk := range leafBlocks {
		if err := b.dev.WriteBlock(blk, leafBufs[i]); err != nil {
			return err
		}
		*drain = append(*drain, blk)
	}
	for i, blk := range idxBlocks {
		if err := b.dev.WriteBlock(blk, idxBufs[i]); err != nil {
			return err
		}
		*drain = append(*drain, blk)
	}

	in.Flags |= briefs.InodeFlagIndexed
	in.ExtentInlineBase = root
	in.NumExtentsInline = 0
	in.NumExtentsTotal = uint64(len(exts))
	return nil
}

// --- extent list helpers ---

// lookupExtent returns the extent covering iblock, or (zero, false) if iblock
// is a hole. exts must be sorted ascending by Offset.
func lookupExtent(exts []briefs.Extent, iblock uint64) (briefs.Extent, bool) {
	lo, hi := 0, len(exts)
	for lo < hi {
		mid := (lo + hi) / 2
		if exts[mid].Offset <= iblock {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo == 0 {
		return briefs.Extent{}, false
	}
	ext := exts[lo-1]
	if iblock < ext.Offset+ext.Len {
		return ext, true
	}
	return briefs.Extent{}, false
}

// insertExtentSorted inserts ext into a sorted-by-offset list, merging with an
// adjacent extent when the two are contiguous in both offset and phys and share
// flags (mirrors btree_inline_insert, btree.c:950). Holes (Phys == 0) are never
// produced by the write path, so exts holds only mapped extents.
func insertExtentSorted(exts []briefs.Extent, ext briefs.Extent) []briefs.Extent {
	i := sort.Search(len(exts), func(i int) bool { return exts[i].Offset >= ext.Offset })

	// Merge with the left neighbor.
	if i > 0 {
		left := &exts[i-1]
		if left.Offset+left.Len == ext.Offset && left.Phys+left.Len == ext.Phys && left.Flags == ext.Flags {
			left.Len += ext.Len
			// The enlarged left may now be contiguous with the right neighbor.
			if i < len(exts) {
				right := &exts[i]
				if left.Offset+left.Len == right.Offset && left.Phys+left.Len == right.Phys && left.Flags == right.Flags {
					left.Len += right.Len
					exts = append(exts[:i], exts[i+1:]...)
				}
			}
			return exts
		}
	}
	// Merge with the right neighbor.
	if i < len(exts) {
		right := &exts[i]
		if ext.Offset+ext.Len == right.Offset && ext.Phys+ext.Len == right.Phys && ext.Flags == right.Flags {
			right.Offset = ext.Offset
			right.Phys = ext.Phys
			right.Len += ext.Len
			return exts
		}
	}
	// No merge: insert a new slot.
	exts = append(exts, briefs.Extent{})
	copy(exts[i+1:], exts[i:])
	exts[i] = ext
	return exts
}

// --- journal + rollback helpers ---

// journalExtentAlloc writes a JRN_EXTENT_ALLOC record for a single block so
// replay reserves it in the bitmap. ExtentIndex is sentinel (replay ignores it).
func (b *BrieFS) journalExtentAlloc(ino, offset, phys uint64) error {
	return b.journal.WriteRecord(briefs.JRN_EXTENT_ALLOC,
		(&briefs.JrnExtentAlloc{Ino: ino, Offset: offset, Length: 1, PhysStart: phys, ExtentIndex: ^uint32(0)}).Marshal())
}

// journalExtentFree writes a JRN_EXTENT_FREE record for a single block so replay
// frees it in the bitmap.
func (b *BrieFS) journalExtentFree(ino, phys uint64) error {
	return b.journal.WriteRecord(briefs.JRN_EXTENT_FREE,
		(&briefs.JrnExtentFree{Ino: ino, Offset: 0, PhysStart: phys, Length: 1}).Marshal())
}

// rollbackAlloc returns a list of data-relative blocks to the allocator. Used
// on phase-1 errors, before any journal record is written.
func (b *BrieFS) rollbackAlloc(allocated []uint64) {
	for _, rel := range allocated {
		b.dataAlloc.FreeBlock(rel)
	}
}

// failWrite marks the filesystem read-only after a post-journal (phase-2)
// error. The journal may hold uncommitted records referencing in-flight
// allocations; refusing further mutations prevents a later Sync from committing
// them against a partially-applied state.
func (b *BrieFS) failWrite() {
	b.readOnly = true
}