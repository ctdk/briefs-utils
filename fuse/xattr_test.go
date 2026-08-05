package fuse

import (
	"syscall"
	"testing"

	"github.com/ctdk/briefs-utils/briefs"
)

// newXattrNode builds a minimal brieFSNode for handler-level xattr tests
// (only bfs + ino are used; the embedded fs.Inode is left zero).
func newXattrNode(b *BrieFS, ino uint64) *brieFSNode {
	return &brieFSNode{bfs: b, ino: ino}
}

func TestXattrSetGetListRemove(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)

	in, err := b.createInDir(1, "x", briefs.ModeFile|0o644, 1000, 1000, false)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ino := in.InodeNumber
	n := newXattrNode(b, ino)

	// --- set/get across the three namespaces ---
	xattrs := []struct {
		name, value string
	}{
		{"user.foo", "hello"},
		{"trusted.bar", "world"},
		{"security.baz", "lockdown"},
	}
	for _, x := range xattrs {
		if err := b.setXattr(ino, x.name, []byte(x.value), 0); err != nil {
			t.Fatalf("setxattr %q: %v", x.name, err)
		}
	}
	for _, x := range xattrs {
		got, err := b.getXattr(ino, x.name)
		if err != nil {
			t.Fatalf("getxattr %q: %v", x.name, err)
		}
		if string(got) != x.value {
			t.Fatalf("getxattr %q: want %q, got %q", x.name, x.value, string(got))
		}
	}

	// --- list ---
	names, err := b.listXattr(ino)
	if err != nil {
		t.Fatalf("listxattr: %v", err)
	}
	if len(names) != len(xattrs) {
		t.Fatalf("listxattr count: want %d, got %d (%v)", len(xattrs), len(names), names)
	}
	got := map[string]bool{}
	for _, nm := range names {
		got[nm] = true
	}
	for _, x := range xattrs {
		if !got[x.name] {
			t.Fatalf("listxattr missing %q (got %v)", x.name, names)
		}
	}

	// --- get absent -> ENODATA ---
	if _, err := b.getXattr(ino, "user.missing"); err != syscall.ENODATA {
		t.Fatalf("getxattr missing: want ENODATA, got %v", err)
	}

	// --- replace ---
	if err := b.setXattr(ino, "user.foo", []byte("updated"), 0); err != nil {
		t.Fatalf("setxattr replace: %v", err)
	}
	if got, _ := b.getXattr(ino, "user.foo"); string(got) != "updated" {
		t.Fatalf("replace readback: want updated, got %q", string(got))
	}

	// --- XATTR_CREATE on existing -> EEXIST ---
	if err := b.setXattr(ino, "user.foo", []byte("x"), 0x1); err != syscall.EEXIST {
		t.Fatalf("XATTR_CREATE on existing: want EEXIST, got %v", err)
	}
	// --- XATTR_REPLACE on absent -> ENODATA ---
	if err := b.setXattr(ino, "user.new", []byte("x"), 0x2); err != syscall.ENODATA {
		t.Fatalf("XATTR_REPLACE absent: want ENODATA, got %v", err)
	}

	// --- handler-level size query + ERANGE ---
	if sz, errno := n.Getxattr(nil, "user.foo", nil); errno != 0 || sz != uint32(len("updated")) {
		t.Fatalf("Getxattr size query: sz=%d errno=%v", sz, errno)
	}
	small := make([]byte, 2)
	if _, errno := n.Getxattr(nil, "user.foo", small); errno != syscall.ERANGE {
		t.Fatalf("Getxattr ERANGE: want ERANGE, got %v", errno)
	}
	// listxattr size query
	if sz, errno := n.Listxattr(nil, nil); errno != 0 || sz == 0 {
		t.Fatalf("Listxattr size query: sz=%d errno=%v", sz, errno)
	}

	// --- remove ---
	if err := b.removeXattr(ino, "trusted.bar"); err != nil {
		t.Fatalf("removexattr: %v", err)
	}
	names, _ = b.listXattr(ino)
	if len(names) != len(xattrs)-1 {
		t.Fatalf("listxattr after remove: want %d, got %d", len(xattrs)-1, len(names))
	}
	// remove absent -> ENODATA.
	if err := b.removeXattr(ino, "trusted.bar"); err != syscall.ENODATA {
		t.Fatalf("removexattr absent: want ENODATA, got %v", err)
	}

	// remove the rest -> chain freed (XattrOffset back to 0).
	if err := b.removeXattr(ino, "user.foo"); err != nil {
		t.Fatalf("remove user.foo: %v", err)
	}
	if err := b.removeXattr(ino, "security.baz"); err != nil {
		t.Fatalf("remove security.baz: %v", err)
	}
	di, _ := b.inodes.ReadInode(ino)
	if di.XattrOffset != 0 {
		t.Fatalf("after removing all xattrs: XattrOffset should be 0, got %d", di.XattrOffset)
	}
	if got, _ := b.listXattr(ino); len(got) != 0 {
		t.Fatalf("listxattr after clearing: want empty, got %v", got)
	}

	fsckClean(t, b, img)
}

