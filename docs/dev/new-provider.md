# Writing a storage provider

A provider is a second `provider.Store` — the common case. Adding a new **mount
frontend** (`mount.Backend`, e.g. a Windows cgofuse binding) is rarer and covered
briefly at the end.

The whole point of the `provider` seam is that the sync engine, FS layer, and CLI
don't change when you add a provider — you write one package and a three-line
`main`. Since M9 a backend runs in **its own process**, so it does not have to live
in this repository at all: `provider`, `ranges` and `plugin` are public packages,
and an out-of-tree module that imports them builds a plugin drivel will load.

A backend does not have to be a cloud. [DESIGN.md §9](../../DESIGN.md) sketches
several that are not: a deduplicating local store (M11), an encrypting layer (M12),
a block-level filesystem over a distributed database (M13), and a group of
filesystem backends — a plain directory, SFTP, SMB/CIFS, NFS and WebDAV (M17–M21),
written as one design because they share almost everything. Four notes for anyone
starting one.

A local store fits this interface as it stands — it is the optional interfaces
below (an exact `ChangeSource`, a real `RangePutter`) that make it interesting.

A *decorator* that wraps another provider does not: it would need to open its inner
store through the registry, and `provider.Params` carries no way to do that yet.
That hook is M12's one new piece of framework, so if you need it, it is a design
discussion before it is a patch. (What a decorator *does* now have is a way to
forward its inner store's capabilities in one value — see "Declaring capabilities"
below — which before M9 would have meant one wrapper type per subset.)

If your backend is already path-addressed — which every filesystem is — you need
none of the machinery `gdrive` carries for path↔ID translation.
`internal/pathindex` and the three-source resolution it feeds exist because Drive
is ID-addressed; a path index over a path-addressed store is a cache keyed by its
own value. Cache metadata if you must cache something, and that is `Stat`.

If your backend has no change feed, say so by omitting `ChangeSource` — do not
synthesize one by diffing listings. The M7b enumeration sweep already is that,
correctly, with the delete guards and the baseline rules that make an inferred
deletion safe. What changes is the *cadence*: with no feed the sweep is the only
inbound path, so `-sweep-interval` is a poll interval rather than a safety net and
your documentation should say what a sensible value is for your backend. Its 24 h
default is tuned for a backend that also has a feed.

And if your provider's configuration names a **local** path (a directory to sync
into, a mounted share), raise it before you write it: `app.Validate`'s overlap
guards only know about paths belonging to *mounts*, so nothing today stops a
provider directory from being pointed at a mount's backing tree or mountpoint.
That is a seam change (`provider.Params` needs a way to declare the paths a
provider will touch), not something to work around inside one provider.

## 1. Implement `provider.Store`

Create a package — `internal/provider/<name>/` in this repository, or any package
in your own module — and implement the required interface from
[provider/provider.go](../../provider/provider.go):

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
- **`Remove` deletes recursively, and should prefer whatever the backend's
  recoverable form is.** The seam says nothing about where the bytes go, because
  backends differ (Drive has a trash, an S3 bucket may have versioning, a CAS
  store has a GC policy). Take the recoverable one where there is a choice, and
  give the operator an explicit way to ask for the outright form: not every
  deletion the engine performs was typed by a user — `syncengine/reconcile.go`
  *infers* them from a baseline — and this is the one operation with no fallback
  above your package. `gdrive`'s `DeleteMode` is the worked example.

  **Take the recoverable form your backend already has; do not build one.** If it
  has none, delete outright and say so in your documentation — `-max-deletes` is
  then the only guard, which users are entitled to know. A trash you emulate
  inside the synced tree is broken by construction (the sweep enumerates it and
  restores everything), and one outside it is a second store with its own quota
  and garbage collection, added under the one call that has no fallback. `sftp` is
  the worked example of the honest version.
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

Providers without a cursor feed (S3, WebDAV, SFTP) simply omit it. That is a
supported configuration, not a degraded one, and it does **not** mean
outbound-only: a store that implements `Enumerator` but not `ChangeSource` gets a
downloader all the same, running sweep-only, and the M7b sweep is then the whole
inbound path. `-sweep-interval` stops being a long-period safety net and becomes
the poll interval — say what a sensible value is for your backend, because the
24 h default is tuned for one that also has a feed.

Do **not** fake a change feed by polling-and-diffing. The sweep already is that,
correctly: it infers a deletion only from a baseline that predates the sweep, it
keeps and pushes back a local copy that diverged, and `-max-deletes` abandons a
whole pass rather than trimming one it cannot justify. A hand-rolled differ
reproduces those guards or, far more likely, quietly does not.

A store with neither interface really is outbound-only, and gets no downloader.

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

## 4. Ship it as a plugin

Since M9 a backend is a **separate executable** that drivel launches and talks to
over gRPC on a unix socket. Nothing about the interface changed — what changed is
where the implementation lives — and the whole of the wiring is a `main`:

```go
// cmd/drivel-provider-<name>/main.go   (or your own repo, if you are out of tree)
package main

import (
    "github.com/zishmusic/drivel/plugin"
    "example.com/drivel-provider-thing/thing"
)

func main() {
    plugin.Serve(thing.Factory)
}
```

