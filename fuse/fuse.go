// Package fuse implements a FUSE filesystem for BrieFS.
package fuse

import (
	"context"
	"fmt"
	"sync"
	"syscall"

	"github.com/ctdk/briefs-utils/briefs"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// BrieFS is the top-level filesystem struct.
type BrieFS struct {
	dev        *BlockDevice
	sb         *briefs.SuperblockLayout
	inodes     *InodeManager
	dataAlloc  *Allocator
	inodeAlloc *Allocator
	blockSize  uint64

	// For converting data-relative blocks to absolute block numbers.
	dataRegionStart uint64

	// journal is the crash-consistency engine (ported from the kernel
	// journal.c write path). nil while the read-only bridge is in use; a
	// Phase-1 Journal is wired in Mount once the writer is implemented.
	journal *briefs.Journal

	// mu serializes directory mutation operations (create/mkdir/unlink/rmdir).
	// It protects the shared per-op block cache (cache/cacheDirty) and the
	// partial-trie-page pool (triePartials), which have no concurrency-safe
	// equivalent of the kernel's buffer cache / partial-pool mutex here, and
	// it closes the existence-check TOCTOU. File writes do NOT take mu: they
	// touch only their own inode-table block (via direct RMW under
	// inodeBlockLocks) plus the data region, so they run concurrently with dir
	// ops and with each other. The kernel lock order (briefs.h:40-71) is
	// inode_block_lock -> trie_lock -> ... ; mu stands in for trie_lock.
	mu sync.Mutex

	// triePartials is the per-superblock pool of trie pages that still have a
	// free slot, mirroring the kernel's bsi->trie_pages.partial list
	// (trie_page.c). It is consulted by trieAllocNode to reuse slots before
	// allocating fresh pages. Protected by mu.
	triePartials []uint64

	// inodeBlockLocks shards per-inode-table-block mutexes. Two ops touching
	// inodes in the same 4K inode-table block must serialize: a per-op cache
	// (dir ops) or a direct RMW (file writes) of that block would otherwise
	// clobber a sibling inode slot. Sharded by absolute table-block number so
	// ops on different table blocks run concurrently (same-block, and
	// hash-colliding, blocks serialize). This mirrors the kernel's
	// inode_block_lock (briefs.h:44), the first lock in the kernel order.
	inodeBlockLocks [64]sync.Mutex

	// Unwritten-extent metadata reservation (kernel alloc->meta_shield,
	// alloc.c:843): unwrittenRes maps inode number to that inode's unwritten
	// block count, from which the data allocator's shield is derived
	// (meta_shield.go).  In-memory only, like the kernel's binfo counters.
	// Lazily initialized; protected by shieldMu (a leaf lock).
	shieldMu     sync.Mutex
	unwrittenRes map[uint64]uint64

	// cache is the per-operation block cache (see cache.go).  A mutating
	// handler calls cacheBegin before its first metadata read and mergeCache
	// at op end to move its dirty blocks into the deferred-metadata map.
	// nil between operations.  Protected by mu.
	cache      map[uint64][]byte
	cacheDirty map[uint64]bool

	// dirtyBlocks is the deferred-metadata write-back map (cache.go): the
	// bridge's analog of the kernel's pinned, not-yet-dirty buffer heads.
	// A successful op's metadata blocks live here until the next journal
	// sync drains them (MetaSyncer hook, after the commit point); readers
	// see them through BlockDevice's dirty-view hook.  All access goes
	// through setDirtyBlock/dirtyView/SyncMeta (dirtyMu), never directly:
	// merges happen under dir-op mu / inode-block shard locks, drains under
	// the journal lock, and dirtyMu is the common leaf that orders them.
	dirtyBlocks map[uint64][]byte
	dirtyMu     sync.Mutex

	// pendingFrees holds data blocks (data-relative) whose freeing records
	// (JRN_EXTENT_FREE / JRN_TRIE_ALLOC op=1) have been journaled but not
	// yet committed.  The free is applied to the in-memory allocator only
	// when the journal commits (SyncMeta, cache.go) — the same gate
	// commitExtentChange applies to extent frees — so a freed block can
	// never be reallocated while the last committed on-disk state still
	// references it.  Guarded by dirtyMu like dirtyBlocks.
	pendingFrees []uint64

	// dataDrainPending is set by buffered extent ops (commitExtentChange and
	// friends, file_ops.go): new data and btree node blocks are in the device
	// page cache while their journal records wait uncommitted.  The next
	// journal sync drains them BEFORE its commit point (DataDrainer hook,
	// journal_write.go) because replay trusts btree root pointers without
	// re-deriving node contents.  Guarded by dirtyMu; cleared by the drain.
	dataDrainPending bool

	// Replay-private maps (journal_replay.go). Non-nil only during
	// replayJournal; nil otherwise.
	xattrFinal map[uint64]uint64 // ino -> final xattr_offset (last JRN_INODE_FULL wins)
	xattrNext  map[uint64]uint64 // phys xattr block -> next_block link
	xattrLive  map[uint64]bool   // phys xattr blocks still referenced by a final chain
	inReplay   bool              // true during replayJournal (trie page-init logging/gating)
	// trieSeeded tracks directories whose partial-page pool has been seeded
	// from the on-disk trie during replay (trieSeedPool — the kernel's
	// generic/475 briefs_trie_seed_pool fix, ported for the 073-family
	// replay ENOSPC). Non-nil only during replayJournal; nil otherwise.
	trieSeeded map[uint64]bool

	// replayParents is the replay-side copy of each directory's disk inode
	// that replayDirUpdate mutates — the port of the kernel's iget-cached
	// binfo->disk_inode (generic/534): the first DIR_UPDATE touching a parent
	// reads its block once, and every later re-derivation for that parent in
	// the window runs against that same copy.  JRN_INODE_FULL snapshot
	// restores write the raw block directly (replayInodeFull) and must NOT
	// re-point the cached copy: a rename that empties a directory trie frees
	// the old root and journals a fresh one, so re-reading the block per
	// record applies the window's dir-delete to the stale pre-rename root
	// while the re-derived add lands on the final root — the old name
	// reappears after replay (observed as generic/534 "file name 'foo'
	// still exists").  Non-nil only during replayJournal; nil otherwise.
	replayParents map[uint64]*briefs.Inode

	// replayTrieBlocks holds the data-relative blocks the journal's
	// JRN_TRIE_ALLOC records named, in journal order (pass 1), for pass-2's
	// triePageInit to reuse LIFO instead of re-allocating — the kernel's
	// replay_trie_blocks pool (journal.c:2082, generic/475): re-allocating
	// ENOSPCs on a full fs (the recorded blocks are pass-1-reserved and so
	// invisible to AllocBlock) and risks aliasing a later-reserved data
	// block.  Popping tail-first consumes the unsynced-tail orphans before
	// the synced (still-referenced) blocks, so re-derivation never clobbers
	// a -EEXIST record's page.  Empty (not nil) outside replayJournal.
	replayTrieBlocks []uint64

	// extentTrees caches the walked on-disk extent index of tree-backed
	// inodes (extent_rebuild.go), keyed by inode number: the localized
	// rebuild diffs per-leaf chunks against it, so without the cache every
	// WRITE request would re-walk and CRC-verify the whole B+tree (for a
	// 30 MB fragmented file ~65 node blocks per 512-byte request — the
	// remaining CPU sink of generic/074 after the write amplification fix).
	// Entries carry (root, total) captured from the inode at walk time;
	// collectExtentTree validates against the freshly-read inode, and
	// every rebuild allocates fresh root blocks, so a mutation by any
	// other path is detected on the next lookup.  Full rebuilds
	// (rebuildExtentIndex) also invalidate eagerly.  Entries are shared
	// read-only: the write path clones the extent list before its
	// in-place insert mutations.  Bounded by extentTreeCacheMax; guarded
	// by extentTreesMu (the map itself — entry contents are only touched
	// under the inode's inodeBlockLock, which serializes all ops on one
	// inode).
	extentTrees   map[uint64]*extentTree
	extentTreesMu sync.Mutex

	// readOnly is set after a post-journal (phase-2) write error leaves the
	// journal with uncommitted records referencing in-flight allocations.
	// Further mutations are refused (EROFS) so a later Sync cannot commit them
	// against a partially-applied state. Protected by mu.
	readOnly bool
}

// MountOptions configures the FUSE mount.
type MountOptions struct {
	MountPoint string
	Debug      bool
}

// Mount mounts a BrieFS image at the given mount point.
func Mount(imagePath string, opts MountOptions) error {
	dev, blockSize, err := OpenBlockDevice(imagePath)
	if err != nil {
		return fmt.Errorf("open device: %w", err)
	}

	// Read superblock
	sb, err := readSuperblock(dev)
	if err != nil {
		dev.Close()
		return fmt.Errorf("read superblock: %w", err)
	}

	// Open data block allocator
	dataAlloc, err := OpenAllocator(dev, sb.TrieNodePoolStart)
	if err != nil {
		dev.Close()
		return fmt.Errorf("open data allocator: %w", err)
	}

	// Open inode allocator
	inodeAlloc, err := OpenAllocator(dev, sb.InodeBMOffset)
	if err != nil {
		dev.Close()
		return fmt.Errorf("open inode allocator: %w", err)
	}

	dataRegionStart := sb.TrieNodePoolStart + sb.TrieNodePoolSize

	bfs := &BrieFS{
		dev:             dev,
		sb:              sb,
		inodes:          NewInodeManager(dev, sb),
		dataAlloc:       dataAlloc,
		inodeAlloc:      inodeAlloc,
		blockSize:       blockSize,
		dataRegionStart: dataRegionStart,
		dirtyBlocks:     make(map[uint64][]byte),
		extentTrees:     make(map[uint64]*extentTree),
	}

	// Deferred-metadata reads: blocks held in dirtyBlocks (daemon memory
	// until the next journal sync) are served to every reader through the
	// device's dirty-view hook, so lookups/readdirs stay coherent without
	// per-op writes.  Wired before replay + serving; the map is empty until
	// the first mutating op.
	dev.SetDirtyView(bfs.dirtyView)

	// Construct the journal (ported from kernel journal.c).  It mutates sb in
	// place and writes through dev.  The allocator interface lets it sync the
	// bitmaps / refresh free counts at checkpoint without an import cycle,
	// and the meta syncer drains the deferred-metadata map at commit time.
	journal, err := briefs.NewJournal(sb, dev.File(), blockSize)
	if err != nil {
		dev.Close()
		return fmt.Errorf("init journal: %w", err)
	}
	journal.SetAllocatorSyncer(bfs)
	journal.SetMetaSyncer(bfs)
	journal.SetDataDrainer(bfs)
	journal.SetWBFlusher(dev)
	bfs.journal = journal

	// Deferred-free reclaim: a failed data allocation commits the journal
	// once and retries when frees are pending (cache.go reclaimPendingFrees).
	// Only the data allocator sees deferred frees; the inode allocator never
	// has a free path.
	dataAlloc.reclaim = bfs.reclaimPendingFrees

	// Replay the journal before serving: a crash (or dm-flakey simulated power
	// failure) leaves a live range [log_start, log_end) that re-derives
	// directory tries, restores inode/symlink/xattr blocks, and reserves
	// allocator bitmap bits, leaving a consistent on-disk state. Mirrors the
	// kernel's briefs_journal_replay() at mount. A clean journal (log_start ==
	// log_end) is a no-op.
	if err := bfs.replayJournal(); err != nil {
		dev.Close()
		return fmt.Errorf("journal replay: %w", err)
	}

	root := &brieFSNode{
		bfs: bfs,
		ino: sb.RootIno,
	}

	server, err := fs.Mount(opts.MountPoint, root, &fs.Options{
		MountOptions: fuse.MountOptions{
			Name:  "briefs",
			Debug: opts.Debug,
			// FsName becomes the mount source (first column of
			// /proc/mounts).  xfstests verifies a mount with
			// `findmnt -S <dev>`, matching by source; without this the
			// FUSE mount's source is the subtype name ("briefs"), so
			// findmnt finds nothing and xfstests treats the device as
			// unmounted (_check_if_dev_already_mounted -> _exit 1).
			// Setting FsName to the backing device path makes the source
			// match, like the kernel mount (/dev/vdb1 ... briefs).
			FsName: imagePath,
			// Negotiate FUSE_POSIX_ACL, the bridge's stand-in for the
			// kernel's SB_POSIXACL (super.c, ACL feature a113c76).  The
			// kernel then evaluates ACL permissions itself from the
			// system.posix_acl_access xattr (fetched through the
			// ordinary Getxattr path) and setfacl writes the canonical
			// blob through Setxattr -- no namespace filtering to lift:
			// the bridge already stores any name verbatim, and ACLs on
			// the on-disk format ARE ordinary prefixed xattrs
			// (briefs-xattr/acl parity), so kernel-created and
			// fuse-created ACLs interoperate.
			EnableAcl: true,
		},
		// Report modes exactly as stored.  Without this, go-fuse patches
		// any zero-permission mode in Getattr/Lookup replies to 0644 (+0111
		// for dirs) -- so `chmod 000` files lied about their mode, and a
		// zero-perm whiteout looked like rwxr-xr-x (generic/585).
		NullPermissions: true,
		// The root is never produced by a Lookup, so without this its
		// stableAttr.Ino is 0 and stat reports ino 0 (and ".." from the
		// root resolves to a go-fuse virtual inode).  Pin it to the real
		// BrieFS root ino for parity with the kernel module.
		RootStableAttr: &fs.StableAttr{
			Ino:  sb.RootIno,
			Mode: uint32(briefs.ModeDir),
		},
	})
	if err != nil {
		dev.Close()
		return fmt.Errorf("mount: %w", err)
	}

	server.Wait()

	// Unmount: always checkpoint (f8ef293) so log_start==log_end and a remount
	// replays nothing, then flush + close.  Checkpoint errors are best-effort
	// (the journal may be dirty on a forced unmount); we still drain + close.
	// flushDirtyMeta is the safety net for the case the checkpoint's inner
	// sync skipped because the journal was clean: deferred metadata can be
	// pending with no uncommitted records (its records were committed by an
	// earlier sync, the blocks merged after it), and that content exists only
	// in daemon memory until this drain.
	if bfs.journal != nil {
		_ = bfs.journal.Checkpoint()
		_ = bfs.journal.Close()
	}
	_ = bfs.flushDirtyMeta()
	_ = dev.Sync()
	dev.Close()
	return nil
}

// RefreshFreeCounts updates the in-memory superblock free counts from the
// authoritative allocator free counts.  Implements briefs.AllocatorSyncer;
// called by the journal before persisting the superblock.  Cheap; no I/O.
func (b *BrieFS) RefreshFreeCounts() {
	b.sb.FreeDataBlks = b.dataAlloc.FreeCount()
	b.sb.FreeInodes = b.inodeAlloc.FreeCount()
}

// SyncAllocators writes both allocator bitmap pools to disk, then rewrites
// the superblock so its free counts match the pools.  Implements
// briefs.AllocatorSyncer; the journal calls it after the commit point and
// after the deferred-metadata drain, so the bitmap it writes reflects the
// frees SyncMeta just applied — never ahead of their records or of the
// drained structure references (generic/040/041 family).  The superblock
// rewrite is needed because the commit point's own superblock write
// (syncSuperblock at the log_end persist) runs before those frees apply; a
// completed sync must leave the on-disk superblock and pool in agreement or
// fsck reports a free-count mismatch.  Mirrors the kernel's
// briefs_alloc_sync() pair (journal.c:394: flush metadata, then sync
// allocators).
func (b *BrieFS) SyncAllocators() error {
	if err := b.dataAlloc.Sync(); err != nil {
		return fmt.Errorf("sync data allocator: %w", err)
	}
	if err := b.inodeAlloc.Sync(); err != nil {
		return fmt.Errorf("sync inode allocator: %w", err)
	}
	b.RefreshFreeCounts()
	buf := make([]byte, b.blockSize)
	data, err := b.sb.MarshalBinary()
	if err != nil {
		return fmt.Errorf("marshal superblock: %w", err)
	}
	copy(buf, data)
	if err := b.dev.WriteBlock(0, buf); err != nil {
		return fmt.Errorf("write superblock: %w", err)
	}
	return nil
}

// inodeBlockShard returns the shard index and its mutex for the inode-table
// block holding @ino. Two ops touching inodes whose slots share a shard (the
// common case: the same 4K table block; also the rare hash-collision of two
// different table blocks) must hold that shard's mutex so they do not clobber a
// sibling slot. Sharding by absolute block number lets ops on different shards
// run concurrently.
func (b *BrieFS) inodeBlockShard(ino uint64) (uint64, *sync.Mutex) {
	blk, _ := b.inodes.inodeLocation(ino)
	shard := blk % uint64(len(b.inodeBlockLocks))
	return shard, &b.inodeBlockLocks[shard]
}

// inodeBlockLock returns the mutex guarding the inode-table block holding @ino.
// Used by file writes, which take a single shard lock for their whole op.
func (b *BrieFS) inodeBlockLock(ino uint64) *sync.Mutex {
	_, m := b.inodeBlockShard(ino)
	return m
}

// lockOtherInodeBlock locks the shard for childIno unless it is the same shard
// as parentIno (in which case the caller already holds that shard's mutex and
// re-locking would self-deadlock). Returns the locked mutex, or nil if no new
// lock was taken. Used by dir ops, which already hold the parent's shard lock
// and need to additionally cover the child's block; the global dir lock
// serializes dir ops, and file writes take only a single shard lock and never
// wait for a second, so the parent-then-child order cannot deadlock.
func (b *BrieFS) lockOtherInodeBlock(parentIno, childIno uint64) *sync.Mutex {
	pShard, _ := b.inodeBlockShard(parentIno)
	cShard, cLock := b.inodeBlockShard(childIno)
	if cShard == pShard {
		return nil
	}
	cLock.Lock()
	return cLock
}

// lockInodeBlockUnlessHeld locks @ino's shard unless that shard is one of the
// shards this goroutine already holds via lockInodeShards(@held). Go mutexes
// are not reentrant, so re-locking a held shard self-deadlocks: the op blocks
// forever on a mutex it itself owns, wedging every later client of that shard.
// Needed where an op holds SEVERAL shards and then allocates a fresh inode
// (rename whiteout): the fresh slot's table block can share a shard with any
// of the held ones -- not just the parent, which is all lockOtherInodeBlock
// dedups against (generic/013 fsstress wedge, 2026-09-07: the fresh whiteout
// landed in the moved inode's shard).
func (b *BrieFS) lockInodeBlockUnlessHeld(held []uint64, ino uint64) *sync.Mutex {
	s, m := b.inodeBlockShard(ino)
	for _, h := range held {
		if hs, _ := b.inodeBlockShard(h); hs == s {
			return nil
		}
	}
	m.Lock()
	return m
}

// lockInodeShards locks the (deduplicated) shards for a set of inodes in
// ascending shard order and returns a cleanup that unlocks in reverse. Used by
// multi-inode dir ops (rename), which also hold the global dir lock, so no two
// dir ops hold these concurrently; the ascending order + dedup avoids self-
// deadlock when several inodes share a shard.
func (b *BrieFS) lockInodeShards(inos []uint64) func() {
	seen := map[uint64]*sync.Mutex{}
	var shards []uint64
	for _, ino := range inos {
		s, m := b.inodeBlockShard(ino)
		if _, ok := seen[s]; !ok {
			seen[s] = m
			shards = append(shards, s)
		}
	}
	sortShards(shards)
	for _, s := range shards {
		seen[s].Lock()
	}
	return func() {
		for i := len(shards) - 1; i >= 0; i-- {
			seen[shards[i]].Unlock()
		}
	}
}

func sortShards(s []uint64) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// readSuperblock reads and parses the superblock from block 0.
func readSuperblock(dev *BlockDevice) (*briefs.SuperblockLayout, error) {
	return briefs.ReadSuperblock(dev, dev.BlockSize())
}

// brieFSNode implements the FUSE inode operations for BrieFS.
type brieFSNode struct {
	fs.Inode
	bfs *BrieFS
	ino uint64
}

// Ensure brieFSNode implements the required interfaces.
var _ = (fs.NodeGetattrer)((*brieFSNode)(nil))
var _ = (fs.NodeLookuper)((*brieFSNode)(nil))
var _ = (fs.NodeReaddirer)((*brieFSNode)(nil))
var _ = (fs.NodeOpener)((*brieFSNode)(nil))
var _ = (fs.NodeReader)((*brieFSNode)(nil))
var _ = (fs.NodeStatfser)((*brieFSNode)(nil))
var _ = (fs.NodeCreater)((*brieFSNode)(nil))
var _ = (fs.NodeMkdirer)((*brieFSNode)(nil))
var _ = (fs.NodeUnlinker)((*brieFSNode)(nil))
var _ = (fs.NodeRmdirer)((*brieFSNode)(nil))
var _ = (fs.NodeWriter)((*brieFSNode)(nil))
var _ = (fs.NodeFsyncer)((*brieFSNode)(nil))
var _ = (fs.NodeGetxattrer)((*brieFSNode)(nil))
var _ = (fs.NodeSetxattrer)((*brieFSNode)(nil))
var _ = (fs.NodeListxattrer)((*brieFSNode)(nil))
var _ = (fs.NodeRemovexattrer)((*brieFSNode)(nil))
var _ = (fs.NodeIoctler)((*brieFSNode)(nil))
var _ = (fs.NodeLinker)((*brieFSNode)(nil))
var _ = (fs.NodeSymlinker)((*brieFSNode)(nil))
var _ = (fs.NodeMknoder)((*brieFSNode)(nil))
var _ = (fs.NodeRenamer)((*brieFSNode)(nil))
var _ = (fs.NodeReadlinker)((*brieFSNode)(nil))
var _ = (fs.NodeAllocater)((*brieFSNode)(nil))
var _ = (fs.NodeSetattrer)((*brieFSNode)(nil))

func (n *brieFSNode) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	diskInode, err := n.bfs.inodes.ReadInode(n.ino)
	if err != nil {
		return syscall.EIO
	}
	if err := n.fillAttrOut(out, diskInode); err != nil {
		return syscall.EIO
	}
	return 0
}

