package main

import (
	"encoding/binary"
	"fmt"
	"os"

	"github.com/ctdk/briefs-utils/types"
)

// verifyInode checks a single inode from an already-read buffer.
// Returns the parsed inode if valid, or an error.
func verifyInode(buf []byte, ino, byteOffset, inodeSize uint64) (*types.Inode, error) {
	inodeBuf := buf[byteOffset : byteOffset+inodeSize]
	magic := binary.LittleEndian.Uint64(inodeBuf[8:])
	if magic == 0 {
		return nil, nil // unallocated inode
	}
	if magic != types.MagicInode {
		return nil, fmt.Errorf("ino %d: bad magic 0x%016X", ino, magic)
	}

	// Use the existing UnmarshalInode for full parsing
	in, err := types.UnmarshalInode(inodeBuf)
	if err != nil {
		return nil, fmt.Errorf("ino %d: unmarshal failed: %w", ino, err)
	}

	// Validate inode number matches
	if in.InodeNumber != ino {
		return nil, fmt.Errorf("ino %d: stored inode number mismatch (%d)", ino, in.InodeNumber)
	}

	// Validate inline-data inodes
	if in.Flags&types.InodeFlagInlineData != 0 {
		if in.FileSize > 256 {
			return nil, fmt.Errorf("ino %d: inline-data file size %d > 256", ino, in.FileSize)
		}
		if in.NumExtentsTotal != 0 {
			return nil, fmt.Errorf("ino %d: inline-data inode has %d extents", ino, in.NumExtentsTotal)
		}
		if in.NumExtentsInline != 0 {
			return nil, fmt.Errorf("ino %d: inline-data inode has %d inline extents", ino, in.NumExtentsInline)
		}
		if in.ExtentInlineBase != 0 {
			return nil, fmt.Errorf("ino %d: inline-data inode has extent_inline_base %d", ino, in.ExtentInlineBase)
		}
		if in.IsDir() {
			return nil, fmt.Errorf("ino %d: directory cannot use inline data", ino)
		}
	} else {
		// Validate extent counts
		if in.NumExtentsInline > 8 {
			return nil, fmt.Errorf("ino %d: too many inline extents %d", ino, in.NumExtentsInline)
		}
		if in.NumExtentsTotal < uint64(in.NumExtentsInline) {
			return nil, fmt.Errorf("ino %d: total extents %d < inline extents %d", ino, in.NumExtentsTotal, in.NumExtentsInline)
		}
	}

	// Validate file mode
	mode := in.Filemode
	if mode == 0 {
		return nil, fmt.Errorf("ino %d: zero file mode", ino)
	}
	if mode&types.ModeDir == 0 && mode&types.ModeFile == 0 && mode&types.ModeSymlink == 0 {
		// Not a dir, file, or symlink — could be a special device, which is fine
	}

	// Validate xattr fields (no BrieFS code writes xattrs yet, so these
	// should always be zero on a healthy filesystem).
	if in.XattrOffset != 0 || in.XattrSize != 0 {
		// Record the xattr offset for later bitmap cross-referencing
		// (the caller will track used blocks, but we just flag it here)
		return in, fmt.Errorf("ino %d: unexpected xattr_offset=%d, xattr_size=%d (xattr not yet implemented)",
			ino, in.XattrOffset, in.XattrSize)
	}

	return in, nil
}

// verifyInodeTable scans the inode table, validates each inode, and populates
// fsckState with inode metadata for cross-referencing.
func verifyInodeTable(fs *fsckState, inodeTableBlock, inodeTableBlocks, blockSize, inodeSize uint64) (totalInodes int) {
	inodesPerBlock := blockSize / inodeSize
	ino := uint64(1)

	// Initialize maps for cross-referencing
	fs.inodes = make(map[uint64]*types.Inode)
	fs.dirs = nil
	fs.usedBlocks = make(map[uint64]bool)
	fs.entryCounts = make(map[uint64]int)
	fs.failedTrieDirs = make(map[uint64]bool)

	fmt.Fprintf(os.Stderr, "  inodes per block: %d\n", inodesPerBlock)

	for bi := uint64(0); bi < inodeTableBlocks; bi++ {
		buf := make([]byte, blockSize)
		if _, err := fs.file.ReadAt(buf, int64((inodeTableBlock+bi)*blockSize)); err != nil {
			fs.errorf("read inode table block %d: %v", inodeTableBlock+bi, err)
			ino += inodesPerBlock
			continue
		}

		for j := uint64(0); j < inodesPerBlock; j++ {
			offset := j * inodeSize
			magic := binary.LittleEndian.Uint64(buf[offset+8:])
			if magic == 0 {
				ino++
				continue
			}

			totalInodes++
			in, err := verifyInode(buf, ino, offset, inodeSize)
			if err != nil {
				fs.errorf("%v", err)
			}
			if in != nil {
				// Even if verifyInode returned warnings (xattr, etc.),
				// we still record the inode data for cross-referencing.
				fs.inodes[ino] = in

				// Collect extents for block cross-reference
				collectInodeExtents(fs, ino, in, blockSize)

				// Collect trie root for directory trie walking
				if in.IsDir() {
					if in.DirTrieRoot == 0 {
						fs.errorf("ino %d: directory with no trie root", ino)
					} else {
						fs.dirs = append(fs.dirs, dirInfo{ino: ino, trieRoot: in.DirTrieRoot})
						fs.usedBlocks[types.TrieRefBlock(in.DirTrieRoot)] = true
					}
				}

				// File with zero size but extents
				if in.IsFile() && in.FileSize == 0 && in.NumExtentsTotal > 0 {
					fs.warnf("ino %d: file with zero size but %d extents", ino, in.NumExtentsTotal)
				}
				// Non-inline file with non-zero size but no extents
				if in.IsFile() && in.FileSize > 0 && in.NumExtentsTotal == 0 && in.Flags&types.InodeFlagInlineData == 0 {
					fs.warnf("ino %d: file with size %d but no extents (not inline)", ino, in.FileSize)
				}
			}
			ino++
		}
	}

	return
}

