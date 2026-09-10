// Package fuse: per-operation block cache + deferred-metadata write-back.
//
// The kernel metadata paths use buffer_heads: sb_bread/sb_getblk return the
// SAME buffer for a given block within an operation, so a trie insert that
// reads a page as a parent and again as a sibling-chain node sees one shared
// buffer and the last mark_buffer_dirty carries all mutations.  The FUSE
// bridge has no buffer cache — every BlockDevice.ReadBlock returns a fresh
// copy — so the same pattern would silently clobber edits when two roles in
// one operation land on the same packed trie page (parent and last sibling
// frequently share a block).
//
// cacheBegin/loadBlock/saveBlock provide that per-operation equivalence: a
// short-lived map of block -> working buffer.  loadBlock returns the cached
// buffer (loading from disk on first touch); saveBlock marks it dirty.
//
// Between ops, mutated blocks live in the deferred-metadata map
// (BrieFS.dirtyBlocks): the analog of the kernel's pinned, not-yet-dirty
// buffer heads.  A successful op ends with mergeCache, which moves the op's
// dirty blocks into that map with NO I/O — the journal is not synced per op
// (kernel parity: the kernel only syncs on fsync/sync_fs/DIRSYNC; dir.c:25
// rejects per-create syncs as far too slow).  The map is drained by the
// journal's sync path via the MetaSyncer hook, AFTER the commit point is
// persisted (kernel parity: briefs_journal_flush_owned after the log_end
// persist), so the records justifying a block are always durable before the
// block reaches the device page cache.  Between the merge and the drain,
// readers see the deferred content through BlockDevice's dirty-view hook, so
// lookups/readdirs/stats stay coherent.  Under the FUSE crash model (kill -9:
// process memory is lost, the OS page cache survives) an unsynced op simply
// never happened — the same semantics a buffered op has against the kernel.

package fuse

import (
	"fmt"

	"github.com/ctdk/briefs-utils/briefs"
)

// cacheBegin starts a new per-operation block cache.  Call before a mutating
// operation's first metadata read; pair with flushCache (or cacheAbort) at
// the end.  Holding the global mu serializes operations, so a single cache
// is sufficient.
func (b *BrieFS) cacheBegin() {
	b.cache = make(map[uint64][]byte)
	b.cacheDirty = make(map[uint64]bool)
}

// loadBlock returns the cached working buffer for a block, reading it from
// disk on first touch.  Callers may mutate the returned slice in place; call
// saveBlock to mark it dirty.
func (b *BrieFS) loadBlock(block uint64) ([]byte, error) {
	if buf, ok := b.cache[block]; ok {
		return buf, nil
	}
	buf, err := b.dev.ReadBlock(block)
	if err != nil {
		return nil, err
	}
	b.cache[block] = buf
	return buf, nil
}

// saveBlock stores a block's working buffer in the cache and marks it dirty.
// It is idempotent and accepts the same slice loadBlock returned (in-place
// edits are already visible); the call just records the dirty bit.  Returns a
// nil error so call sites mirror the direct WriteBlock shape.
func (b *BrieFS) saveBlock(block uint64, buf []byte) error {
	b.cache[block] = buf
	b.cacheDirty[block] = true
	return nil
}

// flushCacheToDevice writes every dirty cached block to the device and
// fdatasyncs once, then drops the op cache.  This is the write-through
// variant used where the blocks must reach the disk immediately: journal
// replay at mount (before any deferral exists).  Mutating ops use
// mergeCache instead.
func (b *BrieFS) flushCacheToDevice() error {
	for block, buf := range b.cache {
		if !b.cacheDirty[block] {
			continue
		}
		if err := b.dev.WriteBlock(block, buf); err != nil {
			b.cache = nil
			b.cacheDirty = nil
			return fmt.Errorf("briefs: flush block %d: %w", block, err)
		}
	}
	if err := b.dev.Sync(); err != nil {
		b.cache = nil
		b.cacheDirty = nil
		return fmt.Errorf("briefs: flush sync: %w", err)
	}
	b.cache = nil
	b.cacheDirty = nil
	return nil
}

