// Package fuse: directory trie mutation (insert / remove / update).
//
// This is a Go port of the kernel's trie.c (briefs_trie_insert:605,
// briefs_trie_remove:803, briefs_trie_update_entry:724, trie_split_leaf:519,
// trie_link_child:177, briefs_trie_create_child:227) and trie_page.c
// (briefs_trie_alloc_node:553, briefs_trie_free_node:711,
// briefs_trie_page_init:240, briefs_trie_node_store_name:664).  It produces
// on-disk trie pages byte-compatible with the kernel so a FUSE-written volume
// mounts and replays under the kernel module.
//
// BrieFS has no buffer cache, so the kernel's "mark_buffer_dirty + lazy
// writeback" becomes direct WriteBlock into the backing file's page cache,
// and the kernel's sync_dirty_buffer in briefs_trie_page_init becomes
// dev.Sync() (fdatasync) so a freshly allocated trie page is durable before
// any later journal record that traverses into it can be committed (the
// generic/065 bad-magic family).

package fuse

import (
	"encoding/binary"
	"syscall"

	"github.com/ctdk/briefs-utils/briefs"
)

// triePageDataEnd is the byte offset where the name heap begins growing
// downward: 20-byte page header + 64 slots of 36 bytes = 2324.
func triePageDataEnd() uint16 {
	return uint16(triePageHeaderSize + trieSlotCount*trieSlotSize)
}

// --- slot write accessors (within a page buffer) ---

func trieSlotBuf(page []byte, slot uint) []byte {
	off := triePageHeaderSize + int(slot)*trieSlotSize
	return page[off : off+trieSlotSize]
}

func slotSetFirstChild(s []byte, v uint64) { binary.LittleEndian.PutUint64(s[trieSlotFirstChild:], v) }
func slotSetNextSibling(s []byte, v uint64) { binary.LittleEndian.PutUint64(s[trieSlotNextSibling:], v) }
func slotSetInode(s []byte, v uint64)      { binary.LittleEndian.PutUint64(s[trieSlotInode:], v) }
func slotSetNameLen(s []byte, v uint16)    { binary.LittleEndian.PutUint16(s[trieSlotNameLen:], v) }
func slotSetNameOffset(s []byte, v uint16) {
	binary.LittleEndian.PutUint16(s[trieSlotNameOffset:], v)
}
func slotSetDepth(s []byte, v uint8)    { s[trieSlotDepth] = v }
func slotSetNodeType(s []byte, v uint8) { s[trieSlotNodeType] = v }
func slotSetByteVal(s []byte, v uint8)  { s[trieSlotByteVal] = v }
func slotSetFType(s []byte, v uint8)    { s[trieSlotFType] = v }
func slotSetChildCount(s []byte, v uint16) {
	binary.LittleEndian.PutUint16(s[trieSlotChildCount:], v)
}

// --- page header accessors ---

func pageLiveCount(p []byte) uint16        { return binary.LittleEndian.Uint16(p[triePageLiveCountOff:]) }
func pageSetLiveCount(p []byte, v uint16)  { binary.LittleEndian.PutUint16(p[triePageLiveCountOff:], v) }
func pageFreeNameOff(p []byte) uint16      { return binary.LittleEndian.Uint16(p[triePageFreeNameOffOff:]) }
func pageSetFreeNameOff(p []byte, v uint16) {
	binary.LittleEndian.PutUint16(p[triePageFreeNameOffOff:], v)
}
func pageFreeSlots(p []byte) uint64        { return binary.LittleEndian.Uint64(p[triePageFreeSlotsOff:]) }
func pageSetFreeSlots(p []byte, v uint64)  { binary.LittleEndian.PutUint64(p[triePageFreeSlotsOff:], v) }

// trieIsLeafNode mirrors TRIE_IS_LEAF: a node is a leaf if it is not an
// intermediate node, or it is an intermediate that also carries a leaf entry.
func trieIsLeafNode(nt uint8) bool {
	return (nt&trieNodeTypeInterm) == 0 || (nt&trieNodeStatusLeaf) != 0
}

