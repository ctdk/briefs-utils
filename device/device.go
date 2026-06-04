// Package device provides types and methods for finding the size of a block
// device and calculating the maximum filesystem size on that device, taking
// blocksize and such into account.
package device

import (
	"fmt"
	"io"
	"os"
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
		devErr := fmt.Errorf("error opening device %s: %w", err)
		return nil, devErr
	}
	defer d.Close()

	// Err if it isn't a block device? Or perhaps if it isn't a block device
	// or a file?

	pos, err := d.Seek(0, io.SeekEnd)
	if err != nil {
		devErr := fmt.Errorf("error seeking to the end of %s: %w", err)
		return nil, devErr
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
