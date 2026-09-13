package fuse

import (
	"context"
	"testing"

	"github.com/ctdk/briefs-utils/briefs"
)

// acl444 is the generic/444 default-ACL shape: full named-user grant,
// group r-x, permissive mask, other r-x. Under mkdir 0777 (setgid parent,
// umask ignored) it must yield mode 02775.
var acl444 = []posixAclEntry{
	{tag: aclTagUserObj, perm: 0o7},
	{tag: aclTagUser, perm: 0o7, id: 101},
	{tag: aclTagGroupObj, perm: 0o5},
	{tag: aclTagMask, perm: 0o7},
	{tag: aclTagOther, perm: 0o5},
}

func TestDecodePosixAclRoundTrip(t *testing.T) {
	blob := encodePosixAcl(acl444)
	got, ok := decodePosixAcl(blob)
	if !ok {
		t.Fatalf("decode failed for valid blob")
	}
	if len(got) != len(acl444) {
		t.Fatalf("entry count: want %d, got %d", len(acl444), len(got))
	}
	for i := range got {
		if got[i] != acl444[i] {
			t.Fatalf("entry %d: want %+v, got %+v", i, acl444[i], got[i])
		}
	}

	// Rejections: empty, bad version, trailing byte, unknown tag.
	for name, blob := range map[string][]byte{
		"empty":       {},
		"no entries":  encodePosixAcl(nil),
		"bad version": {0x01, 0, 0, 0},
		"trailing":    append(append([]byte{}, blob...), 0),
		"short entry": {0x02, 0, 0, 0, 0x01, 0},
		"unknown tag": encodePosixAcl([]posixAclEntry{{tag: 0x99, perm: 0o7}}),
	} {
		if _, ok := decodePosixAcl(blob); ok {
			t.Fatalf("%s: decode unexpectedly succeeded", name)
		}
	}
}

func TestCreateModeFromACL(t *testing.T) {
	// generic/444: mkdir 0777 under u::rwx,u:101:rwx,g::r-x,m::rwx,o::r-x
	// (umask ignored) -> 0775; setid bits pass through the masq untouched.
	got, ok := createModeFromACL(append([]posixAclEntry{}, acl444...), 0o777)
	if !ok {
		t.Fatalf("masq failed for the 444 ACL")
	}
	if got != 0o775 {
		t.Fatalf("444 shape mode: want 0775, got %o", got)
	}

	// Setid bits pass through: mode 06777 -> 06775 (both bits survive).
	got, _ = createModeFromACL(append([]posixAclEntry{}, acl444...), 0o6777)
	if got != 0o6775 {
		t.Fatalf("setid passthrough: want 06775, got %o", got)
	}

	// generic/697 shape 1: a restrictive mask clears group-exec
	// (g::r-x + m::rw -> group rw).
	mrw := []posixAclEntry{
		{tag: aclTagUserObj, perm: 0o7},
		{tag: aclTagGroupObj, perm: 0o5},
		{tag: aclTagMask, perm: 0o6},
		{tag: aclTagOther, perm: 0o5},
	}
	got, ok = createModeFromACL(mrw, 0o777)
	if !ok || got != 0o765 {
		t.Fatalf("m::rw shape: want 0765, got %o (ok %v)", got, ok)
	}

	// generic/697 shape 2: no mask, group_obj rwx keeps group-exec.
	gnoMask := []posixAclEntry{
		{tag: aclTagUserObj, perm: 0o7},
		{tag: aclTagGroupObj, perm: 0o7},
		{tag: aclTagOther, perm: 0o5},
	}
	got, ok = createModeFromACL(gnoMask, 0o777)
	if !ok || got != 0o775 {
		t.Fatalf("g::rwx no-mask shape: want 0775, got %o (ok %v)", got, ok)
	}

	// No USER_OBJ entry: the masq (like the kernel's) passes the owner bits
	// through untouched; such an ACL can't be stored through the kernel's
	// setxattr validation, so this is unreachable live.
	got, ok = createModeFromACL([]posixAclEntry{
		{tag: aclTagGroupObj, perm: 0o7},
		{tag: aclTagOther, perm: 0o5},
	}, 0o777)
	if !ok || got != 0o775 {
		t.Fatalf("no USER_OBJ: want 0775, got %o (ok %v)", got, ok)
	}

	// The kernel's -EIO case: neither MASK nor GROUP_OBJ.
	if _, ok := createModeFromACL([]posixAclEntry{
		{tag: aclTagUserObj, perm: 0o7},
		{tag: aclTagOther, perm: 0o5},
	}, 0o777); ok {
		t.Fatalf("no group class: masq unexpectedly succeeded")
	}
}

