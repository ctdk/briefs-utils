package briefs

import "testing"

// TestBtreeNodeLayout pins the on-disk B+ tree node layout against the kernel's
// struct briefs_extent_btree_node. If the kernel fanout/offset constants drift
// from what these Go helpers assume, fsck would misread every tree-backed inode,
// so this test guards the Go/C struct duplication (there is no shared header).
func TestBtreeNodeLayout(t *testing.T) {
	const blockSize = DefaultBlockSize // 4096

	// Leaf payload region is [BtreeHeaderSize, BtreeChecksumOffset) = blockSize - header - (checksum + slack).
	// 126 extents (4032B) leave 24B of trailing pad; the internal layout fills the
	// same region exactly (253*16+8 = 4056). So the leaf fanout is the max number of
	// 32B extents that fit, not an exact fill.
	leafPayload := blockSize - BtreeHeaderSize - 16 /* checksum + slack */
	if BtreeHeaderSize+BtreeLeafFanout*32 > BtreeChecksumOffset {
		t.Fatalf("leaf fanout drift: %d extents (%d bytes) overflow checksum offset %d",
			BtreeLeafFanout, BtreeHeaderSize+BtreeLeafFanout*32, BtreeChecksumOffset)
	}
	if BtreeHeaderSize+(BtreeLeafFanout+1)*32 <= BtreeChecksumOffset {
		t.Fatalf("leaf fanout drift: %d*32 = %d fits with room for another extent (want max fanout)",
			BtreeLeafFanout, BtreeHeaderSize+BtreeLeafFanout*32)
	}
	if (BtreeChecksumOffset-BtreeHeaderSize)/32 != BtreeLeafFanout {
		t.Fatalf("leaf fanout drift: want %d, got %d", BtreeLeafFanout, (BtreeChecksumOffset-BtreeHeaderSize)/32)
	}

	// Internal payload: BtreeIdxFanout*16 + 8 (trailing_child) must equal the leaf payload.
	if BtreeIdxFanout*16+8 != leafPayload {
		t.Fatalf("idx fanout drift: %d*16+8 = %d, want %d", BtreeIdxFanout, BtreeIdxFanout*16+8, leafPayload)
	}

	// Checksum offset must match the legacy chain block offset (reused CRC coverage).
	if BtreeChecksumOffset != ExtentChainChecksumOffset {
		t.Fatalf("checksum offset drift: btree=%d chain=%d", BtreeChecksumOffset, ExtentChainChecksumOffset)
	}
	if BtreeChecksumOffset != 4080 {
		t.Fatalf("checksum offset: want 4080, got %d", BtreeChecksumOffset)
	}

	// trailing_child sits at header + idx array.
	if BtreeTrailingChildOffset != BtreeHeaderSize+BtreeIdxFanout*16 {
		t.Fatalf("trailing child offset: want %d, got %d",
			BtreeHeaderSize+BtreeIdxFanout*16, BtreeTrailingChildOffset)
	}
	if BtreeTrailingChildOffset != 4072 {
		t.Fatalf("trailing child offset: want 4072, got %d", BtreeTrailingChildOffset)
	}
}