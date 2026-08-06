// Package fuse: extended attributes.
//
// Ports the kernel xattr.c format and set/get/list/remove operations into Go.
//
// On-disk format (mirrors briefs.h:719, xattr.c):
//
//	v2 header [0,32)  { magic@0, version@4, used_size@8, entry_count@12,
//	                   next_block@16, flags@24, reserved@28 }
//	entries  [32,..)  briefs_xattr_entry (8B: name_len, value_len,
//	                   name_offset, value_offset, all u16)
//	names+values      4-byte aligned; names include the namespace prefix
//	                   ("user.foo"); a value may be split across the entry
//	                   block (inline) + following continuation block(s).
//	[4080,4088)       CRC32C over [0,4080) (same coverage as B-tree nodes).
//
// The inode points at the first xattr block via XattrOffset (absolute) /
// XattrSize (used bytes of the first block); 0/0 means "no xattrs".  Blocks
// chain via next_block.  A continuation block (flags & BRIEFS_XATTR_FLAG_CONT)
// holds raw value bytes and no entries.
//
// Layout invariant (xattr.c:837 build_chain): an entry whose value overflows
// its entry block is always the LAST entry in that block, and its inline part
// fills the rest of the used region — so used - value_offset == inline_len for
// an overflowing entry, which is what the read path relies on to know how much
// to take from the entry block before following the chain.
//
// Durability: xattr block content is NOT re-derived from any pointer — it is
// carried verbatim by JRN_XATTR_DATA, which replay restores (replay_xattr_data,
// journal.c:1180).  So xattr blocks are re-derivable (like trie pages), and the
// op uses commit-before-flush with the ordering JRN_XATTR_DATA(new) ->
// JRN_INODE_FULL(new head) -> JRN_EXTENT_FREE(old): a partial commit by a
// concurrent op's Sync can never publish the new head without the content, nor
// free the old chain without the new head.  New xattr blocks are also journaled
// JRN_EXTENT_ALLOC so replay reserves them in the bitmap even before the
// checkpoint syncs the allocator (the kernel omits this, relying on the
// checkpoint; the bridge adds it to close the pre-checkpoint crash window — it
// is replay-compatible since replay_extent_alloc is idempotent).

package fuse

import (
	"encoding/binary"
	"fmt"
	"syscall"

	"github.com/ctdk/briefs-utils/briefs"
)

const (
	xattrEntrySize       = 8
	xattrV2HdrSize       = 32
	xattrMaxUsed         = 4044
	xattrMaxChain        = 1024
	xattrBlockMaxEntries = 501
	xattrMaxNameLen      = 255  // XATTR_NAME_MAX
	xattrMaxValueLen     = 65536 // XATTR_SIZE_MAX
	xattrPayloadCap      = xattrMaxUsed - xattrV2HdrSize // 4012
)

// xattrKV is an in-memory extended attribute (name includes its namespace
// prefix; value may be empty).
type xattrKV struct {
	name  string
	value []byte
}

// xattrBlockDesc is the in-memory plan for one block of a rebuilt xattr chain,
// mirroring struct xattr_block_desc (xattr.c:471).
type xattrBlockDesc struct {
	cont        bool
	payloadUsed uint32 // bytes of payload (after header) used so far
	// entry block:
	kv        []uint32 // indices into the kvs slice
	inlineLen []uint32 // inline value length for each entry
	// continuation block:
	kvIdx    uint32
	valueOff uint32
	fragLen  uint32
}

// align4 rounds n up to a multiple of 4.
func align4(n uint32) uint32 { return (n + 3) &^ 3 }

// getXattr returns the value of the named xattr. Returns ENODATA if absent.
func (b *BrieFS) getXattr(ino uint64, name string) ([]byte, error) {
	if len(name) > xattrMaxNameLen {
		return nil, syscall.ERANGE
	}
	lock := b.inodeBlockLock(ino)
	lock.Lock()
	defer lock.Unlock()
	in, err := b.inodes.ReadInode(ino)
	if err != nil {
		return nil, err
	}
	kvs, _, err := b.loadXattrEntries(in)
	if err != nil {
		return nil, err
	}
	for _, kv := range kvs {
		if kv.name == name {
			return kv.value, nil
		}
	}
	return nil, syscall.ENODATA
}

