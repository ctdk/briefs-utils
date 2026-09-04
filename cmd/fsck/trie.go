package main

import (
	"fmt"
	"math/bits"

	"github.com/ctdk/briefs-utils/briefs"
)

// trieEntry represents a single directory entry found in the trie.
type trieEntry struct {
	Inode  uint64
	FType  uint8
	Name   string
	Parent uint64 // parent directory inode
}

// dirInfo stores info about a directory inode for later trie walking.
type dirInfo struct {
	ino      uint64
	trieRoot uint64
}

// trieReadAt adapts fsck's file reads to the shared trie walker.
func trieReadAt(fs *fsckState, blockSize uint64) briefs.TrieReadFunc {
	return func(block uint64) ([]byte, error) {
		buf := make([]byte, blockSize)
		if _, err := fs.file.ReadAt(buf, int64(block*blockSize)); err != nil {
			return nil, err
		}
		return buf, nil
	}
}

// verifyDirectoryTrie walks a directory's packed trie, validating structure
// and collecting entries.  Traversal mechanics live in briefs.WalkTrie; this
// is the validation pass: it reports every structural problem it can see
// (through fsck's Note hook) but keeps walking, marks the directory in
// failedTrieDirs when the walk is not trustworthy, and returns the entries
// found.
func verifyDirectoryTrie(fs *fsckState, parentIno uint64, rootRef uint64, blockSize uint64) []trieEntry {
	if rootRef == 0 {
		return nil
	}
	var entries []trieEntry

	err := briefs.WalkTrie(trieReadAt(fs, blockSize), rootRef, briefs.TrieVisitor{
		Note: func(ref uint64, kind briefs.TrieWalkNote, nerr error) error {
			switch kind {
			case briefs.TrieNoteRead:
				fs.errorf("ino %d dir trie: read page %d: %v", parentIno, briefs.TrieRefBlock(ref), nerr)
				fs.failedTrieDirs[parentIno] = true
			case briefs.TrieNotePage, briefs.TrieNoteSlot:
				fs.errorf("ino %d dir trie: ref %d: %v", parentIno, ref, nerr)
				fs.failedTrieDirs[parentIno] = true
			case briefs.TrieNoteCycle:
				fs.errorf("ino %d dir trie: cycle detected at ref %d", parentIno, ref)
			case briefs.TrieNoteSiblingCap:
				fs.errorf("ino %d dir trie: sibling chain from ref %d exceeds %d nodes (corrupt/cyclic trie)",
					parentIno, ref, briefs.TrieSiblingMax)
				fs.failedTrieDirs[parentIno] = true
			case briefs.TrieNoteSiblingRead:
				// The child page could not be read or parsed while the walk
				// followed the chain; the chain gathered so far is kept.
				fs.errorf("ino %d dir trie: ref %d: %v", parentIno, ref, nerr)
			}
			return nil // every condition is reported; the walk continues
		},
		VisitNode: func(ref uint64, emitted bool, buf []byte, page *briefs.TriePage, node *briefs.TrieSlot) error {
			if emitted {
				// Post-entry re-visit of a leaf that also branches: the
				// walk is already handling its children, and the node was
				// fully validated on its first visit.
				return nil
			}
			block := briefs.TrieRefBlock(ref)
			slot := briefs.TrieRefSlot(ref)

			// Record the containing page as used.
			fs.usedBlocks[block] = true

			// Cross-check the page header's live_count against the free-slot bitmap.
			allocated := bits.OnesCount64(page.FreeSlots)
			if allocated != int(briefs.TrieSlotsPerBlock-page.LiveCount) {
				fs.errorf("ino %d dir trie: page %d live_count=%d inconsistent with free_slots bitmap (%d allocated)",
					parentIno, block, page.LiveCount, allocated)
			}

			if page.FreeSlots&(1<<slot) != 0 {
				fs.errorf("ino %d dir trie: ref %d: slot %d is marked free", parentIno, ref, slot)
				fs.failedTrieDirs[parentIno] = true
				return briefs.ErrSkipTrieNode
			}

			// Validate node type.
			if node.NodeType != 0 && node.NodeType != briefs.NodeTypeInterm &&
				node.NodeType != (briefs.NodeTypeInterm|briefs.NodeStatusLeaf) {
				fs.errorf("ino %d dir trie: ref %d: invalid node type 0x%02X", parentIno, ref, node.NodeType)
			}

			// Validate node flags.
			if node.Flags&uint16(briefs.NodeFlagDeleted) != 0 {
				fs.warnf("ino %d dir trie: ref %d: NODE_FLAG_DELETED set (pending cleanup)", parentIno, ref)
			}
			if ref != rootRef && node.Flags&uint16(briefs.NodeFlagRoot) != 0 {
				fs.errorf("ino %d dir trie: ref %d: NODE_FLAG_ROOT set on non-root node", parentIno, ref)
			}
			if node.Flags&^(uint16(briefs.NodeFlagDeleted|briefs.NodeFlagRoot)) != 0 {
				fs.warnf("ino %d dir trie: ref %d: unknown flags 0x%04X", parentIno, ref, node.Flags)
			}

			// Validate depth and byte_val for root.
			if ref == rootRef && node.Depth != 0 {
				fs.errorf("ino %d dir trie: root ref %d: depth is %d, expected 0", parentIno, ref, node.Depth)
			}
			if ref == rootRef && node.ByteVal != 0 {
				fs.errorf("ino %d dir trie: root ref %d: byte_val is %d, expected 0", parentIno, ref, node.ByteVal)
			}

			// Validate child_count vs first_child.
			if node.ChildCount == 0 && node.FirstChild != 0 {
				fs.errorf("ino %d dir trie: ref %d: child_count=0 but first_child=%d", parentIno, ref, node.FirstChild)
			}
			if node.ChildCount > 0 && node.FirstChild == 0 {
				fs.errorf("ino %d dir trie: ref %d: child_count=%d but first_child=0", parentIno, ref, node.ChildCount)
			}

			// Validate child/sibling ref ranges.
			if node.FirstChild > 0 && briefs.TrieRefBlock(node.FirstChild) >= fs.sb.TotalBlocks {
				fs.errorf("ino %d dir trie: ref %d: first_child block %d exceeds total blocks %d",
					parentIno, ref, briefs.TrieRefBlock(node.FirstChild), fs.sb.TotalBlocks)
			}
			if node.NextSibling > 0 && briefs.TrieRefBlock(node.NextSibling) >= fs.sb.TotalBlocks {
				fs.errorf("ino %d dir trie: ref %d: next_sibling block %d exceeds total blocks %d",
					parentIno, ref, briefs.TrieRefBlock(node.NextSibling), fs.sb.TotalBlocks)
			}
			return nil
		},
		VisitLeaf: func(ref uint64, buf []byte, node *briefs.TrieSlot) error {
			if node.Flags&uint16(briefs.NodeFlagDeleted) != 0 {
				return nil
			}
			name, err := briefs.ReadTrieName(buf, node.NameLen, node.NameOffset)
			if err != nil {
				fs.errorf("ino %d dir trie: ref %d: empty or invalid name (name_len=%d, name_offset=%d): %v",
					parentIno, ref, node.NameLen, node.NameOffset, err)
				return nil
			}
			entries = append(entries, trieEntry{
				Inode:  node.Inode,
				FType:  node.FType,
				Name:   name,
				Parent: parentIno,
			})
			fs.entryCounts[node.Inode]++
			return nil
		},
	})
	if err != nil {
		// WalkTrie errors are unreachable here: every hook returns nil.
		fs.errorf("ino %d dir trie: %v", parentIno, err)
	}
	return entries
}

