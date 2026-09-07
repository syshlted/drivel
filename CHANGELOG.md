# Changelog

What has changed in Drivel, in plain terms. The long-form reasoning behind each
decision lives in the repo's design notes; this file is the summary.

There are **no tagged releases yet**. Everything below is on the main branch;
install with `go install github.com/zishmusic/drivel/cmd/drivel@latest` or build
from source.

## Unreleased

### Cheaper first sync for folder mounts — 2026-09-07

If you mount a *folder* rather than your whole Drive, the initial scan now
descends that folder instead of listing your entire account. The cost is
proportional to what you mounted, not to how much is in the Drive.

Previously every mount listed the whole account, page by page — minutes on a
large Drive, repeated on every first run, every `-resync`, every recovery from an
expired cursor and every scheduled re-scan.

`-drive-sweep-mode` picks the strategy: `auto` (default) descends unless you
mounted the whole Drive, `scoped` always descends, `flat` always lists the
account. Neither is always cheaper — a subtree with very many directories is
cheaper `flat`, and Drivel says so in the log when it sees that shape.

Under Drive throttling the descent keeps the listings that succeeded, retries only
the ones that failed, and reduces its own concurrency rather than hammering.

### Extended attributes on macOS and FreeBSD — 2026-09-06

The marker that lazy mode uses to tell a not-yet-downloaded file from an empty one
is now implemented natively on all three platforms. **FreeBSD is now a tested
platform** — the full suite ran green on FreeBSD 15.1 with nothing skipped. macOS
still builds and has never been run; treat it as unverified.

That first FreeBSD run also found a real bug affecting every platform: writes
through a write-only file handle could fail with `EBADF` when the kernel read back
part of a block before modifying it. Fixed.

### Extended attributes are no longer passed through by default — 2026-09-05

A Drivel mount now answers extended-attribute operations the way a filesystem
without attribute support does, unless you pass `-xattr`.

This is a safety change. Drivel's placeholder marker lives in an extended
attribute on the backing file; passing attributes through the mountpoint published
it, letting anything with write access to the mount strip it (making Drivel upload
zeros over your real file) or forge it (making the next read overwrite your local
content). Nothing inside Drivel needs the passthrough.

Also: `-pprof ADDR` for profiling a running mount, and credentials and tokens are
now always written `0600`.

### Several accounts in one process — 2026-09-04

One `drivel` process can serve any number of mounts, each with its own account,
credentials, sync state and backing directory, described in a TOML config file at
`~/.config/drivel/config.toml`.

```sh
drivel login -account personal
drivel login -account work
drivel mount                      # serves everything in the config
```

`login` appends the account block for you — the file is only ever appended to, so
your comments and layout survive. Mounts are checked against each other **before
any of them opens**, because several mounts can break each other in ways one
cannot: sharing a sync-state database, or nesting one mount's backing directory
inside another's mountpoint, are now startup errors naming both mounts rather than
a hang or a file synced to the wrong account.

Also in this batch: an inbound rename no longer duplicates the file on every other
client, and deleting a large directory tree is dramatically faster.

### Seeing a Drive that was already there — 2026-09-03

The change feed only reports what changes *after* Drivel first runs, so a Drive
full of existing files used to be invisible. Now the first run — and `-resync`,
and recovery from an expired cursor — scans the whole remote tree once and
reconciles it against your backing directory. It repeats on a schedule
(`-sweep-interval`, default 24h) to catch anything that happened while Drivel was
not running.

**Deletion is guarded.** Drivel infers a deletion only from a record that it
previously synced that exact path — never from a file simply being missing on one
side. So **the first run never deletes anything**; a locally modified file is kept
and pushed back rather than deleted; and `-max-deletes` (default 100) abandons the
entire delete pass if the count looks like a broken setup rather than a real
cleanup.

With `-lazy` the whole Drive becomes visible immediately as placeholders. Without
it, materialising means downloading, so that stays behind `-materialize`.

This also fixed a silent failure: Google expires change cursors, and Drivel used
to retry a dead one forever with inbound sync quietly stopped.

### Restart-safe path resolution — 2026-09-03

Drive addresses files by opaque ID, not by path, and Drivel's path↔ID map is now
persisted so a restart starts warm. It remains a **cache, never an authority**:
every stored mapping is re-checked against Drive before use, because the object
may have been moved or replaced while Drivel was down. Deleting the index costs
API round trips and nothing else.

This closed a real bug that predated the index: a path Drivel had not yet learned
counted as "not on Drive", so a restart followed by an edit uploaded a **duplicate**
beside the real file.

### Lazy mode and smarter uploads — 2026-09-02

**Lazy hydration (`-lazy`, opt-in).** Remote files appear immediately with their
real name, size and modification time but occupy no space; content is fetched the
first time something reads the file. A directory listing costs nothing, so a Drive
larger than the disk is usable. Requires a backing filesystem that supports
extended attributes.

**Smarter uploads (always on).** Before re-uploading a changed file, Drivel checks
whether the bytes actually differ from what Drive already holds — covering an
editor rewriting an identical buffer, or a rebuild producing the same artifact.
Where a provider can write byte ranges, only the changed extents go out; Drive
cannot, so large files there get chunked, resumable uploads with retries instead.
Small files skip both checks, since the round-trip costs about what the upload
would.

### Bidirectional sync — 2026-07-22

Full two-way sync. Uploads are debounced per path and dispatched through a worker
pool with retries and backoff; concurrent edits on two machines produce a
**conflict copy** rather than silent data loss; and Ctrl-C unmounts and drains
in-flight uploads before exiting.

### Inbound sync — 2026-07-21

Changes made in Drive, or on another machine, now appear in the mounted folder.
Drivel follows Drive's change-feed cursor and suppresses the echo of its own
uploads so a change does not ricochet back and overwrite itself.

Licensed AGPLv3 from this point.

### Google Drive, HTTP/3, and the login wizard — 2026-07-20

Uploads on close, an rclone-style interactive OAuth wizard (`drivel login`) using
your own Google credentials, and all Drive traffic over HTTP/3 (QUIC) with
automatic HTTP/2 fallback.

**In-place mode (Linux):** mount a directory onto itself, so the files just stay
in it when Drivel exits and nothing is ever copied.

Renamed from `dedupfs` to `drivel`.

### First mount — 2026-07-17

A FUSE filesystem that proxies every operation to a backing directory and emits a
change event per mutation.

## Planned

None of these is scheduled, and none is started.

- **Third-party providers** — a plugin architecture, out-of-process or WASM, so a
  backend need not be compiled into Drivel.
- **macOS** — the code is written and cross-compiles; what it needs is a live run
  on real hardware.
- **A deduplicating local backend** — a content-addressed local store which, with
  `-lazy`, makes Drivel a deduplicating filesystem.
- **Client-side encryption** — a layer that stacks over any other backend, so
  Drive holds ciphertext.
- **A block-level filesystem over a distributed database.**
- **A control socket** — so other tools can read per-path sync status, pause and
  resume a mount, and stop the daemon; with `drivel status|pause|resume|stop` as
  the first client.
- **POSIX metadata and mount safety** — carrying mode and ownership, refusing hard
  links explicitly rather than mishandling them, and logging the special files
  Drive cannot represent instead of skipping them silently.

Deduplication is on this list; **GPU-accelerated hashing is not** — it would
optimise a component that does not exist yet. The `-gpu` in the repository's
directory name is historical.
