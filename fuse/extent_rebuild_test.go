package fuse

// Regression tests for the localized (leaf-diff) extent-index rebuild on the
// write path (rebuildExtentIndexWrite).  The full rebuild re-emitted the whole
// B+tree on every extent-adding WRITE — generic/074's fragmented fstest
// amplified a few GB of logical writes into 34.5+ GB of device writes and
// hung past every timeout.  These pin the three properties the optimization
// rests on:
//
//   - an append that only touches the last chunk REUSES the equal-content
//     prefix's leaf blocks (the amplification fix itself);
//   - an insert that shifts every chunk reuses NOTHING (the boundary rule:
//     a reused leaf's stored next_leaf must match its new successor);
//   - a committed localized rebuild survives a simulated kill -9: replay
//     walks reused blocks like any other, and the replaced blocks' frees
//     publish at the sync's commit point.

import (
	"context"
	"os/exec"
	"testing"

	"github.com/ctdk/briefs-utils/briefs"
)

// writeFragments writes one patterned block at each of the given logical
// blocks (one extent per block, holes between), fataling on error.
func writeFragments(t *testing.T, b *BrieFS, ino uint64, blocks []uint64, seed int) {
	t.Helper()
	bs := int64(b.blockSize)
	for i, ib := range blocks {
		writeFile(t, b, ino, makePattern(seed+i, int(bs)), int64(ib)*bs)
	}
}

// readFragment reads back the single-block fragment at @ib and compares it
// with makePattern(seed).
func readFragment(t *testing.T, b *BrieFS, ino uint64, ib, seed uint64) {
	t.Helper()
	bs := int64(b.blockSize)
	got := readFile(t, b, ino, int64(ib)*bs, bs)
	if !bytesEqual(got, makePattern(int(seed), int(bs))) {
		t.Fatalf("fragment at block %d: readback mismatch", ib)
	}
}

// leafBlocksOf returns the walked tree's leaf block numbers, ascending.
func leafBlocksOf(t *testing.T, b *BrieFS, ino uint64) []uint64 {
	t.Helper()
	in, err := b.inodes.ReadInode(ino)
	if err != nil {
		t.Fatalf("ReadInode: %v", err)
	}
	tree, err := b.collectExtentTree(in)
	if err != nil {
		t.Fatalf("collectExtentTree: %v", err)
	}
	if in.Flags&briefs.InodeFlagIndexed == 0 || len(tree.leaves) < 2 {
		t.Fatalf("expected an indexed inode with >=2 leaves, flags=0x%x leaves=%d",
			in.Flags, len(tree.leaves))
	}
	blocks := make([]uint64, len(tree.leaves))
	for i, lf := range tree.leaves {
		blocks[i] = lf.block
	}
	return blocks
}

// TestExtentRebuildAppendReusesPrefixLeaves: with a >=4-leaf fragmented tree,
// one more single-block append at the end changes only the last chunk.  The
// equal-content prefix's leaf blocks must be REUSED (all but the boundary leaf
// of the prefix), only the changed leaves re-emitted — the whole-prefix
// rewrite was the 074 hang.
func TestExtentRebuildAppendReusesPrefixLeaves(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 20000)
	b := openBridge(t, img)

	in, err := b.createInDir(1, "frag", briefs.ModeFile|0o644, 1000, 1000, false, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ino := in.InodeNumber

	// 400 single-block fragments (blocks 0,2,4,...) = 400 extents
	// = 4 leaves (126+126+126+22), the >=3-leaf precondition.
	var blocks []uint64
	for i := 0; i < 400; i++ {
		blocks = append(blocks, uint64(2*i))
	}
	writeFragments(t, b, ino, blocks, 1)
	oldLeaves := leafBlocksOf(t, b, ino)
	if len(oldLeaves) != 4 {
		t.Fatalf("setup: want 4 leaves, got %d", len(oldLeaves))
	}

	// Append one more fragment at the end: only chunk 3 grows (22->23).
	writeFragments(t, b, ino, []uint64{800}, 500)
	newLeaves := leafBlocksOf(t, b, ino)
	if len(newLeaves) != 4 {
		t.Fatalf("append: want 4 leaves, got %d", len(newLeaves))
	}

	// Equal prefix = chunks 0..2; its boundary leaf (chunk 2) is rebuilt,
	// so chunks 0 and 1 keep their blocks.
	if newLeaves[0] != oldLeaves[0] || newLeaves[1] != oldLeaves[1] {
		t.Fatalf("prefix leaves not reused: old %v new %v", oldLeaves, newLeaves)
	}
	// The boundary and appended-to leaves are fresh blocks.
	if newLeaves[2] == oldLeaves[2] || newLeaves[3] == oldLeaves[3] {
		t.Fatalf("changed leaves were reused in place: old %v new %v", oldLeaves, newLeaves)
	}

	// Every fragment reads back, through the reused prefix included.
	for i, ib := range blocks {
		readFragment(t, b, ino, ib, uint64(1+i))
	}
	readFragment(t, b, ino, 800, 500)

	fsckClean(t, b, img)
}

