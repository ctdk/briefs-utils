// Package briefs: journal record payloads.
//
// These mirror the kernel's struct jrn_* (briefs.h:136-257) with byte-exact,
// little-endian on-disk layout.  Sizes are pinned by the kernel's
// BUILD_BUG_ON() checks (briefs.h:1552+):
//
//	jrn_extent_alloc   = 80
//	jrn_extent_free    = 80
//	jrn_inode_update   = 88
//	jrn_inode_alloc    = 40
//	jrn_inode_free     = 32
//	jrn_trie_alloc     = 16
//	jrn_dir_update     = 280
//	jrn_inode_full     = 560
//	jrn_xattr_data     = 20-byte prefix + variable data
//	jrn_symlink_data   = 20-byte prefix + variable target
//
// Go struct field alignment differs from C, so each Marshal() writes fields
// at explicit offsets via binary.LittleEndian rather than relying on
// encoding/binary's struct layout.  This guarantees byte-for-byte
// compatibility with the kernel's on-disk format.

package briefs

import "encoding/binary"

// JournalRecordHdrSize is the on-disk size of struct journal_record_hdr (16).
const JournalRecordHdrSize = 16

// JournalBlockHdrSize is the on-disk size of struct journal_block_header (16).
const JournalBlockHdrSize = 16

// Fixed record payload sizes (bytes).
const (
	JrnExtentAllocSize = 80
	JrnExtentFreeSize  = 80
	JrnInodeUpdateSize = 88
	JrnInodeAllocSize  = 40
	JrnInodeFreeSize   = 32
	JrnTrieAllocSize   = 16
	JrnDirUpdateSize   = 280
	JrnInodeFullSize   = 560
)

// Prefix offsets for the variable-length records (offset of the trailing
// variable array).
const (
	JrnSymlinkDataPrefix = 20 // ino(8) + phys(8) + target_len(4)
	JrnXattrDataPrefix   = 20 // ino(8) + phys_block(8) + used_size(4)
)

// JrnExtentAlloc mirrors struct jrn_extent_alloc (80 bytes).
//
//go:briefs-disk size=80
type JrnExtentAlloc struct {
	Ino         uint64
	Offset      uint64
	Length      uint64
	PhysStart   uint64
	ExtentIndex uint32
	Reserved    [44]byte
}

func (r *JrnExtentAlloc) Marshal() []byte {
	b, _ := r.MarshalBinary()
	return b
}

// JrnExtentFree mirrors struct jrn_extent_free (80 bytes).
//
//go:briefs-disk size=80
type JrnExtentFree struct {
	Ino       uint64
	Offset    uint64
	PhysStart uint64
	Length    uint64
	Reserved  [48]byte
}

func (r *JrnExtentFree) Marshal() []byte {
	b, _ := r.MarshalBinary()
	return b
}

// JrnInodeUpdate mirrors struct jrn_inode_update (88 bytes).
//
//go:briefs-disk size=88
type JrnInodeUpdate struct {
	Ino       uint64
	Mode      uint32
	Nlink     uint32
	Uid       uint32
	Gid       uint32
	FileSize  uint64
	ATimeSec  uint64
	ATimeNsec uint64
	MTimeSec  uint64
	MTimeNsec uint64
	CTimeSec  uint64
	CTimeNsec uint64
	Flags     uint32
	Reserved  uint32
}

func (r *JrnInodeUpdate) Marshal() []byte {
	b, _ := r.MarshalBinary()
	return b
}

// JrnInodeAlloc mirrors struct jrn_inode_alloc (40 bytes). The kernel inserts
// 4 bytes of alignment padding at offset 28 (between the trailing __le32
// reserved1 and the __le64 reserved2); the explicit _Pad field reproduces that
// so the generated marshal matches the on-disk layout exactly.
//
//go:briefs-disk size=40
type JrnInodeAlloc struct {
	Ino       uint64
	Mode      uint32
	Nlink     uint32
	Uid       uint32
	Gid       uint32
	Reserved1 uint32
	_Pad      uint32
	Reserved2 uint64
}

func (r *JrnInodeAlloc) Marshal() []byte {
	b, _ := r.MarshalBinary()
	return b
}

// JrnInodeFree mirrors struct jrn_inode_free (32 bytes). The kernel inserts 4
// bytes of alignment padding at offset 20 (between reserved[3] and the __le64
// reserved2); the explicit _Pad field reproduces that.
//
//go:briefs-disk size=32
type JrnInodeFree struct {
	Ino       uint64
	Reserved  [3]uint32
	_Pad      uint32
	Reserved2 uint64
}

func (r *JrnInodeFree) Marshal() []byte {
	b, _ := r.MarshalBinary()
	return b
}

// JrnTrieAlloc mirrors struct jrn_trie_alloc (16 bytes). Op is 0 for
// allocated, 1 for freed.
//
//go:briefs-disk size=16
type JrnTrieAlloc struct {
	Block    uint64
	Op       uint32 // 0 = allocated, 1 = freed
	Reserved uint32
}

