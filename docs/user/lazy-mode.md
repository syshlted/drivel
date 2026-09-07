# Lazy mode (`-lazy`)

```sh
drivel mount -mount ~/drive -data ~/.cache/drivel -lazy      # or lazy = true
```

Off by default. Without it, Drivel is *eager*: the backing directory holds real
content for everything it has synced.

## What it does

Remote files appear immediately with their real name, size and modification time
but occupy no space. Content is fetched the first time something reads the file —
or partially writes it. A directory listing costs nothing, and the whole of a
Drive can be visible on a laptop that could not hold it.

The empty stand-in is called a **placeholder**. It is a zero-byte file that
reports its full remote size, marked with an extended attribute on the backing
file. That attribute is the only record that the file is a placeholder.

Three things fall out of the design and are worth knowing:

- **Opening a file fetches nothing.** Content arrives on the first read or partial
  write, not on `open`. Truncating a file, or rewriting it whole, never downloads
  the old content it is about to discard.
- **A failed fetch is an error, never a short read.** You get `EIO`, not zeros.
- **Drivel never uploads a placeholder.** Every content upload is gated on this
  check, and the check fails safe: if it cannot tell, it reports "placeholder" and
  skips.

## The one rule: the backing filesystem must support extended attributes

**On a filesystem that cannot store user extended attributes, `-lazy` is unsafe.
Not degraded — unsafe.** If the marker cannot be written there is no record that a
file is a placeholder at all, and an un-fetched file is indistinguishable from an
empty one. Drivel may then upload it over your real remote copy.

Drivel checks at mount time and warns, naming the directory and the attribute. The
fix is eager mode or a different `-data`. There is no database to preserve
instead — the attribute is the sole record, by design.

Where this bites:

| | |
| --- | --- |
| **WSL2** | `/mnt/c` and other Windows drives are drvfs and carry no Linux extended attributes. Keep `-data` on the Linux filesystem inside WSL. |
| **exFAT, FAT** | No extended attributes anywhere. |
| **FreeBSD tmpfs** | UFS and ZFS carry them; tmpfs does not, so a tmpfs `/tmp` is not a usable `-data`. |
| **macOS non-APFS volumes** | On exFAT, FAT and some network volumes macOS *emulates* attributes in hidden `._name` companion files — which would put the marker in a file inside the synced tree. Drivel detects that and treats it as having none. Keep `-data` on APFS or HFS+. |

## Why `-xattr` is off by default

`-xattr` serves extended attributes *through the mountpoint*, passing them to the
backing store. It is unrelated to whether Drivel itself uses attributes — it
always does, on the backing file, below the mount.

It is off by default because the placeholder marker lives in a `user.*` attribute
on the backing file, and passing attributes through publishes it at the
mountpoint. Anything that can write to the mount could then:

- **strip it from a placeholder** — the file becomes an ordinary empty file, and
  the next upload pushes zeros over your real remote file; or
- **attach it to a resident file** — the file becomes a placeholder, and the next
  read overwrites its content with a download.

No privilege required, no trace left. Nothing inside Drivel needs the passthrough
(the hydrator works on the backing path directly), so `-xattr` exists purely for
applications on the mountpoint that want attributes of their own.

Without it, a Drivel mount answers extended-attribute operations the way a
filesystem without attribute support does: `EOPNOTSUPP`. Either way, **extended
attributes are never synced to Drive.**

## Lazy mode and the first sweep

With `-lazy`, the initial enumeration makes the entire remote tree visible
immediately, as placeholders — that costs one listing pass and no content
transfer. In eager mode the same operation would be a full download of everything,
so it stays behind `-materialize`.

## Before you uninstall

A placeholder is a promise that content can be fetched later. If you remove Drivel
or delete the credentials, that promise cannot be kept and the placeholder is just
an empty file. See [getting your data
out](install.md#getting-your-data-out).
