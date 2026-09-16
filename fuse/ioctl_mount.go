// Package fuse: mount-level ioctls (FITRIM, FS_IOC_{GET,SET}FSLABEL,
// XFS_IOC_GOINGDOWN shutdown).
//
// Ports the corresponding cases of the kernel's briefs_ioctl (file.c:228-323).
// These are superblock-scoped operations issued on any fd on the mount (fstrim
// opens the mountpoint directory), so the FUSE Ioctl entry routes them here
// before the per-inode FS_IOC_* (chattr) dispatch in fileattr.go.
//
// Assessed but deliberately not forwarded:
//   - O_TMPFILE: the 6.12 FUSE client has no support (no path in fs/fuse that
//     translates it into a FUSE request), so it cannot reach the daemon.
//   - FS_IOC_FIEMAP: never reaches the daemon either — do_vfs_ioctl
//     intercepts the command and routes it to inode->i_op->fiemap
//     (fs/ioctl.c ioctl_fiemap), which fs/fuse does not implement in 6.12,
//     so the caller gets EOPNOTSUPP with no FUSE round trip (verified live on
//     the 6.12.101 test kernel). Even on a client that forwarded it,
//     restricted ioctl mode caps the transfer at _IOC_SIZE(cmd) = 32 bytes,
//     the struct fiemap header alone; the fm_extents[] flex array cannot
//     cross the wire. Same blocker class as O_TMPFILE.
//   - BRIEFS_IOC_{EXCHANGE_RANGE,START_COMMIT,COMMIT_RANGE,SWAPEXT}: the kernel
//     core (briefs_do_exchange, file.c:2450-2615) is a large two-inode port
//     (freshness structs, promote, DRY_RUN, TO_EOF, exch_engine_same/diff,
//     two-inode journaling under a lock-order discipline the bridge's sharded
//     inode locks would have to re-derive). Deferred until something needs it.

package fuse

import (
	"context"
	"encoding/binary"
	"errors"
	"syscall"

	"github.com/ctdk/briefs-utils/briefs"
)

const (
	// FSLABEL_MAX (include/uapi/linux/fs.h): the ioctl interface size.
	fslabelMax = 256

	// struct fstrim_range (uapi/linux/fsmap.h): three __u64s.
	sizeFsTrimRange = 24
)

var (
	fiTrim          = ioc(iocRead|iocWrite, uint32('X'), 121, sizeFsTrimRange) // _IOWR('X', 121, struct fstrim_range)
	fsIocGetfslabel = ioc(iocRead, 0x94, 49, fslabelMax)                       // _IOR(0x94, 49, char[FSLABEL_MAX])
	fsIocSetfslabel = ioc(iocWrite, 0x94, 50, fslabelMax)                      // _IOW(0x94, 50, char[FSLABEL_MAX])
	goDown          = ioc(iocRead, uint32('X'), 125, 4)                        // _IOR('X', 125, __u32) == XFS_IOC_GOINGDOWN == F2FS_IOC_SHUTDOWN == BRIEFS_IOC_GOINGDOWN
)

// XFS_IOC_GOINGDOWN flag values (briefs.h:1145-1151; the same bits as
// xfs_fs.h and f2fs.h).
const (
	shutdownFlagDefault    = 0x0
	shutdownFlagLogFlush   = 0x1
	shutdownFlagNoLogFlush = 0x2
)

