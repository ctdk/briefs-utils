// mkfs.briefs creates a BrieFS filesystem given the relevant parameters.
package main

import (
	"encoding/binary"
	"fmt"
	"os"

	"github.com/ctdk/briefs-utils/device"
	"github.com/ctdk/briefs-utils/types"
	"github.com/urfave/cli/v2"
)

var versionStr = fmt.Sprintf("v%d.%d.%d", types.BrieFSMajorVersion, types.BrieFSMinorVersion, types.BrieFSPatchVersion)

func roundUp(value, alignment uint64) uint64 {
	return (value + alignment - 1) / alignment * alignment
}

// Calculate on-disk location of an inode.
// The inode table starts at: inode_table_offset
// (This matches what the kernel computes in briefs_iget.)
func calculateInodeLocation(sb *types.Superblock, inodeNum uint64) (blockOffset uint64, byteOffset uint64) {
	inodesPerBlock := sb.Lay.BlockSize / sb.Lay.InodeSize // 4096 / 512 = 8
	inodeTableStartBlock := sb.Lay.InodeTableOffset
	inodeIndex := inodeNum - 1
	blockOffset = inodeTableStartBlock + (inodeIndex / inodesPerBlock)
	byteOffset = (inodeIndex % inodesPerBlock) * sb.Lay.InodeSize
	return blockOffset, byteOffset
}

