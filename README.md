BrieFS Utils
===========

![The briefs-utils logo - a pelican wearing briefs (aka The Pelican's Briefs), but holding a hammer.](images/briefs-utils-logo-1.png)

The filesystem utils for BrieFS (`mkfs.briefs`, `fsck.briefs`, `fuse.briefs`).

mkfs.briefs
-----------

Creates new BrieFS volumes. It formats new filesystems with the on-disk v0.9.0
format: the packed directory trie layout plus a B+ tree extent index for files
with more than eight extents. Inodes include a `user_flags` field that stores
chattr/lsattr-visible flags (sync, dirsync, immutable, append-only, nodump,
noatime).

```
NAME:
   mkfs.briefs - Create a new BrieFS filesystem

USAGE:
   mkfs.briefs [global options] DEVICE

VERSION:
   v0.9.6

GLOBAL OPTIONS:
   --size int, -s int          filesystem size in blocks (default: 0)
   --block-size int, -b int    block size in bytes (default: 4096)
   --inode-size int, -I int    inode size in bytes (default: 512)
   --journal-size int, -j int  journal size in blocks (0 = auto: scale with volume size) (default: 0)
   --label string, -L string   filesystem label (default: "BRIEFS")
   --inode-ratio int           blocks per inode (one inode per N blocks) (default: 8)
   --uuid string, -U string    Specify a UUID for the new volume.
   --force, -f                 force overwrite of an existing filesystem
   --help, -h                  show help
   --version, -v               print the version
```

fsck.briefs
-----------

The idea with `fsck.briefs` is that it will repair broken, mangled, and mutilated BrieFS volumes. It performs a growing list of consistency checks, including CRC32C verification of journal records and B+ tree extent index nodes, structural validation of those B+ trees (high-key monotonicity, child-pointer range/level, leaf prev/next linkage, cross-leaf key ordering, extent-count agreement), validation of the packed directory trie pages used by BrieFS 0.7.0+, validation of inode extended-attribute chains (magic, header version, used_size, CRC32C, entry bounds, continuation blocks, and chain length/loop detection), and preservation of the inode `user_flags` field used for chattr/lsattr flags.

Repairs are organized into phases, selectable with `--repair-only` (comma-separated):

* `allocator` — rebuild the block/inode allocator bitmaps from the blocks actually in use.
* `btrees` — recompute and rewrite B+ tree extent-index node checksums (CRC-only; no structural change).
* `btree-rebuild` — fully rebuild a damaged B+ tree extent index from its surviving extents, dropping to inline extents when ≤8 remain.
* `btree-orphan` — free orphan B+ tree node blocks left behind by a torn split. Destructive, so it is **opt-in only and not included in `all`**.
* `extents` — compact and rebalance file extent indexes, merging underfull B+ tree leaves (a no-op on an already-minimal tree).
* `trie` — compact directory trie pages.
* `links` — repair inode link counts.

`--repair` with no `--repair-only` runs `all`, which is `allocator,btrees,btree-rebuild,extents,trie,links` (everything except `btree-orphan`). `--optimize` is an alias for `--repair --repair-only=trie,extents` — safe compaction of directory tries and extent indexes with no allocator rewrite and no B-tree rebuild.

As a safety guard, `fsck.briefs` refuses a default `--repair` when any inode has an unrecoverable B+ tree extent index, since rebuilding the allocator would free that inode's blocks and lose data; the non-allocator phases (`--optimize`, or `--repair-only` without the `allocator` token) are not destructive and may still proceed.

```
NAME:
   fsck.briefs - Check and repair a BrieFS filesystem

USAGE:
   fsck.briefs [global options] DEVICE

VERSION:
   v0.9.6

GLOBAL OPTIONS:
   --verbose, -V             verbose output
   --repair, -r              attempt to repair found errors
   --repair-only string      run only selected repair phases (comma-separated: allocator,btrees,btree-rebuild,btree-orphan,extents,trie,links; default all)
   --optimize                safe compaction only (alias for --repair --repair-only=trie,extents)
   --assume-yes, -y          do not ask for confirmation before modifying the volume
   --no, -n                  read-only check; do not attempt repairs (default mode, made explicit for fsck(8) -n)
   --preen, -p               non-interactive repair; alias for --repair --assume-yes
   --type string, -t string  filesystem type (accepted for fsck(8) compatibility; ignored)
   --help, -h                show help
   --version, -v             print the version
```

fuse.briefs
-----------

A FUSE bridge for BrieFS so you can mount BrieFS volumes without the commitment of loading and/or battling with a kernel module. The FUSE bridge is **read-write** (full kernel parity) but **experimental**: it implements all directory and file operations (create, mkdir, unlink, rmdir, link, symlink, mknod, rename with renameat2 EXCHANGE/WHITEOUT), extended attributes (user/trusted/security), fileattr/chattr (FS_IOC_GETFLAGS/SETFLAGS, FS_IOC_FSGETXATTR/FSSETXATTR), fallocate (KEEP_SIZE/PUNCH_HOLE), setattr (chmod/chown/utimes/truncate), and killpriv (suid/sgid + security.capability stripping). It also ports the kernel journal write path to Go and replays the journal on mount, making FUSE-written volumes crash-consistent and recoverable (a crashed/dirty volume remounts with the same consistency the kernel module provides) and kernel-mountable. However, it has not yet been tested across the full xfstests suite — see the `xfstests-fuse-status.md` document for the current pass/fail record and known issues.

### Mounting with `mount -t fuse.briefs`

`mount -t fuse.briefs <dev> <mnt>` works once the `mount.fuse.briefs` helper
is installed in a directory `mount(8)` searches (`/sbin` or `/usr/sbin` —
**not** `/usr/local/sbin`). `make install` places it in `/usr/sbin` by default
(override `SBINDIR=` otherwise). The helper is a thin wrapper that
backgrounds the `fuse.briefs` daemon and waits for the mount to come up:

```
$ sudo make install
$ sudo mount -t fuse.briefs /dev/vdb1 /mnt/briefs
$ ls -a /mnt/briefs
.  ..  ...
$ sudo umount /mnt/briefs
```

No kernel module and no `umount` helper are needed: `umount <mnt>` goes
through the kernel FUSE unmount path, which delivers DESTROY to the daemon so
it checkpoints the journal and exits cleanly. (`mount -t fuse.briefs` is a
pure userspace mount; the BrieFS kernel module need not be loaded.)

```
NAME:
   briefs-fuse - Mount a BrieFS filesystem image via FUSE

USAGE:
   briefs-fuse [global options]

VERSION:
   v0.9.6

GLOBAL OPTIONS:
   --image string, -i string       filesystem image file or block device
   --mountpoint string, -m string  mount point directory
   --debug, -d                     enable FUSE debug output
   --help, -h                      show help
   --version, -v                   print the version
```

On-disk format codegen
---------------------

Every persisted struct lives in the `briefs/` package with a gendisk marker
(`//go:briefs-disk size=N`, or `//go:briefs-disk packed size=N` for structs
whose C layout is packed/unaligned). The `cmd/gendisk` tool — run via
`//go:generate` and the Makefile `generate` target, which is a build
prerequisite for `mkfs`/`fsck`/`fuse` — emits little-endian
`MarshalBinary`/`UnmarshalBinary`/`Size` methods plus a compile-time size
assertion that pins each struct to its kernel `BUILD_BUG_ON` size. Any layout
drift becomes a build or generation failure, so the on-disk layer is guarded
automatically and no package keeps a hand-rolled copy of a persisted layout.

RELATED
-------

* [BrieFS](https://github.com/ctdk/briefs): The actual BrieFS kernel module.

LICENSE
-------

The briefs-utils are licensed under the terms of the MIT license. See the LICENSE file for details.

AUTHOR
------

Jeremy Bingham <jbingham@gmail.com>

COPYRIGHT
---------

Copyright (c) 2026, Jeremy Bingham