// listXattr returns the names of all xattrs (each including its namespace
// prefix). Returns an empty slice if there are none.
func (b *BrieFS) listXattr(ino uint64) ([]string, error) {
	lock := b.inodeBlockLock(ino)
	lock.Lock()
	defer lock.Unlock()
	in, err := b.inodes.ReadInode(ino)
	if err != nil {
		return nil, err
	}
	kvs, _, err := b.loadXattrEntries(in)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(kvs))
	for _, kv := range kvs {
		names = append(names, kv.name)
	}
	return names, nil
}

// setXattr sets, replaces, or removes (value == nil) an xattr, honoring the
// XATTR_CREATE (0x1) and XATTR_REPLACE (0x2) flags. Mirrors briefs_xattr_set
// (xattr.c:925). Locking: the inode-block lock (like a file write) — xattr
// blocks and the inode are written directly, so the op touches only its own
// shard plus the data region and needs no global dir lock.
func (b *BrieFS) setXattr(ino uint64, name string, value []byte, flags uint32) error {
	if len(name) > xattrMaxNameLen {
		return syscall.ERANGE
	}
	if value != nil && len(value) > xattrMaxValueLen {
		return syscall.E2BIG
	}
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
	return b.setXattrLocked(in, name, value, flags)
}

// setXattrLocked is the core set/remove path assuming the inode-block lock is
// held and @in is the current on-disk inode (it mutates @in in place). Used by
// setXattr and by killpriv (removePrivs) to clear security.capability within a
// write/truncate/chown without re-locking.
func (b *BrieFS) setXattrLocked(in *briefs.Inode, name string, value []byte, flags uint32) error {
	oldHead := in.XattrOffset

	kvs, _, err := b.loadXattrEntries(in)
	if err != nil {
		return err
	}

	// Locate the target entry.
	idx := -1
	for i := range kvs {
		if kvs[i].name == name {
			idx = i
			break
		}
	}

	removing := value == nil
	if removing {
		if idx < 0 {
			return syscall.ENODATA
		}
		kvs = append(kvs[:idx], kvs[idx+1:]...)
	} else {
		if idx >= 0 {
			if flags&0x1 != 0 { // XATTR_CREATE
				return syscall.EEXIST
			}
			kvs[idx].value = value
		} else {
			if flags&0x2 != 0 { // XATTR_REPLACE
				return syscall.ENODATA
			}
			kvs = append(kvs, xattrKV{name: name, value: value})
		}
	}

	// Last entry removed: clear the inode pointer, free the chain.
	if len(kvs) == 0 {
		in.XattrOffset = 0
		in.XattrSize = 0
		sec, nsec := nowTime()
		in.CtimeSec, in.CtimeNsec = sec, nsec
		if err := b.commitXattrOp(in, nil, nil, oldHead, true); err != nil {
			return err
		}
		return nil
	}

	// Build a fresh chain from the modified entry set.
	descs, err := buildXattrChain(kvs)
	if err != nil {
		return err
	}

	// Allocate a block for every descriptor, serialize, and write to the page
	// cache. Track allocated rels (for rollback / journaling) and abs blocks.
	allocated := make([]uint64, 0, len(descs))
	absBlocks := make([]uint64, 0, len(descs))
	bufs := make([][]byte, 0, len(descs))
	for range descs {
		rel := b.dataAlloc.AllocBlock()
		if rel == 0 {
			b.rollbackAlloc(allocated)
			return syscall.ENOSPC
		}
		allocated = append(allocated, rel)
		abs := b.dataRegionStart + rel
		absBlocks = append(absBlocks, abs)
	}
	for i, d := range descs {
		buf := serializeXattrBlock(d, kvs, absBlocks, uint64(i), b.blockSize)
		if err := b.dev.WriteBlock(absBlocks[i], buf); err != nil {
			b.rollbackAlloc(allocated)
			return err
		}
		bufs = append(bufs, buf)
	}

	// Publish the new head + size on the in-memory inode.
	in.XattrOffset = absBlocks[0]
	in.XattrSize = uint64(usedSizeOf(bufs[0]))
	sec, nsec := nowTime()
	in.CtimeSec, in.CtimeNsec = sec, nsec

	if err := b.commitXattrOp(in, absBlocks, bufs, oldHead, false); err != nil {
		return err
	}
	return nil
}