// ioctlMount dispatches the mount-level ioctls. arg is the caller's ioctl
// payload address (IoctlIn.Arg): _IOR commands like GOINGDOWN arrive with an
// empty input buffer in restricted FUSE ioctl mode, so their payload must be
// read from the caller's memory there. The third return value is false when
// cmd is not one of them, so the caller falls through to the per-inode
// FS_IOC_* handling (ioctlFileattr).
func (b *BrieFS) ioctlMount(ctx context.Context, cmd uint32, arg uint64, input, output []byte) (int32, syscall.Errno, bool) {
	switch cmd {
	case fiTrim:
		if !callerCapSysAdmin(ctx) {
			return 0, syscall.EPERM, true
		}
		r, ok := decodeFsTrimRange(input)
		if !ok {
			return 0, syscall.EINVAL, true
		}
		trimmed, err := b.fstrimOp(r)
		if err != nil {
			return 0, errToErrno(err), true
		}
		r.length = trimmed
		encodeFsTrimRange(output, r)
		return 0, 0, true
	case fsIocGetfslabel:
		b.fslabelGetOp(output)
		return 0, 0, true
	case fsIocSetfslabel:
		if !callerCapSysAdmin(ctx) {
			return 0, syscall.EPERM, true
		}
		return 0, errToErrno(b.fslabelSetOp(input)), true
	case goDown:
		if !callerCapSysAdmin(ctx) {
			return 0, syscall.EPERM, true
		}
		flags, ok := callerU32At(ctx, arg)
		if !ok {
			// The kernel's get_user cannot fail here (it runs in the
			// caller's context); the daemon's /proc read can, for a
			// non-root caller under ptrace restrictions. Degrade to
			// DEFAULT rather than EFAULT so desktop mounts stay
			// shut-down-able.
			flags = shutdownFlagDefault
		}
		return 0, errToErrno(b.shutdownOp(flags)), true
	}
	return 0, syscall.ENOTTY, false
}

// --- XFS_IOC_GOINGDOWN ---

// shutdownOp implements forced shutdown (kernel briefs_shutdown,
// inode.c:159, and the freeze-side effects of briefs_force_shutdown):
// freeze the filesystem — mutations refused EROFS, file reads and fsync
// EIO (file.c:105-117, :556-566), the unmount checkpoint skipped so the
// live journal region survives for the next mount's replay.
//
// Idempotent, and checked before flag validation (kernel order): a second
// call returns success whatever flags it carries. An already-frozen bridge
// (readOnly) may still be shut down — the kernel proceeds past the
// read-only mount's -EROFS (file.c:242-250, generic/599).
//
// LOGFLUSH flushes the journal without retiring the ring — Sync(false), the
// analog of briefs_journal_sync_no_checkpoint — plus a device cache flush,
// so committed-but-uncheckpointed records are durable and log_start <
// log_end survives for the next mount's replay (generic/052). DEFAULT and
// NOLOGFLUSH leave the journal as-is: the DEFAULT case's bdev
// freeze/thaw has no FUSE analog, making it NOLOGFLUSH here. Anything else
// is EINVAL.
func (b *BrieFS) shutdownOp(flags uint32) error {
	if b.shutdown {
		return nil
	}
	switch flags {
	case shutdownFlagDefault, shutdownFlagNoLogFlush:
	case shutdownFlagLogFlush:
		if b.journal != nil {
			if err := b.journal.Sync(false); err != nil {
				return err
			}
		}
		if err := b.dev.Sync(); err != nil {
			return err
		}
	default:
		return syscall.EINVAL
	}
	b.shutdown = true
	return nil
}

// --- FITRIM ---

// fsTrimRange mirrors struct fstrim_range: start/len/minlen as byte offsets
// into the data region; on the way out len carries the trimmed byte count.
type fsTrimRange struct {
	start, length, minlen uint64
}

func decodeFsTrimRange(in []byte) (fsTrimRange, bool) {
	if len(in) < sizeFsTrimRange {
		return fsTrimRange{}, false
	}
	return fsTrimRange{
		start:  binary.LittleEndian.Uint64(in[0:]),
		length: binary.LittleEndian.Uint64(in[8:]),
		minlen: binary.LittleEndian.Uint64(in[16:]),
	}, true
}

func encodeFsTrimRange(out []byte, r fsTrimRange) {
	if len(out) < sizeFsTrimRange {
		return
	}
	binary.LittleEndian.PutUint64(out[0:], r.start)
	binary.LittleEndian.PutUint64(out[8:], r.length)
	binary.LittleEndian.PutUint64(out[16:], r.minlen)
}

