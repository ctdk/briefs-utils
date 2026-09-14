package main

import (
	"fmt"
	"math/bits"
	"os"

	"github.com/ctdk/briefs-utils/briefs"
)

const bytesPerWord = 8
// Recklessly assuming 8 bits per byte. This could cause trouble in case
// this program (or Go itself) are ever ported to the PDP-10 or
// something along those lines.
const bitsPerByte = 8
const wordBits = bytesPerWord * bitsPerByte

// allocPool is one allocator pool's header and all three bitmap levels,
// read from disk once and shared by every consumer of a verification pass.
type allocPool struct {
	l0, l1, l2 []uint64
	hdr        *briefs.AllocHeader
	err        error // read failure; hdr and the words are nil then
}

// allocatorPool returns the pool at poolBlock, reading it from disk on
// first use. One verification pass touches each pool many times — header
// print, bitmap pyramid check, inode-table slot filter, block
// cross-reference, free-count cross-check, extent-region bound — and the
// selective repair phases load the same pools again; the cache turns all
// of those into one read per pool per pass. runVerificationPass clears it
// so the post-repair pass re-reads the pools repair rewrote.
func (fs *fsckState) allocatorPool(poolBlock uint64) *allocPool {
	if fs.allocPools == nil {
		fs.allocPools = make(map[uint64]*allocPool)
	}
	if p, ok := fs.allocPools[poolBlock]; ok {
		return p
	}
	p := &allocPool{}
	p.l0, p.l1, p.l2, p.hdr, p.err = briefs.ReadAllocatorBitmap(fs.file, poolBlock, fs.sb.BlockSize)
	fs.allocPools[poolBlock] = p
	return p
}

// verifyAllocatorPool reads and prints the allocator pool header.
func verifyAllocatorPool(fs *fsckState, poolBlock uint64, label string) error {
	p := fs.allocatorPool(poolBlock)
	if p.err != nil {
		return p.err
	}
	hdr := p.hdr

	fmt.Fprintf(os.Stderr, "  %s: pool at block %d, %d entries, %d free\n", label, poolBlock, hdr.BlockCount, hdr.FreeCount)
	fmt.Fprintf(os.Stderr, "    levels: L0=%d words, L1=%d words, L2=%d words\n", hdr.L0Words, hdr.L1Words, hdr.L2Words)

	return nil
}

