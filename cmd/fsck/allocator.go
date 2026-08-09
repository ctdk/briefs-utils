package main

import (
	"encoding/binary"
	"fmt"
	"math/bits"
	"os"

	"github.com/ctdk/briefs-utils/briefs"
)

var bytesPerWord = uint64(binary.Size(uint64(1))) 
// Recklessly assuming 8 bits per byte. This could cause trouble in case
// this program (or Go itself) are ever ported to the PDP-10 or
// something along those lines.
var bitsPerByte = uint64(8) 
var wordBits = bytesPerWord * bitsPerByte

// verifyAllocatorPool reads and prints the allocator pool header.
func verifyAllocatorPool(file *os.File, poolBlock, blockSize uint64, label string) error {
	hdr, err := briefs.ReadAllocatorHeader(file, poolBlock, blockSize)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "  %s: pool at block %d, %d entries, %d free\n", label, poolBlock, hdr.BlockCount, hdr.FreeCount)
	fmt.Fprintf(os.Stderr, "    levels: L0=%d words, L1=%d words, L2=%d words\n", hdr.L0Words, hdr.L1Words, hdr.L2Words)

	return nil
}

// readAllocatorHeader reads the allocator pool header and returns all fields.
func readAllocatorHeader(file *os.File, poolBlock, blockSize uint64) (l0w, l1w, l2w, blockCount, freeCount uint64, err error) {
	hdr, err := briefs.ReadAllocatorHeader(file, poolBlock, blockSize)
	if err != nil {
		return 0, 0, 0, 0, 0, err
	}
	return hdr.L0Words, hdr.L1Words, hdr.L2Words, hdr.BlockCount, hdr.FreeCount, nil
}

