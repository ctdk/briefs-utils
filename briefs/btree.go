package briefs

// B+ tree extent index (v0.9.0). Mirrors struct briefs_extent_btree_node and
// friends from the kernel module's briefs.h / briefs_btree.c.
//
// A tree-backed inode (InodeFlagIndexed set, ExtentInlineBase == root block)
// stores its extents in the leaves of an offset-keyed B+ tree. An inline-only
// inode (flag clear, ExtentInlineBase == 0) keeps up to 8 extents in the inode
// itself. The 9th extent spills inline -> tree.
//
// Node layout (4096 bytes): 24-byte header, payload, u64 checksum at offset
// 4080, 8 bytes slack. The checksum covers bytes [0, 4080) — identical coverage
// to the legacy extent chain block — so ComputeChainChecksum / ReadChainChecksum
// are reused verbatim.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
)

// BtreeMagic is the magic number stored in every B+ tree node header ("BTRE").
const BtreeMagic = 0x42545245

// BtreeFlagLeaf is the flags bit marking a node as a leaf (bit 0).
const BtreeFlagLeaf = 0x0001

// BtreeHeaderSize is the size of struct briefs_btree_header in bytes.
const BtreeHeaderSize = 24

// BtreeChecksumOffset is the byte offset of the checksum field. Same as the
// chain block (4080): coverage is [0, BtreeChecksumOffset).
const BtreeChecksumOffset = ExtentChainChecksumOffset

// BtreeLeafFanout is the max number of extent records in a leaf node:
// (BlockSize - HeaderSize - ChecksumAndSlack) / 32 = (4096 - 24 - 16) / 32 = 126.
const BtreeLeafFanout = 126

// BtreeIdxFanout is the max number of separator keys (and idx entries) in an
// internal node: (4096 - 24 - 16 - 8 trailing_child) / 16 = 253.
const BtreeIdxFanout = 253

// BtreeTrailingChildOffset is the fixed offset of an internal node's
// trailing_child field: HeaderSize + BtreeIdxFanout*16 = 24 + 4048 = 4072.
const BtreeTrailingChildOffset = BtreeHeaderSize + BtreeIdxFanout*16

// BtreeMaxDepth caps recursive descent as a guard against corrupt/cyclic
// trees. A 16-level tree holds far more extents than any real file.
const BtreeMaxDepth = 16

// Errors returned by the B-tree walk. fsck formats these with the offending
// inode/block number.
var (
	ErrBtreeBadMagic       = errors.New("bad magic")
	ErrBtreeChecksum       = errors.New("checksum mismatch")
	ErrBtreeDepth          = errors.New("depth exceeded")
	ErrBtreeCycle          = errors.New("cycle detected")
	ErrBtreeUnsorted        = errors.New("extents unsorted")
	ErrBtreeCountOverflow  = errors.New("count exceeds fanout")
	ErrBtreeBadHighKey      = errors.New("separator high_key not strictly ascending")
	ErrBtreeBadChild        = errors.New("bad child pointer")
	ErrBtreeCrossLeafUnsorted = errors.New("cross-leaf extents unsorted")
	ErrBtreeCountMismatch  = errors.New("extent count != num_extents_total")
)

// BtreeNodeHeader is the 24-byte on-disk header of a B+ tree node, matching
// struct briefs_btree_header in the kernel (briefs.h). The kernel layout is:
// magic@0, flags@4, level@8, num_keys@10, 4 bytes padding@12, next_leaf@16.
// There is NO prev_leaf — the kernel comment (briefs.h) states no path iterates
// backward. next_leaf threads leaves left-to-right and is 0 for the last leaf
// (and for all internal nodes, which set it to 0 on split). The explicit _Pad
// reproduces the kernel's 4-byte alignment padding at offset 12 so the
// generated marshal matches the on-disk layout exactly.
//
//go:briefs-disk size=24
type BtreeNodeHeader struct {
	Magic    uint32
	Flags    uint32
	Level    uint16
	NumKeys  uint16
	_Pad     uint32
	NextLeaf uint64 // offset 16; 0 if none (leaf only); internal nodes always 0
}