// triePageAllocSlot finds the first free slot (bit set in free_slots), clears
// it, bumps live_count, and returns the slot index. Returns ok=false if full.
func triePageAllocSlot(page []byte) (uint, bool) {
	fs := pageFreeSlots(page)
	for slot := uint(0); slot < trieSlotCount; slot++ {
		if fs&(1<<slot) != 0 {
			pageSetFreeSlots(page, fs&^(1<<slot))
			pageSetLiveCount(page, pageLiveCount(page)+1)
			return slot, true
		}
	}
	return 0, false
}

// triePageHasNameHeap reports whether the page can fit a name entry of
// name_size bytes (name_len + 2) in the heap.
func triePageHasNameHeap(page []byte, nameSize uint16) bool {
	if nameSize == 0 {
		return true
	}
	return uint64(pageFreeNameOff(page))+uint64(nameSize) <=
		uint64(len(page))-uint64(triePageDataEnd())
}

// triePageAllocName allocates name-heap space for a node, reusing an existing
// allocation if it is large enough.  Sets the node's name_offset and name_len.
// Returns ENOSPC if the heap is full.
func triePageAllocName(page []byte, s []byte, nameLen int) error {
	if nameLen == 0 {
		return nil
	}
	if nameLen > briefs.BrieFSMaxNameLen {
		return syscall.ENAMETOOLONG
	}
	nameSize := uint16(nameLen + 2)
	// Reuse an existing allocation that is large enough.
	if curOff := binary.LittleEndian.Uint16(s[trieSlotNameOffset:]); curOff > 0 {
		if curLen := binary.LittleEndian.Uint16(s[trieSlotNameLen:]); curLen >= nameSize {
			return nil
		}
	}
	freeOff := pageFreeNameOff(page)
	nameBase := uint16(len(page)) - freeOff
	if nameBase-nameSize < triePageDataEnd() {
		return syscall.ENOSPC
	}
	newOff := freeOff + nameSize
	pageSetFreeNameOff(page, newOff)
	slotSetNameOffset(s, newOff)
	slotSetNameLen(s, nameSize)
	return nil
}

// addPartial adds a block to the partial-page pool (deduped).
func (b *BrieFS) addPartial(block uint64) {
	for _, e := range b.triePartials {
		if e == block {
			return
		}
	}
	b.triePartials = append(b.triePartials, block)
}

// removePartial removes a block from the partial-page pool.
func (b *BrieFS) removePartial(block uint64) {
	out := b.triePartials[:0]
	for _, e := range b.triePartials {
		if e != block {
			out = append(out, e)
		}
	}
	b.triePartials = out
}

// triePageInit allocates a fresh data block, formats it as a trie page with
// slot 0 holding the requested node, journals JRN_TRIE_ALLOC, makes it
// durable, and adds it to the partial pool.  Returns the node ref (slot 0).
func (b *BrieFS) triePageInit(depth, byteVal, nodeType uint8) (uint64, error) {
	rel := b.dataAlloc.AllocBlock()
	if rel == 0 {
		return 0, syscall.ENOSPC
	}
	block := b.dataRegionStart + rel

	buf := make([]byte, b.blockSize)
	binary.LittleEndian.PutUint32(buf[triePageMagicOff:], briefs.MagicTriePage)
	binary.LittleEndian.PutUint32(buf[triePageVersionOff:], 1)
	pageSetLiveCount(buf, 1)
	pageSetFreeNameOff(buf, 0)
	pageSetFreeSlots(buf, ^uint64(1)) // slot 0 allocated; rest free

	s0 := trieSlotBuf(buf, 0)
	slotSetDepth(s0, depth)
	slotSetByteVal(s0, byteVal)
	slotSetNodeType(s0, nodeType)

	if err := b.saveBlock(block, buf); err != nil {
		b.dataAlloc.FreeBlock(rel)
		return 0, err
	}
	// Journal the trie-page allocation (absolute block) so replay reserves it.
	if err := b.journal.WriteRecord(briefs.JRN_TRIE_ALLOC,
		(&briefs.JrnTrieAlloc{Block: block, Op: 0}).Marshal()); err != nil {
		b.dataAlloc.FreeBlock(rel)
		return 0, err
	}
	// The page is held in the operation's block cache; flushCache (called by
	// the handler before journal.Sync) writes it durably before any journal
	// record that traverses into it can be committed — the kernel's
	// sync_dirty_buffer in briefs_trie_page_init, batched (generic/065).
	b.addPartial(block)
	return briefs.TrieMakeRef(block, 0), nil
}

