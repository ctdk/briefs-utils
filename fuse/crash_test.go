package fuse

import (
	"os/exec"
	"testing"

	"github.com/ctdk/briefs-utils/briefs"
)

// TestCrashRecovery simulates a kill -9 of the FUSE bridge mid-workload by
// closing the device WITHOUT the unmount checkpoint.  Each BrieFS op commits
// its journal records + writes the inode/data blocks before returning, so the
// on-disk state is consistent even without the final checkpoint; the journal
// may carry uncommitted (or committed-but-uncheckpointed) records, which a
// kernel remount would replay.  This test verifies the host-observable result:
// fsck clean and a fresh mount reads back the data.
//
// The kernel-mount replay itself (mounting the FUSE-written image with the
// BrieFS kernel module and confirming replay) needs the VM and is exercised
// via the xfstests harness; see HANDOFF.md.
func TestCrashRecovery(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)

	// A short workload: create + write + xattr + chmod.
	in, _ := b.createInDir(1, "c", briefs.ModeFile|0o644, 1000, 1000, false)
	pat := makePattern(3, 4000)
	writeFile(t, b, in.InodeNumber, pat, 0)
	if err := b.setXattr(in.InodeNumber, "user.tag", []byte("v"), 0); err != nil {
		t.Fatalf("setxattr: %v", err)
	}
	if err := b.setattrOp(in.InodeNumber, setattrReq(fattrMode, withMode(briefs.ModeFile|0o640))); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	// "Crash": close WITHOUT the unmount checkpoint (the journal may carry
	// uncheckpointed-but-committed records).
	b.dev.Close()

	// fsck on the crashed image: clean (each op committed a consistent state).
	fsck := buildBinary(t, "github.com/ctdk/briefs-utils/cmd/fsck", "fsck.briefs")
	out, err := exec.Command(fsck, img).CombinedOutput()
	if err != nil {
		t.Fatalf("fsck after crash: %v\n%s", err, out)
	}
	if !contains(string(out), "FSCK COMPLETE: no errors found") {
		t.Fatalf("fsck not clean after crash:\n%s", out)
	}

	// Fresh mount reads back the data + xattr + mode.
	b2 := openBridge(t, img)
	if got := readFile(t, b2, in.InodeNumber, 0, 4000); !bytesEqual(got, pat) {
		t.Fatalf("post-crash readback mismatch")
	}
	if v, _ := b2.getXattr(in.InodeNumber, "user.tag"); string(v) != "v" {
		t.Fatalf("post-crash xattr: want v, got %q", string(v))
	}
	di, _ := b2.inodes.ReadInode(in.InodeNumber)
	if di.Filemode&0o7777 != 0o640 {
		t.Fatalf("post-crash mode: want 0640, got %o", di.Filemode&0o7777)
	}
	_ = b2.journal.Checkpoint()
	_ = b2.dev.Sync()
}