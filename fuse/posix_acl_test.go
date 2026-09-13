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

// aclAcl75 is a u::rwx,u:101:rwx,g::rwx,m::r-x,o::r-x access ACL: the MASK
// (not GROUP_OBJ) fixes the group class.
var aclMasked = []posixAclEntry{
	{tag: aclTagUserObj, perm: 0o7},
	{tag: aclTagUser, perm: 0o7, id: 101},
	{tag: aclTagGroupObj, perm: 0o7},
	{tag: aclTagMask, perm: 0o5},
	{tag: aclTagOther, perm: 0o5},
}

// aclTrivial is the equiv-mode setfacl of `chmod 640`: u::rw-,g::r--,o::---.
var aclTrivial = []posixAclEntry{
	{tag: aclTagUserObj, perm: 0o6},
	{tag: aclTagGroupObj, perm: 0o4},
	{tag: aclTagOther, perm: 0o0},
}

// TestSetAccessAclUpdatesMode covers the setxattr ACL contract
// (generic/375): storing system.posix_acl_access rewrites the mode from the
// ACL — the kernel delegates this to the daemon (fs/fuse/acl.c) — clearing
// S_ISGID unless the caller is in the file's group or capable
// (in_group_or_capable, what the kernel signals to setxattr-ext daemons via
// FUSE_SETXATTR_ACL_KILL_SGID).
func TestSetAccessAclUpdatesMode(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)

	const rootIno = 1

	// accessAclMode: the class entries decide; a MASK overwrites the
	// GROUP_OBJ's group bits; named entries contribute nothing.
	if m := accessAclMode(acl444); m != 0o775 {
		t.Fatalf("444 ACL mode: want 0775, got %o", m)
	}
	if m := accessAclMode(aclMasked); m != 0o755 {
		t.Fatalf("masked ACL mode: want 0755, got %o", m)
	}
	if m := accessAclMode(aclTrivial); m != 0o640 {
		t.Fatalf("trivial ACL mode: want 0640, got %o", m)
	}

	// A caller-less context (no FUSE caller: unreachable live) is the
	// unprivileged worst case: not in the file's group, no capabilities.
	// A 2640 file taking the trivial ACL lands on 0640 — sgid cleared.
	f, err := b.createInDir(rootIno, "f1", briefs.ModeFile|0o2640, 1000, 100, false, 0)
	if err != nil {
		t.Fatalf("createInDir f1: %v", err)
	}
	blob := encodePosixAcl(aclTrivial)
	if err := b.setXattrOp(context.Background(), f.InodeNumber, aclAccessName, blob, 0); err != nil {
		t.Fatalf("setxattrOp access ACL: %v", err)
	}
	in, err := b.inodes.ReadInode(f.InodeNumber)
	if err != nil {
		t.Fatalf("ReadInode f1: %v", err)
	}
	if in.Filemode&0o7777 != 0o640 {
		t.Fatalf("unpriv ACL mode: want 0640, got %o", in.Filemode&0o7777)
	}
	// The blob itself is stored verbatim.
	got, err := b.getXattr(f.InodeNumber, aclAccessName)
	if err != nil || string(got) != string(blob) {
		t.Fatalf("access ACL blob: want % x, got % x (err %v)", blob, got, err)
	}

	// In the file's group (egid 100): the S_ISGID survives.
	f2, err := b.createInDir(rootIno, "f2", briefs.ModeFile|0o2640, 1000, 100, false, 0)
	if err != nil {
		t.Fatalf("createInDir f2: %v", err)
	}
	injectCallerStatus(t, callerStatus{}, true)
	ctx := callerCtx(100, 100, 4321)
	if err := b.setXattrOp(ctx, f2.InodeNumber, aclAccessName, blob, 0); err != nil {
		t.Fatalf("setxattrOp in-group: %v", err)
	}
	in, err = b.inodes.ReadInode(f2.InodeNumber)
	if err != nil {
		t.Fatalf("ReadInode f2: %v", err)
	}
	if in.Filemode&0o7777 != 0o2640 {
		t.Fatalf("in-group ACL mode: want 02640, got %o", in.Filemode&0o7777)
	}

	// Out of the group but holding CAP_FOWNER: the S_ISGID survives too.
	f3, err := b.createInDir(rootIno, "f3", briefs.ModeFile|0o2640, 1000, 100, false, 0)
	if err != nil {
		t.Fatalf("createInDir f3: %v", err)
	}
	injectCallerStatus(t, callerStatus{capEff: 1 << capFOwnerBit}, true)
	ctx = callerCtx(1000, 1000, 4322)
	if err := b.setXattrOp(ctx, f3.InodeNumber, aclAccessName, blob, 0); err != nil {
		t.Fatalf("setxattrOp capable: %v", err)
	}
	in, err = b.inodes.ReadInode(f3.InodeNumber)
	if err != nil {
		t.Fatalf("ReadInode f3: %v", err)
	}
	if in.Filemode&0o7777 != 0o2640 {
		t.Fatalf("capable ACL mode: want 02640, got %o", in.Filemode&0o7777)
	}

	// An undecodable blob is stored verbatim with no mode change.
	f4, err := b.createInDir(rootIno, "f4", briefs.ModeFile|0o640, 1000, 100, false, 0)
	if err != nil {
		t.Fatalf("createInDir f4: %v", err)
	}
	junk := []byte{0x09, 0x09, 0x09, 0x09}
	if err := b.setXattrOp(context.Background(), f4.InodeNumber, aclAccessName, junk, 0); err != nil {
		t.Fatalf("setxattrOp junk: %v", err)
	}
	got, err = b.getXattr(f4.InodeNumber, aclAccessName)
	if err != nil || string(got) != string(junk) {
		t.Fatalf("junk ACL blob: want % x, got % x (err %v)", junk, got, err)
	}
	in, err = b.inodes.ReadInode(f4.InodeNumber)
	if err != nil {
		t.Fatalf("ReadInode f4: %v", err)
	}
	if in.Filemode&0o7777 != 0o640 {
		t.Fatalf("junk ACL mode: want 0640 unchanged, got %o", in.Filemode&0o7777)
	}

	// The default ACL never rewrites a mode (it only shapes creates).
	d, err := b.createInDir(rootIno, "d", briefs.ModeDir|0o755, 1000, 100, false, 0)
	if err != nil {
		t.Fatalf("createInDir d: %v", err)
	}
	if err := b.setXattrOp(context.Background(), d.InodeNumber, aclDefaultName, encodePosixAcl(acl444), 0); err != nil {
		t.Fatalf("setxattrOp default ACL: %v", err)
	}
	in, err = b.inodes.ReadInode(d.InodeNumber)
	if err != nil {
		t.Fatalf("ReadInode d: %v", err)
	}
	if in.Filemode&0o7777 != 0o755 {
		t.Fatalf("default ACL mode: want 0755 unchanged, got %o", in.Filemode&0o7777)
	}
}

