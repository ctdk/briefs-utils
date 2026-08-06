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
	"fmt"
	"syscall"

	"github.com/ctdk/briefs-utils/briefs"
)

// replayJournal replays the live journal range at mount. It is a no-op when the
// journal is clean (log_start == log_end). On success the journal is marked
// clean (log_start advances to log_end) and the allocator bitmaps + superblock
// are persisted.
func (b *BrieFS) replayJournal() error {
	start, end, _ := b.journal.ReplayLogRange()
	if start == end {
		return nil // clean, nothing to replay
	}

	// Replay-private state.
	b.xattrFinal = make(map[uint64]uint64)
	b.xattrNext = make(map[uint64]uint64)
	b.xattrLive = make(map[uint64]bool)
	defer func() {
		b.xattrFinal = nil
		b.xattrNext = nil
		b.xattrLive = nil
	}()

	b.journal.SetInReplay(true)
	defer b.journal.SetInReplay(false)

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
	if err := b.flushCache(); err != nil {
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
// full pass-2 apply.
func (b *BrieFS) walkJournal(reserveOnly bool) error {
	start, end, checkpointBlk := b.journal.ReplayLogRange()
	blockSize := b.journal.JournalBlockSize()
	cur := start

	for cur != end {
		// Skip the reserved checkpoint block (journal_end - 1): the write path
		// never lands ordinary records there, and reading it as an ordinary
		// record block fails when its content is stale from a prior mount.
		if cur == checkpointBlk {
			cur = b.journal.NextJournalBlock(cur)
			continue
		}

		buf, err := b.journal.ReadJournalBlock(cur)
		if err != nil {
			return fmt.Errorf("read journal block %d: %w", cur, err)
		}

		magic := binary.LittleEndian.Uint32(buf[0:])
		if magic != briefs.MagicJournal && magic != briefs.MagicCheckpoint {
			// Stale/garbage block at the tail: stop (don't fail the mount).
			break
		}

		recCount := binary.LittleEndian.Uint32(buf[8:])
		off := uint64(briefs.JournalBlockHdrSize)
		for i := uint32(0); i < recCount && off+briefs.JournalRecordHdrSize <= blockSize; i++ {
			hdr := briefs.ParseRecordHeader(buf[off:])
			rtype := hdr.Type
			dlen := uint64(hdr.DataLen)

			if rtype <= briefs.JRN_NONE || rtype >= briefs.JRN_END {
				return fmt.Errorf("invalid record type %d at block %d", rtype, cur)
			}
			if off+briefs.JournalRecordHdrSize+dlen > blockSize {
				return fmt.Errorf("record overflows block %d (off=%d len=%d)", cur, off, dlen)
			}

			recData := buf[off+briefs.JournalRecordHdrSize : off+briefs.JournalRecordHdrSize+dlen]

			// Checksum verify (zero checksum = legacy, accepted).
			if !briefs.VerifyJournalRecordChecksum(rtype, hdr.Flags, recData, hdr.Checksum) {
				return fmt.Errorf("checksum mismatch at block %d record %d type %d", cur, i, rtype)
			}

			// JRN_CHECKPOINT marker records are not replay-able; skip.
			if rtype == briefs.JRN_CHECKPOINT {
				off += briefs.JournalRecordHdrSize + dlen
				continue
			}

			if err := b.applyRecord(rtype, recData, reserveOnly); err != nil {
				// Freed-inode / not-present results are skippable, not mount-fatal.
				if err == syscall.EINVAL || err == syscall.ENOENT {
					// skip
				} else {
					return fmt.Errorf("replay record type %d at block %d: %w", rtype, cur, err)
				}
			}
			off += briefs.JournalRecordHdrSize + dlen
		}

		cur = b.journal.NextJournalBlock(cur)
	}
	return nil
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
		return b.replayInodeUpdate(briefs.UnmarshalInodeUpdate(data))

	case briefs.JRN_EXTENT_ALLOC:
		return b.replayExtentAlloc(briefs.UnmarshalExtentAlloc(data))

	case briefs.JRN_EXTENT_FREE:
		return b.replayExtentFree(briefs.UnmarshalExtentFree(data))

	case briefs.JRN_TRIE_ALLOC:
		return b.replayTrieAlloc(briefs.UnmarshalTrieAlloc(data))

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

	default:
		return nil
	}
}

// offXattrOffset is the byte offset of the xattr_offset field within the
// 512-byte on-disk inode. It matches the kernel's struct briefs_disk_inode
// layout (see gen_disk.go: XattrOffset is marshaled at this position).
var offXattrOffset = func() uint64 {
	// Compute from a marshaled sentinel inode to avoid hardcoding the layout.
	in := &briefs.Inode{InodeNumber: 0x1122334455667788, XattrOffset: 0xdeadbeefcafebabe}
	raw, _ := in.MarshalBinary()
	for i := uint64(0); i+8 <= uint64(len(raw)); i++ {
		if binary.LittleEndian.Uint64(raw[i:]) == 0xdeadbeefcafebabe {
			return i
		}
	}
	return 0
}()

// xattrRecNextBlock extracts the next_block pointer from a JRN_XATTR_DATA
// content record's block bytes. v1 blocks have no next pointer; v2 blocks carry
// it at offset 16. Mirrors the kernel's xattr_rec_next_block() (journal.c:977).
func xattrRecNextBlock(data []byte, used uint32) uint64 {
	const xattrHdrSize = 32
	if used < xattrHdrSize || uint32(len(data)) < xattrHdrSize {
		return 0
	}
	version := binary.LittleEndian.Uint32(data[4:])
	if version == 1 {
		return 0
	}
	return binary.LittleEndian.Uint64(data[16:])
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
			if visited > xattrMaxChain {
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
// replay_dir_update() (journal.c:548).
func (b *BrieFS) replayDirUpdate(rec *briefs.JrnDirUpdate) error {
	if rec == nil {
		return nil
	}
	blk, off := b.inodes.inodeLocation(rec.ParentIno)
	buf, err := b.loadBlock(blk)
	if err != nil {
		return err
	}
	// Freed parent inode (magic 0): skippable.
	if binary.LittleEndian.Uint64(buf[off+8:]) != briefs.MagicInode {
		return nil
	}
	di, err := briefs.UnmarshalInode(buf[off : off+b.inodes.sb.InodeSize])
	if err != nil {
		return nil
	}

	if rec.Op == 0 {
		if err := b.TrieInsert(di, rec.Name, rec.ChildIno, rec.FType); err != nil {
			if err != syscall.EEXIST {
				return err
			}
		}
	} else {
		if err := b.TrieRemove(di, rec.Name); err != nil {
			if err != syscall.ENOENT {
				return err
			}
		}
	}

	// Persist the parent disk inode (replayed trie root) into the cached block.
	raw, err := di.MarshalBinary()
	if err != nil {
		return err
	}
	copy(buf[off:off+b.inodes.sb.InodeSize], raw)
	b.saveBlock(blk, buf)
	return nil
}

// replayInodeFull restores a 512-byte inode snapshot verbatim into the inode
// table. Mirrors replay_inode_full() (journal.c:859).
func (b *BrieFS) replayInodeFull(ino uint64, data []byte) error {
	raw := briefs.InodeFullRawData(data)
	if raw == nil {
		return nil
	}
	blk, off := b.inodes.inodeLocation(ino)
	buf, err := b.loadBlock(blk)
	if err != nil {
		return err
	}
	copy(buf[off:off+512], raw)
	b.saveBlock(blk, buf)
	return nil
}

// replayInodeUpdate applies a partial inode metadata update (mode/nlink/uid/
// gid/size/times/flags) to the on-disk inode, preserving extent/trie/xattr
// fields. Mirrors replay_inode_update() (journal.c:657).
func (b *BrieFS) replayInodeUpdate(rec *briefs.JrnInodeUpdate) error {
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
	di.FileSize = rec.Size
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
func (b *BrieFS) replayTrieAlloc(rec *briefs.JrnTrieAlloc) error {
	if rec == nil {
		return nil
	}
	if rec.Op == 0 {
		b.dataAlloc.ReserveBlock(rec.Block - b.dataRegionStart)
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
	if rec == nil || rec.UsedSize == 0 || rec.UsedSize > xattrMaxUsed {
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

	dirFtype := uint8(briefs.ModeDir >> 12)

	// Pass 3a: count entries and subdirectories from the re-derived tries.
	for ino := uint64(1); ino <= maxIno; ino++ {
		if !b.inodeAlloc.Allocated(ino - 1) {
			continue
		}
		di, err := b.inodes.ReadInode(ino)
		if err != nil || !di.IsDir() {
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
	for ino := uint64(1); ino <= maxIno; ino++ {
		if !b.inodeAlloc.Allocated(ino - 1) {
			continue
		}
		di, err := b.inodes.ReadInode(ino)
		if err != nil {
			continue
		}
		var expected uint32
		if di.IsDir() {
			expected = 2 + subdirCount[ino]
		} else {
			expected = linkCount[ino]
		}
		if di.Nlinks == expected {
			continue
		}
		di.Nlinks = expected
		if err := b.inodes.WriteInode(di); err != nil {
			return err
		}
	}
	return nil
}