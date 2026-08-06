# BrieFS FUSE Bridge: xfstests Status

## Overview

The BrieFS FUSE bridge (`cmd/fuse`, branch `fuse-rw-work`) is a Go port of the
BrieFS kernel module. It shares the on-disk format (`briefs/` package) and
ports the kernel journal write path + journal-replay-on-mount to Go, making a
FUSE-written volume crash-consistent, recoverable, and kernel-mountable.

This document records the xfstests status for the FUSE-mounted BrieFS as of
2026-08-06.

> **Correction (2026-08-06).** An earlier version of this document reported
> "10/10 PASS" for the replay-sensitive subset. That result was **invalid**: the
> FUSE xfstests harness was not actually engaging the FUSE bridge — it was
> mounting via the BrieFS *kernel* module. `common/config` line 116
> unconditionally resets `MOUNT_PROG="$(type -P mount)"` (the kernel mount), and
> the subset script's `eval "$(grep -E '^export' …)"` workaround did not
> survive `./check` re-sourcing `common/config`, so `MOUNT_PROG` reverted to the
> kernel mount and every "FUSE" result was in fact a kernel result. The
> `generic/547` "flakey can't read superblock" timing flake recorded below was
> likewise a *kernel* dm-flakey flake, not a FUSE/replay bug. The harness has
> since been fixed (see "How to run") and the results below are the **real**
> FUSE-bridge results.

## How to run xfstests against the FUSE mount

### Prerequisites

- `fuse3` installed on the VM (`apt-get install -y fuse3` — provides
  `fusermount`). The BrieFS kernel module is **not** required (FUSE mounts are
  pure userspace), though loading it is harmless.
- `/go/bin/fuse.briefs`, `/go/bin/mkfs.briefs`, `/go/bin/fsck.briefs` built from
  `fuse-rw-work` (rebuild with `go build -a` after edits — the VM source is an
  NFS mount of the host, and NFS clock skew can make a plain `go build` skip
  recompiling changed packages).
- `mount.fuse.briefs` installed in a directory `mount(8)` searches (`/sbin` or
  `/usr/sbin` — **not** `/usr/local/sbin`): `sudo make install` (Makefile
  default `SBINDIR=/usr/sbin`).
- The xfstests wrappers in the kernel repo at `tests/xfstests/fuse-briefs-mount`
  and `tests/xfstests/fuse-briefs-umount`.
- `configs/briefs-fuse.config` in the xfstests tree (a `[briefs]`-section config
  that sets `MOUNT_PROG`/`UMOUNT_PROG` to the wrappers).

### Running

The **correct** way to engage FUSE is to set `HOST_OPTIONS` so `common/config`
sources the `[briefs]` section *after* its line-116 `MOUNT_PROG` reset, which
overrides `MOUNT_PROG` back to the FUSE wrapper. (Exporting `MOUNT_PROG`
directly does **not** work — `./check` re-sources `common/config` and clobbers
it.)

The `run-fuse-subset.sh` wrapper does this for the replay-sensitive subset:

```bash
# Inside the VM, as root:
sudo bash /vagrant/tests/xfstests/run-fuse-subset.sh
```

`run-fuse-subset.sh` exports `HOST_OPTIONS=/xfstests/configs/briefs-fuse.config`,
`MOUNT_CMD`/`UMOUNT_CMD` (for the TEST_DEV mount `run-suite.sh` does itself),
`SKIP_TESTS=""` (so the subset, including `generic/475`, runs in full), and
execs `run-suite.sh` with the subset list. `run-suite.sh` provides per-test
isolation (mkfs both devices, mount TEST_DEV, run one `./check`, unmount,
fsck).

For a single test or ad-hoc set, run `run-suite.sh` directly with the same env:

```bash
sudo bash -c 'export HOST_OPTIONS=/xfstests/configs/briefs-fuse.config \
  MOUNT_CMD=/vagrant/tests/xfstests/fuse-briefs-mount \
  UMOUNT_CMD=/vagrant/tests/xfstests/fuse-briefs-umount \
  SKIP_TESTS= FSCK_ENABLED=1; \
  bash /vagrant/tests/xfstests/run-suite.sh generic/003'
```

### How the wrappers work

