# BrieFS FUSE Bridge: xfstests Status

## Overview

The BrieFS FUSE bridge (`cmd/fuse`) is a Go port of the
BrieFS kernel module. It shares the on-disk format (`briefs/` package) and
ports the kernel journal write path + journal-replay-on-mount to Go, making a
FUSE-written volume crash-consistent, recoverable, and kernel-mountable.

This document records the xfstests status for the FUSE-mounted BrieFS as of
2026-09-15 — the close of the 09-06→09-14 campaign series, which took the
full generic suite from 188 PASS / 110 FAIL / 63 HANG to
328 / 32 / 0, plus the 2026-09-15 re-validation of the 18-commit
re-review refactor series at byte-identical per-test status
(see "Current status").

> **Correction history — two harness bugs, two invalid records.**
>
> 1. *(2026-08-06, first record)* "10/10 PASS" was invalid: the harness was
>    not engaging the FUSE bridge at all. Fixed by the `HOST_OPTIONS` +
>    `configs/briefs-fuse.config` approach (`MOUNT_PROG`/`UMOUNT_PROG`
>    wrappers).
> 2. *(2026-08-06, second record, 7/10 PASS)* — **also invalid for every
>    scratch-mounting test.** `common/config` resets
>    `MOUNT_PROG="$(type -P mount)"` unconditionally (line ~116), but the
>    `[briefs]` section override only re-parses when `CONFIG_INCLUDED` is
>    unset. `./check` exports `CONFIG_INCLUDED=true`, so **every test child**
>    re-sources `common/config` and silently reverts to the plain kernel
>    mount (`SHELLOPTS=xtrace` repro: child trace shows
>    `+ /usr/bin/mount -t briefs /dev/vdc1 /mnt/briefs-scratch`). With the
>    kernel module loaded, those child kernel mounts silently *succeeded*, so
>    the Aug-6 results for 003 029 030 321 322 547 640 475 were all
>    kernel-backed. Only `run-suite.sh`'s TEST_DEV mounts — and hence
>    `generic/011`, which uses `$TEST_DIR` — ever genuinely exercised the
>    bridge.
>
> Both are now fixed (see "How to run"); the results below are the first
> trustworthy per-test FUSE-bridge record, from the 2026-09-06 re-run with the
> kernel module **removed** so any residual kernel mount fails loudly instead
> of silently succeeding.

## Current status (2026-09-15)

Full generic suite (793 tests, kernel module removed, one mkfs/mount per
test via the wrappers). The four most recent full runs:

| Run | PASS | FAIL | NOT RUN | SKIP | HANG |
|-----|-----:|-----:|--------:|-----:|-----:|
| 20260912-132029 (baseline, post 09-08..09-12 campaigns) | 300 | 59 | 431 | 2 | 1 |
| 20260913-201548 (Family 1 fixed) | 327 | 33 | 431 | 2 | 0 |
| 20260914-021234 (closing run, `-o` plumbing fixed) | **328** | **32** | 431 | 2 | **0** |
| 20260915-212640 (re-review refactor series a24c72d re-validated) | **328** | **32** | 431 | 2 | **0** |

- **NOT RUN 431** is a constant set across all three runs (430 in the
  2026-09-07 first honest run) — tests whose prerequisites the FUSE
  harness cannot meet (fiemap, exchange-range, O_TMPFILE, quota, dax, …).
- **SKIP (2)**: generic/475 (dm-error soak wedge — see Known issues) and
  generic/492.
- Closing run vs baseline: 27 FAIL→PASS (the 20 Family-1 tests plus
  126 237 294 317 318 452 547), 476 HANG→PASS, and **zero PASS→FAIL
  regressions** — the campaign's acceptance criterion.

### The 32 remaining FAILs

All 32 were failing at the 2026-09-12 baseline and are untouched by the
Family-1 campaign — previously triaged residue, no new failures:

```
003 026 079 099 108 184 192 213 250 252 285 306 319 423 424 426
434 441 467 477 484 500 504 512 520 537 563 589 631 732 741 756
```

