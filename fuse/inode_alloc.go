// Package fuse: inode allocation and freeing.
//
// These mirror the kernel's briefs_new_inode (inode.c:424) and
// briefs_free_inode_num (inode.c:1108): allocate an inode number from the
// inode-allocator bitmap, journal the allocation, initialize and persist the
// on-disk inode; on free, zero the on-disk slot, journal the free, then return
// the number to the bitmap.  The caller (a FUSE handler) is responsible for
// setgid inheritance and uid/gid derivation from the FUSE caller's
// credentials, matching the kernel's inode_init_owner().

package fuse

import (
	"fmt"
	"math/rand/v2"
	"syscall"

	"github.com/ctdk/briefs-utils/briefs"
)

// AllocInode allocates a new inode number, journals JRN_INODE_ALLOC, and
// builds the initialized inode in memory.  It does NOT persist the inode; the
// caller (createInDir) must writeInodeCached it under the child's inode-block
// lock so a concurrent file write to a sibling in the same 4K table block cannot
// clobber the slot.  mode is the full file mode (including S_IFMT); uid/gid are
// the caller's credentials (computed by the handler, including setgid
// inheritance); parentIno is the parent directory inode number (recorded for
// directories).  On journal failure the bitmap bit is returned.
func (b *BrieFS) AllocInode(mode, uid, gid uint32, parentIno uint64) (*briefs.Inode, error) {
	rel := b.inodeAlloc.AllocBlock()
	if rel == 0 {
		return nil, syscall.ENOSPC
	}
	ino := rel + 1 // inode numbers are 1-based; the bitmap index is 0-based.

	isDir := (mode & briefs.ModeTypeMask) == briefs.ModeDir
	nlink := uint32(1)
	if isDir {
		nlink = 2
	}

	// Journal the allocation before persisting, mirroring briefs_new_inode
	// (inode.c:441).  Replay reserves the inode bit from this record.
	allocRec := &briefs.JrnInodeAlloc{Ino: ino, Mode: mode, Nlink: nlink, Uid: uid, Gid: gid}
	if err := b.journal.WriteRecord(briefs.JRN_INODE_ALLOC, allocRec.Marshal()); err != nil {
		b.inodeAlloc.FreeBlock(rel)
		return nil, fmt.Errorf("briefs: journal inode alloc: %w", err)
	}

	inode := briefs.NewInode(ino, mode)
	inode.Uid = uid
	inode.Gid = gid
	inode.Nlinks = nlink
	if isDir {
		inode.ParentInode = parentIno
	}
	// Random 32-bit generation for stable NFS file handles (kernel uses
	// get_random_u32(); stored in the low 32 bits).
	inode.Generation = uint64(rand.Uint32())
	return inode, nil
}

// FreeInode zeroes the on-disk inode slot, journals JRN_INODE_FREE, and
// returns the inode number to the bitmap.  Mirrors briefs_free_inode_num
// (inode.c:1108): zero the slot so fsck does not see a free inode with valid
// magic, then journal the free (so replay releases the bit even if the
// zeroing is lost), then free the bitmap bit.  The caller must already have
// freed the inode's data extents and trie (see Phase 5 / Phase 3).
func (b *BrieFS) FreeInode(ino uint64) error {
	if ino == 0 {
		return nil
	}
	// Zero the on-disk inode slot (read-modify-write the inode block).
	if err := b.zeroInodeCached(ino); err != nil {
		return fmt.Errorf("briefs: zero inode %d: %w", ino, err)
	}
	// Journal the free before recycling the number.
	freeRec := &briefs.JrnInodeFree{Ino: ino}
	if err := b.journal.WriteRecord(briefs.JRN_INODE_FREE, freeRec.Marshal()); err != nil {
		return fmt.Errorf("briefs: journal inode free: %w", err)
	}
	b.inodeAlloc.FreeBlock(ino - 1)
	return nil
}