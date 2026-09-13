package fuse

import (
	"fmt"
	"math/rand"
	"syscall"
	"testing"

	"github.com/ctdk/briefs-utils/briefs"
)

// TestTriePageCompactNames builds a page with live names and dead space (freed
// slots whose orphaned name bytes sit above the live names) and verifies the
// kernel-faithful compaction (trie_page.c:427): every live name stays readable
// from its new compact offset, FreeNameOff drops to the live total, and a fresh
// allocation that would have failed ENOSPC against the old high-water mark now
// succeeds.
// TestTriePageCompactNames builds a page with live names and dead space (freed
// slots whose orphaned name bytes sit above the live names) and verifies the
// kernel-faithful compaction (trie_page.c:427): every live name stays readable
// from its new compact offset, FreeNameOff drops to the live total, and a fresh
// allocation that would have failed ENOSPC against the old high-water mark now
// succeeds.
func TestTriePageCompactNames(t *testing.T) {
	buf := make([]byte, 4096)
	makeTriePageRoot(buf)
	pg, err := briefs.ReadTriePage(buf)
	if err != nil {
		t.Fatalf("ReadTriePage: %v", err)
	}

	// Slot 0 is the root INTERM; fill slots 1..7 with large leaf names so
	// the heap's high-water mark approaches its 1772-byte capacity and the
	// dead space reclaimed below is big enough to matter.
	pad := func(b byte) string {
		s := make([]byte, 250)
		for i := range s {
			s[i] = 'a' + (b % 26) + byte(i%13) // deterministic distinct filler
		}
		s[0] = b
		return string(s)
	}
	names := []string{pad('b'), pad('c'), pad('d'), pad('e'), pad('f'), pad('g'), pad('h')}
	for i, name := range names {
		slot := uint(i + 1)
		writeTrieLeaf(buf, slot, uint64(i+10), name)
		// writeTrieLeaf wrote at nameOff=len+2 without bumping the page
		// high-water mark or slot bitmap; fix both up like the allocator
		// would.
		s, err := briefs.ReadTrieSlot(buf, slot)
		if err != nil {
			t.Fatalf("ReadTrieSlot %d: %v", slot, err)
		}
		if s.NameOffset != pg.FreeNameOff+uint16(len(name)+2) {
			// writeTrieLeaf always writes at offset len+2 (from the
			// block end); emulate sequential heap growth by
			// rewriting the name at the current high-water mark.
			newOff := pg.FreeNameOff + uint16(len(name)+2)
			if _, err := briefs.WriteTrieName(buf, newOff, name); err != nil {
				t.Fatalf("WriteTrieName: %v", err)
			}
			s.NameOffset = newOff
			putSlot(buf, slot, s)
		}
		pg.FreeNameOff = s.NameOffset
		pg.LiveCount++
		pg.FreeSlots &^= 1 << slot
		putPage(buf, pg)
	}

	// Orphan three slots: free the slot and its name fields, but leave the
	// name bytes in place above the live names (what trieFreeNode does —
	// the bytes are the dead space compaction must reclaim).
	for _, slot := range []uint{1, 3, 5} {
		pg.FreeSlots |= 1 << slot
		pg.LiveCount--
		putSlot(buf, slot, &briefs.TrieSlot{})
	}
	putPage(buf, pg)

	live := map[uint]string{2: names[1], 4: names[3], 6: names[5], 7: names[6]}
	oldOff := pg.FreeNameOff
	heapCap := uint64(4096) - uint64(triePageDataEnd())
	if oldOff == 0 || uint64(oldOff) >= heapCap {
		t.Fatalf("bad fixture: FreeNameOff=%d, heap cap %d", oldOff, heapCap)
	}

	// Sanity: a name as large as the reclaimed dead space cannot fit
	// against the old high-water mark but the live names still read fine.
	dead := uint16(3 * 252)
	if uint64(oldOff)+uint64(dead) <= heapCap {
		t.Fatalf("fixture too small: dead space %d fits without compaction", dead)
	}

	// Compact.
	if !triePageCompactNames(buf, pg) {
		t.Fatal("triePageCompactNames reported no compaction on a page with dead space")
	}
	wantLiveTotal := uint16(4 * 252)
	if pg.FreeNameOff != wantLiveTotal {
		t.Errorf("FreeNameOff after compaction: want %d, got %d", wantLiveTotal, pg.FreeNameOff)
	}
	for slot, name := range live {
		s, err := briefs.ReadTrieSlot(buf, slot)
		if err != nil {
			t.Fatalf("ReadTrieSlot %d: %v", slot, err)
		}
		got, err := briefs.ReadTrieName(buf, s.NameLen, s.NameOffset)
		if err != nil {
			t.Fatalf("ReadTrieName slot %d: %v", slot, err)
		}
		if got != name {
			t.Errorf("slot %d name after compaction: want %q, got %q", slot, name, got)
		}
	}

	// The reclaimed space now fits an allocation that could not before.
	if uint64(pg.FreeNameOff)+uint64(dead) > heapCap {
		t.Errorf("reclaimed heap still cannot fit %d bytes: FreeNameOff=%d cap=%d",
			dead, pg.FreeNameOff, heapCap)
	}
}