```go
// <name>/factory.go
func Factory(ctx context.Context, p provider.Params) (provider.Store, error) {
    var cfg Config                       // your own struct, with toml tags
    if err := p.Config.Decode(&cfg); err != nil {
        return nil, err
    }
    return Open(ctx, cfg, p.Log)         // return the untyped nil on error, not a
}                                        // typed nil inside a non-nil interface
```

Build it as **`drivel-provider-<name>`** and put it somewhere drivel looks:
alongside the `drivel` binary, in `$XDG_DATA_HOME/drivel/plugins`, in
`/usr/local/lib/drivel/plugins` or `/usr/lib/drivel/plugins`. `DRIVEL_PLUGIN_PATH`
replaces that list if you want it somewhere else.

**The kind is the filename.** `drivel-provider-thing` provides `thing`, which is
what a user writes as `provider = "thing"`. Your code never names it — a plugin
that could name itself could contradict its filename, and then two files could
claim one kind.

Three consequences worth knowing before you debug something confusing:

- **Standard output is the handshake.** go-plugin's greeting is written there, so a
  stray `fmt.Println` in your backend breaks the launch rather than appearing
  anywhere. Write to the `*log.Logger` in `provider.Params` (or to stderr); drivel
  forwards it, line by line, to the log of the mount that launched you.
- **Your environment is built, not inherited.** You get `PATH`, `HOME`, `TMPDIR`,
  locale, TLS roots, proxy settings and `SSH_AUTH_SOCK` — and nothing else. In
  particular, no ambient cloud credentials and no `XDG_*`. Everything your backend
  needs must come through its configuration, and paths in it should be absolute.
- **drivel refuses to launch a writable binary.** Group- or world-writable, or
  sitting in a world-writable non-sticky directory, and it will not run — with the
  reason reported when someone tries to use the backend, not silently.

Your config struct is decoded straight from the account's table in the config file,
so a user selects the backend with `provider = "<name>"` and configures it with
whatever keys you defined. Nothing in `internal/config` or `internal/app` learns
what those keys mean. A key you did **not** define is an error, which is
deliberate — a misspelt setting silently doing nothing is the failure that rule
exists to prevent.

### Declaring capabilities

Normally you declare nothing: you implement the optional interfaces you can honour
and drivel reads your method set. That is exact, and it is what the in-tree
backends do.

The exception is a type whose method set is *not* the truth about what it can do —
a decorator wrapping another store, or a test double. Such a type implements
`provider.Declarer`:

```go
func (s *myStore) Capabilities() provider.CapabilitySet { return s.caps }
```

and drivel takes the **intersection** of the declaration and the method set. A
declaration can only ever narrow, so you cannot accidentally advertise something
you have no method for.

### Running in process instead

There is no supported way to compile a backend into `drivel` — the registry it
builds has nothing in it but discovered plugins. `provider.Registry` is still a
plain value, though, so a test (or a program of your own built on these packages)
can register a `Factory` directly and exercise a `Store` with no subprocess at all.
That is how every backend's own tests run, and keeping those two ways of calling
the same code identical is why the plugin server adds no behaviour of its own.

## 5. Test it offline

Match the existing pattern: unit-test the path↔native translation and the
error-classification logic without a live account. The engine's event→API mapping
is already tested against the interface, so a conforming `Store` inherits that
coverage.

## Checklist

- [ ] Your package implements every `Store` method.
- [ ] Paths are root-relative, slash-separated, no leading slash.
- [ ] `Put`/`Mkdir` create ancestors; `Move` handles missing source via
      `ErrNotExist`.
- [ ] Transient errors are wrapped `Retryable() bool`; context cancellation isn't.
- [ ] `ChangeSource` implemented **iff** the provider has a native cursor feed;
      `Enumerator` implemented if the tree can be listed, which is what gives a
      feedless provider any inbound sync at all.
- [ ] `RangeGetter` implemented **iff** the provider serves real byte ranges.
- [ ] Store is safe for concurrent use.
- [ ] A `main` calling `plugin.Serve(Factory)`, built as `drivel-provider-<name>`
      and installed somewhere on the search path; nothing else changed.
- [ ] Nothing written to standard output — that is the handshake.
- [ ] Nothing read from the environment beyond what `plugin.EnvAllowed()` lists;
      everything else comes through the config, as an absolute path.
- [ ] `provider.Declarer` implemented **only** if the method set is not the truth
      (a decorator, a test double). Ordinary backends declare nothing.
- [ ] Config struct carries `toml` tags and errors on keys it does not define.
- [ ] Offline unit tests for translation + error classification, run against the
      `Store` directly — no subprocess needed.
- [ ] `go build ./...`, `go vet ./...`, `go test ./...` clean.

## Adding a new mount frontend (rare)

To support a platform without a Go FUSE binding, implement `mount.Backend` from
[internal/mount/mount.go](../../internal/mount/mount.go) (`Name()` + `Serve(ctx,
Options)`) in a new package, mirroring `internal/vfs`. Keep it below the seam:
proxy reads/writes to `opts.Backing`, emit one `fsevent.Event` per mutation on
`opts.Events`, and honour `ctx` cancellation to unmount. Platform-specific backing
resolution (like in-place mode's dirfd trick) belongs in `internal/mount`'s
`backing_*.go` files, guarded by build tags.
