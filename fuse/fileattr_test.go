package fuse

import (
	"encoding/binary"
	"syscall"
	"testing"

	"github.com/ctdk/briefs-utils/briefs"
)

// TestFileattrSetGet exercises chattr/lsattr via the core fileattrSet/Get and
// the FS_IOC_* ioctl handlers, including immutable/append enforcement.
func TestFileattrSetGet(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)

	in, err := b.createInDir(1, "f", briefs.ModeFile|0o644, 1000, 1000, false)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ino := in.InodeNumber

	// Seed some data so the file is non-empty.
	if _, err := b.writeFileData(ino, makePattern(1, 100), 0); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	// --- lsattr (get) before any flags ---
	flags, _, _, err := b.fileattrGet(ino)
	if err != nil {
		t.Fatalf("fileattrGet: %v", err)
	}
	if flags != 0 {
		t.Fatalf("initial flags: want 0, got 0x%x", flags)
	}

	// --- chattr +i (immutable) ---
	if err := b.fileattrSet(ino, true, fsImmutableFl, false, 0, 0, 0, 0); err != nil {
		t.Fatalf("fileattrSet +i: %v", err)
	}
	flags, xflags, _, _ := b.fileattrGet(ino)
	if flags&fsImmutableFl == 0 {
		t.Fatalf("after +i: flags 0x%x missing IMMUTABLE", flags)
	}
	if xflags&fsXflagImmutable == 0 {
		t.Fatalf("after +i: xflags 0x%x missing XFLAG_IMMUTABLE", xflags)
	}

	// A write to an immutable file must fail with EPERM.
	if _, err := b.writeFileData(ino, makePattern(2, 50), 0); err != syscall.EPERM {
		t.Fatalf("write to immutable: want EPERM, got %v", err)
	}

	// --- chattr -i (clear immutable) ---
	if err := b.fileattrSet(ino, true, 0, false, 0, 0, 0, 0); err != nil {
		t.Fatalf("fileattrSet -i: %v", err)
	}
	if _, err := b.writeFileData(ino, makePattern(3, 50), 0); err != nil {
		t.Fatalf("write after -i: %v", err)
	}

	// --- chattr +a (append-only) ---
	if err := b.fileattrSet(ino, true, fsAppendFl, false, 0, 0, 0, 0); err != nil {
		t.Fatalf("fileattrSet +a: %v", err)
	}
	// A write past EOF (non-append) must fail with EPERM.
	di, _ := b.inodes.ReadInode(ino)
	if _, err := b.writeFileData(ino, makePattern(4, 50), int64(di.FileSize)+100); err != syscall.EPERM {
		t.Fatalf("append-only non-EOF write: want EPERM, got %v", err)
	}
	// A write at EOF must succeed.
	size := int64(di.FileSize)
	if _, err := b.writeFileData(ino, makePattern(5, 50), size); err != nil {
		t.Fatalf("append-only EOF write: %v", err)
	}
	// Clear append.
	if err := b.fileattrSet(ino, true, 0, false, 0, 0, 0, 0); err != nil {
		t.Fatalf("fileattrSet -a: %v", err)
	}

	// --- DIRSYNC only on directories ---
	if err := b.fileattrSet(ino, true, fsDirsyncFl, false, 0, 0, 0, 0); err != syscall.EINVAL {
		t.Fatalf("DIRSYNC on file: want EINVAL, got %v", err)
	}
	dirIn, err := b.createInDir(1, "d", briefs.ModeDir|0o755, 1000, 1000, false)
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := b.fileattrSet(dirIn.InodeNumber, true, fsDirsyncFl, false, 0, 0, 0, 0); err != nil {
		t.Fatalf("DIRSYNC on dir: %v", err)
	}
	dflags, _, _, _ := b.fileattrGet(dirIn.InodeNumber)
	if dflags&fsDirsyncFl == 0 {
		t.Fatalf("DIRSYNC not set on dir")
	}

	// --- unsupported flags -> EOPNOTSUPP ---
	if err := b.fileattrSet(ino, true, 0x40000000, false, 0, 0, 0, 0); err != syscall.EOPNOTSUPP {
		t.Fatalf("unsupported flag: want EOPNOTSUPP, got %v", err)
	}

	fsckClean(t, b, img)
}