// commitXattrOp journals the xattr op and makes it durable. On a free (freeOld
// && no new blocks) it journals only INODE_FULL(cleared) + EXTENT_FREE(old).
// Otherwise it journals EXTENT_ALLOC + XATTR_DATA for each new block, then
// INODE_FULL(new head), then EXTENT_FREE(old). Drains and writes the inode
// block after the commit (xattr blocks are re-derivable via XATTR_DATA; the
// inode block is snapshot-trusted).
func (b *BrieFS) commitXattrOp(in *briefs.Inode, absBlocks []uint64, bufs [][]byte, oldHead uint64, freeOld bool) error {
	ino := in.InodeNumber
	// Journal new blocks: bitmap reservation + content.
	for i, abs := range absBlocks {
		if err := b.journalExtentAlloc(ino, 0, abs); err != nil {
			b.failWrite()
			return err
		}
		used := usedSizeOf(bufs[i])
		if err := b.journal.WriteRecord(briefs.JRN_XATTR_DATA,
			(&briefs.JrnXattrData{Ino: ino, PhysBlk: abs, UsedSize: used, Data: bufs[i][:used]}).Marshal()); err != nil {
			b.failWrite()
			return err
		}
	}
	// Journal the new inode pointer (xattr_offset/size) BEFORE freeing the old
	// chain, so a partial commit cannot free old blocks without the new head.
	if err := b.journalInodeFull(in); err != nil {
		b.failWrite()
		return err
	}
	// Journal EXTENT_FREE for the old chain (if any), after the new head.
	if freeOld || oldHead != 0 {
		oldBlocks, oerr := b.walkXattrChainBlocks(oldHead)
		if oerr != nil {
			b.failWrite()
			return oerr
		}
		for _, blk := range oldBlocks {
			if err := b.journalExtentFree(ino, blk); err != nil {
				b.failWrite()
				return err
			}
		}
		if err := b.journal.Sync(false); err != nil {
			b.failWrite()
			return err
		}
		// The new head is committed; free the old blocks in memory.
		for _, blk := range oldBlocks {
			b.dataAlloc.FreeBlock(blk - b.dataRegionStart)
		}
	} else {
		if err := b.journal.Sync(false); err != nil {
			b.failWrite()
			return err
		}
	}
	// Drain the new xattr blocks to disk (re-derivable via XATTR_DATA on crash,
	// but flush now so the non-crash path sees them without waiting for replay).
	for _, abs := range absBlocks {
		_ = abs // written above; Fdatasync below flushes the whole file.
	}
	if err := b.dev.Fdatasync(); err != nil {
		b.failWrite()
		return err
	}
	if err := b.writeInodeDirect(in); err != nil {
		b.failWrite()
		return err
	}
	if err := b.dev.Fdatasync(); err != nil {
		b.failWrite()
		return err
	}
	return nil
}

// removeXattr removes the named xattr. ENODATA if absent.
func (b *BrieFS) removeXattr(ino uint64, name string) error {
	return b.setXattr(ino, name, nil, 0)
}

// --- read path ---