// mergeCache moves the operation's dirty blocks into the deferred-metadata
// map (whole-block, last-writer-wins) and drops the op cache.  No I/O: the
// blocks are written to the device by the next journal sync, after the
// commit point (MetaSyncer hook in the journal), and until then reads see
// them through the dirty view.  This is the op-end step that replaces the
// old per-op journal.Sync + flushCache (fix B: kernel parity for sync
// frequency).
func (b *BrieFS) mergeCache() {
	for block, buf := range b.cache {
		if !b.cacheDirty[block] {
			continue
		}
		b.setDirtyBlock(block, buf)
	}
	b.cache = nil
	b.cacheDirty = nil
}

// --- deferred-metadata map (the bridge's write-back cache) ---

// setDirtyBlock stores a block's content in the deferred-metadata map.  The
// caller must hold the block's inode-table shard lock (or b.mu for
// dir-op/trie blocks): dir ops hold every touched inode block's shard lock
// until the op returns, and write paths hold their file's shard lock, so a
// whole-block store can never interleave with another writer's read-modify-
// write of the same block.
func (b *BrieFS) setDirtyBlock(block uint64, buf []byte) {
	b.dirtyMu.Lock()
	if b.dirtyBlocks == nil {
		// Mount initializes the map; test harnesses that construct BrieFS
		// directly may skip that.  Lazy init keeps them working.
		b.dirtyBlocks = make(map[uint64][]byte)
	}
	b.dirtyBlocks[block] = buf
	b.dirtyMu.Unlock()
}

// dirtyView serves deferred blocks to BlockDevice.ReadBlock.  Returns a
// private copy: callers mutate the buffers they get (loadBlock's in-place
// edit pattern), and the stored slice must stay pristine so every reader
// sees a whole-block atomic snapshot.
func (b *BrieFS) dirtyView(block uint64) ([]byte, bool) {
	b.dirtyMu.Lock()
	buf, ok := b.dirtyBlocks[block]
	b.dirtyMu.Unlock()
	if !ok {
		return nil, false
	}
	out := make([]byte, len(buf))
	copy(out, buf)
	return out, true
}

// deferBlockFree returns a data block to the allocator only once its freeing
// record commits: the rel is queued in pendingFrees (applied by SyncMeta at
// journal-sync time, after the commit point), and any deferred content for
// the block is dropped — an emptied trie page or a dropped btree node is dead
// (the records re-derive the structure without it), and a stale deferred copy
// would shadow reads of — and a later drain clobber — whatever the block's
// next owner writes into it.  Freeing at op time instead, before the record
// commits, let the block be reallocated for data whose fresh content the drain
// then overwrote (the generic/040/041 free-vs-commit class, seen in
// TestCrashSlotReuseReplay: a reused trie page clobbered the new file's data).
func (b *BrieFS) deferBlockFree(abs uint64) {
	rel := abs - b.dataRegionStart
	b.dirtyMu.Lock()
	b.pendingFrees = append(b.pendingFrees, rel)
	delete(b.dirtyBlocks, abs)
	b.dirtyMu.Unlock()
}

// freeBlockNow returns a data block to the in-memory allocator immediately,
// for the paths that free right after their own journal sync committed the
// freeing records (commitExtentChange, the xattr chain rewrite).  The
// deferred-content clearing deferBlockFree does applies here too: a freed
// btree node's stale deferred copy must not shadow or clobber the block's
// next owner.
func (b *BrieFS) freeBlockNow(abs uint64) {
	b.dirtyMu.Lock()
	delete(b.dirtyBlocks, abs)
	b.dirtyMu.Unlock()
	b.dataAlloc.FreeBlock(abs - b.dataRegionStart)
}

// SyncMeta drains the deferred-metadata map to the device page cache (no
// fdatasync — the enclosing journal sync flushes) and applies the block
// frees deferred since the last commit.  Implements briefs.MetaSyncer; the
// journal calls it only after the commit point is persisted, so both the
// records justifying these blocks and the frees' JRN_EXTENT_FREE /
// JRN_TRIE_ALLOC records are durable.  Kernel parity: briefs_journal_flush_
// owned — the kernel holds freed metadata buffers owned by the journal until
// the transaction commits.  The enclosing sync then persists the allocator
// bitmaps (SyncAllocators runs after this), so a completed sync leaves the
// on-disk bitmap converged with the committed records.
func (b *BrieFS) SyncMeta() error {
	b.dirtyMu.Lock()
	frees := b.pendingFrees
	b.pendingFrees = nil
	if len(b.dirtyBlocks) == 0 {
		b.dirtyMu.Unlock()
		for _, rel := range frees {
			b.dataAlloc.FreeBlock(rel)
		}
		return nil
	}
	snap := b.dirtyBlocks
	b.dirtyBlocks = make(map[uint64][]byte)
	b.dirtyMu.Unlock()
	for _, rel := range frees {
		b.dataAlloc.FreeBlock(rel)
	}
	for block, buf := range snap {
		if err := b.dev.WriteBlock(block, buf); err != nil {
			// Restore this block and every unwritten one (the failed
			// block is still in snap; written ones were deleted) so a
			// later sync retries them.
			b.dirtyMu.Lock()
			for blk2, buf2 := range snap {
				b.dirtyBlocks[blk2] = buf2
			}
			b.dirtyMu.Unlock()
			return fmt.Errorf("briefs: drain deferred block %d: %w", block, err)
		}
		delete(snap, block)
	}
	return nil
}

