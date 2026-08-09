// Package fuse: core directory mutation operations.
//
// This ports the kernel's directory create/unlink/rmdir paths (dir.c
// briefs_add_dir_entry:195, briefs_remove_dir_entry:280, briefs_create:369,
// briefs_unlink_common:586, briefs_rmdir:702; inode.c briefs_finish_create:586,
// briefs_update_parent_dir:343) into Go, driving the trie mutation engine
// (trie_mutate.go) and the journal writer (briefs.Journal) through the
// per-operation block cache (cache.go).
//
// Durability ordering — commit-before-flush.  The kernel commits the journal
// tail (log_end) before flushing the metadata buffers it references, relying on
// idempotent replay to re-derive any metadata that did not reach disk.  The
// FUSE bridge has no buffer cache: metadata lives in the per-op block cache
// (process memory) until flushCache writes it to the backing file's page cache.
// To stay crash-safe under the FUSE crash model (kill -9 the FUSE process: the
// OS page cache survives, process memory is lost) the journal must be committed
// BEFORE the metadata is flushed: if the process dies between the two, the
// committed journal records let the kernel replay re-derive the metadata
// (trie pages via replay_dir_update, inode blocks via replay_inode_full); the
// reverse order would leave durable metadata referencing un-journaled
// allocations with a stale allocator bitmap.  So each op ends with
// journal.Sync(false) then flushCache.
//
// Journal record ordering matches the kernel's intent (the generic/640
// fix): when a directory's trie root is freshly created, JRN_INODE_FULL of the
// parent (carrying the new root) is written BEFORE the JRN_DIR_UPDATE that
// inserts into it, because replay derives the trie root from the parent's most
// recent INODE_FULL.  The child's INODE_FULL is written before the DIR_UPDATE
// too (briefs_finish_create) so replay reconstructs the child inode before it
// is referenced.

package fuse

import (
	"fmt"
	"syscall"
	"time"

	"github.com/ctdk/briefs-utils/briefs"
)

// dirEntryPrefixLen mirrors BRIEFS_DIR_ENTRY_PREFIX_LEN (briefs.h:1020): the
// per-entry overhead added to a directory's logical size.
const dirEntryPrefixLen = 2

// modeSetGID is S_ISGID (02000), used for setgid inheritance on mkdir.
const modeSetGID = 0o2000

// nowTime returns the current wall time as (sec, nsec), mirroring the kernel's
// current_time() for directory mtime/ctime advancement.
func nowTime() (sec, nsec uint64) {
	t := time.Now()
	return uint64(t.Unix()), uint64(t.Nanosecond())
}

// journalInodeFull marshals the inode and writes a JRN_INODE_FULL record (the
// 560-byte ino + 512-byte raw snapshot).  Mirrors briefs_journal_inode_full,
// but written directly rather than deferred: the FUSE bridge serializes every
// op on the global mu, so the kernel's per-inode batching has no analogue.
func (b *BrieFS) journalInodeFull(in *briefs.Inode) error {
	raw, err := in.MarshalBinary()
	if err != nil {
		return fmt.Errorf("briefs: marshal inode %d for journal: %w", in.InodeNumber, err)
	}
	return b.journal.WriteRecord(briefs.JRN_INODE_FULL,
		briefs.MarshalJrnInodeFull(in.InodeNumber, raw))
}

// journalInodeUpdate writes a JRN_INODE_UPDATE record carrying the inode's
// metadata (mode/nlink/uid/gid/size/times/flags).  Mirrors
// briefs_journal_inode_update (journal.c).
func (b *BrieFS) journalInodeUpdate(in *briefs.Inode) error {
	rec := &briefs.JrnInodeUpdate{
		Ino:       in.InodeNumber,
		Mode:      in.Filemode,
		Nlink:     in.Nlinks,
		Uid:       in.Uid,
		Gid:       in.Gid,
		FileSize:  in.FileSize,
		ATimeSec:  in.AtimeSec,
		ATimeNsec: in.AtimeNsec,
		MTimeSec:  in.MtimeSec,
		MTimeNsec: in.MtimeNsec,
		CTimeSec:  in.CtimeSec,
		CTimeNsec: in.CtimeNsec,
		Flags:     in.Flags,
	}
	return b.journal.WriteRecord(briefs.JRN_INODE_UPDATE, rec.Marshal())
}

