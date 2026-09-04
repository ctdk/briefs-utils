// Package fuse: per-operation block cache.
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
// cacheBegin/loadBlock/saveBlock/flushCache provide the equivalent: a
// short-lived per-operation map of block -> working buffer.  loadBlock returns
// the cached buffer (loading from disk on first touch); saveBlock marks it
// dirty.  flushCache writes every dirty block and fdatasyncs once, so all
// metadata for an operation lands on disk together after the journal commits
// the records that reference it (commit-before-flush; replay re-derives any
// metadata that did not reach disk — see the durability-ordering comment in
// dir_ops.go).

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

// flushCache writes every dirty cached block to the device and fdatasyncs
// once, then drops the cache.  Ops call it AFTER journal.Sync (commit-
// before-flush): the committed records let replay re-derive any metadata that
// a mid-op crash kept from reaching the page cache.
func (b *BrieFS) flushCache() error {
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
	child := pnode.FirstChild
	for !briefs.TrieRefIsNull(child) {
		_, cnode, err := b.trieRead(child)
		if err != nil {
			return 0, err
		}
		if cnode.ByteVal == byteVal {
			return child, nil
		}
		child = cnode.NextSibling
	}
	return 0, nil
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
// without the write-through, a mid-op crash (records committed, flushCache
// not yet run) would make the guard skip restoring the fresh inode.  Under
// the FUSE crash model (kill -9; the page cache survives) a plain WriteBlock
// is as durable as the journal block itself.
//
// Only the fresh inode's slot is patched in: the block is read straight from
// the device, not from the per-op cache, because the cache may hold mid-op
// mutations of OTHER inodes sharing this block (e.g. a rename target
// mid-removal) that must not reach the page cache before the journal
// commits the records justifying them.  The op cache is untouched; flushCache
// writes the full block (fresh slot + everything else) after the commit.
func (b *BrieFS) writeThroughFreshInodeSlot(in *briefs.Inode) error {
	blk, off := b.inodes.inodeLocation(in.InodeNumber)
	buf, err := b.dev.ReadBlock(blk)
	if err != nil {
		return err
	}
	data, err := in.MarshalBinary()
	if err != nil {
		return err
	}
	copy(buf[off:], data)
	return b.dev.WriteBlock(blk, buf)
}