// fstrimOp implements FITRIM over the data-block allocator: a read-only walk
// of the L2 bitmap for maximal free runs (Allocator.TrimFreeRuns), with each
// run — or only its slice inside the requested [start, end) range — discarded
// when at least minlen blocks long, mirroring the kernel's briefs_trim_fs
// (alloc.c:512) and briefs_trim_flush (alloc.c:474). The discard analogue is
// a PUNCH_HOLE fallocate on the backing image: BrieFS free data blocks hold
// only stale garbage, and the metadata regions precede the data region, so
// punching never touches live data. Returns the trimmed byte count.
func (b *BrieFS) fstrimOp(r fsTrimRange) (uint64, error) {
	if b.frozen() {
		// The kernel FITRIM needs a writer ref on the mount: EROFS once
		// the freeze has set SB_RDONLY (file.c:258).
		return 0, syscall.EROFS
	}
	bs := b.blockSize
	maxBlks := b.dataAlloc.TotalBlocks()
	start := r.start / bs
	end := start + r.length/bs
	minlen := r.minlen / bs

	// Validation mirrors the kernel (ext4_trim_fs rules, alloc.c:525).
	if r.length < bs || start >= maxBlks || minlen > maxBlks {
		return 0, syscall.EINVAL
	}
	if end > maxBlks {
		end = maxBlks
	}
	if minlen == 0 {
		minlen = 1
	}
	if end <= start {
		return 0, nil
	}

	fd := int(b.dev.File().Fd())
	trimmed := uint64(0)
	err := b.dataAlloc.TrimFreeRuns(func(runStart, runLen uint64) error {
		if runLen < minlen {
			return nil
		}
		ds := max(runStart, start)
		de := min(runStart+runLen, end)
		if de <= ds {
			return nil
		}
		n := de - ds
		if n < minlen {
			return nil
		}
		off := int64((b.dataRegionStart + ds) * bs)
		if err := syscall.Fallocate(fd, fallocPunchHole|fallocKeepSize,
			off, int64(n*bs)); err != nil {
			// The kernel refuses up front when the backing device cannot
			// discard (!bdev_max_discard_sectors, file.c:260); the bridge
			// learns it from the first punch instead.
			if errors.Is(err, syscall.EOPNOTSUPP) || errors.Is(err, syscall.ENOSYS) ||
				errors.Is(err, syscall.ENOTTY) {
				return syscall.EOPNOTSUPP
			}
			return err
		}
		trimmed += n * bs
		return nil
	})
	return trimmed, err
}

// --- FS_IOC_{GET,SET}FSLABEL ---

// fslabelGetOp copies the 64-byte, null-padded on-disk label into the
// FSLABEL_MAX-sized output buffer (zero-filled, so userspace sees a
// terminated string; kernel file.c:282-295).
func (b *BrieFS) fslabelGetOp(out []byte) {
	var label [fslabelMax]byte
	if b.journal != nil {
		vol := b.journal.Label()
		copy(label[:], vol[:])
	} else {
		copy(label[:], b.sb.Label[:])
	}
	copy(out, label[:min(len(out), fslabelMax)])
}

// fslabelSetOp stores a new label (kernel file.c:297-323): CAP_SYS_ADMIN (the
// dispatch layer), at most 64 chars (the fixed 64-byte null-padded field;
// longer is EINVAL), null-padded, block 0 persisted through the journal's
// superblock path.
func (b *BrieFS) fslabelSetOp(input []byte) error {
	if b.frozen() {
		return syscall.EROFS
	}
	if len(input) == 0 {
		return syscall.EINVAL
	}
	label := input
	if len(label) > fslabelMax {
		label = label[:fslabelMax]
	}
	// strnlen: the label runs to the first NUL or the end of the buffer.
	ln := len(label)
	for i, c := range label {
		if c == 0 {
			ln = i
			break
		}
	}
	if ln > briefs.BrieFSVolLabelLen {
		return syscall.EINVAL
	}
	var stored [briefs.BrieFSVolLabelLen]byte
	copy(stored[:], label[:ln])

	if b.journal != nil {
		return b.journal.UpdateLabel(stored)
	}
	// No journal (read-only bridge): the in-memory superblock is the only
	// writer, so a direct whole-block persist is race-free.
	b.sb.Label = stored
	buf := make([]byte, b.blockSize)
	data, err := b.sb.MarshalBinary()
	if err != nil {
		return err
	}
	copy(buf, data)
	if err := b.dev.WriteBlock(0, buf); err != nil {
		return err
	}
	return b.dev.FlushPendingWB()
}
