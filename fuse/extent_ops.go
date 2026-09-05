// Package fuse: fallocate, truncate (setattr size), and setattr metadata.
//
// Ports briefs_fallocate (file.c:3459) with all five modes (preallocate,
// PUNCH_HOLE, ZERO_RANGE, COLLAPSE_RANGE, INSERT_RANGE), the truncate paths of
// briefs_setattr (file.c:836), and the killpriv (file_remove_privs) that
// strips suid/sgid and clears security.capability on file-modifying ops. The
// extent-list changes (punch hole, truncate down, preallocate, collapse,
// insert, zero-range conversion) reuse the Phase-5 infrastructure: collect the
// extents + btree nodes, mutate the sorted extent list, rebuild the index, and
// commit via commitExtentChange.

package fuse

import (
	"syscall"

	"github.com/ctdk/briefs-utils/briefs"
)

// fallocate flags (uapi/linux/fallocate.h).
const (
	fallocKeepSize      uint32 = 0x01
	fallocPunchHole     uint32 = 0x02
	fallocCollapseRange uint32 = 0x08
	fallocZeroRange     uint32 = 0x10
	fallocInsertRange   uint32 = 0x20
)

// S_ISUID / S_ISGID (mode bits stripped by killpriv).
const (
	s_ISUID uint32 = 0o4000
	s_ISGID uint32 = 0o2000
)

// fattr* bits mirror fuse.FATTR_* (the VFS setattr valid mask).
const (
	fattrMode     uint32 = 1 << 0
	fattrUID      uint32 = 1 << 1
	fattrGID      uint32 = 1 << 2
	fattrSize     uint32 = 1 << 3
	fattrATime    uint32 = 1 << 4
	fattrMTime    uint32 = 1 << 5
	fattrATimeNow uint32 = 1 << 7
	fattrMTimeNow uint32 = 1 << 8
	fattrCTime    uint32 = 1 << 10
)

// fuseSetAttrIn is the parsed VFS setattr request (go-fuse's SetAttrIn mapped to
// a plain struct so extent_ops.go need not import go-fuse).
type fuseSetAttrIn struct {
	valid     uint32
	size      uint64
	mode      uint32
	uid       uint32
	gid       uint32
	atime     uint64
	mtime     uint64
	ctime     uint64
	atimensec uint32
	mtimensec uint32
	ctimensec uint32
}

// fallocateOp mirrors briefs_fallocate (file.c:3459): plain preallocation
// (optionally KEEP_SIZE) allocates unwritten extents; PUNCH_HOLE frees the
// blocks in the range and leaves a hole; ZERO_RANGE zeroes the range and
// converts its block-aligned middle to unwritten; COLLAPSE_RANGE removes the
// range and shifts the tail down; INSERT_RANGE opens a hole and shifts the
// tail up.
func (b *BrieFS) fallocateOp(ino uint64, off, size uint64, mode uint32) error {
	if b.readOnly {
		return syscall.EROFS
	}
	if mode&^(fallocKeepSize|fallocPunchHole|fallocCollapseRange|fallocZeroRange|fallocInsertRange) != 0 {
		return syscall.EOPNOTSUPP
	}
	if mode&fallocPunchHole != 0 && mode&fallocKeepSize == 0 {
		return syscall.EINVAL
	}
	if size == 0 {
		return syscall.EINVAL
	}
	// The bridge's s_maxbytes stand-in (the kernel uses MAX_LFS_FILESIZE):
	// no file on this image can exceed the device. Overflow-safe so a
	// userspace negative offset (huge uint64) cannot wrap past the check.
	maxBytes := b.sb.TotalBlocks * b.blockSize
	if off > maxBytes || size > maxBytes-off {
		return syscall.EFBIG
	}

	lock := b.inodeBlockLock(ino)
	lock.Lock()
	defer lock.Unlock()

	in, err := b.inodes.ReadInode(ino)
	if err != nil {
		return err
	}
	if userFlagsImmutable(in) {
		return syscall.EPERM
	}
	// killpriv: strip suid/sgid + clear security.capability (generic/683/688).
	if err := b.removePrivs(in); err != nil {
		return err
	}

	// Dispatch in the kernel's order (file.c:3543-3559).
	switch {
	case mode&fallocPunchHole != 0:
		return b.punchHole(in, off, size)
	case mode&fallocCollapseRange != 0:
		return b.collapseRangeOp(in, off, size)
	case mode&fallocInsertRange != 0:
		return b.insertRangeOp(in, off, size)
	case mode&fallocZeroRange != 0:
		return b.zeroRangeOp(in, off, size, mode)
	}

	// Preallocate: allocate unwritten extents for [off, off+size). Promote
	// inline data first if the range exceeds the inline region; the promoted
	// block joins preallocate's rollback/journal list so a later failure
	// frees it and the commit journals a JRN_EXTENT_ALLOC for it.
	end := off + size
	var allocated []uint64
	if in.Flags&briefs.InodeFlagInlineData != 0 && end > inlineDataMax {
		var drain []uint64
		if err := b.promoteInlineData(in, &drain, &allocated); err != nil {
			b.rollbackAlloc(allocated)
			return err
		}
	}

	return b.preallocate(in, off, end, mode, allocated)
}

