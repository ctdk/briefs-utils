package briefs

// CheckpointSize is the exact on-disk size of a JRN_CHECKPOINT payload,
// matching the kernel's struct jrn_checkpoint (56 bytes).
const CheckpointSize = 56

// Checkpoint mirrors the kernel's struct jrn_checkpoint. It is stored in the
// journal checkpoint block as the payload of a JRN_CHECKPOINT record.
//
//go:briefs-disk size=56
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