func TestCreateUmask(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)

	const rootIno = 1

	// umask 022 on a plain parent: 0666 -> 0644, 0777 -> 0755.
	f, err := b.createInDir(rootIno, "f666", briefs.ModeFile|0o666, 1000, 1000, false, 0o022)
	if err != nil {
		t.Fatalf("createInDir f666: %v", err)
	}
	if f.Filemode&0o777 != 0o644 {
		t.Fatalf("f666 mode: want 0644, got %o", f.Filemode&0o777)
	}
	f, err = b.createInDir(rootIno, "f777", briefs.ModeFile|0o777, 1000, 1000, false, 0o022)
	if err != nil {
		t.Fatalf("createInDir f777: %v", err)
	}
	if f.Filemode&0o777 != 0o755 {
		t.Fatalf("f777 mode: want 0755, got %o", f.Filemode&0o777)
	}

	// umask 0: verbatim.
	f, err = b.createInDir(rootIno, "f0", briefs.ModeFile|0o666, 1000, 1000, false, 0)
	if err != nil {
		t.Fatalf("createInDir f0: %v", err)
	}
	if f.Filemode&0o777 != 0o666 {
		t.Fatalf("f0 mode: want 0666, got %o", f.Filemode&0o777)
	}

	// The umask never touches setid bits (a full umask still clears the
	// perm bits themselves — this checks only the setid half).
	f, err = b.createInDir(rootIno, "setid", briefs.ModeFile|0o4755, 1000, 1000, false, 0o022)
	if err != nil {
		t.Fatalf("createInDir setid: %v", err)
	}
	if f.Filemode&0o7777 != 0o4755 {
		t.Fatalf("setid mode: want 04755, got %o", f.Filemode&0o7777)
	}

	// mknod honors the umask too; symlinks never do.
	n, err := b.mknodInDir(rootIno, "fifo", modeFifo|0o666, 1000, 1000, 0, 0o022)
	if err != nil {
		t.Fatalf("mknodInDir fifo: %v", err)
	}
	if n.Filemode&0o777 != 0o644 {
		t.Fatalf("fifo mode: want 0644, got %o", n.Filemode&0o777)
	}
	l, err := b.symlinkInDir(rootIno, "link", "f666", 1000, 1000)
	if err != nil {
		t.Fatalf("symlinkInDir link: %v", err)
	}
	if l.Filemode&0o777 != 0o777 {
		t.Fatalf("link mode: want 0777, got %o", l.Filemode&0o777)
	}
}

