// Package fuse: link, symlink read, and rename (incl. renameat2).
//
// Ports briefs_link (dir.c:432), briefs_symlink read (file.c briefs_get_link),
// and briefs_rename / briefs_rename_exchange / briefs_rename_whiteout
// (dir.c:1162, 742, 928). The create paths for symlink/mknod reuse
// createNamedInode (dir_ops.go); this file holds link, the symlink read path,
// data-extent freeing (shared by unlink/rename-over), and the rename dispatch.

package fuse

import (
	"syscall"

	"github.com/ctdk/briefs-utils/briefs"
)

// Special-file mode bits (S_IFMT values not in the briefs package's Mode* set).
const (
	modeChr  uint32 = 0020000 // S_IFCHR
	modeBlk  uint32 = 0060000 // S_IFBLK
	modeFifo uint32 = 0010000 // S_IFIFO
	modeSock uint32 = 0140000 // S_IFSOCK
)

// renameat2 flags (uapi/linux/fs.h).
const (
	renameNoreplace uint32 = 1 << 0
	renameExchange  uint32 = 1 << 1
	renameWhiteout  uint32 = 1 << 2
)

// symlinkMaxLen caps a symlink target (the kernel uses BRIEFS_NAME_LEN*10).
const symlinkMaxLen = 2550