func (n *brieFSNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	diskInode, err := n.bfs.inodes.ReadInode(n.ino)
	if err != nil {
		return nil, syscall.EIO
	}

	ino, _, err := TrieLookup(n.bfs.dev, diskInode.DirTrieRoot, name)
	if err != nil {
		return nil, syscall.ENOENT
	}

	childInode, err := n.bfs.inodes.ReadInode(ino)
	if err != nil {
		return nil, syscall.EIO
	}

	if err := n.fillEntryOut(out, childInode); err != nil {
		return nil, syscall.EIO
	}

	// The on-disk inode's mode is authoritative for the node type, like
	// the kernel's briefs_lookup (which derives everything from the inode
	// it igets).  go-fuse masks StableAttr.Mode down to the S_IFMT type
	// bits (newInodeUnlocked), so passing the full Filemode is safe -- and
	// required: the trie's ftype is S_IFMT>>12, where 2 means CHARDEV, but
	// briefs.NodeTypeDir happens to be 0x02 too.  Switching on ftype
	// turned every whiteout into a directory, so rm -rf could never
	// remove it (generic/585).
	childNode := &brieFSNode{
		bfs: n.bfs,
		ino: ino,
	}
	child := n.NewInode(ctx, childNode, fs.StableAttr{
		Ino:  ino,
		Mode: childInode.Filemode,
	})

	return child, 0
}

