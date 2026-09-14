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

	// WHITEOUT_MODE is the permission set the kernel gives whiteouts
	// (fs/namei.c); briefs_rename_whiteout creates them with
	// S_IFCHR | WHITEOUT_MODE (dir.c:1036).
	modeWhiteout uint32 = modeChr | 0600
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
func (b *BrieFS) linkInDir(parentIno uint64, name string, targetIno uint64) (_ *briefs.Inode, err error) {
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

	unlock := b.lockInodeShards([]uint64{parentIno, targetIno})
	defer unlock()
	defer b.cacheEpilogue(&err)()

	parent, err := b.readInodeCached(parentIno)
	if err != nil {
		return nil, err
	}
	target, err := b.readInodeCached(targetIno)
	if err != nil {
		return nil, err
	}

	ftype := uint8(target.Filemode >> 12)
	if err := b.addDirEntry(parent, targetIno, name, ftype); err != nil {
		return nil, err
	}
	if err := b.journalDirUpdate(parentIno, targetIno, name, 0, ftype); err != nil {
		return nil, err
	}

	// Bump the target's nlink + ctime.
	target.Nlinks++
	stampCtime(target)
	if err := b.writeInodeCached(target); err != nil {
		return nil, err
	}
	if err := b.journalInodeFull(target); err != nil {
		return nil, err
	}

	if err := b.updateParentDir(parent, int64(dirEntryPrefixLen+len(name)), 0); err != nil {
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
// JRN_EXTENT_FREE per extent (run-encoded), mirroring briefs_btree_free_all
// (extent.c). Inline-
// data inodes own no blocks. Used when an inode reaches nlink 0 (unlink, rename
// over a target). The frees are deferred until the records commit
// (deferBlockFree): the block must not be reusable while the last committed
// on-disk state still references it. The caller must hold the inode's shard
// lock (so the on-disk btree is stable during the walk) and journal the
// dir-entry removal BEFORE the frees, so a partial commit cannot free blocks
// the on-disk inode still references.
func (b *BrieFS) freeInodeData(in *briefs.Inode) error {
	if in.Flags&briefs.InodeFlagInlineData != 0 {
		return nil
	}
	tree, err := b.collectExtentTree(in)
	if err != nil {
		return err
	}
	// The walk may have served (and stored) a cache entry for this inode;
	// every block it describes is about to be freed, so drop the entry
	// rather than leave a stale tree behind.
	b.invalidateExtentTree(in.InodeNumber)
	for _, ext := range tree.exts {
		if ext.Phys == 0 {
			continue // hole
		}
		// One queued run + one journal record per extent, run-encoded —
		// the kernel's briefs_btree_free_all journals e->len once per
		// extent (btree.c).  A large unlinked file would otherwise queue
		// and emit one pending-free entry and EXTENT_FREE record per
		// block — the same per-block amplification that OOM'd the daemon
		// on generic/299's whole-device falloc/truncate cycle.
		b.deferBlockFreeRun(ext.Phys, ext.Len)
		if err := b.journalExtentFree(in.InodeNumber, ext.Phys, ext.Len); err != nil {
			return err
		}
	}
	// The index nodes free the same run-encoded way: allNodes tail-merges
	// the walk's adjacent node blocks, so a contiguously-allocated index
	// frees as one record.
	for _, run := range tree.allNodes() {
		b.deferBlockFreeRun(run.first, run.n)
		if err := b.journalExtentFree(in.InodeNumber, run.first, run.n); err != nil {
			return err
		}
	}
	// Drop the inode's unwritten-extent reservation with it (kernel
	// briefs_drop_unwritten_reserve at full extent free, extent.c).
	b.setUnwrittenRes(in.InodeNumber, 0)
	return nil
}

// inodeGetter returns a deduplicating reader over readInodeCached: the same
// ino yields the same *briefs.Inode, so a same-directory rename (where the old
// and new parent are one inode) mutates one struct and the trie-root pointer
// changes (collapse on remove, create on add) are not split across two copies.
type inodeGetter struct {
	b *BrieFS
	m map[uint64]*briefs.Inode
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

// removeRenameTarget removes an existing destination entry and retires the
// inode it pointed at, mirroring the target-replacement half of
// briefs_rename (dir.c:1210): the removal is journaled first so a crash
// leaves the old pointer in place, then the target's nlink drop + ctime
// are persisted and journaled, and a target that reaches nlink 0 loses its
// trie root, data blocks and inode slot.
func (b *BrieFS) removeRenameTarget(newParent *briefs.Inode, newParentIno uint64, newName string, targetIno uint64, target *briefs.Inode) error {
	if err := b.removeDirEntry(newParent, newName); err != nil {
		return err
	}
	if err := b.journalDirUpdate(newParentIno, 0, newName, 1, 0); err != nil {
		return err
	}
	if target.IsDir() {
		newParent.Nlinks--
		target.Nlinks = 0
	} else {
		target.Nlinks--
	}
	stampCtime(target)
	if err := b.writeInodeCached(target); err != nil {
		return err
	}
	if err := b.journalInodeFull(target); err != nil {
		return err
	}
	if target.Nlinks == 0 {
		if target.IsDir() && target.DirTrieRoot != 0 {
			if err := b.trieFreeNode(target.DirTrieRoot); err != nil {
				return err
			}
			target.DirTrieRoot = 0
		}
		if err := b.freeInodeData(target); err != nil {
			return err
		}
		if err := b.FreeInode(targetIno); err != nil {
			return err
		}
	}
	return nil
}

// finishCrossDirMove applies the inode-side effects shared by the plain and
// whiteout rename paths (briefs_rename, dir.c:1346-1395): a cross-directory
// directory move repoints the moved inode's parent_inode, the parents'
// mtime/ctime (and nlink, for that move) advance, and the moved inode's
// ctime is stamped.  The kernel stamps the moved inode's ctime
// unconditionally, so the old "(already journaled above)" skip dropped the
// ctime update on cross-dir directory renames.
func (b *BrieFS) finishCrossDirMove(oldParent, newParent *briefs.Inode, oldParentIno, newParentIno uint64, moved *briefs.Inode) error {
	cross := oldParentIno != newParentIno
	dirMove := cross && moved.IsDir()

	if dirMove {
		moved.ParentInode = newParentIno
		if err := b.writeInodeCached(moved); err != nil {
			return err
		}
		if err := b.journalInodeFull(moved); err != nil {
			return err
		}
	}

	// Parent mtime/ctime (+ nlink for a cross-dir dir move).  Dir size is not
	// updated for rename, matching the kernel (briefs_update_parent_dir 0,0).
	oldLinkDelta, newLinkDelta := 0, 0
	if dirMove {
		oldLinkDelta, newLinkDelta = -1, 1
	}
	if err := b.updateParentDir(oldParent, 0, oldLinkDelta); err != nil {
		return err
	}
	if cross {
		if err := b.updateParentDir(newParent, 0, newLinkDelta); err != nil {
			return err
		}
	}

	// The moved inode's ctime advances unconditionally (POSIX; kernel
	// dir.c:1379 stamps it after the cross-dir block).
	stampCtime(moved)
	if err := b.writeInodeCached(moved); err != nil {
		return err
	}
	if err := b.journalInodeFull(moved); err != nil {
		return err
	}
	return nil
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

// renameOp carries the shared rename prologue's results to the op body.
type renameOp struct {
	g           *inodeGetter
	oldParent   *briefs.Inode
	newParent   *briefs.Inode
	inos        []uint64 // the shard list locked for the op
	movedIno    uint64
	movedFtype  uint8
	targetIno   uint64
	targetFtype uint8
}

// beginRename is the shared skeleton of the three rename variants: under
// the global lock, read and validate both parents, look up the source entry
// (ENOENT when missing) and the destination entry — required to exist for
// exchange, otherwise probed with a NOREPLACE/EEXIST check (plain only;
// WHITEOUT|NOREPLACE is not a valid combination and the whiteout path
// ignores NOREPLACE) and a directory target pre-checked empty (the VFS
// does not do that for ->rename).  Then lock the shards of every inode
// the op touches, begin the op cache, and run @body under the dedup
// getter.  The epilogue is shared too (fix B): any error aborts the op
// cache, success merges it — records are in the ring, the metadata blocks
// move to the deferred map, durable at the next journal sync.
func (b *BrieFS) beginRename(oldParentIno uint64, oldName string, newParentIno uint64, newName string, flags uint32, body func(op *renameOp) error) (err error) {
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

	var targetIno uint64
	var targetFtype uint8
	if flags&renameExchange != 0 {
		targetIno, targetFtype, err = TrieLookup(b.dev, newP0.DirTrieRoot, newName)
		if err != nil {
			return syscall.ENOENT
		}
	} else {
		targetIno, targetFtype, _ = TrieLookup(b.dev, newP0.DirTrieRoot, newName)
		if targetIno != 0 && flags&renameNoreplace != 0 && flags&renameWhiteout == 0 {
			return syscall.EEXIST
		}
		// Pre-check a directory target is empty (the VFS does not for
		// ->rename).
		if targetIno != 0 {
			tt, err := b.inodes.ReadInode(targetIno)
			if err != nil {
				return err
			}
			if tt.IsDir() && !b.dirIsEmpty(tt) {
				return syscall.ENOTEMPTY
			}
		}
	}

	inos := []uint64{oldParentIno, newParentIno, movedIno}
	if targetIno != 0 {
		inos = append(inos, targetIno)
	}
	unlock := b.lockInodeShards(inos)
	defer unlock()
	defer b.cacheEpilogue(&err)()

	g := newInodeGetter(b)
	oldParent, err := g.get(oldParentIno)
	if err != nil {
		return err
	}
	newParent, err := g.get(newParentIno)
	if err != nil {
		return err
	}

	return body(&renameOp{
		g:           g,
		oldParent:   oldParent,
		newParent:   newParent,
		inos:        inos,
		movedIno:    movedIno,
		movedFtype:  movedFtype,
		targetIno:   targetIno,
		targetFtype: targetFtype,
	})
}

// renamePlain handles a normal rename (optionally NOREPLACE, optionally
// replacing an existing target). Mirrors briefs_rename's main path (dir.c:1202).
func (b *BrieFS) renamePlain(oldParentIno uint64, oldName string, newParentIno uint64, newName string, flags uint32) error {
	return b.beginRename(oldParentIno, oldName, newParentIno, newName, flags, func(op *renameOp) error {
		moved, err := op.g.get(op.movedIno)
		if err != nil {
			return err
		}

		// 1. Remove an existing target.
		if op.targetIno != 0 {
			target, err := op.g.get(op.targetIno)
			if err != nil {
				return err
			}
			if err := b.removeRenameTarget(op.newParent, newParentIno, newName, op.targetIno, target); err != nil {
				return err
			}
		}

		// 2. Remove the old entry, add the new entry.
		if err := b.removeDirEntry(op.oldParent, oldName); err != nil {
			return err
		}
		if err := b.journalDirUpdate(oldParentIno, 0, oldName, 1, 0); err != nil {
			return err
		}
		if err := b.addDirEntry(op.newParent, op.movedIno, newName, op.movedFtype); err != nil {
			return err
		}
		if err := b.journalDirUpdate(newParentIno, op.movedIno, newName, 0, op.movedFtype); err != nil {
			return err
		}

		// 3. Cross-dir dir move (parent_inode + parent nlinks), parent
		// mtime/ctime, and the moved inode's ctime.
		return b.finishCrossDirMove(op.oldParent, op.newParent, oldParentIno, newParentIno, moved)
	})
}

// renameExchange swaps two existing entries. Mirrors briefs_rename_exchange
// (dir.c:742): remove+add pairs in both dirs journaled as 4 JRN_DIR_UPDATE
// records so replay re-derives the swap (a bare repoint would leave the old
// pointer on replay).
func (b *BrieFS) renameExchange(oldParentIno uint64, oldName string, newParentIno uint64, newName string) error {
	return b.beginRename(oldParentIno, oldName, newParentIno, newName, renameExchange, func(op *renameOp) error {
		oldIn, err := op.g.get(op.movedIno)
		if err != nil {
			return err
		}
		newIn, err := op.g.get(op.targetIno)
		if err != nil {
			return err
		}

		// Swap: old_name -> newIno, new_name -> oldIno.
		if err := b.removeDirEntry(op.oldParent, oldName); err != nil {
			return err
		}
		if err := b.journalDirUpdate(oldParentIno, 0, oldName, 1, 0); err != nil {
			return err
		}
		if err := b.addDirEntry(op.oldParent, op.targetIno, oldName, op.targetFtype); err != nil {
			return err
		}
		if err := b.journalDirUpdate(oldParentIno, op.targetIno, oldName, 0, op.targetFtype); err != nil {
			return err
		}

		if err := b.removeDirEntry(op.newParent, newName); err != nil {
			return err
		}
		if err := b.journalDirUpdate(newParentIno, 0, newName, 1, 0); err != nil {
			return err
		}
		if err := b.addDirEntry(op.newParent, op.movedIno, newName, op.movedFtype); err != nil {
			return err
		}
		if err := b.journalDirUpdate(newParentIno, op.movedIno, newName, 0, op.movedFtype); err != nil {
			return err
		}

		// Cross-directory directory moves: swap parent_inode + .. nlinks.
		cross := oldParentIno != newParentIno
		if cross {
			if oldIn.IsDir() {
				oldIn.ParentInode = newParentIno
				op.oldParent.Nlinks--
				op.newParent.Nlinks++
			}
			if newIn.IsDir() {
				newIn.ParentInode = oldParentIno
				op.newParent.Nlinks--
				op.oldParent.Nlinks++
			}
		}

		// ctime on both moved inodes.
		sec, nsec := nowTime()
		oldIn.CtimeSec, oldIn.CtimeNsec = sec, nsec
		newIn.CtimeSec, newIn.CtimeNsec = sec, nsec
		if err := b.writeInodeCached(oldIn); err != nil {
			return err
		}
		if err := b.journalInodeFull(oldIn); err != nil {
			return err
		}
		if err := b.writeInodeCached(newIn); err != nil {
			return err
		}
		if err := b.journalInodeFull(newIn); err != nil {
			return err
		}

		if err := b.updateParentDir(op.oldParent, 0, 0); err != nil {
			return err
		}
		if cross {
			return b.updateParentDir(op.newParent, 0, 0)
		}
		return nil
	})
}

// renameWhiteout renames the source to the destination and leaves a chardev
// whiteout at the source. Mirrors briefs_rename_whiteout (dir.c:928): the
// whiteout inode is journaled JRN_INODE_FULL BEFORE any dir record references
// it, and the old entry is repointed in place (no trie alloc) so an ENOSPC
// abort leaves the source intact.
func (b *BrieFS) renameWhiteout(oldParentIno uint64, oldName string, newParentIno uint64, newName string) error {
	return b.beginRename(oldParentIno, oldName, newParentIno, newName, renameWhiteout, func(op *renameOp) error {
		moved, err := op.g.get(op.movedIno)
		if err != nil {
			return err
		}

		// 1. Replace an existing target (failure here leaves the source intact).
		if op.targetIno != 0 {
			target, err := op.g.get(op.targetIno)
			if err != nil {
				return err
			}
			if err := b.removeRenameTarget(op.newParent, newParentIno, newName, op.targetIno, target); err != nil {
				return err
			}
		}

		// 2. Allocate the whiteout chardev and journal its snapshot BEFORE any dir
		//    record references it.  S_IFCHR | 0600 like the kernel (dir.c:1036):
		//    a zero-perm whiteout made go-fuse's reply patcher (without
		//    NullPermissions) advertise it as drwxr-xr-x, so nothing could
		//    remove it (generic/585).
		whiteout, err := b.AllocInode(modeWhiteout, 0, 0, oldParentIno)
		if err != nil {
			return err
		}
		// Dedup the whiteout's shard against ALL shards held above, not just the
		// parent's: the fresh slot can share a shard with the moved or target
		// inode's block, and re-locking it would self-deadlock (see
		// lockInodeBlockUnlessHeld).
		wUnlock := b.lockInodeBlockUnlessHeld(op.inos, whiteout.InodeNumber)
		defer func() {
			if wUnlock != nil {
				wUnlock.Unlock()
			}
		}()
		if err := b.writeInodeCached(whiteout); err != nil {
			_ = b.FreeInode(whiteout.InodeNumber)
			return err
		}
		// Arm the fresh whiteout slot (with its new generation) in the page
		// cache before its snapshot is committed, so replay's generation guard
		// finds it (see writeThroughFreshInodeSlot).
		if err := b.writeThroughFreshInodeSlot(whiteout); err != nil {
			_ = b.FreeInode(whiteout.InodeNumber)
			return err
		}
		if err := b.journalInodeFull(whiteout); err != nil {
			_ = b.FreeInode(whiteout.InodeNumber)
			return err
		}

		// 3. Repoint the old entry to the whiteout in place (no trie alloc).
		wFtype := uint8(modeChr >> 12)
		if err := b.TrieUpdateEntry(op.oldParent, oldName, whiteout.InodeNumber, wFtype); err != nil {
			_ = b.FreeInode(whiteout.InodeNumber)
			return err
		}
		if err := b.journalDirUpdate(oldParentIno, 0, oldName, 1, 0); err != nil {
			return err
		}
		if err := b.journalDirUpdate(oldParentIno, whiteout.InodeNumber, oldName, 0, wFtype); err != nil {
			return err
		}

		// 4. Add the new entry pointing at the source inode.
		if err := b.addDirEntry(op.newParent, op.movedIno, newName, op.movedFtype); err != nil {
			return err
		}
		if err := b.journalDirUpdate(newParentIno, op.movedIno, newName, 0, op.movedFtype); err != nil {
			return err
		}

		// 5. Cross-dir dir move (parent_inode + parent nlinks), parent
		// mtime/ctime, and the moved inode's ctime.
		return b.finishCrossDirMove(op.oldParent, op.newParent, oldParentIno, newParentIno, moved)
	})
}
