# CLAUDE.md

Guidance for Claude Code working in this repository.

## What this is

A Go FUSE filesystem that mounts a local directory as an **interceptor**: every
operation is proxied to an underlying directory (the source of truth / local
cache) and asynchronously, bidirectionally synced with a cloud provider. First
(and currently only) provider: **Google Drive**, via the Drive API v3
`changes.list` cursor feed — **not** the Workspace Events API and **not**
webhooks.

Read [DESIGN.md](DESIGN.md) before making architectural changes. The
echo/loop-suppression model (§4) is the load-bearing correctness concern for
bidirectional sync — don't regress it.

> The `-gpu` in the repo/dir name is historical. GPU-accelerated deduplication
> is **out of scope for v1**; don't add it unless asked.

## Layout

- `cmd/drivel` — entrypoint; flag parsing, mount, signal-based unmount.
- `internal/fsevent` — backend-neutral change `Event`/`Op` types (shared by any
  mount backend and the sync engine).
- `internal/mount` — the mount-backend seam: `Backend` interface + `Options`, and
  `ResolveBacking` (separate-dir vs in-place). Platform bits in `backing_*.go`.
- `internal/vfs` — the go-fuse mount backend: loopback that proxies to the backing
  store and emits an `fsevent.Event` per mutation. Reads/lookups/attrs pass through.
- `internal/provider` — cloud-backend interface (the seam). Drive impl is M2.
- `internal/provider/gdrive` — Google Drive impl of the interface.
- `internal/gauth` — Google OAuth: credentials.json/token.json I/O and the
  interactive login flow (loopback redirect + manual paste, rclone-style).
- `internal/transport` — HTTP/3 (QUIC) client with HTTP/2 fallback, injected into the
  Drive provider (M2). See DESIGN.md §2.6.
- `internal/syncengine` — outbound push (`Engine`) + inbound pull loop
  (`Downloader`): `changes.list` cursor feed → backing dir, with §4 echo
  suppression and adaptive cadence. M4 adds per-path debounce + a path-hashed
  worker pool with retry/backoff on the push side, §6 conflict copies on the pull
  side, and a bounded drain on shutdown. M6 adds three gates before every content
  push (placeholder → unchanged-content hash → range write), each falling through
  to the whole-file `Put`.
- `internal/state` — engine-level bbolt sync state: the change-feed cursor and the
  echo-suppression records shared by the up/down paths (DESIGN.md §2.4, §4), plus
  the M5 hydration bitmap cache (opaque bytes; the encoding belongs to `ranges`).
- `internal/hydrate` — M5 lazy hydration: placeholder creation, the authoritative
  `user.drivel.placeholder` xattr marker, and whole-file hydrate-on-first-I/O with
  singleflight. Provider-agnostic.
- `internal/ranges` — leaf value package: the block bitmap (`Set`) used two ways —
  M5's present-ranges and M6's dirty-ranges. Pure, no I/O. It lives outside
  `hydrate` on purpose: M6 runs in eager mode too, and the default path must not
  import the lazy package to describe a write.

## CLI

`drivel` has two subcommands (`mount` is the default, so `drivel -mount … -data …`
still works): `drivel login` (interactive OAuth wizard → credentials.json +
token.json) and `drivel mount`. `mount` is **non-interactive**: it requires a
token from `login` and never prompts on stdin. The login loopback server uses
rclone's port **53682**; forward it into the container for auto-capture, else use
the paste fallback. When adding stdin prompts, share ONE bufio reader — multiple
readers on os.Stdin race and swallow buffered lines.

## Milestones

**v1, shipped.** M1 passthrough mount · M2 transport (HTTP/3→HTTP/2) + Drive auth +
push-on-close (incl. file-handle write capture for `OpWrite`) · M3 `changes.list`
pull loop + echo suppression + `internal/state` bbolt cursor/echo store · M4 full
bidirectional: per-path debounce + worker pool, retries/backoff, §6 conflict
copies, bounded drain on shutdown.