func main() {
	app := &cli.App{
		Name:     "mkfs.briefs",
		Usage:    "Create a new BrieFS filesystem",
		ArgsUsage: "DEVICE",
		Version:  versionStr,
		Before: func(c *cli.Context) error {
			if c.Args().Len() < 1 {
				return fmt.Errorf("missing required argument: DEVICE")
			}
			path := c.Args().First()
			if err := device.CheckMounted(path); err != nil {
				// Reformatting a mounted filesystem is incredibly
				// dangerous. Refuse to continue.
				return fmt.Errorf("refusing to create filesystem: %w\n", err)
			}
			return nil
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
				Value:    512,
				Usage:    "inode size in bytes",
			},
			&cli.IntFlag{
				Name:     "journal-size",
				Aliases:  []string{"j"},
				Value:    64,
				Usage:    "journal size in blocks",
			},
			&cli.StringFlag{
				Name:     "label",
				Value:    "BRIEFS",
				Usage:    "filesystem label",
			},
		},
		Action: func(c *cli.Context) error {
			path := c.Args().First()
			totalBlocks := c.Int64("size")
			blockSize := uint64(c.Int("block-size"))
			inodeSize := uint64(c.Int("inode-size"))
			journalBlocks := uint64(c.Int("journal-size"))
			label := c.String("label")

			// Validate that blockSize and inodeSize are powers of
			// two.
			if !isPowerOfTwo(blockSize) {
				return fmt.Errorf("block-size must be a power of two, which %d isn't", blockSize)
			}
			if !isPowerOfTwo(inodeSize) {
				return fmt.Errorf("inode-size must be a power of two, which %d isn't", inodeSize)
			}

			// Probe device if no explicit size given
			if totalBlocks == 0 {
				bd, err := device.GetDevice(path, blockSize)
				if err != nil {
					return err
				}
				fmt.Fprintf(os.Stderr, "Probed device %s: %d bytes, %d blocks.\n",
					path, bd.Bytes(), bd.Blocks())
				totalBlocks = bd.Blocks()
			}

			// --- Block Layout (matches kernel expectations) ---
			//
			// Block 0:       Superblock          (1 block)
			// Next:           Inode bitmap        (1 bit per inode)
			// Next:           Data bitmap         (1 bit per data block)
			// Next:           Inode table         (8 inodes per block)
			// Next:           EAT                 (reserved, 1 block)
			// Next:           Trie pool           (header + node blocks)
			// Next:           Data region         (free blocks)
			// Last:           Journal             (at the end of the volume)
			//
			// Kernel computes inode table start as:
			//   data_bitmap_offset + data_bitmap_blocks
			// which matches this layout.

			estInodes := uint64(totalBlocks) / 16
			if estInodes < 100 {
				estInodes = 100
			}
			estInodes = roundUp(estInodes, 32)

			inodesPerBlock := blockSize / inodeSize // 8
			inodeTableBlocks := roundUp(estInodes, inodesPerBlock) / inodesPerBlock

			// Inode bitmap size (3-level bitmap pyramid)
			inodeAllocBuilder := types.NewAllocBuilder(estInodes)
			inodeBitmapBlocks := inodeAllocBuilder.NbBlocks()
			inodeAllocBlocks := inodeBitmapBlocks

			// Allocator pool size is deterministic — no iteration needed.
			// Start with a rough estimate of data blocks to size the data bitmap,
			// then compute the exact allocator size.
			estAllocBlocks := uint64(1) + uint64(totalBlocks/100000)
			if estAllocBlocks < 2 {
				estAllocBlocks = 2
			}

			estData1 := uint64(totalBlocks) - inodeBitmapBlocks - inodeTableBlocks - 1 - estAllocBlocks - journalBlocks
			if estData1 < 1 {
				return fmt.Errorf("filesystem too small")
			}
			dataBitmapBytes := (estData1 + 7) / 8
			dataBitmapBlocks := roundUp(dataBitmapBytes, blockSize) / blockSize
			if dataBitmapBlocks < 1 {
				dataBitmapBlocks = 1
			}

			// Compute final data blocks and exact allocator size (one pass)
			finalDataBlocks := uint64(totalBlocks) - 1 - inodeAllocBlocks - dataBitmapBlocks - inodeTableBlocks - 1 - journalBlocks
			builder := types.NewAllocBuilder(finalDataBlocks)
			allocBlocks := builder.NbBlocks()
			finalDataBlocks = uint64(totalBlocks) - 1 - inodeAllocBlocks - dataBitmapBlocks - inodeTableBlocks - 1 - allocBlocks - journalBlocks
			if finalDataBlocks < 1 {
				return fmt.Errorf("filesystem too small")
			}

			// Build final allocator with correct block count
			finalBuilder := types.NewAllocBuilder(finalDataBlocks)
			finalBuilder.MarkAllocated(0) // root dir block
			allocBlocksFinal := finalBuilder.NbBlocks()

			// --- Compute actual block offsets ---
			nextBlock := uint64(0)

			// Block 0: Superblock
			nextBlock++ // now 1

			// Inode bitmap
			inodeBMOffset := nextBlock
			inodeBMBlocks := inodeBitmapBlocks
			nextBlock += inodeBMBlocks

			// Data bitmap (kernel reads inode table start as this + blocks)
			dataBMOffset := nextBlock
			dataBMBlocks := dataBitmapBlocks
			nextBlock += dataBMBlocks

			// Inode table (at inode_table_offset)
			inodeTableOffset := nextBlock
			nextBlock += inodeTableBlocks
			// Sanity: kernel-derived inode table start must match
			if inodeTableOffset != dataBMOffset+dataBMBlocks {
				panic("inode table offset mismatch")
			}

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
			sb := types.NewSuperblock(uint64(totalBlocks), blockSize, inodeSize, journalBlocks, label)
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

			totalSize := sb.Lay.TotalBlocks * sb.Lay.BlockSize
			if stat.Mode().IsRegular() {
				if err := file.Truncate(int64(totalSize)); err != nil {
					return fmt.Errorf("truncate: %w", err)
				}
			}

			// --- Clear metadata regions that mkfs doesn't otherwise write ---
			// Stale data from a previous filesystem can survive in the rest
			// of the inode table and in journal blocks before the checkpoint.
			// Zero those regions explicitly so fsck doesn't see old inodes
			// or stale journal records.
			if err := zeroBlocks(file, inodeTableOffset, inodeTableBlocks, blockSize); err != nil {
				return fmt.Errorf("zero inode table: %w", err)
			}
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

			// 3. Data bitmap (all zeros — all data blocks free)
			dataBitmap := make([]byte, int(dataBMBlocks)*int(blockSize))
			if _, err := file.WriteAt(dataBitmap, int64(dataBMOffset*blockSize)); err != nil {
				return fmt.Errorf("write data bitmap: %w", err)
			}

			// 4. Root directory trie root
			// Allocate a block for a packed trie page; slot 0 is the root node.
			rootTrieBlock := dataRegionStart
			trieBlock := make([]byte, blockSize)
			// Page header (16 bytes)
			binary.LittleEndian.PutUint32(trieBlock[0:], types.MagicTriePage) // "TRNP" magic
			binary.LittleEndian.PutUint32(trieBlock[4:], types.TriePageVersion)
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
			trieBlock[slotOff+29] = byte(types.NodeTypeInterm) // node_type
			// first_child, next_sibling, inode, name_len, name_offset,
			// byte_val, f_type, flags, child_count all default to 0.

			if _, err := file.WriteAt(trieBlock, int64(rootTrieBlock*blockSize)); err != nil {
				return fmt.Errorf("write root trie page at %d: %w", rootTrieBlock, err)
			}

			// Mark the root trie block as allocated in the data bitmap
			dataBitmapByte := rootTrieBlock / 8
			dataBitmapBit := rootTrieBlock % 8
			dataBitmap[dataBitmapByte] |= uint8(1 << dataBitmapBit)

			// 5. Root inode (inode 1) at first slot of inode table
			rootInode := types.NewInode(1, types.ModeDir|0755)
			rootInode.Nlinks = 2
			rootInode.DirTrieRoot = types.TrieMakeRef(rootTrieBlock, 0)

			inodeBlock, inodeByteOffset := calculateInodeLocation(sb, 1)
			fileOffset := int64(inodeBlock*blockSize + inodeByteOffset)
			if err := rootInode.WriteAt(file, fileOffset); err != nil {
				return fmt.Errorf("write root inode: %w", err)
			}

			// Write updated data bitmap (root dir block allocated)
			if _, err := file.WriteAt(dataBitmap, int64(dataBMOffset*blockSize)); err != nil {
				return fmt.Errorf("write updated data bitmap: %w", err)
			}

			// Update superblock's free counts (1 data block used, 1 inode used)
			sb.Lay.FreeDataBlks = finalDataBlocks - 1
			sb.Lay.FreeInodes = estInodes - 1

			// Write updated superblock
			updatedSbData := make([]byte, blockSize)
			copy(updatedSbData, sb.MarshalBinary())
			if _, err := file.WriteAt(updatedSbData, 0); err != nil {
				return fmt.Errorf("write updated superblock: %w", err)
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
			// Checkpoint block header (16 bytes)
			binary.LittleEndian.PutUint32(journalBuf[0:], 0x43485053) // "CHPS" magic
			binary.LittleEndian.PutUint32(journalBuf[4:], 0)          // block_seq
			binary.LittleEndian.PutUint32(journalBuf[8:], 1)          // record_count
			// Checkpoint record header at offset 16
			recOff := uint64(16)
			binary.LittleEndian.PutUint32(journalBuf[recOff:], 9)     // type = JRN_CHECKPOINT
			binary.LittleEndian.PutUint32(journalBuf[recOff+4:], 0)   // flags
			binary.LittleEndian.PutUint32(journalBuf[recOff+8:], 80)  // data_len
			// Checkpoint record data at offset 32 (80 bytes)
			cpOff := recOff + 16
			binary.LittleEndian.PutUint64(journalBuf[cpOff:], 1)    // checkpoint_seq = 1
			binary.LittleEndian.PutUint32(journalBuf[cpOff+8:], 1)  // record_count
			binary.LittleEndian.PutUint64(journalBuf[cpOff+16:], 0) // log_sequence_end
			binary.LittleEndian.PutUint64(journalBuf[cpOff+24:], 0) // trie_root_node
			binary.LittleEndian.PutUint64(journalBuf[cpOff+32:], finalDataBlocks-1) // free_data_count
			binary.LittleEndian.PutUint64(journalBuf[cpOff+40:], estInodes-1)     // free_inode_count
			// Compute and write the CRC32C checksum over type, flags,
			// data_len, and the 80-byte checkpoint data.
			cpData := journalBuf[cpOff : cpOff+80]
			checksum := types.ComputeJournalRecordChecksum(9, 0, cpData)
			binary.LittleEndian.PutUint32(journalBuf[recOff+12:], checksum)
			if _, err := file.WriteAt(journalBuf, int64(checkpointBlock*blockSize)); err != nil {
				return fmt.Errorf("write journal checkpoint at block %d: %w", checkpointBlock, err)
			}

			// --- Report ---
			fmt.Fprintf(os.Stderr, "Created filesystem: %s (%d blocks × %d bytes)\n",
				path, totalBlocks, blockSize)
			fmt.Fprintf(os.Stderr, "  inodes:       %d\n", estInodes)
			fmt.Fprintf(os.Stderr, "  journal:      %d blocks at %d\n", journalBlocks, journalOffset)
			fmt.Fprintf(os.Stderr, "  data blocks:  %d (blocks %d..%d, %d free)\n",
				finalDataBlocks, dataRegionStart, journalOffset-1, finalDataBlocks-1)
			fmt.Fprintf(os.Stderr, "  inode bitmap: %d blocks at offset %d (3-level bitmap pyramid)\n", inodeBMBlocks, inodeBMOffset)
			fmt.Fprintf(os.Stderr, "  data bitmap:  %d blocks at offset %d\n", dataBMBlocks, dataBMOffset)
			fmt.Fprintf(os.Stderr, "  inode table:  %d blocks at offset %d\n", inodeTableBlocks, inodeTableOffset)
			fmt.Fprintf(os.Stderr, "  EAT:          %d block(s) at offset %d\n", eatBlocks, eatOffset)
			fmt.Fprintf(os.Stderr, "  alloc pool:   %d blocks at offset %d (3-level bitmap)\n",
				allocPoolSize, allocPoolStart)
			fmt.Fprintf(os.Stderr, "  root dir:     trie root at block %d\n", rootTrieBlock)
			return nil
		},
	}

	if err := app.Run(os.Args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func isPowerOfTwo(n uint64) bool {
	return ((n == 0) || ((n & (n - 1)) == 0))
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
