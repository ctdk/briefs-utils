package briefs

import (
	"fmt"
	"testing"
)

// trieFixture builds a small directory trie in memory and serves its
// pages through a map-backed TrieReadFunc. Pages are one block wide with
// TrieSlotsPerBlock slots, the same layout the on-disk trie uses.
type trieFixture struct {
	pages map[uint64][]byte
}

func newTrieFixture() *trieFixture {
	return &trieFixture{pages: make(map[uint64][]byte)}
}

// page returns (creating if needed) a blank trie page with a valid header.
func (f *trieFixture) page(block uint64) []byte {
	if buf, ok := f.pages[block]; ok {
		return buf
	}
	buf := make([]byte, 4096)
	hdr := &TriePage{Magic: MagicTriePage, Version: 1, FreeSlots: ^uint64(0)}
	if err := WriteTriePage(buf, hdr); err != nil {
		panic(err)
	}
	f.pages[block] = buf
	return buf
}

// put stores a node in a free slot of a block and returns its reference.
func (f *trieFixture) put(block uint64, s *TrieSlot) uint64 {
	buf := f.page(block)
	page, _ := ReadTriePage(buf)
	for slot := uint(0); slot < TrieSlotsPerBlock; slot++ {
		if page.FreeSlots&(1<<slot) == 0 {
			continue // occupied
		}
		if err := WriteTrieSlot(buf, slot, s); err != nil {
			panic(err)
		}
		page.LiveCount++
		page.FreeSlots &^= 1 << slot
		if err := WriteTriePage(buf, page); err != nil {
			panic(err)
		}
		return TrieMakeRef(block, slot)
	}
	panic(fmt.Sprintf("trie block %d has no free slot", block))
}

// get re-reads a stored slot (to update it after later puts made forward
// references to nodes allocated afterwards).
func (f *trieFixture) get(ref uint64) *TrieSlot {
	s, err := ReadTrieSlot(f.page(TrieRefBlock(ref)), TrieRefSlot(ref))
	if err != nil {
		panic(err)
	}
	return s
}

func (f *trieFixture) set(ref uint64, s *TrieSlot) {
	if err := WriteTrieSlot(f.page(TrieRefBlock(ref)), TrieRefSlot(ref), s); err != nil {
		panic(err)
	}
}

func (f *trieFixture) read(block uint64) ([]byte, error) {
	buf, ok := f.pages[block]
	if !ok {
		return nil, fmt.Errorf("block %d: no such page", block)
	}
	return buf, nil
}

// storeName writes a name into a page's name heap, which grows downward
// from the end of the block. heapUsed is how many bytes of that page's
// heap are already in use. Returns the (nameLen, nameOffset) pair the
// carrying leaf slot should be given.
func (f *trieFixture) storeName(block uint64, name string, heapUsed uint16) (uint16, uint16) {
	nameOffset := uint16(len(name)) + 2 + heapUsed
	nl, err := WriteTrieName(f.page(block), nameOffset, name)
	if err != nil {
		panic(err)
	}
	return nl, nameOffset
}

// buildTestTrie creates a two-level trie whose shape exercises every walk
// mechanic: a root INTERM with a two-node sibling chain below it — a pure
// leaf ("alpha") and a leaf that also branches ("beta", whose child leaf
// "beta!" lives on another page) — so the walk needs one leaf re-visit.
func buildTestTrie(t *testing.T) (f *trieFixture, root uint64) {
	f = newTrieFixture()

	// Block 2 holds the root and the branch nodes, block 3 a leaf page.
	betaBang := &TrieSlot{Inode: 30, FType: 8, NodeType: NodeStatusLeaf}
	nl, no := f.storeName(3, "beta!", 0)
	betaBang.NameLen, betaBang.NameOffset = nl, no
	betaBangRef := f.put(3, betaBang)

	beta := &TrieSlot{Inode: 20, FType: 8, NodeType: NodeStatusLeaf, FirstChild: betaBangRef}
	nl, no = f.storeName(2, "beta", 0)
	beta.NameLen, beta.NameOffset = nl, no
	betaRef := f.put(2, beta)

	alpha := &TrieSlot{Inode: 10, FType: 8, NodeType: NodeStatusLeaf}
	nl, no = f.storeName(2, "alpha", uint16(len("beta"))+2)
	alpha.NameLen, alpha.NameOffset = nl, no
	alphaRef := f.put(2, alpha)

	// Sibling chain: alpha -> beta.
	a := f.get(alphaRef)
	a.NextSibling = betaRef
	f.set(alphaRef, a)

	rootNode := &TrieSlot{NodeType: NodeTypeInterm, ByteVal: 0, Depth: 0, ChildCount: 2, FirstChild: alphaRef}
	root = f.put(2, rootNode)
	return f, root
}