// trieAllocNode allocates a node (and name-heap space if nameLen > 0) from a
// partial page, or a fresh page if none has room.  Mirrors
// briefs_trie_alloc_node (trie_page.c:553), minus the hot-page cache.
func (b *BrieFS) trieAllocNode(nameLen int) (uint64, error) {
	nameSize := uint16(0)
	if nameLen > 0 {
		nameSize = uint16(nameLen + 2)
	}

	// Scan the partial pool for a page with a free slot and name-heap room.
	// Build the surviving list in a fresh slice: aliasing b.triePartials[:0]
	// would overwrite entries we have not yet read.
	out := make([]uint64, 0, len(b.triePartials))
	var ref uint64
	found := false
	for _, block := range b.triePartials {
		if found {
			out = append(out, block)
			continue
		}
		buf, err := b.loadBlock(block)
		if err != nil {
			continue
		}
		if binary.LittleEndian.Uint32(buf[triePageMagicOff:]) != briefs.MagicTriePage {
			continue // stale/corrupt; drop from pool
		}
		if !triePageHasNameHeap(buf, nameSize) {
			out = append(out, block)
			continue
		}
		slot, ok := triePageAllocSlot(buf)
		if !ok {
			continue // full; drop from pool (don't re-add)
		}
		s := trieSlotBuf(buf, slot)
		for i := range s {
			s[i] = 0
		}
		if nameSize > 0 {
			newOff := pageFreeNameOff(buf) + nameSize
			pageSetFreeNameOff(buf, newOff)
			slotSetNameOffset(s, newOff)
			slotSetNameLen(s, nameSize)
		}
		if err := b.saveBlock(block, buf); err != nil {
			return 0, err
		}
		ref = briefs.TrieMakeRef(block, slot)
		// Keep the page in the pool only if it still has free slots.
		if pageFreeSlots(buf) != 0 {
			out = append(out, block)
		}
		found = true
	}
	b.triePartials = out
	if found {
		return ref, nil
	}

	// No partial page had room: allocate a fresh page whose slot 0 is the node.
	r, err := b.triePageInit(0, 0, 0)
	if err != nil {
		return 0, err
	}
	if nameSize > 0 {
		block := briefs.TrieRefBlock(r)
		buf, err := b.loadBlock(block)
		if err != nil {
			return 0, err
		}
		s := trieSlotBuf(buf, 0)
		newOff := pageFreeNameOff(buf) + nameSize
		pageSetFreeNameOff(buf, newOff)
		slotSetNameOffset(s, newOff)
		slotSetNameLen(s, nameSize)
		if err := b.saveBlock(block, buf); err != nil {
			return 0, err
		}
	}
	return r, nil
}

// trieFreeNode frees a node's slot.  If the page becomes empty, the data block
// is returned to the allocator and JRN_TRIE_FREE is journaled.  Mirrors
// briefs_trie_free_node (trie_page.c:711).
func (b *BrieFS) trieFreeNode(ref uint64) error {
	if briefs.TrieRefIsNull(ref) {
		return nil
	}
	block := briefs.TrieRefBlock(ref)
	slot := uint(briefs.TrieRefSlot(ref))

	buf, err := b.loadBlock(block)
	if err != nil {
		return err
	}
	fs := pageFreeSlots(buf)
	if fs&(1<<slot) != 0 { // already free
		return nil
	}
	pageSetFreeSlots(buf, fs|(1<<slot))
	pageSetLiveCount(buf, pageLiveCount(buf)-1)
	s := trieSlotBuf(buf, slot)
	for i := range s {
		s[i] = 0
	}
	pageEmpty := pageLiveCount(buf) == 0

	if !pageEmpty {
		if pageFreeSlots(buf) != 0 {
			b.addPartial(block)
		}
		return b.saveBlock(block, buf)
	}

	// Page is empty: the empty page is held in the cache (flushCache writes it
	// before the journal commits the JRN_TRIE_FREE below), then return the
	// block to the allocator.
	if err := b.saveBlock(block, buf); err != nil {
		return err
	}
	b.removePartial(block)
	if err := b.journal.WriteRecord(briefs.JRN_TRIE_ALLOC,
		(&briefs.JrnTrieAlloc{Block: block, Op: 1}).Marshal()); err != nil {
		return err
	}
	b.dataAlloc.FreeBlock(block - b.dataRegionStart)
	return nil
}