// UnmarshalBtreeHeader reads the node header from a block buffer.
func UnmarshalBtreeHeader(buf []byte) BtreeNodeHeader {
	var h BtreeNodeHeader
	_ = h.UnmarshalBinary(buf)
	return h
}

// IsLeaf reports whether the node header marks a leaf.
func (h BtreeNodeHeader) IsLeaf() bool {
	return h.Flags&BtreeFlagLeaf != 0
}

// ReadBtreeLeafExtent reads the i-th extent from a B-tree leaf node buffer.
func ReadBtreeLeafExtent(buf []byte, i int) Extent {
	offset := BtreeHeaderSize + i*32
	var e Extent
	_ = e.UnmarshalBinary(buf[offset:])
	return e
}

// BtreeIdxEntry is one internal-node entry: a child block pointer and the
// high_key separator (the smallest key in the right sibling, > 0).
//
//go:briefs-disk size=16
type BtreeIdxEntry struct {
	Child   uint64
	HighKey uint64
}

// ReadBtreeIdxEntry reads the i-th internal idx entry from a node buffer.
func ReadBtreeIdxEntry(buf []byte, i int) BtreeIdxEntry {
	offset := BtreeHeaderSize + i*16
	var e BtreeIdxEntry
	_ = e.UnmarshalBinary(buf[offset:])
	return e
}

// BtreeTrailingChild reads the trailing_child pointer of an internal node.
func BtreeTrailingChild(buf []byte) uint64 {
	return binary.LittleEndian.Uint64(buf[BtreeTrailingChildOffset:])
}

// MarshalBtreeHeader writes a BtreeNodeHeader into a node buffer. Bytes 12-15
// (kernel padding) are zeroed so the checksummed region is deterministic. It is
// the inverse of UnmarshalBtreeHeader.
func MarshalBtreeHeader(buf []byte, hdr BtreeNodeHeader) {
	data, _ := hdr.MarshalBinary()
	copy(buf[:BtreeHeaderSize], data)
}

// PutBtreeLeafExtent writes the i-th extent into a B-tree leaf node buffer. It is
// the inverse of ReadBtreeLeafExtent. Does NOT recompute the checksum.
func PutBtreeLeafExtent(buf []byte, i int, ext Extent) {
	offset := BtreeHeaderSize + i*32
	data, _ := ext.MarshalBinary()
	copy(buf[offset:], data)
}

// PutBtreeIdxEntry writes the i-th internal idx entry into a node buffer. It is
// the inverse of ReadBtreeIdxEntry. Does NOT recompute the checksum.
func PutBtreeIdxEntry(buf []byte, i int, e BtreeIdxEntry) {
	offset := BtreeHeaderSize + i*16
	data, _ := e.MarshalBinary()
	copy(buf[offset:], data)
}

// SetBtreeTrailingChild writes the trailing_child pointer of an internal node.
// It is the inverse of BtreeTrailingChild. Does NOT recompute the checksum.
func SetBtreeTrailingChild(buf []byte, child uint64) {
	binary.LittleEndian.PutUint64(buf[BtreeTrailingChildOffset:], child)
}

// SetBtreeNodeChecksum recomputes the node checksum over [0, BtreeChecksumOffset)
// and writes it into the checksum field. Call after any mutation to the
// checksummed region (header, extents, idx entries, trailing child).
func SetBtreeNodeChecksum(buf []byte, blockSize uint64) {
	binary.LittleEndian.PutUint64(buf[BtreeChecksumOffset:], ComputeChainChecksum(buf, blockSize))
}

// WriteBtreeNode writes a node buffer to its block on disk.
func WriteBtreeNode(file *os.File, block, blockSize uint64, buf []byte) error {
	_, err := file.WriteAt(buf, int64(block*blockSize))
	return err
}

