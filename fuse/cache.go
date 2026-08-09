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
// metadata for an operation lands on disk together, before the journal
// commits the records that reference it (the drain-before-snapshot rule).

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
// once, then drops the cache.  Call before journal.Sync so metadata is
// durable before the journal commits the records that reference it.
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