// TestExtentRebuildMiddleShiftReusesNothing: writing into an early hole
// inserts an extent into chunk 0 and shifts every later chunk, so the
// equal-content prefix is empty and NO leaf may be reused — a kept leaf whose
// successor changed would point the next_leaf chain at a freed block.
func TestExtentRebuildMiddleShiftReusesNothing(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 20000)
	b := openBridge(t, img)

	in, err := b.createInDir(1, "frag", briefs.ModeFile|0o644, 1000, 1000, false, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ino := in.InodeNumber

	var blocks []uint64
	for i := 0; i < 300; i++ {
		blocks = append(blocks, uint64(2*i))
	}
	writeFragments(t, b, ino, blocks, 1)
	oldLeaves := leafBlocksOf(t, b, ino)

	// Fill the first hole (block 1): extent inserted at position 1, every
	// chunk's content shifts.
	writeFragments(t, b, ino, []uint64{1}, 500)
	newLeaves := leafBlocksOf(t, b, ino)

	oldSet := make(map[uint64]bool)
	for _, blk := range oldLeaves {
		oldSet[blk] = true
	}
	for i, blk := range newLeaves {
		if oldSet[blk] {
			t.Fatalf("leaf %d reused despite full chunk shift (old %v new %v)", i, oldLeaves, newLeaves)
		}
	}

	// Extents grew by one: 300 -> 301 does not cross a 126 boundary here
	// (126+126+48 -> 126+126+49), same leaf count.
	for i, ib := range blocks {
		readFragment(t, b, ino, ib, uint64(1+i))
	}
	readFragment(t, b, ino, 1, 500)

	fsckClean(t, b, img)
}

// TestExtentRebuildMergeDoesNotCorruptCachedChunks: filling a hole between
// two fragments MERGES extents in place (insertExtentSorted's
// merge-with-left/right paths).  The cached tree the localized rebuild
// diffs against must not alias the list being mutated: without the
// write path's clone, the in-place merge would rewrite the cached
// chunk's tail to look like the NEW chunk, the prefix compare would
// falsely match it, and a reused leaf block would keep its pre-merge
// on-disk content under a root that describes the merged state.
func TestExtentRebuildMergeDoesNotCorruptCachedChunks(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 20000)
	b := openBridge(t, img)

	in, err := b.createInDir(1, "frag", briefs.ModeFile|0o644, 1000, 1000, false, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ino := in.InodeNumber

	// Fragments at blocks 0,2,4,...  (one extent each, holes between).
	var blocks []uint64
	for i := 0; i < 300; i++ {
		blocks = append(blocks, uint64(2*i))
	}
	writeFragments(t, b, ino, blocks, 1)
	oldLeaves := leafBlocksOf(t, b, ino)

	// Fill the hole at block 1: extent bridges 0 and 2 -> merged into the
	// left neighbor (insertExtentSorted merge-with-left), in place.
	writeFragments(t, b, ino, []uint64{1}, 500)

	// The merged-on-disk extent must read back as one contiguous run:
	// blocks 0,1,2 all mapped with data (no hole where the merge happened).
	readFragment(t, b, ino, 0, 1)
	readFragment(t, b, ino, 1, 500) // the hole fill itself
	readFragment(t, b, ino, 2, 2)

	// A second localized op on the same inode exercises the cached tree
	// built by the merge op: any corruption the in-place merge introduced
	// would surface as a wrong prefix reuse here.
	writeFragments(t, b, ino, []uint64{600}, 700)
	newLeaves := leafBlocksOf(t, b, ino)
	if newLeaves[0] == 0 {
		t.Fatal("unreachable")
	}
	for i, ib := range blocks {
		readFragment(t, b, ino, ib, uint64(1+i))
	}
	readFragment(t, b, ino, 1, 500)
	readFragment(t, b, ino, 600, 700)
	_ = oldLeaves

	fsckClean(t, b, img)
}