func (r *JrnTrieAlloc) Marshal() []byte {
	b, _ := r.MarshalBinary()
	return b
}

// JrnDirUpdate mirrors struct jrn_dir_update (280 bytes). Op is 0 for add,
// 1 for delete. FType is the d_type form (S_IFMT >> 12): 4=dir, 8=reg,
// 10=lnk, etc. The name lives in the fixed 255-byte Name field; the live
// byte count is NameLen (<= 255). The kernel layout is parent_ino@0,
// child_ino@8, name_len@16, name[255]@20, op@275, ftype@276, reserved[1]@277,
// then 2 bytes trailing alignment padding to 280; the explicit Reserved and
// _Pad fields reproduce that so the generated marshal matches the on-disk
// layout exactly.
//
//go:briefs-disk size=280
type JrnDirUpdate struct {
	ParentIno uint64
	ChildIno  uint64
	NameLen   uint32
	Name      [255]byte
	Op        uint8 // 0 = add, 1 = delete
	FType     uint8
	Reserved  [1]byte
	_Pad      [2]byte
}

// NewJrnDirUpdate builds a JRN_DIR_UPDATE record from a directory-entry name.
// NameLen is set to the name length (clamped to 255) and the name bytes are
// copied into the fixed Name field. Use this instead of a struct literal so
// NameLen and Name stay in sync.
func NewJrnDirUpdate(parent, child uint64, name string, op, ftype uint8) *JrnDirUpdate {
	nl := len(name)
	if nl > 255 {
		nl = 255
	}
	r := &JrnDirUpdate{
		ParentIno: parent,
		ChildIno:  child,
		NameLen:   uint32(nl),
		Op:        op,
		FType:     ftype,
	}
	copy(r.Name[:], name[:nl])
	return r
}

func (r *JrnDirUpdate) Marshal() []byte {
	b, _ := r.MarshalBinary()
	return b
}

// JrnInodeFull mirrors struct jrn_inode_full (560 bytes): an 8-byte inode
// number followed by the raw 512-byte on-disk inode snapshot.
//
//go:briefs-disk size=560
type JrnInodeFull struct {
	Ino       uint64
	InodeData [512]byte // raw little-endian disk inode
	Reserved  [40]byte
}

// MarshalJrnInodeFull builds a JRN_INODE_FULL payload from an inode number and
// the marshaled 512-byte on-disk inode bytes.
func MarshalJrnInodeFull(ino uint64, rawInode []byte) []byte {
	r := &JrnInodeFull{Ino: ino}
	copy(r.InodeData[:], rawInode)
	b, _ := r.MarshalBinary()
	return b
}

// JrnSymlinkPrefix is the 20-byte fixed prefix of struct jrn_symlink_data
// (ino, phys, target_len). The target bytes follow as a variable-length tail
// and are appended by JrnSymlinkData.Marshal. The prefix is packed: the
// on-disk size is 20 but Go would align a struct ending in uint32 to 24, so
// the packed marker pins Size() to the declared 20 and the gen-time field-
// width sum (8+8+4=20) is verified.
//
//go:briefs-disk packed size=20
type JrnSymlinkPrefix struct {
	Ino       uint64
	Phys      uint64
	TargetLen uint32
}

// JrnSymlinkData mirrors struct jrn_symlink_data: a 20-byte prefix
// (ino, phys, target_len) followed by the target bytes (no trailing NUL).
// The runtime struct keeps the Target slice for convenience; Marshal
// serializes the codegen'd prefix then appends the raw target tail.
type JrnSymlinkData struct {
	Ino       uint64
	Phys      uint64
	TargetLen uint32
	Target    []byte
}

func (r *JrnSymlinkData) Marshal() []byte {
	p := &JrnSymlinkPrefix{
		Ino:       r.Ino,
		Phys:      r.Phys,
		TargetLen: uint32(len(r.Target)),
	}
	b, _ := p.MarshalBinary()
	b = append(b, r.Target...)
	return b
}

// JrnXattrPrefix is the 20-byte fixed prefix of struct jrn_xattr_data
// (ino, phys_block, used_size). The block content follows as a variable-
// length tail. Packed for the same reason as JrnSymlinkPrefix.
//
//go:briefs-disk packed size=20
type JrnXattrPrefix struct {
	Ino      uint64
	PhysBlk  uint64
	UsedSize uint32
}

// JrnXattrData mirrors struct jrn_xattr_data: a 20-byte prefix
// (ino, phys_block, used_size) followed by used_size bytes of block content.
// The runtime struct keeps the Data slice for convenience; Marshal serializes
// the codegen'd prefix then appends the raw content tail.
type JrnXattrData struct {
	Ino      uint64
	PhysBlk  uint64
	UsedSize uint32
	Data     []byte
}

