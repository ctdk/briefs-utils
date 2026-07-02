package main

import (
	"encoding/binary"

	"github.com/ctdk/briefs-utils/briefs"
)

// XATTR block layout (mirrors briefs.h):
//
//	v1 header    [0,16)   { magic, version, used_size, entry_count }
//	v2 header    [0,32)   { magic, version, used_size, entry_count,
//	                       next_block, flags, reserved }
//	[hdr,...)    briefs_xattr_entry[entry_count] (8 bytes each:
//	             name_len, value_len, name_offset, value_offset, all __le16)
//	[name/value] names and values, 4-byte aligned, names include the
//	             namespace prefix (e.g. "user.foo")
//	[4080,4088)  CRC32C over [0,4080) (same convention as B-tree nodes)
//	[4088,4096)  slack
//
// used_size is the number of bytes [0,used) actually holding header+entries+
// name/value data (the rest of the block is zeroed).  It is capped at
// BRIEFS_XATTR_MAX_USED (4044): a journal JRN_XATTR_DATA record must fit in one
// 4096-byte journal block, and its fixed prefix is 20 bytes, leaving 4064
// bytes for the whole record and 4044 for the block content.
//
// Multiple blocks may be chained via v2 briefs_xattr_header.next_block; a block
// with the BRIEFS_XATTR_FLAG_CONT flag holds raw value bytes and no entries.
const (
	xattrEntrySize  = 8
	xattrMaxUsed    = 4044
	xattrMaxChain   = 1024
	xattrV1HdrSize  = 16
	xattrV2HdrSize  = 32
)

func xattrHeaderSize(version uint32) uint32 {
	if version == 1 {
		return xattrV1HdrSize
	}
	return xattrV2HdrSize
}