// TestXattrLargeValue exercises a value that spans continuation block(s), then
// verifies readback and fsck-clean (the fsck xattr verifier checks split-value
// capacity and the chain structure).
func TestXattrLargeValue(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)

	in, err := b.createInDir(1, "big", briefs.ModeFile|0o644, 1000, 1000, false)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ino := in.InodeNumber

	// A value larger than one entry block's payload (~4012B) forces a split
	// across the inline part + one or more continuation blocks.
	val := makePattern(7, 12000)
	if err := b.setXattr(ino, "user.big", val, 0); err != nil {
		t.Fatalf("setxattr large: %v", err)
	}
	got, err := b.getXattr(ino, "user.big")
	if err != nil {
		t.Fatalf("getxattr large: %v", err)
	}
	if !bytesEqual(got, val) {
		t.Fatalf("large value readback mismatch: got %d bytes want %d", len(got), len(val))
	}

	// The inode should reference at least 2 chain blocks (entry + continuation).
	di, _ := b.inodes.ReadInode(ino)
	blocks, _ := b.walkXattrChainBlocks(di.XattrOffset)
	if len(blocks) < 2 {
		t.Fatalf("expected >=2 xattr blocks for 12000B value, got %d", len(blocks))
	}

	// A second (small) xattr coexisting with the large one must round-trip too.
	if err := b.setXattr(ino, "user.small", []byte("ok"), 0); err != nil {
		t.Fatalf("setxattr small: %v", err)
	}
	if got, _ := b.getXattr(ino, "user.small"); string(got) != "ok" {
		t.Fatalf("small xattr readback: want ok, got %q", string(got))
	}
	if got, _ := b.getXattr(ino, "user.big"); !bytesEqual(got, val) {
		t.Fatalf("large value readback after adding small: mismatch")
	}

	// Replacing the large value with another large value reuses/frees the chain.
	val2 := makePattern(9, 9000)
	if err := b.setXattr(ino, "user.big", val2, 0); err != nil {
		t.Fatalf("setxattr replace large: %v", err)
	}
	if got, _ := b.getXattr(ino, "user.big"); !bytesEqual(got, val2) {
		t.Fatalf("replace large readback mismatch")
	}

	fsckClean(t, b, img)
}

// TestXattrRebuildPreservesOthers sets several xattrs, replaces one in the
// middle, and verifies all the others (and the replaced one) read back
// correctly — the rebuild re-packs the whole chain.
func TestXattrRebuildPreservesOthers(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)

	in, err := b.createInDir(1, "r", briefs.ModeFile|0o644, 1000, 1000, false)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ino := in.InodeNumber

	kvs := []struct{ name, value string }{
		{"user.a", "alpha"},
		{"user.b", "bravo"},
		{"user.c", "charlie"},
		{"user.d", "delta"},
	}
	for _, kv := range kvs {
		if err := b.setXattr(ino, kv.name, []byte(kv.value), 0); err != nil {
			t.Fatalf("set %q: %v", kv.name, err)
		}
	}
	// Replace user.b with a much larger value so the chain layout shifts.
	bigVal := makePattern(3, 5000)
	if err := b.setXattr(ino, "user.b", bigVal, 0); err != nil {
		t.Fatalf("replace user.b: %v", err)
	}
	// Every other xattr must survive the rebuild.
	for _, kv := range kvs {
		want := []byte(kv.value)
		if kv.name == "user.b" {
			want = bigVal
		}
		got, err := b.getXattr(ino, kv.name)
		if err != nil {
			t.Fatalf("get %q after rebuild: %v", kv.name, err)
		}
		if !bytesEqual(got, want) {
			t.Fatalf("get %q after rebuild: want %d bytes, got %d", kv.name, len(want), len(got))
		}
	}

	fsckClean(t, b, img)
}