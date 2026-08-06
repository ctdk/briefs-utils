// Package fuse: fileattr / chattr (FS_IOC flags).
//
// Ports the kernel's briefs_fileattr_get/set (file.c:222) over inode.UserFlags,
// exposed through the FS_IOC_GETFLAGS / FS_IOC_SETFLAGS (chattr/lsattr) and
// FS_IOC_FSGETXATTR / FS_IOC_FSSETXATTR (xfs_io/statx) ioctls. go-fuse passes
// ioctls through NodeIoctler; FUSE bypasses the VFS fileattr framework, so the
// bridge decodes the ioctl payloads itself.
//
// disk_inode.user_flags stores the FS_*_FL values verbatim (matching the kernel
// UAPI), so chattr/lsattr see them without translation. The FS_XFLAG_* values
// in struct fsxattr differ from FS_*_FL, so the FSGETXATTR/FSSETXATTR path
// translates (briefs_user_flags_to_xflags / briefs_xflags_to_user_flags,
// briefs.h:383). DIRSYNC is only valid on directories and has no XFLAG.

package fuse

import (
	"encoding/binary"
	"syscall"

	"github.com/ctdk/briefs-utils/briefs"
)

// FS_*_FL user flags (include/uapi/linux/fs.h), stored verbatim in UserFlags.
const (
	fsSyncFl      uint32 = 0x00000008
	fsImmutableFl uint32 = 0x00000010
	fsAppendFl    uint32 = 0x00000020
	fsNodumpFl    uint32 = 0x00000040
	fsNoatimeFl   uint32 = 0x00000080
	fsDirsyncFl   uint32 = 0x00010000
	fsCommonFl            = fsSyncFl | fsImmutableFl | fsAppendFl | fsNodumpFl | fsNoatimeFl | fsDirsyncFl
)

// FS_XFLAG_* (struct fsxattr.fsx_xflags). Note these differ from FS_*_FL.
const (
	fsXflagImmutable uint32 = 0x00000008
	fsXflagAppend    uint32 = 0x00000010
	fsXflagSync      uint32 = 0x00000020
	fsXflagNoatime   uint32 = 0x00000040
	fsXflagNodump    uint32 = 0x00000080
	fsXflagSupported         = fsXflagSync | fsXflagImmutable | fsXflagAppend | fsXflagNodump | fsXflagNoatime
)

// Linux _IOC encoding for the FS_IOC_* commands (sizeof(long)=8,
// sizeof(struct fsxattr)=28 on amd64).
const (
	iocNone  uint32 = 0
	iocWrite uint32 = 1
	iocRead  uint32 = 2
	sizeLong        = 8
	sizeFsxattr     = 28
)

func ioc(dir, typ, nr, size uint32) uint32 {
	return (dir << 30) | (size << 16) | (typ << 8) | nr
}

var (
	fsIocGetflags   = ioc(iocRead, uint32('f'), 1, sizeLong)
	fsIocSetflags   = ioc(iocWrite, uint32('f'), 2, sizeLong)
	fsIocFsgetxattr = ioc(iocRead, uint32('X'), 31, sizeFsxattr)
	fsIocFssetxattr = ioc(iocWrite, uint32('X'), 32, sizeFsxattr)
)

func userFlagsToXflags(uf uint32) uint32 {
	var x uint32
	if uf&fsSyncFl != 0 {
		x |= fsXflagSync
	}
	if uf&fsImmutableFl != 0 {
		x |= fsXflagImmutable
	}
	if uf&fsAppendFl != 0 {
		x |= fsXflagAppend
	}
	if uf&fsNodumpFl != 0 {
		x |= fsXflagNodump
	}
	if uf&fsNoatimeFl != 0 {
		x |= fsXflagNoatime
	}
	return x
}

func xflagsToUserFlags(xf uint32) uint32 {
	var u uint32
	if xf&fsXflagSync != 0 {
		u |= fsSyncFl
	}
	if xf&fsXflagImmutable != 0 {
		u |= fsImmutableFl
	}
	if xf&fsXflagAppend != 0 {
		u |= fsAppendFl
	}
	if xf&fsXflagNodump != 0 {
		u |= fsNodumpFl
	}
	if xf&fsXflagNoatime != 0 {
		u |= fsNoatimeFl
	}
	return u
}

// fileattrGet returns the user flags (FS_*_FL), the xflags (FS_XFLAG_*), and the
// extent count for an inode. DIRSYNC is cleared for non-directories.
func (b *BrieFS) fileattrGet(ino uint64) (flags, xflags uint32, nextents uint64, err error) {
	in, err := b.inodes.ReadInode(ino)
	if err != nil {
		return 0, 0, 0, err
	}
	f := in.UserFlags & fsCommonFl
	if !in.IsDir() {
		f &^= fsDirsyncFl
	}
	return f, userFlagsToXflags(f), in.NumExtentsTotal, nil
}

