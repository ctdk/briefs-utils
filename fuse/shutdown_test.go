package fuse

// XFS_IOC_GOINGDOWN shutdown coverage.
//
// shutdownOp (ioctl_mount.go) ports the kernel's briefs_shutdown
// (inode.c:159): LOGFLUSH flushes the journal without retiring the ring so
// the live region survives for the next mount's replay (generic/052);
// NOLOGFLUSH/DEFAULT leave it as-is; the freeze then refuses mutations
// EROFS and fails reads/fsync EIO (file.c:105-117, :556-566), and the
// unmount skips the checkpoint *and* the Close-flush (kernel put_super
// drops the checkpoint after a shutdown; Close's flush would persist what
// NOLOGFLUSH means to leave unpersisted).
//
// These tests pin both halves: the freeze's error contract at the op and
// handler level, and the journal state across the shutdown-skipped
// "unmount" (close without checkpoint) — LOGFLUSH keeps committed records
// replayable, NOLOGFLUSH leaves the uncommitted current journal block
// unpersisted so the on-disk replay range does not advance past the last
// sync, exactly as the kernel does.

import (
	"context"
	"syscall"
	"testing"

	"github.com/ctdk/briefs-utils/briefs"
)

// TestShutdownFlagSemantics pins the flag dispatch: unknown flags EINVAL on
// a healthy fs, idempotency checked before flag validation (a second call
// succeeds whatever it carries — kernel order), LOGFLUSH flushes a dirty
// journal, NOLOGFLUSH does not.
func TestShutdownFlagSemantics(t *testing.T) {
	mkfs := buildMkfs(t)
	b := openBridge(t, mkfsImage(t, mkfs, 5000))

	if b.shutdown {
		t.Fatal("fresh bridge reports shutdown")
	}
	// Unknown flags on a healthy fs: refused, state unchanged.
	if err := b.shutdownOp(0x3); err != syscall.EINVAL {
		t.Fatalf("shutdownOp(0x3): got %v, want EINVAL", err)
	}
	if b.shutdown {
		t.Fatal("shutdown set after EINVAL flags")
	}

	// A pending journal record: create a file and do not sync.
	if _, err := b.createInDir(1, "rec", briefs.ModeFile|0o644, 1000, 1000, false, 0); err != nil {
		t.Fatalf("create rec: %v", err)
	}
	if !b.journal.Dirty() {
		t.Fatal("journal not dirty after create")
	}

	if err := b.shutdownOp(shutdownFlagNoLogFlush); err != nil {
		t.Fatalf("shutdownOp(NOLOGFLUSH): %v", err)
	}
	if !b.shutdown {
		t.Fatal("shutdown not set after NOLOGFLUSH")
	}
	// NOLOGFLUSH leaves the pending records uncommitted.
	if !b.journal.Dirty() {
		t.Fatal("NOLOGFLUSH synced the journal")
	}

	// Idempotency precedes flag validation: the second call succeeds with
	// flags that a healthy fs would reject.
	if err := b.shutdownOp(0xdeadbeef); err != nil {
		t.Fatalf("second shutdownOp: %v, want nil (idempotent)", err)
	}

	// LOGFLUSH flushes: use a second bridge to get a dirty journal again.
	b2 := openBridge(t, mkfsImage(t, mkfs, 5000))
	if _, err := b2.createInDir(1, "rec", briefs.ModeFile|0o644, 1000, 1000, false, 0); err != nil {
		t.Fatalf("create rec: %v", err)
	}
	if err := b2.shutdownOp(shutdownFlagLogFlush); err != nil {
		t.Fatalf("shutdownOp(LOGFLUSH): %v", err)
	}
	if b2.journal.Dirty() {
		t.Fatal("LOGFLUSH left the journal dirty")
	}
	if !b2.shutdown {
		t.Fatal("shutdown not set after LOGFLUSH")
	}
}

// TestShutdownRefusals pins the freeze's error contract: mutations EROFS
// (create, xattr, label, trim), file reads EIO and fsync EIO through the
// node handlers (the kernel returns EIO, not the EROFS the read-only state
// alone would give — file.c:105-117, :556-566).
func TestShutdownRefusals(t *testing.T) {
	mkfs := buildMkfs(t)
	b := openBridge(t, mkfsImage(t, mkfs, 5000))
	f, err := b.createInDir(1, "file", briefs.ModeFile|0o644, 1000, 1000, false, 0)
	if err != nil {
		t.Fatalf("create file: %v", err)
	}
	writeFile(t, b, f.InodeNumber, makePattern(1, 4096), 0)

	if err := b.shutdownOp(shutdownFlagNoLogFlush); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	if _, err := b.createInDir(1, "new", briefs.ModeFile|0o644, 1000, 1000, false, 0); err != syscall.EROFS {
		t.Fatalf("create after shutdown: got %v, want EROFS", err)
	}
	if err := b.setXattrOp(context.Background(), f.InodeNumber, "user.x", []byte("v"), 0); err != syscall.EROFS {
		t.Fatalf("setxattr after shutdown: got %v, want EROFS", err)
	}
	if err := b.fslabelSetOp([]byte("nope")); err != syscall.EROFS {
		t.Fatalf("setfslabel after shutdown: got %v, want EROFS", err)
	}
	if _, err := b.fstrimOp(fsTrimRange{start: 0, length: 1 << 30}); err != syscall.EROFS {
		t.Fatalf("fstrim after shutdown: got %v, want EROFS", err)
	}

	n := &brieFSNode{bfs: b, ino: f.InodeNumber}
	if _, errno := n.Read(context.Background(), nil, make([]byte, 16), 0); errno != syscall.EIO {
		t.Fatalf("Read after shutdown: got %v, want EIO", errno)
	}
	if errno := n.Fsync(context.Background(), nil, 0); errno != syscall.EIO {
		t.Fatalf("Fsync after shutdown: got %v, want EIO", errno)
	}
}