// allocUnwrittenHole allocates unwritten extent(s) covering the hole blocks
// [startBlk, endBlk), preferring one contiguous run (Allocator.AllocBlocks, one
// bitmap pass) and falling back to per-block allocation when the bitmap is too
// fragmented for a run — the kernel's briefs_zero_alloc_hole pattern
// (file.c:2797), which replaced per-block hole allocation in 0fd1448.
// Allocated data-relative blocks are appended to *allocated (the caller's
// rollback list); on ENOSPC the partial allocations are left in *allocated for
// the caller to roll back. The returned extents are unwritten and sorted; the
// caller inserts them via insertExtentSorted (which merges adjacent
// phys-contiguous runs).
func (b *BrieFS) allocUnwrittenHole(startBlk, endBlk uint64, allocated *[]uint64) ([]briefs.Extent, error) {
	seg := endBlk - startBlk
	if rel := b.dataAlloc.AllocBlocks(seg); rel != 0 {
		for i := uint64(0); i < seg; i++ {
			*allocated = append(*allocated, rel+i)
		}
		return []briefs.Extent{{
			Offset: startBlk,
			Phys:   b.dataRegionStart + rel,
			Len:    seg,
			Flags:  briefs.ExtentFlagUnwritten,
		}}, nil
	}

	// No contiguous run of seg fit: per-block fallback.
	var exts []briefs.Extent
	for blk := startBlk; blk < endBlk; blk++ {
		rel := b.dataAlloc.AllocBlock()
		if rel == 0 {
			return nil, syscall.ENOSPC
		}
		*allocated = append(*allocated, rel)
		exts = append(exts, briefs.Extent{
			Offset: blk,
			Phys:   b.dataRegionStart + rel,
			Len:    1,
			Flags:  briefs.ExtentFlagUnwritten,
		})
	}
	return exts, nil
}

// preallocate allocates unwritten extents covering [start, end) for blocks not
// already mapped, rebuilds the index, and commits. With KEEP_SIZE the file size
// is unchanged; otherwise it grows to end. allocated carries any blocks already
// allocated by the caller this op (inline-data promotion) so they are journaled
// and rolled back with the preallocate's own allocations.
func (b *BrieFS) preallocate(in *briefs.Inode, start, end uint64, mode uint32, allocated []uint64) error {
	bs := b.blockSize
	startBlk := start / bs
	endBlk := (end + bs - 1) / bs // ceiling

	exts, oldNodes, err := b.collectExtentsAndNodes(in)
	if err != nil {
		return err
	}

	for blk := startBlk; blk < endBlk; {
		if _, found := lookupExtent(exts, blk); found {
			blk++ // already mapped (written or unwritten)
			continue
		}
		// Contiguous hole segment [blk, holeEnd): allocate it as one
		// unwritten run when the bitmap allows.
		holeEnd := blk + 1
		for holeEnd < endBlk {
			if _, found := lookupExtent(exts, holeEnd); found {
				break
			}
			holeEnd++
		}
		newExts, err := b.allocUnwrittenHole(blk, holeEnd, &allocated)
		if err != nil {
			b.rollbackAlloc(allocated)
			return err
		}
		for _, e := range newExts {
			// Unwritten blocks read as zeros (readFileData) and
			// convert on write (writeExtentData), so no zeroing here.
			exts = insertExtentSorted(exts, e)
		}
		blk = holeEnd
	}

	// Grow the file size for a plain (non-KEEP_SIZE) preallocate past EOF.
	if mode&fallocKeepSize == 0 && end > in.FileSize {
		in.FileSize = end
	}
	sec, nsec := nowTime()
	in.MtimeSec, in.MtimeNsec = sec, nsec
	in.CtimeSec, in.CtimeNsec = sec, nsec

	// Rebuild the index (new btree nodes are added to allocated) and commit.
	var drain []uint64
	if err := b.rebuildExtentIndex(in, exts, oldNodes, &drain, &allocated); err != nil {
		b.rollbackAlloc(allocated)
		return err
	}
	return b.commitExtentChange(in, allocated, nil, oldNodes)
}