// loadXattrEntries walks the inode's xattr chain and returns the assembled
// key/value set plus the chain's absolute block numbers (for freeing). Returns
// (nil, nil, nil) if the inode has no xattr block.
func (b *BrieFS) loadXattrEntries(in *briefs.Inode) ([]xattrKV, []uint64, error) {
	if in.XattrOffset == 0 {
		return nil, nil, nil
	}
	// Pass 1: load the whole chain into memory.
	var blocks [][]byte
	var abs []uint64
	block := in.XattrOffset
	visited := make(map[uint64]bool)
	for block != 0 {
		if visited[block] {
			return nil, nil, fmt.Errorf("briefs: xattr chain loop at %d", block)
		}
		if len(visited) > xattrMaxChain {
			return nil, nil, fmt.Errorf("briefs: xattr chain too long")
		}
		visited[block] = true
		buf, err := b.dev.ReadBlock(block)
		if err != nil {
			return nil, nil, err
		}
		magic := binary.LittleEndian.Uint32(buf[0:4])
		if magic != briefs.MagicXattr {
			return nil, nil, fmt.Errorf("briefs: xattr block %d bad magic", block)
		}
		if err := briefs.VerifyChainChecksum(buf, b.blockSize); err != nil {
			return nil, nil, fmt.Errorf("briefs: xattr block %d CRC: %w", block, err)
		}
		blocks = append(blocks, buf)
		abs = append(abs, block)
		block = binary.LittleEndian.Uint64(buf[16:24])
	}

	// Pass 2: parse entry blocks and assemble split values from the following
	// continuation blocks. An overflowing entry is the last in its block, so
	// its cont blocks are exactly those that follow until the next entry block.
	var kvs []xattrKV
	for i, buf := range blocks {
		flags := binary.LittleEndian.Uint32(buf[24:28])
		if flags&briefs.BrieFSXattrFlagCont != 0 {
			continue // continuation block; consumed with its entry block
		}
		entryCount := binary.LittleEndian.Uint32(buf[12:16])
		used := binary.LittleEndian.Uint32(buf[8:12])
		for j := uint32(0); j < entryCount; j++ {
			base := xattrV2HdrSize + j*xattrEntrySize
			nameLen := uint32(binary.LittleEndian.Uint16(buf[base:]))
			valueLen := uint32(binary.LittleEndian.Uint16(buf[base+2:]))
			nameOff := uint32(binary.LittleEndian.Uint16(buf[base+4:]))
			valueOff := uint32(binary.LittleEndian.Uint16(buf[base+6:]))
			name := string(buf[nameOff : nameOff+nameLen])
			value := make([]byte, valueLen)
			copied := uint32(0)
			if valueLen > 0 {
				// Inline part from this entry block (valueOff 0 => all in cont).
				if valueOff != 0 {
					avail := used - valueOff
					take := avail
					if take > valueLen {
						take = valueLen
					}
					copy(value[copied:], buf[valueOff:valueOff+take])
					copied += take
				}
				// Remainder from following continuation blocks.
				cb := i + 1
				for copied < valueLen && cb < len(blocks) {
					cbuf := blocks[cb]
					cflags := binary.LittleEndian.Uint32(cbuf[24:28])
					if cflags&briefs.BrieFSXattrFlagCont == 0 {
						break // next entry block
					}
					cused := binary.LittleEndian.Uint32(cbuf[8:12])
					cap := cused - xattrV2HdrSize
					take := cap
					if take > valueLen-copied {
						take = valueLen - copied
					}
					copy(value[copied:], cbuf[xattrV2HdrSize:xattrV2HdrSize+take])
					copied += take
					cb++
				}
			}
			kvs = append(kvs, xattrKV{name: name, value: value})
		}
	}
	return kvs, abs, nil
}

// walkXattrChainBlocks returns the absolute block numbers of a chain starting
// at head, without assembling values. Used to free the old chain.
func (b *BrieFS) walkXattrChainBlocks(head uint64) ([]uint64, error) {
	if head == 0 {
		return nil, nil
	}
	var out []uint64
	block := head
	visited := make(map[uint64]bool)
	for block != 0 {
		if visited[block] {
			return nil, fmt.Errorf("briefs: xattr free loop at %d", block)
		}
		if len(visited) > xattrMaxChain {
			return nil, fmt.Errorf("briefs: xattr chain too long")
		}
		visited[block] = true
		out = append(out, block)
		buf, err := b.dev.ReadBlock(block)
		if err != nil {
			return nil, err
		}
		block = binary.LittleEndian.Uint64(buf[16:24])
	}
	return out, nil
}

// --- chain build / serialize ---

