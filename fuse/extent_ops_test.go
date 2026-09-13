package fuse

import (
	"context"
	"syscall"
	"testing"

	"github.com/ctdk/briefs-utils/briefs"
)

// setattrReq builds a fuseSetAttrIn with the given fields set.
func setattrReq(valid uint32, opts ...func(*fuseSetAttrIn)) *fuseSetAttrIn {
	r := &fuseSetAttrIn{valid: valid}
	for _, o := range opts {
		o(r)
	}
	return r
}

func withSize(s uint64) func(*fuseSetAttrIn) { return func(r *fuseSetAttrIn) { r.size = s } }
func withMode(m uint32) func(*fuseSetAttrIn) { return func(r *fuseSetAttrIn) { r.mode = m } }
func withUID(u uint32) func(*fuseSetAttrIn)  { return func(r *fuseSetAttrIn) { r.uid = u } }
func withGID(g uint32) func(*fuseSetAttrIn)  { return func(r *fuseSetAttrIn) { r.gid = g } }

// TestFallocatePreallocate covers KEEP_SIZE preallocate (unwritten extents that
// read as zeros and convert on write) and plain preallocate (grows size).
func TestFallocatePreallocate(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)

	in, _ := b.createInDir(1, "p", briefs.ModeFile|0o644, 1000, 1000, false, 0)
	ino := in.InodeNumber

	// KEEP_SIZE preallocate [0, 8192): two unwritten blocks (merged into one
	// extent of len 2), size unchanged (0).
	freeBeforePre := b.dataAlloc.FreeCount()
	if err := b.fallocateOp(context.Background(), ino, 0, 8192, fallocKeepSize); err != nil {
		t.Fatalf("fallocate keep-size: %v", err)
	}
	di, _ := b.inodes.ReadInode(ino)
	if di.FileSize != 0 {
		t.Fatalf("KEEP_SIZE grew size: want 0, got %d", di.FileSize)
	}
	if got := b.dataAlloc.FreeCount(); got != freeBeforePre-2 {
		t.Fatalf("preallocate allocated blocks: free %d -> %d (want -2)", freeBeforePre, got)
	}
	exts, _, _ := b.collectExtentsAndNodes(di)
	if len(exts) != 1 || exts[0].Offset != 0 || exts[0].Len != 2 || exts[0].Flags&briefs.ExtentFlagUnwritten == 0 {
		t.Fatalf("preallocate extents: want one unwritten {0,2}, got %+v", exts)
	}
	// Reads return zeros (unwritten).
	got := readFile(t, b, ino, 0, 8192)
	for i, v := range got {
		if v != 0 {
			t.Fatalf("unwritten read byte %d: want 0, got %d", i, v)
		}
	}

	// A write into the unwritten range converts block 0 and grows the size.
	pat := makePattern(1, 100)
	writeFile(t, b, ino, pat, 0)
	di, _ = b.inodes.ReadInode(ino)
	if di.FileSize != 100 {
		t.Fatalf("after write into preallocated: size want 100, got %d", di.FileSize)
	}
	if got := readFile(t, b, ino, 0, 100); !bytesEqual(got, pat) {
		t.Fatalf("written region readback mismatch")
	}
	// Block 1 is still unwritten -> zeros.
	if got := readFile(t, b, ino, 4096, 4096); !allZero(got) {
		t.Fatalf("unwritten block 1 should read zeros")
	}

	// Re-preallocating the same range is a no-op (blocks already mapped).
	freeBefore := b.dataAlloc.FreeCount()
	if err := b.fallocateOp(context.Background(), ino, 0, 8192, fallocKeepSize); err != nil {
		t.Fatalf("re-fallocate: %v", err)
	}
	if got := b.dataAlloc.FreeCount(); got != freeBefore {
		t.Fatalf("re-fallocate consumed blocks: %d -> %d", freeBefore, got)
	}

	// Plain preallocate past EOF grows the size.
	if err := b.fallocateOp(context.Background(), ino, 8192, 4096, 0); err != nil {
		t.Fatalf("plain fallocate: %v", err)
	}
	di, _ = b.inodes.ReadInode(ino)
	if di.FileSize != 12288 {
		t.Fatalf("plain fallocate size: want 12288, got %d", di.FileSize)
	}

	fsckClean(t, b, img)
}