// journalDirUpdate writes a JRN_DIR_UPDATE record. op is 0 for add, 1 for
// delete; ftype is the d_type (S_IFMT >> 12).  Mirrors briefs_journal_dir_update.
func (b *BrieFS) journalDirUpdate(parentIno, childIno uint64, name string, op, ftype uint8) error {
	rec := briefs.NewJrnDirUpdate(parentIno, childIno, name, op, ftype)
	return b.journal.WriteRecord(briefs.JRN_DIR_UPDATE, rec.Marshal())
}

// addDirEntry inserts a directory entry into the parent's trie, mirroring
// briefs_add_dir_entry (dir.c:195) including the generic/640 trie-root pinning
// ordering.  The caller must hold b.mu and have an active block cache.
func (b *BrieFS) addDirEntry(parent *briefs.Inode, childIno uint64, name string, ftype uint8) error {
	if parent.DirTrieRoot == 0 {
		if err := b.trieCreateRoot(parent); err != nil {
			return err
		}
		// Pin the freshly allocated trie root in the journal BEFORE any
		// JRN_DIR_UPDATE that will insert into it (generic/640): replay derives
		// the trie root from the parent's most-recent INODE_FULL, so the root
		// must be journaled before the dir-add record.  INODE_FULL is
		// idempotent on replay, so the later updateParentDir snapshot is
		// harmless.
		if err := b.journalInodeFull(parent); err != nil {
			return err
		}
	}

	if err := b.TrieInsert(parent, name, childIno, ftype); err != nil {
		if err != syscall.EEXIST {
			return err
		}
		// Name already exists: update it in place so the entry points at the
		// new inode/type rather than leaving a stale pointer.
		if uerr := b.TrieUpdateEntry(parent, name, childIno, ftype); uerr == nil {
			return nil
		} else if uerr != syscall.ENOENT {
			return uerr
		}
		// Fallback: the existing entry is a pure leaf the update helper cannot
		// replace.  Remove and re-insert (mirrors the kernel's fallback).
		if rerr := b.TrieRemove(parent, name); rerr != nil {
			return rerr
		}
		return b.TrieInsert(parent, name, childIno, ftype)
	}
	return nil
}

// removeDirEntry removes a directory entry from the parent's trie, mirroring
// briefs_remove_dir_entry (dir.c:280).  The caller must hold b.mu and have an
// active block cache.
func (b *BrieFS) removeDirEntry(parent *briefs.Inode, name string) error {
	if parent.DirTrieRoot == 0 {
		return syscall.ENOENT
	}
	return b.TrieRemove(parent, name)
}

// updateParentDir updates the parent directory's size, nlink, and mtime/ctime,
// persists it, and journals a full snapshot.  Mirrors
// briefs_update_parent_dir (inode.c:343).  linkDelta adjusts nlink (+=1 for a
// new subdir, -=1 for a removed subdir, 0 for a non-dir entry); sizeDelta is
// signed (+= prefix+len on add, -= on remove).  The caller must hold b.mu and
// have an active block cache.
func (b *BrieFS) updateParentDir(parent *briefs.Inode, sizeDelta int64, linkDelta int) error {
	newSize := int64(parent.FileSize) + sizeDelta
	if newSize < 0 {
		newSize = 0
	}
	parent.FileSize = uint64(newSize)
	if linkDelta > 0 {
		parent.Nlinks++
	} else if linkDelta < 0 {
		parent.Nlinks--
	}
	// A create/unlink/rename changes the directory's contents, so mtime/ctime
	// must advance (generic/003).
	sec, nsec := nowTime()
	parent.MtimeSec, parent.MtimeNsec = sec, nsec
	parent.CtimeSec, parent.CtimeNsec = sec, nsec

	if err := b.writeInodeCached(parent); err != nil {
		return err
	}
	return b.journalInodeFull(parent)
}

// dirIsEmpty reports whether a directory trie has no entries.  BrieFS does not
// store "." or ".." in the trie, so any present entry means non-empty.
func (b *BrieFS) dirIsEmpty(di *briefs.Inode) bool {
	if di.DirTrieRoot == 0 {
		return true
	}
	it := NewTrieIterator(b.dev, di.DirTrieRoot)
	ino, _, _, err := it.Next()
	return ino == 0 || err != nil
}

