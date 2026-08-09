package main

import (
	"fmt"
	"math/bits"
	"os"

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

// verifyDirectoryTrie walks a directory's packed trie, validating structure and collecting entries.
// Returns the list of entries found, or nil if the trie is empty.
func verifyDirectoryTrie(fs *fsckState, parentIno uint64, rootRef uint64, blockSize uint64) []trieEntry {
	if rootRef == 0 {
		return nil
	}

	// Track visited node references to detect cycles.
	visited := make(map[uint64]bool)
	var entries []trieEntry

	// Iterative depth-first walk using a stack of node references.
	stack := []uint64{rootRef}
	leafEmitted := []bool{false}

	for len(stack) > 0 {
		ref := stack[len(stack)-1]
		emitted := leafEmitted[len(leafEmitted)-1]
		stack = stack[:len(stack)-1]
		leafEmitted = leafEmitted[:len(leafEmitted)-1]

		if visited[ref] && !emitted {
			fs.errorf("ino %d dir trie: cycle detected at ref %d", parentIno, ref)
			continue
		}
		if !emitted {
			visited[ref] = true
		}

		block := briefs.TrieRefBlock(ref)
		slot := briefs.TrieRefSlot(ref)

		// Record the containing page as used.
		fs.usedBlocks[block] = true

		buf := make([]byte, blockSize)
		if _, err := fs.file.ReadAt(buf, int64(block*blockSize)); err != nil {
			fs.errorf("ino %d dir trie: read page %d: %v", parentIno, block, err)
			fs.failedTrieDirs[parentIno] = true
			continue
		}

		page, err := briefs.ReadTriePage(buf)
		if err != nil {
			fs.errorf("ino %d dir trie: ref %d: %v", parentIno, ref, err)
			fs.failedTrieDirs[parentIno] = true
			continue
		}

		// Cross-check the page header's live_count against the free-slot bitmap.
		allocated := bits.OnesCount64(page.FreeSlots)
		if allocated != int(briefs.TrieSlotsPerBlock-page.LiveCount) {
			fs.errorf("ino %d dir trie: page %d live_count=%d inconsistent with free_slots bitmap (%d allocated)",
				parentIno, block, page.LiveCount, allocated)
		}

		if slot >= briefs.TrieSlotsPerBlock {
			fs.errorf("ino %d dir trie: ref %d: slot %d out of range", parentIno, ref, slot)
			fs.failedTrieDirs[parentIno] = true
			continue
		}

		if page.FreeSlots&(1<<slot) != 0 {
			fs.errorf("ino %d dir trie: ref %d: slot %d is marked free", parentIno, ref, slot)
			fs.failedTrieDirs[parentIno] = true
			continue
		}

		node, err := briefs.ReadTrieSlot(buf, slot)
		if err != nil {
			fs.errorf("ino %d dir trie: ref %d: %v", parentIno, ref, err)
			fs.failedTrieDirs[parentIno] = true
			continue
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
		if ref == rootRef && node.Flags&uint16(briefs.NodeFlagRoot) != 0 {
			// NODE_FLAG_ROOT defined but unused.
		}
		if ref != rootRef && node.Flags&uint16(briefs.NodeFlagRoot) != 0 {
			fs.errorf("ino %d dir trie: ref %d: NODE_FLAG_ROOT set on non-root node", parentIno, ref)
		}
		if node.Flags&^(uint16(briefs.NodeFlagDeleted|briefs.NodeFlagRoot)) != 0 {
			fs.warnf("ino %d dir trie: ref %d: unknown flags 0x%04X", parentIno, ref, node.Flags)
		}

		// Validate depth and byte_val for root.
		if ref == rootRef && node.Depth != 0 {
			fs.errorf("ino %d dir trie: root ref %d: depth is %d, expected 0", parentIno, ref)
		}
		if ref == rootRef && node.ByteVal != 0 {
			fs.errorf("ino %d dir trie: root ref %d: byte_val is %d, expected 0", parentIno, ref)
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

		// If already emitted, just push children.
		if emitted {
			goto pushChildren
		}

		// Extract leaf entry if this node has one.
		if briefs.TrieIsLeaf(node.NodeType) {
			if node.Flags&uint16(briefs.NodeFlagDeleted) == 0 {
				name, err := briefs.ReadTrieName(buf, node.NameLen, node.NameOffset)
				if err != nil {
					fs.errorf("ino %d dir trie: ref %d: empty or invalid name (name_len=%d, name_offset=%d): %v",
						parentIno, ref, node.NameLen, node.NameOffset, err)
				} else {
					entries = append(entries, trieEntry{
						Inode:  node.Inode,
						FType:  node.FType,
						Name:   name,
						Parent: parentIno,
					})
					fs.entryCounts[node.Inode]++
				}
			}

			if node.FirstChild != 0 {
				stack = append(stack, ref)
				leafEmitted = append(leafEmitted, true)
				continue
			}
		}

	pushChildren:
		if node.FirstChild != 0 {
			var siblings []uint64
			child := node.FirstChild
			for child != 0 {
				siblings = append(siblings, child)
				childBlock := briefs.TrieRefBlock(child)
				childSlot := briefs.TrieRefSlot(child)
				cbuf := make([]byte, blockSize)
				if _, err := fs.file.ReadAt(cbuf, int64(childBlock*blockSize)); err != nil {
					fs.errorf("ino %d dir trie: read child page %d: %v", parentIno, childBlock, err)
					break
				}
				if _, err := briefs.ReadTriePage(cbuf); err != nil {
					fs.errorf("ino %d dir trie: child ref %d: %v", parentIno, child, err)
					break
				}
				cn, err := briefs.ReadTrieSlot(cbuf, childSlot)
				if err != nil {
					fs.errorf("ino %d dir trie: child ref %d: %v", parentIno, child, err)
					break
				}
				child = cn.NextSibling
			}
			for i := len(siblings) - 1; i >= 0; i-- {
				stack = append(stack, siblings[i])
				leafEmitted = append(leafEmitted, false)
			}
		}

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

// collectDirectoryEntries walks a directory trie and returns all live entries.
// Unlike verifyDirectoryTrie, it does not emit fsck errors; it returns an error
// only on structural problems that prevent collection.
func collectDirectoryEntries(fs *fsckState, parentIno uint64, rootRef uint64, blockSize uint64) ([]trieEntry, error) {
	if rootRef == 0 {
		return nil, nil
	}
	visited := make(map[uint64]bool)
	var entries []trieEntry
	stack := []uint64{rootRef}
	leafEmitted := []bool{false}

	for len(stack) > 0 {
		ref := stack[len(stack)-1]
		emitted := leafEmitted[len(leafEmitted)-1]
		stack = stack[:len(stack)-1]
		leafEmitted = leafEmitted[:len(leafEmitted)-1]

		if visited[ref] && !emitted {
			return nil, fmt.Errorf("cycle detected at ref %d", ref)
		}
		if !emitted {
			visited[ref] = true
		}

		block := briefs.TrieRefBlock(ref)
		slot := briefs.TrieRefSlot(ref)

		buf := make([]byte, blockSize)
		if _, err := fs.file.ReadAt(buf, int64(block*blockSize)); err != nil {
			return nil, fmt.Errorf("read page %d: %w", block, err)
		}
		if _, err := briefs.ReadTriePage(buf); err != nil {
			return nil, fmt.Errorf("page %d: %w", block, err)
		}
		if slot >= briefs.TrieSlotsPerBlock {
			return nil, fmt.Errorf("slot %d out of range", slot)
		}

		node, err := briefs.ReadTrieSlot(buf, slot)
		if err != nil {
			return nil, err
		}

		if emitted {
			stack, leafEmitted = pushChildren(stack, leafEmitted, fs.file, node, blockSize)
			continue
		}

		if briefs.TrieIsLeaf(node.NodeType) {
			if node.Flags&uint16(briefs.NodeFlagDeleted) == 0 {
				name, err := briefs.ReadTrieName(buf, node.NameLen, node.NameOffset)
				if err == nil && name != "" {
					entries = append(entries, trieEntry{
						Inode:  node.Inode,
						FType:  node.FType,
						Name:   name,
						Parent: parentIno,
					})
				}
			}
			if node.FirstChild != 0 {
				stack = append(stack, ref)
				leafEmitted = append(leafEmitted, true)
				continue
			}
		}

		stack, leafEmitted = pushChildren(stack, leafEmitted, fs.file, node, blockSize)
	}

	return entries, nil
}

// pushChildren pushes a node's children onto the walk stack in reverse order so
// that they are processed first-child-first. It returns the updated slices.
func pushChildren(stack []uint64, leafEmitted []bool, file *os.File, node *briefs.TrieSlot, blockSize uint64) ([]uint64, []bool) {
	if node.FirstChild == 0 {
		return stack, leafEmitted
	}
	var siblings []uint64
	child := node.FirstChild
	for child != 0 {
		siblings = append(siblings, child)
		cbuf := make([]byte, blockSize)
		if _, err := file.ReadAt(cbuf, int64(briefs.TrieRefBlock(child)*blockSize)); err != nil {
			break
		}
		cn, err := briefs.ReadTrieSlot(cbuf, briefs.TrieRefSlot(child))
		if err != nil {
			break
		}
		child = cn.NextSibling
	}
	for i := len(siblings) - 1; i >= 0; i-- {
		stack = append(stack, siblings[i])
		leafEmitted = append(leafEmitted, false)
	}
	return stack, leafEmitted
}

// collectDirectoryTrieBlocks returns the set of absolute block numbers used by a
// directory trie. This is used to free old pages after compaction.
func collectDirectoryTrieBlocks(fs *fsckState, parentIno uint64, rootRef uint64, blockSize uint64) (map[uint64]bool, error) {
	blocks := make(map[uint64]bool)
	if rootRef == 0 {
		return blocks, nil
	}
	visited := make(map[uint64]bool)
	stack := []uint64{rootRef}

	for len(stack) > 0 {
		ref := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if visited[ref] {
			continue
		}
		visited[ref] = true

		block := briefs.TrieRefBlock(ref)
		slot := briefs.TrieRefSlot(ref)
		blocks[block] = true

		buf := make([]byte, blockSize)
		if _, err := fs.file.ReadAt(buf, int64(block*blockSize)); err != nil {
			return nil, fmt.Errorf("read page %d: %w", block, err)
		}
		if _, err := briefs.ReadTriePage(buf); err != nil {
			return nil, fmt.Errorf("page %d: %w", block, err)
		}

		node, err := briefs.ReadTrieSlot(buf, slot)
		if err != nil {
			return nil, err
		}

		child := node.FirstChild
		for child != 0 {
			if !visited[child] {
				stack = append(stack, child)
			}
			cbuf := make([]byte, blockSize)
			if _, err := fs.file.ReadAt(cbuf, int64(briefs.TrieRefBlock(child)*blockSize)); err != nil {
				break
			}
			cn, err := briefs.ReadTrieSlot(cbuf, briefs.TrieRefSlot(child))
			if err != nil {
				break
			}
			child = cn.NextSibling
		}
	}

	return blocks, nil
}