// punchHole frees the data blocks in [off, off+size) and leaves a hole, mirroring
// briefs_do_punch_hole. The range is split out of any overlapping extents.
func (b *BrieFS) punchHole(in *briefs.Inode, off, size uint64) error {
	bs := b.blockSize
	startBlk := off / bs
	endBlk := (off + size + bs - 1) / bs

	exts, oldNodes, err := b.collectExtentsAndNodes(in)
	if err != nil {
		return err
	}
	newExts, freed := freeExtentRange(exts, startBlk, endBlk)

	sec, nsec := nowTime()
	in.MtimeSec, in.MtimeNsec = sec, nsec
	in.CtimeSec, in.CtimeNsec = sec, nsec

	var allocated []uint64
	var drain []uint64
	if err := b.rebuildExtentIndex(in, newExts, oldNodes, &drain, &allocated); err != nil {
		b.rollbackAlloc(allocated)
		return err
	}
	return b.commitExtentChange(in, allocated, freed, oldNodes)
}

// shiftExtents rewrites the extent list for COLLAPSE_RANGE (dir < 0) and
// INSERT_RANGE (dir > 0), ported from briefs_shift_extents (file.c:3129).
//
// For collapse (S, L in blocks): the middle [S, S+L) is removed -- its data
// blocks are collected in freed (journaled + freed by the caller's commit) and
// dropped from the list; every extent at/after S+L shifts down by L; extents
// straddling S or S+L are split (kept prefix + freed middle + re-pointed
// suffix at S).
//
// For insert: a hole [S, S+L) is opened; every extent at/after S shifts up by
// L; an extent straddling S is split into a kept prefix and a suffix shifted
// to S+L. No blocks are freed or allocated. The returned list is folded
// through insertExtentSorted so adjacent phys-contiguous same-flag pieces
// merge, like the kernel's rebuild.
func shiftExtents(exts []briefs.Extent, S, L uint64, dir int) (newExts []briefs.Extent, freed []uint64) {
	var raw []briefs.Extent
	for _, e := range exts {
		o, eend := e.Offset, e.Offset+e.Len

		if dir < 0 {
			// collapse: remove [S, S+L), shift >= S+L down by L
			switch {
			case eend <= S:
				raw = append(raw, e)
			case o >= S+L:
				e.Offset = o - L
				raw = append(raw, e)
			case o >= S && eend <= S+L:
				// Wholly inside the removed range: free, drop.
				if e.Phys != 0 {
					for blk := o; blk < eend; blk++ {
						freed = append(freed, e.Phys+(blk-o))
					}
				}
			default:
				// Straddles S and/or S+L.
				if o < S {
					raw = append(raw, briefs.Extent{Offset: o, Phys: e.Phys, Len: S - o, Flags: e.Flags})
				}
				midStart, midEnd := o, eend
				if midStart < S {
					midStart = S
				}
				if midEnd > S+L {
					midEnd = S + L
				}
				if midStart < midEnd && e.Phys != 0 {
					for blk := midStart; blk < midEnd; blk++ {
						freed = append(freed, e.Phys+(blk-o))
					}
				}
				if eend > S+L {
					raw = append(raw, briefs.Extent{
						Offset: S, // (S+L) - L
						Phys:   e.Phys + (S + L - o),
						Len:    eend - (S + L),
						Flags:  e.Flags,
					})
				}
			}
		} else {
			// insert: open hole [S, S+L), shift >= S up by L
			switch {
			case eend <= S:
				raw = append(raw, e)
			case o >= S:
				e.Offset = o + L
				raw = append(raw, e)
			default:
				// Straddles S: prefix kept, suffix shifted up.
				raw = append(raw, briefs.Extent{Offset: o, Phys: e.Phys, Len: S - o, Flags: e.Flags})
				raw = append(raw, briefs.Extent{
					Offset: S + L,
					Phys:   e.Phys + (S - o),
					Len:    eend - S,
					Flags:  e.Flags,
				})
			}
		}
	}
	for _, e := range raw {
		newExts = insertExtentSorted(newExts, e)
	}
	return newExts, freed
}

