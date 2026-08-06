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
type JrnExtentAlloc struct {
	Ino         uint64
	Offset      uint64
	Length      uint64
	PhysStart   uint64
	ExtentIndex uint32
}

func (r *JrnExtentAlloc) Marshal() []byte {
	b := make([]byte, JrnExtentAllocSize)
	binary.LittleEndian.PutUint64(b[0:], r.Ino)
	binary.LittleEndian.PutUint64(b[8:], r.Offset)
	binary.LittleEndian.PutUint64(b[16:], r.Length)
	binary.LittleEndian.PutUint64(b[24:], r.PhysStart)
	binary.LittleEndian.PutUint32(b[32:], r.ExtentIndex)
	// reserved[44] at 36 stays zero
	return b
}

// JrnExtentFree mirrors struct jrn_extent_free (80 bytes).
type JrnExtentFree struct {
	Ino       uint64
	Offset    uint64
	PhysStart uint64
	Length    uint64
}

func (r *JrnExtentFree) Marshal() []byte {
	b := make([]byte, JrnExtentFreeSize)
	binary.LittleEndian.PutUint64(b[0:], r.Ino)
	binary.LittleEndian.PutUint64(b[8:], r.Offset)
	binary.LittleEndian.PutUint64(b[16:], r.PhysStart)
	binary.LittleEndian.PutUint64(b[24:], r.Length)
	// reserved[48] at 32 stays zero
	return b
}

// JrnInodeUpdate mirrors struct jrn_inode_update (88 bytes).
type JrnInodeUpdate struct {
	Ino       uint64
	Mode      uint32
	Nlink     uint32
	Uid       uint32
	Gid       uint32
	Size      uint64
	ATimeSec  uint64
	ATimeNsec uint64
	MTimeSec  uint64
	MTimeNsec uint64
	CTimeSec  uint64
	CTimeNsec uint64
	Flags     uint32
}

func (r *JrnInodeUpdate) Marshal() []byte {
	b := make([]byte, JrnInodeUpdateSize)
	binary.LittleEndian.PutUint64(b[0:], r.Ino)
	binary.LittleEndian.PutUint32(b[8:], r.Mode)
	binary.LittleEndian.PutUint32(b[12:], r.Nlink)
	binary.LittleEndian.PutUint32(b[16:], r.Uid)
	binary.LittleEndian.PutUint32(b[20:], r.Gid)
	binary.LittleEndian.PutUint64(b[24:], r.Size)
	binary.LittleEndian.PutUint64(b[32:], r.ATimeSec)
	binary.LittleEndian.PutUint64(b[40:], r.ATimeNsec)
	binary.LittleEndian.PutUint64(b[48:], r.MTimeSec)
	binary.LittleEndian.PutUint64(b[56:], r.MTimeNsec)
	binary.LittleEndian.PutUint64(b[64:], r.CTimeSec)
	binary.LittleEndian.PutUint64(b[72:], r.CTimeNsec)
	binary.LittleEndian.PutUint32(b[80:], r.Flags)
	// reserved(4) at 84 stays zero
	return b
}

// JrnInodeAlloc mirrors struct jrn_inode_alloc (40 bytes).
type JrnInodeAlloc struct {
	Ino   uint64
	Mode  uint32
	Nlink uint32
	Uid   uint32
	Gid   uint32
}

func (r *JrnInodeAlloc) Marshal() []byte {
	b := make([]byte, JrnInodeAllocSize)
	binary.LittleEndian.PutUint64(b[0:], r.Ino)
	binary.LittleEndian.PutUint32(b[8:], r.Mode)
	binary.LittleEndian.PutUint32(b[12:], r.Nlink)
	binary.LittleEndian.PutUint32(b[16:], r.Uid)
	binary.LittleEndian.PutUint32(b[20:], r.Gid)
	// reserved1(4) at 24, reserved2(8) at 28 stay zero
	return b
}

// JrnInodeFree mirrors struct jrn_inode_free (32 bytes).
type JrnInodeFree struct {
	Ino uint64
}

func (r *JrnInodeFree) Marshal() []byte {
	b := make([]byte, JrnInodeFreeSize)
	binary.LittleEndian.PutUint64(b[0:], r.Ino)
	// reserved[3] at 8, reserved2(8) at 20 stay zero
	return b
}

// JrnTrieAlloc mirrors struct jrn_trie_alloc (16 bytes). Op is 0 for
// allocated, 1 for freed.
type JrnTrieAlloc struct {
	Block uint64
	Op    uint32 // 0 = allocated, 1 = freed
}

