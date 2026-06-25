package main

import (
	"encoding/binary"

	"github.com/ctdk/briefs-utils/briefs"
)

// XATTR block layout (mirrors briefs.h):
//
//	[0,16)       briefs_xattr_header { magic, version, used_size, entry_count }
//	[16,...)      briefs_xattr_entry[entry_count] (8 bytes each:
//	               name_len, value_len, name_offset, value_offset, all __le16)
//	[name/value]  names and values, 4-byte aligned, names include the
//	               namespace prefix (e.g. "user.foo")
//	[4080,4088)   CRC32C over [0,4080)  (same convention as B-tree nodes)
//	[4088,4096)   slack
//
// used_size is the number of bytes [0,used) actually holding header+entries+
// name/value data (the rest of the block is zeroed).  It is capped at
// BRIEFS_XATTR_MAX_USED (4044): a journal JRN_XATTR_DATA record must fit in one
// 4096-byte journal block, and its fixed prefix is 20 bytes, leaving 4064
// bytes for the whole record and 4044 for the block content.
const (
	xattrHeaderSize = 16
	xattrEntrySize  = 8
	xattrMaxUsed    = 4044
)

// verifyXattrBlock validates an inode's external xattr block, when present,
// and records its absolute block number in fs.usedBlocks so the block
// cross-reference (cmd/fsck/crossref.go) treats it as allocated metadata:
// a referenced-but-unallocated block is flagged an orphan, an
// allocated-but-unreferenced block a leak.  A no-op when the inode has no
// xattr block (XattrOffset == 0).
//
// Called from verifyInodeTable after collectInodeExtents for every allocated
// inode.
func verifyXattrBlock(fs *fsckState, ino uint64, in *briefs.Inode, blockSize uint64) {
	if in.XattrOffset == 0 {
		return
	}

	// xattr_offset is an absolute block number (matches ext->phys, the trie
	// root, and extent_inline_base).
	abs := in.XattrOffset
	buf := make([]byte, blockSize)
	if _, err := fs.file.ReadAt(buf, int64(abs*blockSize)); err != nil {
		fs.errorf("ino %d: read xattr block %d: %v", ino, abs, err)
		return
	}

	magic := binary.LittleEndian.Uint32(buf[0:4])
	if magic != briefs.MagicXattr {
		fs.errorf("ino %d: xattr block %d bad magic 0x%08X (expected 0x%08X)",
			ino, abs, magic, briefs.MagicXattr)
		return
	}
	used := binary.LittleEndian.Uint32(buf[8:12])
	if used > xattrMaxUsed || uint64(used) > blockSize {
		fs.errorf("ino %d: xattr block %d used_size %d out of range (max %d)",
			ino, abs, used, xattrMaxUsed)
		return
	}
	entryCount := binary.LittleEndian.Uint32(buf[12:16])
	// The entry array must fit inside the used region.
	if uint64(entryCount)*xattrEntrySize+xattrHeaderSize > uint64(used) {
		fs.errorf("ino %d: xattr block %d entry_count %d exceeds used_size %d",
			ino, abs, entryCount, used)
		return
	}

	// CRC32C over [0, 4080), stored at offset 4080 -- the same coverage the
	// kernel writes (briefs_chain_checksum) and the B-tree nodes use.  A zero
	// stored CRC is treated as legacy/valid by VerifyChainChecksum.
	if err := briefs.VerifyChainChecksum(buf, blockSize); err != nil {
		fs.errorf("ino %d: xattr block %d CRC mismatch: %v", ino, abs, err)
		return
	}

	// Walk the entries: every name/value offset/length must land in the used
	// region and the name length must be non-zero.  This catches a torn or
	// partially-written block that still has a valid magic + CRC (CRC covers
	// [0,4080), so a torn tail beyond used_size but inside 4080 would already
	// have failed the CRC; this catches logical corruption within the CRC'd
	// region).
	entries := buf[xattrHeaderSize:]
	for i := uint32(0); i < entryCount; i++ {
		base := i * xattrEntrySize
		nameLen := uint32(binary.LittleEndian.Uint16(entries[base+0 : base+2]))
		valueLen := uint32(binary.LittleEndian.Uint16(entries[base+2 : base+4]))
		nameOff := uint32(binary.LittleEndian.Uint16(entries[base+4 : base+6]))
		valueOff := uint32(binary.LittleEndian.Uint16(entries[base+6 : base+8]))

		if nameLen == 0 {
			fs.errorf("ino %d: xattr block %d entry %d has zero name_len",
				ino, abs, i)
			return
		}
		if uint64(nameOff)+uint64(nameLen) > uint64(used) {
			fs.errorf("ino %d: xattr block %d entry %d name [%d,%d) outside used_size %d",
				ino, abs, i, nameOff, nameOff+nameLen, used)
			return
		}
		if valueLen > 0 {
			if valueOff == 0 ||
				uint64(valueOff)+uint64(valueLen) > uint64(used) {
				fs.errorf("ino %d: xattr block %d entry %d value [%d,%d) outside used_size %d",
					ino, abs, i, valueOff, valueOff+valueLen, used)
				return
			}
		}
	}

	// Cross-reference: treat the xattr block as allocated metadata so the
	// orphan/leak passes account for it (it lives in the data block pool,
	// allocated by briefs_alloc_block alongside trie pages and B-tree nodes).
	fs.usedBlocks[abs] = true

	if entryCount == 0 {
		fs.warnf("ino %d: xattr block %d has zero entries (should have been freed)",
			ino, abs)
	}
}
