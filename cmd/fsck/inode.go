package main

import (
	"encoding/binary"
	"fmt"
	"os"

	"github.com/ctdk/briefs-utils/briefs"
)

// verifyInode checks a single inode from an already-read buffer.
// Returns the parsed inode if valid, or an error.
func verifyInode(buf []byte, ino, byteOffset, inodeSize uint64) (*briefs.Inode, error) {
	inodeBuf := buf[byteOffset : byteOffset+inodeSize]
	magic := binary.LittleEndian.Uint64(inodeBuf[8:])
	if magic == 0 {
		return nil, nil // unallocated inode
	}
	if magic != briefs.MagicInode {
		return nil, fmt.Errorf("ino %d: bad magic 0x%016X", ino, magic)
	}

	// Use the existing UnmarshalInode for full parsing
	in, err := briefs.UnmarshalInode(inodeBuf)
	if err != nil {
		return nil, fmt.Errorf("ino %d: unmarshal failed: %w", ino, err)
	}

	// Validate inode number matches
	if in.InodeNumber != ino {
		return nil, fmt.Errorf("ino %d: stored inode number mismatch (%d)", ino, in.InodeNumber)
	}

	// Validate inline-data inodes
	if in.Flags&briefs.InodeFlagInlineData != 0 {
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
	} else if in.Flags&briefs.InodeFlagIndexed != 0 {
		// Tree-backed: extents live in a B+ tree rooted at ExtentInlineBase.
		// On spill the kernel zeroes the inline array and NumExtentsInline.
		if in.NumExtentsInline != 0 {
			return nil, fmt.Errorf("ino %d: tree-backed inode has %d inline extents (must be 0)", ino, in.NumExtentsInline)
		}
		if in.ExtentInlineBase == 0 {
			return nil, fmt.Errorf("ino %d: tree-backed inode has no root (extent_inline_base=0)", ino)
		}
		if in.NumExtentsTotal == 0 {
			return nil, fmt.Errorf("ino %d: tree-backed inode claims 0 extents (should be inline-only)", ino)
		}
	} else {
		// Inline-only: up to 8 extents in the inode.
		if in.NumExtentsInline > 8 {
			return nil, fmt.Errorf("ino %d: too many inline extents %d", ino, in.NumExtentsInline)
		}
		if in.NumExtentsTotal < uint64(in.NumExtentsInline) {
			return nil, fmt.Errorf("ino %d: total extents %d < inline extents %d", ino, in.NumExtentsTotal, in.NumExtentsInline)
		}
		if in.ExtentInlineBase != 0 {
			return nil, fmt.Errorf("ino %d: inline-only inode has extent_inline_base %d (must be 0)", ino, in.ExtentInlineBase)
		}
	}

	// Validate file mode
	mode := in.Filemode
	if mode == 0 {
		return nil, fmt.Errorf("ino %d: zero file mode", ino)
	}
	if mode&briefs.ModeDir == 0 && mode&briefs.ModeFile == 0 && mode&briefs.ModeSymlink == 0 {
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
	fs.inodes = make(map[uint64]*briefs.Inode)
	fs.dirs = nil
	fs.usedBlocks = make(map[uint64]bool)
	fs.entryCounts = make(map[uint64]int)
	fs.failedTrieDirs = make(map[uint64]bool)
	fs.failedBtreeInos = make(map[uint64]bool)

	fmt.Fprintf(os.Stderr, "  inodes per block: %d\n", inodesPerBlock)

	// Read the inode allocator L2 bitmap so the scan below validates only the
	// slots it marks allocated.  mkfs.briefs no longer pre-zeroes the inode
	// table (it would write a device-proportionally-huge region up front and
	// EIO on large volumes -- generic/620), so an unallocated slot on a reused
	// device simply retains the previous filesystem's stale bytes, including a
	// valid inode magic.  Validating those would flood fsck with phantom
	// inodes and false "bad magic" reports.  The kernel only ever consults a
	// slot once it allocates it (writing the inode fresh), so free-slot
	// content is meaningless at runtime.  The verifyInodeBitmapCrossReference
	// pass still flags an allocated slot whose magic is missing/invalid.  If
	// the bitmap read fails, fall back to the legacy behavior of validating
	// every non-zero-magic slot.
	l2, _, _, bitmapErr := readAllocatorL2(fs.file, fs.sb.InodeBMOffset, blockSize)
	haveBitmap := bitmapErr == nil
	if !haveBitmap {
		fs.errorf("read inode bitmap for table scan: %v (will scan all slots)", bitmapErr)
	}

	for bi := uint64(0); bi < inodeTableBlocks; bi++ {
		buf := make([]byte, blockSize)
		if _, err := fs.file.ReadAt(buf, int64((inodeTableBlock+bi)*blockSize)); err != nil {
			fs.errorf("read inode table block %d: %v", inodeTableBlock+bi, err)
			ino += inodesPerBlock
			continue
		}

		for j := uint64(0); j < inodesPerBlock; j++ {
			offset := j * inodeSize

			// Skip slots the inode bitmap marks free.  "allocated" mirrors
			// verifyInodeBitmapCrossReference: bit clear in the L2 word.
			if haveBitmap {
				w := (ino - 1) / wordBits
				b := (ino - 1) % wordBits
				if w >= uint64(len(l2)) || (l2[w]&(1<<b)) != 0 {
					ino++
					continue
				}
			}

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

				// Deep structural checks the basic walk skips (separator
				// ordering, child range/level, cross-leaf ordering, extent
				// count). Runs only for tree-backed inodes whose basic walk
				// succeeded; populates failedBtreeInos on structural faults.
				verifyBtreeStructures(fs, ino, in, blockSize)

				// Collect trie root for directory trie walking
				if in.IsDir() {
					if in.DirTrieRoot == 0 {
						fs.errorf("ino %d: directory with no trie root", ino)
					} else {
						fs.dirs = append(fs.dirs, dirInfo{ino: ino, trieRoot: in.DirTrieRoot})
						fs.usedBlocks[briefs.TrieRefBlock(in.DirTrieRoot)] = true
					}
				}

				// File with zero size but extents
				if in.IsFile() && in.FileSize == 0 && in.NumExtentsTotal > 0 {
					fs.warnf("ino %d: file with zero size but %d extents", ino, in.NumExtentsTotal)
				}
				// Non-inline file with non-zero size but no extents
				if in.IsFile() && in.FileSize > 0 && in.NumExtentsTotal == 0 && in.Flags&briefs.InodeFlagInlineData == 0 {
					fs.warnf("ino %d: file with size %d but no extents (not inline)", ino, in.FileSize)
				}
			}
			ino++
		}
	}

	return
}

