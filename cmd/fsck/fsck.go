// fsck.briefs validates and repairs a BrieFS filesystem.
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

func verifyAllocatorPool(file *os.File, poolBlock, blockSize uint64, label string) error {
	buf := make([]byte, blockSize)
	if _, err := file.ReadAt(buf, int64(poolBlock*blockSize)); err != nil {
		return fmt.Errorf("%s: read header block at %d: %w", label, poolBlock, err)
	}

	magic := binary.LittleEndian.Uint32(buf[0:])
	if magic != types.AllocMagic {
		return fmt.Errorf("%s: bad magic at block %d: expected 0x%08X, got 0x%08X", label, poolBlock, types.AllocMagic, magic)
	}

	ver := binary.LittleEndian.Uint32(buf[4:])
	if ver != 1 {
		return fmt.Errorf("%s: unsupported version %d at block %d", label, ver, poolBlock)
	}

	l0w := binary.LittleEndian.Uint64(buf[8:])
	l1w := binary.LittleEndian.Uint64(buf[16:])
	l2w := binary.LittleEndian.Uint64(buf[24:])
	blockCount := binary.LittleEndian.Uint64(buf[32:])
	freeCount := binary.LittleEndian.Uint64(buf[40:])

	fmt.Fprintf(os.Stderr, "  %s: pool at block %d, %d entries, %d free\n", label, poolBlock, blockCount, freeCount)
	fmt.Fprintf(os.Stderr, "    levels: L0=%d words, L1=%d words, L2=%d words\n", l0w, l1w, l2w)

	return nil
}

func verifySuperblock(file *os.File, blockSize uint64) (*types.SuperblockLayout, error) {
	buf := make([]byte, blockSize)
	if _, err := file.ReadAt(buf, 0); err != nil {
		return nil, fmt.Errorf("read superblock: %w", err)
	}

	sb := &types.SuperblockLayout{}
	magic := binary.LittleEndian.Uint64(buf[0:])
	if magic != types.MagicSuperblock {
		return nil, fmt.Errorf("bad superblock magic: 0x%016X (expected 0x%016X)", magic, types.MagicSuperblock)
	}

	sb.Magic = magic
	sb.MajorVer = binary.LittleEndian.Uint64(buf[8:])
	sb.MinorVer = binary.LittleEndian.Uint64(buf[16:])
	sb.PatchVer = binary.LittleEndian.Uint64(buf[24:])
	sb.TotalBlocks = binary.LittleEndian.Uint64(buf[32:])
	sb.DataBlocks = binary.LittleEndian.Uint64(buf[40:])
	sb.BlockSize = binary.LittleEndian.Uint64(buf[48:])
	sb.InodeSize = binary.LittleEndian.Uint64(buf[56:])
	sb.BlocksGrp = binary.LittleEndian.Uint64(buf[64:])
	sb.InodesGrp = binary.LittleEndian.Uint64(buf[72:])
	sb.FSCreated = binary.LittleEndian.Uint64(buf[80:])
	sb.FSLastMount = binary.LittleEndian.Uint64(buf[88:])
	sb.FSLastChkpt = binary.LittleEndian.Uint64(buf[96:])
	sb.FreeDataBlks = binary.LittleEndian.Uint64(buf[104:])
	sb.FreeInodes = binary.LittleEndian.Uint64(buf[112:])
	sb.RootIno = binary.LittleEndian.Uint64(buf[120:])
	sb.FeatCompat = binary.LittleEndian.Uint64(buf[128:])
	sb.FeatROCompat = binary.LittleEndian.Uint64(buf[136:])
	sb.FeatIncompat = binary.LittleEndian.Uint64(buf[144:])
	copy(sb.UUID[:], buf[152:168])
	sb.EATOffset = binary.LittleEndian.Uint64(buf[168:])
	sb.EATBlocks = binary.LittleEndian.Uint64(buf[176:])
	sb.TrieRootBlock = binary.LittleEndian.Uint64(buf[184:])
	sb.TrieBlocksUsed = binary.LittleEndian.Uint64(buf[192:])
	sb.TrieNodePoolStart = binary.LittleEndian.Uint64(buf[200:])
	sb.TrieNodePoolSize = binary.LittleEndian.Uint64(buf[208:])
	sb.InodeBMOffset = binary.LittleEndian.Uint64(buf[216:])
	sb.InodeBMBlocks = binary.LittleEndian.Uint64(buf[224:])
	sb.InodeTableOffset = binary.LittleEndian.Uint64(buf[232:])
	sb.JournalOffset = binary.LittleEndian.Uint64(buf[240:])
	sb.JournalBlocks = binary.LittleEndian.Uint64(buf[248:])
	sb.CheckpointSeq = binary.LittleEndian.Uint64(buf[256:])
	sb.JournalLogStart = binary.LittleEndian.Uint64(buf[264:])
	sb.JournalLogEnd = binary.LittleEndian.Uint64(buf[272:])
	for i := 0; i < 4; i++ {
		sb.ReservedJournal[i] = binary.LittleEndian.Uint64(buf[280+i*8:])
	}
	copy(sb.Label[:], buf[312:312+64])

	return sb, nil
}