// collectInodeExtents collects all blocks referenced by an inode's extents,
// including both inline extents and overflow chain blocks.
func collectInodeExtents(fs *fsckState, ino uint64, in *types.Inode, blockSize uint64) {
	// Inline-data inodes reference no data blocks.
	if in.Flags&types.InodeFlagInlineData != 0 {
		return
	}

	// Helper to record the blocks from a single extent.
	// Skips hole extents (ExtentFlagHole) which have no physical backing.
	addExtentBlocks := func(ext types.Extent) {
		// Validate extent flags
		if ext.Flags&types.ExtentFlagHole != 0 {
			// Hole extent — no physical blocks, skip
			return
		}
		if ext.Flags&types.ExtentFlagEof != 0 {
			// EOF marker — should only appear on the last extent
			// (we can't easily verify that here, but it's valid)
		}
		if ext.Flags & ^(uint32(types.ExtentFlagHole|types.ExtentFlagEof)) != 0 {
			fs.warnf("ino %d: extent with unknown flags 0x%08X (phys=%d, len=%d)",
				ino, ext.Flags, ext.Phys, ext.Len)
		}

		if ext.Len > 0 && ext.Phys > 0 {
			for bk := uint64(0); bk < ext.Len; bk++ {
				fs.usedBlocks[ext.Phys+bk] = true
			}
		}
	}

	// Collect inline extents
	inlineExtents := in.InlineExtents()
	for ei := uint32(0); ei < in.NumExtentsInline; ei++ {
		addExtentBlocks(inlineExtents[ei])
	}

	// Collect overflow extents from chain blocks
	if in.NumExtentsTotal > uint64(in.NumExtentsInline) && in.ExtentInlineBase != 0 {
		extentsPerBlock := types.ExtentsPerChainBlock(blockSize)
		remaining := int(in.NumExtentsTotal) - int(in.NumExtentsInline)
		chainBlock := in.ExtentInlineBase

		for chainBlock != 0 && remaining > 0 {
			buf := make([]byte, blockSize)
			if _, err := fs.file.ReadAt(buf, int64(chainBlock*blockSize)); err != nil {
				fs.errorf("ino %d: read extent chain block %d: %v", ino, chainBlock, err)
				break
			}

			// Verify extent chain block checksum
			if err := types.VerifyChainChecksum(buf, blockSize); err != nil {
				fs.errorf("ino %d: extent chain block %d: checksum mismatch (stored=0x%08X computed=0x%08X)", ino, chainBlock, types.ReadChainChecksum(buf, blockSize), types.ComputeChainChecksum(buf, blockSize))
				break
			}

			hdr := types.UnmarshalExtentChainHeader(buf)

			// Validate extent count vs capacity
			if hdr.NumExtentsInBlock > uint32(extentsPerBlock) {
				fs.errorf("ino %d: extent chain block %d: %d extents exceeds block capacity %d",
					ino, chainBlock, hdr.NumExtentsInBlock, extentsPerBlock)
				break
			}

			// Record chain block itself as used (it's metadata)
			fs.usedBlocks[chainBlock] = true

			// Process extents in this chain block
			n := int(hdr.NumExtentsInBlock)
			if n > remaining {
				n = remaining
			}
			for i := 0; i < n; i++ {
				ext := types.ReadChainExtent(buf, i)
				addExtentBlocks(ext)
			}

			remaining -= n
			chainBlock = hdr.NextOverflowBlock
		}

		if remaining > 0 {
			fs.errorf("ino %d: extent chain ended early: %d extents left unreachable (total=%d, inline=%d)",
				ino, remaining, in.NumExtentsTotal, in.NumExtentsInline)
		}
	}
}

// writeModifiedInodes writes any inodes staged in the repair plan back to
// their fixed slots in the inode table.
func writeModifiedInodes(file *os.File, sb *types.SuperblockLayout, plan *repairPlan, blockSize uint64) error {
	inodesPerBlock := blockSize / sb.InodeSize
	for ino, in := range plan.inodes {
		blockOffset := sb.InodeTableOffset + (ino-1)/inodesPerBlock
		byteOffset := ((ino - 1) % inodesPerBlock) * sb.InodeSize
		off := int64(blockOffset*blockSize + byteOffset)
		if err := in.WriteAt(file, off); err != nil {
			return fmt.Errorf("write inode %d at offset %d: %w", ino, off, err)
		}
	}
	return nil
}
