# Development guidelines

Working notes for contributors. Read [DESIGN.md](../DESIGN.md) first for the
architecture and the sync/echo model; read [CLAUDE.md](../CLAUDE.md) for the
conventions this repo enforces. The two diagrams in
[architecture.md](architecture.md) show the runtime components and the package
dependency graph.

## Conventions that matter

- **Keep the core provider- and FUSE-agnostic.** The sync engine depends only on
  the `internal/provider` and `internal/mount` seams and on `internal/fsevent` —
  never on a concrete Drive or go-fuse type. New backends implement an interface;
  they don't get special-cased upstream.
- **Never block the FUSE path on the network.** Filesystem operations proxy to the
  backing store and, for mutations, emit an `fsevent.Event` on a buffered channel.
  All network work happens off that path.
- **Paths are root-relative slash paths, no leading slash** — the same form
  `fsevent` emits and `provider.Store` consumes. `NewPath` is set only on
  `OpRename`.
- **`context.Context` is threaded for cancellation/shutdown.** SIGINT/SIGTERM
  unmounts, which makes `server.Wait()` return; the engine then drains on a
  background context (see `cmd/drivel/mount.go`).
- **In-place mode's cardinal rule:** never touch the backing store by the
  *mountpoint* path — only via `backing.Path` (the `/proc/self/fd/N` handle) — or
  reads/writes recurse into the FUSE handler and deadlock. See DESIGN.md §2.7.
- **Secrets** (`credentials.json`, `token.json`, `*.local.json`,
  `drivel-state.db`) are gitignored. Never commit them.

## Build and test

```sh
go build ./...                          # compile everything
go vet ./...                            # static checks — keep clean
go test ./...                           # unit tests (no network required)
go build -o ./bin/drivel ./cmd/drivel   # the CLI binary
```

`go test ./...` runs offline: the transport, event→API mapping, provider, and
sync helpers are unit-tested without hitting Drive. Run it (and `go vet`) before
every commit.

### Running the binary

```sh
# Separate backing dir: operate on ./mnt; changes land in ./data and log as events.
./bin/drivel mount -mount ./mnt -data ./data

# In-place (Linux): ./dir is its own backing store; files stay put on exit.
./bin/drivel mount -mount ./dir

# With Drive sync, after `drivel login`:
./bin/drivel mount -mount ./mnt -data ./data \
  -credentials credentials.json -token token.json -drive-root <folderID>
```

`./mnt` and `./data` are gitignored scratch dirs. Operate on the mount; watch the
log for sync events. Ctrl-C unmounts (a second Ctrl-C hard-exits).

Without `-credentials`, Drivel runs **log-only**: no auth, no network, just prints
the change events it *would* sync. This is the fastest way to exercise the FUSE
and event layers.

### Environment setup

- **FUSE helper.** Mounting needs `fusermount3` from the system **`fuse3`**
  package (`libfuse3-dev` is headers only). Install: `sudo apt install fuse3`
  (Debian) or `sudo dnf install fuse3` (Fedora). This repo's container is
  privileged with `/dev/fuse` present, so unprivileged mounts work once `fuse3` is
  installed.
- **Stale mount** after a crash: `fusermount3 -u ./mnt`.
- **HTTP/3 UDP buffer.** quic-go warns if the UDP receive buffer is small; raise
  it when testing sync: `sudo sysctl -w net.core.rmem_max=7500000` (and
  `wmem_max`).

## Debugging

- **`-debug`** on `mount` turns on go-fuse's FUSE-level tracing — every VFS call
  in and out. Verbose, but the fastest way to see what the kernel is asking for.
- **Log-only mode** (omit `-credentials`) isolates the FS/event layers from sync:
  if a bug reproduces here, it's not in the provider or network path.
- **Sync bugs** are usually echo/loop-suppression (DESIGN.md §4). Inspect the
  state DB — it's bbolt — to see the cursor and echo records. Reproduce inbound
  behaviour by editing a file directly in Drive and watching the downloader poll.
