// Package fuse implements a FUSE filesystem for BrieFS.
package fuse

import (
	"context"
	"fmt"
	"syscall"

	"github.com/ctdk/briefs-utils/types"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// BrieFS is the top-level filesystem struct.
type BrieFS struct {
	dev        *BlockDevice
	sb         *types.SuperblockLayout
	inodes     *InodeManager
	dataAlloc  *Allocator
	inodeAlloc *Allocator
	blockSize  uint64

	// For converting data-relative blocks to absolute block numbers.
	dataRegionStart uint64
}

// MountOptions configures the FUSE mount.
type MountOptions struct {
	MountPoint string
	Debug      bool
}

// Mount mounts a BrieFS image at the given mount point.
func Mount(imagePath string, opts MountOptions) error {
	dev, blockSize, err := OpenBlockDevice(imagePath)
	if err != nil {
		return fmt.Errorf("open device: %w", err)
	}

	// Read superblock
	sb, err := readSuperblock(dev)
	if err != nil {
		dev.Close()
		return fmt.Errorf("read superblock: %w", err)
	}

	// Open data block allocator
	dataAlloc, err := OpenAllocator(dev, sb.TrieNodePoolStart)
	if err != nil {
		dev.Close()
		return fmt.Errorf("open data allocator: %w", err)
	}

	// Open inode allocator
	inodeAlloc, err := OpenAllocator(dev, sb.InodeBMOffset)
	if err != nil {
		dev.Close()
		return fmt.Errorf("open inode allocator: %w", err)
	}

	dataRegionStart := sb.TrieNodePoolStart + sb.TrieNodePoolSize

	bfs := &BrieFS{
		dev:             dev,
		sb:              sb,
		inodes:          NewInodeManager(dev, sb),
		dataAlloc:       dataAlloc,
		inodeAlloc:      inodeAlloc,
		blockSize:       blockSize,
		dataRegionStart: dataRegionStart,
	}

	root := &brieFSNode{
		bfs: bfs,
		ino: sb.RootIno,
	}

	server, err := fs.Mount(opts.MountPoint, root, &fs.Options{
		MountOptions: fuse.MountOptions{
			Name:  "briefs",
			Debug: opts.Debug,
		},
	})
	if err != nil {
		dev.Close()
		return fmt.Errorf("mount: %w", err)
	}

	server.Wait()
	return nil
}

// readSuperblock reads and parses the superblock from block 0.
// readSuperblock reads and parses the superblock from block 0.
func readSuperblock(dev *BlockDevice) (*types.SuperblockLayout, error) {
	return types.ReadSuperblock(dev, dev.BlockSize())
}

// brieFSNode implements the FUSE inode operations for BrieFS.
type brieFSNode struct {
	fs.Inode
	bfs *BrieFS
	ino uint64
}

// Ensure brieFSNode implements the required interfaces.
var _ = (fs.NodeGetattrer)((*brieFSNode)(nil))
var _ = (fs.NodeLookuper)((*brieFSNode)(nil))
var _ = (fs.NodeReaddirer)((*brieFSNode)(nil))
var _ = (fs.NodeOpener)((*brieFSNode)(nil))
var _ = (fs.NodeReader)((*brieFSNode)(nil))
var _ = (fs.NodeStatfser)((*brieFSNode)(nil))

// readExtent reads the i-th extent from an inode, walking chain blocks
// if the extent is beyond the 8 inline slots.
func (n *brieFSNode) readExtent(diskInode *types.Inode, idx int) (types.Extent, error) {
	if idx < 8 {
		return diskInode.InlineExtents[idx], nil
	}

	chainIdx := idx - 8
	chainBlock := diskInode.ExtentInlineBase
	for chainBlock != 0 {
		buf, err := n.bfs.dev.ReadBlock(chainBlock)
		if err != nil {
			return types.Extent{}, fmt.Errorf("read chain block %d: %w", chainBlock, err)
		}
		if err := types.VerifyChainChecksum(buf, n.bfs.blockSize); err != nil {
			return types.Extent{}, fmt.Errorf("chain block %d: checksum mismatch", chainBlock)
		}
		hdr := types.UnmarshalExtentChainHeader(buf)
		if chainIdx < int(hdr.NumExtentsInBlock) {
			return types.ReadChainExtent(buf, chainIdx), nil
		}
		chainIdx -= int(hdr.NumExtentsInBlock)
		chainBlock = hdr.NextOverflowBlock
	}
	return types.Extent{}, fmt.Errorf("extent index %d out of range (total=%d)", idx, diskInode.NumExtentsTotal)
}

func (n *brieFSNode) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	diskInode, err := n.bfs.inodes.ReadInode(n.ino)
	if err != nil {
		return syscall.EIO
	}

	out.Mode = diskInode.Filemode
	out.Size = diskInode.FileSize
	out.Uid = diskInode.Uid
	out.Gid = diskInode.Gid
	out.Nlink = diskInode.Nlinks
	out.Atime = diskInode.AtimeSec
	out.Atimensec = uint32(diskInode.AtimeNsec)
	out.Mtime = diskInode.MtimeSec
	out.Mtimensec = uint32(diskInode.MtimeNsec)
	out.Ctime = diskInode.CtimeSec
	out.Ctimensec = uint32(diskInode.CtimeNsec)

	totalBlocks := uint64(0)
	for i := 0; i < int(diskInode.NumExtentsTotal); i++ {
		ext, err := n.readExtent(diskInode, i)
		if err != nil {
			break
		}
		totalBlocks += ext.Len
	}
	out.Blocks = totalBlocks * (n.bfs.blockSize / 512)
	out.Blksize = uint32(n.bfs.blockSize)

	return 0
}

