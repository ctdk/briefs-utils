// Package fuse: POSIX ACL xattr decoding and create-mode computation.
//
// Because the mount negotiates FUSE_DONT_MASK (fuse.go), the kernel sets
// fc->dont_mask from the init reply (fs/fuse/inode.c:1312) and sends create
// requests with the caller's mode unmasked. The daemon must apply the umask
// itself — or, when the parent directory carries a system.posix_acl_default
// xattr, compute the mode from the ACL instead and ignore the umask,
// mirroring the VFS posix_acl_create path. This file ports the on-wire
// xattr format (posix_acl_from_xattr, fs/posix_acl.c) and the mode
// computation (posix_acl_create_masq, fs/posix_acl.c:459).

package fuse

import (
	"encoding/binary"

	"github.com/ctdk/briefs-utils/briefs"
)

// On-wire POSIX ACL xattr layout (include/uapi/linux/posix_acl_xattr.h):
// a u32le version header (2) followed by 8-byte entries
// {u16le tag, u16le perm, u32le id}.
const (
	posixAclVersion = 2
	posixAclEntrySz = 8

	aclTagUserObj  = 0x01
	aclTagUser     = 0x02
	aclTagGroupObj = 0x04
	aclTagGroup    = 0x08
	aclTagMask     = 0x10
	aclTagOther    = 0x20
)

// Mode bits the masq rebuilds (the kernel's S_IRWX* constants). Typed so
// the masq's | ~S_IRWXO complements stay in uint32 space.
const (
	sIRWXU   uint32 = 0o700
	sIRWXG   uint32 = 0o070
	sIRWXO   uint32 = 0o007
	sIRWXUGO        = sIRWXU | sIRWXG | sIRWXO
)

// aclDefaultName is the parent-directory xattr a default ACL is stored in;
// aclAccessName is the per-file access ACL, whose mode implications the
// kernel does not compute for FUSE (see setXattrOp).
const (
	aclDefaultName = "system.posix_acl_default"
	aclAccessName  = "system.posix_acl_access"
)

// posixAclEntry is one decoded ACL entry (perm is the low 3 bits; id names
// the user/group for the USER and GROUP tags, 0 otherwise).
type posixAclEntry struct {
	tag  uint16
	perm uint16
	id   uint32
}

// decodePosixAcl decodes a POSIX ACL xattr blob. The second return is false
// for anything the kernel's posix_acl_from_xattr would reject — wrong
// version, trailing bytes, unknown tag, or an empty entry set — so callers
// fall back to umask masking. (An empty default ACL means "no ACL" to the
// kernel, which also falls back to the umask, so the shared fallback is
// correct.)
func decodePosixAcl(blob []byte) ([]posixAclEntry, bool) {
	if len(blob) < 4 || binary.LittleEndian.Uint32(blob) != posixAclVersion {
		return nil, false
	}
	rest := blob[4:]
	if len(rest)%posixAclEntrySz != 0 {
		return nil, false
	}
	entries := make([]posixAclEntry, 0, len(rest)/posixAclEntrySz)
	for len(rest) > 0 {
		tag := binary.LittleEndian.Uint16(rest)
		switch tag {
		case aclTagUserObj, aclTagUser, aclTagGroupObj, aclTagGroup, aclTagMask, aclTagOther:
		default:
			return nil, false
		}
		entries = append(entries, posixAclEntry{
			tag:  tag,
			perm: binary.LittleEndian.Uint16(rest[2:]),
			id:   binary.LittleEndian.Uint32(rest[4:]),
		})
		rest = rest[posixAclEntrySz:]
	}
	if len(entries) == 0 {
		return nil, false
	}
	return entries, true
}

// createModeFromACL computes the mode of a new child created under a
// directory with default ACL @acl — a port of posix_acl_create_masq
// (fs/posix_acl.c:459). Every permission not granted by the ACL is removed
// from @mode; the caller's umask plays no part. @mode must be the perm+setid
// bits only (no S_IFMT — the VFS masks create modes to 0o7777 before
// posix_acl_create, and the masq's (mode>>6) shifts type bits into the
// perm space otherwise); setid bits pass through untouched.
//
// The second return is false when the ACL has neither a MASK nor a GROUP_OBJ
// entry (the kernel's -EIO case); the caller falls back to umask masking, a
// documented deviation from the kernel, which fails the create outright.
// (An ACL with no USER_OBJ entry can't be stored through the kernel's
// setxattr validation, so the masq — like the kernel's — simply passes the
// owner bits through.)
func createModeFromACL(entries []posixAclEntry, mode uint32) (uint32, bool) {
	// The kernel's masq starts from the full mode and intersects each
	// class's entry into it (umode_t mode = *mode_p).
	m := mode
	groupIdx, maskIdx := -1, -1
	for i := range entries {
		e := &entries[i]
		switch e.tag {
		case aclTagUserObj:
			e.perm &= uint16((mode >> 6) | ^sIRWXO)
			m &= (uint32(e.perm) << 6) | ^sIRWXU
		case aclTagUser, aclTagGroup:
			// Named entries contribute no mode bits.
		case aclTagGroupObj:
			groupIdx = i
		case aclTagOther:
			e.perm &= uint16(mode | ^sIRWXO)
			m &= uint32(e.perm) | ^sIRWXO
		case aclTagMask:
			maskIdx = i
		default:
			return 0, false
		}
	}
	// The group-class entry: the MASK when present, else GROUP_OBJ.
	gi := maskIdx
	if gi < 0 {
		gi = groupIdx
	}
	if gi < 0 {
		return 0, false
	}
	entries[gi].perm &= uint16((m >> 3) | ^sIRWXO)
	m &= (uint32(entries[gi].perm) << 3) | ^sIRWXG
	return (mode &^ sIRWXUGO) | m, true
}