- **Transport.** HTTP/3 is preferred with an HTTP/2 fallback; a QUIC/UDP dial
  failure silently falls back. If you suspect the transport, force the failure
  path or add logging in `internal/transport` — don't assume you're on H3.
- **Deadlock on a mount** almost always means something read the backing store via
  the mountpoint path in in-place mode (the cardinal rule above). Check that every
  backing access goes through `backing.Path`.
- **Stuck/renamed test mounts:** `fusermount3 -u <dir>` before retrying.

## Creating a new storage backend

A "backend" here means a new **cloud provider** (a second `provider.Store`), which
is the common case. Adding a new **mount frontend** (`mount.Backend`, e.g. a
Windows cgofuse binding) is rarer and covered briefly at the end.

The whole point of the `internal/provider` seam is that the sync engine, FS layer,
and CLI don't change when you add a provider — you write one package.

### 1. Implement `provider.Store`

Create `internal/provider/<name>/` and implement the required interface from
[internal/provider/provider.go](../internal/provider/provider.go):

```go
type Store interface {
    Put(ctx context.Context, path string, r io.Reader) (RemoteFile, error)
    Mkdir(ctx context.Context, path string) (RemoteFile, error)
    Move(ctx context.Context, oldPath, newPath string) (RemoteFile, error)
    Remove(ctx context.Context, path string) error
    Get(ctx context.Context, path string) (io.ReadCloser, error)
    Stat(ctx context.Context, path string) (rf RemoteFile, ok bool, err error)
}
```

Rules the engine relies on:

- **Path-addressed, root-relative slash paths, no leading slash.** If your backend
  is natively ID-addressed (like Drive) or key-addressed (like S3), own the
  path↔native translation *inside* your package. The engine never sees your IDs.
- **`Put` and `Mkdir` create missing ancestor directories.** The engine never
  pre-creates parents.
- **`Move`** renames/relocates. If there's no native server-side move (e.g. S3),
  implement it as copy+delete — note in your docs that this resets object
  identity. Return `provider.ErrNotExist` when the source path is unknown; the
  engine treats that as "upload the destination as fresh content."
- **Concurrency-safe.** The uploader and downloader call your `Store` from
  different goroutines. Guard shared state.

### 2. Signal retryability (don't import the engine)

The engine classifies errors with `provider.IsRetryable`, which uses an
`errors.As` probe for an `interface{ Retryable() bool }` — so you signal
"transient, retry with backoff" without importing `syncengine`. Follow the Drive
implementation in
[internal/provider/gdrive/errors.go](../internal/provider/gdrive/errors.go):

```go
type transientError struct{ err error }

func (e transientError) Error() string   { return e.err.Error() }
func (e transientError) Unwrap() error   { return e.err }
func (e transientError) Retryable() bool { return true }
```

Wrap rate limits, 5xx, and dropped connections as retryable; leave bad-request /
permission / not-found errors unwrapped (permanent). **Never** mark
`context.Canceled` / `context.DeadlineExceeded` retryable — those are intent, not
faults.

### 3. (Optional) Implement `ChangeSource` for inbound sync

Inbound sync is opt-in. If your provider has a native incremental change feed
(Drive's `changes.list`, Dropbox's `list_folder/continue`), also implement:

```go
type ChangeSource interface {
    StartCursor(ctx context.Context) (string, error)
    Changes(ctx context.Context, cursor string) (changes []RemoteChange, next string, err error)
}
```

The CLI enables the pull loop only for stores that also satisfy this interface
(`store.(provider.ChangeSource)`). Providers without a cursor feed (S3, WebDAV)
simply omit it and run **outbound-only** — that's a supported configuration, not a
degraded one. Do **not** fake a change feed by polling-and-diffing; leave it
unimplemented and let the engine skip inbound.

If you implement `ChangeSource` and expect anyone to run your provider with
`-lazy`, note that lazy hydration turns each inbound change into a *placeholder*
rather than a download, and the file's content is fetched later by path via `Get`.
Your `Get` must therefore still resolve a path the pull loop reported minutes or
days earlier.

### 3b. (Optional) Implement `RangeGetter` for lazy hydration