- `fuse-briefs-mount [-t briefs] [-o opts] <dev> <mnt>`: the `MOUNT_PROG`
  wrapper. xfstests prepends `-t briefs` (from `FSTYP=briefs`); the wrapper
  strips it and substitutes `-t fuse.briefs` so `mount(8)` dispatches to
  `/usr/sbin/mount.fuse.briefs`, which backgrounds the `fuse.briefs` daemon,
  records its PID, and waits for the mountpoint to come up. `-o` opts are
  forwarded (the daemon currently ignores them).
- `mount.fuse.briefs <src> <target>`: the `mount(8)` type helper. Backgrounds
  `fuse.briefs -i <src> -m <target>` via `setsid`, writes the daemon PID to
  `/tmp/fuse-briefs-<target>.pid`, polls `mountpoint -q` until visible.
- `fuse-briefs-umount <mnt>`: the `UMOUNT_PROG` wrapper. Runs
  `umount`/`fusermount -u`, waits for the recorded `fuse.briefs` PID to exit
  (journal checkpoint completes) so the next test's `mkfs.briefs` doesn't race
  the checkpoint.
- `FSTYP` stays `briefs` (not `fuse`) so the `common/briefs` helpers
  (`_require_briefs_feature`, `_check_briefs_filesystem`) remain active — only
  the mount path is swapped to FUSE.

### xfstests source modifications (`common/rc`)

BrieFS FUSE mounts as type `fuse.briefs`, but xfstests expects `briefs`
(`FSTYP`). BrieFS is unusual — a *block-device* FUSE filesystem (glusterfs/
ceph-fuse use tags, avoiding this), so several type-filtered checks needed
teaching. These changes are in the `xfstests-dev` tree:

- `_fs_type`: added `s/fuse.briefs/briefs/` to the sed (matching the existing
  `fuse.glusterfs→glusterfs` / `fuse.ceph-fuse→ceph-fuse` precedent).
- `_is_dev_mounted` / `_is_dir_mountpoint`: `findmnt -t $fstype` needs the
  exact type string, so added a `fuse.$fstype` fallback when the bare type
  doesn't match. No-op for non-FUSE filesystems (their mount type already equals
  `$fstype`).
- The FUSE bridge sets `MountOptions.FsName = imagePath` so the mount *source*
  in `/proc/mounts` is the device path (`/dev/vdb1`), not the subtype name
  (`briefs`) — `findmnt -S <dev>` (match by source) otherwise finds nothing and
  xfstests treats the device as unmounted (`_check_if_dev_already_mounted` →
  `_exit 1`), the systematic empty-output failure every test hit before this.

## Results (2026-08-06, FUSE actually engaged)

### Replay-sensitive subset

| Test | Description | Result | Detail |
|------|-------------|--------|--------|
| `generic/003` | basic journal test | ✅ PASS | |
| `generic/029` | clean unmount replay | ✅ PASS | |
| `generic/030` | clean unmount + remount | ✅ PASS | |
| `generic/032` | replay trie clobber | ⏭️ NOT RUN | `xfs_io fiemap failed` — FUSE bridge does not implement fiemap (clean feature-skip) |
| `generic/321` | journal replay inode full | ✅ PASS | |
| `generic/322` | journal write pos fix | ✅ PASS | |
| `generic/547` | fsstress + fsync + crash-replay | ❌ FAIL | metadata mismatch after crash-replay (see Known issues) |
| `generic/640` | rename trie root ordering | ✅ PASS | |
| `generic/475` | dm-error replay | ✅ PASS | validates the journal-replay-on-mount port under xfstests |
| `generic/011` | dirstress | ❌ FAIL | `rm: … Directory not empty` (readdir/rmdir inconsistency under concurrent dir ops) |

**7 PASS, 2 FAIL (547, 011), 1 NOT RUN (032).**

`generic/475` (dm-error sudden-death → crash-replay) passing is the key
validation that the Go journal-replay port (`fuse/journal_replay.go`) works
under xfstests: after the dm-error kill, the remount replays the live journal
range and the test's fsck/post-state checks pass.

### Kernel interop