func (r *JrnTrieAlloc) Marshal() []byte {
	b := make([]byte, JrnTrieAllocSize)
	binary.LittleEndian.PutUint64(b[0:], r.Block)
	binary.LittleEndian.PutUint32(b[8:], r.Op)
	// reserved(4) at 12 stays zero
	return b
}

// JrnDirUpdate mirrors struct jrn_dir_update (280 bytes). Op is 0 for add,
// 1 for delete. FType is the d_type form (S_IFMT >> 12): 4=dir, 8=reg,
// 10=lnk, etc.
type JrnDirUpdate struct {
	ParentIno uint64
	ChildIno  uint64
	Name      string // <= 255 bytes
	Op        uint8  // 0 = add, 1 = delete
	FType     uint8
}

func (r *JrnDirUpdate) Marshal() []byte {
	b := make([]byte, JrnDirUpdateSize)
	binary.LittleEndian.PutUint64(b[0:], r.ParentIno)
	binary.LittleEndian.PutUint64(b[8:], r.ChildIno)
	binary.LittleEndian.PutUint32(b[16:], uint32(len(r.Name)))
	if len(r.Name) > 255 {
		copy(b[20:275], r.Name[:255])
	} else {
		copy(b[20:], r.Name)
	}
	b[275] = r.Op
	b[276] = r.FType
	// reserved(1) at 277, pad(2) at 278 stay zero
	return b
}

// JrnInodeFull mirrors struct jrn_inode_full (560 bytes): an 8-byte inode
// number followed by the raw 512-byte on-disk inode snapshot.
type JrnInodeFull struct {
	Ino       uint64
	InodeData [512]byte // raw little-endian disk inode
}

// MarshalJrnInodeFull builds a JRN_INODE_FULL payload from an inode number and
// the marshaled 512-byte on-disk inode bytes.
func MarshalJrnInodeFull(ino uint64, rawInode []byte) []byte {
	b := make([]byte, JrnInodeFullSize)
	binary.LittleEndian.PutUint64(b[0:], ino)
	copy(b[8:520], rawInode) // 512 bytes of inode data
	// reserved[40] at 520 stays zero
	return b
}

// JrnSymlinkData mirrors struct jrn_symlink_data: a 20-byte prefix
// (ino, phys, target_len) followed by the target bytes (no trailing NUL).
type JrnSymlinkData struct {
	Ino       uint64
	Phys      uint64
	TargetLen uint32
	Target    []byte
}

func (r *JrnSymlinkData) Marshal() []byte {
	b := make([]byte, JrnSymlinkDataPrefix+len(r.Target))
	binary.LittleEndian.PutUint64(b[0:], r.Ino)
	binary.LittleEndian.PutUint64(b[8:], r.Phys)
	binary.LittleEndian.PutUint32(b[16:], uint32(len(r.Target)))
	copy(b[20:], r.Target)
	return b
}

// JrnXattrData mirrors struct jrn_xattr_data: a 20-byte prefix
// (ino, phys_block, used_size) followed by used_size bytes of block content.
type JrnXattrData struct {
	Ino      uint64
	PhysBlk  uint64
	UsedSize uint32
	Data     []byte
}

func (r *JrnXattrData) Marshal() []byte {
	b := make([]byte, JrnXattrDataPrefix+len(r.Data))
	binary.LittleEndian.PutUint64(b[0:], r.Ino)
	binary.LittleEndian.PutUint64(b[8:], r.PhysBlk)
	binary.LittleEndian.PutUint32(b[16:], r.UsedSize)
	copy(b[20:], r.Data)
	return b
}
// --- Unmarshal helpers (journal replay) ---
//
// These parse on-disk record payloads (little-endian, explicit offsets) back
// into Go structs. They are the inverse of the Marshal methods above and are
// used by the FUSE bridge's journal replay at mount.

// UnmarshalExtentAlloc parses an 80-byte JRN_EXTENT_ALLOC payload.
func UnmarshalExtentAlloc(b []byte) *JrnExtentAlloc {
	if len(b) < JrnExtentAllocSize {
		return nil
	}
	return &JrnExtentAlloc{
		Ino:         binary.LittleEndian.Uint64(b[0:]),
		Offset:      binary.LittleEndian.Uint64(b[8:]),
		Length:      binary.LittleEndian.Uint64(b[16:]),
		PhysStart:   binary.LittleEndian.Uint64(b[24:]),
		ExtentIndex: binary.LittleEndian.Uint32(b[32:]),
	}
}