func (n *brieFSNode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	diskInode, err := n.bfs.inodes.ReadInode(n.ino)
	if err != nil {
		return nil, syscall.EIO
	}

	var entries []fuse.DirEntry
	dirMode := uint32(briefs.ModeDir)

	// BrieFS does not store "." or ".." as trie entries. Like the kernel's
	// dir_emit_dots, synthesize them here so `ls -a` (and empty directories)
	// show them. ".." is the parent directory; the root's parent is itself.
	parentIno := n.ino
	if !n.IsRoot() {
		if _, p := n.Parent(); p != nil {
			if pn, ok := p.Operations().(*brieFSNode); ok && pn.ino != 0 {
				parentIno = pn.ino
			}
		}
	}
	entries = append(entries, fuse.DirEntry{Mode: dirMode, Ino: n.ino, Name: "."})
	entries = append(entries, fuse.DirEntry{Mode: dirMode, Ino: parentIno, Name: ".."})

	if diskInode.DirTrieRoot == 0 {
		return fs.NewListDirStream(entries), 0
	}

	iter := NewTrieIterator(n.bfs.dev, diskInode.DirTrieRoot)

	for {
		ino, ftype, name, err := iter.Next()
		if err != nil {
			break
		}
		if ino == 0 {
			break
		}
		// ftype is S_IFMT >> 12 (4=dir, 8=reg, 10=symlink, 2=chardev, ...).
		// The kernel's readdir (dir.c:139) maps every dirent type with
		// file_type = (entry_type << 12) & S_IFMT; do the same rather than
		// switch on a few known values, so chardev/blockdev/fifo/socket
		// d_types match the kernel (a wrong d_type confuses coreutils and
		// libc readdir consumers, and misclassifies whiteouts).
		mode := uint32(ftype) << 12
		entries = append(entries, fuse.DirEntry{
			Mode: mode,
			Ino:  ino,
			Name: name,
		})
	}

	return fs.NewListDirStream(entries), 0
}