// linkInDir creates a hard link @name in @parentIno pointing at @targetIno.
// Mirrors briefs_link (dir.c:432): reject dir targets (EPERM), add the entry,
// bump the target's nlink + ctime, advance the parent mtime/ctime. Locking:
// global dir lock + the parent and target inode-block shards.
func (b *BrieFS) linkInDir(parentIno uint64, name string, targetIno uint64) (*briefs.Inode, error) {
	if b.readOnly {
		return nil, syscall.EROFS
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	// Existence check + target lookup against the on-disk trie/inode.
	p0, err := b.inodes.ReadInode(parentIno)
	if err != nil {
		return nil, err
	}
	if !p0.IsDir() {
		return nil, syscall.ENOTDIR
	}
	if existing, _, _ := TrieLookup(b.dev, p0.DirTrieRoot, name); existing != 0 {
		return nil, syscall.EEXIST
	}
	t0, err := b.inodes.ReadInode(targetIno)
	if err != nil {
		return nil, err
	}
	if t0.IsDir() {
		return nil, syscall.EPERM
	}

	b.cacheBegin()
	unlock := b.lockInodeShards([]uint64{parentIno, targetIno})
	defer unlock()

	parent, err := b.readInodeCached(parentIno)
	if err != nil {
		b.cacheAbort()
		return nil, err
	}
	target, err := b.readInodeCached(targetIno)
	if err != nil {
		b.cacheAbort()
		return nil, err
	}

	ftype := uint8(target.Filemode >> 12)
	if err := b.addDirEntry(parent, targetIno, name, ftype); err != nil {
		b.cacheAbort()
		return nil, err
	}
	if err := b.journalDirUpdate(parentIno, targetIno, name, 0, ftype); err != nil {
		b.cacheAbort()
		return nil, err
	}

	// Bump the target's nlink + ctime.
	target.Nlinks++
	sec, nsec := nowTime()
	target.CtimeSec, target.CtimeNsec = sec, nsec
	if err := b.writeInodeCached(target); err != nil {
		b.cacheAbort()
		return nil, err
	}
	if err := b.journalInodeFull(target); err != nil {
		b.cacheAbort()
		return nil, err
	}

	if err := b.updateParentDir(parent, int64(dirEntryPrefixLen+len(name)), 0); err != nil {
		b.cacheAbort()
		return nil, err
	}
	if err := b.journal.Sync(false); err != nil {
		b.cacheAbort()
		return nil, err
	}
	if err := b.flushCache(); err != nil {
		return nil, err
	}
	return target, nil
}

// readSymlink returns the target of a symlink inode. Mirrors briefs_get_link:
// inline targets come from the inode region, extent targets from the single
// inline-extent block (symlinks are never tree-backed).
func (b *BrieFS) readSymlink(ino uint64) (string, error) {
	in, err := b.inodes.ReadInode(ino)
	if err != nil {
		return "", err
	}
	if in.Flags&briefs.InodeFlagInlineData != 0 {
		region := in.InlineData()
		return string(region[:in.FileSize]), nil
	}
	exts, err := collectInodeExtents(b.dev.File(), in, b.blockSize)
	if err != nil {
		return "", err
	}
	if len(exts) == 0 || exts[0].Phys == 0 {
		return "", nil
	}
	buf, err := b.dev.ReadBlock(exts[0].Phys)
	if err != nil {
		return "", err
	}
	return string(buf[:in.FileSize]), nil
}

// freeInodeData frees every data block and btree node an inode owns and journals
// JRN_EXTENT_FREE for each, mirroring briefs_btree_free_all (extent.c). Inline-
// data inodes own no blocks. Used when an inode reaches nlink 0 (unlink, rename
// over a target). The caller must hold the inode's shard lock (so the on-disk
// btree is stable during the walk) and journal the dir-entry removal BEFORE the
// frees, so a partial commit cannot free blocks the on-disk inode still
// references.
func (b *BrieFS) freeInodeData(in *briefs.Inode) error {
	if in.Flags&briefs.InodeFlagInlineData != 0 {
		return nil
	}
	exts, nodes, err := b.collectExtentsAndNodes(in)
	if err != nil {
		return err
	}
	for _, ext := range exts {
		if ext.Phys == 0 {
			continue // hole
		}
		for k := uint64(0); k < ext.Len; k++ {
			abs := ext.Phys + k
			b.dataAlloc.FreeBlock(abs - b.dataRegionStart)
			if err := b.journalExtentFree(in.InodeNumber, abs); err != nil {
				return err
			}
		}
	}
	for _, blk := range nodes {
		b.dataAlloc.FreeBlock(blk - b.dataRegionStart)
		if err := b.journalExtentFree(in.InodeNumber, blk); err != nil {
			return err
		}
	}
	return nil
}

// inodeGetter returns a deduplicating reader over readInodeCached: the same
// ino yields the same *briefs.Inode, so a same-directory rename (where the old
// and new parent are one inode) mutates one struct and the trie-root pointer
// changes (collapse on remove, create on add) are not split across two copies.
type inodeGetter struct {
	b  *BrieFS
	m  map[uint64]*briefs.Inode
	ok bool
}

func (g *inodeGetter) get(ino uint64) (*briefs.Inode, error) {
	if in, ok := g.m[ino]; ok {
		return in, nil
	}
	in, err := g.b.readInodeCached(ino)
	if err != nil {
		return nil, err
	}
	g.m[ino] = in
	return in, nil
}

func newInodeGetter(b *BrieFS) *inodeGetter {
	return &inodeGetter{b: b, m: map[uint64]*briefs.Inode{}}
}

// renameInDir renames @oldName in @oldParentIno to @newName in @newParentIno,
// dispatching on the renameat2 flags. Mirrors briefs_rename (dir.c:1162).
func (b *BrieFS) renameInDir(oldParentIno uint64, oldName string, newParentIno uint64, newName string, flags uint32) error {
	if b.readOnly {
		return syscall.EROFS
	}
	if flags&renameExchange != 0 {
		return b.renameExchange(oldParentIno, oldName, newParentIno, newName)
	}
	if flags&renameWhiteout != 0 {
		return b.renameWhiteout(oldParentIno, oldName, newParentIno, newName)
	}
	return b.renamePlain(oldParentIno, oldName, newParentIno, newName, flags)
}

// renamePlain handles a normal rename (optionally NOREPLACE, optionally
// replacing an existing target). Mirrors briefs_rename's main path (dir.c:1202).
func (b *BrieFS) renamePlain(oldParentIno uint64, oldName string, newParentIno uint64, newName string, flags uint32) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	oldP0, err := b.inodes.ReadInode(oldParentIno)
	if err != nil {
		return err
	}
	newP0, err := b.inodes.ReadInode(newParentIno)
	if err != nil {
		return err
	}
	if !oldP0.IsDir() || !newP0.IsDir() {
		return syscall.ENOTDIR
	}
	movedIno, movedFtype, err := TrieLookup(b.dev, oldP0.DirTrieRoot, oldName)
	if err != nil {
		return syscall.ENOENT
	}
	targetIno, _, _ := TrieLookup(b.dev, newP0.DirTrieRoot, newName)
	if targetIno != 0 && flags&renameNoreplace != 0 {
		return syscall.EEXIST
	}

	// Pre-check a directory target is empty (the VFS does not for ->rename).
	if targetIno != 0 {
		tt, err := b.inodes.ReadInode(targetIno)
		if err != nil {
			return err
		}
		if tt.IsDir() && !b.dirIsEmpty(tt) {
			return syscall.ENOTEMPTY
		}
	}

	b.cacheBegin()
	inos := []uint64{oldParentIno, newParentIno, movedIno}
	if targetIno != 0 {
		inos = append(inos, targetIno)
	}
	unlock := b.lockInodeShards(inos)
	defer unlock()

	g := newInodeGetter(b)
	oldParent, err := g.get(oldParentIno)
	if err != nil {
		b.cacheAbort()
		return err
	}
	newParent, err := g.get(newParentIno)
	if err != nil {
		b.cacheAbort()
		return err
	}
	moved, err := g.get(movedIno)
	if err != nil {
		b.cacheAbort()
		return err
	}

	// 1. Remove an existing target.
	if targetIno != 0 {
		target, err := g.get(targetIno)
		if err != nil {
			b.cacheAbort()
			return err
		}
		if err := b.removeDirEntry(newParent, newName); err != nil {
			b.cacheAbort()
			return err
		}
		if err := b.journalDirUpdate(newParentIno, 0, newName, 1, 0); err != nil {
			b.cacheAbort()
			return err
		}
		if target.IsDir() {
			newParent.Nlinks--
			target.Nlinks = 0
		} else {
			target.Nlinks--
		}
		sec, nsec := nowTime()
		target.CtimeSec, target.CtimeNsec = sec, nsec
		if err := b.writeInodeCached(target); err != nil {
			b.cacheAbort()
			return err
		}
		if err := b.journalInodeFull(target); err != nil {
			b.cacheAbort()
			return err
		}
		if target.Nlinks == 0 {
			if target.IsDir() && target.DirTrieRoot != 0 {
				if err := b.trieFreeNode(target.DirTrieRoot); err != nil {
					b.cacheAbort()
					return err
				}
				target.DirTrieRoot = 0
			}
			if err := b.freeInodeData(target); err != nil {
				b.cacheAbort()
				return err
			}
			if err := b.FreeInode(targetIno); err != nil {
				b.cacheAbort()
				return err
			}
		}
	}

	// 2. Remove the old entry, add the new entry.
	if err := b.removeDirEntry(oldParent, oldName); err != nil {
		b.cacheAbort()
		return err
	}
	if err := b.journalDirUpdate(oldParentIno, 0, oldName, 1, 0); err != nil {
		b.cacheAbort()
		return err
	}
	if err := b.addDirEntry(newParent, movedIno, newName, movedFtype); err != nil {
		b.cacheAbort()
		return err
	}
	if err := b.journalDirUpdate(newParentIno, movedIno, newName, 0, movedFtype); err != nil {
		b.cacheAbort()
		return err
	}

	// 3. Cross-directory directory move: adjust parent nlinks + parent_inode.
	cross := oldParentIno != newParentIno
	if cross && moved.IsDir() {
		moved.ParentInode = newParentIno
		if err := b.writeInodeCached(moved); err != nil {
			b.cacheAbort()
			return err
		}
		if err := b.journalInodeFull(moved); err != nil {
			b.cacheAbort()
			return err
		}
	}

	// 4. Parent mtime/ctime (+ nlink for cross-dir dir move). Dir size is not
	// updated for rename, matching the kernel (briefs_update_parent_dir 0,0).
	oldLinkDelta := 0
	newLinkDelta := 0
	if cross && moved.IsDir() {
		oldLinkDelta = -1
		newLinkDelta = 1
	}
	if err := b.updateParentDir(oldParent, 0, oldLinkDelta); err != nil {
		b.cacheAbort()
		return err
	}
	if cross {
		if err := b.updateParentDir(newParent, 0, newLinkDelta); err != nil {
			b.cacheAbort()
			return err
		}
	}

	// 5. ctime on the moved inode.
	if !(cross && moved.IsDir()) { // already journaled above for the cross-dir case
		sec, nsec := nowTime()
		moved.CtimeSec, moved.CtimeNsec = sec, nsec
		if err := b.writeInodeCached(moved); err != nil {
			b.cacheAbort()
			return err
		}
		if err := b.journalInodeFull(moved); err != nil {
			b.cacheAbort()
			return err
		}
	}

	if err := b.journal.Sync(false); err != nil {
		b.cacheAbort()
		return err
	}
	if err := b.flushCache(); err != nil {
		return err
	}
	return nil
}