// collapseRangeOp mirrors briefs_do_collapse_range (file.c:3235): remove the
// block-aligned [off, off+size) and shift the tail down by size; FileSize
// shrinks by size. A range reaching EOF is a truncate, not a collapse
// (-EINVAL); the alignment check keeps the block math exact.
func (b *BrieFS) collapseRangeOp(in *briefs.Inode, off, size uint64) error {
	bs := b.blockSize
	if off%bs != 0 || size%bs != 0 {
		return syscall.EINVAL
	}
	if off+size >= in.FileSize {
		return syscall.EINVAL
	}
	if in.Flags&briefs.InodeFlagInlineData != 0 {
		return syscall.EOPNOTSUPP
	}

	S, L := off/bs, size/bs
	exts, oldNodes, err := b.collectExtentsAndNodes(in)
	if err != nil {
		return err
	}
	newExts, freed := shiftExtents(exts, S, L, -1)

	in.FileSize -= size
	sec, nsec := nowTime()
	in.MtimeSec, in.MtimeNsec = sec, nsec
	in.CtimeSec, in.CtimeNsec = sec, nsec

	var allocated []uint64
	var drain []uint64
	if err := b.rebuildExtentIndex(in, newExts, oldNodes, &drain, &allocated); err != nil {
		b.rollbackAlloc(allocated)
		return err
	}
	return b.commitExtentChange(in, allocated, freed, oldNodes)
}

// insertRangeOp mirrors briefs_do_insert_range (file.c:3316): open a
// block-aligned hole of size at off, shifting the data at/after off up by
// size; FileSize grows by size. The inserted range is a plain hole (reads as
// zero); no blocks are allocated. An offset at/past EOF is -EINVAL (it would
// grow the file over a region the shift never touched), as is a request that
// would push FileSize past the device (the bridge's s_maxbytes stand-in,
// mirroring the kernel's overflow-safe insert check, file.c:3520).
func (b *BrieFS) insertRangeOp(in *briefs.Inode, off, size uint64) error {
	bs := b.blockSize
	if off%bs != 0 || size%bs != 0 {
		return syscall.EINVAL
	}
	if off >= in.FileSize {
		return syscall.EINVAL
	}
	maxBytes := b.sb.TotalBlocks * b.blockSize
	if size > maxBytes-in.FileSize {
		return syscall.EFBIG
	}
	if in.Flags&briefs.InodeFlagInlineData != 0 {
		return syscall.EOPNOTSUPP
	}

	S, L := off/bs, size/bs
	exts, oldNodes, err := b.collectExtentsAndNodes(in)
	if err != nil {
		return err
	}
	newExts, _ := shiftExtents(exts, S, L, +1)

	in.FileSize += size
	sec, nsec := nowTime()
	in.MtimeSec, in.MtimeNsec = sec, nsec
	in.CtimeSec, in.CtimeNsec = sec, nsec

	var allocated []uint64
	var drain []uint64
	if err := b.rebuildExtentIndex(in, newExts, oldNodes, &drain, &allocated); err != nil {
		b.rollbackAlloc(allocated)
		return err
	}
	return b.commitExtentChange(in, allocated, nil, oldNodes)
}

