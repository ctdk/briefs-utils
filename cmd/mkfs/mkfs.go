// mkfs.briefs creates a BrieFS filesystem given the relevant parameters.
package main

import (
	"fmt"
	"os"

	"github.com/ctdk/briefs-utils/types"
	"github.com/urfave/cli/v2"
)

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
			journalBlocks := c.Int("journal-size")
			label := c.String("label")

			sb := types.NewSuperblock(uint64(totalBlocks), blockSize, inodeSize, uint64(journalBlocks), label)

			if err := sb.Write(path); err != nil {
				return fmt.Errorf("mkfs failed: %w", err)
			}

			fmt.Fprintf(os.Stderr, "Created filesystem: %s (%d blocks × %d bytes)\n",
				path, totalBlocks, blockSize)
			fmt.Fprintf(os.Stderr, "  inodes:   %d\n", sb.TotalInodes())
			fmt.Fprintf(os.Stderr, "  journal:  %d blocks\n", sb.JournalBlocks())
			fmt.Fprintf(os.Stderr, "  data:     %d blocks\n", sb.DataBlocks())

			return nil
		},
	}

	if err := app.Run(os.Args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