func (n *brieFSNode) Open(ctx context.Context, flags uint32) (fh fs.FileHandle, fuseFlags uint32, errno syscall.Errno) {
	// No per-handle state: reads and writes are served straight from the
	// device by Read/Write below, so a null FileHandle works. KEEP_CACHE
	// lets the kernel keep read-ahead data across opens.
	return nil, fuse.FOPEN_KEEP_CACHE, 0
}

func (n *brieFSNode) Read(ctx context.Context, f fs.FileHandle, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	data, err := n.bfs.readFileData(n.ino, dest, off)
	if err != nil {
		return nil, syscall.EIO
	}
	return fuse.ReadResultData(data), 0
}

func (n *brieFSNode) Statfs(ctx context.Context, out *fuse.StatfsOut) syscall.Errno {
	out.Blocks = n.bfs.sb.DataBlocks
	// f_bfree/f_bavail exclude the meta_shield: those blocks are reserved
	// for the future B+tree metadata of outstanding unwritten-extent
	// reservations, not available to data writers (kernel super.c:799-804).
	// Blocks with journal-recorded but uncommitted frees count as free: a
	// writer reclaims them by committing the journal (reclaimPendingFrees),
	// and the kernel gets the same df behavior from its commit thread and
	// wired sync(2), neither of which a FUSE mount has (inode.c:1742).
	out.Bfree = n.bfs.dataAlloc.FreeCountDataPlus(n.bfs.pendingFreeCount())
	out.Bavail = out.Bfree
	out.Files = n.bfs.inodeAlloc.TotalBlocks()
	out.Ffree = n.bfs.inodeAlloc.FreeCount()
	out.Bsize = uint32(n.bfs.blockSize)
	out.NameLen = uint32(briefs.BrieFSMaxNameLen)
	return 0
}