// renameExchange swaps two existing entries. Mirrors briefs_rename_exchange
// (dir.c:742): remove+add pairs in both dirs journaled as 4 JRN_DIR_UPDATE
// records so replay re-derives the swap (a bare repoint would leave the old
// pointer on replay).
func (b *BrieFS) renameExchange(oldParentIno uint64, oldName string, newParentIno uint64, newName string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	oldP0, err := b.inodes.ReadInode(oldParentIno)
	if err != nil {
		return err
	}
	newP0, err := b.inodes.ReadInode(newParentIno)
	if err != nil {
		return err
	}
	if !oldP0.IsDir() || !newP0.IsDir() {
		return syscall.ENOTDIR
	}
	oldIno, oldFtype, err := TrieLookup(b.dev, oldP0.DirTrieRoot, oldName)
	if err != nil {
		return syscall.ENOENT
	}
	newIno, newFtype, err := TrieLookup(b.dev, newP0.DirTrieRoot, newName)
	if err != nil {
		return syscall.ENOENT
	}

	b.cacheBegin()
	unlock := b.lockInodeShards([]uint64{oldParentIno, newParentIno, oldIno, newIno})
	defer unlock()

	g := newInodeGetter(b)
	oldParent, err := g.get(oldParentIno)
	if err != nil {
		b.cacheAbort()
		return err
	}
	newParent, err := g.get(newParentIno)
	if err != nil {
		b.cacheAbort()
		return err
	}
	oldIn, err := g.get(oldIno)
	if err != nil {
		b.cacheAbort()
		return err
	}
	newIn, err := g.get(newIno)
	if err != nil {
		b.cacheAbort()
		return err
	}

	// Swap: old_name -> newIno, new_name -> oldIno.
	if err := b.removeDirEntry(oldParent, oldName); err != nil {
		b.cacheAbort()
		return err
	}
	if err := b.journalDirUpdate(oldParentIno, 0, oldName, 1, 0); err != nil {
		b.cacheAbort()
		return err
	}
	if err := b.addDirEntry(oldParent, newIno, oldName, newFtype); err != nil {
		b.cacheAbort()
		return err
	}
	if err := b.journalDirUpdate(oldParentIno, newIno, oldName, 0, newFtype); err != nil {
		b.cacheAbort()
		return err
	}

	if err := b.removeDirEntry(newParent, newName); err != nil {
		b.cacheAbort()
		return err
	}
	if err := b.journalDirUpdate(newParentIno, 0, newName, 1, 0); err != nil {
		b.cacheAbort()
		return err
	}
	if err := b.addDirEntry(newParent, oldIno, newName, oldFtype); err != nil {
		b.cacheAbort()
		return err
	}
	if err := b.journalDirUpdate(newParentIno, oldIno, newName, 0, oldFtype); err != nil {
		b.cacheAbort()
		return err
	}

	// Cross-directory directory moves: swap parent_inode + .. nlinks.
	cross := oldParentIno != newParentIno
	if cross {
		if oldIn.IsDir() {
			oldIn.ParentInode = newParentIno
			oldParent.Nlinks--
			newParent.Nlinks++
		}
		if newIn.IsDir() {
			newIn.ParentInode = oldParentIno
			newParent.Nlinks--
			oldParent.Nlinks++
		}
	}

	// ctime on both moved inodes.
	sec, nsec := nowTime()
	oldIn.CtimeSec, oldIn.CtimeNsec = sec, nsec
	newIn.CtimeSec, newIn.CtimeNsec = sec, nsec
	if err := b.writeInodeCached(oldIn); err != nil {
		b.cacheAbort()
		return err
	}
	if err := b.journalInodeFull(oldIn); err != nil {
		b.cacheAbort()
		return err
	}
	if err := b.writeInodeCached(newIn); err != nil {
		b.cacheAbort()
		return err
	}
	if err := b.journalInodeFull(newIn); err != nil {
		b.cacheAbort()
		return err
	}

	if err := b.updateParentDir(oldParent, 0, 0); err != nil {
		b.cacheAbort()
		return err
	}
	if cross {
		if err := b.updateParentDir(newParent, 0, 0); err != nil {
			b.cacheAbort()
			return err
		}
	}

	if err := b.journal.Sync(false); err != nil {
		b.cacheAbort()
		return err
	}
	if err := b.flushCache(); err != nil {
		return err
	}
	return nil
}

