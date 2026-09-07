# Writing a storage provider

A provider is a second `provider.Store` — the common case. Adding a new **mount
frontend** (`mount.Backend`, e.g. a Windows cgofuse binding) is rarer and covered
briefly at the end.

The whole point of the `internal/provider` seam is that the sync engine, FS layer,
and CLI don't change when you add a provider — you write one package.

A backend does not have to be a cloud. [DESIGN.md §9](../../DESIGN.md) sketches three
that are not: a deduplicating local store (M11), an encrypting layer (M12) and a
block-level filesystem over a distributed database (M13). Two notes for anyone
starting one. A local store fits this interface as it stands — it is the optional
interfaces below (an exact `ChangeSource`, a real `RangePutter`) that make it
interesting. A *decorator* that wraps another provider does not: it would need to
open its inner store through the registry, and `provider.Params` carries no way to
do that yet. That hook is M12's one new piece of framework, so if you need it, it
is a design discussion before it is a patch.

## 1. Implement `provider.Store`

Create `internal/provider/<name>/` and implement the required interface from
[internal/provider/provider.go](../../internal/provider/provider.go):

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

## 2. Signal retryability (don't import the engine)

The engine classifies errors with `provider.IsRetryable`, which uses an
`errors.As` probe for an `interface{ Retryable() bool }` — so you signal
"transient, retry with backoff" without importing `syncengine`. Follow the Drive
implementation in
[internal/provider/gdrive/errors.go](../../internal/provider/gdrive/errors.go):

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

## 3. (Optional) Implement `ChangeSource` for inbound sync

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

## 3b. (Optional) Implement `RangeGetter` for lazy hydration

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

## 4. Register it

Since M8 backends are selected by name through a `provider.Registry`, so wiring one
in is a `Factory` plus one line of registration.

```go
// internal/provider/<name>/factory.go
func Factory(ctx context.Context, p provider.Params) (provider.Store, error) {
    var cfg Config                    // your own struct, with toml tags
    if err := p.Decode(&cfg); err != nil {
        return nil, err
    }
    return Open(ctx, cfg, p.Log)      // return the untyped nil on error, not a
}                                     // typed nil inside a non-nil interface
```

```go
// cmd/drivel/mount.go
_ = reg.Register("<name>", yourpkg.Factory)
```

Your config struct is decoded straight from the account's table in the config file,
so a user selects the backend with `provider = "<name>"` and configures it with
whatever keys you defined. Nothing in `internal/config` or `internal/app` learns
what those keys mean, and neither needs changing.

Registration is deliberately by hand rather than by `init()`: an explicit registry
keeps the tree free of process-global mutable state, and it is what lets a test
register the same factory twice to check that two independently-configured stores
really are independent.

Everything downstream — `syncengine.New`, the `ChangeSource` type assertion, the
downloader — is already provider-agnostic.

## 5. Test it offline

Match the existing pattern: unit-test the path↔native translation and the
error-classification logic without a live account. The engine's event→API mapping
is already tested against the interface, so a conforming `Store` inherits that
coverage.

## Checklist

- [ ] `internal/provider/<name>/` implements every `Store` method.
- [ ] Paths are root-relative, slash-separated, no leading slash.
- [ ] `Put`/`Mkdir` create ancestors; `Move` handles missing source via
      `ErrNotExist`.
- [ ] Transient errors are wrapped `Retryable() bool`; context cancellation isn't.
- [ ] `ChangeSource` implemented **iff** the provider has a native cursor feed.
- [ ] `RangeGetter` implemented **iff** the provider serves real byte ranges.
- [ ] Store is safe for concurrent use.
- [ ] A `Factory` registered in `cmd/drivel/mount.go`; nothing else changed.
- [ ] Config struct carries `toml` tags and errors on keys it does not define.
- [ ] Offline unit tests for translation + error classification.
- [ ] `go build ./...`, `go vet ./...`, `go test ./...` clean.

## Adding a new mount frontend (rare)

To support a platform without a Go FUSE binding, implement `mount.Backend` from
[internal/mount/mount.go](../../internal/mount/mount.go) (`Name()` + `Serve(ctx,
Options)`) in a new package, mirroring `internal/vfs`. Keep it below the seam:
proxy reads/writes to `opts.Backing`, emit one `fsevent.Event` per mutation on
`opts.Events`, and honour `ctx` cancellation to unmount. Platform-specific backing
resolution (like in-place mode's dirfd trick) belongs in `internal/mount`'s
`backing_*.go` files, guarded by build tags.