// errToErrno maps a Go error to a FUSE errno.  syscall.Errno values pass through
// unchanged; anything else is treated as an I/O error.
func errToErrno(err error) syscall.Errno {
	if err == nil {
		return 0
	}
	if e, ok := err.(syscall.Errno); ok {
		return e
	}
	return syscall.EIO
}

// callerCreds extracts the invoking process's uid/gid from the FUSE request
// context.  On failure (no caller attached) it defaults to root.
func callerCreds(ctx context.Context) (uid, gid uint32) {
	if c, ok := fuse.FromContext(ctx); ok && c != nil {
		return c.Uid, c.Gid
	}
	return 0, 0
}

// fillEntryOut populates a FUSE EntryOut from an on-disk inode, including the
// block count derived from its extents. The returned error is the extent-walk
// failure, if any: read-only callers propagate it (EIO); post-mutation
// callers ignore it deliberately — the on-disk change already happened, so
// the entry is reported with a best-effort block count.
func (n *brieFSNode) fillEntryOut(out *fuse.EntryOut, in *briefs.Inode) error {
	out.Mode = in.Filemode
	out.Size = in.FileSize
	out.Uid = in.Uid
	out.Gid = in.Gid
	out.Nlink = in.Nlinks
	out.Atime = in.AtimeSec
	out.Atimensec = uint32(in.AtimeNsec)
	out.Mtime = in.MtimeSec
	out.Mtimensec = uint32(in.MtimeNsec)
	out.Ctime = in.CtimeSec
	out.Ctimensec = uint32(in.CtimeNsec)

	totalBlocks, err := n.bfs.extentBlocksOf(in)
	out.Blocks = totalBlocks * (n.bfs.blockSize / 512)
	out.Blksize = uint32(n.bfs.blockSize)
	return err
}

