package main

import (
	"os"

	"github.com/ctdk/briefs-utils/types"
)

// verifySuperblock reads and validates the superblock.
func verifySuperblock(file *os.File, blockSize uint64) (*types.SuperblockLayout, error) {
	sb, err := types.ReadSuperblock(file, blockSize)
	if err != nil {
		return nil, err
	}

	return sb, nil
}
