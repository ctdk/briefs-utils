package main

import (
	"os"

	"github.com/ctdk/briefs-utils/briefs"
)

// verifySuperblock reads and validates the superblock.
func verifySuperblock(file *os.File, blockSize uint64) (*briefs.SuperblockLayout, error) {
	sb, err := briefs.ReadSuperblock(file, blockSize)
	if err != nil {
		return nil, err
	}

	return sb, nil
}
