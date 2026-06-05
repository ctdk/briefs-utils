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
// The inode table starts at: data_bitmap_offset + data_bitmap_blocks
// (This matches what the kernel computes in briefs_iget.)
func calculateInodeLocation(sb *types.Superblock, inodeNum uint64) (blockOffset uint64, byteOffset uint64) {
	inodesPerBlock := sb.Lay.BlockSize / sb.Lay.InodeSize // 4096 / 512 = 8
	inodeTableStartBlock := sb.Lay.DataBitmapOffset + sb.Lay.DataBitmapBlocks
	inodeIndex := inodeNum - 1
	blockOffset = inodeTableStartBlock + (inodeIndex / inodesPerBlock)
	byteOffset = (inodeIndex % inodesPerBlock) * sb.Lay.InodeSize
	return blockOffset, byteOffset
}

// TODO: mkfs.briefs should use the same general argument and flag format that
// other mkfs programs use, i.e. "mkfs.briefs <options> /dev/sda1" instead of
// "mkfs.briefs <options> -o /dev/sda1". Will deal with later, since it looks
// like that'll require a bit of wiring to get the help message showing the
// right things.
//
// On the plus side, I learned that it's easy to make man pages with apps that
// use github.com/urfave/cli for flag/arg processing. Huzzah!
func main() {
	app := &cli.App{
		Name:  "mkfs.briefs",
		Usage: "Create a new BrieFS filesystem",
		Version: versionStr,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:     "output",
				Aliases:  []string{"o"},
				Required: true,
				Usage:    "output file path",
			},
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
			path := c.String("output")
			totalBlocks := c.Int64("size")
			blockSize := uint64(c.Int("block-size"))
			inodeSize := uint64(c.Int("inode-size"))
			journalBlocks := uint64(c.Int("journal-size"))
			label := c.String("label")

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

			// Inode bitmap size
			inodeBitmapBytes := (estInodes + 7) / 8
			inodeBitmapBlocks := roundUp(inodeBitmapBytes, blockSize) / blockSize
			if inodeBitmapBlocks < 1 {
				inodeBitmapBlocks = 1
			}

			// Solve for trie pool size iteratively (depends on data block count)
			triePoolHeader := uint64(1)

			// Pass 1: estimate with rough trie size
			estTrieDataBlocks := uint64(1) + uint64(uint64(totalBlocks)/20000)
			if estTrieDataBlocks < 1 {
				estTrieDataBlocks = 1
			}
			meta1 := uint64(1) + inodeBitmapBlocks + inodeTableBlocks + 1 + triePoolHeader + estTrieDataBlocks
			estData1 := uint64(totalBlocks) - meta1 - journalBlocks
			if estData1 < 1 {
				return fmt.Errorf("filesystem too small")
			}

			// Pass 2: refine data bitmap
			dataBitmapBytes := (estData1 + 7) / 8
			dataBitmapBlocks := roundUp(dataBitmapBytes, blockSize) / blockSize
			if dataBitmapBlocks < 1 {
				dataBitmapBlocks = 1
			}
			meta2 := uint64(1) + inodeBitmapBlocks + dataBitmapBlocks + inodeTableBlocks + 1 + triePoolHeader
			estData2 := uint64(totalBlocks) - meta2 - journalBlocks
			if estData2 < 1 {
				return fmt.Errorf("filesystem too small")
			}

			// Pass 3: build trie with estimated data blocks
			builder := types.NewAllocTreeBuilder(estData2)
			builder.Build(estData2)
			trieDataBlocks := builder.NbBlocks()
			if trieDataBlocks < 1 {
				trieDataBlocks = 1
			}

			// Pass 4: final calculation
			meta3 := uint64(1) + inodeBitmapBlocks + dataBitmapBlocks + inodeTableBlocks + 1 +
				triePoolHeader + trieDataBlocks
			finalDataBlocks := uint64(totalBlocks) - meta3 - journalBlocks
			if finalDataBlocks < 1 {
				return fmt.Errorf("filesystem too small")
			}

			// Build final trie
			finalBuilder := types.NewAllocTreeBuilder(finalDataBlocks)
			finalBuilder.Build(finalDataBlocks)
			// Mark block 0 (data-relative) as allocated for the root directory block
			finalBuilder.MarkRangeAllocated(0, 1)
			finalTrieDataBlocks := finalBuilder.NbBlocks()
			if finalTrieDataBlocks < 1 {
				finalTrieDataBlocks = 1
			}

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

			// Inode table (kernel reads at: data_bitmap_offset + data_bitmap_blocks)
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

			// Trie pool: header + node blocks
			triePoolStart := nextBlock
			trieRootBlock := nextBlock
			nextBlock += triePoolHeader

			trieDataStart := nextBlock
			trieNodeBlocksCount := finalTrieDataBlocks
			nextBlock += trieNodeBlocksCount

			trieBlocksUsed := triePoolHeader + trieNodeBlocksCount
			triePoolSize := trieBlocksUsed

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
			sb.Lay.DataBitmapOffset = dataBMOffset
			sb.Lay.DataBitmapBlocks = dataBMBlocks

			sb.Lay.EATOffset = eatOffset
			sb.Lay.EATBlocks = eatBlocks

			sb.Lay.TrieRootBlock = trieRootBlock
			sb.Lay.TrieBlocksUsed = trieBlocksUsed
			sb.Lay.TrieNodePoolStart = triePoolStart
			sb.Lay.TrieNodePoolSize = triePoolSize

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
			if stat.Mode().IsRegular() {
				totalSize := sb.Lay.TotalBlocks * sb.Lay.BlockSize
				if err := file.Truncate(int64(totalSize)); err != nil {
					return fmt.Errorf("truncate: %w", err)
				}
			}

			// --- Write metadata blocks ---

			// 1. Superblock (block 0)
			sbData := make([]byte, blockSize)
			copy(sbData, sb.MarshalBinary())
			if _, err := file.WriteAt(sbData, 0); err != nil {
				return fmt.Errorf("write superblock: %w", err)
			}

			// 2. Inode bitmap (all zeros)
			inodeBitmap := make([]byte, int(inodeBMBlocks)*int(blockSize))
			if _, err := file.WriteAt(inodeBitmap, int64(inodeBMOffset*blockSize)); err != nil {
				return fmt.Errorf("write inode bitmap: %w", err)
			}

			// 3. Data bitmap (all zeros — all data blocks free)
			dataBitmap := make([]byte, int(dataBMBlocks)*int(blockSize))
			if _, err := file.WriteAt(dataBitmap, int64(dataBMOffset*blockSize)); err != nil {
				return fmt.Errorf("write data bitmap: %w", err)
			}

			// 4. Root directory block
			// Allocate the first data block for the root directory's contents.
			// The kernel will look at root inode -> inline extent -> dir block.
			rootDirBlock := dataRegionStart

			// Build the directory block with . and .. entries (both point to inode 1)
			typeMask := uint8(types.ModeDir >> 9) // S_IFDIR bit (040000 >> 9 = 004)

			dotEntry := types.DirBlockEntry{
				Inode: 1,
				Type:  typeMask,
				Name:  ".",
			}

			dotDotEntry := types.DirBlockEntry{
				Inode: 1,
				Type:  typeMask,
				Name:  "..",
			}

			dirBlockData := types.NewDirBlock([]types.DirBlockEntry{dotEntry, dotDotEntry})
			if _, err := file.WriteAt(dirBlockData, int64(rootDirBlock*blockSize)); err != nil {
				return fmt.Errorf("write root directory block at %d: %w", rootDirBlock, err)
			}

			// Mark the root directory block as allocated in the data bitmap
			dataBitmapByte := rootDirBlock / 8
			dataBitmapBit := rootDirBlock % 8
			dataBitmap[dataBitmapByte] |= uint8(1 << dataBitmapBit)

			// 5. Root inode (inode 1) at first slot of inode table
			rootInode := types.NewInode(1, types.ModeDir|0755)
			rootInode.FileSize = uint64(16 + 2*16 + 7) // data_size (48) + names_size (7) = 55
			rootInode.Nlinks = 2                              // . and ..
			rootInode.NumExtentsInline = 1
			rootInode.NumExtentsTotal = 1
			if err := rootInode.SetInlineExtent(0, 0, rootDirBlock, 1, 0); err != nil {
				return err
			}

			inodeBlock, inodeByteOffset := calculateInodeLocation(sb, 1)
			fileOffset := int64(inodeBlock*blockSize + inodeByteOffset)
			if err := rootInode.WriteAt(file, fileOffset); err != nil {
				return fmt.Errorf("write root inode: %w", err)
			}

			// Mark root inode allocated in bitmap
			inodeBitmap[0] |= 1
			if _, err := file.WriteAt(inodeBitmap, int64(inodeBMOffset*blockSize)); err != nil {
				return fmt.Errorf("write updated inode bitmap: %w", err)
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

			// 6. Trie root header block
			// struct briefs_trie_root at start of block:
			//   magic(4), version(4), root_node(8), free_list(8), node_count(4), reserved(28) = 56 bytes used, padded to 4096
			trieRootBuf := make([]byte, blockSize)
			tr := types.TrieRoot{
				Magic:     0x54524945, // "TRIE"
				Version:   1,
				RootNode:  0, // first node in the trie data (node at index 0 = root)
				FreeList:  0,
				NodeCount: uint32(len(finalBuilder.Nodes)),
			}
			copy(trieRootBuf[:32], tr.MarshalBinary())
			if _, err := file.WriteAt(trieRootBuf, int64(trieRootBlock*blockSize)); err != nil {
				return fmt.Errorf("write trie root header: %w", err)
			}

			// 7. Trie node data blocks
			trieBlocks := finalBuilder.WriteNodes()
			for i, nodeBlock := range trieBlocks {
				writeBlock := trieDataStart + uint64(i)
				if _, err := file.WriteAt(nodeBlock, int64(writeBlock*blockSize)); err != nil {
					return fmt.Errorf("write trie node block %d at %d: %w", i, writeBlock, err)
				}
			}

			// 8. Journal — write a checkpoint in the first journal block
			journalBuf := make([]byte, blockSize)
			// Journal block header (16 bytes)
			binary.LittleEndian.PutUint32(journalBuf[0:], 0x4A4E4C5A) // "JNLZ" magic
			binary.LittleEndian.PutUint32(journalBuf[4:], 0)          // block_seq
			binary.LittleEndian.PutUint32(journalBuf[8:], 1)          // record_count
			// Checkpoint record header at offset 16
			recOff := uint64(16)
			binary.LittleEndian.PutUint32(journalBuf[recOff:], 9)     // type = JRN_CHECKPOINT
			binary.LittleEndian.PutUint32(journalBuf[recOff+4:], 0)   // flags
			binary.LittleEndian.PutUint32(journalBuf[recOff+8:], 80)  // data_len
			binary.LittleEndian.PutUint32(journalBuf[recOff+12:], 0)  // checksum
			// Checkpoint record data at offset 32 (80 bytes)
			cpOff := recOff + 16
			binary.LittleEndian.PutUint64(journalBuf[cpOff:], 1)    // checkpoint_seq = 1
			binary.LittleEndian.PutUint32(journalBuf[cpOff+8:], 1)  // record_count
			binary.LittleEndian.PutUint64(journalBuf[cpOff+16:], 0) // log_sequence_end
			binary.LittleEndian.PutUint64(journalBuf[cpOff+24:], 0) // trie_root_node
			binary.LittleEndian.PutUint64(journalBuf[cpOff+32:], finalDataBlocks-1) // free_data_count
			binary.LittleEndian.PutUint64(journalBuf[cpOff+40:], estInodes-1)     // free_inode_count
			if _, err := file.WriteAt(journalBuf, int64(journalOffset*blockSize)); err != nil {
				return fmt.Errorf("write journal checkpoint: %w", err)
			}

			// --- Report ---
			fmt.Fprintf(os.Stderr, "Created filesystem: %s (%d blocks × %d bytes)\n",
				path, totalBlocks, blockSize)
			fmt.Fprintf(os.Stderr, "  inodes:       %d\n", estInodes)
			fmt.Fprintf(os.Stderr, "  journal:      %d blocks at %d\n", journalBlocks, journalOffset)
			fmt.Fprintf(os.Stderr, "  data blocks:  %d (blocks %d..%d, %d free)\n",
				finalDataBlocks, dataRegionStart, journalOffset-1, finalDataBlocks-1)
			fmt.Fprintf(os.Stderr, "  inode bitmap: %d blocks at offset %d\n", inodeBMBlocks, inodeBMOffset)
			fmt.Fprintf(os.Stderr, "  data bitmap:  %d blocks at offset %d\n", dataBMBlocks, dataBMOffset)
			fmt.Fprintf(os.Stderr, "  inode table:  %d blocks at offset %d\n", inodeTableBlocks, inodeTableOffset)
			fmt.Fprintf(os.Stderr, "  EAT:          %d block(s) at offset %d\n", eatBlocks, eatOffset)
			fmt.Fprintf(os.Stderr, "  trie pool:    %d blocks at offset %d (header + %d data, %d nodes)\n",
				triePoolSize, triePoolStart, trieNodeBlocksCount, len(finalBuilder.Nodes))
			fmt.Fprintf(os.Stderr, "  root dir:     block %d, . and .. entries written\n", rootDirBlock)
			return nil
		},
	}

	if err := app.Run(os.Args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
