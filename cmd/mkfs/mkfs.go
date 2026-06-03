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

func roundUp(value, alignment uint64) uint64 {
	return (value + alignment - 1) / alignment * alignment
}

// Calculate inode table layout
// Inode table follows data bitmap, with 8 inodes per block (512 byte inodes in 4096 byte blocks)
// Returns: (block offset, byte offset within block)
func calculateInodeLocation(sb *types.Superblock, inodeNum uint64) (blockOffset uint64, byteOffset uint64) {
	inodesPerBlock := sb.Lay.BlockSize / sb.Lay.InodeSize // 4096 / 512 = 8
	
	// Inode table starts right after data bitmap
	inodeTableStartBlock := sb.Lay.DataBitmapOffset + sb.Lay.DataBitmapBlocks
	
	// Inode N is at index (N-1) in the table
	inodeIndex := inodeNum - 1
	
	// Calculate which block and offset within that block
	blockOffset = inodeTableStartBlock + (inodeIndex / inodesPerBlock)
	byteOffset = (inodeIndex % inodesPerBlock) * sb.Lay.InodeSize
	
	return blockOffset, byteOffset
}

func main() {
	app := &cli.App{
		Name:  "mkfs.briefs",
		Usage: "Create a new BrieFS filesystem",
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
				Value: 	  0,
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

			// If the number of blocks isn't specified, probe the
			// device to find the size.
			if totalBlocks == 0 {
				bd, err := device.GetDevice(path, blockSize)
				if err != nil {
					return err
				}
				fmt.Fprintf(os.Stderr, "DEBUG: Probed device %s. Size is %d bytes, %d blocks.\n", path, bd.Bytes(), bd.Blocks())
				totalBlocks = bd.Blocks()
			}

			label := c.String("label")

			// Calculate space needed for metadata
			// Superblock: 1 block
			// Inode bitmap: enough bits for all inodes
			// Journal: N blocks (at end of filesystem)
			
			// Estimate inodes (we'll set this in superblock)
			// For now, assume 1 inode per 16 data blocks, minimum 100
			estInodes := totalBlocks / 16
			if estInodes < 100 {
				estInodes = 100
			}

			// Calculate inode bitmap size (1 bit per inode, rounded to bytes, then to blocks)
			inodeBitmapBits := estInodes
			inodeBitmapBytes := (inodeBitmapBits + 7) / 8
			inodeBitmapBlocks := roundUp(uint64(inodeBitmapBytes), blockSize) / blockSize
			if inodeBitmapBlocks == 0 {
				inodeBitmapBlocks = 1
			}

			// Data bitmap: 1 bit per data block
			dataBlocks := uint64(totalBlocks) - 1 - inodeBitmapBlocks - journalBlocks
			if dataBlocks < 1 {
				return fmt.Errorf("filesystem too small")
			}
			dataBitmapBytes := (dataBlocks + 7) / 8
			dataBitmapBlocks := roundUp(uint64(dataBitmapBytes), blockSize) / blockSize
			if dataBitmapBlocks == 0 {
				dataBitmapBlocks = 1
			}

			// Calculate final data blocks
			finalDataBlocks := uint64(totalBlocks) - 1 - inodeBitmapBlocks - dataBitmapBlocks - journalBlocks

			// EAT (extent allocation table) - 1 block after bitmaps

			// Create superblock
			sb := types.NewSuperblock(uint64(totalBlocks), blockSize, inodeSize, journalBlocks, label)
			sb.Lay.FreeInodes = uint64(estInodes) - 1 // -1 for root inode
			sb.Lay.InodeBMOffset = 1                  // right after superblock
			fmt.Fprintf(os.Stderr, "Debug: Before - InodeBMOffset=%d, DataBitmapOffset=%d, DataBitmapBlocks=%d\n", sb.Lay.InodeBMOffset, sb.Lay.DataBitmapOffset, sb.Lay.DataBitmapBlocks)
			sb.Lay.InodeBMBlocks = inodeBitmapBlocks
			sb.Lay.DataBitmapOffset = 1 + inodeBitmapBlocks
			sb.Lay.DataBitmapBlocks = dataBitmapBlocks
			fmt.Fprintf(os.Stderr, "Debug: After - InodeBMOffset=%d, DataBitmapOffset=%d, DataBitmapBlocks=%d\n", sb.Lay.InodeBMOffset, sb.Lay.DataBitmapOffset, sb.Lay.DataBitmapBlocks)
			sb.Lay.DataBlocks = finalDataBlocks
			sb.Lay.EATOffset = 1 + inodeBitmapBlocks + dataBitmapBlocks + 1
			fmt.Fprintf(os.Stderr, "Debug: EATOffset=%d, EATBlocks=%d\n", sb.Lay.EATOffset, sb.Lay.EATBlocks)
			sb.Lay.EATBlocks = 1

			// Are we working with a file, or a device? If it's a
			// file, don't try and truncate it.

			// Create and truncate file to full size
			file, err := os.Create(path)
			if err != nil {
				return fmt.Errorf("create file: %w", err)
			}
			defer file.Close()

			// Make sure this isn't something we really, really
			// shouldn't be trying to create a volume on, like a
			// directory, character device, etc.
			stat, err := file.Stat()
			if err != nil {
				return fmt.Errorf("stat file: %w", err)
			}

			// It is conceivable that we may need to take symbolic
			// links into account. TODO: check on that.
			if !(stat.Mode().IsRegular() || stat.Mode() & os.ModeDevice != 0 && stat.Mode() & os.ModeCharDevice == 0) {
				return fmt.Errorf("not an appropriate file or device type to create a volume on")
			}

			if stat.Mode().IsRegular() {
				totalSize := sb.Lay.TotalBlocks * sb.Lay.BlockSize
				if err := file.Truncate(int64(totalSize)); err != nil {
					return fmt.Errorf("truncate: %w", err)
				}
			}

			// Write bitmaps first
			inodeBitmap := make([]byte, int(inodeBitmapBlocks)*int(blockSize))
			if _, err := file.WriteAt(inodeBitmap, int64(sb.Lay.InodeBMOffset*sb.Lay.BlockSize)); err != nil {
				return fmt.Errorf("write inode bitmap: %w", err)
			}

			dataBitmap := make([]byte, int(dataBitmapBlocks)*int(blockSize))
			if _, err := file.WriteAt(dataBitmap, int64(sb.Lay.DataBitmapOffset*sb.Lay.BlockSize)); err != nil {
				return fmt.Errorf("write data bitmap: %w", err)
			}

			// Write root inode at first slot of inode table
			inodeTableBlock, inodeByteOffset := calculateInodeLocation(sb, 1)
			rootInode := types.NewInode(1, types.ModeDir|0755)
			
			// Write to file at calculated location
			fileOffset := int64(inodeTableBlock*sb.Lay.BlockSize + inodeByteOffset)
			if err := rootInode.WriteAt(file, fileOffset); err != nil {
				return fmt.Errorf("write root inode: %w", err)
			}

			// Mark root inode as allocated in bitmap
			inodeBitmap[0] |= 1 // Set bit for inode 1
			if _, err := file.WriteAt(inodeBitmap, int64(sb.Lay.InodeBMOffset*sb.Lay.BlockSize)); err != nil {
				return fmt.Errorf("write updated inode bitmap: %w", err)
			}

			// Initialize EAT trie with root node
			// EAT node is 32 bytes: range_start, range_len, free_count, left_child, right_child
			eatNode := make([]byte, 32)
			// range_start = 0 (start of data region)
			binary.LittleEndian.PutUint64(eatNode[0:], 0)
			// range_len = finalDataBlocks (total data blocks)
			binary.LittleEndian.PutUint32(eatNode[8:], uint32(finalDataBlocks))
			// free_count = finalDataBlocks (all free initially)
			binary.LittleEndian.PutUint32(eatNode[12:], uint32(finalDataBlocks))
			// left_child = 0 (no child - this is a leaf node initially)
			binary.LittleEndian.PutUint64(eatNode[16:], 0)
			// right_child = 0 (no child)
			binary.LittleEndian.PutUint64(eatNode[24:], 0)

			// Write EAT node at block 3 (offset 3 * 4096 = 12288)
			if _, err := file.WriteAt(eatNode, int64(sb.Lay.EATOffset*sb.Lay.BlockSize)); err != nil {
				return fmt.Errorf("write EAT trie node: %w", err)
			}

			// Write superblock last (at offset 0)
			sbBlock := make([]byte, sb.Lay.BlockSize)
			copy(sbBlock, sb.MarshalBinary())
			if _, err := file.WriteAt(sbBlock, 0); err != nil {
				return fmt.Errorf("write superblock: %w", err)
			}

			fmt.Fprintf(os.Stderr, "Created filesystem: %s (%d blocks × %d bytes)\n",
				path, totalBlocks, blockSize)
			fmt.Fprintf(os.Stderr, "  inodes:   %d\n", sb.TotalInodes())
			fmt.Fprintf(os.Stderr, "  journal:  %d blocks\n", sb.JournalBlocks())
			fmt.Fprintf(os.Stderr, "  data:     %d blocks\n", sb.DataBlocks())
			fmt.Fprintf(os.Stderr, "  inode bitmap: %d blocks at offset %d\n", sb.Lay.InodeBMBlocks, sb.Lay.InodeBMOffset)
			fmt.Fprintf(os.Stderr, "  data bitmap:  %d blocks at offset %d\n", sb.Lay.DataBitmapBlocks, sb.Lay.DataBitmapOffset)
			fmt.Fprintf(os.Stderr, "  EAT trie: %d blocks at offset %d\n", sb.Lay.EATBlocks, sb.Lay.EATOffset)

			return nil
		},
	}

	if err := app.Run(os.Args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