// zeroRangeOp mirrors briefs_do_zero_range (file.c:2846): zero the contents
// of [off, off+size) with ext4/xfs semantics (generic/009):
//
//   - byte-granular zeroing of the partial head/tail blocks (or of the whole
//     range when it never covers a full block); written blocks are zeroed in
//     place and stay "data" -- a partial-block zero must NOT convert to
//     unwritten (generic/009 case 17). Holes/unwritten already read as zero
//     and are skipped.
//
//   - the block-aligned middle [ceil(off), floor(end)) is converted to
//     UNWRITTEN extents: written blocks have their flag flipped in place
//     (same phys; the on-disk data is masked by the unwritten flag, as on
//     ext4), and holes are allocated as fresh unwritten blocks.
//
// With !KEEP_SIZE the file may be extended to end (the extension's old-EOF
// tail is zeroed first unless the EOF block was just converted to unwritten).
// ctime advances always; mtime only when the file grew (the kernel's
// zero-range timestamp tail, file.c:3085).
func (b *BrieFS) zeroRangeOp(in *briefs.Inode, off, size uint64, mode uint32) error {
	bs := b.blockSize
	end := off + size

	var allocated []uint64

	// Inline-data file whose range fits the 256-byte region: zero in place.
	if in.Flags&briefs.InodeFlagInlineData != 0 {
		if end <= inlineDataMax {
			return b.zeroRangeInline(in, off, end, mode)
		}
		// Promote to extent-backed, then take the extent path below; the
		// promoted block joins this op's rollback/journal list.
		var drain []uint64
		if err := b.promoteInlineData(in, &drain, &allocated); err != nil {
			b.rollbackAlloc(allocated)
			return err
		}
	}

	exts, oldNodes, err := b.collectExtentsAndNodes(in)
	if err != nil {
		b.rollbackAlloc(allocated)
		return err
	}

	// Full-block bounds of the conversion middle.
	sFull := (off + bs - 1) / bs
	eFull := end / bs

	// Byte-granular zeroing of the partial blocks: everything outside the
	// middle (or the whole range when there is no middle). Only mapped,
	// written blocks are touched; holes/unwritten read as zero already.
	zeroRegion := func(from, to uint64) error {
		for blk := from / bs; blk*bs < to; blk++ {
			ext, found := lookupExtent(exts, blk)
			if !found || ext.Phys == 0 || ext.Flags&briefs.ExtentFlagUnwritten != 0 {
				continue
			}
			abs := ext.Phys + (blk - ext.Offset)
			// Zero range within this block: [max(from, blkStart),
			// min(to, blkEnd)) shifted to block-relative offsets.
			zFrom := uint64(0)
			if blk*bs < from {
				zFrom = from - blk*bs
			}
			zTo := to - blk*bs
			if zTo > bs {
				zTo = bs
			}
			if err := b.zeroBlockRange(abs, zFrom, zTo); err != nil {
				return err
			}
		}
		return nil
	}
	if sFull < eFull {
		if err := zeroRegion(off, sFull*bs); err != nil {
			b.rollbackAlloc(allocated)
			return err
		}
		if err := zeroRegion(eFull*bs, end); err != nil {
			b.rollbackAlloc(allocated)
			return err
		}
	} else {
		if err := zeroRegion(off, end); err != nil {
			b.rollbackAlloc(allocated)
			return err
		}
	}

	// Convert the middle to unwritten: written extents flip their flag in
	// place (same phys); hole segments are allocated as fresh unwritten
	// blocks. cursor walks the logical block range covered so far.
	var newExts []briefs.Extent
	converted := false
	if sFull < eFull {
		converted = true
		var raw []briefs.Extent
		cursor := sFull
		for _, e := range exts {
			o, ee := e.Offset, e.Offset+e.Len

			// Hole inside the range, before this extent.
			if o > cursor {
				holeEnd := o
				if holeEnd > eFull {
					holeEnd = eFull
				}
				if cursor < holeEnd {
					segs, err := b.allocUnwrittenHole(cursor, holeEnd, &allocated)
					if err != nil {
						b.rollbackAlloc(allocated)
						return err
					}
					raw = append(raw, segs...)
				}
			}

			switch {
			case ee <= sFull || o >= eFull:
				// Entirely outside the range: keep as-is.
				raw = append(raw, e)
			default:
				// Prefix before the range: keep as-is.
				if o < sFull {
					raw = append(raw, briefs.Extent{Offset: o, Phys: e.Phys, Len: sFull - o, Flags: e.Flags})
				}
				// In-range portion: flip to unwritten, same phys.
				ms, me := o, ee
				if ms < sFull {
					ms = sFull
				}
				if me > eFull {
					me = eFull
				}
				raw = append(raw, briefs.Extent{
					Offset: ms,
					Phys:   e.Phys + (ms - o),
					Len:    me - ms,
					Flags:  briefs.ExtentFlagUnwritten,
				})
				// Suffix after the range: keep as-is.
				if ee > eFull {
					raw = append(raw, briefs.Extent{
						Offset: eFull,
						Phys:   e.Phys + (eFull - o),
						Len:    ee - eFull,
						Flags:  e.Flags,
					})
				}
			}

			if c := min(ee, eFull); c > cursor {
				cursor = c
			}
		}
		// Trailing hole after the last extent, inside the range.
		if cursor < eFull {
			segs, err := b.allocUnwrittenHole(cursor, eFull, &allocated)
			if err != nil {
				b.rollbackAlloc(allocated)
				return err
			}
			raw = append(raw, segs...)
		}
		// Fold through insertExtentSorted so adjacent phys-contiguous
		// same-flag pieces merge (a kept unwritten prefix and the flipped
		// in-range portion become one extent), like the kernel's rebuild.
		for _, e := range raw {
			newExts = insertExtentSorted(newExts, e)
		}
	}

	// Extension: zero the old-EOF block's tail before the size advances past
	// it (generic/363), unless the EOF block was just converted to unwritten
	// (it already reads as zero; zeroing its on-disk bytes is unnecessary).
	oldSize := in.FileSize
	grew := false
	if mode&fallocKeepSize == 0 && end > in.FileSize {
		if oldSize%bs != 0 && oldSize/bs < sFull {
			var drain []uint64
			if err := b.zeroEofTail(exts, int64(oldSize), &drain); err != nil {
				b.rollbackAlloc(allocated)
				return err
			}
		}
		in.FileSize = end
		grew = true
	}

	sec, nsec := nowTime()
	in.CtimeSec, in.CtimeNsec = sec, nsec
	if grew {
		in.MtimeSec, in.MtimeNsec = sec, nsec
	}

	if converted {
		var drain []uint64
		if err := b.rebuildExtentIndex(in, newExts, oldNodes, &drain, &allocated); err != nil {
			b.rollbackAlloc(allocated)
			return err
		}
		return b.commitExtentChange(in, allocated, nil, oldNodes)
	}

	// No extent change: persist the inode (times, possible growth) and the
	// zeroed partial blocks via the metadata-only commit.
	if err := b.dev.Fdatasync(); err != nil {
		b.failWrite()
		return err
	}
	if err := b.journalInodeFull(in); err != nil {
		b.failWrite()
		return err
	}
	if err := b.journal.Sync(false); err != nil {
		b.failWrite()
		return err
	}
	if err := b.writeInodeDirect(in); err != nil {
		b.failWrite()
		return err
	}
	return b.dev.Fdatasync()
}

