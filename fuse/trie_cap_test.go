package fuse

import (
	"strings"
	"testing"

	"github.com/ctdk/briefs-utils/briefs"
)

// TestTrieIteratorDeepNames exercises the readdir walk on a trie whose DFS
// stack occupancy exceeds 256 entries: names diverging at every level of a
// 90-deep shared prefix leave 3 unvisited siblings per level (~270 stack
// entries).  The iterator's stack is dynamic, so every entry must be
// emitted; the old fixed [256] stack silently dropped entries here.
func TestTrieIteratorDeepNames(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 20000)
	b := openBridge(t, img)

	d, err := b.createInDir(1, "d", briefs.ModeDir|0o755, 0, 0, false, 0)
	if err != nil {
		t.Fatalf("mkdir d: %v", err)
	}

	names := []string{strings.Repeat("a", 90)}
	for i := 0; i < 90; i++ {
		for _, c := range []byte{'b', 'c', 'd'} {
			names = append(names, strings.Repeat("a", i)+string(c)+"z")
		}
	}

	di, err := b.inodes.ReadInode(d.InodeNumber)
	if err != nil {
		t.Fatalf("read dir inode: %v", err)
	}
	b.cacheBegin()
	for i, name := range names {
		if err := b.TrieInsert(di, name, uint64(i+10), 8); err != nil {
			b.cacheAbort()
			t.Fatalf("TrieInsert(%q): %v", name, err)
		}
	}
	if err := b.journal.Sync(false); err != nil {
		b.cacheAbort()
		t.Fatalf("journal sync: %v", err)
	}
	if err := b.flushCacheToDevice(); err != nil {
		t.Fatalf("flushCacheToDevice: %v", err)
	}

	iter := NewTrieIterator(b.dev, di.DirTrieRoot)
	got := make(map[string]uint64, len(names))
	for {
		ino, _, name, err := iter.Next()
		if err != nil {
			t.Fatalf("iterator: %v", err)
		}
		if ino == 0 {
			break
		}
		if _, dup := got[name]; dup {
			t.Fatalf("duplicate entry %q", name)
		}
		got[name] = ino
	}
	if len(got) != len(names) {
		t.Fatalf("readdir emitted %d entries, want %d (stack overflow drops entries)", len(got), len(names))
	}
	for i, name := range names {
		if got[name] != uint64(i+10) {
			t.Fatalf("entry %q: ino %d, want %d", name, got[name], i+10)
		}
	}
}

// TestTrieSiblingChainCap verifies the sibling-chain cap: a next_sibling
// back-edge must abort lookups with an error (not spin forever) and the
// readdir walk must terminate on the corrupt trie.
func TestTrieSiblingChainCap(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)

	d, err := b.createInDir(1, "d", briefs.ModeDir|0o755, 0, 0, false, 0)
	if err != nil {
		t.Fatalf("mkdir d: %v", err)
	}
	for _, name := range []string{"aa", "ab", "ac"} {
		if _, err := b.createInDir(d.InodeNumber, name, briefs.ModeFile|0o644, 0, 0, false, 0); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}

	// Corrupt: the 'a' interm node's first child is the "aa" leaf; point
	// its next_sibling at itself so the level-1 sibling chain loops.
	// The corruption is written directly to the device, so first push the
	// daemon's deferred metadata down (fix B keeps the just-created trie
	// pages in daemon memory between journal syncs; the dirty-view hook
	// would serve those copies and shadow the on-disk corruption).
	if err := b.flushDirtyMeta(); err != nil {
		t.Fatalf("flush deferred metadata: %v", err)
	}
	di, err := b.inodes.ReadInode(d.InodeNumber)
	if err != nil {
		t.Fatalf("read dir inode: %v", err)
	}
	_, rnode, err := trieReadNode(b.dev, di.DirTrieRoot)
	if err != nil {
		t.Fatalf("read root node: %v", err)
	}
	nodeA := rnode.FirstChild
	if briefs.TrieRefIsNull(nodeA) {
		t.Fatal("root has no children")
	}
	_, aNode, err := trieReadNode(b.dev, nodeA)
	if err != nil {
		t.Fatalf("read 'a' node: %v", err)
	}
	child := aNode.FirstChild
	if briefs.TrieRefIsNull(child) {
		t.Fatal("'a' node has no children")
	}
	cblock := briefs.TrieRefBlock(child)
	cslot := briefs.TrieRefSlot(child)
	buf, err := b.dev.ReadBlock(cblock)
	if err != nil {
		t.Fatalf("read child page: %v", err)
	}
	node, err := briefs.ReadTrieSlot(buf, cslot)
	if err != nil {
		t.Fatalf("read child slot: %v", err)
	}
	node.NextSibling = child // back-edge
	if err := briefs.WriteTrieSlot(buf, cslot, node); err != nil {
		t.Fatalf("write child slot: %v", err)
	}
	if err := b.dev.WriteBlock(cblock, buf); err != nil {
		t.Fatalf("write child page: %v", err)
	}

	// A lookup that must walk past the looping sibling aborts at the cap.
	_, _, err = TrieLookup(b.dev, di.DirTrieRoot, "ad")
	if err == nil {
		t.Fatal("lookup on cyclic sibling chain: want cap error, got nil")
	}
	if !strings.Contains(err.Error(), "exceeded") {
		t.Fatalf("lookup error: want sibling-walk cap error, got %v", err)
	}

	// The cached write-path walk aborts the same way.
	b.cacheBegin()
	_, err = b.trieFindChild(nodeA, 'd')
	b.cacheAbort()
	if err == nil || !strings.Contains(err.Error(), "exceeded") {
		t.Fatalf("write-path find: want cap error, got %v", err)
	}

	// The readdir walk terminates on the corrupt trie.
	iter := NewTrieIterator(b.dev, di.DirTrieRoot)
	emitted := 0
	for {
		ino, _, _, err := iter.Next()
		if err != nil {
			t.Fatalf("iterator on cyclic trie: %v", err)
		}
		if ino == 0 {
			break
		}
		emitted++
		if emitted > 4*len([]string{"aa", "ab", "ac"}) {
			t.Fatal("iterator did not terminate on cyclic trie")
		}
	}
}
