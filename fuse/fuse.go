// Package fuse implements a FUSE filesystem for BrieFS.
package fuse

import (
	"context"
	"fmt"
	"sync"
	"syscall"

	"github.com/ctdk/briefs-utils/briefs"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// BrieFS is the top-level filesystem struct.
type BrieFS struct {
	dev        *BlockDevice
	sb         *briefs.SuperblockLayout
	inodes     *InodeManager
	dataAlloc  *Allocator
	inodeAlloc *Allocator
	blockSize  uint64

	// For converting data-relative blocks to absolute block numbers.
	dataRegionStart uint64

	// journal is the crash-consistency engine (ported from the kernel
	// journal.c write path). nil while the read-only bridge is in use; a
	// Phase-1 Journal is wired in Mount once the writer is implemented.
	journal *briefs.Journal

	// mu serializes all mutating FUSE handlers. Phase 0 uses a single global
	// lock for correctness; Phase 6 replaces it with per-inode locks following
	// the kernel lock order (briefs.h:40-71). Reads stay lockless — the bridge
	// re-reads from disk on every call and caches nothing — until Phase 6.
	mu sync.Mutex

	// triePartials is the per-superblock pool of trie pages that still have a
	// free slot, mirroring the kernel's bsi->trie_pages.partial list
	// (trie_page.c). It is consulted by trieAllocNode to reuse slots before
	// allocating fresh pages. Protected by mu.
	triePartials []uint64

	// cache is the per-operation block cache (see cache.go).  A mutating
	// handler calls cacheBegin before its first metadata read and flushCache
	// before journal.Sync so all of the operation's metadata lands on disk
	// together.  nil between operations.  Protected by mu.
	cache     map[uint64][]byte
	cacheDirty map[uint64]bool
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

	// Construct the journal (ported from kernel journal.c).  It mutates sb in
	// place and writes through dev.  The allocator interface lets it sync the
	// bitmaps / refresh free counts at checkpoint without an import cycle.
	journal, err := briefs.NewJournal(sb, dev.File(), blockSize)
	if err != nil {
		dev.Close()
		return fmt.Errorf("init journal: %w", err)
	}
	journal.SetAllocatorSyncer(bfs)
	bfs.journal = journal

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

	// Unmount: checkpoint (leaves log_start==log_end, nothing to replay on
	// next mount) then close.  Phase 11 hardens this into a proper
	// always-checkpoint-at-unmount with full shutdown handling.
	if bfs.journal != nil {
		_ = bfs.journal.Checkpoint()
		_ = bfs.journal.Close()
	}
	dev.Close()
	return nil
}

// RefreshFreeCounts updates the in-memory superblock free counts from the
// authoritative allocator free counts.  Implements briefs.AllocatorSyncer;
// called by the journal before persisting the superblock.  Cheap; no I/O.
func (b *BrieFS) RefreshFreeCounts() {
	b.sb.FreeDataBlks = b.dataAlloc.FreeCount()
	b.sb.FreeInodes = b.inodeAlloc.FreeCount()
}

// SyncAllocators writes both allocator bitmap pools to disk.  Implements
// briefs.AllocatorSyncer; called by the journal at checkpoint, mirroring the
// kernel's briefs_alloc_sync() pair.  Without this a back-pressure checkpoint
// would advance log_start past allocation records while the on-disk bitmap
// still failed to mark the blocks allocated (generic/040/041 family).
func (b *BrieFS) SyncAllocators() error {
	if err := b.dataAlloc.Sync(); err != nil {
		return fmt.Errorf("sync data allocator: %w", err)
	}
	return b.inodeAlloc.Sync()
}

// readSuperblock reads and parses the superblock from block 0.
// readSuperblock reads and parses the superblock from block 0.
func readSuperblock(dev *BlockDevice) (*briefs.SuperblockLayout, error) {
	return briefs.ReadSuperblock(dev, dev.BlockSize())
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
var _ = (fs.NodeCreater)((*brieFSNode)(nil))
var _ = (fs.NodeMkdirer)((*brieFSNode)(nil))
var _ = (fs.NodeUnlinker)((*brieFSNode)(nil))
var _ = (fs.NodeRmdirer)((*brieFSNode)(nil))

// collectExtents returns every extent of an inode in ascending offset order,
// via briefs.IterateInodeExtents (which dispatches on InodeFlagIndexed: inline
// array for inline-only inodes, B+ tree leaves for tree-backed inodes, and no
// extents for inline-data inodes). Replaces the old chain-block walk; the chain
// format no longer exists on v0.9 images.
func (n *brieFSNode) collectExtents(diskInode *briefs.Inode) ([]briefs.Extent, error) {
	var exts []briefs.Extent
	err := briefs.IterateInodeExtents(n.bfs.dev.File(), diskInode, n.bfs.blockSize,
		briefs.InodeExtentVisitor{
			VisitExtent: func(ext briefs.Extent) error {
				exts = append(exts, ext)
				return nil
			},
		})
	return exts, err
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
	exts, err := n.collectExtents(diskInode)
	if err != nil {
		return syscall.EIO
	}
	for _, ext := range exts {
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
	childExts, err := n.collectExtents(childInode)
	if err != nil {
		return nil, syscall.EIO
	}
	for _, ext := range childExts {
		totalBlocks += ext.Len
	}
	out.Blocks = totalBlocks * (n.bfs.blockSize / 512)
	out.Blksize = uint32(n.bfs.blockSize)

	var childMode uint32
	switch ftype {
	case trieNodeTypeDir:
		childMode = uint32(briefs.ModeDir)
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
		// ftype is S_IFMT >> 12 (4=dir, 8=reg, 10=symlink, etc.)
		// Map to FUSE mode bits
		var mode uint32
		switch ftype {
		case 4: // S_IFDIR
			mode = uint32(briefs.ModeDir)
		case 8: // S_IFREG
			mode = uint32(briefs.ModeFile)
		case 10: // S_IFLNK
			mode = uint32(briefs.ModeSymlink)
		default:
			// Unknown type, default to regular file
			mode = uint32(briefs.ModeFile)
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
	// The bridge is now read-write; file data writes land in Phase 5. For
	// now accept read opens and write opens alike (a write open on a regular
	// file simply has no Write handler yet, so writes will ENOSYS until Phase
	// 5 wires them up).
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
	if diskInode.Flags&briefs.InodeFlagInlineData != 0 {
		start := off
		if start < 0 {
			start = 0
		}
		end := endOff
		if end > int64(diskInode.FileSize) {
			end = int64(diskInode.FileSize)
		}
		inlineData := diskInode.InlineData()
		nc := copy(readBuf, inlineData[start:end])
		return fuse.ReadResultData(readBuf[:nc]), 0
	}

	// Walk all extents (inline array or B+ tree) in ascending offset order.
	exts, err := n.collectExtents(diskInode)
	if err != nil {
		return nil, syscall.EIO
	}
	for _, ext := range exts {
		// Holes have Phys == 0. Return zeros for hole regions.
		if ext.Phys == 0 {
			holeStart := int64(ext.Offset) * blkSize
			holeEnd := holeStart + int64(ext.Len)*blkSize
			if off >= holeEnd || endOff <= holeStart {
				continue
			}
			// Zero the overlapping region
			zeroStart := off
			if zeroStart < holeStart {
				zeroStart = holeStart
			}
			zeroEnd := endOff
			if zeroEnd > holeEnd {
				zeroEnd = holeEnd
			}
			// Zero the corresponding portion of the buffer
			bufPos := zeroStart - off
			bufLen := zeroEnd - zeroStart
			if bufPos >= 0 && bufLen > 0 && bufPos < int64(len(readBuf)) {
				for i := bufPos; i < bufPos+bufLen && i < int64(len(readBuf)); i++ {
					readBuf[i] = 0
				}
			}
			continue
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
	out.NameLen = uint32(briefs.BrieFSMaxNameLen)
	return 0
}

// errToErrno maps a Go error to a FUSE errno.  syscall.Errno values pass through
// unchanged; anything else is treated as an I/O error.
func errToErrno(err error) syscall.Errno {
	if err == nil {
		return 0
	}
	if e, ok := err.(syscall.Errno); ok {
		return e
	}
	return syscall.EIO
}

// callerCreds extracts the invoking process's uid/gid from the FUSE request
// context.  On failure (no caller attached) it defaults to root.
func callerCreds(ctx context.Context) (uid, gid uint32) {
	if c, ok := fuse.FromContext(ctx); ok && c != nil {
		return c.Uid, c.Gid
	}
	return 0, 0
}

// fillEntryOut populates a FUSE EntryOut from an on-disk inode, including the
// block count derived from its extents.
func (n *brieFSNode) fillEntryOut(out *fuse.EntryOut, in *briefs.Inode) {
	out.Mode = in.Filemode
	out.Size = in.FileSize
	out.Uid = in.Uid
	out.Gid = in.Gid
	out.Nlink = in.Nlinks
	out.Atime = in.AtimeSec
	out.Atimensec = uint32(in.AtimeNsec)
	out.Mtime = in.MtimeSec
	out.Mtimensec = uint32(in.MtimeNsec)
	out.Ctime = in.CtimeSec
	out.Ctimensec = uint32(in.CtimeNsec)

	var totalBlocks uint64
	exts, err := n.collectExtents(in)
	if err == nil {
		for _, ext := range exts {
			totalBlocks += ext.Len
		}
	}
	out.Blocks = totalBlocks * (n.bfs.blockSize / 512)
	out.Blksize = uint32(n.bfs.blockSize)
}

// newChildNode wraps a freshly created inode in a go-fuse Inode linked to its
// parent (this node).
func (n *brieFSNode) newChildNode(ctx context.Context, in *briefs.Inode) *fs.Inode {
	child := &brieFSNode{bfs: n.bfs, ino: in.InodeNumber}
	return n.NewInode(ctx, child, fs.StableAttr{
		Ino:  in.InodeNumber,
		Mode: in.Filemode,
	})
}

// Create handles O_CREAT.  Mirrors briefs_create (dir.c:369).
func (n *brieFSNode) Create(ctx context.Context, name string, flags, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	n.bfs.mu.Lock()
	defer n.bfs.mu.Unlock()

	uid, gid := callerCreds(ctx)
	child, err := n.bfs.createInDir(n.ino, name, briefs.ModeFile|mode, uid, gid, flags&syscall.O_EXCL != 0)
	if err != nil {
		return nil, nil, 0, errToErrno(err)
	}
	n.fillEntryOut(out, child)
	return n.newChildNode(ctx, child), nil, fuse.FOPEN_KEEP_CACHE, 0
}

// Mkdir handles directory creation.  Mirrors briefs_mkdir (dir.c:414).
func (n *brieFSNode) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	n.bfs.mu.Lock()
	defer n.bfs.mu.Unlock()

	uid, gid := callerCreds(ctx)
	child, err := n.bfs.createInDir(n.ino, name, briefs.ModeDir|mode, uid, gid, false)
	if err != nil {
		return nil, errToErrno(err)
	}
	n.fillEntryOut(out, child)
	return n.newChildNode(ctx, child), 0
}

// Unlink handles file removal.  Mirrors briefs_unlink (dir.c:680).
func (n *brieFSNode) Unlink(ctx context.Context, name string) syscall.Errno {
	n.bfs.mu.Lock()
	defer n.bfs.mu.Unlock()
	return errToErrno(n.bfs.unlinkInDir(n.ino, name, false))
}

// Rmdir handles directory removal.  Mirrors briefs_rmdir (dir.c:702).
func (n *brieFSNode) Rmdir(ctx context.Context, name string) syscall.Errno {
	n.bfs.mu.Lock()
	defer n.bfs.mu.Unlock()
	return errToErrno(n.bfs.unlinkInDir(n.ino, name, true))
}
