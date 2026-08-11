// fsck.briefs validates and repairs a BrieFS filesystem.
package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/ctdk/briefs-utils/device"
	"github.com/ctdk/briefs-utils/briefs"
	"github.com/ctdk/briefs-utils/manpage"
	"github.com/urfave/cli/v3"
)

// parseRepairOptions converts a comma-separated phase list into a repairOptions
// value. An empty list or "all" enables every phase.
func parseRepairOptions(list string) (*repairOptions, error) {
	list = strings.TrimSpace(list)
	if list == "" || strings.ToLower(list) == "all" {
		return &repairOptions{
			RebuildAllocator: true,
			RepairBtreeCRC:   true,
			RebuildBtree:     true,
			CompactExtents:   true,
			CompactTries:     true,
			RepairLinks:      true,
		}, nil
	}

	opts := &repairOptions{}
	for _, tok := range strings.Split(list, ",") {
		tok = strings.TrimSpace(strings.ToLower(tok))
		switch tok {
		case "allocator":
			opts.RebuildAllocator = true
		case "btrees":
			opts.RepairBtreeCRC = true
		case "btree-rebuild":
			opts.RebuildBtree = true
		case "btree-orphan":
			opts.ReclaimOrphanBtree = true
		case "extents":
			opts.CompactExtents = true
		case "trie":
			opts.CompactTries = true
		case "links":
			opts.RepairLinks = true
		case "":
			// ignore empty tokens
		default:
			return nil, fmt.Errorf("unknown repair phase %q (expected allocator, btrees, btree-rebuild, btree-orphan, extents, trie, or links)", tok)
		}
	}
	return opts, nil
}