// newChildNode wraps a freshly created inode in a go-fuse Inode linked to its
// parent (this node).
func (n *brieFSNode) newChildNode(ctx context.Context, in *briefs.Inode) *fs.Inode {
	child := &brieFSNode{bfs: n.bfs, ino: in.InodeNumber}
	return n.NewInode(ctx, child, fs.StableAttr{
		Ino:  in.InodeNumber,
		Mode: in.Filemode,
	})
}

// Create handles O_CREAT.  Mirrors briefs_create (dir.c:369).
func (n *brieFSNode) Create(ctx context.Context, name string, flags, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	uid, gid := callerCreds(ctx)
	child, err := n.bfs.createInDir(n.ino, name, briefs.ModeFile|mode, uid, gid, flags&syscall.O_EXCL != 0)
	if err != nil {
		return nil, nil, 0, errToErrno(err)
	}
	n.fillEntryOut(out, child)
	return n.newChildNode(ctx, child), nil, fuse.FOPEN_KEEP_CACHE, 0
}

// Mkdir handles directory creation.  Mirrors briefs_mkdir (dir.c:414).
func (n *brieFSNode) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	uid, gid := callerCreds(ctx)
	child, err := n.bfs.createInDir(n.ino, name, briefs.ModeDir|mode, uid, gid, false)
	if err != nil {
		return nil, errToErrno(err)
	}
	n.fillEntryOut(out, child)
	return n.newChildNode(ctx, child), 0
}

