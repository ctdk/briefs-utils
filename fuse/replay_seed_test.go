package fuse

// Regression tests for the two prongs of the kernel's generic/475 replay fix
// (journal.c replay_trie_blocks + trie.c briefs_trie_seed_pool), ported for
// the generic/073-family replay ENOSPC: a crash-replay of a journal window
// on a FULL filesystem must not need a single fresh data block beyond the
// ones the window's own JRN_TRIE_ALLOC records named.
//
// Both tests fill the data region completely, commit with Sync(false), and
// crash without the checkpoint.  Because the bridge's commit drains deferred
// metadata to disk, a clean kill -9 leaves the on-disk trie containing every
// committed effect and replay's dir-adds short-circuit with EEXIST — the real
// re-derivation work only happens when the drain is dropped mid-flight
// (dm-flakey/generic/073), so each test simulates the drop: after the commit,
// it rewrites the trie pages on disk back to their pre-window state (seeding
// test) or zeroes them (block-pool test) while leaving the journal intact.
// Each fails deterministically without its prong:
//
//   - seeding (trieSeedPool): the window's inserts must reuse free slots on
//     the checkpointed trie's partial pages where the live path did.  The
//     window journals no JRN_TRIE_ALLOC (live reused partial pages), so the
//     replay block pool cannot help; without seeding the first insert takes
//     the fresh-page branch and ENOSPCs on the zero-free region.
//   - the replay block pool (replayTrieBlocks): the window's own page-inits
//     must pop the blocks pass 1 collected from the JRN_TRIE_ALLOC records
//     instead of re-allocating (the recorded blocks are pass-1-reserved and
//     invisible to AllocBlock's free scan).  Here the checkpointed trie is
//     completely full — there is nothing to seed — so without the pool every
//     re-derivation page-init ENOSPCs on the zero-free region.

import (
	"context"
	"fmt"
	"math/bits"
	"strings"
	"syscall"
	"testing"

	"github.com/ctdk/briefs-utils/briefs"
)