Known characterizations:

- **003** — atime never updated on read (open bridge gap; see Known
  issues).
- **250, 252** — DIO stress shapes that the kernel module passes and the
  bridge does not.
- **520** — `sync(2)` is a structural no-op on a non-fuseblk FUSE mount
  (accepted; see the coverage gaps section).
- **563** — cgroup writeback; the kernel module also accepts this failure
  (`SB_I_CGROUPWB` dropped for the 6.12 CVE-2026-31703 iput race).
- **537** — the ro/dax mount-option family.

### Family 1 — permission/setid/privilege tests (closed 2026-09-13)

generic/087 088 093 125 193 256 314 355 375 444 597 598 633 680 683
684 685 688 696 697 — all 20 failed at the baseline; all 20 PASS since
run-20260913-200549. Nine root causes, all fixed in this repo on
`bu-refactor-1`:

| Root cause | Fix |
|---|---|
| No `allow_other`: `fuse_permissible_uidgid` EACCES'd every non-daemon-uid process before any semantics ran | `628bd48` |
| Unconditional killpriv — the kernel strips nothing on fallocate without killpriv_v2, so the daemon must, gated per fs/attr.c (CAP_FSETID, S_ISREG, S_IXGRP/in-group); security.capability removal always | `9aa8ae6` (caller status from /proc) + `d319c89` (gating) |
| FUSE never runs `inode_init_owner`: setgid-directory gid inheritance missing | `846c5b6` |
| Create modes applied verbatim — no umask, no default-ACL computation | `2aff8e9` |
| `fc->dont_mask` comes only from the daemon's init reply (FUSE_DONT_MASK), not SB_POSIXACL — the kernel pre-masked create modes with the umask | `0eb4d12` |
| The kernel delegates the ACL→mode update to the daemon on `setfacl`; the bridge stored the access-ACL blob verbatim | `1b2528c` |
| `fuse_setattr` never runs `posix_acl_chmod`: a chmod left the stored access ACL stale, so a later matching `setfacl` issued no setxattr | `c4688dc` |
| fusermount3 mounts nosuid,nodev,noexec — setid exec from the mount impossible | `5a2093b` |
| `mount.fuse.briefs` discarded the `-o` value, so `mount -t fuse.briefs -o nosuid` never reached the FUSE mount (generic/128 was a vacuous baseline pass unmasked by allow_other) | `9bd009c` |

The detailed R1–R8 root-cause table, the validation-ladder history, and
the go-fuse 2.10.1 contract facts (no `setxattr_flags` without
FUSE_SETXATTR_EXT; CAP numbering follows the kernel uapi not libfuse;
CAP_DONT_MASK via `ExtraCapabilities`) live in the kernel repo's
`tests/xfstests/xfstests-fuse-status.md`; run archives in the kernel
repo's `tests/xfstests/runs/` (closing archive
`run-20260914-021234-fuse.txt`).

## How to run xfstests against the FUSE mount

### Prerequisites

- `fuse3` installed on the VM (`apt-get install -y fuse3` — provides
  `fusermount`). The BrieFS kernel module is **not** required (FUSE mounts are
  pure userspace).
