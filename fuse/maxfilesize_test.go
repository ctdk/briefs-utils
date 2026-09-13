package fuse

// Regression tests for the MAX_LFS_FILESIZE tail family fixed 2026-09-11:
// the write loop's blockEnd = blockStart + blockSize overflows to INT64_MIN
// at the final block, walking garbage block indices until a slice panic
// (generic/525 daemon death 2026-09-11); the read loop had the same
// overflow in endOff/extEnd/blkEnd plus a phantom loop iteration from the
// blkOff += blockSize wrap. s_maxbytes gates now fail such ops with EFBIG
// (kernel parity: sb->s_maxbytes = MAX_LFS_FILESIZE, super.c:495; the
// write-overflow check, file.c:3483), and the tail block's sums are clamped
// — a write ending exactly at s_maxbytes is legal (generic/525 passes on
// the kernel module).

import (
	"context"
	"syscall"
	"testing"

	"github.com/ctdk/briefs-utils/briefs"
)

// TestMaxFileSizeEFBIG: writes and truncates past s_maxbytes fail with EFBIG
// instead of overflowing the block arithmetic (generic/525 daemon panic).
func TestMaxFileSizeEFBIG(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)

	in, err := b.createInDir(1, "f", briefs.ModeFile|0o644, 1000, 1000, false, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ino := in.InodeNumber

	// A write spilling past MAX_LFS_FILESIZE: off fits, off+len does not.
	if _, err := b.writeFileData(context.Background(), ino, makePattern(1, 16), maxFileSize-8); err != syscall.EFBIG {
		t.Fatalf("write spilling past s_maxbytes: got %v, want EFBIG", err)
	}
	// The rejected write must not have touched the file.
	if di, _ := b.inodes.ReadInode(ino); di.FileSize != 0 {
		t.Fatalf("FileSize after rejected write: got %d, want 0", di.FileSize)
	}
	// Truncate past s_maxbytes fails the same way (inode_newsize_ok parity).
	if err := b.truncateInode(context.Background(), ino, uint64(maxFileSize)+1); err != syscall.EFBIG {
		t.Fatalf("truncate past s_maxbytes: got %v, want EFBIG", err)
	}

	fsckClean(t, b, img)
}

// TestWriteAtMaxFileSizeTail: a write ending exactly at MAX_LFS_FILESIZE is
// legal (generic/525 passes on the kernel module) — the final block's
// blockStart + blockSize sum wraps, and both the write loop and the read loop
// must clamp it instead of walking garbage block indices.
func TestWriteAtMaxFileSizeTail(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)

	in, err := b.createInDir(1, "f", briefs.ModeFile|0o644, 1000, 1000, false, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ino := in.InodeNumber

	p := makePattern(3, 100)
	off := maxFileSize - 100 // write ends exactly at s_maxbytes
	writeFile(t, b, ino, p, off)

	di, _ := b.inodes.ReadInode(ino)
	if di.FileSize != uint64(maxFileSize) {
		t.Fatalf("FileSize: got %d, want %d", di.FileSize, uint64(maxFileSize))
	}
	if !bytesEqual(readFile(t, b, ino, off, 100), p) {
		t.Fatal("write at the s_maxbytes tail read back wrong")
	}

	fsckClean(t, b, img)
}