// trieStoreName writes the name bytes (2-byte LE length prefix + name) into
// the node's allocated name-heap space, growing it if needed.  Mirrors
// briefs_trie_node_store_name (trie_page.c:664).
func (b *BrieFS) trieStoreName(ref uint64, name string) error {
	block := briefs.TrieRefBlock(ref)
	slot := uint(briefs.TrieRefSlot(ref))
	buf, err := b.loadBlock(block)
	if err != nil {
		return err
	}
	s := trieSlotBuf(buf, slot)
	if err := triePageAllocName(buf, s, len(name)); err != nil {
		return err
	}
	if len(name) == 0 {
		slotSetNameLen(s, 0)
		slotSetNameOffset(s, 0)
		return b.saveBlock(block, buf)
	}
	nameOff := binary.LittleEndian.Uint16(s[trieSlotNameOffset:])
	nameStart := uint16(b.blockSize) - nameOff
	binary.LittleEndian.PutUint16(buf[nameStart:], uint16(len(name)))
	copy(buf[nameStart+2:], name)
	slotSetNameLen(s, uint16(2+len(name)))
	return b.saveBlock(block, buf)
}

// trieLinkChild links a child into the parent's sibling chain and bumps the
// parent's child_count.  Mirrors trie_link_child (trie.c:177).
func (b *BrieFS) trieLinkChild(parent, child uint64) error {
	pbuf, pnode, err := b.trieRead(parent)
	if err != nil {
		return err
	}
	ps := trieSlotBuf(pbuf, uint(briefs.TrieRefSlot(parent)))
	if briefs.TrieRefIsNull(pnode.FirstChild) {
		slotSetFirstChild(ps, child)
		slotSetChildCount(ps, pnode.ChildCount+1)
		return b.saveBlock(briefs.TrieRefBlock(parent), pbuf)
	}
	// Walk to the last sibling.
	last := pnode.FirstChild
	for {
		lbuf, lnode, err := b.trieRead(last)
		if err != nil {
			return err
		}
		if briefs.TrieRefIsNull(lnode.NextSibling) {
			ls := trieSlotBuf(lbuf, uint(briefs.TrieRefSlot(last)))
			slotSetNextSibling(ls, child)
			if err := b.saveBlock(briefs.TrieRefBlock(last), lbuf); err != nil {
				return err
			}
			break
		}
		last = lnode.NextSibling
	}
	slotSetChildCount(ps, pnode.ChildCount+1)
	return b.saveBlock(briefs.TrieRefBlock(parent), pbuf)
}

// trieCreateChild allocates a child node, sets its depth/byte_val/node_type,
// and links it to the parent.  Mirrors briefs_trie_create_child (trie.c:227).
func (b *BrieFS) trieCreateChild(parent uint64, depth, byteVal, nodeType uint8, nameLen int) (uint64, error) {
	child, err := b.trieAllocNode(nameLen)
	if err != nil || briefs.TrieRefIsNull(child) {
		return 0, err
	}
	cbuf, _, err := b.trieRead(child)
	if err != nil {
		_ = b.trieFreeNode(child)
		return 0, err
	}
	cs := trieSlotBuf(cbuf, uint(briefs.TrieRefSlot(child)))
	slotSetDepth(cs, depth)
	slotSetByteVal(cs, byteVal)
	slotSetNodeType(cs, nodeType)
	if err := b.saveBlock(briefs.TrieRefBlock(child), cbuf); err != nil {
		_ = b.trieFreeNode(child)
		return 0, err
	}
	if err := b.trieLinkChild(parent, child); err != nil {
		_ = b.trieFreeNode(child)
		return 0, err
	}
	return child, nil
}

