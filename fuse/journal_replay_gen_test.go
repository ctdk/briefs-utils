package fuse

import (
	"os/exec"
	"testing"

	"github.com/ctdk/briefs-utils/briefs"
)

// Tests for the replay generation guards (kernel commit 33e4019, generic/536):
// a stale inode snapshot / inode-update record must not clobber a slot that
// has been freed and reallocated to a different generation.

// armInodeSlot persists a synthetic armed inode slot (magic INOD) so replay
// guard tests have controlled slot content.
func armInodeSlot(t *testing.T, b *BrieFS, in *briefs.Inode) {
	t.Helper()
	b.cacheBegin()
	if err := b.writeInodeCached(in); err != nil {
		t.Fatalf("writeInodeCached(%d): %v", in.InodeNumber, err)
	}
	if err := b.flushCache(); err != nil {
		t.Fatalf("flushCache: %v", err)
	}
}

// TestReplayInodeFullGenerationGuard covers all three replay_inode_full
// guards (kernel journal.c:1234): matching snapshot applies, stale
// generation skips, freed/empty slot skips.
func TestReplayInodeFullGenerationGuard(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)

	slot := &briefs.Inode{
		Magic:       briefs.MagicInode,
		InodeNumber: 2,
		Filemode:    briefs.ModeFile | 0o600,
		Nlinks:      1,
		FileSize:    111,
		Generation:  7,
	}
	armInodeSlot(t, b, slot)

	// Matching generation: the snapshot is applied.
	snap := *slot
	snap.FileSize = 222
	raw, err := snap.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	b.cacheBegin()
	if err := b.replayInodeFull(2, briefs.MarshalJrnInodeFull(2, raw)); err != nil {
		t.Fatalf("replayInodeFull(matching): %v", err)
	}
	if err := b.flushCache(); err != nil {
		t.Fatalf("flushCache: %v", err)
	}
	di, err := b.inodes.ReadInode(2)
	if err != nil || di == nil {
		t.Fatalf("read inode 2 after restore: %v", err)
	}
	if di.FileSize != 222 || di.Generation != 7 {
		t.Fatalf("matching snapshot not applied: size=%d gen=%d", di.FileSize, di.Generation)
	}

	// Stale generation: the snapshot is skipped (slot reused since).
	stale := *slot
	stale.FileSize = 333
	stale.Generation = 9
	rawStale, _ := stale.MarshalBinary()
	b.cacheBegin()
	if err := b.replayInodeFull(2, briefs.MarshalJrnInodeFull(2, rawStale)); err != nil {
		t.Fatalf("replayInodeFull(stale): %v", err)
	}
	if err := b.flushCache(); err != nil {
		t.Fatalf("flushCache: %v", err)
	}
	di, _ = b.inodes.ReadInode(2)
	if di == nil || di.FileSize != 222 || di.Generation != 7 {
		t.Fatalf("stale snapshot clobbered slot: %+v", di)
	}

	// Freed/empty slot: the snapshot is skipped (kernel -EINVAL path).  A
	// record for a freed inode must not resurrect it into the zeroed slot.
	b.cacheBegin()
	if err := b.zeroInodeCached(2); err != nil {
		t.Fatalf("zeroInodeCached: %v", err)
	}
	if err := b.flushCache(); err != nil {
		t.Fatalf("flushCache: %v", err)
	}
	rawSnap, _ := snap.MarshalBinary() // generation 7 == the slot's old gen
	b.cacheBegin()
	if err := b.replayInodeFull(2, briefs.MarshalJrnInodeFull(2, rawSnap)); err != nil {
		t.Fatalf("replayInodeFull(freed slot): %v", err)
	}
	if err := b.flushCache(); err != nil {
		t.Fatalf("flushCache: %v", err)
	}
	di, _ = b.inodes.ReadInode(2)
	if di != nil && di.Magic == briefs.MagicInode {
		t.Fatalf("snapshot resurrected a freed slot: %+v", di)
	}
}