func TestCreateDefaultACLMode(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)

	const rootIno = 1

	// A setgid parent with the generic/444 default ACL.
	parent, err := b.createInDir(rootIno, "sgdir", briefs.ModeDir|modeSetGID|0o755, 1000, 777, false, 0)
	if err != nil {
		t.Fatalf("createInDir sgdir: %v", err)
	}
	if err := b.setXattr(parent.InodeNumber, aclDefaultName, encodePosixAcl(acl444), 0); err != nil {
		t.Fatalf("setxattr default ACL: %v", err)
	}

	// mkdir 0777 under umask 022: the ACL decides — 0775 — and the
	// setgid-dir inherit still applies on top (02775).
	d, err := b.createInDir(parent.InodeNumber, "sub", briefs.ModeDir|0o777, 1000, 1000, false, 0o022)
	if err != nil {
		t.Fatalf("createInDir sub: %v", err)
	}
	if d.Filemode != briefs.ModeDir|modeSetGID|0o775 {
		t.Fatalf("sub mode: want 02775, got %o", d.Filemode)
	}
	// The setgid-dir gid inheritance still applies alongside.
	if d.Gid != 777 {
		t.Fatalf("sub gid: want 777, got %d", d.Gid)
	}

	// A file too: 0666 request, umask ignored, ACL masks it (owner rw,
	// group r--, other r--: the masq intersects, it does not replace).
	f, err := b.createInDir(parent.InodeNumber, "file", briefs.ModeFile|0o666, 1000, 1000, false, 0o022)
	if err != nil {
		t.Fatalf("createInDir file: %v", err)
	}
	if f.Filemode&0o7777 != 0o664 {
		t.Fatalf("file mode: want 0664, got %o", f.Filemode&0o7777)
	}

	// Malformed default ACL: umask fallback (documented deviation).
	if err := b.setXattr(parent.InodeNumber, aclDefaultName, []byte{0x99, 0, 0, 0}, 0); err != nil {
		t.Fatalf("setxattr garbage ACL: %v", err)
	}
	g, err := b.createInDir(parent.InodeNumber, "fallback", briefs.ModeFile|0o666, 1000, 1000, false, 0o022)
	if err != nil {
		t.Fatalf("createInDir fallback: %v", err)
	}
	if g.Filemode&0o777 != 0o644 {
		t.Fatalf("fallback mode: want 0644, got %o", g.Filemode&0o777)
	}

	// No ACL at all: umask alone (regression guard for the ACL lookup path
	// on an xattr-less inode — the other parent in this image).
	p2, err := b.createInDir(rootIno, "plain", briefs.ModeDir|0o755, 1000, 1000, false, 0)
	if err != nil {
		t.Fatalf("createInDir plain: %v", err)
	}
	f2, err := b.createInDir(p2.InodeNumber, "file", briefs.ModeFile|0o666, 1000, 1000, false, 0o022)
	if err != nil {
		t.Fatalf("createInDir file2: %v", err)
	}
	if f2.Filemode&0o777 != 0o644 {
		t.Fatalf("file2 mode: want 0644, got %o", f2.Filemode&0o777)
	}
}

// live444Blob is the exact system.posix_acl_default blob the live VM's
// setfacl -d -m u:101:rwx wrote (od dump) in the generic/444 failure: a full
// named-user grant, group r-x, permissive mask, other r-x.
var live444Blob = []byte{
	0x02, 0, 0, 0,
	0x01, 0, 0x07, 0, 0xff, 0xff, 0xff, 0xff,
	0x02, 0, 0x07, 0, 0x65, 0, 0, 0,
	0x04, 0, 0x05, 0, 0xff, 0xff, 0xff, 0xff,
	0x10, 0, 0x07, 0, 0xff, 0xff, 0xff, 0xff,
	0x20, 0, 0x05, 0, 0xff, 0xff, 0xff, 0xff,
}

// TestCreateMode444LiveSequence replays the exact generic/444 sequence:
// mkdir, setfacl a default ACL, chown 100:100 + chmod 2755, then a 0777
// mkdir under umask 022 — which must land on 02775: the ACL masq (umask
// ignored) gives 0775 and the setgid parent adds S_ISGID and its gid.
func TestCreateMode444LiveSequence(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)

	const rootIno = 1
	parent, err := b.createInDir(rootIno, "testdir.444", briefs.ModeDir|0o755, 0, 0, false, 0o022)
	if err != nil {
		t.Fatalf("mkdir testdir: %v", err)
	}
	if err := b.setXattr(parent.InodeNumber, "system.posix_acl_default", live444Blob, 0); err != nil {
		t.Fatalf("setfacl default ACL: %v", err)
	}
	req := setattrReq(fattrUID|fattrGID|fattrMode,
		withUID(100), withGID(100), withMode(briefs.ModeDir|0o2755))
	if err := b.setattrOp(context.Background(), parent.InodeNumber, req); err != nil {
		t.Fatalf("chown+chmod: %v", err)
	}

	sub, err := b.createInDir(parent.InodeNumber, "testsub1", briefs.ModeDir|0o777, 100, 100, false, 0o022)
	if err != nil {
		t.Fatalf("mkdir testsub1: %v", err)
	}
	if sub.Filemode != briefs.ModeDir|0o2775 {
		t.Fatalf("testsub1 mode: want 02775, got %o", sub.Filemode)
	}
	if sub.Gid != 100 {
		t.Fatalf("testsub1 gid: want 100, got %d", sub.Gid)
	}
}