// trieFindOrCreateChild finds a child by byte_val, creating an INTERM child if
// absent.  Mirrors briefs_trie_find_or_create_child (trie.c:262).
func (b *BrieFS) trieFindOrCreateChild(parent uint64, depth, byteVal uint8) (uint64, error) {
	child, err := b.trieFindChild(parent, byteVal)
	if err != nil {
		return 0, err
	}
	if !briefs.TrieRefIsNull(child) {
		return child, nil
	}
	return b.trieCreateChild(parent, depth, byteVal, trieNodeTypeInterm, 0)
}

// trieFindChildWithPrev finds a child by byte_val, returning the child and its
// previous sibling (0 if it is the first child).  Mirrors
// trie_find_child_with_prev (trie.c:127).
func (b *BrieFS) trieFindChildWithPrev(parent uint64, byteVal uint8) (child, prev uint64, found bool, err error) {
	_, pnode, err := b.trieRead(parent)
	if err != nil {
		return 0, 0, false, err
	}
	cur := pnode.FirstChild
	var prevRef uint64
	for !briefs.TrieRefIsNull(cur) {
		_, cnode, err := b.trieRead(cur)
		if err != nil {
			return 0, 0, false, err
		}
		if cnode.ByteVal == byteVal {
			return cur, prevRef, true, nil
		}
		prevRef = cur
		cur = cnode.NextSibling
	}
	return 0, 0, false, nil
}

// trieUnlinkChild unlinks a child from the parent's sibling chain and decrements
// the parent's child_count.  Mirrors trie_unlink_child (trie.c:477).
func (b *BrieFS) trieUnlinkChild(parent, childPrev, child uint64) error {
	pbuf, pnode, err := b.trieRead(parent)
	if err != nil {
		return err
	}
	_, cnode, err := b.trieRead(child)
	if err != nil {
		return err
	}
	next := cnode.NextSibling
	ps := trieSlotBuf(pbuf, uint(briefs.TrieRefSlot(parent)))
	if briefs.TrieRefIsNull(childPrev) {
		slotSetFirstChild(ps, next)
	} else {
		pvbuf, pnode2, err := b.trieRead(childPrev)
		if err != nil {
			return err
		}
		pvs := trieSlotBuf(pvbuf, uint(briefs.TrieRefSlot(childPrev)))
		slotSetNextSibling(pvs, next)
		if err := b.saveBlock(briefs.TrieRefBlock(childPrev), pvbuf); err != nil {
			return err
		}
		_ = pnode2
	}
	slotSetChildCount(ps, pnode.ChildCount-1)
	return b.saveBlock(briefs.TrieRefBlock(parent), pbuf)
}