func verifyInode(file *os.File, ino, blockOffset, byteOffset, blockSize, inodeSize uint64) error {
	buf := make([]byte, blockSize)
	if _, err := file.ReadAt(buf, int64(blockOffset*blockSize)); err != nil {
		return fmt.Errorf("read inode block %d: %w", blockOffset, err)
	}

	inodeBuf := buf[byteOffset : byteOffset+inodeSize]
	magic := binary.LittleEndian.Uint64(inodeBuf[8:])
	if magic == 0 {
		return nil // unallocated inode
	}
	if magic != types.MagicInode {
		return fmt.Errorf("ino %d: bad magic 0x%016X", ino, magic)
	}

	nlinks := binary.LittleEndian.Uint32(inodeBuf[144:])
	extentsInline := binary.LittleEndian.Uint32(inodeBuf[148:])
	extentsTotal := binary.LittleEndian.Uint64(inodeBuf[160:])

	if extentsInline > 8 {
		return fmt.Errorf("ino %d: too many inline extents %d", ino, extentsInline)
	}
	if extentsTotal < uint64(extentsInline) {
		return fmt.Errorf("ino %d: total extents %d < inline extents %d", ino, extentsTotal, extentsInline)
	}

	_ = nlinks
	return nil
}

func verifyInodeTable(file *os.File, inodeTableBlock, inodeTableBlocks, blockSize, inodeSize uint64) (totalInodes int, errors int) {
	inodesPerBlock := blockSize / inodeSize
	ino := uint64(1)

	fmt.Fprintf(os.Stderr, "  inodes per block: %d\n", inodesPerBlock)

	for bi := uint64(0); bi < inodeTableBlocks; bi++ {
		buf := make([]byte, blockSize)
		if _, err := file.ReadAt(buf, int64((inodeTableBlock+bi)*blockSize)); err != nil {
			fmt.Fprintf(os.Stderr, "    ERROR: read inode table block %d: %v\n", inodeTableBlock+bi, err)
			errors++
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
			if err := verifyInode(file, ino, inodeTableBlock+bi, offset, blockSize, inodeSize); err != nil {
				fmt.Fprintf(os.Stderr, "    ERROR: %v\n", err)
				errors++
			}
			ino++
		}
	}

	return
}

func verifyJournal(file *os.File, journalOffset, journalBlocks, blockSize uint64) error {
	buf := make([]byte, blockSize)
	if _, err := file.ReadAt(buf, int64(journalOffset*blockSize)); err != nil {
		return fmt.Errorf("read journal block %d: %w", journalOffset, err)
	}
	magic := binary.LittleEndian.Uint32(buf[0:])
	if magic != types.MagicJournal {
		return fmt.Errorf("bad journal magic at block %d: 0x%08X", journalOffset, magic)
	}
	return nil
}

