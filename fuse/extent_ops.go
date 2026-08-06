// Package fuse: fallocate, truncate (setattr size), and setattr metadata.
//
// Ports briefs_fallocate (file.c:1876), the truncate paths of briefs_setattr
// (file.c:836), and the killpriv (file_remove_privs) that strips suid/sgid and
// clears security.capability on file-modifying ops. The extent-list changes
// (punch hole, truncate down, preallocate) reuse the Phase-5 infrastructure:
// collect the extents + btree nodes, mutate the sorted extent list, rebuild the
// index, and commit via commitExtentChange.

package fuse

import (
	"syscall"

	"github.com/ctdk/briefs-utils/briefs"
)

// fallocate flags (uapi/linux/fallocate.h).
const (
	fallocKeepSize  uint32 = 0x01
	fallocPunchHole uint32 = 0x02
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

// fallocateOp mirrors briefs_fallocate (file.c:1876): preallocate (optionally
// KEEP_SIZE) allocates unwritten extents; PUNCH_HOLE frees the blocks in the
// range and leaves a hole. COLLAPSE/INSERT range are unsupported (EOPNOTSUPP).
func (b *BrieFS) fallocateOp(ino uint64, off, size uint64, mode uint32) error {
	if b.readOnly {
		return syscall.EROFS
	}
	if mode&^(fallocKeepSize|fallocPunchHole) != 0 {
		return syscall.EOPNOTSUPP
	}
	if mode&fallocPunchHole != 0 && mode&fallocKeepSize == 0 {
		return syscall.EINVAL
	}
	if off < 0 || size == 0 {
		return syscall.EINVAL
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

	end := off + size
	if mode&fallocPunchHole != 0 {
		return b.punchHole(in, off, size)
	}

	// Preallocate: allocate unwritten extents for [off, end). Promote inline
	// data first if the range exceeds the inline region.
	if in.Flags&briefs.InodeFlagInlineData != 0 && end > inlineDataMax {
		var drain, allocated []uint64
		if err := b.promoteInlineData(in, &drain, &allocated); err != nil {
			b.rollbackAlloc(allocated)
			return err
		}
		// Persist the promoted inode (the preallocate below rebuilds again, but
		// the promoted block must be on disk before the unwritten extents).
	}

	return b.preallocate(in, off, end, mode)
}

// preallocate allocates unwritten extents covering [start, end) for blocks not
// already mapped, rebuilds the index, and commits. With KEEP_SIZE the file size
// is unchanged; otherwise it grows to end.
func (b *BrieFS) preallocate(in *briefs.Inode, start, end uint64, mode uint32) error {
	bs := b.blockSize
	startBlk := start / bs
	endBlk := (end + bs - 1) / bs // ceiling

	exts, oldNodes, err := b.collectExtentsAndNodes(in)
	if err != nil {
		return err
	}

	var allocated []uint64
	for blk := startBlk; blk < endBlk; blk++ {
		if _, found := lookupExtent(exts, blk); found {
			continue // already mapped (written or unwritten)
		}
		rel := b.dataAlloc.AllocBlock()
		if rel == 0 {
			b.rollbackAlloc(allocated)
			return syscall.ENOSPC
		}
		allocated = append(allocated, rel)
		abs := b.dataRegionStart + rel
		// Unwritten blocks read as zeros (readFileData) and convert on write
		// (writeExtentData), so no need to zero them here.
		exts = insertExtentSorted(exts, briefs.Extent{Offset: blk, Phys: abs, Len: 1, Flags: briefs.ExtentFlagUnwritten})
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
	buf, err := b.dev.ReadBlock(abs)
	if err != nil {
		return err
	}
	tail := size % bs
	for i := tail; i < bs; i++ {
		buf[i] = 0
	}
	if err := b.dev.WriteBlock(abs, buf); err != nil {
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