// collectInodeExtents collects all blocks referenced by an inode's extents
// (and, for tree-backed inodes, the B-tree node blocks themselves) into
// fs.usedBlocks for cross-referencing against the allocator bitmap.
func collectInodeExtents(fs *fsckState, ino uint64, in *briefs.Inode, blockSize uint64) {
	// Inline-data inodes reference no data blocks.
	if in.Flags&briefs.InodeFlagInlineData != 0 {
		return
	}

	// Record the blocks from a single extent (skips hole extents).
	addExtentBlocks := func(ext briefs.Extent) error {
		if ext.Flags&briefs.ExtentFlagHole != 0 {
			return nil
		}
		if ext.Flags&^(uint32(briefs.ExtentFlagHole|briefs.ExtentFlagEof)) != 0 {
			fs.warnf("ino %d: extent with unknown flags 0x%08X (phys=%d, len=%d)",
				ino, ext.Flags, ext.Phys, ext.Len)
		}
		if ext.Len > 0 && ext.Phys > 0 {
			for bk := uint64(0); bk < ext.Len; bk++ {
				fs.usedBlocks[ext.Phys+bk] = true
			}
		}
		return nil
	}

	// Record every B-tree node block as used metadata.
	addNodeBlock := func(block uint64) error {
		fs.usedBlocks[block] = true
		return nil
	}

	if err := briefs.IterateInodeExtents(fs.file, in, blockSize, briefs.InodeExtentVisitor{
		VisitNode:   addNodeBlock,
		VisitExtent: addExtentBlocks,
	}); err != nil {
		fs.errorf("ino %d: %v", ino, err)
		// Record tree-backed inodes whose B+ tree walk failed. Their extents
		// and node blocks were not reached, so they are absent from usedBlocks;
		// an allocator rebuild would free them. Inline-only walk errors (the
		// inline-array path) don't carry this destructive risk, so only flag
		// InodeFlagIndexed inodes here.
		if in.Flags&briefs.InodeFlagIndexed != 0 {
			fs.failedBtreeInos[ino] = true
		}
	}
}

// writeModifiedInodes writes any inodes staged in the repair plan back to
// their fixed slots in the inode table.
func writeModifiedInodes(file *os.File, sb *briefs.SuperblockLayout, plan *repairPlan, blockSize uint64) error {
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
