// mkfs.briefs creates a BrieFS filesystem given the relevant parameters.
package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"

	"github.com/ctdk/briefs-utils/device"
	"github.com/ctdk/briefs-utils/briefs"
	"github.com/urfave/cli/v3"
)

func roundUp(value, alignment uint64) uint64 {
	return (value + alignment - 1) / alignment * alignment
}

// stderrIsTerminal reports whether os.Stderr is an interactive terminal
// (a char device).  mkfs.briefs writes its human-facing progress/report to
// stderr; this gates that output so it shows up when a human runs mkfs on a
// terminal but is suppressed when stderr is redirected to a file or pipe.
// That matters under xfstests, whose `check` runs each test with
// `_run_seq >$tmp.out 2>&1`, merging the test's stderr into the output that is
// diffed against the expected .out -- so any mkfs chatter on stderr would leak
// into the compared result and spuriously fail tests (e.g. generic/732).
func stderrIsTerminal() bool {
	fi, err := os.Stderr.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// Calculate on-disk location of an inode.
// The inode table starts at: inode_table_offset
// (This matches what the kernel computes in briefs_iget.)
func calculateInodeLocation(sb *briefs.Superblock, inodeNum uint64) (blockOffset uint64, byteOffset uint64) {
	return briefs.InodeLocation(&sb.Lay, inodeNum)
}

func main() {
	app := &cli.Command{
		Name:     "mkfs.briefs",
		Usage:    "Create a new BrieFS filesystem",
		ArgsUsage: "DEVICE",
		Version:  briefs.VersionStr,
		Before: func(ctx context.Context, c *cli.Command) (context.Context, error) {
			if c.Args().Len() < 1 {
				return ctx, fmt.Errorf("missing required argument: DEVICE")
			}
			path := c.Args().First()
			if err := device.CheckMounted(path); err != nil {
				// Reformatting a mounted filesystem is incredibly
				// dangerous. Refuse to continue.
				return ctx, fmt.Errorf("refusing to create filesystem: %w\n", err)
			}
			return ctx, nil
		},
		Flags: []cli.Flag{
			&cli.Int64Flag{
				Name:     "size",
				Aliases:  []string{"s"},
				Value:    0,
				Usage:    "filesystem size in blocks",
			},
			&cli.IntFlag{
				Name:     "block-size",
				Aliases:  []string{"b"},
				Value:    4096,
				Usage:    "block size in bytes",
			},
			&cli.IntFlag{
				Name:     "inode-size",
				Aliases:  []string{"I"},
				Value:    512,
				Usage:    "inode size in bytes",
			},
			&cli.IntFlag{
				Name:     "journal-size",
				Aliases:  []string{"j"},
				Value:    0,
				Usage:    "journal size in blocks (0 = auto: scale with volume size)",
			},
			&cli.StringFlag{
				Name:     "label",
				Aliases:  []string{"L"},
				Value:    "BRIEFS",
				Usage:    "filesystem label",
			},
			&cli.IntFlag{
				Name:     "inode-ratio",
				Value:    8,
				Usage:    "blocks per inode (one inode per N blocks)",
			},
			&cli.StringFlag{
				Name: "uuid",
				Aliases: []string{"U"},
				Usage: "Specify a UUID for the new volume.",
			},
			&cli.BoolFlag{
				Name:     "force",
				Aliases:  []string{"f"},
				Usage:    "force overwrite of an existing filesystem",
			},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			path := c.Args().First()
			totalBlocks := c.Int64("size")
			blockSize := uint64(c.Int("block-size"))
			inodeSize := uint64(c.Int("inode-size"))
			journalBlocks := uint64(c.Int("journal-size"))
			label := c.String("label")
			inodeRatio := c.Int("inode-ratio")
			uuidStr := c.String("uuid")
			force := c.Bool("force")

			// Validate size parameters are non-negative
			if totalBlocks < 0 {
				return fmt.Errorf("size must be non-negative, got %d", totalBlocks)
			}
			if c.Int("block-size") < 0 {
				return fmt.Errorf("block-size must be non-negative, got %d", c.Int("block-size"))
			}
			if c.Int("inode-size") < 0 {
				return fmt.Errorf("inode-size must be non-negative, got %d", c.Int("inode-size"))
			}
			if c.Int("journal-size") < 0 {
				return fmt.Errorf("journal-size must be non-negative, got %d", c.Int("journal-size"))
			}

			// Enforce block size = 4096 (kernel hardcodes this value)
			if blockSize != 4096 {
				return fmt.Errorf("block-size must be 4096 bytes (kernel requirement), got %d", blockSize)
			}

			// Enforce minimum inode size (>= 512 bytes)
			if inodeSize < 512 {
				return fmt.Errorf("inode-size must be at least 512 bytes, got %d", inodeSize)
			}

			// Validate that blockSize and inodeSize are powers of two.
			if !isPowerOfTwo(blockSize) {
				return fmt.Errorf("block-size must be a power of two, which %d isn't", blockSize)
			}
			if !isPowerOfTwo(inodeSize) {
				return fmt.Errorf("inode-size must be a power of two, which %d isn't", inodeSize)
			}

			// Auto-scale journal size when not explicitly set.
			if journalBlocks == 0 {
				journalBlocks = briefs.DefaultJournalBlocks(uint64(totalBlocks))
			}

			// Enforce minimum journal size (>= 4 blocks for ring buffer to function)
			if journalBlocks < 4 {
				return fmt.Errorf("journal-size must be at least 4 blocks, got %d", journalBlocks)
			}

			if inodeRatio < 1 {
				return fmt.Errorf("inode-ratio must be at least 1, which %d isn't", inodeRatio)
			}

			// Probe device if no explicit size given
			if totalBlocks == 0 {
				bd, err := device.GetDevice(path, blockSize)
				if err != nil {
					return err
				}
				if stderrIsTerminal() {
					fmt.Fprintf(os.Stderr, "Probed device %s: %d bytes, %d blocks.\n",
						path, bd.Bytes(), bd.Blocks())
				}
				totalBlocks = bd.Blocks()
			}

			// --- Block Layout (matches kernel expectations) ---
			//
			// Block 0:       Superblock          (1 block)
			// Next:           Inode bitmap        (3-level pyramid, 1 bit per inode)
			// Next:           Inode table         (8 inodes per block)
			// Next:           EAT                 (reserved, 1 block)
			// Next:           Trie pool           (data allocator header + level blocks)
			// Next:           Data region         (free blocks)
			// Last:           Journal             (at the end of the volume)
			//
			// The kernel reads inode_table_offset directly from the superblock.
			// The legacy flat data bitmap is no longer used; the 3-level allocator
			// pyramid in the trie pool tracks all data blocks.

			estInodes := uint64(totalBlocks) / uint64(inodeRatio)
			if estInodes < briefs.MinInodes {
				estInodes = briefs.MinInodes
			}
			estInodes = roundUp(estInodes, briefs.InodeAlign)

			inodesPerBlock := blockSize / inodeSize // 8
			inodeTableBlocks := roundUp(estInodes, inodesPerBlock) / inodesPerBlock

			// Inode bitmap size (3-level bitmap pyramid)
			inodeAllocBuilder := briefs.NewAllocBuilder(estInodes)
			inodeBitmapBlocks := inodeAllocBuilder.NbBlocks()
			inodeAllocBlocks := inodeBitmapBlocks

			// Compute final data blocks and exact allocator size (one pass).
			// No flat data bitmap is written; the allocator pyramid lives in
			// the trie pool region.
			// Ensure there is enough room for superblock, inode bitmap, inode
			// table, EAT, a one-block allocator, and the journal, plus at least
			// one data block.
			minBlocks := uint64(1) + inodeAllocBlocks + inodeTableBlocks + 1 + 1 + journalBlocks + 1
			if uint64(totalBlocks) < minBlocks {
				return fmt.Errorf("filesystem too small")
			}

			finalDataBlocks := uint64(totalBlocks) - 1 - inodeAllocBlocks - inodeTableBlocks - 1 - journalBlocks
			builder := briefs.NewDataAllocBuilder(finalDataBlocks)
			allocBlocks := builder.NbBlocks()
			finalDataBlocks = uint64(totalBlocks) - 1 - inodeAllocBlocks - inodeTableBlocks - 1 - allocBlocks - journalBlocks
			if finalDataBlocks < 1 {
				return fmt.Errorf("filesystem too small")
			}

			// Build final data allocator with correct block count.
			// NewDataAllocBuilder reserves block 0 as the ENOSPC sentinel.
			// Allocate block 1 for the root directory trie.
			finalBuilder := briefs.NewDataAllocBuilder(finalDataBlocks)
			finalBuilder.MarkAllocated(1) // root dir block (block 0 is ENOSPC sentinel)
			allocBlocksFinal := finalBuilder.NbBlocks()

			// --- Compute actual block offsets ---
			nextBlock := uint64(0)

			// Block 0: Superblock
			nextBlock++ // now 1

			// Inode bitmap
			inodeBMOffset := nextBlock
			inodeBMBlocks := inodeBitmapBlocks
			nextBlock += inodeBMBlocks

			// Inode table (at inode_table_offset)
			inodeTableOffset := nextBlock
			nextBlock += inodeTableBlocks

			// EAT (placeholder)
			eatOffset := nextBlock
			eatBlocks := uint64(1)
			nextBlock += eatBlocks

			// Allocator pool: header + level data (deterministic size)
			allocPoolStart := nextBlock
			allocHeaderBlock := nextBlock
			nextBlock += 1 // header block

			allocDataBlocks := allocBlocksFinal - 1
			nextBlock += allocDataBlocks

			allocPoolSize := allocBlocksFinal

			// Data region start
			dataRegionStart := nextBlock

			// Journal at end of volume
			journalOffset := uint64(totalBlocks) - journalBlocks

			if journalOffset < nextBlock {
				return fmt.Errorf("filesystem too small: metadata needs %d blocks up to block %d, journal at %d",
					nextBlock, journalOffset-1, journalOffset)
			}

			// --- Build superblock ---
			sb, err := briefs.NewSuperblock(uint64(totalBlocks), blockSize, inodeSize, journalBlocks, label, uuidStr)
			if err != nil {
				return err
			}
			sb.Lay.DataBlocks = finalDataBlocks
			sb.Lay.FreeDataBlks = finalDataBlocks
			sb.Lay.FreeInodes = estInodes - 1 // -1 for root inode

			sb.Lay.InodeBMOffset = inodeBMOffset
			sb.Lay.InodeBMBlocks = inodeBMBlocks
			sb.Lay.InodeTableOffset = inodeTableOffset

			sb.Lay.EATOffset = eatOffset
			sb.Lay.EATBlocks = eatBlocks

			sb.Lay.TrieRootBlock = allocHeaderBlock
			sb.Lay.TrieBlocksUsed = allocPoolSize
			sb.Lay.TrieNodePoolStart = allocPoolStart
			sb.Lay.TrieNodePoolSize = allocPoolSize

			sb.Lay.JournalOffset = journalOffset
			sb.Lay.JournalLogStart = journalOffset
			sb.Lay.JournalLogEnd = journalOffset

			// --- Open output ---
			file, err := os.Create(path)
			if err != nil {
				return fmt.Errorf("create file: %w", err)
			}
			defer file.Close()

			stat, err := file.Stat()
			if err != nil {
				return fmt.Errorf("stat: %w", err)
			}
			if !(stat.Mode().IsRegular() ||
				(stat.Mode()&os.ModeDevice != 0 && stat.Mode()&os.ModeCharDevice == 0)) {
				return fmt.Errorf("not an appropriate file or device type")
			}

			// --- Refuse to overwrite an existing filesystem unless forced ---
			// mkfs.briefs must not silently clobber a device that already
			// holds a filesystem (its own or a foreign one); xfstests
			// generic/740 checks exactly this.  A fresh device reads back as
			// all zeros, while every filesystem writes a non-zero superblock
			// into block 0.  os.Create() truncates regular files (so they
			// read back zero and are always allowed), but block devices are
			// not truncated, so an existing filesystem is detected and
			// refused unless --force/-f was given.
			if !force {
				first := make([]byte, blockSize)
				n, err := file.ReadAt(first, 0)
				if err != nil && err != io.EOF {
					return fmt.Errorf("read first block: %w", err)
				}
				nonzero := false
				for i := 0; i < n; i++ {
					if first[i] != 0 {
						nonzero = true
						break
					}
				}
				if nonzero {
					return fmt.Errorf("%s already contains a filesystem or data; use --force/-f to overwrite", path)
				}
			}

			totalSize := sb.Lay.TotalBlocks * sb.Lay.BlockSize
			if stat.Mode().IsRegular() {
				if err := file.Truncate(int64(totalSize)); err != nil {
					return fmt.Errorf("truncate: %w", err)
				}
			}

			// --- Clear metadata regions that mkfs doesn't otherwise write ---
			// Stale journal records before the checkpoint must be zeroed so a
			// dirty-looking journal can't be misread on mount.  The inode table,
			// however, is NOT pre-zeroed: on a large volume the table scales
			// with the device (one inode per inode-ratio blocks), so zeroing it
			// in full writes a huge contiguous region up front -- enough to fill
			// a dm-snapshot COW store and EIO before mkfs even writes the root
			// inode (generic/620, 17TB dm-hugedisk).  The kernel writes each new
			// inode fresh into its slot on allocation, so unallocated slots are
			// never consulted at runtime.  fsck.briefs validates only the
			// bitmap-allocated inode slots, skipping free slots (which on a
			// reused device simply retain the previous filesystem's stale
			// bytes); the bitmap/table cross-check still flags an allocated
			// slot that lacks a valid inode magic.
			if err := zeroBlocks(file, journalOffset, journalBlocks-1, blockSize); err != nil {
				return fmt.Errorf("zero journal: %w", err)
			}

			// --- Write metadata blocks ---

			// 1. Superblock (block 0)
			sbData := make([]byte, blockSize)
			copy(sbData, sb.MarshalBinary())
			if _, err := file.WriteAt(sbData, 0); err != nil {
				return fmt.Errorf("write superblock: %w", err)
			}

			// 2. Inode bitmap pyramid (3-level allocator)
			// Mark root inode (index 0) as allocated
			inodeAllocBuilder.MarkAllocated(0)
			inodeBitmapWrites := inodeAllocBuilder.WriteBlocks()
			for i, blk := range inodeBitmapWrites {
				writeBlock := inodeBMOffset + uint64(i)
				if _, err := file.WriteAt(blk, int64(writeBlock*blockSize)); err != nil {
					return fmt.Errorf("write inode allocator block %d at %d: %w", i, writeBlock, err)
				}
			}

			// 3. Root directory trie root
			// Allocate a block for a packed trie page; slot 0 is the root node.
			// Block 0 of the data region is reserved as ENOSPC sentinel, so root dir
			// goes at data-relative block 1 (absolute: dataRegionStart + 1).
			rootTrieBlock := dataRegionStart + 1
			trieBlock := make([]byte, blockSize)
			// Page header (16 bytes)
			binary.LittleEndian.PutUint32(trieBlock[0:], briefs.MagicTriePage) // "TRNP" magic
			binary.LittleEndian.PutUint32(trieBlock[4:], briefs.TriePageVersion)
			binary.LittleEndian.PutUint16(trieBlock[8:], 1)   // live_count = 1
			binary.LittleEndian.PutUint16(trieBlock[10:], 0)  // free_name_off = 0
			// free_slots: slot 0 allocated, rest free -> bitmap = ~1
			binary.LittleEndian.PutUint64(trieBlock[12:], ^uint64(1))
			// Slot 0 at offset 20.  Fields are little-endian:
			//   first_child (8), next_sibling (8), inode (8),
			//   name_len (2), name_offset (2), depth (1), node_type (1),
			//   byte_val (1), f_type (1), flags (2), child_count (2)
			slotOff := uint64(20)
			trieBlock[slotOff+28] = 0                       // depth = 0
			trieBlock[slotOff+29] = byte(briefs.NodeTypeInterm) // node_type
			// first_child, next_sibling, inode, name_len, name_offset,
			// byte_val, f_type, flags, child_count all default to 0.

			if _, err := file.WriteAt(trieBlock, int64(rootTrieBlock*blockSize)); err != nil {
				return fmt.Errorf("write root trie page at %d: %w", rootTrieBlock, err)
			}

			// 4. Root inode (inode 1) at first slot of inode table
			rootInode := briefs.NewInode(1, briefs.ModeDir|0755)
			rootInode.Nlinks = 2
			rootInode.ParentInode = 1
			rootInode.DirTrieRoot = briefs.TrieMakeRef(rootTrieBlock, 0)
			// Stable nonzero generation for the root inode so NFS export
			// handles reference it safely. Root is never freed/reallocated,
			// so a fixed value is fine and stays constant across mounts.
			rootInode.Generation = 1

			inodeBlock, inodeByteOffset := calculateInodeLocation(sb, 1)
			fileOffset := int64(inodeBlock*blockSize + inodeByteOffset)
			if err := rootInode.WriteAt(file, fileOffset); err != nil {
				return fmt.Errorf("write root inode: %w", err)
			}

			// 6. EAT block (placeholder — already zero from truncation)

			// 7. Allocator pool blocks (header + level data)
			allocWrites := finalBuilder.WriteBlocks()
			for i, blk := range allocWrites {
				writeBlock := allocPoolStart + uint64(i)
				if _, err := file.WriteAt(blk, int64(writeBlock*blockSize)); err != nil {
					return fmt.Errorf("write allocator block %d at %d: %w", i, writeBlock, err)
				}
			}

			// 8. Journal — write the initial checkpoint in the last journal
			// block. This matches the kernel's checkpoint_block location
			// (journal_offset + journal_blocks - 1).
			checkpointBlock := journalOffset + journalBlocks - 1
			journalBuf := make([]byte, blockSize)
			cp := &briefs.Checkpoint{
				Seq:            1,
				RecordCount:    1,
				LogSequenceEnd: journalOffset,
				TrieRootNode:   0,
				// Block 0 is ENOSPC sentinel, block 1 is root dir trie.
				FreeDataCount:  finalDataBlocks - 2,
				FreeInodeCount: estInodes - 1,
			}
			if err := briefs.WriteCheckpointBlock(journalBuf, 0, cp); err != nil {
				return err
			}
			if _, err := file.WriteAt(journalBuf, int64(checkpointBlock*blockSize)); err != nil {
				return fmt.Errorf("write journal checkpoint at block %d: %w", checkpointBlock, err)
			}

			// Update superblock's free counts and checkpoint sequence.
			// Block 0 is reserved as ENOSPC sentinel, block 1 is root dir trie.
			sb.Lay.FreeDataBlks = finalDataBlocks - 2 // -1 for ENOSPC sentinel, -1 for root dir
			sb.Lay.FreeInodes = estInodes - 1         // -1 for root inode
			sb.Lay.CheckpointSeq = cp.Seq

			// Write updated superblock after all metadata is on disk.
			updatedSbData := make([]byte, blockSize)
			copy(updatedSbData, sb.MarshalBinary())
			if _, err := file.WriteAt(updatedSbData, 0); err != nil {
				return fmt.Errorf("write updated superblock: %w", err)
			}

			// Flush the device and check for write errors before reporting
			// success.  os.File.WriteAt only stages pages in the page cache;
			// deferred Close does not fsync a block device.  On a thin-
			// provisioned volume with error_if_no_space (e.g. xfstests
			// generic/405's 1 TiB volume over a 1 MiB pool), provisioning
			// fails asynchronously at writeback time, so without an explicit
			// fsync mkfs returns success while the superblock never lands and
			// the filesystem is left inconsistent on disk.  fsync propagates
			// the writeback EIO so mkfs fails non-zero and the test's "mkfs
			// failed" branch is taken.
			if err := file.Sync(); err != nil {
				return fmt.Errorf("fsync after mkfs: %w", err)
			}

			// --- Report ---
			if stderrIsTerminal() {
				fmt.Fprintf(os.Stderr, "Created filesystem: %s (%d blocks × %d bytes)\n",
					path, totalBlocks, blockSize)
				fmt.Fprintf(os.Stderr, "  inodes:       %d (ratio 1 inode per %d blocks)\n", estInodes, inodeRatio)
				fmt.Fprintf(os.Stderr, "  journal:      %d blocks at %d\n", journalBlocks, journalOffset)
				fmt.Fprintf(os.Stderr, "  data blocks:  %d (blocks %d..%d, %d free)\n",
					finalDataBlocks, dataRegionStart, journalOffset-1, finalDataBlocks-1)
				fmt.Fprintf(os.Stderr, "  inode bitmap: %d blocks at offset %d (3-level bitmap pyramid)\n", inodeBMBlocks, inodeBMOffset)
				fmt.Fprintf(os.Stderr, "  inode table:  %d blocks at offset %d\n", inodeTableBlocks, inodeTableOffset)
				fmt.Fprintf(os.Stderr, "  EAT:          %d block(s) at offset %d\n", eatBlocks, eatOffset)
				fmt.Fprintf(os.Stderr, "  alloc pool:   %d blocks at offset %d (3-level bitmap)\n",
					allocPoolSize, allocPoolStart)
				fmt.Fprintf(os.Stderr, "  root dir:     trie root at block %d\n", rootTrieBlock)
			}
			return nil
		},
	}

	if err := app.Run(context.Background(), os.Args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func isPowerOfTwo(n uint64) bool {
	return n != 0 && (n&(n-1)) == 0
}

// zeroBlocks overwrites a range of blocks with zeros.  start is the first
// block offset and count is the number of blocks.  The caller must ensure
// start and count are within the device bounds.
func zeroBlocks(file *os.File, start, count, blockSize uint64) error {
	if count == 0 {
		return nil
	}
	zeroBlock := make([]byte, blockSize)
	for i := uint64(0); i < count; i++ {
		off := int64((start + i) * blockSize)
		if _, err := file.WriteAt(zeroBlock, off); err != nil {
			return fmt.Errorf("write zero block at %d: %w", start+i, err)
		}
	}
	return nil
}