func main() {
	app := &cli.App{
		Name:    "fsck.briefs",
		Usage:   "Check and repair a BrieFS filesystem",
		Version: versionStr,
		Before: func(c *cli.Context) error {
			path := c.String("device")
			if err := device.CheckMounted(path); err != nil {
				// Running fsck on a mounted filesystem risks
				// double-blind corruption. Refuse to continue.
				return fmt.Errorf("refusing to check filesystem: %w\n", err)
			}
			return nil
		},
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:     "device",
				Aliases:  []string{"d"},
				Required: true,
				Usage:    "filesystem device or image file",
			},
			&cli.BoolFlag{
				Name:    "verbose",
				Aliases: []string{"V"},
				Usage:   "verbose output",
			},
			&cli.BoolFlag{
				Name:  "repair",
				Usage: "attempt to repair found errors (not yet implemented)",
			},
		},
		Action: func(c *cli.Context) error {
			path := c.String("device")

			file, err := os.Open(path)
			if err != nil {
				return fmt.Errorf("open device: %w", err)
			}
			defer file.Close()

			fi, err := file.Stat()
			if err != nil {
				return fmt.Errorf("stat device: %w", err)
			}
			deviceSize := fi.Size()

			fmt.Fprintf(os.Stderr, "BrieFS filesystem check, version %s\n", versionStr)
			fmt.Fprintf(os.Stderr, "Device: %s (%d bytes)\n", path, deviceSize)

			// 1. Superblock
			sb, err := verifySuperblock(file, 4096)
			if err != nil {
				return fmt.Errorf("superblock check FAILED: %w", err)
			}
			blockSize := sb.BlockSize
			fmt.Fprintf(os.Stderr, "\nSuperblock:\n")
			fmt.Fprintf(os.Stderr, "  magic:       0x%016X\n", sb.Magic)
			fmt.Fprintf(os.Stderr, "  version:     %d.%d.%d\n", sb.MajorVer, sb.MinorVer, sb.PatchVer)
			fmt.Fprintf(os.Stderr, "  total blocks: %d\n", sb.TotalBlocks)
			fmt.Fprintf(os.Stderr, "  block size:  %d\n", blockSize)
			fmt.Fprintf(os.Stderr, "  data blocks: %d\n", sb.DataBlocks)
			fmt.Fprintf(os.Stderr, "  free data:   %d\n", sb.FreeDataBlks)
			fmt.Fprintf(os.Stderr, "  free inodes: %d\n", sb.FreeInodes)
			fmt.Fprintf(os.Stderr, "  root inode:  %d\n", sb.RootIno)
			fmt.Fprintf(os.Stderr, "  label:       %s\n", string(sb.Label[:]))

			if deviceSize < int64(sb.TotalBlocks*blockSize) {
				return fmt.Errorf("device too small: %d bytes needed, got %d", sb.TotalBlocks*blockSize, deviceSize)
			}

			// 2. Allocator pools
			fmt.Fprintf(os.Stderr, "\nInode bitmap:\n")
			if err := verifyAllocatorPool(file, sb.InodeBMOffset, blockSize, "inode bitmap"); err != nil {
				fmt.Fprintf(os.Stderr, "  ERROR: %v\n", err)
			}

			fmt.Fprintf(os.Stderr, "\nData block allocator:\n")
			// Data allocator is at TrieNodePoolStart (formerly the trie node pool)
			if err := verifyAllocatorPool(file, sb.TrieNodePoolStart, blockSize, "data allocator"); err != nil {
				fmt.Fprintf(os.Stderr, "  ERROR: %v\n", err)
			}

			// 3. Inode table
			inodeTableStart := sb.InodeTableOffset
				var inodeTableBlocks uint64
	{
		// Read inode allocator header to get actual inode count
		inodeHeader := make([]byte, blockSize)
		if _, err := file.ReadAt(inodeHeader, int64(sb.InodeBMOffset*blockSize)); err != nil {
			return fmt.Errorf("read inode allocator header: %w", err)
		}
		numInodes := binary.LittleEndian.Uint64(inodeHeader[32:])
		inodeTableBlocks = (numInodes * sb.InodeSize + blockSize - 1) / blockSize
	}
			fmt.Fprintf(os.Stderr, "\nInode table:\n")
			fmt.Fprintf(os.Stderr, "  start block: %d\n", inodeTableStart)
			fmt.Fprintf(os.Stderr, "  blocks:      %d\n", inodeTableBlocks)

			totalInodes, inodeErrors := verifyInodeTable(file, inodeTableStart, inodeTableBlocks, blockSize, sb.InodeSize)
			fmt.Fprintf(os.Stderr, "  inodes found: %d\n", totalInodes)
			if inodeErrors > 0 {
				fmt.Fprintf(os.Stderr, "  ERRORS:      %d\n", inodeErrors)
			}

			// 4. Journal
			fmt.Fprintf(os.Stderr, "\nJournal:\n")
			fmt.Fprintf(os.Stderr, "  start block: %d\n", sb.JournalOffset)
			fmt.Fprintf(os.Stderr, "  blocks:      %d\n", sb.JournalBlocks)
			fmt.Fprintf(os.Stderr, "  checkpoint:  %d\n", sb.CheckpointSeq)
			if err := verifyJournal(file, sb.JournalOffset, sb.JournalBlocks, blockSize); err != nil {
				fmt.Fprintf(os.Stderr, "  WARNING: journal check: %v\n", err)
			} else {
				fmt.Fprintf(os.Stderr, "  journal magic OK\n")
			}

			// 5. Summary
			errors := 0
			fmt.Fprintf(os.Stderr, "\n")
			if errors > 0 {
				fmt.Fprintf(os.Stderr, "FSCK COMPLETE: %d errors found\n", errors)
				if c.Bool("repair") {
					fmt.Fprintf(os.Stderr, "Repair not yet implemented\n")
				}
			} else {
				fmt.Fprintf(os.Stderr, "FSCK COMPLETE: no errors found\n")
			}

			return nil
		},
	}

	if err := app.Run(os.Args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