// fileattrSet updates the user flags. With setFlags the argument is FS_*_FL
// (SETFLAGS); with setXflags it is FS_XFLAG_* plus extsize/projid/cowextsize
// (FSSETXATTR). Mirrors briefs_fileattr_set (file.c:243). Locking: the
// inode-block shard lock, like a file write.
func (b *BrieFS) fileattrSet(ino uint64, setFlags bool, flags uint32, setXflags bool, xflags, extsize, projid, cowextsize uint32) error {
	if b.readOnly {
		return syscall.EROFS
	}
	lock := b.inodeBlockLock(ino)
	lock.Lock()
	defer lock.Unlock()

	in, err := b.inodes.ReadInode(ino)
	if err != nil {
		return err
	}
	old := in.UserFlags
	var newFlags uint32
	switch {
	case setFlags:
		if flags&^fsCommonFl != 0 {
			return syscall.EOPNOTSUPP
		}
		newFlags = (old &^ fsCommonFl) | (flags & fsCommonFl)
	case setXflags:
		if xflags&^fsXflagSupported != 0 {
			return syscall.EOPNOTSUPP
		}
		if extsize != 0 || projid != 0 || cowextsize != 0 {
			return syscall.EOPNOTSUPP
		}
		newFlags = (old &^ fsCommonFl) | xflagsToUserFlags(xflags)
	default:
		return syscall.EOPNOTSUPP
	}

	// DIRSYNC only valid on directories.
	if newFlags&fsDirsyncFl != 0 && !in.IsDir() {
		return syscall.EINVAL
	}
	if old == newFlags {
		return nil
	}

	// Setting immutable on a regular file: the bridge has no page cache and
	// each write is already on disk, so there is no pending data to flush (the
	// kernel's filemap_write_and_wait here is a no-op equivalent).
	in.UserFlags = newFlags
	sec, nsec := nowTime()
	in.CtimeSec, in.CtimeNsec = sec, nsec

	// Commit the snapshot (user_flags is carried by JRN_INODE_FULL), then write
	// the inode block. The inode block is snapshot-trusted, so commit-before-
	// flush is safe (replay restores it).
	if err := b.journalInodeFull(in); err != nil {
		b.failWrite()
		return err
	}
	if err := b.journal.Sync(false); err != nil {
		b.failWrite()
		return err
	}
	if err := b.writeInodeDirect(in); err != nil {
		b.failWrite()
		return err
	}
	if err := b.dev.Fdatasync(); err != nil {
		b.failWrite()
		return err
	}
	return nil
}

// --- ioctl payload (de)serialization ---

// encodeGetFlags writes the FS_*_FL flags into the GETFLAGS output buffer (a
// long: 8 bytes on amd64, 4 on 32-bit).
func encodeGetFlags(out []byte, flags uint32) {
	if len(out) >= 8 {
		binary.LittleEndian.PutUint64(out, uint64(flags))
	} else if len(out) >= 4 {
		binary.LittleEndian.PutUint32(out, flags)
	}
}

// decodeSetFlags reads the FS_*_FL flags from the SETFLAGS input buffer.
func decodeSetFlags(in []byte) (uint32, bool) {
	if len(in) >= 4 {
		return binary.LittleEndian.Uint32(in), true
	}
	return 0, false
}

// encodeFsxattr writes struct fsxattr {xflags, extsize, nextents, projid,
// cowextsize, pad[8]} (28 bytes) into out.
func encodeFsxattr(out []byte, xflags uint32, nextents uint64) {
	if len(out) < 20 {
		return
	}
	binary.LittleEndian.PutUint32(out[0:], xflags)
	binary.LittleEndian.PutUint32(out[4:], 0) // extsize
	binary.LittleEndian.PutUint32(out[8:], uint32(nextents))
	binary.LittleEndian.PutUint32(out[12:], 0) // projid
	binary.LittleEndian.PutUint32(out[16:], 0) // cowextsize
	for i := 20; i < len(out) && i < 28; i++ {
		out[i] = 0
	}
}

// decodeFsxattr reads struct fsxattr from in (the settable fields; nextents is
// ignored on FSSETXATTR).
func decodeFsxattr(in []byte) (xflags, extsize, projid, cowextsize uint32, ok bool) {
	if len(in) < 20 {
		return 0, 0, 0, 0, false
	}
	xflags = binary.LittleEndian.Uint32(in[0:])
	extsize = binary.LittleEndian.Uint32(in[4:])
	projid = binary.LittleEndian.Uint32(in[12:])
	cowextsize = binary.LittleEndian.Uint32(in[16:])
	return xflags, extsize, projid, cowextsize, true
}

// ioctlFileattr dispatches the FS_IOC_* ioctls. Returns ENOTTY for unknown.
func (b *BrieFS) ioctlFileattr(ino uint64, cmd uint32, input, output []byte) (int32, syscall.Errno) {
	switch cmd {
	case fsIocGetflags:
		flags, _, _, err := b.fileattrGet(ino)
		if err != nil {
			return 0, errToErrno(err)
		}
		encodeGetFlags(output, flags)
		return 0, 0
	case fsIocSetflags:
		flags, ok := decodeSetFlags(input)
		if !ok {
			return 0, syscall.EINVAL
		}
		return 0, errToErrno(b.fileattrSet(ino, true, flags, false, 0, 0, 0, 0))
	case fsIocFsgetxattr:
		flags, xflags, nextents, err := b.fileattrGet(ino)
		if err != nil {
			return 0, errToErrno(err)
		}
		_ = flags
		encodeFsxattr(output, xflags, nextents)
		return 0, 0
	case fsIocFssetxattr:
		xf, extsize, projid, cowextsize, ok := decodeFsxattr(input)
		if !ok {
			return 0, syscall.EINVAL
		}
		return 0, errToErrno(b.fileattrSet(ino, false, 0, true, xf, extsize, projid, cowextsize))
	}
	return 0, syscall.ENOTTY
}

// userFlagsImmutable reports whether @in is immutable (writes/truncate forbidden).
func userFlagsImmutable(in *briefs.Inode) bool {
	return in.UserFlags&fsImmutableFl != 0
}

// userFlagsAppendOnly reports whether @in is append-only (only writes at EOF).
func userFlagsAppendOnly(in *briefs.Inode) bool {
	return in.UserFlags&fsAppendFl != 0
}