// Unlink handles file removal.  Mirrors briefs_unlink (dir.c:680).
func (n *brieFSNode) Unlink(ctx context.Context, name string) syscall.Errno {
	return errToErrno(n.bfs.unlinkInDir(n.ino, name, false))
}

// Rmdir handles directory removal.  Mirrors briefs_rmdir (dir.c:702).
func (n *brieFSNode) Rmdir(ctx context.Context, name string) syscall.Errno {
	return errToErrno(n.bfs.unlinkInDir(n.ino, name, true))
}

// Write handles file data writes.  Mirrors briefs_write_iter (file.c:640):
// inline-data for small files, extent-backed per-block RMW + hole allocation
// otherwise, with drain-before-snapshot durability (see file_ops.go).
// Locking is per inode-table block (inodeBlockLocks), not the global dir lock,
// so writes to files in different inode blocks run concurrently.
func (n *brieFSNode) Write(ctx context.Context, f fs.FileHandle, data []byte, off int64) (uint32, syscall.Errno) {
	nwritten, err := n.bfs.writeFileData(n.ino, data, off)
	if err != nil {
		return 0, errToErrno(err)
	}
	return uint32(nwritten), 0
}

// Fsync flushes file data and metadata to disk.  Mirrors the kernel fsync
// path (file.c: briefs_file_fsync): commit the journal — which drains the
// buffered data before the commit point and the deferred-metadata map after
// it, each to writeback completion — then, for the case the journal was
// already clean but deferred blocks merged after the last commit, drain those
// too, and finish with ONE device cache flush for power-fail parity with the
// kernel's blkdev_issue_flush (82c9a61).  The old leading dev.Sync was a
// second whole-device fsync per fsync, on top of the per-commit ones — the
// fsx-family hang ladder.  Fsync takes no lock: the journal, the device
// flush, and the deferred-map drain are internally synchronized, and Fsync
// mutates nothing.
func (n *brieFSNode) Fsync(ctx context.Context, f fs.FileHandle, flags uint32) syscall.Errno {
	if n.bfs.readOnly {
		return syscall.EROFS
	}
	if err := n.bfs.journal.Sync(false); err != nil {
		return syscall.EIO
	}
	if err := n.bfs.flushDirtyMeta(); err != nil {
		return syscall.EIO
	}
	if err := n.bfs.dev.Fdatasync(); err != nil {
		return syscall.EIO
	}
	return 0
}

// Getxattr returns an xattr value. With a zero-length dest it reports the size;
// with a too-small dest it returns ERANGE. Mirrors briefs_xattr_get (xattr.c:320).
func (n *brieFSNode) Getxattr(ctx context.Context, name string, dest []byte) (uint32, syscall.Errno) {
	val, err := n.bfs.getXattr(n.ino, name)
	if err != nil {
		return 0, errToErrno(err)
	}
	if len(dest) == 0 {
		return uint32(len(val)), 0
	}
	if len(dest) < len(val) {
		return uint32(len(val)), syscall.ERANGE
	}
	copy(dest, val)
	return uint32(len(val)), 0
}

// Setxattr sets/replaces an xattr (value != nil) or removes it (value == nil).
// Mirrors briefs_xattr_set (xattr.c:925).
func (n *brieFSNode) Setxattr(ctx context.Context, name string, data []byte, flags uint32) syscall.Errno {
	return errToErrno(n.bfs.setXattr(n.ino, name, data, flags))
}

// Listxattr lists all xattr names (NUL-separated). With a zero-length dest it
// reports the needed size; with a too-small dest it returns ERANGE + the size.
func (n *brieFSNode) Listxattr(ctx context.Context, dest []byte) (uint32, syscall.Errno) {
	names, err := n.bfs.listXattr(n.ino)
	if err != nil {
		return 0, errToErrno(err)
	}
	total := 0
	for _, name := range names {
		total += len(name) + 1
	}
	if len(dest) == 0 {
		return uint32(total), 0
	}
	if len(dest) < total {
		return uint32(total), syscall.ERANGE
	}
	off := 0
	for _, name := range names {
		copy(dest[off:], name)
		off += len(name)
		dest[off] = 0
		off++
	}
	return uint32(total), 0
}

// Removexattr removes an xattr. Mirrors briefs_xattr_set with value == nil.
func (n *brieFSNode) Removexattr(ctx context.Context, name string) syscall.Errno {
	return errToErrno(n.bfs.removeXattr(n.ino, name))
}