If your backend can serve byte ranges (an HTTP `Range` header, S3's `Range`, a
seekable handle), also implement:

```go
type RangeGetter interface {
    GetRange(ctx context.Context, path string, off, length int64) (io.ReadCloser, error)
}
```

`length <= 0` means "to end of object", and the returned reader covers at most the
requested extent starting at `off`. This is what M5b's per-block faulting uses. It
is genuinely optional: M5's whole-file hydration works through plain `Get`, so
omitting it costs efficiency on large files, not features.

Don't emulate it by fetching the whole object and discarding the prefix — that is
worse than not implementing it, because the hydrator would then choose the ranged
path believing it to be cheap.

If your backend can *write* byte ranges in place, implement M6's mirror image:

```go
type RangePutter interface {
    PutRange(ctx context.Context, path string, src io.ReaderAt, size int64, extents []ranges.Range) (provider.RemoteFile, error)
}
```

The uploader calls it only after checking that the object exists at exactly `size`
and that the mount reported which extents changed, so your implementation gets a
narrow contract: overwrite those extents, never create, never resize, and fail
rather than doing something clever if the size disagrees. Failing is cheap — the
engine logs it and re-does the write as a whole-file `Put`.

Google Drive omits this one, and that is not an oversight to copy blindly: Drive
genuinely has no partial-content write. Omit it if your backend is the same;
implement it if you have `Range`-style PUTs, multipart part replacement, or a
seekable handle.

Finally, if your backend exposes a content checksum in `RemoteFile.Hash`, also
implement:

```go
type ContentHasher interface {
    HashContent(r io.Reader) (string, error)
}
```

It must produce exactly the digest and encoding you put in `RemoteFile.Hash`
(Drive: MD5, lowercase hex). The engine uses it to skip uploads whose content did
not really change. Getting the encoding subtly wrong is harmless in the dangerous
direction — hashes simply never match and every push proceeds — but you lose the
optimisation, so it is worth a test against a real round-trip.

### 4. Wire it into the CLI

Backend selection currently lives in [cmd/drivel/mount.go](../cmd/drivel/mount.go),
where `gdrive.Open(...)` constructs the store and assigns it to a
`provider.Store`. Add your constructor alongside it behind a flag (e.g. a
`-provider` selector or a provider-specific credentials flag). Everything
downstream — `syncengine.New`, the `ChangeSource` type assertion, the downloader —
is already provider-agnostic and needs no changes.

### 5. Test it offline

Match the existing pattern: unit-test the path↔native translation and the
error-classification logic without a live account. The engine's event→API mapping
is already tested against the interface, so a conforming `Store` inherits that
coverage.

### Checklist

- [ ] `internal/provider/<name>/` implements every `Store` method.
- [ ] Paths are root-relative, slash-separated, no leading slash.
- [ ] `Put`/`Mkdir` create ancestors; `Move` handles missing source via
      `ErrNotExist`.
- [ ] Transient errors are wrapped `Retryable() bool`; context cancellation isn't.
- [ ] `ChangeSource` implemented **iff** the provider has a native cursor feed.
- [ ] `RangeGetter` implemented **iff** the provider serves real byte ranges.
- [ ] Store is safe for concurrent use.
- [ ] Wired into `cmd/drivel/mount.go` behind a flag; nothing else changed.
- [ ] Offline unit tests for translation + error classification.
- [ ] `go build ./...`, `go vet ./...`, `go test ./...` clean.

### Adding a new mount frontend (rare)

To support a platform without a Go FUSE binding, implement `mount.Backend` from
[internal/mount/mount.go](../internal/mount/mount.go) (`Name()` + `Serve(ctx,
Options)`) in a new package, mirroring `internal/vfs`. Keep it below the seam:
proxy reads/writes to `opts.Backing`, emit one `fsevent.Event` per mutation on
`opts.Events`, and honour `ctx` cancellation to unmount. Platform-specific backing
resolution (like in-place mode's dirfd trick) belongs in `internal/mount`'s
`backing_*.go` files, guarded by build tags.