// trieSplitLeaf splits a pure leaf at the given position.  Mirrors
// trie_split_leaf (trie.c:519).
func (b *BrieFS) trieSplitLeaf(cur, child uint64, pos int, bval uint8, name string, ino uint64, ftype uint8) (uint64, error) {
	nameLen := len(name)
	lbuf, lnode, err := b.trieRead(child)
	if err != nil {
		return 0, err
	}
	oldNameLen := int(lnode.NameLen) - 2

	if oldNameLen == pos+1 {
		// Old leaf is a prefix of the new name: promote it to INTERM|LEAF.
		ls := trieSlotBuf(lbuf, uint(briefs.TrieRefSlot(child)))
		slotSetNodeType(ls, trieNodeTypeInterm|trieNodeStatusLeaf)
		slotSetDepth(ls, uint8(pos+1))
		if err := b.saveBlock(briefs.TrieRefBlock(child), lbuf); err != nil {
			return 0, err
		}
		return child, nil
	}

	oldSibling := lnode.NextSibling
	internal, err := b.trieCreateChild(cur, uint8(pos+1), bval, trieNodeTypeInterm, 0)
	if err != nil || briefs.TrieRefIsNull(internal) {
		return 0, syscall.ENOSPC
	}

	// Reparent: redirect cur's link to child -> internal.
	gbuf, gnode, err := b.trieRead(cur)
	if err != nil {
		return 0, err
	}
	gs := trieSlotBuf(gbuf, uint(briefs.TrieRefSlot(cur)))
	if gnode.FirstChild == child {
		slotSetFirstChild(gs, internal)
	} else {
		w := gnode.FirstChild
		for !briefs.TrieRefIsNull(w) {
			wbuf, wnode, err := b.trieRead(w)
			if err != nil {
				break
			}
			if wnode.NextSibling == child {
				ws := trieSlotBuf(wbuf, uint(briefs.TrieRefSlot(w)))
				slotSetNextSibling(ws, internal)
				if err := b.saveBlock(briefs.TrieRefBlock(w), wbuf); err != nil {
					return 0, err
				}
				break
			}
			w = wnode.NextSibling
		}
	}
	if err := b.saveBlock(briefs.TrieRefBlock(cur), gbuf); err != nil {
		return 0, err
	}

	// Link the old leaf as the internal node's child.
	ibuf, _, err := b.trieRead(internal)
	if err == nil {
		is := trieSlotBuf(ibuf, uint(briefs.TrieRefSlot(internal)))
		slotSetFirstChild(is, child)
		slotSetNextSibling(is, oldSibling)
		slotSetChildCount(is, 1)
		if err := b.saveBlock(briefs.TrieRefBlock(internal), ibuf); err != nil {
			return 0, err
		}
	}

	// If split at the last byte, store the new name on the internal node.
	if pos == nameLen-1 {
		ibuf2, _, err := b.trieRead(internal)
		if err == nil {
			is2 := trieSlotBuf(ibuf2, uint(briefs.TrieRefSlot(internal)))
			slotSetNodeType(is2, trieNodeTypeInterm|trieNodeStatusLeaf)
			slotSetFType(is2, ftype)
			slotSetInode(is2, ino)
			if err := b.saveBlock(briefs.TrieRefBlock(internal), ibuf2); err != nil {
				return 0, err
			}
		}
		if err := b.trieStoreName(internal, name); err != nil {
			return 0, err
		}
	}
	return internal, nil
}

// trieCreateRoot creates the root node of a directory trie.  Mirrors
// briefs_trie_create_root (trie.c:285).
func (b *BrieFS) trieCreateRoot(di *briefs.Inode) error {
	ref, err := b.triePageInit(0, 0, trieNodeTypeInterm)
	if err != nil {
		return err
	}
	di.DirTrieRoot = ref
	return nil
}

// TrieInsert adds a name/inode entry to the directory trie.  Mirrors
// briefs_trie_insert (trie.c:605).  Returns syscall.EEXIST on a duplicate.
func (b *BrieFS) TrieInsert(di *briefs.Inode, name string, ino uint64, ftype uint8) error {
	nameLen := len(name)
	if nameLen > briefs.BrieFSMaxNameLen {
		return syscall.ENAMETOOLONG
	}
	if briefs.TrieRefIsNull(di.DirTrieRoot) {
		if err := b.trieCreateRoot(di); err != nil {
			return err
		}
	}
	cur := di.DirTrieRoot

	for pos := 0; pos < nameLen; pos++ {
		bval := name[pos]

		if pos == nameLen-1 {
			existing, err := b.trieFindChild(cur, bval)
			if err != nil {
				return err
			}
			if !briefs.TrieRefIsNull(existing) {
				cbuf, cnode, err := b.trieRead(existing)
				if err != nil {
					return err
				}
				cs := trieSlotBuf(cbuf, uint(briefs.TrieRefSlot(existing)))
				if cnode.NodeType&trieNodeTypeInterm != 0 {
					if cnode.NodeType&trieNodeStatusLeaf != 0 {
						ename, _ := readTrieLeafName(cbuf, b.blockSize, cnode)
						if len(ename) == nameLen && string(ename) == name {
							return syscall.EEXIST
						}
					}
					slotSetNodeType(cs, cnode.NodeType|trieNodeStatusLeaf)
					slotSetFType(cs, ftype)
					slotSetInode(cs, ino)
					if err := b.saveBlock(briefs.TrieRefBlock(existing), cbuf); err != nil {
						return err
					}
					return b.trieStoreName(existing, name)
				}
				// Existing pure leaf: check duplicate, then split.
				ename, _ := readTrieLeafName(cbuf, b.blockSize, cnode)
				if len(ename) == nameLen && string(ename) == name {
					return syscall.EEXIST
				}
				cur, err = b.trieSplitLeaf(cur, existing, pos, bval, name, ino, ftype)
				if err != nil {
					return err
				}
				continue
			}

			// No existing child: create a new pure leaf.
			newLeaf, err := b.trieCreateChild(cur, uint8(pos), bval, 0, nameLen)
			if err != nil || briefs.TrieRefIsNull(newLeaf) {
				return syscall.ENOSPC
			}
			lbuf, _, err := b.trieRead(newLeaf)
			if err != nil {
				_ = b.trieFreeNode(newLeaf)
				return err
			}
			ls := trieSlotBuf(lbuf, uint(briefs.TrieRefSlot(newLeaf)))
			slotSetNodeType(ls, 0)
			slotSetFType(ls, ftype)
			slotSetInode(ls, ino)
			if err := b.saveBlock(briefs.TrieRefBlock(newLeaf), lbuf); err != nil {
				return err
			}
			return b.trieStoreName(newLeaf, name)
		}

		// Middle byte: find or create an INTERM child.
		child, err := b.trieFindOrCreateChild(cur, uint8(pos+1), bval)
		if err != nil || briefs.TrieRefIsNull(child) {
			return syscall.ENOSPC
		}
		_, cnode, err := b.trieRead(child)
		if err != nil {
			return err
		}
		if cnode.NodeType&trieNodeTypeInterm == 0 {
			cur, err = b.trieSplitLeaf(cur, child, pos, bval, name, ino, ftype)
			if err != nil {
				return err
			}
			continue
		}
		cur = child
	}
	return nil
}