// TestFallocatePunchHole writes data, punches a hole in the middle, and verifies
// the hole reads zeros while the data outside survives.
func TestFallocatePunchHole(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)

	in, _ := b.createInDir(1, "h", briefs.ModeFile|0o644, 1000, 1000, false, 0)
	ino := in.InodeNumber
	bs := int64(b.blockSize)

	// Write three blocks of distinct data.
	p0 := makePattern(1, int(bs))
	p1 := makePattern(2, int(bs))
	p2 := makePattern(3, int(bs))
	writeFile(t, b, ino, p0, 0)
	writeFile(t, b, ino, p1, bs)
	writeFile(t, b, ino, p2, 2*bs)

	// Punch a hole in block 1.
	if err := b.fallocateOp(context.Background(), ino, uint64(bs), uint64(bs), fallocPunchHole|fallocKeepSize); err != nil {
		t.Fatalf("punch hole: %v", err)
	}
	if got := readFile(t, b, ino, bs, bs); !allZero(got) {
		t.Fatalf("punched block should read zeros")
	}
	if got := readFile(t, b, ino, 0, bs); !bytesEqual(got, p0) {
		t.Fatalf("block 0 changed after punch")
	}
	if got := readFile(t, b, ino, 2*bs, bs); !bytesEqual(got, p2) {
		t.Fatalf("block 2 changed after punch")
	}
	// The punched block's data block must be freed.
	di, _ := b.inodes.ReadInode(ino)
	exts, _, _ := b.collectExtentsAndNodes(di)
	for _, e := range exts {
		if e.Offset == 1 && e.Phys != 0 {
			t.Fatalf("punched block 1 still mapped (phys %d)", e.Phys)
		}
	}

	fsckClean(t, b, img)
}

// TestTruncate covers truncate down (frees extents, zeroes EOF tail) and up
// (grows size, gap reads zeros).
func TestTruncate(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)

	in, _ := b.createInDir(1, "t", briefs.ModeFile|0o644, 1000, 1000, false, 0)
	ino := in.InodeNumber
	bs := int64(b.blockSize)

	// Write ~2.5 blocks.
	writeFile(t, b, ino, makePattern(1, int(2*bs+1000)), 0)
	freeBefore := b.dataAlloc.FreeCount()

	// Truncate down to 1000 (frees blocks 1+; zeroes the tail of block 0).
	if err := b.truncateInode(context.Background(), ino, 1000); err != nil {
		t.Fatalf("truncate down: %v", err)
	}
	di, _ := b.inodes.ReadInode(ino)
	if di.FileSize != 1000 {
		t.Fatalf("truncate down size: want 1000, got %d", di.FileSize)
	}
	// Fix C defers the frees to the next journal sync's SyncMeta (records
	// commit first); commit like an fsync would, then the blocks are free.
	if err := b.journal.Sync(false); err != nil {
		t.Fatalf("journal sync: %v", err)
	}
	if got := b.dataAlloc.FreeCount(); got <= freeBefore {
		t.Fatalf("truncate down did not free blocks: %d -> %d", freeBefore, got)
	}
	if got := readFile(t, b, ino, 0, 1000); !bytesEqual(got, makePattern(1, 1000)) {
		t.Fatalf("truncate down data mismatch")
	}

	// Truncate up to 2*bs; the gap reads zeros.
	if err := b.truncateInode(context.Background(), ino, uint64(2*bs)); err != nil {
		t.Fatalf("truncate up: %v", err)
	}
	di, _ = b.inodes.ReadInode(ino)
	if di.FileSize != uint64(2*bs) {
		t.Fatalf("truncate up size: want %d, got %d", 2*bs, di.FileSize)
	}
	got := readFile(t, b, ino, 1000, int64(2*bs)-1000)
	for i, v := range got {
		if v != 0 {
			t.Fatalf("truncate-up gap byte %d: want 0, got %d", i, v)
		}
	}

	fsckClean(t, b, img)
}

