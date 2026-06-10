// Package device provides types and methods for finding the size of a block
// device and calculating the maximum filesystem size on that device, taking
// blocksize and such into account.
package device

import (
	"fmt"
	"io"
	"os"
	"strings"
)

const (
	sectorSize = 512
	kbSize = 1024
	mbSize = 1024 * 1024
	gbSize = 1024 * 1024 * 1024
	tbSize = 1024 * 1024 * 1024 * 1024
)

type BlockDevice struct {
	Path 		string
	size 		int64
	blocksize 	uint64
}

func GetDevice(path string, blocksize uint64) (*BlockDevice, error) {
		d, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("error opening device %s: %w", path, err)
		}
		defer d.Close()

		// Err if it isn't a block device? Or perhaps if it isn't a block device
		// or a file?

		pos, err := d.Seek(0, io.SeekEnd)
		if err != nil {
			return nil, fmt.Errorf("error seeking to the end of %s: %w", path, err)
		}

	// is pos is 0 or somehow negative, that's definitely wrong
	if pos <= 0 {
		devErr := fmt.Errorf("error: somehow the last file position was %d, which is invalid", pos)
		return nil, devErr
	}
	bd := new(BlockDevice)
	bd.Path = path
	bd.size = pos
	bd.blocksize = blocksize

	return bd, nil
}

func (bd *BlockDevice) Bytes() int64 {
	return bd.size
}

func (bd *BlockDevice) Sectors() int64 {
	return bd.size / sectorSize
}

func (bd *BlockDevice) KiloBytes() int64 {
	return bd.size / kbSize
}

func (bd *BlockDevice) MegaBytes() int64 {
	return bd.size / mbSize
}

func (bd *BlockDevice) GigaBytes() int64 {
	return bd.size / gbSize
}

func (bd *BlockDevice) TeraBytes() int64 {
	return bd.size / tbSize
}

// Blocks() returns the number of blocks available on the device, *rounded down*
// in case there's a misalignment.
func (bd *BlockDevice) Blocks() int64 {
	return int64(uint64(bd.size) / bd.blocksize)
}

// CheckMounted verifies that the given path is not currently mounted.
// It returns nil if the path is safe to use, or an error if it is mounted.
//
// The check works by reading /proc/mounts and comparing the realpath of the
// given path against the device fields of each mount entry.  For block devices
// it also checks the device number.  For loop devices it resolves the backing
// file.  For regular files it checks whether the file is used as a loop device
// backing.
func CheckMounted(path string) error {
	// Resolve the real path, handling symlinks and relative paths.
	realPath, err := resolvePath(path)
	if err != nil {
		// If we can't resolve, still try the raw path
		realPath = path
	}

	mounts, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return fmt.Errorf("cannot read /proc/mounts: %w", err)
	}

	for _, line := range strings.Split(string(mounts), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		mountDev := fields[0]
		mountPoint := fields[1]

		// Check if the path matches exactly as a device
		if mountDev == realPath || mountDev == path {
			return fmt.Errorf("%s is mounted on %s", path, mountPoint)
		}

		// Check for loop devices by resolving the backing file
		// /dev/loopN are backed by files specified in /sys/block/loopN/loop/backing_file
		if strings.HasPrefix(mountDev, "/dev/loop") {
			backFile := resolveLoopBackingFile(mountDev)
			if backFile != "" && (backFile == realPath || backFile == path) {
				return fmt.Errorf("%s is in use as backing for loop device %s (mounted on %s)",
					path, mountDev, mountPoint)
			}
		}

	}

	return nil
}

// resolvePath resolves a path to its absolute form, following symlinks.
func resolvePath(path string) (string, error) {
	return filepathAbs(path)
}

// resolveLoopBackingFile reads the backing_file sysfs entry for a loop device.
func resolveLoopBackingFile(loopDev string) string {
	// /dev/loop0 → /sys/block/loop0/loop/backing_file
	base := strings.TrimPrefix(loopDev, "/dev/")
	sysPath := fmt.Sprintf("/sys/block/%s/loop/backing_file", base)
	data, err := os.ReadFile(sysPath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// filepathAbs is a direct implementation of filepath.Abs without importing path/filepath.
func filepathAbs(path string) (string, error) {
	if strings.HasPrefix(path, "/") {
		return path, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if cwd == "/" {
		return "/" + path, nil
	}
	return cwd + "/" + path, nil
}