// verifyAllocatorBitmap reads and validates the full 3-level allocator bitmap.
// It checks:
//   - L0 bits correctly summarize L1 (a set L0 bit means at least one L1 word under it is non-zero)
//   - L1 bits correctly summarize L2 (a set L1 bit means at least one L2 word under it is non-zero)
//   - Trailing bits in the last L0/L1/L2 word are properly masked
//   - Computed free count from L2 matches the header's free count
//   - The header's free count matches the superblock's expectation (sbExpectedFree)
func verifyAllocatorBitmap(fs *fsckState, poolBlock, blockSize, sbExpectedFree uint64, label string) {
	errorReportLimit := 10

	// Read the header and all three bitmap levels through the shared codec.
	l0, l1, l2, hdr, err := briefs.ReadAllocatorBitmap(fs.file, poolBlock, blockSize)
	if err != nil {
		fs.errorf("%s: read allocator bitmap: %v", label, err)
		return
	}
	l0w := hdr.L0Words
	l1w := hdr.L1Words
	l2w := hdr.L2Words
	blockCount := hdr.BlockCount
	headerFree := hdr.FreeCount

	if sbExpectedFree != headerFree {
		fs.errorf("%s free count mismatch: superblock says %d, allocator says %d",
			label, sbExpectedFree, headerFree)
	}

	// Compute expected level sizes
	expectedL2 := (blockCount + (wordBits - 1)) / wordBits
	expectedL1 := (expectedL2 + (wordBits - 1)) / wordBits
	expectedL0 := (expectedL1 + (wordBits - 1)) / wordBits
	if expectedL0 < 1 {
		expectedL0 = 1
	}
	if expectedL1 < 1 {
		expectedL1 = 1
	}
	if expectedL2 < 1 {
		expectedL2 = 1
	}

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
			if l1Errors < errorReportLimit {
				fs.errorf("%s: L1 word %d mismatch: on-disk 0x%016X, computed 0x%016X", label, i, l1[i], expected)
			} else if l1Errors == errorReportLimit {
				fs.errorf("%s: (more L1 errors suppressed)", label)
			}
			l1Errors++
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
			if l0Errors < errorReportLimit {
				fs.errorf("%s: L0 word %d mismatch: on-disk 0x%016X, computed 0x%016X", label, i, l0[i], expected)
			} else if l0Errors == errorReportLimit {
				fs.errorf("%s: (more L0 errors suppressed)", label)
			}
			l0Errors++
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

// readAllocatorL2 reads the L2 bitmap words from an allocator pool.
func readAllocatorL2(file *os.File, poolBlock, blockSize uint64) (l2 []uint64, l2w uint64, blockCount uint64, err error) {
	_, _, l2, hdr, err := briefs.ReadAllocatorBitmap(file, poolBlock, blockSize)
	if err != nil {
		return nil, 0, 0, err
	}
	return l2, hdr.L2Words, hdr.BlockCount, nil
}

// verifyInodeBitmapCrossReference checks that every allocated inode bitmap slot
// corresponds to an inode with valid magic on disk.  It does NOT check the
// reverse (a free slot still holding a valid magic): mkfs.briefs no longer
// pre-zeroes the inode table, so on a reused device an unallocated slot
// legitimately retains the previous filesystem's stale inode bytes, including a
// valid magic.  The kernel only consults a slot once it allocates it, so such
// stale content is harmless at runtime and is not an inconsistency worth
// flagging.  An allocated slot that lacks a valid magic, however, is a real
// bitmap/table mismatch and is reported.
func verifyInodeBitmapCrossReference(fs *fsckState, blockSize, inodeSize uint64) {
	inodeTableStart := fs.sb.InodeTableOffset
	inodesPerBlock := blockSize / inodeSize

	l2, _, blockCount, err := readAllocatorL2(fs.file, fs.sb.InodeBMOffset, blockSize)
	if err != nil {
		fs.errorf("inode bitmap cross-ref: %v", err)
		return
	}

	// Check each inode slot
	badAllocated := 0 // bitmap says allocated, but no valid inode magic
	ino := uint64(1)
	errorReportLimit := 20

	for bi := uint64(0); bi < (blockCount+inodesPerBlock-1)/inodesPerBlock; bi++ {
		absBlock := inodeTableStart + bi
		buf := make([]byte, blockSize)
		if _, err := fs.file.ReadAt(buf, int64(absBlock*blockSize)); err != nil {
			fs.errorf("inode bitmap cross-ref: read inode table block %d: %v", absBlock, err)
			ino += inodesPerBlock
			continue
		}

		for j := uint64(0); j < inodesPerBlock && ino <= blockCount; j++ {
			offset := j * inodeSize
			magic := binary.LittleEndian.Uint64(buf[offset+bytesPerWord:])

			w := (ino - 1) / wordBits
			b := (ino - 1) % wordBits
			allocated := w < uint64(len(l2)) && (l2[w]&(1<<b)) == 0

			hasMagic := magic == briefs.MagicInode

			if allocated && !hasMagic {
				if badAllocated < errorReportLimit {
					fs.errorf("ino %d: bitmap says allocated but inode has no valid magic (0x%016X)", ino, magic)
				} else if badAllocated == errorReportLimit {
					fs.errorf("(more inode bitmap/table mismatch errors suppressed)")
				}
				badAllocated++
			}
			ino++
		}
	}

	if badAllocated == 0 {
		fmt.Fprintf(os.Stderr, "  inode bitmap cross-ref: all allocated bitmap entries have valid inode magic\n")
	}
}

// verifySuperblockFreeCounts cross-checks the superblock free counts against
// the allocator headers and the actual inode/found counts.
func verifySuperblockFreeCounts(fs *fsckState, totalInodesFound int) {
	// Read data allocator free count
	_, _, _, _, dataFree, err := readAllocatorHeader(fs.file, fs.sb.TrieNodePoolStart, fs.sb.BlockSize)
	if err == nil {
		if dataFree != fs.sb.FreeDataBlks {
			fs.errorf("superblock free data blocks mismatch: superblock says %d, allocator says %d",
				fs.sb.FreeDataBlks, dataFree)
		}
	}

	// Read inode allocator free count
	_, _, _, _, inodeFree, err := readAllocatorHeader(fs.file, fs.sb.InodeBMOffset, fs.sb.BlockSize)
	if err == nil {
		if inodeFree != fs.sb.FreeInodes {
			fs.errorf("superblock free inodes mismatch: superblock says %d, allocator says %d",
				fs.sb.FreeInodes, inodeFree)
		}
	}

	// Cross-check: total inodes = (blockCount - inodeFree), should be totalInodesFound
	inodeHeader := make([]byte, fs.sb.BlockSize)
	if _, err := fs.file.ReadAt(inodeHeader, int64(fs.sb.InodeBMOffset*fs.sb.BlockSize)); err == nil {
		inodeBlockCount := binary.LittleEndian.Uint64(inodeHeader[32:])
		expectedInodes := int(inodeBlockCount - inodeFree)
		if expectedInodes != totalInodesFound {
			fs.errorf("inode count mismatch: bitmap says %d in-use, inode table scan found %d",
				expectedInodes, totalInodesFound)
		}
	}
}