// TestKillpriv checks that suid/sgid and security.capability are stripped on
// write and chown (generic/093/193).
func TestKillpriv(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)

	in, _ := b.createInDir(1, "k", briefs.ModeFile|0o644, 1000, 1000, false, 0)
	ino := in.InodeNumber

	// chmod 4755 (setuid).
	if err := b.setattrOp(context.Background(), ino, setattrReq(fattrMode, withMode(briefs.ModeFile|0o4755))); err != nil {
		t.Fatalf("chmod setuid: %v", err)
	}
	di, _ := b.inodes.ReadInode(ino)
	if di.Filemode&0o4000 == 0 {
		t.Fatalf("setuid not set by chmod")
	}
	// A write strips setuid.
	writeFile(t, b, ino, makePattern(1, 50), 0)
	di, _ = b.inodes.ReadInode(ino)
	if di.Filemode&0o4000 != 0 {
		t.Fatalf("setuid not stripped on write: mode %o", di.Filemode)
	}

	// chmod setuid again, then chown -> strips setuid.
	if err := b.setattrOp(context.Background(), ino, setattrReq(fattrMode, withMode(briefs.ModeFile|0o4755))); err != nil {
		t.Fatalf("chmod setuid 2: %v", err)
	}
	if err := b.setattrOp(context.Background(), ino, setattrReq(fattrUID, withUID(2000))); err != nil {
		t.Fatalf("chown: %v", err)
	}
	di, _ = b.inodes.ReadInode(ino)
	if di.Filemode&0o4000 != 0 {
		t.Fatalf("setuid not stripped on chown: mode %o", di.Filemode)
	}
	if di.Uid != 2000 {
		t.Fatalf("chown uid: want 2000, got %d", di.Uid)
	}

	// security.capability is cleared on a write.
	if err := b.setXattr(ino, "security.capability", []byte{0, 0, 0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 0, 1, 2, 3}, 0); err != nil {
		t.Fatalf("set security.capability: %v", err)
	}
	if got, _ := b.getXattr(ino, "security.capability"); len(got) == 0 {
		t.Fatalf("security.capability not set")
	}
	writeFile(t, b, ino, makePattern(2, 50), 0)
	if _, err := b.getXattr(ino, "security.capability"); err != syscall.ENODATA {
		t.Fatalf("security.capability not cleared on write: err %v", err)
	}

	fsckClean(t, b, img)
}