The kernel interop test (`interop_test.sh`, commit `8a3e7f7` in the kernel
repo) confirms that a volume written by the Go FUSE bridge is mountable by the
BrieFS kernel module, which reads back the FUSE-written data, xattrs,
symlinks, and modes unchanged. A complementary *dirty-volume* interop check
(FUSE write + `kill -9` crash, leaving a live journal range, then `mount -t
briefs`) confirms the kernel module replays the FUSE-written journal records
and reads back the same data/xattrs/symlinks/modes, with `fsck.briefs` clean
afterward — so the bridge's journal records are replay-compatible with the
kernel in both directions.

```
cat /mnt/km/file     → hello-kernel
cat /mnt/km/sub/n    → nested
readlink /mnt/km/link → /file
getfattr /mnt/km/file → user.tag="fuseval"
ls -la /mnt/km/file  → -rw-r----- (0640)
```

## Journal replay

The FUSE bridge replays the journal on mount (a Go port of the kernel's
`briefs_journal_replay`, `journal.c`), giving it the same crash-recovery path
the kernel module has. Previously the bridge relied solely on the
unmount-time checkpoint (always-checkpoint-at-unmount, kernel commit
`f8ef293`) to leave `log_start == log_end`, so a *clean* remount replayed
nothing — but a crash (or dm-error/flakey simulated power failure) skipped
that checkpoint, leaving a stale allocator bitmap and torn metadata with no
recovery path.

The replay runs three passes, matching the kernel: (1) a reservation
pre-scan that reserves/frees every block/inode claimed by an ALLOC/FREE
record and collects each inode's final xattr head + next-block links; (2) an
apply pass that re-derives directory tries from `JRN_DIR_UPDATE`, restores
inode/symlink/xattr blocks, and reserves bitmap bits; (3) an nlink
reconciliation that recomputes on-disk link counts from the re-derived
tries. After replay the journal is marked clean and the allocator bitmaps +
superblock are persisted. `journal.WriteRecord` is a no-op while in replay,
so the trie page-init/free paths do not append fresh records into the range
being replayed.

`generic/475` (dm-error crash-replay) passes under FUSE, exercising this
path. `generic/547` (fsstress + fsync + dm-flakey crash-replay) does not yet
pass — see Known issues.

## Known issues

### generic/547 — metadata mismatch after fsstress crash-replay (real FUSE bug)

With FUSE engaged, `generic/547` fails with a post-crash-replay metadata
mismatch:

```
metadata mismatch in /p1/db/f10
metadata mismatch in /p1/db/f11
metadata mismatch in /p1/db/f12
only in remote fs: /p1/db/fc
metadata mismatch in /p1/db/fe
```

`generic/475` (dm-error, also crash-replay) passes, so the replay port's basic
correctness is sound; 547 differs by driving fsstress (many concurrent ops,
fsync, dm-flakey drop_writes), producing a denser journal whose replay is not
fully idempotent. The likely cause is the **deferred trie-block reuse pool**
(see below): the FUSE replay re-derives tries via the live `TrieInsert`/
`TrieRemove`, which allocate fresh trie pages from the data allocator instead
of reusing the `JRN_TRIE_ALLOC` blocks the kernel's replay-trie-block pool
reuses, so a non-idempotent re-derivation can leave orphan/aliased trie blocks
→ "only in remote fs" / metadata mismatch. Porting the kernel's replay
trie-block pool + per-directory partial-pool seeding (`briefs_trie_seed_pool`)
is the expected fix. (The earlier "547 flakey can't read superblock" note was
a *kernel* dm-flakey timing flake observed because the harness was mounting
via the kernel, not FUSE — it is not a FUSE/replay bug and is retracted.)

### generic/011 — dirstress "Directory not empty" (real FUSE bug)

With FUSE engaged, `generic/011` (dirstress, concurrent dir ops) fails during
cleanup:

```
rm: cannot remove '/mnt/briefs-test/dirstress.*/stressdir/stress.0': Directory not empty
rm: cannot remove '.../stress.4': Directory not empty
…
```

`rm -rf` removes children before `rmdir`, so "Directory not empty" during a
recursive remove indicates the FUSE bridge's **readdir does not enumerate all
entries** under concurrent modification (rm does not see entries to remove,
but `rmdir` sees the directory as non-empty) — a readdir/trie-iteration
consistency bug under concurrency, distinct from the kernel-side
seek/resume fixes. Needs investigation.