// zeroRangeInline applies ZERO_RANGE to an inline-data file whose range fits
// the 256-byte region (the kernel do_zero_range's first branch, file.c:2878):
// zero [off, min(end, size)) of the region, optionally grow the size to end,
// and commit via the metadata-only path. An extension's new bytes are not
// zeroed here (the kernel does not either): the inline region past the old
// size reads as zero by convention.
func (b *BrieFS) zeroRangeInline(in *briefs.Inode, off, end uint64, mode uint32) error {
	zStart, zEnd := off, end
	if zEnd > in.FileSize {
		zEnd = in.FileSize
	}
	if zStart < zEnd {
		region := in.InlineData()
		for i := zStart; i < zEnd; i++ {
			region[i] = 0
		}
		in.SetInlineData(region)
	}

	grew := false
	if mode&fallocKeepSize == 0 && end > in.FileSize {
		in.FileSize = end
		grew = true
	}
	sec, nsec := nowTime()
	in.CtimeSec, in.CtimeNsec = sec, nsec
	if grew {
		in.MtimeSec, in.MtimeNsec = sec, nsec
	}

	if err := b.journalInodeFull(in); err != nil {
		b.failWrite()
		return err
	}
	if err := b.journal.Sync(false); err != nil {
		b.failWrite()
		return err
	}
	if err := b.writeInodeDirect(in); err != nil {
		b.failWrite()
		return err
	}
	return b.dev.Fdatasync()
}

// truncateInode is the public truncate entry: lock + read + truncateLocked.
func (b *BrieFS) truncateInode(ino uint64, newSize uint64) error {
	if b.readOnly {
		return syscall.EROFS
	}
	lock := b.inodeBlockLock(ino)
	lock.Lock()
	defer lock.Unlock()
	in, err := b.inodes.ReadInode(ino)
	if err != nil {
		return err
	}
	return b.truncateLocked(in, newSize)
}