// TestRemovePrivsGating checks removePrivs against the kernel's
// setattr_should_drop_suidgid rules (fs/attr.c): nothing for a CAP_FSETID
// caller or a non-regular file; suid always for an unprivileged caller; sgid
// only when the file is group-executable or the caller is outside the file's
// group; security.capability cleared even for the privileged caller
// (generic/093's root append, 683's root cases, 355's sgid golden lines).
func TestRemovePrivsGating(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)

	in, _ := b.createInDir(1, "g", briefs.ModeFile|0o644, 1000, 1000, false, 0)
	ino := in.InodeNumber

	// Caller contexts: unprivileged gid-1000 (in the file's group), and a
	// CAP_FSETID holder. The injected /proc status applies to whichever
	// caller each write carries.
	unprivGroup := callerCtx(1000, 1000, 1234)
	unprivOther := callerCtx(1000, 2000, 1234)
	capFsetid := callerCtx(0, 0, 1234)

	chmod := func(m uint32) {
		t.Helper()
		if err := b.setattrOp(context.Background(), ino,
			setattrReq(fattrMode, withMode(briefs.ModeFile|m))); err != nil {
			t.Fatalf("chmod %o: %v", m, err)
		}
	}
	mode := func() uint32 {
		t.Helper()
		di, _ := b.inodes.ReadInode(ino)
		return di.Filemode & 0o7777
	}
	write := func(ctx context.Context) {
		t.Helper()
		if _, err := b.writeFileData(ctx, ino, makePattern(1, 50), 0); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	// 4755, unprivileged in-group write -> 755 (suid stripped).
	injectCallerStatus(t, callerStatus{}, true)
	chmod(0o4755)
	write(unprivGroup)
	if got := mode(); got != 0o755 {
		t.Fatalf("4755 unpriv write: want 755, got %o", got)
	}

	// 2644 (sgid, no group-exec), unprivileged in-group write -> sgid kept
	// (setattr_should_drop_sgid: S_IXGRP clear and caller in group).
	chmod(0o2644)
	write(unprivGroup)
	if got := mode(); got != 0o2644 {
		t.Fatalf("2644 in-group write: want 2644, got %o", got)
	}

	// 2644, unprivileged out-of-group write -> sgid stripped.
	chmod(0o2644)
	write(unprivOther)
	if got := mode(); got != 0o644 {
		t.Fatalf("2644 out-of-group write: want 644, got %o", got)
	}

	// 2755 (sgid + group-exec), in-group -> stripped anyway (S_IXGRP).
	chmod(0o2755)
	write(unprivGroup)
	if got := mode(); got != 0o755 {
		t.Fatalf("2755 in-group write: want 755, got %o", got)
	}

	// 6755, CAP_FSETID write -> both bits preserved (root case).
	injectCallerStatus(t, callerStatus{capEff: 1 << capFSetIDBit}, true)
	chmod(0o6755)
	write(capFsetid)
	if got := mode(); got != 0o6755 {
		t.Fatalf("6755 CAP_FSETID write: want 6755, got %o", got)
	}

	// security.capability is cleared even for the CAP_FSETID caller
	// (generic/093: a root append still drops file capabilities), and other
	// xattrs are untouched.
	if err := b.setXattr(ino, "security.capability", makePattern(9, 20), 0); err != nil {
		t.Fatalf("set security.capability: %v", err)
	}
	if err := b.setXattr(ino, "trusted.other", makePattern(8, 20), 0); err != nil {
		t.Fatalf("set trusted.other: %v", err)
	}
	chmod(0o6755)
	write(capFsetid)
	if _, err := b.getXattr(ino, "security.capability"); err != syscall.ENODATA {
		t.Fatalf("security.capability not cleared for CAP_FSETID write: err %v", err)
	}
	if _, err := b.getXattr(ino, "trusted.other"); err != nil {
		t.Fatalf("trusted.other disturbed by killpriv: err %v", err)
	}

	// A non-regular file (setgid directory) is a no-op.
	injectCallerStatus(t, callerStatus{}, true)
	dir, _ := b.createInDir(1, "d", briefs.ModeDir|0o2755, 1000, 1000, false, 0)
	di, _ := b.inodes.ReadInode(dir.InodeNumber)
	if di.Filemode&0o7777 != 0o2755 {
		t.Fatalf("dir mode setup: want 2755, got %o", di.Filemode&0o7777)
	}
	if err := b.removePrivs(unprivGroup, di); err != nil {
		t.Fatalf("removePrivs on dir: %v", err)
	}
	di, _ = b.inodes.ReadInode(dir.InodeNumber)
	if di.Filemode&0o7777 != 0o2755 {
		t.Fatalf("removePrivs stripped a non-regular file: got %o", di.Filemode&0o7777)
	}

	fsckClean(t, b, img)
}

// TestKillprivFallocate checks the fallocate killpriv site (generic/683):
// an unprivileged fallocate strips suid; a CAP_FSETID fallocate preserves it.
func TestKillprivFallocate(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)

	in, _ := b.createInDir(1, "f", briefs.ModeFile|0o644, 1000, 1000, false, 0)
	ino := in.InodeNumber
	chmod := func(m uint32) {
		t.Helper()
		if err := b.setattrOp(context.Background(), ino,
			setattrReq(fattrMode, withMode(briefs.ModeFile|m))); err != nil {
			t.Fatalf("chmod %o: %v", m, err)
		}
	}

	// Unprivileged fallocate strips suid (683's qa_user case). The kernel
	// strips nothing on the FUSE fallocate path without killpriv_v2, so the
	// daemon-side strip is the only one.
	injectCallerStatus(t, callerStatus{}, true)
	chmod(0o4755)
	if err := b.fallocateOp(callerCtx(1000, 1000, 1234), ino, 0, 8192, 0); err != nil {
		t.Fatalf("fallocate: %v", err)
	}
	di, _ := b.inodes.ReadInode(ino)
	if di.Filemode&0o4000 != 0 {
		t.Fatalf("setuid not stripped on unpriv fallocate: mode %o", di.Filemode)
	}

	// CAP_FSETID fallocate preserves it (683's root case).
	injectCallerStatus(t, callerStatus{capEff: 1 << capFSetIDBit}, true)
	chmod(0o4755)
	if err := b.fallocateOp(callerCtx(0, 0, 1234), ino, 8192, 8192, 0); err != nil {
		t.Fatalf("fallocate 2: %v", err)
	}
	di, _ = b.inodes.ReadInode(ino)
	if di.Filemode&0o4000 == 0 {
		t.Fatalf("setuid stripped on CAP_FSETID fallocate: mode %o", di.Filemode)
	}

	fsckClean(t, b, img)
}