// TestWalkTrieEntryOrder pins the walk mechanics: sibling chains are
// walked first-child-first, a leaf that also branches emits its entry
// before its children (the re-visit), and names come back with the right
// inodes.
func TestWalkTrieEntryOrder(t *testing.T) {
	f, root := buildTestTrie(t)

	want := []struct {
		name string
		ino  uint64
	}{
		{"alpha", 10},
		{"beta", 20},
		{"beta!", 30},
	}

	var order []string
	got := make(map[string]uint64)
	// Every node must surface exactly once to VisitNode except the
	// branching leaf, which surfaces twice (emitted re-visit).
	var visits int
	var leafRevisits int
	err := WalkTrie(f.read, root, TrieVisitor{
		VisitNode: func(ref uint64, emitted bool, buf []byte, page *TriePage, node *TrieSlot) error {
			visits++
			if emitted {
				leafRevisits++
			}
			return nil
		},
		VisitLeaf: func(ref uint64, buf []byte, node *TrieSlot) error {
			name, nerr := ReadTrieName(buf, node.NameLen, node.NameOffset)
			if nerr != nil {
				return nerr
			}
			got[name] = node.Inode
			order = append(order, name)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("WalkTrie: %v", err)
	}
	if visits != 5 || leafRevisits != 1 {
		t.Errorf("node visits = %d (revisits %d), want 5 visits with 1 leaf re-visit", visits, leafRevisits)
	}
	for i, w := range want {
		if i >= len(order) || order[i] != w.name {
			t.Fatalf("entry %d = %v, want %q (order: %v)", i, order, w.name, order)
		}
		if got[w.name] != w.ino {
			t.Errorf("entry %q: ino %d, want %d", w.name, got[w.name], w.ino)
		}
	}
	if len(order) != len(want) {
		t.Errorf("walk emitted %d entries, want %d", len(order), len(want))
	}
}

// TestWalkTrieLazyWalker pins that the underlying stepper surfaces nodes
// one at a time and can stop early — what the FUSE bridge's dirIsEmpty
// relies on.
func TestWalkTrieLazyWalker(t *testing.T) {
	f, root := buildTestTrie(t)

	w := NewTrieWalker(f.read, root)
	steps := 0
	for {
		ref, emitted, _, _, node, ok := w.Next()
		if !ok {
			break
		}
		steps++
		_ = ref
		_ = emitted
		_ = node
	}
	// root + alpha + beta + beta(revisit) + beta! = 5 steps, but stopping
	// after the first must have been possible without walking the rest.
	if steps != 5 {
		t.Errorf("full walk took %d steps, want 5", steps)
	}

	w2 := NewTrieWalker(f.read, root)
	_, _, buf, _, node, ok := w2.Next()
	if !ok {
		t.Fatal("first step returned ok=false")
	}
	if node.NodeType != NodeTypeInterm {
		t.Errorf("first node type = 0x%02x, want the root INTERM", node.NodeType)
	}
	if len(buf) != 4096 {
		t.Errorf("page buffer size = %d, want 4096", len(buf))
	}
	// One step must not have consumed the whole walk.
	if _, _, _, _, _, ok := w2.Next(); !ok {
		t.Fatal("second step returned ok=false; stepper is not lazy")
	}
}

// TestWalkTrieCycleDetection plants a first_child back-edge (beta's chain
// points back at the root) and checks the walk terminates both with the
// default silent skip and with a Note that aborts.
func TestWalkTrieCycleDetection(t *testing.T) {
	f, root := buildTestTrie(t)
	// The root's chain: find beta by walking alpha's NextSibling.
	alphaRef := f.get(root).FirstChild
	betaRef := f.get(alphaRef).NextSibling
	// Back-edge: beta's sibling chain points back to the root.
	b := f.get(betaRef)
	b.NextSibling = root
	f.set(betaRef, b)

	// Silent skip: walk still finishes and emits all three entries.
	err := WalkTrie(f.read, root, TrieVisitor{
		VisitLeaf: func(ref uint64, buf []byte, node *TrieSlot) error {
			return nil
		},
	})
	if err != nil {
		t.Fatalf("silent cycle walk: %v", err)
	}

	// Aborting Note: cycle surfaces as a TrieNoteCycle note and aborts.
	var sawCycle bool
	err = WalkTrie(f.read, root, TrieVisitor{
		Note: func(ref uint64, kind TrieWalkNote, nerr error) error {
			if kind == TrieNoteCycle {
				sawCycle = true
				return fmt.Errorf("stop")
			}
			return nil
		},
	})
	if err == nil || err.Error() != "stop" {
		t.Fatalf("aborting Note: err = %v, want %q", err, "stop")
	}
	if !sawCycle {
		t.Error("aborting Note ran without seeing the cycle")
	}
}

// TestWalkTrieSiblingCap plants a sibling chain longer than
// TrieSiblingMax by pointing each node's NextSibling at the next slot
// around a ring inside one page. The cap must end the chain; with an
// aborting Note the walk stops entirely.
func TestWalkTrieSiblingCap(t *testing.T) {
	f, root := buildTestTrie(t)

	// Build a long sibling chain under the root: slots of block 4 in a
	// ring, so the chain never runs out (the cap must cut it).
	b4 := f.page(4)
	page, _ := ReadTriePage(b4)
	page.FreeSlots = ^uint64(0)
	page.LiveCount = 0
	if err := WriteTriePage(b4, page); err != nil {
		t.Fatal(err)
	}
	var firstChild uint64
	for slot := uint(0); slot < 64; slot++ {
		next := TrieMakeRef(4, (slot+1)%64)
		s := &TrieSlot{NodeType: NodeTypeInterm, ByteVal: byte('a' + slot%26), NextSibling: next}
		if slot == 0 {
			firstChild = TrieMakeRef(4, 0)
		}
		if err := WriteTrieSlot(b4, slot, s); err != nil {
			t.Fatal(err)
		}
	}
	r := f.get(root)
	r.FirstChild = firstChild
	r.ChildCount = 0
	f.set(root, r)

	capped := false
	err := WalkTrie(f.read, root, TrieVisitor{
		Note: func(ref uint64, kind TrieWalkNote, nerr error) error {
			if kind == TrieNoteSiblingCap {
				capped = true
			}
			return nil // keep the gathered chain, continue
		},
	})
	if err != nil {
		t.Fatalf("capped walk: %v", err)
	}
	if !capped {
		t.Error("sibling cap never fired")
	}

	// With an aborting Note the walk stops.
	err = WalkTrie(f.read, root, TrieVisitor{
		Note: func(ref uint64, kind TrieWalkNote, nerr error) error {
			if kind == TrieNoteSiblingCap {
				return nerr
			}
			return nil
		},
	})
	if err == nil {
		t.Fatal("aborting cap Note did not abort the walk")
	}
}

// TestWalkTrieBadPage checks the note kinds for unreadable/corrupt pages:
// a missing block surfaces as TrieNoteRead, a bad-magic page as
// TrieNotePage, and by default both are skipped without aborting.
func TestWalkTrieBadPage(t *testing.T) {
	f, root := buildTestTrie(t)

	// Point the root's only child at an unwritten block.
	r := f.get(root)
	r.FirstChild = TrieMakeRef(99, 0)
	f.set(root, r)

	var kinds []TrieWalkNote
	err := WalkTrie(f.read, root, TrieVisitor{
		Note: func(ref uint64, kind TrieWalkNote, nerr error) error {
			kinds = append(kinds, kind)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	// The failed read surfaces twice: once while following the root's
	// child chain (TrieNoteSiblingRead) and once when the pushed child
	// is visited (TrieNoteRead). Both are skipped, the walk continues.
	if len(kinds) != 2 || kinds[0] != TrieNoteSiblingRead || kinds[1] != TrieNoteRead {
		t.Errorf("notes = %v, want [SiblingRead Read]", kinds)
	}

	// A page with a clobbered magic: the child surfaces as TrieNotePage.
	buf := f.page(5)
	buf[0] = 0
	r = f.get(root)
	r.FirstChild = TrieMakeRef(5, 0)
	f.set(root, r)

	kinds = nil
	err = WalkTrie(f.read, root, TrieVisitor{
		Note: func(ref uint64, kind TrieWalkNote, nerr error) error {
			kinds = append(kinds, kind)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(kinds) != 2 || kinds[0] != TrieNoteSiblingRead || kinds[1] != TrieNotePage {
		t.Errorf("notes = %v, want [SiblingRead Page]", kinds)
	}
}

// TestWalkTrieSkipNode checks the ErrSkipTrieNode sentinel: the node's
// leaf entry is dropped but the walk (including its children) continues.
func TestWalkTrieSkipNode(t *testing.T) {
	f, root := buildTestTrie(t)

	var emitted []string
	err := WalkTrie(f.read, root, TrieVisitor{
		VisitNode: func(ref uint64, emitted bool, buf []byte, page *TriePage, node *TrieSlot) error {
			if !emitted && TrieIsLeaf(node.NodeType) {
				name, _ := ReadTrieName(buf, node.NameLen, node.NameOffset)
				if name == "beta" {
					return ErrSkipTrieNode
				}
			}
			return nil
		},
		VisitLeaf: func(ref uint64, buf []byte, node *TrieSlot) error {
			name, nerr := ReadTrieName(buf, node.NameLen, node.NameOffset)
			if nerr != nil {
				return nerr
			}
			emitted = append(emitted, name)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	// beta is skipped, but its child beta! is still walked.
	if len(emitted) != 2 || emitted[0] != "alpha" || emitted[1] != "beta!" {
		t.Errorf("entries = %v, want [alpha beta!]", emitted)
	}
}

// TestWalkTrieNullRoot: a null root is an immediately finished walk.
func TestWalkTrieNullRoot(t *testing.T) {
	w := NewTrieWalker(newTrieFixture().read, 0)
	if _, _, _, _, _, ok := w.Next(); ok {
		t.Error("null root yielded a node")
	}
	err := WalkTrie(newTrieFixture().read, 0, TrieVisitor{})
	if err != nil {
		t.Errorf("WalkTrie(null root): %v", err)
	}
}