// VerifyBtreeNodeChecksum checks a B-tree node's checksum (no legacy zero
// exemption: a B-tree node always carries a real checksum). Returns nil if OK.
func VerifyBtreeNodeChecksum(buf []byte, blockSize uint64) error {
	if uint64(len(buf)) < blockSize || blockSize < BtreeChecksumOffset {
		return ErrBtreeChecksum
	}
	stored := ReadChainChecksum(buf, blockSize)
	if stored == 0 {
		return ErrBtreeChecksum
	}
	if stored != ComputeChainChecksum(buf, blockSize) {
		return ErrBtreeChecksum
	}
	return nil
}

// InodeExtentVisitor is called by IterateInodeExtents. VisitNode is invoked
// for every B-tree node block read (leaf or internal) so the caller can record
// it as a used metadata block; VisitExtent is invoked once per leaf extent in
// ascending offset order. For inline-only inodes only VisitExtent is called
// (no nodes). A non-nil error from either aborts the walk.
type InodeExtentVisitor struct {
	VisitNode   func(block uint64) error
	VisitExtent func(ext Extent) error
}

// IterateInodeExtents walks every extent of @in in ascending logical-offset
// order, dispatching to the visitor. Handles inline-data (no extents),
// inline-only (the inline array), and tree-backed (B+ tree leaves via
// next_leaf) inodes. The tree descent is bounded by BtreeMaxDepth and a
// visited-set cycle guard. Returns the first visitor error or a tree-structure
// error (wrapped with the offending block number).
func IterateInodeExtents(file *os.File, in *Inode, blockSize uint64, v InodeExtentVisitor) error {
	// Inline-data inodes reference no data extents.
	if in.Flags&InodeFlagInlineData != 0 {
		return nil
	}

	// Inline-only: walk the inline array (already sorted).
	if in.Flags&InodeFlagIndexed == 0 {
		inlineExtents := in.InlineExtents()
		// Cap NumExtentsInline to 8 (the fixed inline extent array size).
		// Malformed inodes could have a larger value, which would panic.
		maxExtents := in.NumExtentsInline
		if maxExtents > 8 {
			maxExtents = 8
		}
		for ei := uint32(0); ei < maxExtents; ei++ {
			if v.VisitExtent != nil {
				if err := v.VisitExtent(inlineExtents[ei]); err != nil {
					return err
				}
			}
		}
		return nil
	}

	// Tree-backed.
	root := in.ExtentInlineBase
	if root == 0 {
		return nil
	}
	visited := make(map[uint64]bool)
	return btreeWalk(file, root, blockSize, 0, visited, v)
}

// btreeWalk recursively descends the subtree rooted at @block, yielding leaf
// extents. @depth bounds recursion; @visited guards against cycles.
func btreeWalk(file *os.File, block, blockSize uint64, depth int, visited map[uint64]bool, v InodeExtentVisitor) error {
	if block == 0 {
		return nil
	}
	if depth > BtreeMaxDepth {
		return fmt.Errorf("btree node %d: %w", block, ErrBtreeDepth)
	}
	if visited[block] {
		return fmt.Errorf("btree node %d: %w", block, ErrBtreeCycle)
	}
	visited[block] = true

	if v.VisitNode != nil {
		if err := v.VisitNode(block); err != nil {
			return err
		}
	}

	buf := make([]byte, blockSize)
	if _, err := file.ReadAt(buf, int64(block*blockSize)); err != nil {
		return fmt.Errorf("btree node %d: read: %w", block, err)
	}

	hdr := UnmarshalBtreeHeader(buf)
	if hdr.Magic != BtreeMagic {
		return fmt.Errorf("btree node %d: %w (0x%08X)", block, ErrBtreeBadMagic, hdr.Magic)
	}
	if err := VerifyBtreeNodeChecksum(buf, blockSize); err != nil {
		return fmt.Errorf("btree node %d: %w", block, err)
	}

	if hdr.IsLeaf() {
		if int(hdr.NumKeys) > BtreeLeafFanout {
			return fmt.Errorf("btree node %d: %w (leaf %d > %d)", block, ErrBtreeCountOverflow, hdr.NumKeys, BtreeLeafFanout)
		}
		var prevOffset uint64
		for i := uint16(0); i < hdr.NumKeys; i++ {
			ext := ReadBtreeLeafExtent(buf, int(i))
			if i > 0 && ext.Offset <= prevOffset {
				return fmt.Errorf("btree node %d: %w (offset %d after %d)", block, ErrBtreeUnsorted, ext.Offset, prevOffset)
			}
			prevOffset = ext.Offset
			if v.VisitExtent != nil {
				if err := v.VisitExtent(ext); err != nil {
					return err
				}
			}
		}
		return nil
	}

	// Internal node.
	if int(hdr.NumKeys) > BtreeIdxFanout {
		return fmt.Errorf("btree node %d: %w (internal %d > %d)", block, ErrBtreeCountOverflow, hdr.NumKeys, BtreeIdxFanout)
	}
	for i := uint16(0); i < hdr.NumKeys; i++ {
		entry := ReadBtreeIdxEntry(buf, int(i))
		if err := btreeWalk(file, entry.Child, blockSize, depth+1, visited, v); err != nil {
			return err
		}
	}
	trailing := BtreeTrailingChild(buf)
	return btreeWalk(file, trailing, blockSize, depth+1, visited, v)
}