// Ioctl handles the mount-level FITRIM / FS_IOC_{GET,SET}FSLABEL (ioctlMount,
// mirroring the superblock-scoped cases of the kernel's briefs_ioctl) and the
// per-inode FS_IOC_GETFLAGS/SETFLAGS (chattr/lsattr) and
// FS_IOC_FSGETXATTR/FSSETXATTR (xfs_io/statx). Unknown ioctls return ENOTTY.
func (n *brieFSNode) Ioctl(ctx context.Context, f fs.FileHandle, cmd uint32, arg uint64, input []byte, output []byte) (int32, syscall.Errno) {
	if r, errno, handled := n.bfs.ioctlMount(ctx, cmd, input, output); handled {
		return r, errno
	}
	return n.bfs.ioctlFileattr(n.ino, cmd, input, output)
}

// Link creates a hard link. Mirrors briefs_link (dir.c:432).
func (n *brieFSNode) Link(ctx context.Context, target fs.InodeEmbedder, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	src, ok := target.(*brieFSNode)
	if !ok {
		return nil, syscall.EINVAL
	}
	in, err := n.bfs.linkInDir(n.ino, name, src.ino)
	if err != nil {
		return nil, errToErrno(err)
	}
	n.fillEntryOut(out, in)
	return n.newChildNode(ctx, in), 0
}

// Symlink creates a symbolic link. Mirrors briefs_symlink (file.c:2160).
func (n *brieFSNode) Symlink(ctx context.Context, target, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	uid, gid := callerCreds(ctx)
	in, err := n.bfs.symlinkInDir(n.ino, name, target, uid, gid)
	if err != nil {
		return nil, errToErrno(err)
	}
	n.fillEntryOut(out, in)
	return n.newChildNode(ctx, in), 0
}

// Mknod creates a special file. Mirrors briefs_mknod (file.c:2265).
func (n *brieFSNode) Mknod(ctx context.Context, name string, mode, dev uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	uid, gid := callerCreds(ctx)
	in, err := n.bfs.mknodInDir(n.ino, name, mode, uid, gid, uint64(dev))
	if err != nil {
		return nil, errToErrno(err)
	}
	n.fillEntryOut(out, in)
	return n.newChildNode(ctx, in), 0
}

// Rename renames a directory entry, dispatching on the renameat2 flags
// (EXCHANGE / WHITEOUT / NOREPLACE). Mirrors briefs_rename (dir.c:1162).
func (n *brieFSNode) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	np, ok := newParent.(*brieFSNode)
	if !ok {
		return syscall.EINVAL
	}
	return errToErrno(n.bfs.renameInDir(n.ino, name, np.ino, newName, flags))
}

// Readlink returns the target of a symlink. Mirrors briefs_get_link.
func (n *brieFSNode) Readlink(ctx context.Context) ([]byte, syscall.Errno) {
	target, err := n.bfs.readSymlink(n.ino)
	if err != nil {
		return nil, errToErrno(err)
	}
	return []byte(target), 0
}

// fillAttrOut populates a FUSE AttrOut from an on-disk inode, including the
// block count derived from its extents. Shared by Getattr and Setattr; the
// returned error is the extent-walk failure, if any (callers return EIO).
func (n *brieFSNode) fillAttrOut(out *fuse.AttrOut, in *briefs.Inode) error {
	out.Mode = in.Filemode
	out.Size = in.FileSize
	out.Uid = in.Uid
	out.Gid = in.Gid
	out.Nlink = in.Nlinks
	out.Atime = in.AtimeSec
	out.Atimensec = uint32(in.AtimeNsec)
	out.Mtime = in.MtimeSec
	out.Mtimensec = uint32(in.MtimeNsec)
	out.Ctime = in.CtimeSec
	out.Ctimensec = uint32(in.CtimeNsec)
	totalBlocks, err := n.bfs.extentBlocksOf(in)
	out.Blocks = totalBlocks * (n.bfs.blockSize / 512)
	out.Blksize = uint32(n.bfs.blockSize)
	return err
}

// Setattr handles chmod/chown/utimes/truncate. Mirrors briefs_setattr
// (file.c:836) via setattrOp (killpriv + truncate + metadata).
func (n *brieFSNode) Setattr(ctx context.Context, f fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	req := fuseSetAttrIn{
		valid:     in.Valid,
		size:      in.Size,
		mode:      in.Mode,
		uid:       in.Uid,
		gid:       in.Gid,
		atime:     in.Atime,
		mtime:     in.Mtime,
		ctime:     in.Ctime,
		atimensec: in.Atimensec,
		mtimensec: in.Mtimensec,
		ctimensec: in.Ctimensec,
	}
	if err := n.bfs.setattrOp(n.ino, &req); err != nil {
		return errToErrno(err)
	}
	di, err := n.bfs.inodes.ReadInode(n.ino)
	if err != nil {
		return syscall.EIO
	}
	if err := n.fillAttrOut(out, di); err != nil {
		return syscall.EIO
	}
	return 0
}

// Allocate handles fallocate. Mirrors briefs_fallocate (file.c:3459) with
// all five modes: preallocate (unwritten extents), PUNCH_HOLE, ZERO_RANGE,
// COLLAPSE_RANGE, and INSERT_RANGE.
func (n *brieFSNode) Allocate(ctx context.Context, f fs.FileHandle, off, size uint64, mode uint32) syscall.Errno {
	return errToErrno(n.bfs.fallocateOp(n.ino, off, size, mode))
}
