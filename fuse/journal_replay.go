// Package fuse: journal replay at mount.
//
// This is a Go port of the kernel's briefs_journal_replay() (journal.c:1671)
// and its helpers (walk_journal, replay_dir_update, replay_inode_full,
// replay_inode_update, replay_extent_alloc/free, replay_trie_alloc,
// replay_symlink_data, replay_xattr_data, replay_reconcile_nlinks).  It gives
// the FUSE bridge the same crash-recovery path the kernel module has: after a
// crash (or a dm-flakey simulated power failure), a remount replays the live
// journal range [log_start, log_end) to re-derive directory tries, restore
// inode/symlink/xattr blocks, and reserve allocator bitmap bits, leaving a
// consistent on-disk state.
//
// The bridge previously relied only on the unmount-time checkpoint
// (always-checkpoint-at-unmount, f8ef293) to leave log_start==log_end, so a
// clean remount replayed nothing.  A crash skipped that checkpoint, leaving a
// stale allocator bitmap and torn metadata (e.g. a durable directory entry
// pointing at an inode block that never reached disk) with no recovery path —
// the generic/547 failure.
//
// Three passes, matching the kernel:
//   (1) reservation pre-scan: reserve/free every block/inode claimed by an
//       ALLOC/FREE record so the in-memory allocators reflect the full
//       post-crash allocation state before any trie re-derivation runs, and
//       collect each inode's final xattr head + xattr next_block links;
//   (2) apply pass: re-derive directory tries from JRN_DIR_UPDATE, restore
//       inode/symlink/xattr blocks;
//   (3) nlink reconciliation: recompute on-disk nlinks from the re-derived
//       tries so a partial-tail crash cannot leave nlink==0 on a named inode.
//
// Trie re-derivation reuses the live TrieInsert/TrieRemove, which share one
// per-replay block cache (b.cache) so a page_init within a replay sees the
// pages earlier records in the same replay touched.  journal.WriteRecord is a
// no-op while in replay (Journal.SetInReplay), so the trie page_init/free paths
// do not append fresh JRN_TRIE_ALLOC records into the range being replayed.

package fuse

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"syscall"

	"github.com/ctdk/briefs-utils/briefs"
)

// replayDebug, when set (BRIEFS_REPLAY_DEBUG=1), logs the replay to
// /tmp/briefs-replay.log for diagnosis.
var replayDebug = os.Getenv("BRIEFS_REPLAY_DEBUG") == "1"

