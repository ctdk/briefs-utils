// briefs-fuse mounts a BrieFS filesystem image via FUSE.
package main

import (
	"fmt"
	"os"

	"github.com/ctdk/briefs-utils/fuse"
	"github.com/urfave/cli/v2"
)

func main() {
	app := &cli.App{
		Name:  "briefs-fuse",
		Usage: "Mount a BrieFS filesystem image via FUSE",
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
		Action: func(c *cli.Context) error {
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

	if err := app.Run(os.Args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}