// accessAclMode builds the permission bits an access ACL implies — the
// mode half of posix_acl_equiv_mode (fs/posix_acl.c:312): owner from
// USER_OBJ, group from GROUP_OBJ, overwritten by MASK when present, other
// from OTHER; named USER/GROUP entries contribute no bits. Only each
// entry's low 3 perm bits count (the kernel masks e_perm & S_IRWXO).
func accessAclMode(entries []posixAclEntry) uint32 {
	var mode uint32
	for _, e := range entries {
		perm := uint32(e.perm & 0o7)
		switch e.tag {
		case aclTagUserObj:
			mode |= perm << 6
		case aclTagGroupObj:
			mode |= perm << 3
		case aclTagOther:
			mode |= perm
		case aclTagMask:
			mode = (mode &^ sIRWXG) | (perm << 3)
		}
	}
	return mode
}

// chmodAcl rewrites an access ACL's entries to match a new mode — a port
// of __posix_acl_chmod_masq (fs/posix_acl.c:516): USER_OBJ and OTHER take
// the mode's owner/other perm bits, the group class (MASK when present,
// else GROUP_OBJ) the group bits; named entries are untouched. Returns
// false for the kernel's -EIO shape (no group-class entry).
func chmodAcl(entries []posixAclEntry, mode uint32) bool {
	groupIdx, maskIdx := -1, -1
	for i := range entries {
		switch entries[i].tag {
		case aclTagUserObj:
			entries[i].perm = uint16((mode & sIRWXU) >> 6)
		case aclTagGroupObj:
			groupIdx = i
		case aclTagMask:
			maskIdx = i
		case aclTagOther:
			entries[i].perm = uint16(mode & sIRWXO)
		}
	}
	gi := maskIdx
	if gi < 0 {
		gi = groupIdx
	}
	if gi < 0 {
		return false
	}
	entries[gi].perm = uint16((mode & sIRWXG) >> 3)
	return true
}

// syncAccessAclToMode rewrites @in's stored system.posix_acl_access to
// match @in's (already updated) Filemode — what native filesystems do from
// their ->setattr via posix_acl_chmod (e.g. ext4/inode.c:6156,
// xfs/xfs_iops.c:888). fuse_setattr never runs it: fs/fuse/dir.c:2365
// comments that the daemon "may have updated acl xattrs in the filesystem",
// so the bridge must, or a chmod leaves the stored ACL stale and a
// subsequent setfacl whose change already matches the stale ACL issues no
// setxattr — leaving the mode stuck (generic/375). The masq'd blob rides
// setXattrLocked's commit, which persists @in, so the ACL and the new mode
// publish together; an absent, undecodable, or unmasq-able ACL is left
// alone (the mode alone is then authoritative, as for an ACL-less inode).
func (b *BrieFS) syncAccessAclToMode(in *briefs.Inode) error {
	kvs, _, err := b.loadXattrEntries(in)
	if err != nil {
		return err
	}
	blob, ok := getXattrFromKvs(kvs, aclAccessName)
	if !ok {
		return nil
	}
	acl, aok := decodePosixAcl(blob)
	if !aok {
		return nil
	}
	if !chmodAcl(acl, in.Filemode&0o777) {
		return nil
	}
	newBlob := encodePosixAcl(acl)
	if string(newBlob) == string(blob) {
		return nil
	}
	return b.setXattrLocked(in, aclAccessName, newBlob, 0)
}

// encodePosixAcl serializes entries into the on-wire blob format (tests).
func encodePosixAcl(entries []posixAclEntry) []byte {
	buf := make([]byte, 4+posixAclEntrySz*len(entries))
	binary.LittleEndian.PutUint32(buf, posixAclVersion)
	for i, e := range entries {
		off := 4 + posixAclEntrySz*i
		binary.LittleEndian.PutUint16(buf[off:], e.tag)
		binary.LittleEndian.PutUint16(buf[off+2:], e.perm)
		binary.LittleEndian.PutUint32(buf[off+4:], e.id)
	}
	return buf
}

// getXattrFromKvs scans an already-loaded entry set for name. Used where
// the inode-block lock is already held (createNamedInode); b.getXattr takes
// that lock itself and would self-deadlock there.
func getXattrFromKvs(kvs []xattrKV, name string) ([]byte, bool) {
	for _, kv := range kvs {
		if kv.name == name {
			return kv.value, true
		}
	}
	return nil, false
}

// applyCreateMode masks a raw create mode per posix_acl_create: from the
// parent's system.posix_acl_default when the parent carries a decodable one
// (umask ignored), else from the caller's umask. Malformed or invalid ACL
// blobs fall back to umask masking (documented deviation: the kernel fails
// the create with the ACL error instead).
//
// The caller must hold the parent's inode-block lock: loadXattrEntries takes
// no locks of its own, and b.getXattr would self-deadlock here.
func (b *BrieFS) applyCreateMode(parent *briefs.Inode, mode, umask uint32) uint32 {
	if parent.XattrOffset != 0 {
		kvs, _, err := b.loadXattrEntries(parent)
		if err == nil {
			if blob, ok := getXattrFromKvs(kvs, aclDefaultName); ok {
				if acl, aok := decodePosixAcl(blob); aok {
					// perm+setid bits only: type bits would shift into
					// the masq's perm space.
					if newMode, cok := createModeFromACL(acl, mode&0o7777); cok {
						return newMode | (mode &^ 0o7777)
					}
				}
			}
		}
	}
	return mode &^ (umask & sIRWXUGO)
}