// TrieUpdateEntry updates the inode and file type of an existing entry.  Mirrors
// briefs_trie_update_entry (trie.c:724).  Returns syscall.ENOENT if the name is
// not present as a leaf.
func (b *BrieFS) TrieUpdateEntry(di *briefs.Inode, name string, newIno uint64, newType uint8) error {
	nameLen := len(name)
	if briefs.TrieRefIsNull(di.DirTrieRoot) {
		return syscall.ENOENT
	}
	cur := di.DirTrieRoot
	for pos := 0; pos < nameLen; pos++ {
		child, err := b.trieFindChild(cur, name[pos])
		if err != nil || briefs.TrieRefIsNull(child) {
			return syscall.ENOENT
		}
		cbuf, cnode, err := b.trieRead(child)
		if err != nil {
			return err
		}
		if pos == nameLen-1 {
			if trieIsLeafNode(cnode.NodeType) {
				ename, _ := readTrieLeafName(cbuf, b.blockSize, cnode)
				if len(ename) == nameLen && string(ename) == name {
					cs := trieSlotBuf(cbuf, uint(briefs.TrieRefSlot(child)))
					slotSetInode(cs, newIno)
					slotSetFType(cs, newType)
					return b.saveBlock(briefs.TrieRefBlock(child), cbuf)
				}
			}
			return syscall.ENOENT
		}
		if cnode.NodeType&trieNodeTypeInterm == 0 {
			// Pure leaf where an INTERM is needed; caller must remove+insert.
			return syscall.ENOENT
		}
		cur = child
	}
	return syscall.ENOENT
}