// zeroEofTailBlock zeroes [size, block_end) of the block containing @size, if
// that block is mapped and written. Mirrors briefs_zero_eof_tail (defensive).
func (b *BrieFS) zeroEofTailBlock(exts []briefs.Extent, size uint64) error {
	bs := b.blockSize
	blk := (size - 1) / bs
	ext, found := lookupExtent(exts, blk)
	if !found || ext.Phys == 0 || ext.Flags&briefs.ExtentFlagUnwritten != 0 {
		return nil
	}
	abs := ext.Phys + (blk - ext.Offset)
	if err := b.zeroBlockTail(abs, size%bs); err != nil {
		return err
	}
	return b.dev.Fdatasync()
}

// setattrOp applies a VFS setattr (chmod/chown/utimes/truncate) to an inode,
// mirroring briefs_setattr (file.c:836). go-fuse passes the requested fields via
// SetAttrIn.Valid (FATTR_* bits). Truncate delegates to truncateInode's path;
// metadata changes journal a fresh JRN_INODE_FULL.
func (b *BrieFS) setattrOp(ino uint64, in *fuseSetAttrIn) error {
	if b.readOnly {
		return syscall.EROFS
	}
	lock := b.inodeBlockLock(ino)
	lock.Lock()
	defer lock.Unlock()

	di, err := b.inodes.ReadInode(ino)
	if err != nil {
		return err
	}

	// Size change first (it handles its own killpriv + commit).
	if in.valid&fattrSize != 0 {
		if err := b.truncateLocked(di, in.size); err != nil {
			return err
		}
		// Re-read after truncate (truncateLocked mutated + persisted di).
		di, err = b.inodes.ReadInode(ino)
		if err != nil {
			return err
		}
	}

	// Metadata: mode / uid / gid / times.
	changed := false
	if in.valid&fattrMode != 0 {
		di.Filemode = (di.Filemode &^ 0o7777) | (in.mode & 0o7777)
		changed = true
	}
	if in.valid&fattrUID != 0 {
		di.Uid = in.uid
		changed = true
	}
	if in.valid&fattrGID != 0 {
		di.Gid = in.gid
		changed = true
	}
	if in.valid&fattrATime != 0 {
		di.AtimeSec, di.AtimeNsec = in.atime, uint64(in.atimensec)
		changed = true
	}
	if in.valid&fattrMTime != 0 {
		di.MtimeSec, di.MtimeNsec = in.mtime, uint64(in.mtimensec)
		changed = true
	}
	if in.valid&fattrCTime != 0 {
		di.CtimeSec, di.CtimeNsec = in.ctime, uint64(in.ctimensec)
		changed = true
	}
	if in.valid&fattrATimeNow != 0 {
		sec, nsec := nowTime()
		di.AtimeSec, di.AtimeNsec = sec, nsec
		changed = true
	}
	if in.valid&fattrMTimeNow != 0 {
		sec, nsec := nowTime()
		di.MtimeSec, di.MtimeNsec = sec, nsec
		changed = true
	}

	// killpriv on a chown of a setid file (generic/193): strip suid/sgid and
	// clear security.capability. removePrivs strips the mode on the in-memory
	// di (persisted by the final commit below) and, if a capability is present,
	// commits its removal. Do NOT re-read di here -- that would discard the
	// in-memory mode strip.
	if (in.valid&fattrUID != 0 || in.valid&fattrGID != 0) && di.Filemode&(s_ISUID|s_ISGID) != 0 {
		if err := b.removePrivs(di); err != nil {
			return err
		}
		changed = true
	}

	if !changed {
		return nil
	}
	// ctime advances on any metadata change.
	sec, nsec := nowTime()
	di.CtimeSec, di.CtimeNsec = sec, nsec
	if err := b.journalInodeFull(di); err != nil {
		b.failWrite()
		return err
	}
	if err := b.journal.Sync(false); err != nil {
		b.failWrite()
		return err
	}
	if err := b.writeInodeDirect(di); err != nil {
		b.failWrite()
		return err
	}
	return b.dev.Fdatasync()
}