// allZero reports whether b is all zero bytes.
func allZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

// TestFallocateCollapseRange covers COLLAPSE_RANGE: the removed middle's
// blocks are freed, the tail shifts down (data readback at the new offsets),
// the size shrinks, and the alignment / reaching-EOF / inline-data guards
// fire (mirrors the kernel's do_collapse_range checks, file.c:3258).
func TestFallocateCollapseRange(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)

	in, _ := b.createInDir(1, "c", briefs.ModeFile|0o644, 1000, 1000, false, 0)
	ino := in.InodeNumber
	bs := int64(b.blockSize)

	// Four blocks of distinct data.
	pats := make([][]byte, 4)
	for i := range pats {
		pats[i] = makePattern(i+1, int(bs))
		writeFile(t, b, ino, pats[i], int64(i)*bs)
	}

	freeBefore := b.dataAlloc.FreeCount()
	if err := b.fallocateOp(context.Background(), ino, uint64(bs), uint64(bs), fallocCollapseRange); err != nil {
		t.Fatalf("collapse range: %v", err)
	}

	di, _ := b.inodes.ReadInode(ino)
	if di.FileSize != uint64(3*bs) {
		t.Fatalf("after collapse: size want %d, got %d", 3*bs, di.FileSize)
	}
	// Fix C defers the free to the next journal sync's SyncMeta; commit
	// like an fsync would, then the removed block is free.
	if err := b.journal.Sync(false); err != nil {
		t.Fatalf("journal sync: %v", err)
	}
	if got := b.dataAlloc.FreeCount(); got != freeBefore+1 {
		t.Fatalf("collapse must free the removed block: free %d -> %d (want +1)", freeBefore, got)
	}
	// Tail shifted down: p0 p2 p3.
	for i, want := range [][]byte{pats[0], pats[2], pats[3]} {
		if got := readFile(t, b, ino, int64(i)*bs, bs); !bytesEqual(got, want) {
			t.Fatalf("post-collapse block %d: data mismatch", i)
		}
	}

	// Guards: unaligned offset, range reaching EOF, inline data.
	if err := b.fallocateOp(context.Background(), ino, 100, uint64(bs), fallocCollapseRange); err != syscall.EINVAL {
		t.Errorf("unaligned collapse: want EINVAL, got %v", err)
	}
	if err := b.fallocateOp(context.Background(), ino, 2*uint64(bs), uint64(bs), fallocCollapseRange); err != syscall.EINVAL {
		t.Errorf("collapse reaching EOF: want EINVAL, got %v", err)
	}

	inl, _ := b.createInDir(1, "ci", briefs.ModeFile|0o644, 1000, 1000, false, 0)
	writeFile(t, b, inl.InodeNumber, makePattern(9, 100), 0)
	// A block-aligned range on a <= 256-byte inline file always reaches
	// EOF first, so the bounds EINVAL fires before the inline EOPNOTSUPP
	// (the kernel's check order, file.c:3258-3273).
	if err := b.fallocateOp(context.Background(), inl.InodeNumber, 0, uint64(bs), fallocCollapseRange); err != syscall.EINVAL {
		t.Errorf("collapse on inline file: want EINVAL (bounds first), got %v", err)
	}

	fsckClean(t, b, img)
}