**v2, in progress.** M5 lazy hydration **shipped**: `drivel mount -lazy` (opt-in;
default is unchanged eager mode). See "Lazy hydration" below for the invariants.
M6 range writes **shipped**: always on, no flag. See "Range writes" below.

**v2, planned** (DESIGN.md §9 has the detail). M7 path↔ID index persistence (provider-private bbolt store, latency only, must
stay non-load-bearing) · M8 multi-account (real) + multi-provider (framework only;
proven with a pseudo-provider that is `gdrive` registered under a second name) ·
M9 plugin architecture (out-of-process or WASM; Go's `plugin` package is a poor
fit).

DESIGN.md §10 is a design note on carrying POSIX metadata (mode, ACLs, xattrs,
SELinux) over Drive — unscheduled. If you touch it, the rule is that permission
metadata from a remote source is *executable trust*: authenticate it, bind it to
the file, fail closed, and mask setuid/setgid by default.

## Lazy hydration (M5)

Three invariants; breaking any of them loses user data.

1. **The xattr is authoritative, the DB is a cache.** `user.drivel.placeholder`
   lives on the backing file; the `RangeSet` in `internal/state` is a fast path.
   Never invert this — the xattr is what survives losing `drivel-state.db`.
2. **Never push a placeholder.** `syncengine.Placeholders` gates every content
   upload. A placeholder holds zero bytes at full apparent size, so uploading it
   replaces the remote file with nothing. The guard fails **safe**: unsure ⇒ report
   "placeholder" ⇒ skip. Do *not* replace it with a size heuristic — a legitimate
   truncate-to-zero looks identical and must still sync.
3. **Hydrate on first I/O, not on `Open`.** `fileHandle.ensureResident` does it, so
   open/truncate/rewrite fetches nothing. FUSE delivers `O_TRUNC` as a separate
   `Setattr` unless `atomic_o_trunc` is negotiated, so `Open` handles only the
   atomic case (dropping the mark), and `Setattr` handles truncation.

Also: a failed hydration must return `EIO`, never a short read (zeros served as
content are silent corruption); and §6 conflict copies are always downloaded in
full, because their paths exist only locally and could never be hydrated later.

## Range writes (M6)

Three gates run before every content push, in `syncengine.pushContent`; anything
that doesn't clear one falls through to the whole-file `Put` that M1–M5 always
did. That fallback is never wrong, only slower — which is what makes the whole
feature safe.

1. **Placeholder** (M5) — unchanged, and nothing may get in front of it.
2. **Range write** — `provider.RangePutter`, only when the extents are known, the
   object exists remotely at exactly the local size, **and the remote still
   matches the §4 echo record**. That last one is not optional: a whole-file `Put`
   over someone's edit loses it per documented §6 policy, but splicing extents
   into a diverged remote makes a hybrid file that existed nowhere, with no
   conflict copy. Diverged / no echo / no state store ⇒ decline.
3. **Unchanged content** — hash the local bytes (`provider.ContentHasher`, so the
   engine never assumes an algorithm) and compare to **what `Stat` says the remote
   holds now**, not to the echo. A stale echo (expired cursor, reused state DB,
   missed delete) would otherwise skip the push forever and never restore the
   file. This is the gate that actually helps Drive users.

Gates 2 and 3 share one `Stat` and are both skipped below `hashSkipMinSize` (one
block) — for a small file the round-trip costs what the upload would. Gate 2 runs
before gate 3 on purpose: hashing reads the whole file, so checking "did anything
change?" first would spend a 4 GB read to avoid a 4 MiB upload.

**Drive cannot patch byte ranges** — `files.update` replaces content wholesale and
resumable chunks still carry a complete new body. `gdrive` therefore does *not*
implement `RangePutter`, deliberately; don't "fix" that. What Drive gets from M6 is
gate 3 plus chunked/resumable upload sessions (`mediaOptions`). Session URIs are
not persisted: this survives a flaky network, not a restart. `ChunkTransferTimeout`
is intentionally unset — it is a hard per-attempt deadline that never resets on
progress, so setting it caps the slowest link that can ever finish a chunk, and it
must stay well under `ChunkRetryDeadline` or the retry never fires.