// renameWhiteout renames the source to the destination and leaves a chardev
// whiteout at the source. Mirrors briefs_rename_whiteout (dir.c:928): the
// whiteout inode is journaled JRN_INODE_FULL BEFORE any dir record references
// it, and the old entry is repointed in place (no trie alloc) so an ENOSPC
// abort leaves the source intact.
func (b *BrieFS) renameWhiteout(oldParentIno uint64, oldName string, newParentIno uint64, newName string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	oldP0, err := b.inodes.ReadInode(oldParentIno)
	if err != nil {
		return err
	}
	newP0, err := b.inodes.ReadInode(newParentIno)
	if err != nil {
		return err
	}
	if !oldP0.IsDir() || !newP0.IsDir() {
		return syscall.ENOTDIR
	}
	movedIno, movedFtype, err := TrieLookup(b.dev, oldP0.DirTrieRoot, oldName)
	if err != nil {
		return syscall.ENOENT
	}
	targetIno, _, _ := TrieLookup(b.dev, newP0.DirTrieRoot, newName)
	if targetIno != 0 {
		tt, err := b.inodes.ReadInode(targetIno)
		if err != nil {
			return err
		}
		if tt.IsDir() && !b.dirIsEmpty(tt) {
			return syscall.ENOTEMPTY
		}
	}

	b.cacheBegin()
	inos := []uint64{oldParentIno, newParentIno, movedIno}
	if targetIno != 0 {
		inos = append(inos, targetIno)
	}
	unlock := b.lockInodeShards(inos)
	defer unlock()

	g := newInodeGetter(b)
	oldParent, err := g.get(oldParentIno)
	if err != nil {
		b.cacheAbort()
		return err
	}
	newParent, err := g.get(newParentIno)
	if err != nil {
		b.cacheAbort()
		return err
	}
	moved, err := g.get(movedIno)
	if err != nil {
		b.cacheAbort()
		return err
	}

	// 1. Replace an existing target (failure here leaves the source intact).
	if targetIno != 0 {
		target, err := g.get(targetIno)
		if err != nil {
			b.cacheAbort()
			return err
		}
		if err := b.removeDirEntry(newParent, newName); err != nil {
			b.cacheAbort()
			return err
		}
		if err := b.journalDirUpdate(newParentIno, 0, newName, 1, 0); err != nil {
			b.cacheAbort()
			return err
		}
		if target.IsDir() {
			newParent.Nlinks--
			target.Nlinks = 0
		} else {
			target.Nlinks--
		}
		sec, nsec := nowTime()
		target.CtimeSec, target.CtimeNsec = sec, nsec
		if err := b.writeInodeCached(target); err != nil {
			b.cacheAbort()
			return err
		}
		if err := b.journalInodeFull(target); err != nil {
			b.cacheAbort()
			return err
		}
		if target.Nlinks == 0 {
			if target.IsDir() && target.DirTrieRoot != 0 {
				if err := b.trieFreeNode(target.DirTrieRoot); err != nil {
					b.cacheAbort()
					return err
				}
				target.DirTrieRoot = 0
			}
			if err := b.freeInodeData(target); err != nil {
				b.cacheAbort()
				return err
			}
			if err := b.FreeInode(targetIno); err != nil {
				b.cacheAbort()
				return err
			}
		}
	}

	// 2. Allocate the whiteout chardev and journal its snapshot BEFORE any dir
	//    record references it.
	whiteout, err := b.AllocInode(modeChr, 0, 0, oldParentIno)
	if err != nil {
		b.cacheAbort()
		return err
	}
	wUnlock := b.lockOtherInodeBlock(oldParentIno, whiteout.InodeNumber)
	defer func() {
		if wUnlock != nil {
			wUnlock.Unlock()
		}
	}()
	if err := b.writeInodeCached(whiteout); err != nil {
		_ = b.FreeInode(whiteout.InodeNumber)
		b.cacheAbort()
		return err
	}
	if err := b.journalInodeFull(whiteout); err != nil {
		_ = b.FreeInode(whiteout.InodeNumber)
		b.cacheAbort()
		return err
	}

	// 3. Repoint the old entry to the whiteout in place (no trie alloc).
	wFtype := uint8(modeChr >> 12)
	if err := b.TrieUpdateEntry(oldParent, oldName, whiteout.InodeNumber, wFtype); err != nil {
		_ = b.FreeInode(whiteout.InodeNumber)
		b.cacheAbort()
		return err
	}
	if err := b.journalDirUpdate(oldParentIno, 0, oldName, 1, 0); err != nil {
		b.cacheAbort()
		return err
	}
	if err := b.journalDirUpdate(oldParentIno, whiteout.InodeNumber, oldName, 0, wFtype); err != nil {
		b.cacheAbort()
		return err
	}

	// 4. Add the new entry pointing at the source inode.
	if err := b.addDirEntry(newParent, movedIno, newName, movedFtype); err != nil {
		b.cacheAbort()
		return err
	}
	if err := b.journalDirUpdate(newParentIno, movedIno, newName, 0, movedFtype); err != nil {
		b.cacheAbort()
		return err
	}

	// 5. Cross-directory directory move.
	cross := oldParentIno != newParentIno
	if cross && moved.IsDir() {
		moved.ParentInode = newParentIno
		if err := b.writeInodeCached(moved); err != nil {
			b.cacheAbort()
			return err
		}
		if err := b.journalInodeFull(moved); err != nil {
			b.cacheAbort()
			return err
		}
	}
	sec, nsec := nowTime()
	moved.CtimeSec, moved.CtimeNsec = sec, nsec
	if !(cross && moved.IsDir()) {
		if err := b.writeInodeCached(moved); err != nil {
			b.cacheAbort()
			return err
		}
		if err := b.journalInodeFull(moved); err != nil {
			b.cacheAbort()
			return err
		}
	}

	oldLinkDelta := 0
	newLinkDelta := 0
	if cross && moved.IsDir() {
		oldLinkDelta = -1
		newLinkDelta = 1
	}
	if err := b.updateParentDir(oldParent, 0, oldLinkDelta); err != nil {
		b.cacheAbort()
		return err
	}
	if cross {
		if err := b.updateParentDir(newParent, 0, newLinkDelta); err != nil {
			b.cacheAbort()
			return err
		}
	}

	if err := b.journal.Sync(false); err != nil {
		b.cacheAbort()
		return err
	}
	if err := b.flushCache(); err != nil {
		return err
	}
	return nil
}