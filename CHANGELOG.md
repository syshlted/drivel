# Changelog

What has changed in Drivel, in plain terms. The long-form reasoning behind each
decision lives in the repo's design notes; this file is the summary.

There are **no tagged releases yet**. Everything below is on the main branch.
Install with `go install github.com/zishmusic/drivel/cmd/drivel@latest`
**plus at least one backend** (`…/cmd/drivel-provider-gdrive@latest`), or build
from source with `make build`.

## Unreleased

### Storage backends are now separate programs — 2026-09-11

**Drivel no longer contains its storage backends; it starts them.** Google Drive
lives in `drivel-provider-gdrive`, SFTP in `drivel-provider-sftp`, and `drivel`
launches whichever a mount needs, talking to it over a private socket.

**This changes how you install it.** Install at least one backend alongside the
command — a `drivel` with none can still mount a directory, but has nothing to
sync with:

```sh
go install github.com/zishmusic/drivel/cmd/drivel@latest
go install github.com/zishmusic/drivel/cmd/drivel-provider-gdrive@latest
```

From a checkout, `make build` builds all of them and `sudo make install` places
them in `/usr/local/lib/drivel/plugins`. Nothing else changes: your config file,
your flags, your credentials and your backing directories are all as they were.

**What you get for it.** A backend that crashes no longer takes your filesystem
with it — the mount stays up, Drivel restarts the backend behind it, and anything
that was waiting to upload is retried. And you carry only the backends you use:
the Drive client, its OAuth stack and its HTTP/3 transport are no longer in the
binary of someone who only syncs to SFTP.

**It also means Drivel can be extended without being forked.** A backend is an
ordinary Go program in its own repository: implement the storage interface, call
`plugin.Serve`, build it as `drivel-provider-<name>`, and a config file can name
it. See [writing a provider](docs/dev/new-provider.md).

Four things worth knowing:

- **Drivel refuses to start a backend that is group- or world-writable**, or one
  sitting in a world-writable directory. A backend runs with your credentials, so
  a file anyone could swap out is a file anyone could swap out *with*. The
  refusal names the file and the problem.
- **Drivel talks to a backend over a private socket on your own machine, and
  nothing else.** A backend that asks to be reached over the network — even on
  this machine's own loopback address — is refused before anything connects to
  it. A loopback port is reachable by every user on the machine; the socket
  Drivel uses is readable only by you.
- **A backend gets a deliberately small environment** — `PATH`, `HOME`, `TMPDIR`,
  locale, TLS roots, proxy settings and `SSH_AUTH_SOCK`, and nothing else.
  Ambient cloud credentials such as `GOOGLE_APPLICATION_CREDENTIALS` are not
  passed on, so which account a mount uses is a fact about your config file
  rather than about the shell you started it from.
- **This is not a sandbox**, and it is not described as one. A backend runs as
  you, with your files. Installing one is trusting it, exactly as much as if it
  had been compiled in.

