package fuse

// Regression test for the read-path hole bug found by fsx (2026-09-10
// write-path forensics): readFileData returned only extent-covered bytes
// (a running total), so a read starting in a hole — e.g. the never-written
// tail after fallocate ZERO_RANGE + truncate-up — returned 0 bytes, and
// mapped data after a hole landed at the wrong buffer offset.  The kernel
// (generic_file_read_iter) serves holes as zero pages and returns the full
// clamped count.

import (
	"bytes"
	"context"
	"testing"

	"github.com/ctdk/briefs-utils/briefs"
)

func TestReadHoleFullCount(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)

	const rootIno = 1
	in, err := b.createInDir(rootIno, "f", briefs.ModeFile|0o644, 1000, 1000, false, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ino := in.InodeNumber

	// Data in block 0, a hole in block 1, data in block 2.
	bs := int64(b.blockSize)
	blk0 := bytes.Repeat([]byte{0xA5}, int(bs))
	blk2 := bytes.Repeat([]byte{0x5A}, int(bs))
	if _, err := b.writeFileData(context.Background(), ino, blk0, 0); err != nil {
		t.Fatalf("write blk0: %v", err)
	}
	if err := b.truncateInode(context.Background(), ino, uint64(3*bs)); err != nil {
		t.Fatalf("truncate up: %v", err)
	}
	if _, err := b.writeFileData(context.Background(), ino, blk2, 2*bs); err != nil {
		t.Fatalf("write blk2: %v", err)
	}

	// Read entirely inside the hole: full count of zeros (the fsx failure
	// was exactly this shape: 0 bytes instead of 0xad6e).
	got, err := b.readFileData(ino, make([]byte, bs), bs)
	if err != nil {
		t.Fatalf("read hole: %v", err)
	}
	if len(got) != int(bs) {
		t.Fatalf("hole read returned %d bytes, want %d", len(got), bs)
	}
	if !allZero(got) {
		t.Fatal("hole read returned non-zero bytes")
	}

	// Read spanning [data, hole, data]: every byte at its absolute offset.
	full := make([]byte, 3*bs)
	got, err = b.readFileData(ino, full, 0)
	if err != nil {
		t.Fatalf("read span: %v", err)
	}
	if len(got) != int(3*bs) {
		t.Fatalf("span read returned %d bytes, want %d", len(got), 3*bs)
	}
	if !bytes.Equal(got[:bs], blk0) {
		t.Fatal("block 0 mismatch after hole rewrite")
	}
	if !allZero(got[bs : 2*bs]) {
		t.Fatal("hole (block 1) not zero")
	}
	if !bytes.Equal(got[2*bs:], blk2) {
		t.Fatal("block 2 mismatch — data after a hole landed at the wrong offset")
	}

	// Read past the last extent within EOF (tail hole after truncate-up):
	// full count of zeros too.
	if err := b.truncateInode(context.Background(), ino, uint64(4*bs)); err != nil {
		t.Fatalf("truncate up to 4 blocks: %v", err)
	}
	tail := make([]byte, bs)
	got, err = b.readFileData(ino, tail, 3*bs)
	if err != nil {
		t.Fatalf("read tail hole: %v", err)
	}
	if int64(len(got)) != bs {
		t.Fatalf("tail hole read returned %d bytes, want %d", len(got), bs)
	}
	if !allZero(got) {
		t.Fatal("tail hole read returned non-zero bytes")
	}
}
