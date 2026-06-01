// mkfs.briefs creates a BrieFS filesystem given the relevant parameters.
package main

import (
	"fmt"
	"os"

	"github.com/ctdk/briefs-utils/types"
	"github.com/urfave/cli/v2"
)

func roundUp(value, alignment uint64) uint64 {
	return (value + alignment - 1) / alignment * alignment
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
				Required: true,
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
			dataBlocks := uint64(totalBlocks) - 1 - inodeBitmapBlocks - journalBlocks // -1 for superblock
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

			// Create superblock
			sb := types.NewSuperblock(uint64(totalBlocks), blockSize, inodeSize, journalBlocks, label)
			sb.Lay.FreeInodes = uint64(estInodes) - 1 // -1 for root inode
			sb.Lay.InodeBMOffset = 1          // right after superblock
			sb.Lay.InodeBMBlocks = inodeBitmapBlocks
			sb.Lay.DSBMOffset = 1 + inodeBitmapBlocks
			sb.Lay.DSBMBlocks = dataBitmapBlocks
			sb.Lay.DataBlocks = finalDataBlocks

			// Write superblock first
			if err := sb.Write(path); err != nil {
				return fmt.Errorf("mkfs failed: %w", err)
			}

			// Open file to write bitmaps
			file, err := os.OpenFile(path, os.O_WRONLY, 0644)
			if err != nil {
				return fmt.Errorf("open file: %w", err)
			}
			defer file.Close()

			// Write inode bitmap (all zeros = all free)
			inodeBitmap := make([]byte, int(inodeBitmapBlocks)*int(blockSize))
			if err := file.Truncate(int64(sb.Lay.TotalBlocks * sb.Lay.BlockSize)); err != nil {
				return fmt.Errorf("truncate: %w", err)
			}
			if _, err := file.WriteAt(inodeBitmap, int64(sb.Lay.InodeBMOffset*sb.Lay.BlockSize)); err != nil {
				return fmt.Errorf("write inode bitmap: %w", err)
			}

			// Write data bitmap (all zeros = all free)
			dataBitmap := make([]byte, int(dataBitmapBlocks)*int(blockSize))
			if _, err := file.WriteAt(dataBitmap, int64(sb.Lay.DSBMOffset*sb.Lay.BlockSize)); err != nil {
				return fmt.Errorf("write data bitmap: %w", err)
			}

			// Initialize root inode at block 0 of inode table
			// Inode table follows data bitmap
			inodeTableOffset := sb.Lay.DSBMOffset + sb.Lay.DSBMBlocks
			rootInode := types.NewInode(1, types.ModeDir|0755)
			if err := rootInode.Write(file, inodeTableOffset); err != nil {
				return fmt.Errorf("write root inode: %w", err)
			}

			// Mark root inode as allocated in bitmap
			// Set bit 0 (inode 1) in inode bitmap
			if inodeBitmapBlocks > 0 {
				inodeBitmap[0] |= 1 // Set bit for inode 1
				if _, err := file.WriteAt(inodeBitmap, int64(sb.Lay.InodeBMOffset*sb.Lay.BlockSize)); err != nil {
					return fmt.Errorf("write updated inode bitmap: %w", err)
				}
			}

			fmt.Fprintf(os.Stderr, "Created filesystem: %s (%d blocks × %d bytes)\n",
				path, totalBlocks, blockSize)
			fmt.Fprintf(os.Stderr, "  inodes:   %d\n", sb.TotalInodes())
			fmt.Fprintf(os.Stderr, "  journal:  %d blocks\n", sb.JournalBlocks())
			fmt.Fprintf(os.Stderr, "  data:     %d blocks\n", sb.DataBlocks())
			fmt.Fprintf(os.Stderr, "  inode bitmap: %d blocks at offset %d\n", sb.Lay.InodeBMBlocks, sb.Lay.InodeBMOffset)
			fmt.Fprintf(os.Stderr, "  data bitmap:  %d blocks at offset %d\n", sb.Lay.DSBMBlocks, sb.Lay.DSBMOffset)

			return nil
		},
	}

	if err := app.Run(os.Args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
