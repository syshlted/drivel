# Platform support

| Platform | Status | Notes |
| --- | --- | --- |
| **Linux** (all architectures) | **Supported** — built and tested, and what CI exercises | In-place mode available. |
| **FreeBSD** | **Supported** — the full test suite has been run on FreeBSD 15.1 | Needs the `fusefs` module and `vfs.usermount=1`. `-data` required. |
| **macOS** (Intel and Apple Silicon) | **Builds; never run** | Needs macFUSE. `-data` required. Treat as unverified. |
| **Windows** | **Not supported, and not planned** | Use WSL2, or Google Drive for Desktop. |
| Other Unixes (OpenBSD, NetBSD, Solaris, illumos, AIX) | Not supported | No Go FUSE binding exists. Everything except the mount layer already compiles. |

"Builds; never run" means exactly that: `GOOS=darwin` produces a working binary
and nobody has run it against a real Drive. Treat it as unverified rather than as
supported.

FreeBSD moved out of that category on 2026-09-06, when the full suite ran green on
FreeBSD 15.1 with nothing skipped. That run also found a real bug — reads arriving
on write-only file handles — which is worth knowing as a general lesson: a
platform is not verified until it has been run.

## What changes off Linux

**In-place mode is Linux-only.** It routes backing I/O through `/proc/self/fd/N`,
and neither macOS nor FreeBSD has procfs. Pass `-data` there.

**`-lazy` needs a backing filesystem with extended attributes**, on every
platform. Drivel implements the marker natively on Linux, macOS and FreeBSD, but
the *filesystem* has to be able to store it. The full rule, and the list of
filesystems that cannot, is in [lazy mode](lazy-mode.md#the-one-rule-the-backing-filesystem-must-support-extended-attributes).
On FreeBSD in particular: UFS and ZFS carry them, **tmpfs does not**, so a tmpfs
`/tmp` is not a usable `-data`.

**`-lazy` has never been run on macOS.** The attribute code there is largely the
same body Linux runs and is covered by Linux CI, but the platform as a whole is
unverified. Prefer eager mode on a Mac until someone has run it.

## FreeBSD

```sh
kldload fusefs                 # not loaded by default; add fusefs_load="YES" to /boot/loader.conf
sysctl vfs.usermount=1
drivel mount -mount ~/drive -data ~/.drivel-data
```

Keep `-data` on UFS or ZFS if you use `-lazy`.

## macOS

Drivel needs a FUSE implementation, and today that means
**[macFUSE](https://macfuse.github.io/)**. Drivel does not bundle it, ship an
installer for it, or distribute it in any form — installing it is between you and
its authors, and macFUSE 4.x is not open source and restricts commercial use.

**FUSE-T does not work**, and it is not a flag away. Drivel's FUSE library speaks
the kernel protocol directly rather than linking libfuse, which is FUSE-T's
integration point, and it probes for exactly two mount helpers (`mount_macfuse`
and `mount_osxfuse`). Supporting FUSE-T is upstream work in that library.

**Keep the backing directory on APFS or HFS+.** On exFAT, FAT and some network
volumes, macOS emulates extended attributes in hidden `._name` companion files —
which would put Drivel's placeholder marker in a file *inside the directory it
syncs*. Drivel detects the emulation and reports the volume as having no
attributes.

**A case-insensitive backing filesystem is a real hazard**, and macOS defaults to
one. Drive is case-sensitive, so `Foo.txt` and `foo.txt` are two distinct remote
files that collide into one local path. Nothing currently handles this; a live
macOS run is what will establish how it behaves.

### On AGPLv3 and macFUSE

There is no licence conflict, and using Drivel with macFUSE does not affect
Drivel's licence or yours. Drivel does not link macFUSE: its FUSE library is pure
Go and speaks the protocol directly, launching macFUSE's mount helper as a
separate program and talking to it over a file descriptor. Separate programs
communicating at arm's length are not a combined work, so no copyleft obligation
crosses in either direction, and nothing in macFUSE's licence restricts what you
may do with Drivel.

(Not legal advice.)

## Windows

A decided non-goal rather than a gap. WSL2 covers the technical audience and
Google Drive for Desktop covers everyone else; the available FUSE bindings would
forfeit the pure-Go build; and NTFS case-insensitivity opens the same
Drive-is-case-sensitive collision described above. The code seams are kept clean
anyway — everything except the mount layer already cross-compiles for Windows —
but there is no plan to ship it.