func (n *brieFSNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	diskInode, err := n.bfs.inodes.ReadInode(n.ino)
	if err != nil {
		return nil, syscall.EIO
	}

	ino, ftype, err := TrieLookup(n.bfs.dev, diskInode.DirTrieRoot, name)
	if err != nil {
		return nil, syscall.ENOENT
	}

	childInode, err := n.bfs.inodes.ReadInode(ino)
	if err != nil {
		return nil, syscall.EIO
	}

	out.Mode = childInode.Filemode
	out.Size = childInode.FileSize
	out.Uid = childInode.Uid
	out.Gid = childInode.Gid
	out.Nlink = childInode.Nlinks
	out.Atime = childInode.AtimeSec
	out.Atimensec = uint32(childInode.AtimeNsec)
	out.Mtime = childInode.MtimeSec
	out.Mtimensec = uint32(childInode.MtimeNsec)
	out.Ctime = childInode.CtimeSec
	out.Ctimensec = uint32(childInode.CtimeNsec)

	totalBlocks := uint64(0)
	for i := 0; i < int(childInode.NumExtentsTotal); i++ {
		ext, err := n.readExtent(childInode, i)
		if err != nil {
			break
		}
		totalBlocks += ext.Len
	}
	out.Blocks = totalBlocks * (n.bfs.blockSize / 512)
	out.Blksize = uint32(n.bfs.blockSize)

	var childMode uint32
	switch ftype {
	case trieNodeTypeDir:
		childMode = uint32(types.ModeDir)
	default:
		childMode = childInode.Filemode
	}

	childNode := &brieFSNode{
		bfs: n.bfs,
		ino: ino,
	}
	child := n.NewInode(ctx, childNode, fs.StableAttr{
		Ino:  ino,
		Mode: childMode,
	})

	return child, 0
}