// TestFallocateInsertRange covers INSERT_RANGE: a hole opens at the offset,
// the tail shifts up (data readback at the new offsets), the size grows, and
// the offset-past-EOF / alignment / inline-data guards fire (mirrors the
// kernel's do_insert_range checks, file.c:3343).
func TestFallocateInsertRange(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)

	in, _ := b.createInDir(1, "i", briefs.ModeFile|0o644, 1000, 1000, false, 0)
	ino := in.InodeNumber
	bs := int64(b.blockSize)

	p0 := makePattern(1, int(bs))
	p1 := makePattern(2, int(bs))
	writeFile(t, b, ino, p0, 0)
	writeFile(t, b, ino, p1, bs)

	freeBefore := b.dataAlloc.FreeCount()
	if err := b.fallocateOp(context.Background(), ino, uint64(bs), uint64(bs), fallocInsertRange); err != nil {
		t.Fatalf("insert range: %v", err)
	}

	di, _ := b.inodes.ReadInode(ino)
	if di.FileSize != uint64(3*bs) {
		t.Fatalf("after insert: size want %d, got %d", 3*bs, di.FileSize)
	}
	if got := b.dataAlloc.FreeCount(); got != freeBefore {
		t.Fatalf("insert must not allocate: free %d -> %d", freeBefore, got)
	}
	if got := readFile(t, b, ino, 0, bs); !bytesEqual(got, p0) {
		t.Fatalf("block 0 changed after insert")
	}
	if got := readFile(t, b, ino, bs, bs); !allZero(got) {
		t.Fatalf("inserted range should read zeros")
	}
	if got := readFile(t, b, ino, 2*bs, bs); !bytesEqual(got, p1) {
		t.Fatalf("tail block should hold the old block-1 data")
	}
	// The shifted tail keeps its phys (a straddling extent splits into a kept
	// prefix and a re-pointed suffix).
	exts, _, _ := b.collectExtentsAndNodes(di)
	if len(exts) != 2 || exts[1].Offset != 2 || exts[1].Len != 1 {
		t.Fatalf("insert extents: want kept {0,1} + shifted {2,1}, got %+v", exts)
	}

	// Guards: unaligned, offset at/past EOF, inline data.
	if err := b.fallocateOp(context.Background(), ino, 100, uint64(bs), fallocInsertRange); err != syscall.EINVAL {
		t.Errorf("unaligned insert: want EINVAL, got %v", err)
	}
	if err := b.fallocateOp(context.Background(), ino, 3*uint64(bs), uint64(bs), fallocInsertRange); err != syscall.EINVAL {
		t.Errorf("insert at EOF: want EINVAL, got %v", err)
	}

	inl, _ := b.createInDir(1, "ii", briefs.ModeFile|0o644, 1000, 1000, false, 0)
	writeFile(t, b, inl.InodeNumber, makePattern(9, 100), 0)
	if err := b.fallocateOp(context.Background(), inl.InodeNumber, 0, uint64(bs), fallocInsertRange); err != syscall.EOPNOTSUPP {
		t.Errorf("insert on inline file: want EOPNOTSUPP, got %v", err)
	}

	fsckClean(t, b, img)
}