// TestReplayInodeUpdateGenerationGuard covers replay_inode_update's
// length-gated generation guard (kernel journal.c:1042): 96-byte records
// carry the generation and skip when stale; legacy 88-byte records apply
// unguarded.
func TestReplayInodeUpdateGenerationGuard(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)

	slot := &briefs.Inode{
		Magic:       briefs.MagicInode,
		InodeNumber: 2,
		Filemode:    briefs.ModeFile | 0o600,
		Nlinks:      1,
		FileSize:    111,
		Generation:  7,
	}
	armInodeSlot(t, b, slot)

	// Matching generation: applied.
	ok := &briefs.JrnInodeUpdate{Ino: 2, Mode: slot.Filemode, Nlink: 1, FileSize: 222, Generation: 7}
	b.cacheBegin()
	if err := b.replayInodeUpdate(ok, true); err != nil {
		t.Fatalf("replayInodeUpdate(matching): %v", err)
	}
	if err := b.flushCache(); err != nil {
		t.Fatalf("flushCache: %v", err)
	}
	di, _ := b.inodes.ReadInode(2)
	if di == nil || di.FileSize != 222 {
		t.Fatalf("matching update not applied: %+v", di)
	}

	// Stale generation: skipped.
	stale := &briefs.JrnInodeUpdate{Ino: 2, Mode: slot.Filemode, Nlink: 1, FileSize: 333, Generation: 9}
	b.cacheBegin()
	if err := b.replayInodeUpdate(stale, true); err != nil {
		t.Fatalf("replayInodeUpdate(stale): %v", err)
	}
	if err := b.flushCache(); err != nil {
		t.Fatalf("flushCache: %v", err)
	}
	di, _ = b.inodes.ReadInode(2)
	if di == nil || di.FileSize != 222 {
		t.Fatalf("stale update clobbered slot: %+v", di)
	}

	// Legacy record (no generation field): applied unguarded.
	legacy := &briefs.JrnInodeUpdate{Ino: 2, Mode: slot.Filemode, Nlink: 2, FileSize: 444}
	b.cacheBegin()
	if err := b.replayInodeUpdate(legacy, false); err != nil {
		t.Fatalf("replayInodeUpdate(legacy): %v", err)
	}
	if err := b.flushCache(); err != nil {
		t.Fatalf("flushCache: %v", err)
	}
	di, _ = b.inodes.ReadInode(2)
	if di == nil || di.FileSize != 444 || di.Nlinks != 2 {
		t.Fatalf("legacy update not applied: %+v", di)
	}
}

// TestCrashSlotReuseReplay is the generic/536 scenario end to end: create a
// file, delete it, create a second file that reuses the freed slot, then
// "crash" (close without the unmount checkpoint) and replay.  The replay
// walks the stale first file's INODE_FULL/INODE_UPDATE records (which must
// be skipped by the generation guards) and the second file's records (which
// must apply — only possible because the allocation path armed the fresh
// slot in the page cache before committing its snapshot,
// writeThroughFreshInodeSlot).  Post-replay the second file must be intact.
func TestCrashSlotReuseReplay(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)

	a, err := b.createInDir(1, "a", briefs.ModeFile|0o644, 1000, 1000, false)
	if err != nil {
		t.Fatalf("create a: %v", err)
	}
	writeFile(t, b, a.InodeNumber, makePattern(3, 2000), 0)
	if err := b.unlinkInDir(1, "a", false); err != nil {
		t.Fatalf("unlink a: %v", err)
	}

	bIn, err := b.createInDir(1, "b", briefs.ModeFile|0o644, 1000, 1000, false)
	if err != nil {
		t.Fatalf("create b: %v", err)
	}
	if bIn.InodeNumber != a.InodeNumber {
		t.Fatalf("slot not reused: b=%d a=%d", bIn.InodeNumber, a.InodeNumber)
	}
	pat := makePattern(5, 3000)
	writeFile(t, b, bIn.InodeNumber, pat, 0)

	// "Crash": close WITHOUT the unmount checkpoint.
	b.dev.Close()

	// fsck on the crashed image must be clean.
	fsck := buildBinary(t, "github.com/ctdk/briefs-utils/cmd/fsck", "fsck.briefs")
	if out, err := exec.Command(fsck, img).CombinedOutput(); err != nil {
		t.Fatalf("fsck after crash: %v\n%s", err, out)
	} else if !contains(string(out), "FSCK COMPLETE: no errors found") {
		t.Fatalf("fsck not clean after crash:\n%s", out)
	}

	// Reopen and replay the live journal range.
	b2 := openBridge(t, img)
	if err := b2.replayJournal(); err != nil {
		t.Fatalf("replayJournal: %v", err)
	}

	// "b" intact: slot generation + size + data.
	di, err := b2.inodes.ReadInode(bIn.InodeNumber)
	if err != nil || di == nil {
		t.Fatalf("read inode %d after replay: %v", bIn.InodeNumber, err)
	}
	if di.Generation != bIn.Generation {
		t.Fatalf("slot clobbered by stale snapshot: gen=%d want %d", di.Generation, bIn.Generation)
	}
	if di.FileSize != 3000 {
		t.Fatalf("post-replay size: %d want 3000", di.FileSize)
	}
	if got := readFile(t, b2, bIn.InodeNumber, 0, 3000); !bytesEqual(got, pat) {
		t.Fatalf("post-replay readback mismatch")
	}

	// The re-derived trie references "b" at its slot.
	p1, _ := b2.inodes.ReadInode(1)
	if ino, _, _ := TrieLookup(b2.dev, p1.DirTrieRoot, "b"); ino != bIn.InodeNumber {
		t.Fatalf("post-replay trie: b -> %d, want %d", ino, bIn.InodeNumber)
	}

	_ = b2.journal.Checkpoint()
	_ = b2.dev.Sync()
}