// TestExtentTreeCacheInvalidatedByPunch: a punch goes through the full
// rebuild, which must drop the cached walked tree — a stale entry would
// validate against nothing (the root moved) but also must not survive to
// feed a bogus prefix compare on the next write.
func TestExtentTreeCacheInvalidatedByPunch(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 20000)
	b := openBridge(t, img)

	in, err := b.createInDir(1, "frag", briefs.ModeFile|0o644, 1000, 1000, false, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ino := in.InodeNumber

	var blocks []uint64
	for i := 0; i < 300; i++ {
		blocks = append(blocks, uint64(2*i))
	}
	writeFragments(t, b, ino, blocks, 1)

	// Punch a hole over fragments [10, 290): those extents drop out via
	// the full rebuild (extent_ops), invalidating the cache entry.
	bs := int64(b.blockSize)
	if err := b.fallocateOp(context.Background(), ino, uint64(10*bs), uint64(280*bs), fallocPunchHole|fallocKeepSize); err != nil {
		t.Fatalf("punch: %v", err)
	}

	// The next write rebuilds locally against a FRESH walk.  A stale
	// cached entry would compare against the pre-punch chunks and reuse
	// freed leaf blocks whose extent lists include the punched fragments.
	writeFragments(t, b, ino, []uint64{400}, 700)

	// Punched blocks read as zeros; survivors keep their data.
	got := readFile(t, b, ino, 10*bs, 2*bs)
	for i := range got {
		if got[i] != 0 {
			t.Fatalf("punched fragment at block %d: byte %d not zero", 10+i/4096, i%4096)
		}
	}
	for _, ib := range []uint64{0, 2, 4, 8} {
		readFragment(t, b, ino, ib, 1+ib/2)
	}
	readFragment(t, b, ino, 400, 700)

	fsckClean(t, b, img)
}

// TestExtentRebuildCrashReplayReusedBlocks: a committed localized rebuild
// (with reused prefix leaves) must survive a simulated kill -9 — the
// INODE_FULL-published root resolves through the reused blocks, the fresh
// blocks were drained pre-commit, and the replaced leaves' frees published
// at the sync's commit point (fsck: bitmap vs references).
func TestExtentRebuildCrashReplayReusedBlocks(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 20000)
	b := openBridge(t, img)

	in, err := b.createInDir(1, "frag", briefs.ModeFile|0o644, 1000, 1000, false, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ino := in.InodeNumber

	var blocks []uint64
	for i := 0; i < 400; i++ {
		blocks = append(blocks, uint64(2*i))
	}
	writeFragments(t, b, ino, blocks, 1)
	writeFragments(t, b, ino, []uint64{800}, 500) // localized rebuild, prefix reused
	reused := leafBlocksOf(t, b, ino)[0]

	// Commit without checkpointing: the sync publishes the records, drains
	// the deferred metadata, and applies the deferred frees.
	if err := b.journal.Sync(false); err != nil {
		t.Fatalf("journal sync: %v", err)
	}

	// "Crash": close WITHOUT the unmount checkpoint.
	b.dev.Close()

	fsck := buildBinary(t, "github.com/ctdk/briefs-utils/cmd/fsck", "fsck.briefs")
	out, err := exec.Command(fsck, img).CombinedOutput()
	if err != nil {
		t.Fatalf("fsck after crash: %v\n%s", err, out)
	}
	if !contains(string(out), "FSCK COMPLETE: no errors found") {
		t.Fatalf("fsck not clean after crash:\n%s", out)
	}

	// Fresh mount replays the committed records and reads back through the
	// reused leaf blocks.
	b2 := openBridge(t, img)
	for i, ib := range blocks {
		readFragment(t, b2, ino, ib, uint64(1+i))
	}
	readFragment(t, b2, ino, 800, 500)
	if got := leafBlocksOf(t, b2, ino)[0]; got != reused {
		t.Fatalf("replayed tree's first leaf moved: was %d now %d", reused, got)
	}
	_ = b2.journal.Checkpoint()
	_ = b2.dev.Sync()
}