// markDataDrain records that the current op left new data and/or btree node
// blocks in the device page cache whose journal records are not yet
// committed.  The journal's next sync must flush them before its commit
// point (DrainPendingData below): replay trusts the btree root pointers
// published by the INODE_FULL records, so the blocks must be on disk before
// log_end advances past those records.  Kernel parity: the kernel gets this
// ordering from the fsync path itself — file_write_and_wait_range drains the
// data before the journal sync (file.c:103-180).
func (b *BrieFS) markDataDrain() {
	b.dirtyMu.Lock()
	b.dataDrainPending = true
	b.dirtyMu.Unlock()
}

// DrainPendingData flushes the device page cache when a buffered extent op
// marked a pending drain.  Implements briefs.DataDrainer; the journal calls
// it before persisting its commit point.  The mark is CLAIMED (cleared) before
// the flush, so an op that marks concurrently with the drain re-arms it for
// the next sync — a plain read-then-clear could lose a mark that arrived
// between the flush and the clear, leaving that op's records committable
// without a data drain.  On flush failure the mark is restored so a later
// sync retries.
func (b *BrieFS) DrainPendingData() error {
	b.dirtyMu.Lock()
	pending := b.dataDrainPending
	b.dataDrainPending = false
	b.dirtyMu.Unlock()
	if !pending {
		return nil
	}
	if err := b.dev.Fdatasync(); err != nil {
		b.dirtyMu.Lock()
		b.dataDrainPending = true
		b.dirtyMu.Unlock()
		return err
	}
	return nil
}

// flushDirtyMeta drains the deferred-metadata map and fdatasyncs.  Used by
// the sync entry points that can run with a clean journal (the records were
// committed by an earlier sync but this deferred content merged after it):
// Fsync must make the deferred blocks durable even when journal.Sync is a
// no-op, and unmount must never leave blocks unsaved in daemon memory.
func (b *BrieFS) flushDirtyMeta() error {
	b.dirtyMu.Lock()
	pending := len(b.dirtyBlocks) > 0
	b.dirtyMu.Unlock()
	if !pending {
		return nil
	}
	if err := b.SyncMeta(); err != nil {
		return err
	}
	return b.dev.Fdatasync()
}

// cacheDrop removes a block from the op cache without writing it, for paths
// that free a metadata block mid-op (an emptied trie page): the block's
// content is dead and must not survive into the deferred map, where it would
// shadow reads and be drained over the block's next owner.
func (b *BrieFS) cacheDrop(block uint64) {
	delete(b.cache, block)
	delete(b.cacheDirty, block)
}

// cacheAbort drops the cache without writing (for error rollback paths).
func (b *BrieFS) cacheAbort() {
	b.cache = nil
	b.cacheDirty = nil
}

// --- cached trie node read (write path) ---

// trieRead loads a trie node's page via the cache and returns the working
// buffer plus the parsed slot.  Mutations to the buffer are visible to the
// cache; call saveBlock to persist.
func (b *BrieFS) trieRead(ref uint64) ([]byte, *briefs.TrieSlot, error) {
	block := briefs.TrieRefBlock(ref)
	slot := briefs.TrieRefSlot(ref)
	buf, err := b.loadBlock(block)
	if err != nil {
		return nil, nil, err
	}
	if _, err := briefs.ReadTriePage(buf); err != nil {
		return nil, nil, err
	}
	node, err := briefs.ReadTrieSlot(buf, uint(slot))
	if err != nil {
		return nil, nil, err
	}
	return buf, node, nil
}

