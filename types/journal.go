package types

import (
	"encoding/binary"
	"fmt"
)

// CheckpointSize is the exact on-disk size of a JRN_CHECKPOINT payload,
// matching the kernel's struct jrn_checkpoint (56 bytes).
const CheckpointSize = 56

// Checkpoint mirrors the kernel's struct jrn_checkpoint. It is stored in the
// journal checkpoint block as the payload of a JRN_CHECKPOINT record.
type Checkpoint struct {
	Seq            uint64
	RecordCount    uint32
	Reserved1      uint32
	LogSequenceEnd uint64
	TrieRootNode   uint64
	FreeDataCount  uint64
	FreeInodeCount uint64
	Reserved2      uint64
}

// MarshalBinary serializes the checkpoint to its 56-byte little-endian
// on-disk representation.
func (c *Checkpoint) MarshalBinary() ([]byte, error) {
	data := make([]byte, CheckpointSize)
	binary.LittleEndian.PutUint64(data[0:], c.Seq)
	binary.LittleEndian.PutUint32(data[8:], c.RecordCount)
	binary.LittleEndian.PutUint32(data[12:], c.Reserved1)
	binary.LittleEndian.PutUint64(data[16:], c.LogSequenceEnd)
	binary.LittleEndian.PutUint64(data[24:], c.TrieRootNode)
	binary.LittleEndian.PutUint64(data[32:], c.FreeDataCount)
	binary.LittleEndian.PutUint64(data[40:], c.FreeInodeCount)
	binary.LittleEndian.PutUint64(data[48:], c.Reserved2)
	return data, nil
}

// UnmarshalBinary parses a 56-byte little-endian checkpoint payload.
func (c *Checkpoint) UnmarshalBinary(data []byte) error {
	if len(data) < CheckpointSize {
		return fmt.Errorf("checkpoint payload too short: %d < %d", len(data), CheckpointSize)
	}
	c.Seq = binary.LittleEndian.Uint64(data[0:])
	c.RecordCount = binary.LittleEndian.Uint32(data[8:])
	c.Reserved1 = binary.LittleEndian.Uint32(data[12:])
	c.LogSequenceEnd = binary.LittleEndian.Uint64(data[16:])
	c.TrieRootNode = binary.LittleEndian.Uint64(data[24:])
	c.FreeDataCount = binary.LittleEndian.Uint64(data[32:])
	c.FreeInodeCount = binary.LittleEndian.Uint64(data[40:])
	c.Reserved2 = binary.LittleEndian.Uint64(data[48:])
	return nil
}