// verifyAllDirTries walks the trie of every directory inode found during the inode table scan.
// It collects all entries and returns them for cross-referencing.
func verifyAllDirTries(fs *fsckState, blockSize uint64, dirs []dirInfo) []trieEntry {
	var allEntries []trieEntry

	for _, d := range dirs {
		entries := verifyDirectoryTrie(fs, d.ino, d.trieRoot, blockSize)
		allEntries = append(allEntries, entries...)
	}

	return allEntries
}

// collectDirectoryEntries walks a directory trie and returns all live
// entries.  Unlike verifyDirectoryTrie, it does not emit fsck errors; it
// returns an error only on structural problems that prevent collection.
func collectDirectoryEntries(fs *fsckState, parentIno uint64, rootRef uint64, blockSize uint64) ([]trieEntry, error) {
	if rootRef == 0 {
		return nil, nil
	}
	var entries []trieEntry
	err := briefs.WalkTrie(trieReadAt(fs, blockSize), rootRef, briefs.TrieVisitor{
		Note: func(ref uint64, kind briefs.TrieWalkNote, nerr error) error {
			switch kind {
			case briefs.TrieNoteRead:
				return fmt.Errorf("read page %d: %w", briefs.TrieRefBlock(ref), nerr)
			case briefs.TrieNotePage:
				return fmt.Errorf("page %d: %w", briefs.TrieRefBlock(ref), nerr)
			case briefs.TrieNoteSlot:
				return nerr
			case briefs.TrieNoteCycle:
				return fmt.Errorf("cycle detected at ref %d", ref)
			case briefs.TrieNoteSiblingCap, briefs.TrieNoteSiblingRead:
				// Keep the chain gathered so far and continue: the caller
				// gets every entry that is still reachable.
				return nil
			}
			return nil
		},
		VisitLeaf: func(ref uint64, buf []byte, node *briefs.TrieSlot) error {
			if node.Flags&uint16(briefs.NodeFlagDeleted) != 0 {
				return nil
			}
			name, err := briefs.ReadTrieName(buf, node.NameLen, node.NameOffset)
			if err != nil {
				return nil // unverifiable name: skip the entry, keep walking
			}
			entries = append(entries, trieEntry{
				Inode:  node.Inode,
				FType:  node.FType,
				Name:   name,
				Parent: parentIno,
			})
			return nil
		},
	})
	if err != nil {
		return nil, err
	}
	return entries, nil
}