The invariant, and it mirrors M5's: **`fsevent.Event.Dirty == nil` means "extents
unknown", which means push the whole file.** Anything that cannot account for every
changed byte must report nil — a size change (truncate/`fallocate` poisons the
handle's tracker), an event with no handle behind it, a rename fallback, a union
across mismatched block grids, a file that shrank behind an open handle, an
unreadable size. In the coalescer, unknown *absorbs* known. Forgetting an extent
corrupts a file; sending too much costs bandwidth.

One trap already paid for: a handle only sees its own writes, so its idea of the
file's length stops at the last byte written. `Release` fstats the file (before the
wrapped handle closes the fd) and grows the set to the real size. Skip that and the
engine's size cross-check rejects every set — M6 silently never fires.

In `internal/ranges`, present-ranges round **inward** (`Mark`) and dirty-ranges
round **outward** (`MarkCovering`). Same bitmap, opposite rounding, and the
asymmetry is the point.

## Transport

All Drive traffic goes over **HTTP/3** (QUIC), required by project decision. Go 1.26
stdlib has no HTTP/3 client — use `github.com/quic-go/quic-go` (`http3.Transport`).
It's HTTP/3-*preferred*: fall back to HTTP/2 when the QUIC/UDP dial fails. The
transport sits **below** OAuth — pass the client via `option.WithHTTPClient` and fold
the token source into `oauth2.Transport{Base: ...}`; do **not** also pass
`WithTokenSource` (conflicts). See DESIGN.md §2.6.

## Mount modes

`drivel mount -mount DIR -data BACKING` uses a separate backing directory
(portable; required off Linux). Omitting `-data` selects **in-place mode**: the
mount dir is its own backing store, so files remain in it after Drivel exits. It
works by opening a dirfd to the mountpoint *before* mounting and routing backing
I/O through `/proc/self/fd/N` (Linux-only). **Cardinal rule:** in in-place mode,
never access the backing store by the mountpoint path — only via `backing.Path`
(the `/proc/self/fd/N` handle) — or reads/writes recurse into our FUSE handler and
deadlock. See DESIGN.md §2.7. New mount backends implement `mount.Backend`; keep
everything below the seam provider- and FUSE-agnostic.

## Build / test / run

```sh
go build ./...
go vet ./...
go build -o ./bin/drivel ./cmd/drivel
./bin/drivel mount -mount ./mnt -data ./data   # separate backing dir; -debug for FUSE tracing
./bin/drivel mount -mount ./dir                # in-place: ./dir is its own backing (Linux)
```

`./mnt` and `./data` are gitignored scratch dirs; create them (the binary
mkdirs them too). Operate on `./mnt`; changes land in `./data` and log as sync
events. Ctrl-C unmounts.

## Environment notes

- Mounting needs the `fusermount3` helper from the **`fuse3`** package (Debian);
  `libfuse3-dev` alone is only headers. Install: `sudo apt-get install -y fuse3`.
- This runs in a **privileged Docker container** with passwordless `sudo` and
  `/dev/fuse` present, so unprivileged FUSE mounts work once `fuse3` is installed.
- If a run leaves a stale mount: `fusermount3 -u ./mnt`.
- HTTP/3/QUIC wants a larger UDP receive buffer or quic-go logs a warning; raise it
  with `sudo sysctl -w net.core.rmem_max=7500000` (and `wmem_max`) when testing sync.

## Conventions

- `context.Context` threaded for cancellation/shutdown; the mount unmounts on
  SIGINT/SIGTERM, which makes `server.Wait()` return.
- Change events use root-relative paths (no leading slash); `NewPath` is set only
  for `OpRename`.
- Keep the FS layer provider-agnostic — depend on `internal/provider`, never on a
  concrete Drive type.
- FS operations must not block on the network; sync happens off the FUSE path via
  the buffered event channel.
- Secrets (`credentials.json`, `token.json`, `*.local.json`) are gitignored —
  never commit them.