// createInDir allocates a new inode, adds it to the parent directory, and
// journals the operation.  Mirrors briefs_create (dir.c:369) +
// briefs_finish_create (inode.c:586).  mode is the full file mode including
// S_IFMT; uid/gid are the caller's credentials; excl selects O_EXCL
// semantics (the kernel relies on the VFS to pre-check existence, but go-fuse
// calls Create directly, so the bridge does the check here).  Returns the new
// inode on a fresh create, the existing inode when the name is already present
// and excl is false, or EEXIST when the name is present and excl is true.  The
// caller must hold b.mu.
// createNamedInode is the shared create path for files, directories, special
// files (mknod), and symlinks. rdev is stored on the inode for block/char
// devices; symlinkTarget (non-empty only for symlinks) is stored inline (<=256B)
// or in one data block plus a JRN_SYMLINK_DATA record. The caller must hold
// b.mu (taken here) — it is a directory operation.
func (b *BrieFS) createNamedInode(parentIno uint64, name string, mode, uid, gid uint32, excl bool, rdev uint64, symlinkTarget string) (*briefs.Inode, error) {
	if b.readOnly {
		return nil, syscall.EROFS
	}
	// The global dir lock covers the existence check (TOCTOU), the shared
	// per-op block cache, and the partial-trie-page pool — none of which are
	// concurrency-safe without a buffer cache. Only one dir op runs at a time.
	b.mu.Lock()
	defer b.mu.Unlock()

	// Existence check against the on-disk trie (current as of the last op's
	// flush; the global mu serializes ops so there is no TOCTOU window).
	parent0, err := b.inodes.ReadInode(parentIno)
	if err != nil {
		return nil, err
	}
	if existing, _, _ := TrieLookup(b.dev, parent0.DirTrieRoot, name); existing != 0 {
		if excl {
			return nil, syscall.EEXIST
		}
		// O_CREAT without O_EXCL on an existing name: open the existing inode
		// rather than creating a new one (would leak the old inode).
		return b.inodes.ReadInode(existing)
	}

	b.cacheBegin()

	// Lock the parent's inode-table block for the cached RMW of the parent
	// inode (updateParentDir) and the parent's trie pages. This serializes
	// against a concurrent file write to a sibling inode in the same block.
	pLock := b.inodeBlockLock(parentIno)
	pLock.Lock()
	defer pLock.Unlock()

	parent, err := b.readInodeCached(parentIno)
	if err != nil {
		b.cacheAbort()
		return nil, err
	}
	if !parent.IsDir() {
		b.cacheAbort()
		return nil, syscall.ENOTDIR
	}

	isDir := (mode & briefs.ModeTypeMask) == briefs.ModeDir
	// setgid inheritance: a new subdirectory inherits S_ISGID from the parent
	// (mkdir); mirrors briefs_mkdir.
	if isDir && (parent.Filemode&modeSetGID) != 0 {
		mode |= modeSetGID
	}

	// AllocInode journals JRN_INODE_ALLOC and builds the inode in memory; it does
	// NOT persist (the caller writes the inode under the child's block lock).
	child, err := b.AllocInode(mode, uid, gid, parentIno)
	if err != nil {
		b.cacheAbort()
		return nil, err
	}

	// Lock the child's inode-table block (now that its number is known) for the
	// cached write of the child inode. Dedup against the parent lock: if the
	// child shares the parent's shard (same 4K table block, or a hash collision)
	// the parent lock already covers it and re-locking would self-deadlock.
	// Held with the parent lock; the global dir lock serializes dir ops and file
	// writes take only a single shard lock, so the parent-then-child order cannot
	// deadlock.
	if cLock := b.lockOtherInodeBlock(parentIno, child.InodeNumber); cLock != nil {
		defer cLock.Unlock()
	}

	// Rollback helper: free the allocated inode and abort the cache. FreeInode
	// zeroes the slot (a cached write) so the child block lock must be held;
	// every abort() below runs after that lock is taken.
	abort := func() {
		_ = b.FreeInode(child.InodeNumber)
		b.cacheAbort()
	}

	// Special files carry their device number.
	if rdev != 0 {
		child.Rdev = rdev
	}

	// Symlinks store their target before the inode is persisted: inline for
	// targets <= 256B, otherwise one data block journaled JRN_SYMLINK_DATA (the
	// target is restored verbatim on replay, so the block is re-derivable and
	// the cache flush at the end suffices).
	if symlinkTarget != "" {
		tlen := uint64(len(symlinkTarget))
		child.FileSize = tlen
		if tlen <= inlineDataMax {
			region := [256]byte{}
			copy(region[:], symlinkTarget)
			child.SetInlineData(region)
			child.Flags |= briefs.InodeFlagInlineData
		} else {
			rel := b.dataAlloc.AllocBlock()
			if rel == 0 {
				abort()
				return nil, syscall.ENOSPC
			}
			abs := b.dataRegionStart + rel
			buf := make([]byte, b.blockSize)
			copy(buf, symlinkTarget)
			if err := b.saveBlock(abs, buf); err != nil {
				b.dataAlloc.FreeBlock(rel)
				abort()
				return nil, err
			}
			if err := b.journalExtentAlloc(child.InodeNumber, 0, abs); err != nil {
				b.dataAlloc.FreeBlock(rel)
				abort()
				return nil, err
			}
			if err := b.journal.WriteRecord(briefs.JRN_SYMLINK_DATA,
				(&briefs.JrnSymlinkData{Ino: child.InodeNumber, Phys: abs, TargetLen: uint32(tlen), Target: []byte(symlinkTarget)}).Marshal()); err != nil {
				b.dataAlloc.FreeBlock(rel)
				abort()
				return nil, err
			}
			child.SetInlineExtent(0, 0, abs, 1, 0)
			child.NumExtentsInline = 1
			child.NumExtentsTotal = 1
		}
	}

	// Directories get their own trie root before being linked into the parent.
	if isDir {
		if err := b.trieCreateRoot(child); err != nil {
			abort()
			return nil, err
		}
	}
	// Persist the child inode (with its trie root if a dir) under the child lock.
	if err := b.writeInodeCached(child); err != nil {
		abort()
		return nil, err
	}

	ftype := uint8(mode >> 12) // S_IFMT >> 12 = d_type

	// addDirEntry pins the parent's trie root (if new) and inserts the entry.
	if err := b.addDirEntry(parent, child.InodeNumber, name, ftype); err != nil {
		abort()
		return nil, err
	}

	// Journal the child's full snapshot BEFORE the JRN_DIR_UPDATE that
	// references it (briefs_finish_create): replay reconstructs the child
	// inode so post-replay reads see the finalized mode/type.
	if err := b.journalInodeFull(child); err != nil {
		abort()
		return nil, err
	}

	// JRN_DIR_UPDATE(add).
	if err := b.journalDirUpdate(parentIno, child.InodeNumber, name, 0, ftype); err != nil {
		abort()
		return nil, err
	}

	// Update the parent: size grows by the entry prefix + name length; nlink
	// grows by one for a new subdirectory.
	linkDelta := 0
	if isDir {
		linkDelta = 1
	}
	if err := b.updateParentDir(parent, int64(dirEntryPrefixLen+len(name)), linkDelta); err != nil {
		abort()
		return nil, err
	}

	// Commit before flush (see file header): commit the journal records, then
	// make the metadata durable.
	if err := b.journal.Sync(false); err != nil {
		b.cacheAbort()
		return nil, err
	}
	if err := b.flushCache(); err != nil {
		return nil, err
	}
	return child, nil
}