// buildXattrChain packs the entries into one or more xattr block descriptors,
// mirroring xattr_build_chain (xattr.c:837). An entry whose value does not fit
// inline spills into continuation block(s); an overflowing entry is always the
// last in its entry block (its inline part fills the remaining payload, so the
// next entry starts a fresh block).
func buildXattrChain(kvs []xattrKV) ([]*xattrBlockDesc, error) {
	if len(kvs) == 0 {
		return nil, nil
	}
	var descs []*xattrBlockDesc
	addEntryBlock := func() *xattrBlockDesc {
		d := &xattrBlockDesc{}
		descs = append(descs, d)
		return d
	}
	addContBlock := func(kvIdx, valueOff, frag uint32) {
		descs = append(descs, &xattrBlockDesc{
			cont: true, kvIdx: kvIdx, valueOff: valueOff, fragLen: frag,
			payloadUsed: frag,
		})
	}
	cur := addEntryBlock()
	for i, kv := range kvs {
		nameLen := uint32(len(kv.name))
		valueLen := uint32(len(kv.value))
		needEntry := uint32(xattrEntrySize) + align4(nameLen)
	again:
		maxPayload := uint32(xattrPayloadCap)
		if needEntry > maxPayload-cur.payloadUsed || len(cur.kv) >= xattrBlockMaxEntries {
			cur = addEntryBlock()
			goto again
		}
		remaining := maxPayload - cur.payloadUsed - needEntry
		var inlineLen uint32
		switch {
		case valueLen == 0 || remaining < 4:
			inlineLen = 0
		case align4(valueLen) <= remaining:
			inlineLen = valueLen
		default:
			inlineLen = valueLen
			if r4 := remaining &^ 3; r4 < inlineLen {
				inlineLen = r4
			}
		}
		cur.kv = append(cur.kv, uint32(i))
		cur.inlineLen = append(cur.inlineLen, inlineLen)
		cur.payloadUsed += needEntry + align4(inlineLen)
		if valueLen > inlineLen {
			rem := valueLen - inlineLen
			off := inlineLen
			for rem > 0 {
				cap := uint32(xattrPayloadCap)
				frag := rem
				if frag > cap {
					frag = cap
				}
				addContBlock(uint32(i), off, frag)
				rem -= frag
				off += frag
			}
		}
	}
	if len(descs) > xattrMaxChain {
		return nil, syscall.ENOSPC
	}
	return descs, nil
}

// serializeXattrBlock renders descriptor @i (with its successor chain links)
// into a full block buffer. Mirrors the kernel serialize loop (xattr.c:1072).
func serializeXattrBlock(d *xattrBlockDesc, kvs []xattrKV, absBlocks []uint64, i uint64, blockSize uint64) []byte {
	buf := make([]byte, blockSize)
	var next uint64
	if i+1 < uint64(len(absBlocks)) {
		next = absBlocks[i+1]
	}
	binary.LittleEndian.PutUint32(buf[0:4], briefs.MagicXattr)
	binary.LittleEndian.PutUint32(buf[4:8], briefs.BrieFSXattrVersion)
	binary.LittleEndian.PutUint64(buf[16:24], next)
	binary.LittleEndian.PutUint32(buf[28:32], 0) // reserved

	var used uint32
	if d.cont {
		binary.LittleEndian.PutUint32(buf[24:28], briefs.BrieFSXattrFlagCont)
		binary.LittleEndian.PutUint32(buf[12:16], 0) // entry_count
		frag := d.fragLen
		copy(buf[xattrV2HdrSize:], kvs[d.kvIdx].value[d.valueOff:d.valueOff+frag])
		used = uint32(xattrV2HdrSize) + frag
	} else {
		binary.LittleEndian.PutUint32(buf[24:28], 0) // flags
		binary.LittleEndian.PutUint32(buf[12:16], uint32(len(d.kv)))
		off := uint32(xattrV2HdrSize) + uint32(len(d.kv))*xattrEntrySize
		for j, kvIdx := range d.kv {
			kv := kvs[kvIdx]
			nameLen := uint32(len(kv.name))
			valueLen := uint32(len(kv.value))
			inlineLen := d.inlineLen[j]
			base := xattrV2HdrSize + uint32(j)*xattrEntrySize
			binary.LittleEndian.PutUint16(buf[base:], uint16(nameLen))
			binary.LittleEndian.PutUint16(buf[base+2:], uint16(valueLen))
			binary.LittleEndian.PutUint16(buf[base+4:], uint16(off))
			copy(buf[off:], kv.name)
			off += align4(nameLen)
			switch {
			case valueLen == 0:
				binary.LittleEndian.PutUint16(buf[base+6:], 0)
			case inlineLen == 0:
				// Sentinel: the whole value lives in continuation block(s).
				binary.LittleEndian.PutUint16(buf[base+6:], 0)
			default:
				binary.LittleEndian.PutUint16(buf[base+6:], uint16(off))
				copy(buf[off:], kv.value[:inlineLen])
				off += align4(inlineLen)
			}
		}
		used = off
	}
	binary.LittleEndian.PutUint32(buf[8:12], used)
	// Zero the tail beyond used; the CRC over [0,4080) is recomputed.
	binary.LittleEndian.PutUint64(buf[4080:], briefs.ComputeChainChecksum(buf, blockSize))
	return buf
}

// usedSizeOf returns the used_size field of a serialized xattr block buffer.
func usedSizeOf(buf []byte) uint32 {
	return binary.LittleEndian.Uint32(buf[8:12])
}