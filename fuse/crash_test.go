package fuse

import (
	"context"
	"os/exec"
	"testing"

	"github.com/ctdk/briefs-utils/briefs"
)

// TestCrashRecovery simulates a kill -9 of the FUSE bridge mid-workload by
// closing the device WITHOUT the unmount checkpoint.  Ops no longer sync
// per-op (kernel parity: the bridge defers metadata between journal syncs,
// like the kernel's pinned buffer heads), so the workload commits
// explicitly before the simulated crash -- Sync(false) persists the commit
// point and drains the deferred metadata without checkpointing, leaving
// exactly the state this test exercises: committed-but-uncheckpointed
// records that a kernel remount would replay.  This verifies the
// host-observable result: fsck clean and a fresh mount reads back the data.
//
// The kernel-mount replay itself (mounting the FUSE-written image with the
// BrieFS kernel module and confirming replay) needs the VM and is exercised
// via the xfstests harness; see HANDOFF.md.
func TestCrashRecovery(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)

	// A short workload: create + write + xattr + chmod.
	in, _ := b.createInDir(1, "c", briefs.ModeFile|0o644, 1000, 1000, false, 0)
	pat := makePattern(3, 4000)
	writeFile(t, b, in.InodeNumber, pat, 0)
	if err := b.setXattr(in.InodeNumber, "user.tag", []byte("v"), 0); err != nil {
		t.Fatalf("setxattr: %v", err)
	}
	if err := b.setattrOp(context.Background(), in.InodeNumber, setattrReq(fattrMode, withMode(briefs.ModeFile|0o640))); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	// Commit the workload without checkpointing (an unsynced buffered op
	// would legitimately vanish across a kill -9 -- kernel parity -- which
	// is not what this crash test is about).
	if err := b.journal.Sync(false); err != nil {
		t.Fatalf("journal sync: %v", err)
	}

	// "Crash": close WITHOUT the unmount checkpoint (the journal carries
	// uncheckpointed-but-committed records).
	b.dev.Close()

	// fsck on the crashed image: clean (each op committed a consistent state).
	fsck := buildBinary(t, "github.com/ctdk/briefs-utils/cmd/fsck", "fsck.briefs")
	out, err := exec.Command(fsck, img).CombinedOutput()
	if err != nil {
		t.Fatalf("fsck after crash: %v\n%s", err, out)
	}
	if !contains(string(out), "FSCK COMPLETE: no errors found") {
		t.Fatalf("fsck not clean after crash:\n%s", out)
	}

	// Fresh mount reads back the data + xattr + mode.
	b2 := openBridge(t, img)
	if got := readFile(t, b2, in.InodeNumber, 0, 4000); !bytesEqual(got, pat) {
		t.Fatalf("post-crash readback mismatch")
	}
	if v, _ := b2.getXattr(in.InodeNumber, "user.tag"); string(v) != "v" {
		t.Fatalf("post-crash xattr: want v, got %q", string(v))
	}
	di, _ := b2.inodes.ReadInode(in.InodeNumber)
	if di.Filemode&0o7777 != 0o640 {
		t.Fatalf("post-crash mode: want 0640, got %o", di.Filemode&0o7777)
	}
	_ = b2.journal.Checkpoint()
	_ = b2.dev.Sync()
}

// TestCrashFreshSlotWriteThroughIsolation pins the slot-granularity of
// writeThroughFreshInodeSlot: arming a fresh inode's slot must not publish
// sibling slots' uncommitted deferred state to the device.  The unlink below
// empties the root trie and defers the parent inode with DirTrieRoot=0 (and
// the trie page's free); the following create's fresh-slot write-through
// shares the parent's inode-table block.  A whole-block read-modify-write
// read through the dirty view and wrote the parent's uncommitted state along
// with the fresh slot, so this crash left the on-disk root pointing at no
// trie while the trie page stayed bitmap-allocated (fsck: "allocated but NOT
// referenced" on the slot-reuse crash test).  The kernel avoids this by
// construction: its buffer heads are per-slot, so arming one slot never
// writes another.
func TestCrashFreshSlotWriteThroughIsolation(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)

	a, err := b.createInDir(1, "a", briefs.ModeFile|0o644, 1000, 1000, false, 0)
	if err != nil {
		t.Fatalf("create a: %v", err)
	}
	writeFile(t, b, a.InodeNumber, makePattern(3, 2000), 0)
	// Unlink with NOTHING committed: the parent inode (trie emptied,
	// DirTrieRoot=0) and the trie page free sit in the deferred map.
	if err := b.unlinkInDir(1, "a", false); err != nil {
		t.Fatalf("unlink a: %v", err)
	}

	// The fresh "b" slot reuses "a"'s slot, in the parent's inode-table
	// block: the write-through must arm only the slot, not the parent's
	// deferred state.
	bIn, err := b.createInDir(1, "b", briefs.ModeFile|0o644, 1000, 1000, false, 0)
	if err != nil {
		t.Fatalf("create b: %v", err)
	}
	if bIn.InodeNumber != a.InodeNumber {
		t.Fatalf("slot not reused: b=%d a=%d", bIn.InodeNumber, a.InodeNumber)
	}
	writeFile(t, b, bIn.InodeNumber, makePattern(5, 3000), 0)

	// "Crash" with no journal sync at all: every op above never happened, so
	// the on-disk state must still be a consistent pristine filesystem
	// (plus benign armed-slot and data-block residue).
	b.dev.Close()

	fsck := buildBinary(t, "github.com/ctdk/briefs-utils/cmd/fsck", "fsck.briefs")
	if out, err := exec.Command(fsck, img).CombinedOutput(); err != nil {
		t.Fatalf("fsck after crash: %v\n%s", err, out)
	} else if !contains(string(out), "FSCK COMPLETE: no errors found") {
		t.Fatalf("fsck not clean after crash:\n%s", out)
	}
}