// createInDir is the file/directory create path (no rdev, no symlink target).
func (b *BrieFS) createInDir(parentIno uint64, name string, mode, uid, gid uint32, excl bool) (*briefs.Inode, error) {
	return b.createNamedInode(parentIno, name, mode, uid, gid, excl, 0, "")
}

// mknodInDir creates a special file (block/char device, fifo, socket).
func (b *BrieFS) mknodInDir(parentIno uint64, name string, mode, uid, gid uint32, rdev uint64) (*briefs.Inode, error) {
	return b.createNamedInode(parentIno, name, mode, uid, gid, false, rdev, "")
}

// symlinkInDir creates a symbolic link with the given target.
func (b *BrieFS) symlinkInDir(parentIno uint64, name string, target string, uid, gid uint32) (*briefs.Inode, error) {
	return b.createNamedInode(parentIno, name, briefs.ModeSymlink|0o777, uid, gid, false, 0, target)
}
// mirroring briefs_unlink_common (dir.c:586).  isRmdir selects the rmdir path
// (ENOTDIR/ENOTEMPTY checks, parent nlink drop, child nlink cleared).  When the
// target's nlink reaches zero its inode is freed immediately (the kernel
// defers this to evict; the FUSE bridge has no VFS inode cache, and Phase 4
// tests do not exercise open-unlinked files).  Data-extent freeing for
// non-empty files lands in Phase 5.  Locking: the global dir lock + the parent
// and child inode-block locks (parent then child; the global lock serializes
// dir ops so the order cannot deadlock).
func (b *BrieFS) unlinkInDir(parentIno uint64, name string, isRmdir bool) error {
	if b.readOnly {
		return syscall.EROFS
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	b.cacheBegin()

	// Lock the parent's inode-table block for the cached parent-inode RMW
	// (removeDirEntry + updateParentDir) and trie mutation.
	pLock := b.inodeBlockLock(parentIno)
	pLock.Lock()
	defer pLock.Unlock()

	parent, err := b.readInodeCached(parentIno)
	if err != nil {
		b.cacheAbort()
		return err
	}
	if !parent.IsDir() {
		b.cacheAbort()
		return syscall.ENOTDIR
	}

	// Resolve the entry against the on-disk trie (current as of the last op's
	// flush) to get the child ino and d_type.
	childIno, ftype, err := TrieLookup(b.dev, parent.DirTrieRoot, name)
	if err != nil {
		b.cacheAbort()
		return syscall.ENOENT
	}

	// Lock the child's inode-table block for the cached child-inode RMW
	// (nlink/ctime, possible trie free, FreeInode zeroing). Dedup against the
	// parent lock (same shard => already covered; re-locking would self-deadlock).
	if cLock := b.lockOtherInodeBlock(parentIno, childIno); cLock != nil {
		defer cLock.Unlock()
	}

	child, err := b.readInodeCached(childIno)
	if err != nil {
		b.cacheAbort()
		return err
	}

	if isRmdir {
		if !child.IsDir() {
			b.cacheAbort()
			return syscall.ENOTDIR
		}
		if !b.dirIsEmpty(child) {
			b.cacheAbort()
			return syscall.ENOTEMPTY
		}
	} else {
		// unlink on a directory must fail with EISDIR (POSIX).
		if child.IsDir() {
			b.cacheAbort()
			return syscall.EISDIR
		}
	}

	// Remove the directory entry from the trie first.
	if err := b.removeDirEntry(parent, name); err != nil {
		b.cacheAbort()
		return err
	}
	// JRN_DIR_UPDATE(del) after the trie is changed.
	if err := b.journalDirUpdate(parentIno, 0, name, 1, 0); err != nil {
		b.cacheAbort()
		return err
	}

	// Drop nlink.  For rmdir the parent loses the child's ".." link and the
	// child is cleared; for unlink the child loses one link.
	if isRmdir {
		parent.Nlinks--
		child.Nlinks = 0
	} else {
		child.Nlinks--
	}
	// Bump the target's ctime: an nlink change is a metadata change
	// (generic/755).
	sec, nsec := nowTime()
	child.CtimeSec, child.CtimeNsec = sec, nsec
	if err := b.writeInodeCached(child); err != nil {
		b.cacheAbort()
		return err
	}
	// Journal the child nlink/ctime change.
	if err := b.journalInodeUpdate(child); err != nil {
		b.cacheAbort()
		return err
	}

	// If the child is now unreferenced, free its data extents, its trie (for a
	// dir), and its inode.  The JRN_DIR_UPDATE(del) above is already journaled,
	// so the JRN_EXTENT_FREE records here cannot be committed without the entry
	// removal (a partial commit never frees blocks the on-disk inode still
	// references).
	if child.Nlinks == 0 {
		// For a removed directory, free its own trie root page.  The kernel
		// does this at evict time (briefs_evict_inode); the FUSE bridge has no
		// VFS inode cache, so free it here.  An emptied directory's trie is
		// just the root page (TrieRemove's collapse already freed any entry
		// pages and may have cleared DirTrieRoot to 0; if so, nothing to do).
		// A directory that never held entries still has its mkdir-time root
		// page allocated, so this is what reclaims it.
		if isRmdir && child.DirTrieRoot != 0 {
			if err := b.trieFreeNode(child.DirTrieRoot); err != nil {
				b.cacheAbort()
				return err
			}
			child.DirTrieRoot = 0
		}
		// Free the file's data blocks + btree nodes (no-op for dirs/inline).
		if err := b.freeInodeData(child); err != nil {
			b.cacheAbort()
			return err
		}
		if err := b.FreeInode(childIno); err != nil {
			b.cacheAbort()
			return err
		}
	}

	// Update the parent: size shrinks by the entry prefix + name length; nlink
	// was already adjusted for rmdir, so linkDelta is 0 (updateParentDir
	// persists the already-dropped nlink).
	if err := b.updateParentDir(parent, -int64(dirEntryPrefixLen+len(name)), 0); err != nil {
		b.cacheAbort()
		return err
	}

	if err := b.journal.Sync(false); err != nil {
		b.cacheAbort()
		return err
	}
	if err := b.flushCache(); err != nil {
		return err
	}
	_ = ftype
	return nil
}