func (n *brieFSNode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	diskInode, err := n.bfs.inodes.ReadInode(n.ino)
	if err != nil {
		return nil, syscall.EIO
	}

	if diskInode.DirTrieRoot == 0 {
		return fs.NewListDirStream(nil), 0
	}

	iter := NewTrieIterator(n.bfs.dev, diskInode.DirTrieRoot)
	var entries []fuse.DirEntry

	for {
		ino, ftype, name, err := iter.Next()
		if err != nil {
			break
		}
		if ino == 0 {
			break
		}
		var mode uint32
		switch ftype {
		case trieNodeTypeDir:
			mode = uint32(types.ModeDir)
		default:
			mode = uint32(types.ModeFile)
		}
		entries = append(entries, fuse.DirEntry{
			Mode: mode,
			Ino:  ino,
			Name: name,
		})
	}

	return fs.NewListDirStream(entries), 0
}

func (n *brieFSNode) Open(ctx context.Context, flags uint32) (fh fs.FileHandle, fuseFlags uint32, errno syscall.Errno) {
	// Read-only filesystem — reject anything that requests write access
	if acc := flags & 3; acc == 1 || acc == 2 || acc == 3 {
		return nil, 0, syscall.EROFS
	}
	return nil, fuse.FOPEN_KEEP_CACHE, 0
}

func (n *brieFSNode) Read(ctx context.Context, f fs.FileHandle, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	diskInode, err := n.bfs.inodes.ReadInode(n.ino)
	if err != nil {
		return nil, syscall.EIO
	}

	if off >= int64(diskInode.FileSize) {
		return fuse.ReadResultData(nil), 0
	}

	blockSize := n.bfs.blockSize
	blkSize := int64(blockSize)
	endOff := off + int64(len(dest))
	if endOff > int64(diskInode.FileSize) {
		endOff = int64(diskInode.FileSize)
	}
	toRead := endOff - off

	readBuf := make([]byte, toRead)
	readPos := int64(0)

	// Inline data is stored directly inside the inode.
	if diskInode.Flags&types.InodeFlagInlineData != 0 {
		start := off
		if start < 0 {
			start = 0
		}
		end := endOff
		if end > int64(diskInode.FileSize) {
			end = int64(diskInode.FileSize)
		}
		nc := copy(readBuf, diskInode.InlineData[start:end])
		return fuse.ReadResultData(readBuf[:nc]), 0
	}

	// Walk all extents (inline + chain)
	for i := 0; i < int(diskInode.NumExtentsTotal); i++ {
		ext, err := n.readExtent(diskInode, i)
		if err != nil {
			return nil, syscall.EIO
		}

		extStart := int64(ext.Offset) * blkSize
		extEnd := extStart + int64(ext.Len)*blkSize

		if off >= extEnd || endOff <= extStart {
			continue
		}

		// Clamp to overlapping region
		readStart := off
		if readStart < extStart {
			readStart = extStart
		}
		readEnd := endOff
		if readEnd > extEnd {
			readEnd = extEnd
		}

		// Read blocks
		for blkOff := readStart; blkOff < readEnd; blkOff += blkSize {
			absBlock := ext.Phys + uint64((blkOff - extStart)/blkSize)
			buf, err := n.bfs.dev.ReadBlock(absBlock)
			if err != nil {
				return nil, syscall.EIO
			}

			blkEnd := blkOff + blkSize
			copyStart := readStart
			if copyStart < blkOff {
				copyStart = blkOff
			}
			copyEnd := readEnd
			if copyEnd > blkEnd {
				copyEnd = blkEnd
			}

			nc := copy(readBuf[readPos:], buf[copyStart-blkOff:copyEnd-blkOff])
			readPos += int64(nc)
		}
	}

	return fuse.ReadResultData(readBuf[:readPos]), 0
}

func (n *brieFSNode) Statfs(ctx context.Context, out *fuse.StatfsOut) syscall.Errno {
	out.Blocks = n.bfs.sb.DataBlocks
	out.Bfree = n.bfs.dataAlloc.FreeCount()
	out.Bavail = out.Bfree
	out.Files = n.bfs.inodeAlloc.TotalBlocks()
	out.Ffree = n.bfs.inodeAlloc.FreeCount()
	out.Bsize = uint32(n.bfs.blockSize)
	out.NameLen = uint32(types.BrieFSMaxNameLen)
	return 0
}