// trieFindChild is the cached write-path version of TrieFindChild.
func (b *BrieFS) trieFindChild(parent uint64, byteVal byte) (uint64, error) {
	_, pnode, err := b.trieRead(parent)
	if err != nil {
		return 0, err
	}
	child, _, err := trieScanSiblings(parent, pnode.FirstChild, byteVal, func(ref uint64) (*briefs.TrieSlot, error) {
		_, cnode, rerr := b.trieRead(ref)
		return cnode, rerr
	})
	return child, err
}

// --- cached inode read/write (write path) ---

// readInodeCached reads an inode through the block cache.
func (b *BrieFS) readInodeCached(ino uint64) (*briefs.Inode, error) {
	blk, off := b.inodes.inodeLocation(ino)
	buf, err := b.loadBlock(blk)
	if err != nil {
		return nil, err
	}
	return briefs.UnmarshalInode(buf[off : off+b.inodes.sb.InodeSize])
}

// writeInodeCached marshals an inode into the cached inode-table block.
func (b *BrieFS) writeInodeCached(inode *briefs.Inode) error {
	blk, off := b.inodes.inodeLocation(inode.InodeNumber)
	buf, err := b.loadBlock(blk)
	if err != nil {
		return err
	}
	data, err := inode.MarshalBinary()
	if err != nil {
		return err
	}
	copy(buf[off:], data)
	b.saveBlock(blk, buf)
	return nil
}

// zeroInodeCached zeroes an inode slot in the cached inode-table block.
func (b *BrieFS) zeroInodeCached(ino uint64) error {
	blk, off := b.inodes.inodeLocation(ino)
	buf, err := b.loadBlock(blk)
	if err != nil {
		return err
	}
	for i := uint64(0); i < b.inodes.sb.InodeSize; i++ {
		buf[off+i] = 0
	}
	b.saveBlock(blk, buf)
	return nil
}

// writeThroughFreshInodeSlot persists a freshly allocated inode's slot to the
// device page cache immediately, bypassing the per-op cache flush.  The
// inode-allocation paths call it right before journaling the fresh inode's
// JRN_INODE_FULL: the kernel pins the inode buffer with its journal record
// (journal-owned bh lifetimes), so a committed snapshot record implies its
// slot — carrying the new generation — is already on disk.  Replay's
// generation guard (kernel 33e4019, generic/536) relies on that invariant;
// without the write-through, a mid-op crash (records committed by another
// op's sync, the slot never written) would make the guard skip restoring the
// fresh inode.  Under the FUSE crash model (kill -9; the page cache
// survives) a slot write is as durable as the journal block itself.
//
// The write is slot-granular (WriteBlockSlot), NOT a whole-block read-
// modify-write: sibling slots in the same 4096-byte inode block can hold
// uncommitted state — in the per-op cache of the running op, or deferred in
// the write-back map by earlier ops — and a whole-block write would publish
// it to the page cache ahead of the journal records justifying it.  That
// was TestCrashSlotReuseReplay's failure: unlink emptied the root trie and
// deferred the parent inode with DirTrieRoot=0; the next create's whole-block
// write-through dragged that onto disk, so a crash left the root pointing at
// no trie while the trie page stayed allocated (fsck: allocated but not
// referenced).  The kernel avoids this by construction: buffer heads are
// per-slot, so arming one slot never writes another.
//
// If the deferred map holds this block, the slot is also patched into the
// stored copy: the next drain must not regress the page cache to a version
// without the fresh slot.  The op cache is untouched, and the op's merge
// (which includes the fresh slot via writeInodeCached) supersedes the map
// entry at op end.
func (b *BrieFS) writeThroughFreshInodeSlot(in *briefs.Inode) error {
	blk, off := b.inodes.inodeLocation(in.InodeNumber)
	data, err := in.MarshalBinary()
	if err != nil {
		return err
	}
	if err := b.dev.WriteBlockSlot(blk, off, data); err != nil {
		return err
	}
	b.dirtyMu.Lock()
	if cur, ok := b.dirtyBlocks[blk]; ok {
		merged := make([]byte, len(cur))
		copy(merged, cur)
		copy(merged[off:], data)
		b.dirtyBlocks[blk] = merged
	}
	b.dirtyMu.Unlock()
	return nil
}
