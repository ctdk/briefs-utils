package fuse

import (
	"context"
	"syscall"
	"testing"

	"github.com/ctdk/briefs-utils/briefs"
)

// TestMetaReserveSize pins metaReserveSize to the kernel's
// briefs_meta_reserve_size (alloc.c:877): ceil(n/126) leaves +
// ceil(leaves/253) internal nodes + 1.
func TestMetaReserveSize(t *testing.T) {
	cases := []struct {
		n    uint64
		want uint64
	}{
		{0, 0},
		{1, 3},
		{125, 3},
		{126, 3},     // one full leaf
		{127, 4},     // second leaf
		{252, 4},     // two leaves
		{31878, 255}, // 253 leaves, one idx node
		{31879, 257}, // 254 leaves, two idx nodes
	}
	for _, c := range cases {
		if got := metaReserveSize(c.n); got != c.want {
			t.Errorf("metaReserveSize(%d) = %d, want %d", c.n, got, c.want)
		}
	}
	// Monotonic non-decreasing in n.
	prev := uint64(0)
	for n := uint64(0); n <= 1000; n++ {
		r := metaReserveSize(n)
		if r < prev {
			t.Fatalf("metaReserveSize not monotonic at n=%d: %d < %d", n, r, prev)
		}
		prev = r
	}
}

// TestAllocatorShieldEnforcement verifies the data-vs-metadata split: data
// allocations stop at free_count == meta_shield while AllocBlockMeta draws
// from the full free count (kernel alloc.c:209-217/315-318), and the run
// allocator clamps its search the same way (alloc.c:365-371).
func TestAllocatorShieldEnforcement(t *testing.T) {
	const total = 10
	const shield = 3

	// Single-block path. Block 0 is the ENOSPC sentinel (mkfs reserves it
	// on real images), so consume it first: a bare AllocBlock returning 0
	// would be indistinguishable from ENOSPC.
	a := openTestAllocator(t, briefs.NewAllocBuilder(total))
	a.ReserveBlock(0)
	a.AdjustShield(shield)
	if a.Shield() != shield {
		t.Fatalf("Shield: want %d, got %d", shield, a.Shield())
	}
	if got := a.FreeCountData(); got != total-1-shield {
		t.Fatalf("FreeCountData: want %d, got %d", total-1-shield, got)
	}
	for i := 0; i < total-1-shield; i++ {
		if rel := a.AllocBlock(); rel == 0 {
			t.Fatalf("AllocBlock #%d: got 0 with data available", i)
		}
	}
	if rel := a.AllocBlock(); rel != 0 {
		t.Fatalf("AllocBlock over the shield: got %d, want 0", rel)
	}
	// Metadata allocation proceeds into the shielded blocks, then stops at
	// true exhaustion.
	for i := 0; i < shield; i++ {
		if rel := a.AllocBlockMeta(); rel == 0 {
			t.Fatalf("AllocBlockMeta #%d: got 0, want a shielded block", i)
		}
	}
	if rel := a.AllocBlockMeta(); rel != 0 {
		t.Fatalf("AllocBlockMeta at true exhaustion: got %d, want 0", rel)
	}
	if got := a.FreeCountData(); got != 0 {
		t.Fatalf("FreeCountData at exhaustion: want 0, got %d", got)
	}

	// Run path: a run is a data allocation, so it must fit within
	// free_count - shield.
	a2 := openTestAllocator(t, briefs.NewAllocBuilder(total))
	a2.AdjustShield(shield)
	if rel := a2.AllocBlocks(total - shield + 1); rel != 0 {
		t.Fatalf("AllocBlocks(%d) over the shield: got %d, want 0", total-shield+1, rel)
	}
	if rel := a2.AllocBlocks(total - shield); rel == 0 {
		t.Fatalf("AllocBlocks(%d) within the shield: got 0", total-shield)
	}
	if got := a2.FreeCountData(); got != 0 {
		t.Fatalf("FreeCountData after run: want 0, got %d", got)
	}
}