// BtreeNodeInfo describes one node visited during WalkBtree.
type BtreeNodeInfo struct {
	Block  uint64
	Hdr    BtreeNodeHeader
	Buf    []byte
	Depth  int  // 0 at the root, increments per descent level
	IsRoot bool // true only for the tree root
	// ExpectedLevel is parent.Level - 1; it is meaningful only when IsRoot is
	// false and lets a visitor enforce the "child sits one level below its
	// parent" invariant.
	ExpectedLevel uint16
}

// BtreeNodeVisitor receives parsed nodes during WalkBtree. VisitNode is called
// for every node (leaf or internal) after it passes WalkBtree's structural
// checks (magic, fanout, within-leaf sort, and checksum when VerifyCRC is set),
// before its children are recursed (internal) or its extents yielded (leaf).
// VisitLeaf is called for each leaf after VisitNode, with the parsed extents in
// ascending offset order. A non-nil error from either callback aborts the walk
// immediately, regardless of Tolerant.
type BtreeNodeVisitor struct {
	VisitNode func(BtreeNodeInfo) error
	VisitLeaf func(BtreeNodeInfo, []Extent) error
}

// BtreeWalkOptions configures WalkBtree's fault policy.
type BtreeWalkOptions struct {
	BlockSize uint64

	// VerifyCRC, when true, verifies each node's checksum and treats a mismatch
	// as a fault. When false, checksums are not checked — use only when the tree
	// has already been validated (e.g. a structural verify pass running after the
	// extent-collection pass).
	VerifyCRC bool

	// NullChildIsFault, when true, treats a 0 child pointer as a fault. When
	// false (the default), a 0 child is skipped silently, matching the basic
	// extent walk which tolerates dangling pointers after range deletes.
	NullChildIsFault bool

	// Tolerant, when true, routes every fault to OnFault and continues walking
	// sibling subtrees (the offending subtree is skipped) instead of aborting.
	// When false (the default), the first fault aborts the walk and is returned.
	Tolerant bool

	// OnFault is called once per fault when Tolerant is true, with the offending
	// block number (0 for a null child) and a wrapped error. It is never called
	// when Tolerant is false. A nil OnFault tolerates faults silently.
	OnFault func(block uint64, err error)
}

// WalkBtree descends the B+ tree rooted at @root, dispatching each node to the
// visitor after it passes the structural checks selected by opts. It is the
// shared descent behind the fsck extent-collection and structural-verification
// walks: it centralizes the read/magic/fanout/sort/checksum skeleton and the
// cycle guard, and lets each caller choose a fault policy (strict vs tolerant)
// and supply the per-node checks it cares about (high-key/level ordering,
// cross-leaf cursors, extent accumulation) via the visitor callbacks.
//
// The descent is bounded by BtreeMaxDepth and a visited-set cycle guard. It
// does NOT re-implement the basic IterateInodeExtents walk, whose VisitNode
// fires before the block is read (for used-block recording) — a different
// contract than this post-validation visitor.
func WalkBtree(file *os.File, root uint64, opts BtreeWalkOptions, v BtreeNodeVisitor) error {
	visited := make(map[uint64]bool)
	return walkBtreeNode(file, root, opts, v, visited, 0, 0, true)
}

