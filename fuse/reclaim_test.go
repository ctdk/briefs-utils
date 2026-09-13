package fuse

import (
	"context"
	"os/exec"
	"syscall"
	"testing"

	"github.com/ctdk/briefs-utils/briefs"
)

// TestDeferredFreeReclaimOnENOSPC pins the generic/275 shape.  Deferred frees
// (deferBlockFree) apply to the allocator's bitmap only when their journal
// records commit — the 040/041 reuse-before-commit guard.  The kernel's
// delete-then-write workloads survive that because kjournald commits every
// few seconds and sync(2) reaches the fs; a regular FUSE mount has neither
// (the 6.12 client wires SYNCFS only for fuseblk, inode.c:1742), so a write
// after a delete must reclaim on its own: the failed allocation scan calls
// the Allocator.reclaim hook (reclaimPendingFrees), which commits the
// journal — the free is applied by SyncMeta AFTER the commit point — and the
// retry scans again.  A block therefore never becomes reusable before its
// freeing record is durable.
func TestDeferredFreeReclaimOnENOSPC(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)

	// Fill the data region: write 4K chunks until the allocator refuses.
	a, err := b.createInDir(1, "a", briefs.ModeFile|0o644, 1000, 1000, false, 0)
	if err != nil {
		t.Fatalf("create a: %v", err)
	}
	chunk := makePattern(1, 4096)
	filled := 0
	for i := 0; i < 20000; i++ {
		if _, err := b.writeFileData(context.Background(), a.InodeNumber, chunk, int64(i)*4096); err != nil {
			if err != syscall.ENOSPC {
				t.Fatalf("fill write %d: %v", i, err)
			}
			break
		}
		filled++
	}
	if filled == 0 {
		t.Fatal("test image has no allocatable data space")
	}
	if free := b.dataAlloc.FreeCountData(); free != 0 {
		t.Fatalf("fill left %d data blocks unallocated", free)
	}

	// Truncate to zero WITHOUT any sync: every data block's free goes to
	// pendingFrees, invisible to the bitmap until its records commit.
	if err := b.setattrOp(context.Background(), a.InodeNumber, setattrReq(fattrSize, withSize(0))); err != nil {
		t.Fatalf("truncate a: %v", err)
	}
	pending := b.pendingFreeCount()
	if pending == 0 {
		t.Fatal("truncate deferred no block frees")
	}
	// df honesty: statvfs-style free must count the pending frees as
	// available, or generic/275's "Post rm space: 0 available" reports
	// exhaustion the fs does not actually have.
	if b.dataAlloc.FreeCountDataPlus(pending) == 0 {
		t.Fatal("statvfs free counts no pending frees")
	}

	// Delete-then-write: the allocation's first scan fails, the reclaim hook
	// commits the journal (applying a's frees after the commit point), and
	// the retry succeeds — no ENOSPC, no explicit sync anywhere.
	bIn, err := b.createInDir(1, "b", briefs.ModeFile|0o644, 1000, 1000, false, 0)
	if err != nil {
		t.Fatalf("create b: %v", err)
	}
	pat := makePattern(5, 8192)
	writeFile(t, b, bIn.InodeNumber, pat, 0)

	// The reclaiming sync's SyncMeta drained every pending free.
	if n := b.pendingFreeCount(); n != 0 {
		t.Fatalf("reclaim left %d pending frees undrained", n)
	}

	// Crash consistency: the reclaiming sync committed a's truncate BEFORE
	// b's writes reused the freed blocks (no unmount checkpoint below), so
	// the image replays clean with a empty — the pre-fix code could not
	// reach this point at all (the write ENOSPC'd).
	b.dev.Close()

	fsck := buildBinary(t, "github.com/ctdk/briefs-utils/cmd/fsck", "fsck.briefs")
	out, err := exec.Command(fsck, img).CombinedOutput()
	if err != nil {
		t.Fatalf("fsck after crash: %v\n%s", err, out)
	}
	if !contains(string(out), "FSCK COMPLETE: no errors found") {
		t.Fatalf("fsck not clean after crash:\n%s", out)
	}

	b2 := openBridge(t, img)
	di, err := b2.inodes.ReadInode(a.InodeNumber)
	if err != nil {
		t.Fatalf("read a after replay: %v", err)
	}
	if di.FileSize != 0 {
		t.Fatalf("a not truncated after replay: size %d", di.FileSize)
	}
	_ = b2.journal.Checkpoint()
	_ = b2.dev.Sync()
}