// TestMetaShieldConversionOnFullFs is the E5 acceptance test: after a
// tree-backed file's unwritten extents raise the shield and a second file
// fills the fs to free_count == meta_shield, a write converting an unwritten
// block must still succeed — the conversion needs no new data block (the
// block exists) and its B+tree rebuild draws nodes from the shielded blocks
// via AllocBlockMeta. Without the shield this write ENOSPCs on the full fs
// (the kernel succeeds).
func TestMetaShieldConversionOnFullFs(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 500)
	b := openBridge(t, img)

	pre, err := b.createInDir(1, "pre", briefs.ModeFile|0o644, 1000, 1000, false, 0)
	if err != nil {
		t.Fatalf("create pre: %v", err)
	}
	preIno := pre.InodeNumber
	scratch, err := b.createInDir(1, "scratch", briefs.ModeFile|0o644, 1000, 1000, false, 0)
	if err != nil {
		t.Fatalf("create scratch: %v", err)
	}

	// Nine unwritten blocks at even logical blocks, interleaved with the
	// scratch file's written blocks so the unwritten extents' physical
	// blocks are never contiguous (insertExtentSorted would merge
	// phys-adjacent same-flag extents). Nine extents force the file
	// tree-backed, so the conversion's rebuild must allocate B+tree nodes.
	blk4096 := make([]byte, 4096)
	const nUnwritten = 9
	for i := 0; i < nUnwritten; i++ {
		if err := b.fallocateOp(context.Background(), preIno, uint64(i)*2*4096, 4096, fallocKeepSize); err != nil {
			t.Fatalf("fallocate #%d: %v", i, err)
		}
		writeFile(t, b, scratch.InodeNumber, blk4096, int64(i)*4096)
	}
	di, err := b.inodes.ReadInode(preIno)
	if err != nil {
		t.Fatalf("read pre: %v", err)
	}
	if di.Flags&briefs.InodeFlagIndexed == 0 {
		t.Fatal("pre is not tree-backed after 9 unwritten extents")
	}
	// nUnwritten unwritten blocks -> shield = metaReserveSize(nUnwritten).
	if got, want := b.dataAlloc.Shield(), metaReserveSize(nUnwritten); got != want {
		t.Fatalf("shield after preallocate: got %d, want %d", got, want)
	}

	// Fill the fs with written data on a third file until ENOSPC. Data
	// allocations stop exactly at free_count == shield.
	fill, err := b.createInDir(1, "fill", briefs.ModeFile|0o644, 1000, 1000, false, 0)
	if err != nil {
		t.Fatalf("create fill: %v", err)
	}
	fillIno := fill.InodeNumber
	for off := int64(0); ; off += 4096 {
		if _, err := b.writeFileData(context.Background(), fillIno, blk4096, off); err != nil {
			if err != syscall.ENOSPC {
				t.Fatalf("fill write at %d: %v (want ENOSPC)", off, err)
			}
			break
		}
	}
	if got := b.dataAlloc.FreeCountData(); got != 0 {
		t.Fatalf("FreeCountData after fill: want 0, got %d", got)
	}
	if free, shield := b.dataAlloc.FreeCount(), b.dataAlloc.Shield(); free != shield {
		t.Fatalf("fill stopped at free=%d, shield=%d (want equal)", free, shield)
	}

	// The conversion write into the first unwritten block succeeds on the
	// full fs: the data block already exists and the rebuild's B+tree
	// nodes come from the shielded blocks.
	pat := makePattern(2, 4096)
	writeFile(t, b, preIno, pat, 0)
	got := readFile(t, b, preIno, 0, 4096)
	for i, v := range got {
		if v != pat[i] {
			t.Fatalf("converted block byte %d: want %d, got %d", i, pat[i], v)
		}
	}

	// The conversion shrank the unwritten region to nUnwritten-1 blocks;
	// the shield recomputed from the remaining extents.
	if got, want := b.dataAlloc.Shield(), metaReserveSize(nUnwritten-1); got != want {
		t.Fatalf("shield after conversion: got %d, want %d", got, want)
	}
}

// TestMetaShieldPunchRelease verifies the release side: punching out
// unwritten extents returns their share of the shield, and the freed space
// becomes data-allocatable again.
func TestMetaShieldPunchRelease(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 500)
	b := openBridge(t, img)

	in, err := b.createInDir(1, "p", briefs.ModeFile|0o644, 1000, 1000, false, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ino := in.InodeNumber

	// Three unwritten blocks -> shield = metaReserveSize(3).
	if err := b.fallocateOp(context.Background(), ino, 0, 3*4096, 0); err != nil {
		t.Fatalf("fallocate: %v", err)
	}
	shield := b.dataAlloc.Shield()
	if shield != metaReserveSize(3) {
		t.Fatalf("shield after preallocate: got %d, want %d", shield, metaReserveSize(3))
	}
	if free, data := b.dataAlloc.FreeCount(), b.dataAlloc.FreeCountData(); data != free-shield {
		t.Fatalf("FreeCountData after preallocate: got %d, want %d", data, free-shield)
	}

	// Punch the middle block: unwritten count drops to 2, the shield to
	// metaReserveSize(2), and the punched block returns to the data pool.
	if err := b.fallocateOp(context.Background(), ino, 4096, 4096, fallocPunchHole|fallocKeepSize); err != nil {
		t.Fatalf("punch: %v", err)
	}
	shield = b.dataAlloc.Shield()
	if shield != metaReserveSize(2) {
		t.Fatalf("shield after punch: got %d, want %d", shield, metaReserveSize(2))
	}
	if free, data := b.dataAlloc.FreeCount(), b.dataAlloc.FreeCountData(); data != free-shield {
		t.Fatalf("FreeCountData after punch: got %d, want %d", data, free-shield)
	}
}

// TestMetaShieldDropOnUnlink verifies freeInodeData drops a deleted file's
// reservation entirely.
func TestMetaShieldDropOnUnlink(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 500)
	b := openBridge(t, img)

	in, err := b.createInDir(1, "d", briefs.ModeFile|0o644, 1000, 1000, false, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ino := in.InodeNumber
	if err := b.fallocateOp(context.Background(), ino, 0, 2*4096, 0); err != nil {
		t.Fatalf("fallocate: %v", err)
	}
	if got := b.dataAlloc.Shield(); got != metaReserveSize(2) {
		t.Fatalf("shield after preallocate: got %d, want %d", got, metaReserveSize(2))
	}

	if err := b.unlinkInDir(1, "d", false); err != nil {
		t.Fatalf("unlink: %v", err)
	}
	if got := b.dataAlloc.Shield(); got != 0 {
		t.Fatalf("shield after unlink: got %d, want 0", got)
	}
}
