// fuse.briefs mounts a BrieFS filesystem image via FUSE.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/ctdk/briefs-utils/fuse"
	"github.com/ctdk/briefs-utils/briefs"
	"github.com/ctdk/briefs-utils/manpage"
	"github.com/urfave/cli/v3"
)

func main() {
	app := &cli.Command{
		Name:  "fuse.briefs",
		Usage: "Mount a BrieFS filesystem image via FUSE",
		UsageText: "fuse.briefs [global options]",
		Version: briefs.VersionStr,
		HideHelpCommand: true,
		Description: "Mount a BrieFS filesystem image as a FUSE filesystem.  " +
			"The bridge is read-write with full kernel parity: all directory " +
			"and file operations, extended attributes, chattr/fileattr, " +
			"renameat2 (EXCHANGE/WHITEOUT), fallocate, setattr, and killpriv.  " +
			"It ports the kernel journal to Go and replays it on mount, so " +
			"FUSE-written volumes are crash-consistent and kernel-mountable.  " +
			"The bridge is experimental; see xfstests-fuse-status.md for the " +
			"xfstests pass/fail record.",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:   "generate-man-page",
				Hidden: true,
				Usage:  "write the section-8 man page to man/man8/fuse.briefs.8 and exit",
			},
			&cli.StringFlag{
				Name:    "image",
				Aliases: []string{"i"},
				Usage:   "filesystem image file or block device",
			},
			&cli.StringFlag{
				Name:    "mountpoint",
				Aliases: []string{"m"},
				Usage:   "mount point directory",
			},
			&cli.BoolFlag{
				Name:    "debug",
				Aliases: []string{"d"},
				Usage:   "enable FUSE debug output",
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
			imagePath := c.String("image")
			mountPoint := c.String("mountpoint")
			debug := c.Bool("debug")

			if imagePath == "" {
				return fmt.Errorf("required flag --image, -i not provided")
			}
			if mountPoint == "" {
				return fmt.Errorf("required flag --mountpoint, -m not provided")
			}

			// Verify mount point exists
			if fi, err := os.Stat(mountPoint); err != nil {
				return fmt.Errorf("mount point: %w", err)
			} else if !fi.IsDir() {
				return fmt.Errorf("mount point %s is not a directory", mountPoint)
			}

			fmt.Fprintf(os.Stderr, "Mounting %s on %s\n", imagePath, mountPoint)
			return fuse.Mount(imagePath, fuse.MountOptions{
				MountPoint: mountPoint,
				Debug:      debug,
			})
		},
	}

	if err := app.Run(context.Background(), os.Args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