// collectDirectoryTrieBlocks returns the set of absolute block numbers used
// by a directory trie. This is used to free old pages after compaction.
func collectDirectoryTrieBlocks(fs *fsckState, rootRef uint64, blockSize uint64) (map[uint64]bool, error) {
	blocks := make(map[uint64]bool)
	if rootRef == 0 {
		return blocks, nil
	}
	err := briefs.WalkTrie(trieReadAt(fs, blockSize), rootRef, briefs.TrieVisitor{
		Note: func(ref uint64, kind briefs.TrieWalkNote, nerr error) error {
			switch kind {
			case briefs.TrieNoteRead:
				return fmt.Errorf("read page %d: %w", briefs.TrieRefBlock(ref), nerr)
			case briefs.TrieNotePage:
				return fmt.Errorf("page %d: %w", briefs.TrieRefBlock(ref), nerr)
			case briefs.TrieNoteSlot:
				return nerr
			case briefs.TrieNoteCycle:
				return nil // already visited: skip
			case briefs.TrieNoteSiblingCap:
				return fmt.Errorf("sibling chain from ref %d exceeds %d nodes (corrupt/cyclic trie)",
					ref, briefs.TrieSiblingMax)
			case briefs.TrieNoteSiblingRead:
				return nil // keep the chain gathered so far
			}
			return nil
		},
		VisitNode: func(ref uint64, emitted bool, buf []byte, page *briefs.TriePage, node *briefs.TrieSlot) error {
			blocks[briefs.TrieRefBlock(ref)] = true
			return nil
		},
	})
	if err != nil {
		return nil, err
	}
	return blocks, nil
}