// UnmarshalExtentFree parses an 80-byte JRN_EXTENT_FREE payload.
func UnmarshalExtentFree(b []byte) *JrnExtentFree {
	if len(b) < JrnExtentFreeSize {
		return nil
	}
	return &JrnExtentFree{
		Ino:       binary.LittleEndian.Uint64(b[0:]),
		Offset:    binary.LittleEndian.Uint64(b[8:]),
		PhysStart: binary.LittleEndian.Uint64(b[16:]),
		Length:    binary.LittleEndian.Uint64(b[24:]),
	}
}

// UnmarshalInodeUpdate parses an 88-byte JRN_INODE_UPDATE payload.
func UnmarshalInodeUpdate(b []byte) *JrnInodeUpdate {
	if len(b) < JrnInodeUpdateSize {
		return nil
	}
	return &JrnInodeUpdate{
		Ino:       binary.LittleEndian.Uint64(b[0:]),
		Mode:      binary.LittleEndian.Uint32(b[8:]),
		Nlink:     binary.LittleEndian.Uint32(b[12:]),
		Uid:       binary.LittleEndian.Uint32(b[16:]),
		Gid:       binary.LittleEndian.Uint32(b[20:]),
		Size:      binary.LittleEndian.Uint64(b[24:]),
		ATimeSec:  binary.LittleEndian.Uint64(b[32:]),
		ATimeNsec: binary.LittleEndian.Uint64(b[40:]),
		MTimeSec:  binary.LittleEndian.Uint64(b[48:]),
		MTimeNsec: binary.LittleEndian.Uint64(b[56:]),
		CTimeSec:  binary.LittleEndian.Uint64(b[64:]),
		CTimeNsec: binary.LittleEndian.Uint64(b[72:]),
		Flags:     binary.LittleEndian.Uint32(b[80:]),
	}
}

// UnmarshalInodeAlloc parses a 40-byte JRN_INODE_ALLOC payload.
func UnmarshalInodeAlloc(b []byte) *JrnInodeAlloc {
	if len(b) < JrnInodeAllocSize {
		return nil
	}
	return &JrnInodeAlloc{
		Ino:   binary.LittleEndian.Uint64(b[0:]),
		Mode:  binary.LittleEndian.Uint32(b[8:]),
		Nlink: binary.LittleEndian.Uint32(b[12:]),
		Uid:   binary.LittleEndian.Uint32(b[16:]),
		Gid:   binary.LittleEndian.Uint32(b[20:]),
	}
}

// UnmarshalInodeFree parses a 32-byte JRN_INODE_FREE payload.
func UnmarshalInodeFree(b []byte) *JrnInodeFree {
	if len(b) < JrnInodeFreeSize {
		return nil
	}
	return &JrnInodeFree{
		Ino: binary.LittleEndian.Uint64(b[0:]),
	}
}

// UnmarshalTrieAlloc parses a 16-byte JRN_TRIE_ALLOC payload.
func UnmarshalTrieAlloc(b []byte) *JrnTrieAlloc {
	if len(b) < JrnTrieAllocSize {
		return nil
	}
	return &JrnTrieAlloc{
		Block: binary.LittleEndian.Uint64(b[0:]),
		Op:    binary.LittleEndian.Uint32(b[8:]),
	}
}

// UnmarshalDirUpdate parses a 280-byte JRN_DIR_UPDATE payload.
func UnmarshalDirUpdate(b []byte) *JrnDirUpdate {
	if len(b) < JrnDirUpdateSize {
		return nil
	}
	nameLen := binary.LittleEndian.Uint32(b[16:])
	if nameLen > 255 {
		nameLen = 255
	}
	return &JrnDirUpdate{
		ParentIno: binary.LittleEndian.Uint64(b[0:]),
		ChildIno:  binary.LittleEndian.Uint64(b[8:]),
		Name:      string(b[20 : 20+nameLen]),
		Op:        b[275],
		FType:     b[276],
	}
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
	tlen := binary.LittleEndian.Uint32(b[16:])
	if uint32(len(b)) < JrnSymlinkDataPrefix+tlen {
		return nil
	}
	return &JrnSymlinkData{
		Ino:       binary.LittleEndian.Uint64(b[0:]),
		Phys:      binary.LittleEndian.Uint64(b[8:]),
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
	used := binary.LittleEndian.Uint32(b[16:])
	if uint32(len(b)) < JrnXattrDataPrefix+used {
		return nil
	}
	return &JrnXattrData{
		Ino:      binary.LittleEndian.Uint64(b[0:]),
		PhysBlk:  binary.LittleEndian.Uint64(b[8:]),
		UsedSize: used,
		Data:     b[JrnXattrDataPrefix : JrnXattrDataPrefix+used],
	}
}