// rlog appends a replay debug line to /tmp/briefs-replay.log.
func rlog(format string, args ...interface{}) {
	if !replayDebug {
		return
	}
	f, err := os.OpenFile("/tmp/briefs-replay.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, format+"\n", args...)
}

// replayJournal replays the live journal range at mount. It is a no-op when the
// journal is clean (log_start == log_end). On success the journal is marked
// clean (log_start advances to log_end) and the allocator bitmaps + superblock
// are persisted.
func (b *BrieFS) replayJournal() error {
	start, end, _ := b.journal.ReplayLogRange()
	rlog("replayJournal: start=%d end=%d dirty=%v", start, end, start != end)
	if start == end {
		return nil // clean, nothing to replay
	}

	// Replay-private state.
	b.xattrFinal = make(map[uint64]uint64)
	b.xattrNext = make(map[uint64]uint64)
	b.xattrLive = make(map[uint64]bool)
	b.trieSeeded = make(map[uint64]bool)
	b.replayParents = make(map[uint64]*briefs.Inode)
	b.replayParentsPristine = make(map[uint64][]byte)
	b.replayTrieBlocks = nil
	defer func() {
		b.xattrFinal = nil
		b.xattrNext = nil
		b.xattrLive = nil
		b.trieSeeded = nil
		b.replayParents = nil
		b.replayParentsPristine = nil
		b.replayTrieBlocks = nil
	}()

	b.journal.SetInReplay(true)
	b.inReplay = true
	defer func() {
		b.journal.SetInReplay(false)
		b.inReplay = false
	}()

	// One block cache spans the whole replay so re-derivation shares buffers.
	b.cacheBegin()
	b.triePartials = nil

	// Pass 1: reserve/free allocator bits + collect xattr final/next maps.
	if err := b.walkJournal(true); err != nil {
		b.cacheAbort()
		return fmt.Errorf("briefs: replay pass 1 (reserve): %w", err)
	}

	// Build the xattr live-set from the final heads + next links.
	b.buildXattrLiveSet()

	// Pass 2: re-derive tries, restore inode/symlink/xattr blocks.
	if err := b.walkJournal(false); err != nil {
		b.cacheAbort()
		return fmt.Errorf("briefs: replay pass 2 (apply): %w", err)
	}

	// Flush all replay-dirty metadata (trie pages, inode/symlink/xattr blocks)
	// before pass 3 reads the re-derived tries back from disk.
	if err := b.flushCacheToDevice(); err != nil {
		return fmt.Errorf("briefs: replay flush: %w", err)
	}

	// Pass 3: reconcile on-disk nlinks with the re-derived tries.
	if err := b.replayReconcileNlinks(); err != nil {
		return fmt.Errorf("briefs: replay pass 3 (nlink): %w", err)
	}

	// Persist the replayed allocator bitmaps + a final metadata flush.
	if err := b.dataAlloc.Sync(); err != nil {
		return fmt.Errorf("briefs: replay sync data alloc: %w", err)
	}
	if err := b.inodeAlloc.Sync(); err != nil {
		return fmt.Errorf("briefs: replay sync inode alloc: %w", err)
	}
	if err := b.dev.Sync(); err != nil {
		return fmt.Errorf("briefs: replay final flush: %w", err)
	}

	// Mark the journal clean and persist the superblock.
	if err := b.journal.MarkCleanAfterReplay(); err != nil {
		return fmt.Errorf("briefs: replay mark clean: %w", err)
	}
	return nil
}

// walkJournal walks [log_start, log_end) once, applying each record. When
// reserveOnly is true this is the pass-1 reservation pre-scan (only allocator
// ALLOC/FREE records + xattr metadata collection apply); otherwise it is the
// full pass-2 apply. The ring-walk skeleton (cursor advance, block magic
// check, record framing) is shared with fsck through briefs.WalkJournalRing
// and briefs.IterateJournalBlockRecords; this function owns the replay
// policy (type range, checksum strictness, handler dispatch).
func (b *BrieFS) walkJournal(reserveOnly bool) error {
	start, end, checkpointBlk := b.journal.ReplayLogRange()
	journalOffset, journalBlocks := b.journal.JournalRingGeometry()
	blockSize := b.journal.JournalBlockSize()

	err := briefs.WalkJournalRing(b.journal.ReadJournalBlock, journalOffset, journalBlocks, start, end,
		func(cur uint64, buf []byte) error {
			// The reserved checkpoint block (journal_end - 1) never carries
			// replayable records: the write path does not land ordinary
			// records there, and its content may be stale from a prior mount.
			// The degenerate start == end visit (clean journal) carries none
			// either.
			if cur == checkpointBlk || start == end {
				return nil
			}

			bh := briefs.ParseJournalBlockHeader(buf)
			return briefs.IterateJournalBlockRecords(buf, blockSize, bh.RecordCount,
				func(i uint32, hdr briefs.RecordHeader, recData []byte) error {
					rtype := hdr.Type

					if rtype <= briefs.JRN_NONE || rtype >= briefs.JRN_END {
						return fmt.Errorf("invalid record type %d at block %d", rtype, cur)
					}
					rlog("rec block=%d type=%d dlen=%d", cur, rtype, len(recData))

					// Checksum verify (zero checksum = legacy, accepted).
					if !briefs.VerifyJournalRecordChecksum(rtype, hdr.Flags, recData, hdr.Checksum) {
						return fmt.Errorf("checksum mismatch at block %d record %d type %d", cur, i, rtype)
					}

					// JRN_CHECKPOINT marker records are not replay-able; skip.
					if rtype == briefs.JRN_CHECKPOINT {
						return nil
					}

					if err := b.applyRecord(rtype, recData, reserveOnly); err != nil {
						// Freed-inode / not-present results are skippable, not mount-fatal.
						if err == syscall.EINVAL || err == syscall.ENOENT {
							return nil
						}
						return fmt.Errorf("replay record type %d at block %d: %w", rtype, cur, err)
					}
					return nil
				})
		})
	// A stale/garbage block at the tail ends the walk (don't fail the mount).
	if errors.Is(err, briefs.ErrJournalBadMagic) {
		return nil
	}
	return err
}

// applyRecord dispatches one journal record to its replay handler. Mirrors the
// kernel's apply_record() (journal.c:1234).
func (b *BrieFS) applyRecord(rtype uint32, data []byte, reserveOnly bool) error {
	switch rtype {
	case briefs.JRN_DIR_UPDATE:
		if reserveOnly {
			return nil
		}
		return b.replayDirUpdate(briefs.UnmarshalDirUpdate(data))

	case briefs.JRN_INODE_ALLOC:
		ia := briefs.UnmarshalInodeAlloc(data)
		if ia != nil && ia.Ino > 0 {
			b.inodeAlloc.ReserveBlock(ia.Ino - 1)
		}
		return nil

	case briefs.JRN_INODE_FREE:
		ifr := briefs.UnmarshalInodeFree(data)
		if ifr != nil && ifr.Ino > 0 {
			b.inodeAlloc.FreeBlock(ifr.Ino - 1)
		}
		return nil

	case briefs.JRN_INODE_UPDATE:
		if reserveOnly {
			return nil
		}
		// Generation guard applies only to current 96-byte records; legacy
		// 88-byte records carry no generation (kernel journal.c:1047).
		hasGen := len(data) >= briefs.JrnInodeUpdateSize
		return b.replayInodeUpdate(briefs.UnmarshalInodeUpdate(data), hasGen)

	case briefs.JRN_EXTENT_ALLOC:
		return b.replayExtentAlloc(briefs.UnmarshalExtentAlloc(data))

	case briefs.JRN_EXTENT_FREE:
		return b.replayExtentFree(briefs.UnmarshalExtentFree(data))

	case briefs.JRN_TRIE_ALLOC:
		return b.replayTrieAlloc(briefs.UnmarshalTrieAlloc(data), reserveOnly)

	case briefs.JRN_INODE_FULL:
		ino := briefs.UnmarshalInodeFullIno(data)
		if reserveOnly {
			// Record the inode's final xattr_offset (last-wins) so pass-2's
			// xattr restore can skip stale content records for freed blocks.
			raw := briefs.InodeFullRawData(data)
			if raw != nil {
				xo := binary.LittleEndian.Uint64(raw[offXattrOffset:])
				b.xattrFinal[ino] = xo
			}
			return nil
		}
		return b.replayInodeFull(ino, data)

	case briefs.JRN_SYMLINK_DATA:
		if reserveOnly {
			return nil
		}
		return b.replaySymlinkData(briefs.UnmarshalSymlinkData(data))

	case briefs.JRN_XATTR_DATA:
		xd := briefs.UnmarshalXattrData(data)
		if xd == nil {
			return nil
		}
		if reserveOnly {
			// Reserve the xattr block and record its next_block link.
			if xd.PhysBlk != 0 {
				b.dataAlloc.ReserveBlock(xd.PhysBlk - b.dataRegionStart)
				b.xattrNext[xd.PhysBlk] = xattrRecNextBlock(xd.Data, xd.UsedSize)
			}
			return nil
		}
		return b.replayXattrData(xd)

	case briefs.JRN_TRIE_PAGE:
		// Reserved record type (kernel branch wip/475-content-journaling,
		// unmerged): no payload struct or replay handler exists yet. Fail
		// loudly rather than falling through to the silent default skip —
		// a skipped trie-page restore would leave stale directory data
		// in place. Mounting such an image stays impossible until the
		// branch merges and this grows a real handler.
		return fmt.Errorf("JRN_TRIE_PAGE record: no replay handler (unmerged kernel branch wip/475-content-journaling is not supported)")

	default:
		return nil
	}
}

// inodeFieldOffset computes the byte offset of an 8-byte field within the
// 512-byte on-disk inode by marshaling a sentinel inode and locating the
// marker value, so raw-snapshot readers stay correct if the layout ever
// shifts (see gen_disk.go for the authoritative field order).
func inodeFieldOffset(set func(in *briefs.Inode)) uint64 {
	in := &briefs.Inode{InodeNumber: 0x1122334455667788}
	set(in)
	raw, _ := in.MarshalBinary()
	for i := uint64(0); i+8 <= uint64(len(raw)); i++ {
		if binary.LittleEndian.Uint64(raw[i:]) == 0xdeadbeefcafebabe {
			return i
		}
	}
	return 0
}

// offXattrOffset is the byte offset of the xattr_offset field within the
// on-disk inode (kernel struct briefs_disk_inode; 384 on the current layout).
var offXattrOffset = inodeFieldOffset(func(in *briefs.Inode) {
	in.XattrOffset = 0xdeadbeefcafebabe
})

// offGeneration is the byte offset of the generation field within the on-disk
// inode (432 on the current layout). Replay reads it directly from raw
// snapshots (kernel 432; kernel commit 33e4019).
var offGeneration = inodeFieldOffset(func(in *briefs.Inode) {
	in.Generation = 0xdeadbeefcafebabe
})

// offDirTrieRoot is the byte offset of the dir_trie_root field within the
// on-disk inode (416 on the current layout; 384 is xattr_offset).
var offDirTrieRoot = inodeFieldOffset(func(in *briefs.Inode) {
	in.DirTrieRoot = 0xdeadbeefcafebabe
})

// xattrRecNextBlock extracts the next_block pointer from a JRN_XATTR_DATA
// content record's block bytes via the shared header codec. v1 blocks have no
// next pointer (the codec reads it as 0); a block whose header does not
// validate ends the chain rather than following a garbage pointer. Mirrors
// the kernel's xattr_rec_next_block() (journal.c:977).
func xattrRecNextBlock(data []byte, used uint32) uint64 {
	minHdr := uint32(briefs.XattrHeaderSize(1))
	if used < minHdr || uint32(len(data)) < minHdr {
		return 0
	}
	hdr, err := briefs.ReadXattrHeader(data)
	if err != nil {
		return 0
	}
	return hdr.NextBlock
}

// buildXattrLiveSet walks each inode's final xattr chain (via the pass-1
// next_block links) and marks every block it references as live. Pass-2's
// xattr restore only writes blocks still in the live set, so a freed-then-
// reused xattr block is not clobbered with stale content.
func (b *BrieFS) buildXattrLiveSet() {
	for _, head := range b.xattrFinal {
		block := head
		visited := 0
		for block != 0 {
			if visited > briefs.XattrMaxChain {
				break
			}
			b.xattrLive[block] = true
			block = b.xattrNext[block]
			visited++
		}
	}
}

// replayDirUpdate re-derives a directory trie entry. Add -> TrieInsert (EEXIST
// tolerated); delete -> TrieRemove (ENOENT tolerated). The parent inode is
// re-persisted so the replayed trie root reaches its inode block. Mirrors
// replay_dir_update() (journal.c:898).
//
// The parent's disk inode is read from its block ONCE per replay (first touch)
// and kept in replayParents — the port of the kernel's iget-cached
// binfo->disk_inode.  Re-reading the block per record would re-point
// re-derivation at whatever an interleaved JRN_INODE_FULL restore most
// recently wrote there, splitting a rename's delete/add pair across the stale
// and final roots (generic/534: the old name survives replay).  The cached
// anchor is the PRE-replay on-disk state: when an INODE_FULL restore has
// already overwritten the block before the first DIR_UPDATE, the pristine
// stashed slot (replayParentsPristine) is used instead of the restored
// content, so re-derivation never anchors at a mid-window snapshot's stale
// DirTrieRoot (generic/341: the stale root's freed slot was live-reused and
// re-derivation linked a second copy of every entry into it).
func (b *BrieFS) replayDirUpdate(rec *briefs.JrnDirUpdate) error {
	if rec == nil {
		return nil
	}
	blk, off := b.inodes.inodeLocation(rec.ParentIno)
	di, ok := b.replayParents[rec.ParentIno]
	if !ok {
		var slot []byte
		if pristine, seen := b.replayParentsPristine[rec.ParentIno]; seen {
			slot = pristine
		} else {
			buf, err := b.loadBlock(blk)
			if err != nil {
				return err
			}
			slot = buf[off : off+b.inodes.sb.InodeSize]
		}
		// Freed parent inode (magic 0): skippable (kernel iget -EINVAL).
		if binary.LittleEndian.Uint64(slot[8:]) != briefs.MagicInode {
			return nil
		}
		pdi, err := briefs.UnmarshalInode(slot)
		if err != nil {
			return nil
		}
		di = pdi
		b.replayParents[rec.ParentIno] = di
	}

	// Seed the partial-page pool from the parent's on-disk trie the first
	// time replay touches this directory (the kernel seeds per parent under
	// binfo->trie_pool_seeded, journal.c:938): replay's alloc-vs-reuse
	// decisions must match the live path's or it over-allocates trie pages
	// and ENOSPCs on a full fs (generic/475 / the 073 family).
	if !b.trieSeeded[rec.ParentIno] {
		b.trieSeeded[rec.ParentIno] = true
		b.trieSeedPool(di.DirTrieRoot)
	}

	if rec.Op == 0 {
		name := string(rec.Name[:rec.NameLen])
		rlog("  dir-add parent=%d name=%q child=%d ftype=%d rootBefore=%d", rec.ParentIno, name, rec.ChildIno, rec.FType, di.DirTrieRoot)
		if err := b.TrieInsert(di, name, rec.ChildIno, rec.FType); err != nil {
			if err != syscall.EEXIST {
				return err
			}
		}
		rlog("    -> rootAfter=%d", di.DirTrieRoot)
	} else {
		name := string(rec.Name[:rec.NameLen])
		rlog("  dir-del parent=%d name=%q rootBefore=%d", rec.ParentIno, name, di.DirTrieRoot)
		if err := b.TrieRemove(di, name); err != nil {
			if err != syscall.ENOENT {
				return err
			}
		}
		rlog("    -> rootAfter=%d", di.DirTrieRoot)
	}

	// Persist the parent disk inode (replayed trie root) into the cached block.
	raw, err := di.MarshalBinary()
	if err != nil {
		return err
	}
	buf, err := b.loadBlock(blk)
	if err != nil {
		return err
	}
	copy(buf[off:off+b.inodes.sb.InodeSize], raw)
	b.saveBlock(blk, buf)
	return nil
}

// replayInodeFull restores a 512-byte inode snapshot into the inode table,
// guarded against stale snapshots on reused slots. Mirrors
// replay_inode_full() (journal.c:1234, kernel commit 33e4019 / generic/536):
//
//   - A slot without the inode magic is a freed/empty slot; the kernel's
//     briefs_read_inode_block returns -EINVAL and the record is skipped. The
//     allocation paths arm the fresh slot in the page cache before committing
//     its snapshot (writeThroughFreshInodeSlot), so a committed INODE_FULL
//     always finds an armed slot for a live inode.
//   - If the snapshot's generation differs from the slot's, the slot has been
//     freed and reallocated to a different inode since this record was
//     written; restoring the stale snapshot would clobber the new owner.
func (b *BrieFS) replayInodeFull(ino uint64, data []byte) error {
	raw := briefs.InodeFullRawData(data)
	if raw == nil {
		return nil
	}
	// Log the DirTrieRoot carried in the snapshot (offset 416; 384 is
	// XattrOffset).
	var snapRoot uint64
	if uint64(len(raw)) >= offDirTrieRoot+8 {
		snapRoot = binary.LittleEndian.Uint64(raw[offDirTrieRoot:])
	}
	rlog("  inode-full ino=%d snapDirTrieRoot=%d", ino, snapRoot)
	blk, off := b.inodes.inodeLocation(ino)
	buf, err := b.loadBlock(blk)
	if err != nil {
		return err
	}
	// Preserve the pre-replay slot content the first time this replay
	// restores an inode that no DIR_UPDATE has cached yet (see
	// replayParentsPristine): the restore below overwrites the block with a
	// mid-window snapshot, and without the stash a later first-touch read
	// would anchor that parent's whole re-derivation at the snapshot's
	// stale DirTrieRoot instead of the state the live path drained
	// (generic/341).  Stash even when the guards below skip the restore —
	// the slot then still holds the pre-replay content, which is what the
	// stash is for.
	if _, cached := b.replayParents[ino]; !cached && b.replayParentsPristine != nil {
		if _, seen := b.replayParentsPristine[ino]; !seen {
			slot := make([]byte, b.inodes.sb.InodeSize)
			copy(slot, buf[off:off+b.inodes.sb.InodeSize])
			b.replayParentsPristine[ino] = slot
		}
	}
	// Freed/empty slot: skip (kernel -EINVAL path).
	if binary.LittleEndian.Uint64(buf[off+8:]) != briefs.MagicInode {
		rlog("  inode-full ino=%d skipped: slot not armed", ino)
		return nil
	}
	// Stale snapshot on a reused slot: skip (generation guard).
	var snapGen, slotGen uint64
	if len(raw) >= int(offGeneration)+8 {
		snapGen = binary.LittleEndian.Uint64(raw[offGeneration:])
	}
	if offGeneration+8 <= b.inodes.sb.InodeSize {
		slotGen = binary.LittleEndian.Uint64(buf[off+offGeneration:])
	}
	if snapGen != slotGen {
		rlog("  inode-full ino=%d skipped: stale snapshot (snap gen %d != slot gen %d)", ino, snapGen, slotGen)
		return nil
	}
	copy(buf[off:off+512], raw)
	b.saveBlock(blk, buf)
	return nil
}

// replayInodeUpdate applies a partial inode metadata update (mode/nlink/uid/
// gid/size/times/flags) to the on-disk inode, preserving extent/trie/xattr
// fields. Mirrors replay_inode_update() (journal.c:1024, kernel commit
// 33e4019): when the record carries a generation (96-byte records; legacy
// 88-byte records are applied unguarded, matching the kernel's
// rec_data_len-gated guard) and it differs from the slot's, the slot has been
// freed and reallocated since this record was written and applying the stale
// metadata would clobber the new owner.
func (b *BrieFS) replayInodeUpdate(rec *briefs.JrnInodeUpdate, hasGeneration bool) error {
	if rec == nil {
		return nil
	}
	blk, off := b.inodes.inodeLocation(rec.Ino)
	buf, err := b.loadBlock(blk)
	if err != nil {
		return err
	}
	if binary.LittleEndian.Uint64(buf[off+8:]) != briefs.MagicInode {
		return nil // freed inode, skip
	}
	if hasGeneration {
		if offGeneration+8 > b.inodes.sb.InodeSize {
			return nil // layout mismatch, skip conservatively
		}
		slotGen := binary.LittleEndian.Uint64(buf[off+offGeneration:])
		if rec.Generation != slotGen {
			rlog("  inode-update ino=%d skipped: stale record (rec gen %d != slot gen %d)", rec.Ino, rec.Generation, slotGen)
			return nil
		}
	}
	di, err := briefs.UnmarshalInode(buf[off : off+b.inodes.sb.InodeSize])
	if err != nil {
		return nil
	}
	di.InodeNumber = rec.Ino
	di.Magic = briefs.MagicInode
	di.Filemode = rec.Mode
	di.Nlinks = rec.Nlink
	di.Uid = rec.Uid
	di.Gid = rec.Gid
	di.FileSize = rec.FileSize
	di.AtimeSec = rec.ATimeSec
	di.AtimeNsec = rec.ATimeNsec
	di.MtimeSec = rec.MTimeSec
	di.MtimeNsec = rec.MTimeNsec
	di.CtimeSec = rec.CTimeSec
	di.CtimeNsec = rec.CTimeNsec
	di.Flags = rec.Flags
	raw, err := di.MarshalBinary()
	if err != nil {
		return err
	}
	copy(buf[off:off+b.inodes.sb.InodeSize], raw)
	b.saveBlock(blk, buf)
	return nil
}

// replayExtentAlloc reserves the recorded data blocks in the bitmap.
// Idempotent. Mirrors replay_extent_alloc() (journal.c:717).
func (b *BrieFS) replayExtentAlloc(rec *briefs.JrnExtentAlloc) error {
	if rec == nil {
		return nil
	}
	for i := uint64(0); i < rec.Length; i++ {
		b.dataAlloc.ReserveBlock(rec.PhysStart + i - b.dataRegionStart)
	}
	return nil
}

// replayExtentFree frees the recorded data blocks. Mirrors
// replay_extent_free() (journal.c:740).
func (b *BrieFS) replayExtentFree(rec *briefs.JrnExtentFree) error {
	if rec == nil {
		return nil
	}
	b.dataAlloc.FreeBlocksRange(b.dataRegionStart, rec.PhysStart, rec.Length)
	return nil
}

// replayTrieAlloc reserves (op=0) or frees (op=1) a trie page block in the
// data bitmap. Idempotent. Mirrors replay_trie_alloc() (journal.c:758).
func (b *BrieFS) replayTrieAlloc(rec *briefs.JrnTrieAlloc, collect bool) error {
	if rec == nil {
		return nil
	}
	if rec.Op == 0 {
		rel := rec.Block - b.dataRegionStart
		b.dataAlloc.ReserveBlock(rel)
		// In the pass-1 reservation pre-scan also record the block in the
		// replay pool so pass-2's triePageInit can reuse it instead of
		// re-allocating (generic/475; the kernel's collect flag).  Pass-2
		// re-reserves idempotently but must NOT re-push, or a block could be
		// consumed twice.
		if collect {
			b.replayTrieBlocks = append(b.replayTrieBlocks, rel)
		}
	} else {
		b.dataAlloc.FreeBlock(rec.Block - b.dataRegionStart)
	}
	return nil
}

// replaySymlinkData restores an extent-backed symlink's target block, but only
// if the symlink inode still exists and its first extent still points at the
// recorded block. Mirrors replay_symlink_data() (journal.c:893).
func (b *BrieFS) replaySymlinkData(rec *briefs.JrnSymlinkData) error {
	if rec == nil || rec.TargetLen == 0 || uint64(rec.TargetLen) > b.blockSize {
		return nil
	}
	blk, off := b.inodes.inodeLocation(rec.Ino)
	buf, err := b.loadBlock(blk)
	if err != nil {
		return nil // unreadable inode, skip
	}
	if binary.LittleEndian.Uint64(buf[off+8:]) != briefs.MagicInode {
		return nil
	}
	di, err := briefs.UnmarshalInode(buf[off : off+b.inodes.sb.InodeSize])
	if err != nil || !di.IsSymlink() {
		return nil
	}
	if di.NumExtentsTotal == 0 || di.NumExtentsInline < 1 {
		return nil
	}
	ext := di.InlineExtents()
	if ext[0].Len == 0 || ext[0].Phys != rec.Phys {
		return nil
	}
	pbuf, err := b.loadBlock(rec.Phys)
	if err != nil {
		return nil
	}
	for i := range pbuf {
		pbuf[i] = 0
	}
	copy(pbuf, rec.Target)
	b.saveBlock(rec.Phys, pbuf)
	return nil
}

// replayXattrData restores an xattr block's content (used_size bytes) + tail
// zero + CRC, but only when the block is still in its owning inode's final
// xattr chain (the pass-1 live set). Mirrors replay_xattr_data() (journal.c:1180).
func (b *BrieFS) replayXattrData(rec *briefs.JrnXattrData) error {
	if rec == nil || rec.UsedSize == 0 || rec.UsedSize > briefs.XattrMaxUsed {
		return nil
	}
	if !b.xattrLive[rec.PhysBlk] {
		return nil
	}
	buf, err := b.loadBlock(rec.PhysBlk)
	if err != nil {
		return nil // unreadable xattr block, skip
	}
	copy(buf, rec.Data)
	for i := uint32(rec.UsedSize); i < uint32(len(buf)); i++ {
		buf[i] = 0
	}
	// Recompute the CRC at offset 4080 over [0, 4080).
	binary.LittleEndian.PutUint64(buf[briefs.ExtentChainChecksumOffset:],
		briefs.ComputeChainChecksum(buf, b.blockSize))
	b.saveBlock(rec.PhysBlk, buf)
	return nil
}

// replayReconcileNlinks recomputes on-disk inode nlinks from the re-derived
// directory tries: directories get 2 + subdir_count, others get their directory
// entry link count. Mirrors replay_reconcile_nlinks() (journal.c:1516).
func (b *BrieFS) replayReconcileNlinks() error {
	maxIno := b.inodeAlloc.TotalBlocks()
	if maxIno == 0 {
		return nil
	}
	linkCount := make(map[uint64]uint32)
	subdirCount := make(map[uint64]uint32)
	// Pass 3a below reads every allocated inode once; this keeps the two
	// fields pass 3b needs so it does not have to read the whole table a
	// second time. Only an inode whose nlink actually disagrees is read
	// again, for the WriteInode patch.
	type inodeState struct {
		isDir  bool
		nlinks uint32
	}
	state := make(map[uint64]inodeState)

	dirFtype := uint8(briefs.ModeDir >> 12)

	// Pass 3a: count entries and subdirectories from the re-derived tries.
	for ino := uint64(1); ino <= maxIno; ino++ {
		if !b.inodeAlloc.Allocated(ino - 1) {
			continue
		}
		di, err := b.inodes.ReadInode(ino)
		if err != nil {
			continue
		}
		state[ino] = inodeState{isDir: di.IsDir(), nlinks: di.Nlinks}
		if !di.IsDir() {
			continue
		}
		iter := NewTrieIterator(b.dev, di.DirTrieRoot)
		for {
			childIno, ftype, _, ierr := iter.Next()
			if ierr != nil || childIno == 0 {
				break
			}
			if b.inodeAlloc.Allocated(childIno - 1) {
				linkCount[childIno]++
			}
			if ftype == dirFtype {
				subdirCount[ino]++
			}
		}
	}

	// Pass 3b: patch on-disk nlinks where they disagree.
	for ino, st := range state {
		var expected uint32
		if st.isDir {
			expected = 2 + subdirCount[ino]
		} else {
			expected = linkCount[ino]
		}
		if st.nlinks == expected {
			continue
		}
		di, err := b.inodes.ReadInode(ino)
		if err != nil {
			continue
		}
		di.Nlinks = expected
		if err := b.inodes.WriteInode(di); err != nil {
			return err
		}
	}
	return nil
}