// verifyXattrBlock validates an inode's external xattr chain, when present,
// and records every block in fs.usedBlocks so the cross-reference
// (cmd/fsck/crossref.go) treats them as allocated metadata.  A no-op when the
// inode has no xattr block (XattrOffset == 0).
//
// Called from verifyInodeTable after collectInodeExtents for every allocated
// inode.
func verifyXattrBlock(fs *fsckState, ino uint64, in *briefs.Inode, blockSize uint64) {
	if in.XattrOffset == 0 {
		return
	}

	type xattrBlock struct {
		abs        uint64
		buf        []byte
		version    uint32
		hdrSize    uint32
		used       uint32
		entryCount uint32
		next       uint64
		cont       bool
	}

	visited := make(map[uint64]bool)
	var chain []xattrBlock
	block := in.XattrOffset

	// Pass 1: load the whole chain, validate per-block structure, and mark every
	// block in the cross-reference set.  We collect the chain first so pass 2 can
	// validate split values against the continuation blocks that follow their
	// entry block.
	for block != 0 {
		if len(visited) > xattrMaxChain {
			fs.errorf("ino %d: xattr chain exceeds max length %d", ino, xattrMaxChain)
			return
		}
		if visited[block] {
			fs.errorf("ino %d: xattr chain loop at block %d", ino, block)
			return
		}
		visited[block] = true

		abs := block
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
		version := binary.LittleEndian.Uint32(buf[4:8])
		if version != 1 && version != 2 {
			fs.errorf("ino %d: xattr block %d unsupported version %d",
				ino, abs, version)
			return
		}
		hdrSize := xattrHeaderSize(version)
		used := binary.LittleEndian.Uint32(buf[8:12])
		if used > xattrMaxUsed || used < hdrSize {
			fs.errorf("ino %d: xattr block %d used_size %d out of range [%d,%d]",
				ino, abs, used, hdrSize, xattrMaxUsed)
			return
		}
		entryCount := binary.LittleEndian.Uint32(buf[12:16])

		var nextBlock uint64
		var flags uint32
		if version == 2 {
			nextBlock = binary.LittleEndian.Uint64(buf[16:24])
			flags = binary.LittleEndian.Uint32(buf[24:28])
		}

		// CRC32C over [0, 4080), stored at offset 4080 -- the same coverage the
		// kernel writes (briefs_chain_checksum) and the B-tree nodes use.  A zero
		// stored CRC is treated as legacy/valid by VerifyChainChecksum.
		if err := briefs.VerifyChainChecksum(buf, blockSize); err != nil {
			fs.errorf("ino %d: xattr block %d CRC mismatch: %v", ino, abs, err)
			return
		}

		isCont := (flags & briefs.BrieFSXattrFlagCont) != 0
		if isCont {
			if entryCount != 0 {
				fs.errorf("ino %d: xattr continuation block %d has entry_count %d",
					ino, abs, entryCount)
				return
			}
		}

		chain = append(chain, xattrBlock{
			abs:        abs,
			buf:        buf,
			version:    version,
			hdrSize:    hdrSize,
			used:       used,
			entryCount: entryCount,
			next:       nextBlock,
			cont:       isCont,
		})

		// Cross-reference: treat the xattr block as allocated metadata so the
		// orphan/leak passes account for it.
		fs.usedBlocks[abs] = true

		if !isCont && entryCount == 0 {
			fs.warnf("ino %d: xattr block %d has zero entries (should have been freed)",
				ino, abs)
		}

		block = nextBlock
	}

	// Pass 2: validate entry records and split-value capacity.
	for b := range chain {
		bblk := &chain[b]
		if bblk.cont {
			continue
		}
		hdrSize := bblk.hdrSize
		used := bblk.used
		entryCount := bblk.entryCount

		// The entry array must fit inside the used region.
		if uint64(entryCount)*xattrEntrySize+uint64(hdrSize) > uint64(used) {
			fs.errorf("ino %d: xattr block %d entry_count %d exceeds used_size %d",
				ino, bblk.abs, entryCount, used)
			return
		}

		entries := bblk.buf[hdrSize:]
		var totalDemand uint64
		var totalInline uint64
		for i := uint32(0); i < entryCount; i++ {
			base := i * xattrEntrySize
			nameLen := uint32(binary.LittleEndian.Uint16(entries[base+0 : base+2]))
			valueLen := uint32(binary.LittleEndian.Uint16(entries[base+2 : base+4]))
			nameOff := uint32(binary.LittleEndian.Uint16(entries[base+4 : base+6]))
			valueOff := uint32(binary.LittleEndian.Uint16(entries[base+6 : base+8]))

			if nameLen == 0 {
				fs.errorf("ino %d: xattr block %d entry %d has zero name_len",
					ino, bblk.abs, i)
				return
			}
			if uint64(nameOff)+uint64(nameLen) > uint64(used) {
				fs.errorf("ino %d: xattr block %d entry %d name [%d,%d) outside used_size %d",
					ino, bblk.abs, i, nameOff, nameOff+nameLen, used)
				return
			}

			if valueLen == 0 {
				continue
			}
			totalDemand += uint64(valueLen)

			if valueOff == 0 {
				// Sentinel: the whole value is stored in continuation block(s).
				continue
			}

			// valueOff points into the entry block's payload, after the entry
			// array and the names/inline values that precede this entry.
			minValueOff := hdrSize + entryCount*xattrEntrySize
			if valueOff < minValueOff || valueOff > used {
				fs.errorf("ino %d: xattr block %d entry %d value_offset %d out of range [%d,%d]",
					ino, bblk.abs, i, valueOff, minValueOff, used)
				return
			}
			inlineCap := used - valueOff
			if valueLen <= inlineCap {
				totalInline += uint64(valueLen)
			} else {
				totalInline += uint64(inlineCap)
			}
		}

		// Capacity available in the contiguous continuation blocks that follow
		// this entry block.
		var contCap uint64
		for k := b + 1; k < len(chain) && chain[k].cont; k++ {
			contCap += uint64(chain[k].used - chain[k].hdrSize)
		}

		if totalDemand > totalInline+contCap {
			fs.errorf("ino %d: xattr block %d split value(s) need %d bytes but only %d inline + %d continuation bytes available",
				ino, bblk.abs, totalDemand, totalInline, contCap)
			return
		}
	}
}