func (r *JrnXattrData) Marshal() []byte {
	p := &JrnXattrPrefix{
		Ino:      r.Ino,
		PhysBlk:  r.PhysBlk,
		UsedSize: r.UsedSize,
	}
	b, _ := p.MarshalBinary()
	b = append(b, r.Data...)
	return b
}
// --- Unmarshal helpers (journal replay) ---
//
// These parse on-disk record payloads (little-endian, explicit offsets) back
// into Go structs. They are the inverse of the Marshal methods above and are
// used by the FUSE bridge's journal replay at mount.

// UnmarshalExtentAlloc parses an 80-byte JRN_EXTENT_ALLOC payload.
func UnmarshalExtentAlloc(b []byte) *JrnExtentAlloc {
	r := &JrnExtentAlloc{}
	if err := r.UnmarshalBinary(b); err != nil {
		return nil
	}
	return r
}

// UnmarshalExtentFree parses an 80-byte JRN_EXTENT_FREE payload.
func UnmarshalExtentFree(b []byte) *JrnExtentFree {
	r := &JrnExtentFree{}
	if err := r.UnmarshalBinary(b); err != nil {
		return nil
	}
	return r
}

// UnmarshalInodeUpdate parses an 88-byte JRN_INODE_UPDATE payload.
func UnmarshalInodeUpdate(b []byte) *JrnInodeUpdate {
	r := &JrnInodeUpdate{}
	if err := r.UnmarshalBinary(b); err != nil {
		return nil
	}
	return r
}

// UnmarshalInodeAlloc parses a 40-byte JRN_INODE_ALLOC payload.
func UnmarshalInodeAlloc(b []byte) *JrnInodeAlloc {
	r := &JrnInodeAlloc{}
	if err := r.UnmarshalBinary(b); err != nil {
		return nil
	}
	return r
}

// UnmarshalInodeFree parses a 32-byte JRN_INODE_FREE payload.
func UnmarshalInodeFree(b []byte) *JrnInodeFree {
	r := &JrnInodeFree{}
	if err := r.UnmarshalBinary(b); err != nil {
		return nil
	}
	return r
}

// UnmarshalTrieAlloc parses a 16-byte JRN_TRIE_ALLOC payload.
func UnmarshalTrieAlloc(b []byte) *JrnTrieAlloc {
	r := &JrnTrieAlloc{}
	if err := r.UnmarshalBinary(b); err != nil {
		return nil
	}
	return r
}

// UnmarshalDirUpdate parses a 280-byte JRN_DIR_UPDATE payload.
func UnmarshalDirUpdate(b []byte) *JrnDirUpdate {
	r := &JrnDirUpdate{}
	if err := r.UnmarshalBinary(b); err != nil {
		return nil
	}
	if r.NameLen > 255 {
		r.NameLen = 255
	}
	return r
}

// UnmarshalInodeFullIno returns the inode number from a JRN_INODE_FULL payload.
func UnmarshalInodeFullIno(b []byte) uint64 {
	if len(b) < 8 {
		return 0
	}
	return binary.LittleEndian.Uint64(b[0:])
}

// InodeFullRawData returns the 512-byte raw on-disk inode snapshot from a
// JRN_INODE_FULL payload (offset 8, 512 bytes). The bytes are written verbatim
// to the inode-table slot during replay (matching the kernel's memcpy of
// rec->inode_data), so the snapshot is restored without a marshal round-trip.
func InodeFullRawData(b []byte) []byte {
	if len(b) < 8+512 {
		return nil
	}
	return b[8 : 8+512]
}

// UnmarshalSymlinkData parses a JRN_SYMLINK_DATA payload (20-byte prefix +
// target bytes).
func UnmarshalSymlinkData(b []byte) *JrnSymlinkData {
	if len(b) < JrnSymlinkDataPrefix {
		return nil
	}
	var p JrnSymlinkPrefix
	if err := p.UnmarshalBinary(b); err != nil {
		return nil
	}
	tlen := p.TargetLen
	if uint32(len(b)) < JrnSymlinkDataPrefix+tlen {
		return nil
	}
	return &JrnSymlinkData{
		Ino:       p.Ino,
		Phys:      p.Phys,
		TargetLen: tlen,
		Target:    b[JrnSymlinkDataPrefix : JrnSymlinkDataPrefix+tlen],
	}
}

// UnmarshalXattrData parses a JRN_XATTR_DATA payload (20-byte prefix + used
// bytes of block content).
func UnmarshalXattrData(b []byte) *JrnXattrData {
	if len(b) < JrnXattrDataPrefix {
		return nil
	}
	var p JrnXattrPrefix
	if err := p.UnmarshalBinary(b); err != nil {
		return nil
	}
	used := p.UsedSize
	if uint32(len(b)) < JrnXattrDataPrefix+used {
		return nil
	}
	return &JrnXattrData{
		Ino:      p.Ino,
		PhysBlk:  p.PhysBlk,
		UsedSize: used,
		Data:     b[JrnXattrDataPrefix : JrnXattrDataPrefix+used],
	}
}