func walkBtreeNode(file *os.File, block uint64, opts BtreeWalkOptions, v BtreeNodeVisitor, visited map[uint64]bool, depth int, expectedLevel uint16, isRoot bool) error {
	// fault surfaces the error for @block/@err: when tolerant, report via
	// OnFault and return nil so sibling subtrees continue; when strict, return
	// the wrapped error to abort the whole walk.
	fault := func(b uint64, err error) error {
		wrapped := fmt.Errorf("btree node %d: %w", b, err)
		if opts.Tolerant {
			if opts.OnFault != nil {
				opts.OnFault(b, wrapped)
			}
			return nil
		}
		return wrapped
	}

	if block == 0 {
		if opts.NullChildIsFault {
			return fault(block, fmt.Errorf("%w (null child pointer)", ErrBtreeBadChild))
		}
		return nil
	}
	if depth > BtreeMaxDepth {
		return fault(block, ErrBtreeDepth)
	}
	if visited[block] {
		return fault(block, ErrBtreeCycle)
	}
	visited[block] = true

	buf := make([]byte, opts.BlockSize)
	if _, err := file.ReadAt(buf, int64(block*opts.BlockSize)); err != nil {
		return fault(block, fmt.Errorf("read: %w", err))
	}
	hdr := UnmarshalBtreeHeader(buf)
	if hdr.Magic != BtreeMagic {
		return fault(block, fmt.Errorf("%w (0x%08X)", ErrBtreeBadMagic, hdr.Magic))
	}
	if opts.VerifyCRC {
		if err := VerifyBtreeNodeChecksum(buf, opts.BlockSize); err != nil {
			return fault(block, err)
		}
	}

	info := BtreeNodeInfo{
		Block:         block,
		Hdr:            hdr,
		Buf:            buf,
		Depth:          depth,
		IsRoot:         isRoot,
		ExpectedLevel:  expectedLevel,
	}

	if hdr.IsLeaf() {
		if int(hdr.NumKeys) > BtreeLeafFanout {
			return fault(block, fmt.Errorf("%w (leaf %d > %d)", ErrBtreeCountOverflow, hdr.NumKeys, BtreeLeafFanout))
		}
		extents := make([]Extent, 0, hdr.NumKeys)
		var prev uint64
		for i := uint16(0); i < hdr.NumKeys; i++ {
			ext := ReadBtreeLeafExtent(buf, int(i))
			if i > 0 && ext.Offset <= prev {
				return fault(block, fmt.Errorf("%w (offset %d after %d)", ErrBtreeUnsorted, ext.Offset, prev))
			}
			prev = ext.Offset
			extents = append(extents, ext)
		}
		if v.VisitNode != nil {
			if err := v.VisitNode(info); err != nil {
				return err
			}
		}
		if v.VisitLeaf != nil {
			if err := v.VisitLeaf(info, extents); err != nil {
				return err
			}
		}
		return nil
	}

	// Internal node.
	if int(hdr.NumKeys) > BtreeIdxFanout {
		return fault(block, fmt.Errorf("%w (internal %d > %d)", ErrBtreeCountOverflow, hdr.NumKeys, BtreeIdxFanout))
	}
	if v.VisitNode != nil {
		if err := v.VisitNode(info); err != nil {
			return err
		}
	}
	// Recurse into children left-to-right (idx children then the trailing
	// child), yielding leaves in ascending key order. Each child must sit one
	// level below this node.
	childLevel := hdr.Level - 1
	for i := uint16(0); i < hdr.NumKeys; i++ {
		entry := ReadBtreeIdxEntry(buf, int(i))
		if err := walkBtreeNode(file, entry.Child, opts, v, visited, depth+1, childLevel, false); err != nil {
			return err
		}
	}
	trailing := BtreeTrailingChild(buf)
	return walkBtreeNode(file, trailing, opts, v, visited, depth+1, childLevel, false)
}