// verifyAllocatorBitmap reads and validates the full 3-level allocator bitmap.
// It checks:
//   - L0 bits correctly summarize L1 (a set L0 bit means at least one L1 word under it is non-zero)
//   - L1 bits correctly summarize L2 (a set L1 bit means at least one L2 word under it is non-zero)
//   - Trailing bits in the last L0/L1/L2 word are properly masked
//   - Computed free count from L2 matches the header's free count
//   - The header's free count matches the superblock's expectation (sbExpectedFree)
func verifyAllocatorBitmap(fs *fsckState, poolBlock, sbExpectedFree uint64, label string) {
	errorReportLimit := 10

	// The header and all three bitmap levels, read once per pass.
	p := fs.allocatorPool(poolBlock)
	if p.err != nil {
		fs.errorf("%s: read allocator bitmap: %v", label, p.err)
		return
	}
	l0, l1, l2, hdr := p.l0, p.l1, p.l2, p.hdr
	l0w := hdr.L0Words
	l1w := hdr.L1Words
	l2w := hdr.L2Words
	blockCount := hdr.BlockCount
	headerFree := hdr.FreeCount

	if sbExpectedFree != headerFree {
		fs.errorf("%s free count mismatch: superblock says %d, allocator says %d",
			label, sbExpectedFree, headerFree)
	}

	// Compute expected level sizes through the shared layout math.
	expectedL0, expectedL1, expectedL2 := briefs.AllocLevelWords(blockCount)

	if l0w != expectedL0 {
		fs.errorf("%s: L0 word count mismatch: header says %d, expected %d", label, l0w, expectedL0)
	}
	if l1w != expectedL1 {
		fs.errorf("%s: L1 word count mismatch: header says %d, expected %d", label, l1w, expectedL1)
	}
	if l2w != expectedL2 {
		fs.errorf("%s: L2 word count mismatch: header says %d, expected %d", label, l2w, expectedL2)
	}

	// Verify trailing bits in last L2 word are properly masked
	if tail := blockCount % wordBits; tail != 0 {
		lastWord := l2[len(l2)-1]
		mask := (uint64(1) << tail) - 1
		if lastWord&^mask != 0 {
			fs.errorf("%s: trailing bits set in last L2 word (0x%016X, mask 0x%016X)", label, lastWord, mask)
		}
	}

	// Verify trailing bits in last L1 word
	if tail := l2w % wordBits; tail != 0 {
		lastWord := l1[len(l1)-1]
		mask := (uint64(1) << tail) - 1
		if lastWord&^mask != 0 {
			fs.errorf("%s: trailing bits set in last L1 word (0x%016X, mask 0x%016X)", label, lastWord, mask)
		}
	}

	// Verify trailing bits in last L0 word
	if tail := l1w % wordBits; tail != 0 {
		lastWord := l0[len(l0)-1]
		mask := (uint64(1) << tail) - 1
		if lastWord&^mask != 0 {
			fs.errorf("%s: trailing bits set in last L0 word (0x%016X, mask 0x%016X)", label, lastWord, mask)
		}
	}

	// Verify L1 -> L2 pyramid: for each L1 word, check its bits correctly
	// summarize the corresponding L2 words.
	l1Errors := 0
	for i := uint64(0); i < l1w; i++ {
		expected := uint64(0)
		start := i * wordBits
		for j := uint64(0); j < wordBits && start+j < l2w; j++ {
			if l2[start+j] != 0 {
				expected |= 1 << j
			}
		}
		if l1[i] != expected {
			fs.reportLimited(&l1Errors, errorReportLimit, fs.errorf,
				"%s: (more L1 errors suppressed)", "%s: L1 word %d mismatch: on-disk 0x%016X, computed 0x%016X",
				label, i, l1[i], expected)
		}
	}

	// Verify L0 -> L1 pyramid
	l0Errors := 0
	for i := uint64(0); i < l0w; i++ {
		expected := uint64(0)
		start := i * wordBits
		for j := uint64(0); j < wordBits && start+j < l1w; j++ {
			if l1[start+j] != 0 {
				expected |= 1 << j
			}
		}
		if l0[i] != expected {
			fs.reportLimited(&l0Errors, errorReportLimit, fs.errorf,
				"%s: (more L0 errors suppressed)", "%s: L0 word %d mismatch: on-disk 0x%016X, computed 0x%016X",
				label, i, l0[i], expected)
		}
	}

	// Compute actual free count from L2 bitmap
	computedFree := uint64(0)
	for i := uint64(0); i < l2w; i++ {
		computedFree += uint64(bits.OnesCount64(l2[i]))
	}

	if computedFree != headerFree {
		fs.errorf("%s: free count mismatch: header says %d, bitmap scan says %d", label, headerFree, computedFree)
	}

	if l1Errors > 0 || l0Errors > 0 {
		return
	}

	fmt.Fprintf(os.Stderr, "  %s bitmap pyramid: consistent (%d L0, %d L1, %d L2 words, %d free)\n",
		label, l0w, l1w, l2w, computedFree)
}

// verifySuperblockFreeCounts cross-checks the superblock free counts against
// the allocator headers and the actual inode/found counts. The headers come
// from the pass-cached pools (a header read failure was already reported by
// whichever pool consumer touched it first, so err is silently skipped here
// exactly as before).
func verifySuperblockFreeCounts(fs *fsckState, totalInodesFound int) {
	// Read data allocator free count
	if p := fs.allocatorPool(fs.sb.TrieNodePoolStart); p.err == nil {
		if p.hdr.FreeCount != fs.sb.FreeDataBlks {
			fs.errorf("superblock free data blocks mismatch: superblock says %d, allocator says %d",
				fs.sb.FreeDataBlks, p.hdr.FreeCount)
		}
	}

	// The inode allocator header: its free count cross-checks the
	// superblock, and its block count drives the in-use tally below.
	if p := fs.allocatorPool(fs.sb.InodeBMOffset); p.err == nil {
		inoHdr := p.hdr
		if inoHdr.FreeCount != fs.sb.FreeInodes {
			fs.errorf("superblock free inodes mismatch: superblock says %d, allocator says %d",
				fs.sb.FreeInodes, inoHdr.FreeCount)
		}
		// Cross-check: total inodes = (blockCount - freeCount), should be
		// totalInodesFound
		expectedInodes := int(inoHdr.BlockCount - inoHdr.FreeCount)
		if expectedInodes != totalInodesFound {
			fs.errorf("inode count mismatch: bitmap says %d in-use, inode table scan found %d",
				expectedInodes, totalInodesFound)
		}
	}
}