// TestChmodUpdatesAccessAcl covers the chmod-masq (generic/375): the stored
// access ACL must track the mode after a chmod — posix_acl_chmod, which
// native filesystems run from their ->setattr and fuse_setattr defers to
// the daemon — or a later setfacl whose change already matches the stale
// ACL issues no setxattr and the mode never follows the ACL.
func TestChmodUpdatesAccessAcl(t *testing.T) {
	// chmodAcl unit shapes: group class from MASK when present (GROUP_OBJ
	// and named entries untouched), else from GROUP_OBJ; the kernel's
	// -EIO shape has no group class.
	acl := []posixAclEntry{
		{tag: aclTagUserObj, perm: 0o6},
		{tag: aclTagUser, perm: 0o7, id: 101},
		{tag: aclTagGroupObj, perm: 0o4},
		{tag: aclTagMask, perm: 0o7},
		{tag: aclTagOther, perm: 0o4},
	}
	if !chmodAcl(acl, 0o755) {
		t.Fatalf("chmodAcl failed for masked shape")
	}
	want := []posixAclEntry{
		{tag: aclTagUserObj, perm: 0o7},
		{tag: aclTagUser, perm: 0o7, id: 101},
		{tag: aclTagGroupObj, perm: 0o4},
		{tag: aclTagMask, perm: 0o5},
		{tag: aclTagOther, perm: 0o5},
	}
	for i := range want {
		if acl[i] != want[i] {
			t.Fatalf("entry %d: want %+v, got %+v", i, want[i], acl[i])
		}
	}
	noMask := []posixAclEntry{
		{tag: aclTagUserObj, perm: 0o6},
		{tag: aclTagGroupObj, perm: 0o7},
		{tag: aclTagOther, perm: 0o4},
	}
	if !chmodAcl(noMask, 0o755) {
		t.Fatalf("chmodAcl failed for group-obj shape")
	}
	if noMask[1].perm != 0o5 || noMask[0].perm != 0o7 || noMask[2].perm != 0o5 {
		t.Fatalf("group-obj shape: got %+v", noMask)
	}
	if chmodAcl([]posixAclEntry{
		{tag: aclTagUserObj, perm: 0o7},
		{tag: aclTagOther, perm: 0o5},
	}, 0o755) {
		t.Fatalf("no group class: chmodAcl unexpectedly succeeded")
	}

	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)

	const rootIno = 1

	// The generic/375 file sequence. Store an access ACL (setfacl), then
	// chmod 2755: the stored ACL must masq to u::rwx,g::r-x,o::r-x — with
	// the mode already set on the inode, both publish in one commit.
	f, err := b.createInDir(rootIno, "f", briefs.ModeFile|0o644, 1000, 100, false, 0)
	if err != nil {
		t.Fatalf("createInDir f: %v", err)
	}
	injectCallerStatus(t, callerStatus{}, true)
	ctx := callerCtx(100, 100, 4321)
	if err := b.setXattrOp(ctx, f.InodeNumber, aclAccessName, encodePosixAcl(aclTrivial), 0); err != nil {
		t.Fatalf("setfacl initial: %v", err)
	}
	if err := b.setattrOp(context.Background(), f.InodeNumber,
		setattrReq(fattrMode, withMode(0o2755))); err != nil {
		t.Fatalf("chmod 2755: %v", err)
	}
	got, err := b.getXattr(f.InodeNumber, aclAccessName)
	if err != nil {
		t.Fatalf("getXattr after chmod: %v", err)
	}
	entries, ok := decodePosixAcl(got)
	if !ok || len(entries) != 3 {
		t.Fatalf("post-chmod ACL decode: ok %v, %d entries", ok, len(entries))
	}
	want = []posixAclEntry{
		{tag: aclTagUserObj, perm: 0o7},
		{tag: aclTagGroupObj, perm: 0o5},
		{tag: aclTagOther, perm: 0o5},
	}
	for i := range want {
		if entries[i] != want[i] {
			t.Fatalf("post-chmod entry %d: want %+v, got %+v", i, want[i], entries[i])
		}
	}

	// Then the out-of-group setfacl (its change now differs from the
	// masq'd ACL, so it issues a setxattr): mode follows the ACL and the
	// S_ISGID is cleared.
	ctx = callerCtx(100, 101, 4322)
	if err := b.setXattrOp(ctx, f.InodeNumber, aclAccessName,
		encodePosixAcl([]posixAclEntry{
			{tag: aclTagUserObj, perm: 0o7, id: 0xffffffff},
			{tag: aclTagGroupObj, perm: 0o7, id: 0xffffffff},
			{tag: aclTagOther, perm: 0o7, id: 0xffffffff},
		}), 0); err != nil {
		t.Fatalf("setfacl clear: %v", err)
	}
	in, err := b.inodes.ReadInode(f.InodeNumber)
	if err != nil {
		t.Fatalf("ReadInode f: %v", err)
	}
	if in.Filemode&0o7777 != 0o777 {
		t.Fatalf("post-setfacl mode: want 0777, got %o", in.Filemode&0o7777)
	}

	// A further chmod re-masqs the stored ACL to the new mode too; an
	// ACL-less inode's chmod path is unaffected (the XattrOffset guard).
	if err := b.setattrOp(context.Background(), f.InodeNumber,
		setattrReq(fattrMode, withMode(0o755))); err != nil {
		t.Fatalf("chmod matching: %v", err)
	}
	in, err = b.inodes.ReadInode(f.InodeNumber)
	if err != nil {
		t.Fatalf("ReadInode f2: %v", err)
	}
	if in.Filemode&0o7777 != 0o755 {
		t.Fatalf("matching chmod mode: want 0755, got %o", in.Filemode&0o7777)
	}
	got, err = b.getXattr(f.InodeNumber, aclAccessName)
	if err != nil {
		t.Fatalf("getXattr after matching chmod: %v", err)
	}
	if entries, ok := decodePosixAcl(got); !ok || entries[1].perm != 0o5 || entries[2].perm != 0o5 {
		t.Fatalf("matching chmod ACL: got % x", got)
	}
}