func main() {
	app := &cli.Command{
		Name:     "fsck.briefs",
		Usage:    "Check and repair a BrieFS filesystem",
		ArgsUsage: "DEVICE",
		UsageText: "fsck.briefs [global options] DEVICE",
		Version:  briefs.VersionStr,
		HideHelpCommand: true,
		Description: "Check and, with --repair, fix a BrieFS filesystem on " +
			"DEVICE.  fsck.briefs verifies journal-record and B+ tree checksums, " +
			"validates directory-trie pages, inode extended-attribute chains, " +
			"and inode link counts, and can rebuild the allocator bitmaps and " +
			"B+ tree extent indexes, compact directory tries, and repair link " +
			"counts.  Repairs are phased and selectable with --repair-only.",
		Before: func(ctx context.Context, c *cli.Command) (context.Context, error) {
			if c.Bool("generate-man-page") {
				return ctx, nil
			}
			if c.Args().Len() < 1 {
				return ctx, fmt.Errorf("missing required argument: DEVICE")
			}
			path := c.Args().First()
			if err := device.CheckMounted(path); err != nil {
				return ctx, fmt.Errorf("refusing to check filesystem: %w\n", err)
			}
			return ctx, nil
		},
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:   "generate-man-page",
				Hidden: true,
				Usage:  "write the section-8 man page to man/man8/fsck.briefs.8 and exit",
			},
			&cli.BoolFlag{
				Name:    "verbose",
				Aliases: []string{"V"},
				Usage:   "verbose output",
			},
			&cli.BoolFlag{
				Name:    "repair",
				Aliases: []string{"r"},
				Usage:   "attempt to repair found errors",
			},
			&cli.StringFlag{
				Name:  "repair-only",
				Usage: "run only selected repair phases (comma-separated: allocator,btrees,btree-rebuild,btree-orphan,extents,trie,links; default all)",
			},
			&cli.BoolFlag{
				Name:  "optimize",
				Usage: "safe compaction only (alias for --repair --repair-only=trie,extents)",
			},
			&cli.BoolFlag{
				Name:    "assume-yes",
				Aliases: []string{"y"},
				Usage:   "do not ask for confirmation before modifying the volume",
			},
			// fsck(8)-style flags, for xfstests interoperability and direct
			// invocation parity with other fsck.<fstyp> tools:
			&cli.BoolFlag{
				Name:    "no",
				Aliases: []string{"n"},
				Usage:   "read-only check; do not attempt repairs (default mode, made explicit for fsck(8) -n)",
			},
			&cli.BoolFlag{
				Name:    "preen",
				Aliases: []string{"p"},
				Usage:   "non-interactive repair; alias for --repair --assume-yes",
			},
			&cli.StringFlag{
				Name:    "type",
				Aliases: []string{"t"},
				Usage:   "filesystem type (accepted for fsck(8) compatibility; ignored)",
				Value:   "",
			},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			if c.Bool("generate-man-page") {
				wrote, err := manpage.Generate(c, "man/man8")
				if err != nil {
					return err
				}
				fmt.Fprintf(os.Stderr, "wrote %s\n", wrote)
				return nil
			}
			path := c.Args().First()
			repair := c.Bool("repair")
			repairOnly := c.String("repair-only")
			optimize := c.Bool("optimize")
			assumeYes := c.Bool("assume-yes")
			preen := c.Bool("preen")
			noMode := c.Bool("no")

			if optimize && repairOnly != "" {
				return fmt.Errorf("--optimize and --repair-only cannot be used together")
			}
			if optimize {
				repairOnly = "trie,extents"
			}
			// --preen is an alias for --repair --assume-yes: automatic,
			// non-interactive repair (the fsck(8) -p convention).
			if preen {
				repair = true
				assumeYes = true
			}
			// -n forces a read-only check and overrides any repair/preen/optimize
			// request, so `fsck -n` (from xfstests' _check_generic_filesystem and
			// the fsck(8) -n convention) never mutates the volume.
			if noMode {
				repair = false
				repairOnly = ""
				optimize = false
			}
			repairRequested := repair || repairOnly != ""

			var file *os.File
			var err error
			if repairRequested {
				file, err = os.OpenFile(path, os.O_RDWR, 0)
				if err != nil {
					return fmt.Errorf("open device read-write for repair: %w", err)
				}
			} else {
				file, err = os.Open(path)
				if err != nil {
					return fmt.Errorf("open device: %w", err)
				}
			}
			defer file.Close()

			// Probe the device size using seeking (works for both regular
			// files and block devices; os.Stat().Size() returns 0 for
			// block devices).
			bd, err := device.GetDevice(path, 4096)
			if err != nil {
				return fmt.Errorf("probe device size: %w", err)
			}
			deviceSize := bd.Bytes()

			fs := &fsckState{
				file:   file,
				repair: repair,
			}

			fmt.Fprintf(os.Stderr, "BrieFS filesystem check, version %s\n", briefs.VersionStr)
			fmt.Fprintf(os.Stderr, "Device: %s (%d bytes)\n", path, deviceSize)

			// 1. Superblock
			sb, err := verifySuperblock(file, 4096)
			if err != nil {
				return fmt.Errorf("superblock check FAILED: %w", err)
			}
			fs.sb = sb
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

			// Validate superblock field sanity
			if sb.BlockSize < 512 || sb.BlockSize > 65536 || (sb.BlockSize&(sb.BlockSize-1)) != 0 {
				fs.errorf("superblock: invalid block size %d (must be power of 2, 512-65536)", sb.BlockSize)
			}
			if sb.InodeSize < 128 || sb.InodeSize > 4096 || (sb.InodeSize&(sb.InodeSize-1)) != 0 {
				fs.errorf("superblock: invalid inode size %d (must be power of 2, 128-4096)", sb.InodeSize)
			}
			if sb.TotalBlocks == 0 {
				fs.errorf("superblock: zero total blocks")
			}
			if sb.DataBlocks > sb.TotalBlocks {
				fs.errorf("superblock: data blocks (%d) > total blocks (%d)", sb.DataBlocks, sb.TotalBlocks)
			}
			if sb.FreeDataBlks > sb.DataBlocks {
				fs.errorf("superblock: free data blocks (%d) > data blocks (%d)", sb.FreeDataBlks, sb.DataBlocks)
			}
			if sb.RootIno != 1 {
				fs.errorf("superblock: root inode is %d, expected 1", sb.RootIno)
			}
			if sb.InodeTableOffset == 0 {
				fs.errorf("superblock: inode table offset is 0")
			}
			if sb.JournalOffset == 0 || sb.JournalBlocks == 0 {
				fs.errorf("superblock: invalid journal offset %d / blocks %d", sb.JournalOffset, sb.JournalBlocks)
			}
			if sb.JournalOffset+sb.JournalBlocks > sb.TotalBlocks {
				fs.errorf("superblock: journal extends past end of device (offset %d + blocks %d > total %d)",
					sb.JournalOffset, sb.JournalBlocks, sb.TotalBlocks)
			}

			totalInodes := runVerificationPass(fs, blockSize, sb.InodeSize)

			// 7. Repair / optimization phase
			if repairRequested {
				if fs.errors > 0 && len(fs.failedTrieDirs) > 0 {
					fmt.Fprintf(os.Stderr, "Refusing repair: %d director(ies) have unrecoverable trie errors\n", len(fs.failedTrieDirs))
					fmt.Fprintf(os.Stderr, "FSCK COMPLETE: %d error(s) found, repair skipped\n", fs.errors)
					return nil
				}
				if sb.JournalLogStart != sb.JournalLogEnd {
					fmt.Fprintf(os.Stderr, "Refusing repair: journal has un-replayed records\n")
					fmt.Fprintf(os.Stderr, "FSCK COMPLETE: %d error(s) found, repair skipped\n", fs.errors)
					return nil
				}

				repairOpts, err := parseRepairOptions(repairOnly)
				if err != nil {
					return err
				}

				// Refuse repair when a tree-backed inode's B+ tree walk failed AND
				// the allocator rebuild phase is active: that phase rebuilds the
				// on-disk bitmap from fs.usedBlocks, and a failed walk never
				// recorded the inode's extents/node blocks, so they would be
				// marked free — permanent data loss. Non-allocator phases
				// (--optimize, --repair-only without the allocator token) load the
				// on-disk allocator instead and are not destructive, so they may
				// proceed (the user can still run e.g. --repair-only=links to fix
				// other faults while leaving the torn tree's blocks intact).
				// This mirrors the failedTrieDirs guard above.
				if len(fs.failedBtreeInos) > 0 && repairOpts.RebuildAllocator {
					fmt.Fprintf(os.Stderr, "Refusing repair: %d inode(s) have unrecoverable B-tree extent-index errors; rebuilding the allocator would free their blocks\n", len(fs.failedBtreeInos))
					fmt.Fprintf(os.Stderr, "FSCK COMPLETE: %d error(s) found, repair skipped\n", fs.errors)
					return nil
				}

				if !assumeYes {
					fmt.Fprintf(os.Stderr, "This will modify %s. Proceed? (y/N) ", path)
					reader := bufio.NewReader(os.Stdin)
					line, err := reader.ReadString('\n')
					if err != nil || (line != "y\n" && line != "Y\n") {
						fmt.Fprintf(os.Stderr, "\nRepair cancelled.\n")
						return nil
					}
				}

				if err := runRepair(fs, blockSize, totalInodes, repairOpts); err != nil {
					fmt.Fprintf(os.Stderr, "Repair failed: %v\n", err)
					fmt.Fprintf(os.Stderr, "FSCK COMPLETE: %d error(s) found, repair failed\n", fs.errors)
					return nil
				}
				fmt.Fprintf(os.Stderr, "\nRepair complete. Re-running verification pass...\n")
				fs.errors = 0
				fs.inodes = make(map[uint64]*briefs.Inode)
				fs.dirs = nil
				fs.usedBlocks = make(map[uint64]bool)
				fs.entryCounts = make(map[uint64]int)
				fs.failedTrieDirs = make(map[uint64]bool)
				fs.failedBtreeInos = make(map[uint64]bool)
				runVerificationPass(fs, blockSize, sb.InodeSize)
			}

			// 8. Summary
			fmt.Fprintf(os.Stderr, "\n")
			if fs.errors > 0 {
				fmt.Fprintf(os.Stderr, "FSCK COMPLETE: %d error(s) found\n", fs.errors)
			} else {
				fmt.Fprintf(os.Stderr, "FSCK COMPLETE: no errors found\n")
			}

			return nil
		},
	}

	if err := app.Run(context.Background(), os.Args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
