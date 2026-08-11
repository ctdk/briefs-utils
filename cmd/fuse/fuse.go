// briefs-fuse mounts a BrieFS filesystem image via FUSE.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/ctdk/briefs-utils/fuse"
	"github.com/ctdk/briefs-utils/briefs"
	"github.com/urfave/cli/v3"
)

func main() {
	app := &cli.Command{
		Name:  "briefs-fuse",
		Usage: "Mount a BrieFS filesystem image via FUSE",
		Version: briefs.VersionStr,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:     "image",
				Aliases:  []string{"i"},
				Required: true,
				Usage:    "filesystem image file or block device",
			},
			&cli.StringFlag{
				Name:     "mountpoint",
				Aliases:  []string{"m"},
				Required: true,
				Usage:    "mount point directory",
			},
			&cli.BoolFlag{
				Name:    "debug",
				Aliases: []string{"d"},
				Usage:   "enable FUSE debug output",
			},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			imagePath := c.String("image")
			mountPoint := c.String("mountpoint")
			debug := c.Bool("debug")

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