// TrieRemove removes an entry from the directory trie, collapsing empty
// intermediate nodes afterward.  Mirrors briefs_trie_remove (trie.c:803).
// Freed nodes are freed immediately (the kernel defers to drop trie_lock; the
// FUSE bridge holds the global mu throughout).
func (b *BrieFS) TrieRemove(di *briefs.Inode, name string) error {
	nameLen := len(name)
	if briefs.TrieRefIsNull(di.DirTrieRoot) {
		return syscall.ENOENT
	}
	const ancestryLimit = 256
	ancestry := make([]uint64, 0, ancestryLimit+1)
	cur := di.DirTrieRoot
	anc := 0
	ancestry = append(ancestry, cur)
	anc++

	for pos := 0; pos < nameLen; pos++ {
		child, childPrev, found, err := b.trieFindChildWithPrev(cur, name[pos])
		if err != nil {
			return err
		}
		if !found {
			return syscall.ENOENT
		}

		if pos == nameLen-1 {
			cbuf, cnode, err := b.trieRead(child)
			if err != nil {
				return err
			}
			if cnode.NodeType&trieNodeTypeInterm != 0 && cnode.NodeType&trieNodeStatusLeaf == 0 {
				// Name is a prefix of longer entries but not itself an entry.
				return syscall.ENOENT
			}
			if cnode.NodeType&trieNodeTypeInterm != 0 {
				hasChildren := !briefs.TrieRefIsNull(cnode.FirstChild) || cnode.ChildCount != 0
				cs := trieSlotBuf(cbuf, uint(briefs.TrieRefSlot(child)))
				slotSetNodeType(cs, cnode.NodeType&^trieNodeStatusLeaf)
				if hasChildren {
					return b.saveBlock(briefs.TrieRefBlock(child), cbuf)
				}
				if err := b.saveBlock(briefs.TrieRefBlock(child), cbuf); err != nil {
					return err
				}
				if err := b.trieUnlinkChild(cur, childPrev, child); err != nil {
					return err
				}
				if err := b.trieFreeNode(child); err != nil {
					return err
				}
				return b.collapseAncestry(di, ancestry, anc)
			}
			// Pure leaf: unlink and free.
			if err := b.trieUnlinkChild(cur, childPrev, child); err != nil {
				return err
			}
			if err := b.trieFreeNode(child); err != nil {
				return err
			}
			return b.collapseAncestry(di, ancestry, anc)
		}

		cur = child
		if anc < ancestryLimit {
			ancestry = append(ancestry, cur)
			anc++
		}
	}
	return syscall.ENOENT
}

// collapseAncestry walks the ancestry from the removed leaf's parent up to the
// root, freeing empty intermediate nodes, and frees the root if it becomes an
// empty intermediate.  Mirrors the collapse loop in briefs_trie_remove
// (trie.c:907-994).
func (b *BrieFS) collapseAncestry(di *briefs.Inode, ancestry []uint64, anc int) error {
	for i := anc - 1; i >= 1; i-- {
		check := ancestry[i]
		pblk := ancestry[i-1]
		_, cnode, err := b.trieRead(check)
		if err != nil {
			break
		}
		if cnode.NodeType&trieNodeTypeInterm == 0 ||
			cnode.NodeType&trieNodeStatusLeaf != 0 ||
			cnode.ChildCount != 0 ||
			!briefs.TrieRefIsNull(cnode.FirstChild) {
			break
		}
		// Unlink check from its parent pblk.
		pbuf, pnode, err := b.trieRead(pblk)
		if err != nil {
			break
		}
		ps := trieSlotBuf(pbuf, uint(briefs.TrieRefSlot(pblk)))
		if pnode.FirstChild == check {
			slotSetFirstChild(ps, cnode.NextSibling)
		} else {
			w := pnode.FirstChild
			for !briefs.TrieRefIsNull(w) {
				wbuf, wnode, err := b.trieRead(w)
				if err != nil {
					break
				}
				if wnode.NextSibling == check {
					ws := trieSlotBuf(wbuf, uint(briefs.TrieRefSlot(w)))
					slotSetNextSibling(ws, cnode.NextSibling)
					_ = b.saveBlock(briefs.TrieRefBlock(w), wbuf)
					break
				}
				w = wnode.NextSibling
			}
		}
		slotSetChildCount(ps, pnode.ChildCount-1)
		if err := b.saveBlock(briefs.TrieRefBlock(pblk), pbuf); err != nil {
			return err
		}
		if err := b.trieFreeNode(check); err != nil {
			return err
		}
	}

	// If the root is now an empty intermediate, free it and clear the trie root.
	if !briefs.TrieRefIsNull(di.DirTrieRoot) {
		_, rnode, err := b.trieRead(di.DirTrieRoot)
		if err == nil {
			if rnode.NodeType&trieNodeTypeInterm != 0 &&
				rnode.NodeType&trieNodeStatusLeaf == 0 &&
				rnode.ChildCount == 0 &&
				briefs.TrieRefIsNull(rnode.FirstChild) {
				root := di.DirTrieRoot
				di.DirTrieRoot = 0
				return b.trieFreeNode(root)
			}
		}
	}
	return nil
}