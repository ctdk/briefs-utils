BrieFS Utils
===========

![The briefs-utils logo - a pelican wearing briefs (aka The Pelican's Briefs), but holding a hammer.](images/briefs-utils-logo-1.png)

The filesystem utils for BrieFS (`mkfs.briefs`, `fsck.briefs`, `fuse.briefs`).

mkfs.briefs
-----------

Creates new BrieFS volumes. It formats new filesystems with the packed directory trie layout used by BrieFS 0.7.0+.

```
NAME:
   mkfs.briefs - Create a new BrieFS filesystem

USAGE:
   mkfs.briefs [global options] command [command options]

VERSION:
   v0.7.0

COMMANDS:
   help, h  Shows a list of commands or help for one command

GLOBAL OPTIONS:
   --output value, -o value        output file path
   --size value, -s value          filesystem size in blocks (default: 0)
   --block-size value, -b value    block size in bytes (default: 4096)
   --inode-size value              inode size in bytes (default: 512)
   --journal-size value, -j value  journal size in blocks (default: 64)
   --label value                   filesystem label (default: "BRIEFS")
   --help, -h                      show help
   --version, -v                   print the version
```

fsck.briefs
-----------

The idea with `fsck.briefs` is that it will repair broken, mangled, and mutilated BrieFS volumes. It doesn't actually do that yet, but that's the dream. It does, however, perform a growing list of consistency checks, including CRC32C verification of journal records and extent chain blocks, and validates the packed directory trie pages used by BrieFS 0.7.0+.

```
NAME:
   fsck.briefs - Check and repair a BrieFS filesystem

USAGE:
   fsck.briefs [global options] command [command options]

VERSION:
   v0.7.0

COMMANDS:
   help, h  Shows a list of commands or help for one command

GLOBAL OPTIONS:
   --device value, -d value  filesystem device or image file
   --verbose, -V             verbose output (default: false)
   --repair                  attempt to repair found errors (not yet implemented) (default: false)
   --help, -h                show help
   --version, -v             print the version
```

fuse.briefs
-----------

A FUSE bridge for BrieFS so you can mount BrieFS volumes without the commitment of loading and/or battling with a kernel module. Currently read-only and basic. Supports the packed directory trie format introduced in BrieFS 0.7.0.

```
NAME:
   briefs-fuse - Mount a BrieFS filesystem image via FUSE

USAGE:
   briefs-fuse [global options] command [command options]

VERSION:
   v0.7.0

COMMANDS:
   help, h  Shows a list of commands or help for one command

GLOBAL OPTIONS:
   --image value, -i value       filesystem image file or block device
   --mountpoint value, -m value  mount point directory
   --debug, -d                   enable FUSE debug output (default: false)
   --help, -h                    show help
```

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