// truncateLocked is the size-change path assuming the inode-block lock is held
// (shared with truncateInode, which also locks). Mirrors briefs_setattr truncate.
func (b *BrieFS) truncateLocked(in *briefs.Inode, newSize uint64) error {
	if userFlagsImmutable(in) {
		return syscall.EPERM
	}
	if userFlagsAppendOnly(in) && newSize < in.FileSize {
		return syscall.EPERM
	}
	if newSize == in.FileSize {
		return nil
	}
	if err := b.removePrivs(in); err != nil {
		return err
	}
	bs := b.blockSize
	oldSize := in.FileSize
	if newSize < oldSize {
		startFree := (newSize + bs - 1) / bs
		exts, oldNodes, err := b.collectExtentsAndNodes(in)
		if err != nil {
			return err
		}
		newExts, freed := freeExtentRange(exts, startFree, ^uint64(0))
		in.FileSize = newSize
		if newSize%bs != 0 && newSize > 0 {
			if err := b.zeroEofTailBlock(newExts, newSize); err != nil {
				return err
			}
		}
		sec, nsec := nowTime()
		in.MtimeSec, in.MtimeNsec = sec, nsec
		in.CtimeSec, in.CtimeNsec = sec, nsec
		var allocated []uint64
		var drain []uint64
		if err := b.rebuildExtentIndex(in, newExts, oldNodes, &drain, &allocated); err != nil {
			b.rollbackAlloc(allocated)
			return err
		}
		return b.commitExtentChange(in, allocated, freed, oldNodes)
	}
	// Truncate up.
	if oldSize%bs != 0 && oldSize > 0 && in.Flags&briefs.InodeFlagInlineData == 0 {
		exts, _, err := b.collectExtentsAndNodes(in)
		if err != nil {
			return err
		}
		if err := b.zeroEofTailBlock(exts, oldSize); err != nil {
			return err
		}
	}
	in.FileSize = newSize
	sec, nsec := nowTime()
	in.MtimeSec, in.MtimeNsec = sec, nsec
	in.CtimeSec, in.CtimeNsec = sec, nsec
	if err := b.journalInodeFull(in); err != nil {
		b.failWrite()
		return err
	}
	if err := b.journal.Sync(false); err != nil {
		b.failWrite()
		return err
	}
	if err := b.writeInodeDirect(in); err != nil {
		b.failWrite()
		return err
	}
	return b.dev.Fdatasync()
}

// removePrivs strips suid/sgid and clears security.capability (killpriv),
// mirroring file_remove_privs (file.c). The suid/sgid bits are cleared on the
// in-memory inode (persisted by the caller's JRN_INODE_FULL); the
// security.capability xattr, if present, is removed via a committed xattr op.
// The caller must hold the inode-block lock.
func (b *BrieFS) removePrivs(in *briefs.Inode) error {
	if in.Filemode&(s_ISUID|s_ISGID) != 0 {
		in.Filemode &^= s_ISUID | s_ISGID
	}
	// Clear security.capability if present (ENODATA => none).
	if in.XattrOffset != 0 {
		if err := b.setXattrLocked(in, "security.capability", nil, 0); err != nil && err != syscall.ENODATA {
			return err
		}
	}
	return nil
}

// freeExtentRange removes the blocks in [startBlk, endBlk) from the extent list,
// freeing any mapped blocks in the range and splitting overlapping extents. The
// range becomes a hole (a gap in the returned list). Returns the new list and
// the freed absolute block numbers.
func freeExtentRange(exts []briefs.Extent, startBlk, endBlk uint64) (newExts []briefs.Extent, freed []uint64) {
	for _, ext := range exts {
		extEnd := ext.Offset + ext.Len
		if extEnd <= startBlk || ext.Offset >= endBlk {
			newExts = append(newExts, ext)
			continue
		}
		ovStart := ext.Offset
		if ovStart < startBlk {
			ovStart = startBlk
		}
		ovEnd := extEnd
		if ovEnd > endBlk {
			ovEnd = endBlk
		}
		// Free the overlapping mapped blocks.
		if ext.Phys != 0 {
			for blk := ovStart; blk < ovEnd; blk++ {
				freed = append(freed, ext.Phys+(blk-ext.Offset))
			}
		}
		// Keep the portion before the overlap.
		if ext.Offset < ovStart {
			newExts = append(newExts, briefs.Extent{Offset: ext.Offset, Phys: ext.Phys, Len: ovStart - ext.Offset, Flags: ext.Flags})
		}
		// Keep the portion after the overlap (re-pointed phys).
		if ovEnd < extEnd {
			newExts = append(newExts, briefs.Extent{Offset: ovEnd, Phys: ext.Phys + (ovEnd - ext.Offset), Len: extEnd - ovEnd, Flags: ext.Flags})
		}
	}
	return
}