// fillDataRegion writes big chunks into @ino until the data allocator
// refuses, and requires the region to end completely full (the precondition
// that makes any fresh re-derivation allocation fail).  The chunks are large
// so the fill journals few records: a ring-pressure checkpoint mid-window
// would advance log_start and trim the record set these tests replay.
func fillDataRegion(t *testing.T, b *BrieFS, ino uint64) {
	t.Helper()
	chunk := makePattern(1, 512*1024)
	filled := 0
	for i := 0; i < 2000; i++ {
		if _, err := b.writeFileData(context.Background(), ino, chunk, int64(i)*int64(len(chunk))); err != nil {
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
	// The last full-chunk write ENOSPC'd whole, leaving a tail of free
	// blocks; one exactly-sized write fills it (a 4K-at-a-time top-up would
	// journal hundreds of extra records and risk a ring-pressure checkpoint
	// that trims the window these tests replay).
	free := b.dataAlloc.FreeCountData()
	if free > 0 {
		if free*4096 > 512*1024 {
			t.Fatalf("tail fill left %d blocks free — more than one chunk", free)
		}
		if _, err := b.writeFileData(context.Background(), ino, makePattern(2, int(free)*4096),
			int64(filled)*int64(len(chunk))); err != nil {
			t.Fatalf("tail fill write (%d blocks): %v", free, err)
		}
	}
	if free = b.dataAlloc.FreeCountData(); free != 0 {
		t.Fatalf("fill left %d data blocks unallocated", free)
	}
}

// captureTriePages reads every trie page reachable from rootRef.  The walk is
// read-only and cycle-safe (briefs.TrieWalker).
func captureTriePages(t *testing.T, b *BrieFS, rootRef uint64) map[uint64][]byte {
	t.Helper()
	pages := make(map[uint64][]byte)
	if briefs.TrieRefIsNull(rootRef) {
		return pages
	}
	w := briefs.NewTrieWalker(b.dev.ReadBlock, rootRef)
	for {
		ref, _, buf, _, _, ok := w.Next()
		if !ok {
			return pages
		}
		blk := briefs.TrieRefBlock(ref)
		if _, done := pages[blk]; !done {
			saved := make([]byte, len(buf))
			copy(saved, buf)
			pages[blk] = saved
		}
	}
}

// TestReplayTriePoolSeedingFullFs pins the seeding prong: the window starts
// from a CHECKPOINTED trie that still has a partial page (short names leave
// slots and name-heap room), and every window insert fits on it — the live
// path needed no fresh page, so the journal records no JRN_TRIE_ALLOC and the
// replay block pool cannot help.  Re-derivation must reuse the checkpointed
// partial page (trieSeedPool) or ENOSPC on the full fs.
func TestReplayTriePoolSeedingFullFs(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 1500)
	b := openBridge(t, img)

	// Phase A: one trie page with free slots and name-heap room, then a
	// checkpoint — the replay window starts from this on-disk trie.
	for i := 0; i < 15; i++ {
		if _, err := b.createInDir(1, fmt.Sprintf("a%02d", i),
			briefs.ModeFile|0o644, 1000, 1000, false); err != nil {
			t.Fatalf("phase A create %d: %v", i, err)
		}
	}
	if err := b.journal.Checkpoint(); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	root, err := b.inodes.ReadInode(1)
	if err != nil {
		t.Fatalf("read root: %v", err)
	}
	checkpointPages := captureTriePages(t, b, root.DirTrieRoot)
	if len(checkpointPages) == 0 {
		t.Fatal("phase A built no trie pages to seed from")
	}

	// Phase B (the window): fill the data region completely, then two more
	// creates whose names fit on the checkpointed partial page — they
	// succeed live precisely because the partial-page pool knows that page.
	fin, err := b.createInDir(1, "bigfill", briefs.ModeFile|0o644, 1000, 1000, false)
	if err != nil {
		t.Fatalf("create bigfill: %v", err)
	}
	fillDataRegion(t, b, fin.InodeNumber)
	for _, name := range []string{"blast", "blown"} {
		if _, err := b.createInDir(1, name, briefs.ModeFile|0o644, 1000, 1000, false); err != nil {
			t.Fatalf("create %s on full fs (live reused a partial page): %v", name, err)
		}
	}

	// Commit, then drop the window's trie-page writes (dm-cut simulation):
	// rewrite the pages to their checkpoint contents while the journal —
	// committed by the same Sync — keeps the records.
	if err := b.journal.Sync(false); err != nil {
		t.Fatalf("journal sync: %v", err)
	}
	for blk, saved := range checkpointPages {
		if err := b.dev.WriteBlock(blk, saved); err != nil {
			t.Fatalf("restore trie page %d: %v", blk, err)
		}
	}
	b.dev.Close()

	// Crash-replay: re-derivation of the window's dir-adds must reuse the
	// checkpointed partial page (trieSeedPool) — without seeding the first
	// insert page-inits and ENOSPCs on the zero-free data region, failing
	// the mount (the generic/073 replay failure).
	b2 := openBridge(t, img)
	if err := b2.replayJournal(); err != nil {
		t.Fatalf("replay on full fs: %v", err)
	}
	di, err := b2.inodes.ReadInode(1)
	if err != nil {
		t.Fatalf("read root after replay: %v", err)
	}
	for _, name := range []string{"bigfill", "blast", "blown"} {
		if _, _, err := TrieLookup(b2.dev, di.DirTrieRoot, name); err != nil {
			t.Fatalf("post-replay lookup of %s: %v", name, err)
		}
	}
	_ = b2.journal.Checkpoint()
	_ = b2.dev.Sync()
}

// trieFreeSlotSum sums the free slots of every trie page reachable from
// rootRef, counting each page once.  allocNode takes a slot from any page
// with room, so the sum — not any single page — is what an insert consumes.
// The root page itself (the page holding the trie's root node, created by
// mkfs) is excluded: the live partial-page pool never learns it, so its
// free slots are invisible to the live insert path and can never be filled.
func trieFreeSlotSum(t *testing.T, b *BrieFS, rootRef uint64) int {
	t.Helper()
	if briefs.TrieRefIsNull(rootRef) {
		return 1 << 30 // empty trie: the root page still needs creating
	}
	rootBlk := briefs.TrieRefBlock(rootRef)
	seen := make(map[uint64]bool)
	w := briefs.NewTrieWalker(b.dev.ReadBlock, rootRef)
	sum := 0
	for {
		ref, _, _, pg, _, ok := w.Next()
		if !ok {
			return sum
		}
		blk := briefs.TrieRefBlock(ref)
		if seen[blk] || blk == rootBlk {
			continue
		}
		seen[blk] = true
		sum += bits.OnesCount64(pg.FreeSlots)
	}
}

// fillTriePages fills every trie page to zero free slots.  The partial-page
// pool drains as pages fill (full pages are dropped from it), so this leaves
// the pool empty and the next insert has no choice but to page-init a fresh
// page (journaling JRN_TRIE_ALLOC).  Long names fill several pages per
// create (~221 slots, distinct first bytes so their paths share nothing);
// 1-char names then top the final partial page off exactly (1 slot each) —
// the small mkfs image provisions few inodes, so the fill must not need
// hundreds of creates.
func fillTriePages(t *testing.T, b *BrieFS) {
	t.Helper()
	longTags := "ghjkmn" // first bytes; distinct from the window's 'A'..'E'
	for i := 0; i < len(longTags); i++ {
		root, err := b.inodes.ReadInode(1)
		if err != nil {
			t.Fatalf("read root: %v", err)
		}
		free := trieFreeSlotSum(t, b, root.DirTrieRoot)
		if free == 0 {
			return
		}
		if free > 62 {
			if _, err := b.createInDir(1, fmt.Sprintf("%c%s%02d", longTags[i], strings.Repeat("q", 218), i),
				briefs.ModeFile|0o644, 1000, 1000, false); err != nil {
				t.Fatalf("trie-fill long create: %v", err)
			}
			continue
		}
		// Top off: each 1-char name takes exactly one slot.
		for _, c := range "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ" {
			root, err := b.inodes.ReadInode(1)
			if err != nil {
				t.Fatalf("read root: %v", err)
			}
			if trieFreeSlotSum(t, b, root.DirTrieRoot) == 0 {
				return
			}
			if _, err := b.createInDir(1, string(c),
				briefs.ModeFile|0o644, 1000, 1000, false); err != nil {
				t.Fatalf("trie-fill create %q: %v", string(c), err)
			}
		}
	}
	t.Fatal("trie pages did not fill after 6 long + 62 single-char creates")
}

// TestReplayTrieBlockPoolFullFs pins the replay block pool.  The checkpointed
// trie is completely FULL — every page the live path can allocate from has
// zero free slots (the mkfs-created root page's 63 slots are live-invisible
// and exempt), so the window's live inserts could only page-init, journaling
// one JRN_TRIE_ALLOC per fresh page (long names with distinct first bytes
// walk disjoint ~220-node paths).  The window then refills the data region to
// zero free blocks, so pass 1's reservations leave the allocator empty:
// re-derivation must pop the recorded blocks (LIFO) for its page-inits, and
// without the pool the first page-init re-allocates and ENOSPCs — the
// generic/073 replay failure.
func TestReplayTrieBlockPoolFullFs(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 1500)
	b := openBridge(t, img)

	// Phase A: fill the trie, then a fill file, then the data region — every
	// trie page ends full, and the checkpoint makes that state durable.
	fin, err := b.createInDir(1, "bigfill", briefs.ModeFile|0o644, 1000, 1000, false)
	if err != nil {
		t.Fatalf("create bigfill: %v", err)
	}
	fillTriePages(t, b)
	fillDataRegion(t, b, fin.InodeNumber)
	if err := b.journal.Checkpoint(); err != nil {
		t.Fatalf("checkpoint 1: %v", err)
	}

	// Free the data region again, retiring the truncate's records with a
	// second checkpoint: the window must not contain extent-frees, whose
	// pass-2 application would hand the unpooled re-derivation free blocks.
	if err := b.truncateInode(context.Background(), fin.InodeNumber, 0); err != nil {
		t.Fatalf("truncate bigfill: %v", err)
	}
	if err := b.journal.Checkpoint(); err != nil {
		t.Fatalf("checkpoint 2: %v", err)
	}
	root, err := b.inodes.ReadInode(1)
	if err != nil {
		t.Fatalf("read root: %v", err)
	}
	checkpointPages := captureTriePages(t, b, root.DirTrieRoot)
	if len(checkpointPages) == 0 {
		t.Fatal("checkpointed trie has no pages")
	}
	rootBlk := briefs.TrieRefBlock(root.DirTrieRoot)
	for blk, saved := range checkpointPages {
		if blk == rootBlk {
			continue // mkfs-created root page: live-invisible free slots
		}
		pg, err := briefs.ReadTriePage(saved)
		if err != nil {
			t.Fatalf("parse checkpointed trie page %d: %v", blk, err)
		}
		if pg.FreeSlots != 0 {
			t.Fatalf("checkpointed trie page %d still has free slots — seeding prong would couple", blk)
		}
	}

	// The window: long-name creates that page-init (nothing to reuse), then
	// a refill that takes the data region back to zero free blocks.
	names := make([]string, 0, 5)
	for i := 0; i < 5; i++ {
		names = append(names, fmt.Sprintf("%c%s%03d", 'A'+i, strings.Repeat("z", 220), i))
	}
	for _, name := range names {
		if _, err := b.createInDir(1, name, briefs.ModeFile|0o644, 1000, 1000, false); err != nil {
			t.Fatalf("long-name create %q: %v", name, err)
		}
	}
	rfin, err := b.createInDir(1, "refill", briefs.ModeFile|0o644, 1000, 1000, false)
	if err != nil {
		t.Fatalf("create refill: %v", err)
	}
	fillDataRegion(t, b, rfin.InodeNumber)
	if free := b.dataAlloc.FreeCountData(); free != 0 {
		t.Fatalf("window left %d data blocks free — unpooled replay would not ENOSPC", free)
	}

	// Commit, then drop the window's trie-page writes (dm-cut simulation):
	// restore every checkpointed page to its checkpoint contents while the
	// journal — committed by the same Sync — keeps the records.  The pages
	// the window added stay on disk, orphaned (no live pointer reaches them
	// once their parents' edits are rolled back) — exactly the unsynced-tail
	// orphans the pool's LIFO pop is ordered to consume first.
	if err := b.journal.Sync(false); err != nil {
		t.Fatalf("journal sync: %v", err)
	}
	root, err = b.inodes.ReadInode(1)
	if err != nil {
		t.Fatalf("read root: %v", err)
	}
	pages := captureTriePages(t, b, root.DirTrieRoot)
	if len(pages) <= len(checkpointPages) {
		t.Fatal("window added no trie pages")
	}
	for blk, saved := range checkpointPages {
		if err := b.dev.WriteBlock(blk, saved); err != nil {
			t.Fatalf("restore trie page %d: %v", blk, err)
		}
	}
	b.dev.Close()

	// Crash-replay: re-derivation re-runs the page-inits, which must reuse
	// the blocks the JRN_TRIE_ALLOC records named (LIFO) instead of
	// re-allocating on the zero-free region.
	b2 := openBridge(t, img)
	if err := b2.replayJournal(); err != nil {
		t.Fatalf("replay on full fs: %v", err)
	}
	di, err := b2.inodes.ReadInode(1)
	if err != nil {
		t.Fatalf("read root after replay: %v", err)
	}
	for _, name := range names {
		if _, _, err := TrieLookup(b2.dev, di.DirTrieRoot, name); err != nil {
			t.Fatalf("post-replay lookup of long name %q: %v", name, err)
		}
	}
	_ = b2.journal.Checkpoint()
	_ = b2.dev.Sync()
}