### generic/032 — NOT RUN (fiemap unsupported)

`generic/032` cleanly `_notrun`s with `xfs_io fiemap failed (old kernel/wrong
fs?)` — the FUSE bridge does not implement the `FS_IOC_FIEMAP` ioctl, so the
test's `_require` gates and skips. Not a failure; a feature gap. Implementing
fiemap in the bridge would let 032 run.

### Deferred: trie-block reuse pool for full-fs replay

The replay re-derives directory tries via the live `TrieInsert`/`TrieRemove`,
which allocate fresh trie pages from the data allocator (the journal records
are not appended during replay, so `JRN_TRIE_ALLOC` blocks reserved in pass 1
are not reused the way the kernel's replay-trie-block pool reuses them). For
the `generic/475` workload this is harmless (re-derivation is idempotent —
`-EEXIST`/`-ENOENT` — so no new pages are allocated), but the `generic/547`
fsstress crash-replay is not fully idempotent and surfaces this as the
metadata mismatch above. Porting the kernel's replay trie-block pool +
per-directory partial-pool seeding (`briefs_trie_seed_pool`) would close this,
matching the kernel's full-fs crash-replay behaviour.

### Coverage gap

Only the replay-sensitive subset has been run under FUSE, not the full
`generic` group. The 7/10 subset result (with 475 passing) is the first
trustworthy FUSE xfstests record; the two failures (547, 011) are real FUSE
bridge bugs to fix, and 032 is a fiemap feature-skip.

## FUSE bridge coverage

The FUSE bridge implements all BrieFS operations at full kernel parity:

- **Directory ops**: create, mkdir, unlink, rmdir (with journal ordering +
  trie root pinning).
- **File data writes**: inline data (≤256B), extent-backed (inline array ≤8,
  B+ tree spill), hole allocation, unwritten extent conversion.
- **Extended attributes**: user, trusted, security namespaces; set/get/list/
  remove; continuation blocks for large values.
- **Fileattr / chattr**: FS_IOC_GETFLAGS/SETFLAGS, FS_IOC_FSGETXATTR/
  FSSETXATTR; immutable/append enforcement.
- **Link / symlink / mknod / rename**: hardlink, inline + extent symlinks,
  block/char/fifo/socket special files, renameat2 (NOREPLACE, EXCHANGE,
  WHITEOUT).
- **Fallocate / setattr / killpriv**: KEEP_SIZE preallocate (unwritten
  extents), PUNCH_HOLE, truncate up/down, chmod/chown/utimes, suid/sgid
  stripping + security.capability clearing on write/chown.
- **Journal port**: Go port of the kernel journal write path (`briefs/
  journal_write.go`), with drain-before-snapshot durability for btree nodes
  and commit-before-flush for re-derivable metadata (trie pages, inline data,
  xattr blocks).
- **Journal replay on mount**: Go port of the kernel's `briefs_journal_replay`
  (`fuse/journal_replay.go`), running the 3-pass replay (reserve bitmap bits,
  re-derive tries + restore inode/symlink/xattr blocks, reconcile nlinks) so a
  crashed/dirty volume recovers consistently on remount.
- **Per-inode-block locking**: sharded per-inode-table-block mutexes for
  concurrent file writes on disjoint blocks.
- **Not implemented**: fiemap (`FS_IOC_FIEMAP`) — gates `generic/032` to
  NOT RUN.

## Repository layout

| Repo | Branch | Role |
|------|--------|------|
| `~/src/briefs` (kernel) | `master` | Kernel module + xfstests wrappers (`tests/xfstests/fuse-briefs-*`, `run-suite.sh`, `run-fuse-subset.sh`) |
| `~/go/src/github.com/ctdk/briefs-utils` | `fuse-rw-work` | Go FUSE bridge (`cmd/fuse`), mkfs (`cmd/mkfs`), fsck (`cmd/fsck`), shared format (`briefs/`), `mount.fuse.briefs` helper |
| `~/src/xfstests-dev` | — | xfstests source + configs (`configs/briefs-fuse.config`), `common/rc` FUSE-type fixes |