// TestFileattrIoctl round-trips flags through the FS_IOC_* ioctl handlers
// (chattr uses GETFLAGS/SETFLAGS; xfs_io uses FSGETXATTR/FSSETXATTR), verifying
// the FS_*_FL <-> FS_XFLAG_* translation.
func TestFileattrIoctl(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)

	in, err := b.createInDir(1, "io", briefs.ModeFile|0o644, 1000, 1000, false)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	n := &brieFSNode{bfs: b, ino: in.InodeNumber}

	// GETFLAGS initially 0.
	out := make([]byte, 8)
	if _, errno := n.Ioctl(nil, nil, fsIocGetflags, 0, nil, out); errno != 0 {
		t.Fatalf("GETFLAGS: %v", errno)
	}
	if got := binary.LittleEndian.Uint32(out); got != 0 {
		t.Fatalf("GETFLAGS initial: want 0, got 0x%x", got)
	}

	// SETFLAGS +i (immutable + nodump).
	inFlags := make([]byte, 8)
	binary.LittleEndian.PutUint32(inFlags, fsImmutableFl|fsNodumpFl)
	if _, errno := n.Ioctl(nil, nil, fsIocSetflags, 0, inFlags, nil); errno != 0 {
		t.Fatalf("SETFLAGS: %v", errno)
	}
	out = make([]byte, 8)
	if _, errno := n.Ioctl(nil, nil, fsIocGetflags, 0, nil, out); errno != 0 {
		t.Fatalf("GETFLAGS: %v", errno)
	}
	if got := binary.LittleEndian.Uint32(out); got&fsImmutableFl == 0 || got&fsNodumpFl == 0 {
		t.Fatalf("GETFLAGS after SETFLAGS: want IMMUTABLE|NODUMP, got 0x%x", got)
	}

	// FSGETXATTR translates to XFLAG_IMMUTABLE|XFLAG_NODUMP.
	fsx := make([]byte, sizeFsxattr)
	if _, errno := n.Ioctl(nil, nil, fsIocFsgetxattr, 0, nil, fsx); errno != 0 {
		t.Fatalf("FSGETXATTR: %v", errno)
	}
	if got := binary.LittleEndian.Uint32(fsx[0:]); got&(fsXflagImmutable|fsXflagNodump) != (fsXflagImmutable | fsXflagNodump) {
		t.Fatalf("FSGETXATTR xflags: want IMMUTABLE|NODUMP, got 0x%x", got)
	}

	// FSSETXATTR with XFLAG_SYNC -> GETFLAGS shows FS_SYNC_FL.
	setFsx := make([]byte, sizeFsxattr)
	binary.LittleEndian.PutUint32(setFsx[0:], fsXflagSync)
	if _, errno := n.Ioctl(nil, nil, fsIocFssetxattr, 0, setFsx, nil); errno != 0 {
		t.Fatalf("FSSETXATTR: %v", errno)
	}
	out = make([]byte, 8)
	if _, errno := n.Ioctl(nil, nil, fsIocGetflags, 0, nil, out); errno != 0 {
		t.Fatalf("GETFLAGS: %v", errno)
	}
	if got := binary.LittleEndian.Uint32(out); got&fsSyncFl == 0 || got&fsImmutableFl != 0 {
		t.Fatalf("after FSSETXATTR SYNC: want SYNC set & immutable cleared, got 0x%x", got)
	}

	// FSSETXATTR with unsupported xflag / extsize -> EOPNOTSUPP.
	setFsx = make([]byte, sizeFsxattr)
	binary.LittleEndian.PutUint32(setFsx[0:], 0x00000002) // FS_XFLAG_PREALLOC (unsupported)
	if _, errno := n.Ioctl(nil, nil, fsIocFssetxattr, 0, setFsx, nil); errno != syscall.EOPNOTSUPP {
		t.Fatalf("FSSETXATTR unsupported xflag: want EOPNOTSUPP, got %v", errno)
	}
	setFsx = make([]byte, sizeFsxattr)
	binary.LittleEndian.PutUint32(setFsx[0:], fsXflagSync)
	binary.LittleEndian.PutUint32(setFsx[4:], 1) // extsize != 0
	if _, errno := n.Ioctl(nil, nil, fsIocFssetxattr, 0, setFsx, nil); errno != syscall.EOPNOTSUPP {
		t.Fatalf("FSSETXATTR extsize: want EOPNOTSUPP, got %v", errno)
	}

	// Unknown ioctl -> ENOTTY.
	if _, errno := n.Ioctl(nil, nil, 0x12345678, 0, nil, nil); errno != syscall.ENOTTY {
		t.Fatalf("unknown ioctl: want ENOTTY, got %v", errno)
	}

	fsckClean(t, b, img)
}