// TestTrieNameHeapChurnNoSpuriousENOSPC reproduces the generic/007 mechanism:
// a create/remove loop over a fixed name set churns the per-page name heap,
// and freed nodes orphan their name bytes above the live names (the heap's
// high-water mark never shrinks).  Without lazy compaction the re-leafing
// stores (trieStoreName on re-allocated INTERM nodes) eventually fail with a
// spurious ENOSPC while the heap is mostly dead space.  Ported from the
// kernel's generic/089 fix (trie_page_alloc_name compaction).
func TestTrieNameHeapChurnNoSpuriousENOSPC(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)

	const rootIno = 1
	const iters = 50000
	// Prefix-shaped name families ("c.3" is a prefix of "c.3m"): a short
	// name inserted while a longer sibling exists re-leafs an existing
	// nameless INTERM node (the middle-byte node the longer name created),
	// whose trieStoreName needs fresh heap space on that node's page.  The
	// node pool scan admits full-heap pages for nameless nodes
	// (triePageHasNameHeap passes for size 0), so the target page can be
	// full of dead space -- the generic/007 nametest ENOSPC mechanism.  The
	// pick order is randomized (fixed seed) so longer names are sometimes
	// created before their shorter prefixes, which is what leaves the
	// middle node nameless for the prefix insert to re-leaf.
	names := make([]string, 0, 32)
	for n := 0; n < 8; n++ {
		for m := 0; m < 3; m++ {
			names = append(names, fmt.Sprintf("c.%d%d", n, m))
		}
		names = append(names, fmt.Sprintf("c.%d", n))
	}
	rng := rand.New(rand.NewSource(1))

	exists := make(map[string]bool)
	for i := 0; i < iters; i++ {
		name := names[rng.Intn(len(names))]
		if exists[name] {
			if err := b.unlinkInDir(rootIno, name, false); err != nil {
				t.Fatalf("iter %d unlink %q: %v", i, name, err)
			}
			exists[name] = false
		} else {
			_, err := b.createInDir(rootIno, name, briefs.ModeFile|0o644, 1000, 1000, false, 0)
			if err == syscall.ENOSPC {
				t.Fatalf("iter %d create %q: spurious ENOSPC (name-heap dead space not compacted)", i, name)
			}
			if err != nil {
				t.Fatalf("iter %d create %q: %v", i, name, err)
			}
			exists[name] = true
		}
	}

	// Every name is findable exactly when the loop left it in place, and
	// the trie holds at most the 32 live names.
	for _, name := range names {
		got := lookupEntry(t, b, rootIno, name)
		if (got != 0) != exists[name] {
			t.Errorf("lookup %q after churn: exists=%v, got ino %d", name, exists[name], got)
		}
	}
	if got := dirEntryCount(t, b, rootIno); got > len(names) {
		t.Fatalf("%d live entries, want <= %d (entries leaked)", got, len(names))
	}
}