// TestShutdownLogflushReplay is the generic/052 shape: create a file,
// commit its records without checkpointing, LOGFLUSH shutdown, then
// "unmount" the way the shutdown path does (no Checkpoint, no Close —
// fuse.go skips both). The next mount replays the live region and the
// file is back.
func TestShutdownLogflushReplay(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)
	f, err := b.createInDir(1, "keep", briefs.ModeFile|0o644, 1000, 1000, false, 0)
	if err != nil {
		t.Fatalf("create keep: %v", err)
	}
	data := makePattern(7, 4096)
	writeFile(t, b, f.InodeNumber, data, 0)
	if err := b.journal.Sync(false); err != nil {
		t.Fatalf("journal sync: %v", err)
	}
	// A live region exists: committed records beyond the last checkpoint.
	if start, end, _ := b.journal.ReplayLogRange(); start >= end {
		t.Fatalf("no live journal region after sync (start=%d end=%d)", start, end)
	}

	if err := b.shutdownOp(shutdownFlagLogFlush); err != nil {
		t.Fatalf("shutdownOp(LOGFLUSH): %v", err)
	}

	// The shutdown unmount: skip Checkpoint and Close, drain deferred
	// metadata, sync, close (the tail of fuse.go Mount).
	if err := b.flushDirtyMeta(); err != nil {
		t.Fatalf("flushDirtyMeta: %v", err)
	}
	if err := b.dev.Sync(); err != nil {
		t.Fatalf("dev sync: %v", err)
	}
	b.dev.Close()

	b2 := openBridge(t, img)
	if err := b2.replayJournal(); err != nil {
		t.Fatalf("replay: %v", err)
	}
	root, err := b2.inodes.ReadInode(1)
	if err != nil {
		t.Fatalf("read root: %v", err)
	}
	ino, _, err := TrieLookup(b2.dev, root.DirTrieRoot, "keep")
	if err != nil || ino == 0 {
		t.Fatalf("keep not found after LOGFLUSH-shutdown replay: ino=%d err=%v", ino, err)
	}
	if got := readFile(t, b2, ino, 0, int64(len(data))); !bytesEqual(got, data) {
		t.Fatal("keep content mismatch after replay")
	}
}

// TestShutdownNologflushSkipsFlush pins the NOLOGFLUSH half: shutdown
// without LOGFLUSH leaves the current journal block unpersisted. The
// observable is the replay range, not the mutation's visibility — the
// deferred-metadata drain the shutdown unmount still runs (the kernel's
// put_super runs flush_owned after a shutdown too, generic/417) can
// persist the mutation's metadata directly, kernel-parity, so "pending"
// being findable after the remount is fine. What must NOT happen is the
// unflushed journal records reaching disk: the on-disk log_end stays
// where the last Sync left it, so the reopened journal's replay range is
// unchanged by the post-sync mutation.
func TestShutdownNologflushSkipsFlush(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)
	if _, err := b.createInDir(1, "durable", briefs.ModeFile|0o644, 1000, 1000, false, 0); err != nil {
		t.Fatalf("create durable: %v", err)
	}
	if err := b.journal.Sync(false); err != nil {
		t.Fatalf("journal sync: %v", err)
	}
	// The replay range the last Sync committed on disk.
	syncStart, syncEnd, _ := b.journal.ReplayLogRange()

	if _, err := b.createInDir(1, "pending", briefs.ModeFile|0o644, 1000, 1000, false, 0); err != nil {
		t.Fatalf("create pending: %v", err)
	}
	if !b.journal.Dirty() {
		t.Fatal("journal not dirty after second create")
	}

	if err := b.shutdownOp(shutdownFlagNoLogFlush); err != nil {
		t.Fatalf("shutdownOp(NOLOGFLUSH): %v", err)
	}
	// NOLOGFLUSH leaves the pending records uncommitted.
	if !b.journal.Dirty() {
		t.Fatal("NOLOGFLUSH synced the journal")
	}

	// The shutdown unmount: skip Checkpoint AND Close — Close would flush
	// the dirty current block, which NOLOGFLUSH semantics forbid. The
	// deferred-metadata drain and device sync still run.
	if err := b.flushDirtyMeta(); err != nil {
		t.Fatalf("flushDirtyMeta: %v", err)
	}
	if err := b.dev.Sync(); err != nil {
		t.Fatalf("dev sync: %v", err)
	}
	b.dev.Close()

	b2 := openBridge(t, img)
	start, end, _ := b2.journal.ReplayLogRange()
	if start != syncStart || end != syncEnd {
		t.Fatalf("replay range advanced past the last sync: got [%d,%d), want [%d,%d)",
			start, end, syncStart, syncEnd)
	}
	if err := b2.replayJournal(); err != nil {
		t.Fatalf("replay: %v", err)
	}
	root, err := b2.inodes.ReadInode(1)
	if err != nil {
		t.Fatalf("read root: %v", err)
	}
	if ino, _, err := TrieLookup(b2.dev, root.DirTrieRoot, "durable"); err != nil || ino == 0 {
		t.Fatalf("durable not found after replay: ino=%d err=%v", ino, err)
	}
}