- `/go/bin/fuse.briefs`, `/go/bin/mkfs.briefs`, `/go/bin/fsck.briefs` built from
  the briefs-utils repo (rebuild with `go build -a` after edits — the VM source is an
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

**Remove the kernel module first.** `MODULE_ALIAS_FS("briefs")` makes
`mount -t briefs` auto-load `briefs_fs`, so an installed module turns any
accidental kernel mount into a silent false pass — the exact failure mode that
invalidated both earlier records. For a FUSE run:

```bash
sudo rmmod briefs_fs 2>/dev/null
sudo mv /lib/modules/$(uname -r)/extra/briefs/briefs_fs.ko /root/briefs_fs.ko.current
sudo depmod -a
```

(restore with the reverse `mv` + `depmod -a` + `modprobe fs-briefs` when done).
`run-suite.sh` now guards its `modprobe` so a FUSE run (`MOUNT_CMD` matching
`*fuse*`) never auto-loads the module itself.

The `run-fuse-subset.sh` wrapper runs the replay-sensitive subset:

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

### Harness fixes that make the results trustworthy

- `common/config` (xfstests-dev): the unconditional
  `MOUNT_PROG="$(type -P mount)"` / `UMOUNT_PROG` reset now only runs when the
  variable is unset, so an exported/inherited wrapper survives `./check`'s
  child re-sourcing (`CONFIG_INCLUDED=true` path). This is the child-mount
  bypass fix.
- `run-suite.sh` (kernel repo): `modprobe briefs_fs` skipped for FUSE runs.
- `fuse-briefs-umount` (kernel repo): resolves a **device** argument
  (`/dev/vdc1`) to its mountpoint via `findmnt -S` before unmounting.
  `_check_briefs_filesystem` → `_umount_or_remount_ro` passes `$device`, and
  the wrapper previously (a) could not reliably unmount by device spec and
  (b) computed a pidfile key that never matched the one `fuse-briefs-mount`
  wrote (keyed by mountpoint). Necessary for the post-test unmount to work
  at all, but **not sufficient** to fix 030/029 — the real cause was the
  daemon dying at test exit (next bullet).
- `mount.fuse.briefs` (utils repo): the daemon was launched with plain
  `setsid`, which stays in the caller's cgroup. xfstests wraps each test in
  its own transient systemd scope (`fstests-generic-030.scope`) and *stops*
  that scope when the test exits — killing the daemon with it and leaving a
  dead FUSE mount: still listed in `/proc/mounts` (stat: "Transport endpoint
  is not connected") but invisible to `df`, because coreutils `df` omits
  mounts it cannot stat. `common/rc`'s `_fs_type` is df-based, so
  `_check_briefs_filesystem` no longer recognised the scratch as a mounted
  briefs fs, skipped its `_umount_or_remount_ro`, and ran fsck on the
  still-listed mount — which refused ("is mounted"). This is what actually
  failed 030 (which deliberately leaves the scratch mounted at exit) in
  every 2026-09-06/07 run. Fixed 2026-09-07: the daemon is now launched as
  a transient systemd service in `system.slice` (`systemd-run --unit=…
  --collect`), which the test scope's teardown does not touch; falls back to
  `setsid` when systemd-run is unavailable or fails. Verified end-to-end
  (mount inside a stopped scope survives; umount wrapper reaps it) and
  generic/030 now **passes** (clean `./check` run, 2026-09-07).

### How the wrappers work

- `fuse-briefs-mount [-t briefs] [-o opts] <dev> <mnt>`: the `MOUNT_PROG`
  wrapper. xfstests prepends `-t briefs` (from `FSTYP=briefs`); the wrapper
  strips it and substitutes `-t fuse.briefs` so `mount(8)` dispatches to
  `/usr/sbin/mount.fuse.briefs`, which backgrounds the `fuse.briefs` daemon,
  records its PID, and waits for the mountpoint to come up. `-o` opts are
  forwarded to the daemon via `--mount-opts` (briefs-utils `9bd009c`):
  nosuid/nodev/noexec drop the matching permissive default, and anything
  unrecognized passes through for the kernel mount to validate.
- `mount.fuse.briefs <src> <target>`: the `mount(8)` type helper. Launches
  `fuse.briefs -i <src> -m <target>` as a transient systemd service
  (`systemd-run --unit=… --collect` in `system.slice`, so the daemon survives
  the mounting process's cgroup teardown — e.g. xfstests stopping the test
  scope; falls back to `setsid` without systemd-run), writes the daemon PID
  to `/tmp/fuse-briefs-<target>.pid`, polls `mountpoint -q` until visible.
- `fuse-briefs-umount <mnt-or-device>`: the `UMOUNT_PROG` wrapper. Resolves
  device→mountpoint (see above), runs `umount`/`fusermount -u`, waits for the
  recorded `fuse.briefs` PID to exit (journal checkpoint completes) so the
  next test's `mkfs.briefs` doesn't race the checkpoint.
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

## Results (2026-09-06 subset run — historical, the first honest record)

The 2026-09-06 run was the first trustworthy per-test record (10-test
replay-sensitive subset). Every real bridge bug it surfaced has since been
fixed — 011 and 013 (readdir under concurrent modification, the fixed
256-byte trie-iterator name stack), 029 (mmap-writeback tail loss), and
322 (sparse-write i_size loss) all PASS in the 2026-09-12 baseline full
run and every run since; only 003 (atime) remains open. Keep the detail
below for the root-cause history.

Pre-fix sanity run (harness fix not yet applied, module removed → bypass made
loud): 0 PASS / 8 FAIL / 2 NOT RUN — every scratch test died with
`mount: unknown filesystem type 'briefs'`, proving the Aug-6 "passes" were
kernel mounts. Archive: `tests/xfstests/runs/run-20260906-180622-fuse.txt`.

Post-fix run (archive `tests/xfstests/runs/run-20260906-184643-fuse.txt` in
the kernel repo, recorded at commit `ee47e72`):

| Test | Description | Result | Detail |
|------|-------------|--------|--------|
| `generic/003` | atime updates | ❌ FAIL | **Real bridge gap**: atime never updated after access (all four checks). The bridge's getattr serves its own stored atime; it never advances atime on read ops. |
| `generic/029` | mmap write vs truncate down/up | ❌ FAIL | **Real bridge bug**: all 6 hexdumps (pre *and* post remount, all 3 cases) end at `0x1000` instead of `0x1400` — data written past the 4096-byte boundary after truncate-up is lost at write time. mmap-writeback size/extension handling. Plus the 030-style fsck-on-mounted artifact (see below). |
| `generic/030` | mmap truncate | ✅ PASS (2026-09-07 revalidation) | In the 2026-09-06 run the `.out.bad` was **empty** — test content matched golden exactly — and the only failure was the post-test `_check_scratch_fs` fsck refusing to run on the still-mounted scratch. Root cause (found 2026-09-07): the test scope teardown killed the FUSE daemon, leaving a dead mount that `df` cannot see, so the pre-fsck unmount gate never fired (see harness fixes). Fixed in `mount.fuse.briefs`; revalidated with a clean `./check generic/030` run — **Passed all 1 tests**. |
| `generic/032` | fiemap | ⏭️ NOT RUN | `xfs_io fiemap failed` — bridge does not implement fiemap (clean feature-skip) |
| `generic/321` | fsync under dm-flakey | ✅ PASS | first genuine FUSE pass for a scratch-mounting replay test |
| `generic/322` | fsync rename under dm-flakey | ❌ FAIL | **Real bridge bug**: scenario 2's `pwrite 2M 1M` (offset 2 MiB past EOF 1 MiB) reports success (`wrote 1048576/1048576 bytes at offset 2097152`) yet the file reads back as 1 MiB — sparse writes past EOF do not advance i_size / do not persist. md5 = the 1 MiB-file hash even *pre-drop*, so replay is not implicated. |
| `generic/475` | dm-error fsstress crash-replay | 🟠 HANG (timeout) | test body completed ("Silence is golden" — the crash-replay scenario itself succeeded) but 4 fsstress workers sat in D-state for 5+ hours with the `error-test.475` dm device up; `./check` never reaped them and `timeout 900` could not SIGTERM it. They eventually returned on their own (after a `dmsetup remove -f` attempt window) and the run recorded HANG. Same VM-reboot-only class as the kernel 127/521 flush wedges; needs root-cause (daemon-side blocked I/O on the dm-error device should get EIO, not an infinite wait). |
| `generic/547` | fsstress + fsync + flakey crash-replay | ✅ PASS | **first genuine FUSE 547 pass** — the Aug-6 "metadata mismatch" record was a *kernel* dm-flakey flake observed through the harness bypass and is retracted (see Known issues) |
| `generic/640` | rename trie-root journal ordering | ✅ PASS | |
| `generic/011` | dirstress | ❌ FAIL | **Real bridge bug, confirmed**: `rm: … Directory not empty` under concurrent dir ops — identical to the Aug-6 signature (the one genuine FUSE result of that run), now reproduced on the honest harness. readdir does not enumerate all entries under concurrent modification. |

**3 PASS (321 547 640), 5 FAIL (003 011 029 030 322), 1 HANG (475), 1 NOT
RUN (032)** as run on 2026-09-06. 030's failure was a harness artifact and
is now fixed and revalidated (2026-09-07 clean `./check` PASS), so the real
bridge bugs surfaced by the honest run are: **003 (atime), 011 (readdir under
concurrent modification), 029 (mmap/truncate tail loss), 322 (sparse-write
i_size loss)**, plus the **475 wedge**. 029 also carried the 030-style
post-test fsck artifact; its content-level hexdump failure is independent of
that and stands.

`generic/321`, `547`, and `640` passing are the meaningful new signals: the
journal write path + replay-on-mount port survives fsync/flakey crash-replay
and the rename journal-ordering scenario genuinely under FUSE.

### Verification that this run really used FUSE

- No `mount: unknown filesystem type 'briefs'` (kernel-mount attempt) in any
  `.out.bad`/`.full` from the post-fix run — all scratch mounts succeeded,
  i.e. via the wrappers.
- The 475 fuse daemon runs `-i /dev/mapper/error-test.475` (dm scratch) and
  547/321/322 likewise exercised flakey/dm paths through `fuse.briefs`.
- The per-test failure signatures above (atime, mmap tail, sparse i_size) are
  *bridge-shaped* bugs the kernel module does not have — independent
  confirmation the bridge was under test.

## Results history

| Date | Record | Validity |
|------|--------|-----------|
| 2026-08-06 (1st) | "10/10 PASS" | invalid — no FUSE engagement at all (MOUNT_PROG clobbered by config reset) |
| 2026-08-06 (2nd) | "7/10 PASS (547, 011 FAIL)" | invalid for all scratch tests — child-mount bypass; only 011 was genuine FUSE |
| 2026-09-06 (pre-fix) | 0/8/2 | valid but uninformative — proves the bypass (loud kernel-mount failures) |
| 2026-09-06 (post-fix) | see table above | **first trustworthy per-test FUSE record** |
| 2026-09-07 | generic/030 re-run: PASS | valid — first clean 030 pass; scope-kill harness fix (`mount.fuse.briefs`) validated |
| 2026-09-07 (full suite) | 188/110/63 (run-20260907-225019) | first honest full-suite run — surfaced the 63-HANG class (per-op journal checkpoint throughput blowout, not deadlocks) and the four subset bugs at full-suite scale |
| 2026-09-11 (full suite) | 277/72/11 | after fixes A/B/C + the rebuild-amplification and journal run-encoding campaigns |
| 2026-09-12 (full suite) | 289/67/4, then 300/59/1 | re-run #2 after the OOM/replay-anchor fixes; run #3 is the Family-1 baseline (all closures held; 476 raised to a 900 s budget and solo PASS) |
| 2026-09-13 (full suite) | 327/33/0 (run-20260913-201548) | Family 1 closed — 27 promotions, 476 HANG→PASS, one regression generic/128 (root cause R8, fixed after the run) |
| 2026-09-14 (full suite) | **328/32/0 (run-20260914-021234)** | closing record — 128 fixed by the `-o` plumbing, zero baseline regressions |
| 2026-09-15 (full suite) | 328/32/0 (run-20260915-212640) | re-validation of the 18-commit re-review refactor series (symlink parity, dead code, dedups, O1-O8 optimizations) at a24c72d — per-test status byte-identical to the closing run across all 793 tests; zero behavioral change |

## Kernel interop

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

`generic/321` and `generic/547` (fsync/dm-flakey crash-replay) now pass
genuinely under FUSE (2026-09-06), exercising this path; `generic/475`'s test
body also completed its crash-replay scenario before the harness wedge.

## Known issues

### generic/003 — atime never updated (real bridge gap — OPEN, the only remaining confirmed bridge bug)

All four atime checks fail. The bridge never advances atime on read
operations; since FUSE getattr is served by the daemon, its stored atime
shadows whatever the kernel VFS might have cached. Fix: update atime (with
relatime-style throttling to taste) in the bridge's read path.

### generic/029 — mmap write after truncate-down/up loses the past-page tail (FIXED)

Both this and the 322 size bug below were closed by the 2026-09-09..09-12
fix series (tail/boundary zeroing via the pre-commit drain, read-hole full
read count, fresh-slot write-through); 029 and 322 both PASS in the
2026-09-12 baseline full run and every run since. Original triage kept
for the root-cause history:

All three cases, both pre- and post-remount: expected file size 5120/5121
bytes, actual 4096 — the mwrite region `[4096, 5120)` is lost while
`[2048, 4096)` survives. Already wrong before the remount, so this is at
write time, not checkpoint/replay. Points at the mmap-writeback path
clamping/mis-extending the size when writes land past EOF in the block/page
that follows a truncate-up. Needs debug in `fuse/file_ops.go` (write-back
handling of partial final blocks) — plausibly related to the 322 size bug
below.

### generic/322 — sparse write past EOF does not advance i_size (FIXED — see the 029 note above)

`pwrite 2M 1M` on a 1 MiB file reports full success, but the file still reads
back as 1 MiB afterward (pre-drop md5 equals the 1 MiB-file hash) — the data
extent and/or the size update for a write starting past EOF is dropped.
Replay is not implicated (wrong before the flakey drop). Needs debug in the
bridge's write path (hole creation + size extension for offset > i_size).

### generic/475 — dm-error soak wedge (on the SKIP list)

Skipped in every full run since (SKIP_TESTS includes 475; generic/492 is
the other skip). Original triage kept for the record:

The test's crash-replay content completed, but 4 fsstress workers entered
uninterruptible D-state against the dm-error scratch mount and stayed there
for over 5 hours; the test shell could not reap them, `timeout 900` could not
terminate `./check`, and the run stalled (they eventually returned on their
own and the run recorded 475 as HANG; `sudo dmsetup remove -f
error-test.475` is the intended manual unwedge). Root cause open: why
daemon-side I/O on an error-table dm device blocked indefinitely instead of
returning EIO. Same "VM-reboot-only" flavor as the kernel-side 127/521
flush wedges, but those were kernel bugs — this one is bridge-side and new.

### generic/013 — RENAME_WHITEOUT shard self-deadlock (real bridge bug, FIXED 2026-09-07)

The 2026-09-07 full-suite run wedged in generic/013: one fsstress worker sat
in D-state in `renameat2(..., RENAME_WHITEOUT)` for 2+ hours, the FUSE queue
showed `waiting=1`, and a Go goroutine dump (SIGQUIT on the daemon) showed
the handler stuck 100 minutes in `sync.Mutex.Lock` inside
`renameWhiteout` → `lockOtherInodeBlock` (`fuse/link_ops.go`) — with **no
other goroutine holding the mutex**.

Root cause: `renameWhiteout` first locks the inode-table-block shards of
{old parent, new parent, moved, target} via `lockInodeShards`, then
allocates the fresh whiteout inode and locks *its* shard via
`lockOtherInodeBlock(oldParentIno, whiteout)`, which dedups only against the
**parent's** shard. The fresh slot can land in a table block sharing a shard
with the *moved* or *target* inode (fsstress allocates everything from the
same free-slot region, so this is common); re-locking a shard the same
goroutine already holds self-deadlocks, since Go mutexes are not reentrant.
The stuck handler wedges every later client of that shard, and the whole
mount freezes until the daemon is killed. Fix: `lockInodeBlockUnlessHeld`
(fuse/fuse.go) dedups against *all* held shards; `renameWhiteout` now uses
it. Also found while unwedging: the run-suite `timeout` used SIGTERM, which
bash defers while waiting on a foreground child, so a wedged test defeats
its own timeout — run-suite.sh now uses `timeout -s KILL` (exit 137 →
`HANG (timeout)`).

**Validated 2026-09-07**: with the fix, generic/013 runs to completion —
all three fsstress phases (including the rename-heavy phase 3) finish
without wedging; 300 s was additionally too short for the bridge's
per-op journal sync + device fdatasync under fsstress (013 now gets a
1200 s run-suite timeout). 013 still FAILs, but on a *different*, known
bug: `rm: cannot remove '…': Directory not empty` during its cleanup — the
generic/011 readdir family.  New signal from 013's failure: every affected
directory sits behind long name components (30–115 bytes each) whose
cumulative depth overflows the trie iterator's fixed 256-byte name stack,
which drops entries silently (see the 2026-09-04 review, E1 secondary) —
concrete root-cause lead for the 011 bug.

**Fully closed since**: the E1 fix (`TrieSiblingMax` cap + the
dynamic-stack `TrieIterator`, briefs-utils `09c7fbc`) made the iterator
safe for deep/long name chains; 011 and 013 both PASS from the 2026-09-12
baseline full run onward.

### Retracted: generic/547 "metadata mismatch" (was a kernel flake)

The Aug-6 547 failure narrative (replay non-idempotence via the deferred
trie-block reuse pool) was observed through the harness bypass and was a
*kernel* dm-flakey flake — 547 passes genuinely under FUSE (2026-09-06). The
replay-trie-block pool port (kernel `briefs_trie_seed_pool`) remains a
worthwhile parity item from the 2026-09-04 review, but it is no longer
implicated in any observed failure.

### generic/032 — NOT RUN (fiemap unsupported)

`generic/032` cleanly `_notrun`s — the bridge does not implement the
`FS_IOC_FIEMAP` ioctl. Not a failure; a feature gap. Implementing fiemap in
the bridge would let 032 run.

### generic/011 — dirstress "Directory not empty" (FIXED — the E1 dynamic-stack TrieIterator)

First seen in the Aug-6 run (the one genuine FUSE result of that run, via
`$TEST_DIR`), and reproduced identically by the 2026-09-06 honest run:
`rm: … Directory not empty` during dirstress cleanup — readdir does not
enumerate all entries under concurrent modification (rm does not see the
entries to remove, but rmdir sees the directory as non-empty). Root cause
confirmed via 013's failure shape (long name components overflowing the
trie iterator's fixed 256-byte name stack, silently dropping entries);
fixed by briefs-utils `09c7fbc` (E1: `TrieSiblingMax` cap + dynamic-stack
`TrieIterator`). 011 and 013 both PASS from the 2026-09-12 baseline full
run onward.

## FUSE bridge coverage

The FUSE bridge implements all BrieFS operations at full kernel parity
(feature list updated 2026-09-14 for the bu-refactor-1 branch):

- **Directory ops**: create, mkdir, unlink, rmdir (with journal ordering +
  trie root pinning).
- **File data writes**: inline data (≤256B), extent-backed (inline array ≤8,
  B+ tree spill), hole allocation, unwritten extent conversion.
- **Extended attributes**: user, trusted, security namespaces; set/get/list/
  remove; continuation blocks for large values. POSIX ACLs are enforced
  (mount enables go-fuse ACL negotiation; ACLs live in the
  `system.posix_acl_*` xattrs exactly as in the kernel).
- **Fileattr / chattr**: FS_IOC_GETFLAGS/SETFLAGS, FS_IOC_FSGETXATTR/
  FSSETXATTR; immutable/append enforcement.
- **Link / symlink / mknod / rename**: hardlink, inline + extent symlinks,
  block/char/fifo/socket special files, renameat2 (NOREPLACE, EXCHANGE,
  WHITEOUT).
- **Fallocate / setattr / killpriv**: all five fallocate modes (KEEP_SIZE
  preallocate with unwritten extents, PUNCH_HOLE, ZERO_RANGE, COLLAPSE_RANGE,
  INSERT_RANGE), truncate up/down, chmod/chown/utimes, suid/sgid stripping +
  security.capability clearing on write/chown. Like the kernel's
  `meta_shield`, B+ tree metadata is reserved for unwritten extents, so
  converting a preallocated block cannot ENOSPC on a full filesystem.
- **Ioctls**: FITRIM and FS_IOC_{GET,SET}FSLABEL (see `fuse/ioctl_mount.go`).
- **Journal port**: Go port of the kernel journal write path (`briefs/
  journal_write.go`), with drain-before-snapshot durability for btree nodes
  and commit-before-flush for re-derivable metadata (trie pages, inline data,
  xattr blocks).
- **Journal replay on mount**: Go port of the kernel's `briefs_journal_replay`
  (`fuse/journal_replay.go`), running the 3-pass replay (reserve bitmap bits,
  re-derive tries + restore inode/symlink/xattr blocks, reconcile nlinks) so a
  crashed/dirty volume recovers consistently on remount. Replay applies the
  kernel's generation guards (kernel commit 33e4019) for inode-full/update
  records.
- **Per-inode-block locking**: sharded per-inode-table-block mutexes for
  concurrent file writes on disjoint blocks.
- **Permissions / setid / privilege parity (Family 1, 2026-09-13/14)**:
  `allow_other` with `default_permissions` (the kernel runs all DAC/sticky/
  setattr_prepare/protected_* checks); caller caps/umask/groups read from
  `/proc/<caller>/status`; kernel-parity killpriv gating
  (CAP_FSETID/S_ISREG/S_IXGRP/in-group) with unconditional
  security.capability removal; setgid-directory gid inheritance; create
  modes computed from the caller's umask or the parent's default ACL
  (`posix_acl_create_masq`); FUSE_DONT_MASK negotiated via
  `ExtraCapabilities`; ACL→mode recompute on setfacl and chmod; and
  suid/dev/exec mount defaults with `-o` override support.
- **Known gaps (as of 2026-09-14)**: atime maintenance on read (003 — the
  one remaining confirmed bridge bug from the xfstests record), fiemap
  (`FS_IOC_FIEMAP` — gates `generic/032` to NOT RUN), and the file-range
  exchange ioctls (XFS_IOC_EXCHANGE_RANGE/SWAP_RANGE, COMMIT_RANGE —
  deferred, rationale in `fuse/ioctl_mount.go`). The 2026-09-06 subset run
  additionally surfaced mmap-writeback size extension past a truncate-up
  (029) and sparse-write i_size extension (322); both were fixed by the
  2026-09-09..09-12 series and pass since.
  O_TMPFILE is not bridge-addressable: the 6.12 FUSE client has no O_TMPFILE
  support.  `sync(2)` on the mount is a silent no-op: the 6.12 FUSE client
  only wires `fc->sync_fs` for fuseblk mounts (`ctx->is_bdev`,
  fs/fuse/inode.c:1742) so FUSE_SYNCFS is never sent, and go-fuse has no
  dispatch for it regardless — accepted as a documented limitation
  (generic/520's sync-only cases fail; every fsync case passes).  Fix shape
  if ever needed: fuseblk mount plus a patched go-fuse FUSE_SYNCFS dispatch.

## Repository layout

| Repo | Branch | Role |
|------|--------|------|
| `~/src/briefs` (kernel) | `bu-refactor-1` | Kernel module + xfstests wrappers (`tests/xfstests/fuse-briefs-*`, `run-suite.sh`, `run-fuse-subset.sh`), campaign record + run archives (`tests/xfstests/xfstests-fuse-status.md`, `runs/`) |
| `~/go/src/github.com/ctdk/briefs-utils` | `bu-refactor-1` | Go FUSE bridge (`cmd/fuse`), mkfs (`cmd/mkfs`), fsck (`cmd/fsck`), shared format (`briefs/`), `mount.fuse.briefs` helper (current dev branch; the read-write bridge work is in `master`) |
| `~/src/xfstests-dev` | — | xfstests source + configs (`configs/briefs-fuse.config`), `common/rc` + `common/config` FUSE harness fixes |