New troubleshooting for all of this is on the [troubleshooting
page](docs/user/troubleshooting.md#backends).

### Drivel now syncs to SFTP servers — 2026-09-10

**Any machine you have an SSH account on can be a Drivel backend.** There is
nothing to install at the other end — SFTP is a subsystem of OpenSSH — and no
credentials to create: it is your existing SSH key.

```toml
[account.server]
provider = "sftp"
host     = "files.example.com"
user     = "you"
key      = "~/.ssh/id_ed25519"
root     = "/srv/share"

[[mount]]
account        = "server"
path           = "~/share"
data           = "~/.cache/drivel/server"
sweep-interval = "5m"
```

What this gives you over `sshfs` is the thing Drivel is for: a write lands in the
local backing directory and returns immediately, with the upload happening behind
it and retrying if the link is down. With `lazy = true` a large remote tree is
browsable at full size while holding almost none of it, and two machines editing
one file produce a conflict copy instead of a silent loss.

**Editing part of a large file now uploads only that part.** SFTP writes at an
offset, so changing one block of a 4 GB file transfers one block. Google Drive
cannot do this at all — it replaces the whole object — so this is the first
backend where that saving is real.

Three things behave differently from Drive, and all three are documented on the
[SFTP page](docs/user/sftp.md):

- **`sweep-interval` is the poll interval.** SFTP has no change notification of
  any kind, so remote changes are found by walking the tree on a schedule. The
  24-hour default is tuned for a backend that also has a live feed and is wrong
  here — set it to the inbound delay you can live with, e.g. `"5m"`.
- **A deletion has no undo.** There is no trash on an SFTP server and Drivel does
  not invent one, so `-max-deletes` is not the guard in front of a safety net —
  it is the only guard.
- **The server's host key must already be in `known_hosts`.** Drivel never trusts
  a key on first use and there is no option to make it: a mount brought up from
  `/etc/fstab` at boot has nobody to answer a prompt. `ssh-keyscan -H host >>
  ~/.ssh/known_hosts`, after checking the fingerprint.

There is no password or passphrase option, and there will not be one. Use an
agent for an encrypted key.

**Inbound sync no longer requires a provider with a change feed.** Backends
without one previously mounted upload-only, silently; they are now polled by the
enumeration sweep, which already carries the guards that make an inferred
deletion safe.

**Deletions made on a backend with no content checksum now reach you.** Drivel
decides whether to apply a remote deletion by asking whether your local copy is
still the one it last synced, and it could only answer that from a checksum the
provider supplied — which SFTP servers do not. The file was kept and re-uploaded
instead, so a file you deleted on the server came back on the next sweep, every
time. Drivel now also records the local file's size and modification time when it
syncs, and uses those when there is no checksum. A file you really did edit is
still kept and pushed back, unchanged. Google Drive is unaffected.

### Every mount flag now works in /etc/fstab, and completions are generated — 2026-09-09

Four flags described a mount but had no fstab option, so a line using one failed
with `unknown option` and nothing said why: **`drive-delete=`**,
`drive-sweep-mode=`, `upload-workers=` and `hydrate-workers=`. All four work now.
The first is the one that mattered — a mount that comes up at boot is exactly the
one nobody is watching, and `config=` is a poor answer for a line that is
otherwise complete.

```
mydrive /home/you/drive fuse.drivel data=/home/you/.cache/drivel,run-as=you,drive-delete=permanent 0 0
```

A test now checks that every mount-describing flag has an option, so this
particular gap cannot come back. `-pprof` is deliberately still absent: it opens a
debug endpoint for the process, which an unattended boot mount should not be
doing.

**The shell completions are generated from Drivel's own flag definitions**
(`make completions`) rather than maintained by hand beside them. The hand-written
files had drifted — they were missing `-upload-workers`, `-hydrate-workers` and
`-pprof-allow-remote` — and the generator now fails the build if a flag has no
completion entry, or if an entry names a flag that no longer exists. Values that
come from a fixed set are taken from the program's own constants, so
`-drive-delete <TAB>` cannot offer a word the binary would refuse.

`sudo make install` now installs them too, to
`$PREFIX/share/bash-completion/completions/drivel` and
`$PREFIX/share/zsh/site-functions/_drivel`.

### Deleting a file now sends it to the Drive trash — 2026-09-09

When Drivel removes a file from Google Drive it now **moves it to the trash**
instead of deleting it outright. A deletion is recoverable from
[drive.google.com](https://drive.google.com/drive/trash) for 30 days, after which
Drive purges it.

This matters most for the deletions you did not personally ask for. An
enumeration sweep can *infer* one — you deleted a file on another machine while
this one was offline, and the sweep works that out from a baseline — and if the
premise is wrong (a mount pointed at the wrong folder, a backing directory that
was emptied) the inference is wrong with it. Every other guard in Drivel fails
towards keeping your bytes; this was the one operation that did not.

Nothing above the change is different: the path stops existing, the removal
reaches your other machines the same way, and deleting a folder still takes its
contents with it. Two things do change. Trashed files **still count against your
Drive quota** until you empty the trash, and a file you delete and immediately
recreate leaves the old copy in the trash.

If you would rather have the old behaviour — a mount that reclaims space as you
delete — ask for it:

```sh
drivel mount ... -drive-delete permanent
```

```toml
[account.work.provider]  # or [mount.provider]
delete = "permanent"
```

`-max-deletes` is unchanged and still on by default: a sweep that wants to delete
more than 100 files still refuses the whole pass. Filling your trash with a
thousand files you did not mean to delete is better than deleting them, but it is
still not what you wanted.

### Mount from /etc/fstab, and at boot — 2026-09-09

On Linux, Drivel is now a `mount(8)` helper as well as a command. Install it as
`/sbin/mount.fuse.drivel` (`sudo make install` puts it there) and a mount can be an
ordinary fstab line, brought up by `mount /your/mountpoint`, by `mount -a`, or by systemd at
boot with no terminal attached:

```
mydrive  /home/you/drive  fuse.drivel  data=/home/you/.cache/drivel,run-as=you,lazy  0 0
```

Three things are worth knowing. The type is **`fuse.drivel`**, not `drivel` —
that is what the kernel reports for the mount, and systemd compares the two, so an
fstab line saying `drivel` describes a mount that never appears under that name
(`mount.drivel` is installed as an alias anyway). **`run-as=NAME`** drops root to
the account that owns the token and the backing directory, which is what makes a
boot-time mount work at all; it is spelled `run-as` and not `user` because `user`
already means something else in fstab. And the helper's output goes to `logfile=`
or nowhere — never to whoever ran `mount`, because an inherited pipe would hang
until the filesystem was unmounted.

Full guide: [docs/user/fstab.md](docs/user/fstab.md). Unknown options fail the
mount rather than being ignored, unless `mount(8)` passes `-s`.

### Hard links are refused, and the files Drivel skips now say so — 2026-09-08

Mounts are now always `nodev` and `nosuid`, with no option to turn them off. A
device node or a setuid binary arriving from a remote is never something you asked
for. This covers the mountpoint; with `-data`, the backing directory is an
ordinary directory and is unaffected either way. FreeBSD requests only `nosuid`,
because its kernel has no `nodev` flag to request.

**Hard links now fail with `EPERM`** — the error `link(2)` defines for a
filesystem that cannot create them. Before, the link succeeded and the outcome was
worse than the error: both names were ordinary files, so both were uploaded, as
two independent remote objects that diverged on the first write. You were told it
worked. Symlinks are unaffected.

Symlinks, FIFOs, sockets and device nodes are still created locally and still
never synced — but Drivel now **logs one line naming each one**, when you create
it and when the enumeration sweep walks past it, and the sweep's summary counts
them. Silence was how "Drivel does not sync symlinks" reached people as "Drivel
lost my file".

One inconsistency fixed while writing that down: a regular file created with
`mknod(2)` rather than `open(O_CREAT)` emitted no event, so whether it synced
depended on which syscall made it and on whether Drivel was watching. It now syncs
like any other regular file.

On FreeBSD, creating a FIFO or a device node through the mount fails with
`EINVAL`. That is the platform, below anything Drivel controls; nothing is lost,
because none of these ever synced.

### The profiling endpoint stops trusting the network — 2026-09-08

`-pprof` now binds **loopback only**. A non-loopback address is refused at
startup and takes a new `-pprof-allow-remote` to proceed; before, it was served
with a warning, which arrives after the heap is already exposed. `-pprof 6060`
(a bare port) now means `127.0.0.1:6060` instead of failing to parse.

`/debug/pprof/cmdline` is no longer served at all. It returned the process's
command line, which names your credentials file, your token, your backing
directory and your account — a map to the secrets rather than the secrets.

Idle connections to the endpoint are now closed after a minute rather than held
open indefinitely. Nothing else about profiling changes: it is still off unless
you ask for it, still one endpoint for the whole process, and a port it cannot
bind is still a startup error.

### Tunable transfer concurrency, and a cap on lazy fetches — 2026-09-08

Two new per-mount settings: `-upload-workers` / `upload-workers` (default 4) for
how many files are uploaded at once, and `-hydrate-workers` / `hydrate-workers`
(default 8) for how many placeholders are fetched at once in lazy mode.

The upload pool was always four and could not be changed. The fetch pool did not
exist: a recursive read over a lazy tree faulted in every file simultaneously, so
a `grep -r` across a thousand placeholders opened a thousand downloads. It is now
bounded by default, which is a behaviour change for lazy mounts — a large parallel
read is paced rather than issued all at once.

They are two numbers rather than one because the directions are not alike: a
typical link's downlink is several times its uplink, the pools bound different
work, and a provider may cap the two differently. Both are per mount and nothing
budgets across mounts — a shared limit would let one account's traffic starve
another's and let each infer when the other was busy.

Raising the upload count buys less than it looks like it should, and
[the configuration guide](docs/user/configuration.md#transfer-concurrency) says
why. Writes to the same path stay serialised whatever you set. The inbound change
feed is unaffected: it still applies remote changes one at a time.

### Release builds are now statically linked — 2026-09-08

`make build` produces a binary with no library dependencies, so it runs on any
Linux of the same architecture regardless of that machine's glibc version.
Previously the default build linked the build host's C library, which meant a
binary built on a recent distro would refuse to start on an older one.

Distribution packagers can opt back in with `make build CGO_ENABLED=1`, which is
the point: a distro package should link the system's libraries so its security
updates arrive through the package manager rather than waiting on a Drivel
release.

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