// TestFallocateZeroRange covers ZERO_RANGE semantics (generic/009): partial
// blocks are zeroed byte-granular and stay data; the block-aligned middle is
// converted to unwritten (written blocks flip in place, holes are allocated);
// !KEEP_SIZE extends the file; the inline-data branch zeroes in place.
func TestFallocateZeroRange(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)

	in, _ := b.createInDir(1, "z", briefs.ModeFile|0o644, 1000, 1000, false, 0)
	ino := in.InodeNumber
	bs := int64(b.blockSize)

	// --- Partial-block zero keeps the block as data (case 17). ---
	pat := makePattern(1, 300)
	writeFile(t, b, ino, pat, 0)
	if err := b.fallocateOp(context.Background(), ino, 100, 100, fallocZeroRange); err != nil {
		t.Fatalf("partial zero range: %v", err)
	}
	got := readFile(t, b, ino, 0, 300)
	if !bytesEqual(got[:100], pat[:100]) || !allZero(got[100:200]) || !bytesEqual(got[200:], pat[200:]) {
		t.Fatalf("partial zero: [100,200) must be zeroed, rest kept")
	}
	di, _ := b.inodes.ReadInode(ino)
	exts, _, _ := b.collectExtentsAndNodes(di)
	if len(exts) == 0 || exts[0].Flags&briefs.ExtentFlagUnwritten != 0 {
		t.Fatalf("partial zero must not convert to unwritten: %+v", exts)
	}

	// --- Aligned middle flips written blocks to unwritten in place. ---
	p0 := makePattern(2, int(bs))
	p1 := makePattern(3, int(bs))
	p2 := makePattern(4, int(bs))
	writeFile(t, b, ino, p0, 0)
	writeFile(t, b, ino, p1, bs)
	writeFile(t, b, ino, p2, 2*bs)
	di, _ = b.inodes.ReadInode(ino)
	exts, _, _ = b.collectExtentsAndNodes(di)
	physBefore := exts[0].Phys
	freeBefore := b.dataAlloc.FreeCount()

	if err := b.fallocateOp(context.Background(), ino, 0, 2*uint64(bs), fallocZeroRange); err != nil {
		t.Fatalf("aligned zero range: %v", err)
	}
	if got := readFile(t, b, ino, 0, 2*bs); !allZero(got) {
		t.Fatalf("zeroed middle must read zeros")
	}
	if got := readFile(t, b, ino, 2*bs, bs); !bytesEqual(got, p2) {
		t.Fatalf("block past the range changed")
	}
	di, _ = b.inodes.ReadInode(ino)
	exts, _, _ = b.collectExtentsAndNodes(di)
	if len(exts) != 2 || exts[0].Len != 2 || exts[0].Flags&briefs.ExtentFlagUnwritten == 0 || exts[0].Phys != physBefore {
		t.Fatalf("middle must flip to unwritten in place: %+v", exts)
	}
	if got := b.dataAlloc.FreeCount(); got != freeBefore {
		t.Fatalf("in-place flip must not allocate or free: free %d -> %d", freeBefore, got)
	}
	// A write into the unwritten range converts it (and overwrites the
	// masked old data).
	writeFile(t, b, ino, p1, bs)
	if got := readFile(t, b, ino, bs, bs); !bytesEqual(got, p1) {
		t.Fatalf("write into converted block must see new data")
	}

	// --- Hole in the middle is allocated as unwritten. ---
	in2, _ := b.createInDir(1, "zh", briefs.ModeFile|0o644, 1000, 1000, false, 0)
	ino2 := in2.InodeNumber
	writeFile(t, b, ino2, p0, 0)    // block 0
	writeFile(t, b, ino2, p2, 2*bs) // block 2 (block 1 stays a hole)
	freeBefore = b.dataAlloc.FreeCount()
	if err := b.fallocateOp(context.Background(), ino2, uint64(bs), uint64(bs), fallocZeroRange); err != nil {
		t.Fatalf("zero range over a hole: %v", err)
	}
	if got := b.dataAlloc.FreeCount(); got != freeBefore-1 {
		t.Fatalf("hole zero must allocate one unwritten block: free %d -> %d", freeBefore, got)
	}
	if got := readFile(t, b, ino2, bs, bs); !allZero(got) {
		t.Fatalf("zeroed hole must read zeros")
	}

	// --- !KEEP_SIZE extends the file; the old-EOF tail is zeroed. ---
	in3, _ := b.createInDir(1, "zx", briefs.ModeFile|0o644, 1000, 1000, false, 0)
	ino3 := in3.InodeNumber
	writeFile(t, b, ino3, pat, 0) // 300 bytes, mid-block EOF
	if err := b.fallocateOp(context.Background(), ino3, 400, 2*uint64(bs), fallocZeroRange); err != nil {
		t.Fatalf("extending zero range: %v", err)
	}
	di, _ = b.inodes.ReadInode(ino3)
	if di.FileSize != 400+2*uint64(bs) {
		t.Fatalf("extending zero: size want %d, got %d", 400+2*bs, di.FileSize)
	}
	got = readFile(t, b, ino3, 0, 400+2*bs)
	if !bytesEqual(got[:300], pat) || !allZero(got[300:]) {
		t.Fatalf("extending zero: [0,300) kept, rest zero; got tail %v", got[300:340])
	}

	// --- Inline-data branch: zero within the region, optionally grow. ---
	inl, _ := b.createInDir(1, "zi", briefs.ModeFile|0o644, 1000, 1000, false, 0)
	writeFile(t, b, inl.InodeNumber, pat, 0)
	if err := b.fallocateOp(context.Background(), inl.InodeNumber, 100, 100, fallocZeroRange); err != nil {
		t.Fatalf("inline zero range: %v", err)
	}
	got = readFile(t, b, inl.InodeNumber, 0, 300)
	if !bytesEqual(got[:100], pat[:100]) || !allZero(got[100:200]) || !bytesEqual(got[200:], pat[200:]) {
		t.Fatalf("inline partial zero mismatch")
	}
	if err := b.fallocateOp(context.Background(), inl.InodeNumber, 300, 200, fallocZeroRange); err != nil {
		t.Fatalf("inline extending zero: %v", err)
	}
	// end (500) exceeds the inline region, so this promotes to extent-backed
	// (the kernel's promote branch) and the readback spans the promoted block.
	di, _ = b.inodes.ReadInode(inl.InodeNumber)
	if di.FileSize != 500 {
		t.Fatalf("inline grow: size want 500, got %d", di.FileSize)
	}
	want := append([]byte{}, pat...)
	for i := 100; i < 200; i++ {
		want[i] = 0 // the earlier partial zero persists
	}
	got = readFile(t, b, inl.InodeNumber, 0, 500)
	if !bytesEqual(got[:300], want) || !allZero(got[300:]) {
		t.Fatalf("inline grow readback mismatch")
	}

	fsckClean(t, b, img)
}

// TestFallocateFragmentedHole covers allocUnwrittenHole's fragmentation
// fallback: with the data region carved into short free runs, a hole larger
// than any single run is covered by run harvesting — one extent per
// free-space fragment, never one per block.  An oversized hole ENOSPCs and
// rolls back the partially harvested runs.
func TestFallocateFragmentedHole(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)

	bs := b.blockSize
	// The file is created first (its dir trie page takes a low data block),
	// then everything except data-relative runs [100,103), [200,202),
	// [300,303) is reserved: 8 free blocks, no contiguous run longer than 3.
	in, _ := b.createInDir(1, "frag", briefs.ModeFile|0o644, 1000, 1000, false, 0)
	ino := in.InodeNumber
	free := map[uint64]bool{}
	for blk := uint64(100); blk < 103; blk++ {
		free[blk] = true
	}
	for blk := uint64(200); blk < 202; blk++ {
		free[blk] = true
	}
	for blk := uint64(300); blk < 303; blk++ {
		free[blk] = true
	}
	for blk := uint64(0); blk < b.dataAlloc.blockCount; blk++ {
		if !free[blk] {
			b.dataAlloc.ReserveBlock(blk)
		}
	}
	if got := b.dataAlloc.FreeCount(); got != 8 {
		t.Fatalf("fragmented setup: FreeCount = %d, want 8", got)
	}

	// 16 blocks wanted, 8 free: ENOSPC with the partial harvest rolled
	// back (the free count must come home unchanged).
	if err := b.fallocateOp(context.Background(), ino, 0, 16*bs, 0); err != syscall.ENOSPC {
		t.Fatalf("oversized fragmented fallocate: err = %v, want ENOSPC", err)
	}
	if got := b.dataAlloc.FreeCount(); got != 8 {
		t.Fatalf("after ENOSPC rollback: FreeCount = %d, want 8", got)
	}

	// 8 blocks across three fragments: three unwritten extents, in offset
	// order, covering exactly the three free runs.
	if err := b.fallocateOp(context.Background(), ino, 0, 8*bs, 0); err != nil {
		t.Fatalf("fragmented fallocate: %v", err)
	}
	di, _ := b.inodes.ReadInode(ino)
	if di.FileSize != 8*bs {
		t.Fatalf("size after fragmented fallocate = %d, want %d", di.FileSize, 8*bs)
	}
	if got := b.dataAlloc.FreeCount(); got != 0 {
		t.Fatalf("FreeCount after fallocate = %d, want 0", got)
	}
	exts, _, _ := b.collectExtentsAndNodes(di)
	type wantExt struct{ off, rel, n uint64 }
	want := []wantExt{{0, 100, 3}, {3, 200, 2}, {5, 300, 3}}
	if len(exts) != len(want) {
		t.Fatalf("fragmented extents: got %d, want %d (%+v)", len(exts), len(want), exts)
	}
	for i, w := range want {
		e := exts[i]
		if e.Offset != w.off || e.Phys-b.dataRegionStart != w.rel || e.Len != w.n ||
			e.Flags&briefs.ExtentFlagUnwritten == 0 {
			t.Fatalf("extent %d = %+v, want {off:%d rel:%d len:%d unwritten}", i, e, w.off, w.rel, w.n)
		}
	}
	// Unwritten fragments read as zeros end to end.
	got := readFile(t, b, ino, 0, int64(8*bs))
	if !allZero(got) {
		t.Fatalf("fragmented unwritten readback not zero")
	}
	// (No fsckClean here: the 4835 blocks this test reserved straight
	// into the bitmap have no owning inode, which fsck rightly flags —
	// the other fallocate tests own the fsck-clean invariant.)
}
