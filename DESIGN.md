# Drivel — Design

A Go FUSE filesystem that mounts a local directory as an **interceptor**, proxies
all operations to an underlying directory (the source of truth / local cache), and
**asynchronously and bidirectionally syncs** that directory with a cloud-storage
provider. First (and currently only) provider: **Google Drive**.

> GPU-accelerated deduplication is explicitly **out of scope for v1** (the repo name
> is historical). v1 is a clean interceptor → Drive sync engine. Deduplication
> without the GPU returns in §9 as M11, as a *provider* rather than as a layer
> inside the mount — unscheduled, and the hashing stays CPU-side.

---

## 1. Goals & non-goals

**Goals (v1)**
- Mount a local path; every FS op is applied to an underlying directory (passthrough).
- Push local changes to Google Drive asynchronously (don't block FS ops on network).
- Pull remote changes from Google Drive into the underlying directory.
- Event-driven *feel* without webhook infrastructure (cursor-based change polling).
- Robust against sync loops (a change we push must not bounce back and re-apply).

**Non-goals (v1)**
- Deduplication / content-defined chunking / GPU. (v2+)
- Multi-provider abstraction beyond a thin interface. (Drive-only now, but don't
  hard-code Drive into the FS layer.)
- Conflict *resolution* UI. v1 uses a deterministic policy (last-writer-wins by
  mtime, with conflict copies) and logs conflicts.
- Real-time collaborative editing semantics.

---

## 2. Component overview

```
          ┌─────────────────────────────────────────────────────────┐
          │                        Process                           │
          │                                                          │
  user →  │  ┌──────────────┐   ops    ┌───────────────┐            │
  (VFS)   │  │  FUSE layer  │ ───────►  │  Underlying   │            │
          │  │ (go-fuse     │           │  directory    │            │
          │  │  loopback)   │ ◄───────  │ (source of    │            │
          │  └──────┬───────┘   data    │  truth/cache) │            │
          │         │                   └───────┬───────┘            │
          │         │ change events              │  fs watch/journal │
          │         ▼                            ▼                   │
          │  ┌─────────────────────────────────────────┐            │
          │  │            Sync Engine                    │           │
          │  │  ┌───────────┐        ┌────────────────┐ │           │
          │  │  │ Uploader  │        │  Downloader    │ │           │
          │  │  │ (local→   │        │  (remote→      │ │           │
          │  │  │  Drive)   │        │   local)       │ │           │
          │  │  └─────┬─────┘        └───────┬────────┘ │           │
          │  │        │                       │          │          │
          │  │   ┌────▼───────────────────────▼─────┐   │           │
          │  │   │   State store (bbolt): path↔fileID│   │          │
          │  │   │   cursor, versions, pending ops   │   │          │
          │  │   └───────────────┬───────────────────┘  │           │
          │  └───────────────────┼──────────────────────┘           │
          └──────────────────────┼──────────────────────────────────┘
                                 │
                          ┌──────▼───────┐
                          │  Provider    │  Google Drive API v3
                          │  (Drive)     │  - files.* (CRUD)
                          └──────┬───────┘  - changes.list (pull cursor)
                                 │ injected *http.Client
                          ┌──────▼───────────────────┐
                          │  Transport (§2.6)         │  HTTP/3 (QUIC) preferred,
                          │  HTTP/3 → HTTP/2 fallback  │  HTTP/2 fallback if UDP
                          └───────────────────────────┘  blocked; OAuth wraps it
```

### 2.1 FUSE layer
- Built on [`hanwen/go-fuse`](https://github.com/hanwen/go-fuse), embedding its
  `LoopbackNode` and overriding write-side ops (`Create`, `Write`, `Rename`,
  `Unlink`, `Mkdir`, `Rmdir`, `Setattr`, `Release`).
- Reads pass straight through to the underlying dir (cache-first; v1 assumes the
  underlying dir holds full file content — no lazy hydration yet).
- On each mutating op, after it succeeds against the underlying dir, it enqueues a
  **local change event** to the Sync Engine. FS ops never block on the network.
- **Extended attributes are refused, not proxied** — `ENOSYS`, which the kernel
  reports to the caller as `EOPNOTSUPP` and then stops asking. `drivel mount
  -xattr` (config: `xattr = true`) turns the passthrough on; the default is off.

**Why xattrs are the one thing that does not pass through.** Everything else about
the FUSE layer is a loopback, and a loopback that dropped xattrs would just be
lossy. This one is not lossy, it is *load-bearing*: drivel's own control metadata
lives in an xattr on the backing file (`user.drivel.placeholder`, §9/M5, whose
authority over the state DB is invariant 1 of that milestone). Proxying xattrs
publishes that marker at the mountpoint and makes it writable by anything that can
write there, which turns two of M5's invariants into ordinary user commands:

- `setfattr -x user.drivel.placeholder` on an unhydrated file removes the mark, so
  the next push replaces the *remote* file with the placeholder's zero bytes —
  M5 invariant 2, reached without touching drivel.
- `setfattr -n user.drivel.placeholder -v …` on a resident file adds one, so the
  next read hydrates the remote copy over local content.

Neither needs privilege and neither leaves a trace, so the exposure is opt-in
rather than a caveat. Nothing inside drivel wants the passthrough either — the
hydrator reads and writes the marker on the backing path, *below* the mount, so
the option changes nothing about how M5 works; and no xattr is carried to the
provider in either mode (§10 is the unscheduled design note on doing that).

Mechanically it is two guards, because one is not enough. `fuse.MountOptions.
DisableXAttrs` stops the kernel issuing GETXATTR and LISTXATTR at all — one
`ENOSYS` per mount rather than one per file — but go-fuse's `doSetXAttr` and
`doRemoveXAttr` have no such check, so SETXATTR and REMOVEXATTR would still reach
`LoopbackNode` and land on the backing file. That is the *write* side, i.e. the
dangerous one, so `internal/vfs/xattr.go` also overrides all four node ops. A test
asserts the second guard by itself: removing it lets an attribute set through the
mountpoint appear on the backing file with `DisableXAttrs` still in force.

**The backing file is opened readable even when the caller asked to write.** A
kernel may read through a write handle. FreeBSD's fusefs fills a buffer-cache block
before writing part of it, and `fuse_io_strategy` deliberately falls back to the
write filehandle for that read-modify-write when no read handle is open — so a READ
arrives on a handle opened `O_WRONLY`. Passing the caller's flags straight to
`LoopbackNode.Open` leaves the backing fd write-only, go-fuse resolves its fd-backed
`ReadResult` with a `pread` at the moment it writes the reply rather than when it
builds it, and the resulting `EBADF` reaches the caller as the *write* failing —
after a `-debug` trace has already logged that READ as `OK`, which is what makes it
hard to read. `node.Open` therefore opens `O_WRONLY` as `O_RDWR`, falling back to
the caller's flags if that fails, because write permission does not imply read
permission and a mode-0222 backing file must still open.

Linux never issues that read, so the upgrade is invisible there. It is one body
rather than a FreeBSD build tag for the reason `hydrate/xattr_unix.go` shares its:
the delta a build tag would create is code no tested platform runs. This was found
by running `internal/vfs` on FreeBSD (§9/M10) and it is a mount-layer bug, not an
xattr one — four tests failed there and none of them was about M5.

### 2.2 Underlying directory
- The real bytes live here. It is both the passthrough target and the local cache.
- Acts as the reconciliation point: both the FUSE layer and the Downloader write
  here; the Uploader reads from here.

### 2.3 Sync Engine
- **Uploader**: consumes local change events, applies them to the store by path
  (Put/Move/Remove/Mkdir), records the resulting content hash + version for echo
  suppression. Path↔fileID mapping lives inside the provider (§2.5), not here.
- **Downloader**: runs the change-feed poll loop (a store's optional `ChangeSource`),
  applies remote deltas to the underlying directory, updates the state store.
- Both coordinate through the **state store** and the **echo-suppression** logic
  (§4) so neither re-processes the other's writes.

### 2.4 State store (bbolt)
Single embedded key/value DB ([`go.etcd.io/bbolt`](https://github.com/etcd-io/bbolt)).
Ownership splits along the provider seam (§2.5):

Provider-internal (below the seam — Drive's private path↔ID translation, M7). A
**separate DB file** (`drivel-index.db`), not a bucket in this one: its contents are
meaningless outside the one provider and the one account that wrote them, and the
engine must never be able to reach them. See §2.5 and `internal/pathindex`.
- `path`       : relative path → Drive fileID
- `id`         : Drive fileID → relative path (the change feed's direction)
- `meta`       : the account+root this index was built against (see §2.5)

Engine-level (provider-agnostic sync state):
- `cursor`     : the change-feed cursor (single key; opaque provider token)
- `pending`    : in-flight/echo-suppression records, keyed by path + content hash (see §4)
- `hydration`  : per-path present-ranges bitmaps (M5), opaque here — a cache over the
  authoritative xattr marker
- `sweep`      : the in-progress enumeration sweep (M7b): the pre-sweep change-feed
  token, how far the sweep got, and the generation the marks below belong to — plus,
  under a separate key, when the last sweep *completed*, which is what the periodic
  schedule (`-sweep-interval`) measures from. Persisted rather than counted from
  process start because the gap a periodic sweep closes is the one where drivel was
  not running: an interval measured from startup would never elapse for someone who
  mounts for an hour a day
- `seen`       : per-generation marks for the paths one sweep observed remotely. They
  are persistent rather than in-memory precisely so a sweep that resumed after a
  restart still knows about the pages its predecessor consumed — an in-memory set
  would report every one of them as remotely deleted (§9, M7b)

### 2.5 Provider interface — path-addressed store + optional change feed
Thin seam so the FS/sync layers don't hard-code Drive. The seam is **path-addressed**:
every method speaks the same root-relative slash paths that `internal/fsevent` emits.
A provider's native addressing — Drive's opaque fileIDs, S3 keys, WebDAV URLs — is its
own private concern; the sync engine never sees it. This is deliberately unlike Drive's
own API (which is ID-addressed): pushing the path↔native-ID translation *below* the seam
is what lets a path-addressed backend (S3, local FS, WebDAV) slot in without synthesizing
fake IDs, and it keeps the engine free of provider-shaped state.

The surface is split into a required store and an optional change feed, so a provider
that has no incremental feed can still be used (outbound-only):

```go
// Required. Put/Mkdir create missing ancestor dirs; the engine never pre-creates parents.
type Store interface {
    Put(ctx, path string, r io.Reader) (RemoteFile, error)       // create-or-replace
    Mkdir(ctx, path string) (RemoteFile, error)
    Move(ctx, oldPath, newPath string) (RemoteFile, error)
    Remove(ctx, path string) error
    Get(ctx, path string) (io.ReadCloser, error)
    Stat(ctx, path string) (rf RemoteFile, ok bool, err error)
}

// Optional capability: an incremental inbound feed (the M3 pull loop). The engine
// enables inbound sync only for stores that also satisfy this — Drive does
// (changes.list); an S3/WebDAV store may omit it and run push-only.
type ChangeSource interface {
    StartCursor(ctx) (string, error)
    Changes(ctx, cursor string) (changes []RemoteChange, next string, err error)
}

// Optional capability: a complete listing of everything under the mount root
// (M7b). Metadata only, ~one request per page. cursor resumes an interrupted
// sweep; next == "" means complete. Providers that cannot enumerate omit it.
type Enumerator interface {
    Enumerate(ctx, cursor string) (files []RemoteFile, next string, err error)
}

// Optional capability: ranged reads (M5 hydration). Length <= 0 means "to EOF".
type RangeGetter interface {
    GetRange(ctx, path string, off, length int64) (io.ReadCloser, error)
}

// Optional capability: ranged WRITES (M6) — RangeGetter's mirror. Replaces the
// named extents in place and may neither create nor resize, so the engine checks
// the object exists at exactly `size` first. Drive does NOT implement this (see
// §9, M6); callers fall back to whole-file Put.
type RangePutter interface {
    PutRange(ctx, path string, src io.ReaderAt, size int64, extents []ranges.Range) (RemoteFile, error)
}

// Optional capability: the provider's own content digest, computed locally (M6).
// Lets the engine ask "does the remote already hold these exact bytes?" — and skip
// the upload — without knowing which algorithm the provider uses.
type ContentHasher interface {
    HashContent(r io.Reader) (string, error)
}
```

The five optional interfaces are all *accelerators that may decline*. Every one of
them has a correct, slower answer available if the provider omits it or a call
fails, and the engine is written so that "unsure" always selects that answer.
`Enumerator` is the one whose absence costs a *feature* rather than speed — without
it a pre-existing remote tree stays invisible (§9, M7b) — but its absence is still
safe, because nothing infers anything from a sweep that never ran.

Two sentinel errors cross the seam, both because they need a *different response*
rather than a retry: `ErrNotExist` from `Move` (upload the destination as fresh
content) and `ErrCursorExpired` from `Changes` (the changes are gone; re-enumerate
and reconcile — §9, M7b). Providers classify them below the seam, the same
convention `IsRetryable` uses.

`RemoteFile`/`RemoteChange` are keyed by `Path`, not fileID. The Drive implementation
owns the path↔fileID index and is constructed with the **root folder ID** it maps the
mount root to, plus an injected `*http.Client` (see §2.6) so transport is chosen
independently of provider logic.

**Resolving a path is a three-source question** (M7, `internal/provider/gdrive/index.go`).
In cost order: the in-memory maps, then the persistent index (`internal/pathindex`),
then Drive itself via a name query. Only the last is a source of truth. The first is
derived from the API within this session and kept current by the pull loop; the second
is a *hint* written by a process that may have exited months ago, so it is verified
before it is believed and dropped when it no longer matches. The rule the whole design
hangs on: **deleting the index costs latency and quota, never correctness.**

**Move semantics — a real capability difference, documented not abstracted.** Drive's
`Move` preserves object identity (a cheap metadata reparent), so history/permissions
survive a rename. A store with no server-side move (S3, plain HTTP) implements `Move` as
**copy + delete**, which *resets* identity. For a tool that mirrors a path-unique local
tree this is fine — the local FS is the source of truth and paths are unique — but it
means "rename" is not universally atomic or identity-preserving. Providers document their
behaviour; the engine does not branch on it. (`Move` on an unknown source returns
`provider.ErrNotExist`, which the engine recovers by uploading the destination as fresh
content — the one place only the engine has the bytes.)

**Reporting a move *inbound* is the seam's hardest case, and it is the provider's
job.** `RemoteChange` is `{Path, File, Removed}` — no identity — so the only way a
change feed can say an object moved is a removal of the path it left plus an
addition of the path it now occupies. A provider whose native feed is keyed by
object identity (Drive's `changes.list` is: one entry per fileID, carrying that
object's metadata *now*) is never told the old path by the API, and never says it
either unless it works it out from its own path↔ID mapping. Left unsaid, a rename
on one client duplicates the file on every other, because the new path is
downloaded and nothing ever removes the old one — until an enumeration sweep
infers the delete, up to `-sweep-interval` later. `gdrive.vacatedPathLocked` is
where that is worked out, with two deliberate limits: **directories are excluded**
(a removal above the seam is a recursive local delete, and Drive reports no
changes for the children of a moved folder, so the subtree would be deleted and
not come back until a sweep — leaving the stale copy is the lesser failure), and
**only the in-memory mapping is consulted, never the persistent index** (acting on
it here means deleting a local file, and M7's first rule is that a persisted entry
is a hint to be verified before it is believed). Both fall back to the sweep,
which is slower and never wrong.

### 2.6 Transport — HTTP/3 (QUIC) with HTTP/2 fallback
All Drive API traffic goes over **HTTP/3**. Rationale: QUIC's connection reuse and
0-RTT resumption suit our access pattern (a frequent `changes.list` poll loop plus
bursty uploads) — repeated TLS/TCP handshakes are avoided, and head-of-line blocking
across concurrent transfers is eliminated.

Implementation facts that shape the design:
- **Go 1.27's `net/http` has no HTTP/3 client** (HTTP/1.1 + HTTP/2 only). HTTP/3 comes
  from [`github.com/quic-go/quic-go`](https://github.com/quic-go/quic-go) (`http3.Transport`,
  which implements `http.RoundTripper`). Pure Go, no CGO.
- Google's `googleapis.com` endpoints advertise `h3` via Alt-Svc, so the server side
  needs no special handling.

**QUIC is UDP/443, and quic-go does not auto-fall back to TCP.** So a small
`internal/transport` package builds a **composite `RoundTripper`**: it prefers HTTP/3
and transparently falls back to a standard HTTP/2 client when the QUIC dial fails or
times out (UDP blocked, restrictive networks). The fallback decision is cached per
host so we don't re-probe UDP on every request. This is HTTP/3-*preferred*, not
HTTP/3-*only*.

**Composition with OAuth (§ auth, M2).** The transport sits *below* auth. Passing
`option.WithHTTPClient` to the Drive service means we must **not** also pass
`WithTokenSource` — they conflict — so the token source is folded into the client:

```
composite RoundTripper (HTTP/3 → HTTP/2)   // internal/transport
        └─ wrapped by oauth2.Transport{Base: ...}
                └─ &http.Client{Transport: ...}
                        └─ drive.NewService(ctx, option.WithHTTPClient(client))
```

This ordering is why transport is settled before the OAuth work in M2.

Ops note: quic-go wants a larger UDP receive buffer on Linux (`sysctl
net.core.rmem_max`); otherwise it logs a warning. Documented in CLAUDE.md.

### 2.7 Mount frontend (backend seam) & in-place mode
The FUSE code is quarantined behind a small **mount-backend seam**
(`internal/mount`): a `Backend` interface (`Serve(ctx, Options)`) plus the
backend-neutral change-event type in `internal/fsevent`. The go-fuse backend
(`internal/vfs`, Linux/macOS/FreeBSD) is the only implementation today, but the
seam lets others slot in without touching the sync core — cgofuse for Windows, or
an NFS-loopback backend for platforms with no Go FUSE binding (OpenBSD/NetBSD).
Everything below the seam (transport, provider, syncengine, gauth) is pure Go that
cross-compiles anywhere Go runs.

**Backing store — two modes** (`mount.ResolveBacking`):
- **Separate directory** (`-data DIR`): the mount and the backing dir are distinct.
  Portable; the only mode off Linux.
- **In-place** (`-data` omitted): the mount directory *is* its own backing store, so
  when Drivel exits the files simply remain in that directory — no separate copy.

In-place works because a FUSE mount *overlays* its mountpoint: once mounted, access
to the mountpoint **by path** is routed to our handler, shadowing the original
contents. So before mounting we open a **directory fd** to the mountpoint and route
all backing I/O through `/proc/self/fd/N`, which resolves via the fd to the original
underlying directory rather than through the overlay. (Verified: writes through the
preserved fd land in the real directory and survive unmount.)

> **Cardinal rule (load-bearing, like §4):** in-place mode must **never** touch the
> backing store by the mountpoint *path* — only via the preserved fd
> (`/proc/self/fd/N`). A path access to the mountpoint re-enters our own FUSE handler
> → recursion/deadlock. This is why the sync engine is handed `backing.Path`
> (the `/proc/self/fd/N` path), not the mountpoint.

Platform note: `/proc/self/fd` is the Linux shortcut that lets the path-based
go-fuse loopback work unchanged. macOS/FreeBSD have no procfs, so in-place there
would need a fd-relative loopback using the `*at` syscall family (`openat`,
`renameat`, …) — future work; in-place is Linux-only for now.

### 2.8 Configuration, the provider registry, and N mounts per process (M8)

Everything above is per-instance by construction: `gdrive.Open`, `state.Open`,
`pathindex.Open`, `syncengine.New`, `hydrate.New` and `vfs.NewBackend` all take
their configuration explicitly, and there is no process-global mutable state
anywhere in the tree. Serving several mounts from one process is therefore a
wiring question, not an internals one — which is what M8 is.

**`internal/app` owns a mount's lifecycle**, in three phases, and the split is the
whole point. `Open` acquires (backing dirfd, provider, state DB, hydrator, engine)
and can fail; `Run` serves and then drains; `Close` releases. A function that owned
the process's signal context and its defers — which is what `runMount` was — cannot
do this, because bringing up three of five mounts and then failing has to unmount
and drain the three, not exit holding their dirfds and bbolt locks. The per-mount
shutdown ordering inside `Run` is unchanged and still load-bearing: unmount (which
flushes pending FUSE events), then `close(events)`, then wait for the engine's
bounded drain, with the engine on a detached context (§7). `App` aggregates; it
must never replace that sequence with one cancellation.

**The registry is an explicit value, not a package-level map filled by `init()`.**
The `database/sql` shape would introduce the only process-global mutable state in
the tree — in the very milestone whose point is that everything is per-instance —
and would hide the dependency behind an import for side effect. It also would not
allow the thing M8 exists to prove: a test registering `gdrive` under two names and
running both, so the seam is exercised by two independently-configured Drive stores
without a pseudo-provider ever reaching a user's binary.

A provider's own configuration crosses the registry **undecoded**, as a
`Params.Decode` callback filling a provider-defined struct. `internal/config` never
learns what a Drive folder ID is, and adding a provider touches neither package.
`Params` is a struct rather than bare arguments so that giving providers something
new — the per-mount logger it already carries, a metrics sink later — does not churn
every `Factory` signature.

**The config file is TOML, and the format choice is about comments.** Everything
drivel does that is worth configuring has a reason that belongs next to it, and
JSON cannot hold one. That in turn dictates how `drivel login -account NAME` writes
to it: **append, never re-serialize**, because a round trip through any encoder
silently discards every comment in the file. An account that already exists is
printed for the user to reconcile by hand rather than replaced.

The schema splits along a real seam. An **account** is credentials — a provider kind
plus that provider's settings, in a directory of its own under
`$XDG_CONFIG_HOME/drivel/NAME` at 0700. A **mount** references an account and says
what to mount from it; its `[mount.provider]` table is laid over the account's
settings, so one account can be mounted twice with different roots. Relative paths
resolve against the config file's directory, which makes a config directory
self-contained enough to move or check into dotfiles. Inside a *provider* table the
rule has to be syntactic, since this layer cannot know which of a provider's keys
name files: a value is expanded only when written like a path (`~/`, `./`, `../`),
so a Drive folder ID is never rewritten. `login` writes absolute paths, so the
common case does not depend on it.

Unknown keys are an error. `lazzy = true` doing nothing is the same failure as a
flag that silently stopped being read, and the free-form regions are exempt because
unknown keys are precisely their purpose. Fields whose zero value is meaningful —
`max-deletes`, `sweep-interval` — are pointers, so an explicit `0` stays
distinguishable from unset; conflating them would silently uncap M7b's delete guard.

**Flags remain, and synthesize a one-entry config.** Both paths end in the same
`[]app.MountSpec`, so validation, opening and shutdown have one implementation
rather than a single-mount one that drifts from the N-mount one. `-config` and a
flag that describes *what* to mount cannot be combined: the alternative is a
precedence rule nobody would remember.

**The guards are the interesting part**, because several mounts can corrupt each
other in ways one never could, and every one of them otherwise surfaces as a
deadlock, an opaque five-second bbolt timeout, or a file quietly synced to the wrong
account. `app.Validate` runs before anything is opened, on the flag path too:

- **Two mounts sharing a state DB.** Each engine would read the other's echo
  records as its own — and §4 echoes are also M7b's *delete* baseline, so this can
  infer deletions across accounts.
- **Either direction of overlap between one mount's backing tree and another's
  mountpoint.** §2.7's cardinal rule generalised: reading a backing store through
  another mount's FUSE handler recurses into our own filesystem, and the mirror case
  leaks — everything written to one mount appears in the other's backing tree and is
  pushed to *its* remote.
- **A state DB inside a backing tree.** This one needed no second mount to be wrong:
  the DB syncs itself to the cloud, and its own writes generate the events that
  cause more writes. It was previously documented in a flag's help string and
  enforced nowhere.
- Shared mountpoints, duplicate names, an account or provider kind nothing answers
  to, and a backing dir written out to equal its own mountpoint (which is in-place
  mode, spelled the way that recurses).

**Logging is per mount.** With two mounts the default logger is unreadable —
`[sync] create a.txt` does not say whose. Each mount gets a `*log.Logger` threaded
through the existing config structs. A single mount keeps the bare default, so its
output is byte for byte what drivel printed before several were possible.

Deliberately absent: a second *real* provider (M8 is framework only, and the
pseudo-provider proof is a test), hot-reloading the config, and any daemon or IPC
control surface.

---
### 2.9 Platform support

Everything below the mount seam is portable by construction, and the seam is where
portability actually stops. The whole tree cross-compiles for `darwin/{amd64,arm64}`,
`freebsd/{386,amd64,arm,arm64}` and every `linux/*` arch; on `windows/amd64` every
package builds *except* `internal/vfs` (and `app`/`config`/`cmd` only because they
import it transitively). Every cross-compile failure on every target traces to
go-fuse and nothing else. That is a stronger statement of §2.5/§2.7 than M8's
two-Drive-stores test: adding a platform is a `mount.Backend`, never a port.

**Tier 1 — builds today.** Linux (all 13 arches), macOS (amd64/arm64), FreeBSD.
Linux and FreeBSD are *tested*; macOS is compile-verified only, and is documented
as such rather than as supported. M10 gave macOS a real xattr implementation
(§2.9.1), which changes what the platform *can* do and not what has been observed
on it: still compile-verified, still not supported.

FreeBSD moved on 2026-09-06, when the whole suite ran on FreeBSD 15.1 under `-race`
with `DRIVEL_REQUIRE_TESTENV=all` — the mount half included, since fusefs is in
base. It passes, and it did not pass on the first attempt: see §2.1 on why an
`O_WRONLY` open needs a readable backing fd. What that run does *not* establish is
the long-running behaviour a soak would (§9/M0), so FreeBSD is "tested" in the sense
Linux was before the multi-client campaign, not in the sense it is after it.

**Tier 2 — one `mount.Backend` away.** Windows, OpenBSD, NetBSD, DragonFly, Solaris,
illumos, AIX, Plan 9. The sync core alone (`syncengine`, `provider`, `gdrive`,
`state`, `pathindex`, `transport`, `hydrate`, `gauth`, `ranges`, `fsevent`) builds
clean on OpenBSD and Solaris, so a headless no-mount daemon is a far smaller lift
than any mount frontend. On `wasip1`/`js` and Plan 9 the core stops at bbolt, which
needs mmap and flock.

**Tier 3 — no Go port exists.** OS/2, QNX, Haiku. Porting Go to a new OS is a
*runtime* port (per-OS assembly for threads, signals, memory; a linker story), not a
recompile. gccgo is the usual escape hatch and is not one here: GCC's Go frontend is
years behind, and `go.mod` requires a toolchain far newer than it provides.

**Embedded** splits into two unrelated questions. Embedded *Linux* (OpenWrt, Yocto,
Buildroot on arm/mips/riscv) is Tier 1 already; the binding constraint is size —
19–22 MB stripped, mostly `google.golang.org/api` + gRPC + OpenTelemetry — not
portability. Bare-metal/RTOS is out of reach and not for a fixable reason: TinyGo
cannot build bbolt (mmap/flock), quic-go (`crypto/tls`) or the reflection-heavy Drive
client, and more fundamentally there is no kernel VFS to interpose on. The premise of
the program is intercepting a mount point.

#### 2.9.1 What degrades off Linux

Four things, and the second is a correctness matter rather than a missing feature
— and since M10 it is no longer strictly an off-Linux one, because what decides it
is the backing filesystem rather than the kernel. The fourth arrived with M15 and
is the cheapest kind of platform fact to have: one the tests assert rather than one
a user discovers.

1. **In-place mode is unavailable** (`mount.openInPlace` refuses off Linux).
   `/proc/self/fd/N` is the Linux shortcut that lets the path-based loopback work
   unchanged; macOS and FreeBSD have no procfs and would need an fd-relative loopback
   built on the `*at` family. `-data` is mandatory there.

2. **M5's placeholder marker now has authority on every platform that can run
   drivel — and none on any backing filesystem that cannot hold it, which is the
   distinction that actually matters.** M10 implemented both halves:
   `hydrate/xattr_unix.go` covers Linux and macOS with one body (x/sys/unix
   normalises the two system-call shapes — macOS has an extra `position` argument,
   meaningful only for resource forks, and an options word where Linux has flags),
   and `hydrate/xattr_freebsd.go` implements `extattr_*` separately because that
   interface takes the namespace as an argument rather than as a name prefix.
   `xattr_other.go`'s `ENOTSUP` stubs now compile only where `internal/vfs` does
   not, so they are unreachable in a program that mounts anything.

   **What replaces the old platform caveat is a filesystem one, and it is worse than
   the caveat it replaces.** Where the backing store cannot hold the attribute —
   drvfs under WSL2, exFAT or FAT, a tmpfs `/tmp` on FreeBSD — `setxattr` fails,
   `CreatePlaceholder` continues without a marker, and there is then **no placeholder
   record at all**: `Hydrator.IsPlaceholder` reads the marker and nothing else, so an
   un-fetched placeholder is an ordinary empty file to every guard in the tree and
   the uploader will push its zeros over the remote copy. The state DB does not stand
   in for it. Its hydration entries cache present *ranges* (M5's `Hydrator.Ranges`,
   which nothing outside tests reads today); no code path has ever consulted them to
   answer "is this a placeholder". Earlier drafts of this section said losing
   `drivel-state.db` was what triggered the data loss — that was never true, and the
   correct advice is not "keep the DB" but **do not run `-lazy` on a backing store
   without user extended attributes**. `app.Mount` says so at startup. Eager mode is
   unaffected throughout.

   The macOS-specific trap is that a successful `setxattr` proves less there than on
   Linux. On volumes with no native extended attributes — exFAT, FAT, some SMB and
   NFS mounts — macOS emulates them in an AppleDouble `._name` sidecar, so the write
   succeeds and the value reads back, while the marker is now a *file inside the
   backing tree*: drivel syncs it like any other file, another client materialises
   it as literal garbage, and anything that separates it from its parent leaves the
   unmarked placeholder above. `xattrNative` therefore looks for the sidecar after
   the probe write and reports emulation as "no xattrs", which earns the mount the
   same warning — and the same advice — as a filesystem that has none. Keep a macOS
   backing directory on APFS or HFS+.

   The FreeBSD-specific traps are two. The marker is spelled `drivel.placeholder` in
   `EXTATTR_NAMESPACE_USER` rather than `user.drivel.placeholder`, because there the
   namespace is an argument — same attribute, same namespace, different spelling,
   which is why `XattrName` is now assembled per platform and why nothing may
   hardcode the Linux form. And **tmpfs has no extended attributes**, so a FreeBSD
   `/tmp` that is tmpfs (a common install choice) is one of the unsafe backing
   stores above; UFS and ZFS both carry them natively.

   **FreeBSD's has now been run on its own platform; macOS's has not.** On
   2026-09-06 `internal/hydrate` passed on FreeBSD 15.1 with nothing skipped, and
   base-system tools confirmed independently of drivel's own code what the marker
   is: `lsextattr user` reports exactly `drivel.placeholder`, `lsextattr system`
   reports nothing, the JSON round-trips byte for byte, and the placeholder is
   genuinely sparse. Neither thing this section warned about appeared — no short
   write from `extattr_set_file`, and no sign under `-race` of the
   `//go:uintptrescapes` wrappers failing to hold. The macOS half remains written,
   compile-verified for every arch, and lint-clean under its own `GOOS`; §9's M10
   says what a live run has to establish before it is called supported.

3. **The default filesystem is case-insensitive on macOS**, as APFS and HFS+ both
   are unless a volume was deliberately created case-sensitive. Drive is
   case-sensitive, so `Foo.txt` and `foo.txt` are two remote objects that collide
   into one local path — the same fresh variant of §2.5 / MC-30's same-name-sibling
   problem that §2.9.4 names as a reason not to build a Windows frontend, arriving
   on a platform that is otherwise Tier 1. Nothing handles it today, and a live
   macOS run should establish what actually happens before anything is promised.

4. **FreeBSD cannot create a special file through the mount, and its compulsory
   mount options are a shorter list** (M15 items 1 and 3). Both were found by
   running `internal/vfs` on FreeBSD 15.1 on 2026-09-08, and neither was predicted.

   `mkfifo` on the mountpoint returns `EINVAL`. The MKNOD does reach drivel — a
   `-debug` trace shows `MKNOD n1 {010644 (022), 4294967295}` — so the override
   runs and delegates, and the loss happens below: fusefs sends `rdev = ~0`, and
   FreeBSD's `mknod(2)` accepts `S_IFIFO` only when `dev == 0`, so go-fuse's
   loopback, which passes rdev through verbatim, cannot spell a fifo on this
   platform. Independently, FreeBSD's `mknod(2)` refuses `S_IFREG` outright — on an
   ordinary ZFS directory as much as through a mount — so the "a regular file made
   by mknod must still sync" case does not exist there either. Neither risks data:
   the caller gets `EINVAL` and no file, and none of these would have synced. The
   tests assert both rather than skipping, so a change in either is a failure and
   not a surprise. Symlinks are unaffected and work normally.

   And FreeBSD requests only `nosuid`. `MNT_NODEV` was removed from the kernel —
   only devfs may hold device nodes, so the guarantee holds by construction — while
   `mount_fusefs` parses `-o` against a fixed table and exits non-zero on an
   unknown option, reporting it with the `no` prefix stripped: `mount_fusefs: -o
   dev: option not supported`. Asking for a flag this platform does not have would
   have cost every FreeBSD mount, to tighten nothing.

#### 2.9.2 macOS: AGPLv3 and macFUSE

There is **no license incompatibility**, and the reason is structural rather than a
judgement call. go-fuse contains no cgo and does not link libfuse — it implements the
FUSE kernel protocol in pure Go. Its entire interaction with macFUSE is to `exec` the
`mount_macfuse` helper and then read and write a file descriptor received over a unix
socketpair (`fuse/mount_darwin.go`). Two programs communicating at arms length over a
pipe are separate works under long-standing GPL doctrine, not a combined one, so no
copyleft obligation propagates in either direction. AGPLv3's §13 network clause is
about *our* users interacting with *our* program remotely and is not implicated at
all; §1's System Library carve-out would cover an OS-level component regardless,
though nothing here needs to rely on it.

The obligation that does exist is a distribution rule, and it is easy to honour:
**never bundle macFUSE, never ship an installer that fetches it, never publish a
combined image.** Distributing our AGPL binaries is unaffected by what the user
separately chooses to install, exactly as GPL software running on a proprietary OS is.

What is *not* a license conflict but is a real adoption problem: macFUSE 4.x is no
longer open source (osxfuse 3.x was BSD-2-clause), and its own terms restrict
commercial use. That burden falls on the user, not on us — but it is a reason to
document the dependency prominently and a reason FUSE-T matters. Confirm macFUSE's
current terms before making any claim about them; none of the above is legal advice.

#### 2.9.3 macOS: FUSE-T

Two corrections to the obvious framing.

**FUSE-T is not "the user-only install".** Both need admin rights. The real
difference is that macFUSE installs a *kernel extension* — which on Apple Silicon
means reduced-security boot, an explicit approval and a reboot, and which MDM-managed
fleets frequently forbid outright — while FUSE-T runs a userspace NFS server and
needs none of that. FUSE-T is the option for machines where a kext is not permitted,
which is a policy distinction, not a privilege one.

**It is not a build-time choice, because go-fuse cannot use FUSE-T at all.** Verified
in both v2.10.1 and v2.11.0: `fusermountBinary()` probes exactly two paths
(`mount_macfuse`, `mount_osxfuse`) and fails otherwise, and go-fuse speaks the raw
protocol over the macFUSE device rather than linking libfuse — which is precisely
FUSE-T's integration point. Supporting it is upstream work in go-fuse or a separate
`mount.Backend`; no `-tags` combination reaches it today.

When it is reachable, the choice must be **runtime detection, never build tags**:
which helper exists is a property of the machine the binary runs on, not the one it
was built on, and a single darwin binary should probe and say clearly which it found.

On performance, macFUSE should win and we have not measured it. The architecture
predicts it — an in-kernel VFS against a loopback NFS server that adds protocol
translation and a network-stack traversal, with metadata-heavy workloads suffering
most — but that is a prediction, and a benchmark belongs in the M10 work rather than
in this paragraph. One unknown outranks it anyway: FUSE-T is NFS-backed, and whether
it carries `user.*` xattrs at all is unverified. If it does not, the macOS half of
M10 buys nothing under FUSE-T and `-lazy` stays DB-only there. Settle that before
promising either.

#### 2.9.4 Windows: an explicit non-goal

Not "unsupported pending effort" — decided against, for four reasons that are worth
writing down so the question stops recurring.

- **WSL2 already covers the audience.** drivel's users are people who want a POSIX
  filesystem interface; on Windows they have one.
- **Google Drive for Desktop covers everyone else**, natively and free. There is no
  user left who is served better by a Windows port of this.
- **The cost is a new dependency class.** WinFsp/cgofuse means cgo, which forfeits
  the pure-Go build the whole tree currently enjoys. ProjFS is architecturally the
  better fit — it is Microsoft's API for exactly this placeholder/hydrate-on-first-IO
  model and would map onto M5 nearly 1:1 — but it is a ground-up backend, not a port.
- **NTFS case-insensitivity is a new correctness surface, not just labour.** Drive is
  case-sensitive, so `Foo.txt` and `foo.txt` are distinct remote objects that collide
  into one local path. That is a fresh variant of the same-name-sibling problem of
  §2.5 / MC-30, on a layer that has no policy for it.

The seam costs nothing to keep clean — the cross-compile above proves it stays clean
for free — so this is a decision not to build a frontend, not a decision to let the
option close.

**WSL2 caveat, and it is the M5 one again:** files under `/mnt/c` are drvfs, which
does not carry Linux user xattrs. A `-lazy` mount whose `-data` lives there gets no
placeholder marker at all — which since M10 is a *filesystem* failure rather than a
platform one, and the more dangerous of the two because the kernel is Linux and
everything looks supported. Keep the backing directory on the ext4 filesystem inside
the WSL2 VHD.

---


## 3. The sync loop (inbound / pull)

Google Drive gives us **`changes.list`**, a cursor-based incremental feed — simpler
and lighter than the Workspace Events API, and no webhook endpoint required.

1. On first run: `changes.getStartPageToken` → persist as `cursor`.
2. Poll loop: `changes.list(pageToken=cursor)` →
   - list of changed files (adds/updates/removes, each with fileID + metadata),
   - a `newStartPageToken` to persist for next round.
3. For each remote change, run it through **echo suppression** (§4). If it's ours,
   drop it. Otherwise apply to the underlying dir (download/rename/delete) and update
   state.
4. **Cursor expiry**: Drive answers a token it can no longer serve with 410. That is
   not a transient failure — the changes it covered are gone — so the provider
   reports `provider.ErrCursorExpired` and the loop responds by resyncing: a fresh
   start token, then a full enumeration and reconcile (§9, M7b). Retrying instead is
   what the pre-M7b loop did, and it left inbound sync silently dead forever.
5. **Adaptive cadence**: poll fast (~2–5s) while there's recent local or remote
   activity; back off (up to ~30–60s) when idle. Gives event-driven feel without
   webhooks. (Optional future: `changes.watch` push as a latency optimization, but it
   needs a public HTTPS endpoint + channel renewal — impractical for a laptop mount.)

**Downloads are applied atomically — no read-through streaming here (deliberate).**
Each downloaded file is written to a hidden temp in the destination dir and
`rename`d into place only when complete, so a reader on the mount always sees either
the old complete version or the new complete version — never a partial file, and a
mid-download failure leaves nothing half-written. We *considered* serving a file to
the mount while it downloads (populate the real path progressively, teeing bytes to
readers). For the **proactive pull** case this is a net loss, not a win: the reader
never blocks today (it reads the old inode at local speed until the atomic flip),
whereas streaming would force reads to either block until the stream reaches their
offset — worse, since FUSE readahead issues parallel reads ahead of the cursor — or
expose inconsistent half-old/half-new content and forfeit the crash-safe guarantee.
It also breaks the provider-agnostic FS layer (§2.7, CLAUDE.md), since the read path
would have to call the provider on a cache miss. Freshness lag (how soon a remote
edit becomes visible) is bounded by download time and is addressed by download
*scheduling* (start-on-event, prioritization, parallel pulls), not by read-through.

Read-through streaming pays off only for **lazy hydration** (download-on-open with
placeholder/sparse files) — a different feature with its own machinery (a range
cache: per-file present-ranges bitmap + ranged `GET`s for correct random access).
That is deliberately out of scope for v1; see §8.

---

## 4. Echo / loop suppression  ← the critical correctness concern

**Problem:** When the Uploader pushes a local write to Drive, the *next* `changes.list`
poll reports that same change back. Naively applying it re-downloads the file we just
uploaded — wasted work at best, an oscillating loop at worst. The symmetric hazard: a
Downloader write into the underlying dir can trigger a local change event that the
Uploader then pushes back up.

**Approach — attribute every mutation to its origin, then ignore self-origin echoes:**

1. **Content identity, not event identity.** Track each file's `(fileID, driveVersion/
   md5Checksum, localMtime/md5)` in the state store. When a remote change arrives,
   compare the incoming Drive version/checksum against what we last recorded:
   - matches what we last *uploaded* → it's our echo → **drop**.
   - differs → genuine remote edit → apply.
2. **Pending-op guard.** Before an upload, write a `pending` record keyed by fileID
   with the expected resulting checksum. The Downloader consults `pending` and skips
   matching changes, clearing the record once matched (or after a TTL).
3. **Suppress Downloader-originated local events.** When the Downloader writes into
   the underlying dir, it marks that path as "remote-applied" (path + expected mtime)
   so the FUSE-layer change event for that write is recognized and *not* re-uploaded.
4. **Loop breaker.** Any change whose resulting content hash equals the currently
   recorded hash for that fileID is a no-op — dropped regardless of origin. This makes
   the whole system convergent: identical content never generates further work.

This checksum/version-reconciliation approach (rather than trying to perfectly track
event provenance) is what real sync engines converge on, because FS and cloud events
are both lossy and racy.

---

## 5. Outbound (push) path

1. FUSE op succeeds against underlying dir → enqueue `LocalChange{op, path}`.
2. Uploader coalesces rapid events per path (debounce writes; a burst of `Write`s +
   `Release` becomes one upload) and serializes per-path to avoid reordering.
3. Resolve parent Drive folder (create dirs on demand, memoized), write `pending`
   record, call `Upload`/`Update`/`Move`/`Delete`, then record new version in state.
4. Retries with backoff on transient Drive errors; failures re-queued and surfaced in
   status/logs.

---

## 6. Conflict policy (v1, deliberately simple)
- Reconcile by comparing recorded vs incoming version/mtime.
- If both sides changed since last sync (divergent versions): **last-writer-wins by
  modifiedTime**, and the losing side is preserved as a conflict copy
  (`name (conflict 2026-07-17 …).ext`). Log it. No interactive resolution in v1.

---

## 7. Concurrency model
- One goroutine for the FUSE mount (go-fuse manages its own pool).
- Uploader: a bounded worker pool consuming a channel of `LocalChange`, keyed/
  serialized per path.
- Downloader: single goroutine running the poll loop; applies changes sequentially
  (parallelizable later).
- State store guarded by bbolt transactions; the `pending`/echo maps are the only
  shared mutable state between up/down paths — accessed only through the store.
- `context.Context` threaded everywhere for clean shutdown; on unmount, drain the
  uploader queue (bounded) before exit.

---

## 8. Open questions / future
- **Deduplication**: no longer shelved as an idea, but as a *provider* rather than
  as a layer inside the mount — see M11 in §9. GPU-accelerated hashing stays
  shelved (the repo name is historical); it is an optimisation of a component that
  does not exist yet.
- **Virtual xattrs**: emulate extended attributes out of a file in the backing
  store, so they work on backing filesystems that have none and travel with the
  content. Unscheduled, and gated behind a build tag if it is ever built — the
  design note and the reasons for the compiler-level default are §11.
- **`changes.watch` push** as a latency optimization behind an optional relay.
- **POSIX metadata preservation** (mode bits, POSIX/extended ACLs, xattrs, SELinux
  contexts) carried alongside content — see §10. Not scheduled; the security
  analysis is the blocker, not the plumbing. Note that §11 would carry xattrs over
  the same provider by a different route, which is one mechanism too many if both
  are ever built.
- **Backends that are not a cloud**: a deduplicating local store (M11), an
  encrypting decorator over any other provider (M12), and a block-level filesystem
  over a distributed database (M13). The first two are `provider.Store`
  implementations that need no new seam; the third is only partly one, and §9 says
  where it stops fitting.
- **A control & status API** over a unix socket, so third-party tools can read
  per-path sync status and per-backend statistics, pause and resume a mount, see
  whether a backend is reachable, and stop the process — M14 in §9. The design work
  is in what it may promise: it is the first surface other people's software binds
  to, it must stay a view and never an authority, and the pause has to hold the
  *dispatch* rather than the event channel, which would block the mount.
- **Google-native docs** (Docs/Sheets/Slides) have no binary content and no
  `md5Checksum`. They fall back to the opaque `Version` for echo matching, and since
  M7b they are marked `RemoteFile.ExportOnly`: reported by the sweep (so their
  absence locally is never read as a deletion) but never materialised and never
  given an echo. Export-on-read (`files.export`) is the unexplored half — it would
  need a policy for which format a `.gdoc` becomes locally, and a story for writing
  one back.

---

## 9. Milestones

**Shipped (v1).**

1. **M1 — Passthrough mount.** go-fuse loopback proxying to underlying dir. No cloud. ✅
2. **M2 — Transport + Drive auth + one-shot push.** `internal/transport` HTTP/3→HTTP/2
   client (§2.6), OAuth folded on top, upload a file on close. ✅
3. **M3 — Pull loop.** `changes.list` cursor loop (`internal/syncengine.Downloader`) →
   underlying dir, with §4 echo suppression and adaptive cadence (§3.4). Engine-level
   state (cursor + echo records) persisted in `internal/state` (bbolt). ✅ The
   provider-internal path↔ID index stays in-memory (self-rebuilding, §2.5); its bbolt
   persistence lands in M7.
4. **M4 — Full bidirectional** with debounce, retries, conflict copies (§6), clean
   shutdown (drain the uploader queue; the downloader stops on ctx cancel). ✅
   Outbound: events are coalesced per path behind a debounce window and dispatched
   to a bounded pool of path-hashed workers, each op retried with exponential
   backoff on transient provider errors (`provider.IsRetryable`, classified below
   the seam). Inbound: a remote edit that collides with a divergent local edit
   triggers the §6 last-writer-wins policy with a local-only conflict copy. On
   unmount the engine runs on a background context so `close(events)` (post-unmount)
   drives a bounded drain of pending + in-flight uploads before exit.

**In progress / planned (v2).**

5. **M5 — Lazy hydration.** ✅ Cache-on-demand: the backing dir no longer has to
   hold full content. Opt-in via `drivel mount -lazy` (requires `-credentials`);
   the default stays fully-resident, so M1–M4 behaviour is unchanged.

   A remote file the pull loop learns about materialises as a **placeholder** —
   correct name, size and mtime, zero bytes resident (a sparse `ftruncate`) — and
   its content is faulted in on first use. `internal/hydrate` owns the model;
   `provider.RangeGetter` (implemented by `gdrive` via an HTTP `Range` header) and
   the per-file present-ranges bitmap are defined in full, though M5 itself only
   ever stores the all-or-nothing cases. That is deliberate: M5b (per-block
   faulting) and M6 (dirty ranges) inherit the schema rather than migrating it.

   Three decisions carry the correctness:

   - **The marker is an xattr, not a database row.** `user.drivel.placeholder` on
     the backing file is authoritative; the state-store bitmap is a cache. The
     failure this prevents is data loss, not slowness: a placeholder mistaken for
     a genuinely empty file gets uploaded as zero bytes over the remote content it
     was standing in for. The xattr survives losing `drivel-state.db`. Where the
     backing filesystem has no user xattrs, the mount warns and falls back to the
     state store alone.
   - **Push-path suppression is load-bearing** (`syncengine.Placeholders`). The
     uploader consults the marker before every content push and skips placeholders
     outright. It fails *safe*: an unreadable marker reports "placeholder" and
     suppresses the upload, because a spurious skip costs one deferred sync and a
     spurious upload costs the user their file. Note the guard cannot be a size
     heuristic — a legitimate truncation to zero is indistinguishable from a
     placeholder by size, and must still sync.
   - **Hydration happens on first I/O, not on open.** Deferring that far makes
     open/truncate/rewrite free (nothing is fetched for content about to be
     discarded) and keeps directory walks and file probes from downloading a tree.
     It also sidesteps a FUSE detail: the kernel delivers `O_TRUNC` as a separate
     `Setattr` unless `atomic_o_trunc` is negotiated, so an Open-time check alone
     would both miss truncations and hydrate needlessly. `Open` still drops the
     mark when it *does* see `O_TRUNC`, or the loopback open would zero a file
     that a later read would then "restore".

   Two consequences worth remembering. A failed hydration returns `EIO` rather
   than a short read — a placeholder reads as zeros, and serving those as content
   is silent corruption. And **conflict copies (§6) are always downloaded in
   full**, even in lazy mode: a conflict copy's path exists only locally, so a
   placeholder there could never be redeemed.

6. **M6 — Partial-file / range writes.** ✅ Shipped. Always on; there is no flag,
   because every path it adds either provably saves work or declines to act.

   The premise was that editing one byte of a 4 GB file costs a 4 GB upload. It
   still does *on Drive*, and that is the first thing to record honestly:

   > **Drive cannot patch byte ranges.** `files.update` replaces an object's
   > content wholesale, and the resumable upload protocol chunks the transfer but
   > every chunk still belongs to one complete new body — there is no way to say
   > "keep bytes 0..N, replace only these". No amount of precision above the seam
   > changes that. `gdrive` therefore does **not** implement `RangePutter`, and
   > the omission is the design, not a gap to fill in later.

   So M6 is three gates in `Engine.pushContent`, each able to end the push, and
   all of them falling through to the M1–M5 whole-file `Put`:

   1. **Placeholder** (M5, unchanged) — skip; nothing local to send. Nothing may
      get in front of this one.
   2. **Range write** — if the provider implements `provider.RangePutter` and the
      mount reported exactly which extents changed, send those. Drive declines
      here; the path is exercised by providers that can patch, and it is what M8's
      second registered provider and any future WebDAV/S3-multipart backend plug
      into.
   3. **Unchanged content** — hash the local bytes and compare against what `Stat`
      says the remote currently holds. Equal means there is no request to make.
      This is the gate that actually helps a Drive user: a touch, an editor
      rewriting an identical buffer, a rebuild producing the same artifact — each
      costs a local read instead of a transfer. The digest is the provider's
      (`provider.ContentHasher`, Drive's md5), so the engine never bakes in an
      algorithm.

   Two orderings in that list are deliberate and both are counter-intuitive.

   **The range write runs before the hash check**, even though hashing is
   "cheaper" in request count, because hashing means reading the *entire* file. On
   the exact case M6 exists for — one block changed in a multi-gigabyte file —
   checking "did anything really change?" first would spend a 4 GB read to avoid a
   4 MiB upload. If the content turns out not to have changed after all, the range
   write rewrites identical bytes: wasteful, not wrong.

   **The hash compares against the remote, not against the echo record.** The echo
   is a claim about the past. If the remote diverged in a way the change feed never
   delivered — a cursor expired across a long downtime, a state DB reused against a
   different `-drive-root`, a delete we never learned about — a stale echo would go
   on matching our unchanged local file forever, and the push would be skipped
   every time. The file would silently never be restored, where pre-M6 it
   self-healed on the next write. Asking the remote what it currently holds cannot
   go stale.

   Both gates need the remote's state, so they share one `Stat`, and both are
   skipped outright below `hashSkipMinSize` (one block): for a small file the
   round-trip costs about what the upload would, so the gate would spend a request
   to save a request.

   One further check is what makes partial writes safe at all: **a range write may
   only be applied to the exact version it was based on.** Before patching, the
   engine requires the remote to still match the §4 echo record. A whole-file `Put`
   over a remote someone else edited loses their edit — that is the documented §6
   last-writer-wins policy, and the loser's bytes at least existed as one coherent
   version. Splicing our extents into their file produces a hybrid that existed
   nowhere, with no conflict copy and no intact version of anyone's work. So a
   divergent remote, a missing echo, or no state store at all all decline to the
   whole-file path and its normal conflict semantics.

   Above the seam, the dirty-range map rides on `fsevent.Event.Dirty` rather than
   living in a store of its own. Extents describe one pending push and nothing
   more; if the process dies before the push, the event that would have carried
   them is gone too, so there is nothing left to go stale. `internal/ranges` holds
   the structure, moved out of `internal/hydrate` so the eager path does not
   import the lazy one — the present-ranges bitmap and the dirty-ranges map really
   are the same data structure read two ways, and the whole difference is the
   rounding direction: **present rounds inward** (a partially fetched block is not
   safe to read), **dirty rounds outward** (a partially written block must still
   be shipped). Both errors fall on the side of doing more work rather than losing
   data.

   The correctness rule is M5's, reflected: **`nil` means "extents unknown", and
   unknown means push the whole file.** Everything that cannot account for every
   changed byte says `nil` and is right by construction — a truncate or any other
   size change (which moves every offset after the cut), a `fallocate`, an event
   with no file handle behind it, a rename fallback, a union across mismatched
   block grids, a file that shrank behind an open handle, a size the handle could
   not read. In the coalescer, unknown *absorbs* known. A push that forgets an
   extent corrupts a file; a push that sends too much costs bandwidth.

   One subtlety worth keeping: a handle only sees the writes that came through it,
   so its own idea of the file's length stops at the last byte written. The
   extents are grown to the file's real size (an `fstat` at `Release`, before the
   descriptor closes) — otherwise a mid-file edit yields a set describing a file
   nobody is holding, the engine's size cross-check rejects it, and M6 silently
   never fires.

   Below the seam, Drive's unavoidable large upload is at least made survivable:
   uploads run as chunked resumable sessions with an explicit chunk size, a
   per-chunk retry deadline raised well above the library's 32 s (barely one 429
   backoff), and `EnableAutoChecksum` so a corrupted chunk fails the upload rather
   than becoming the file's new content. The session URI is *not* persisted —
   resuming across a drivel restart was deliberately deferred, since it means
   driving Drive's resumable protocol by hand against a state record. What this
   buys is survival of a flaky network, not of a process death.

   `googleapi.ChunkTransferTimeout` is deliberately left unset. It reads like a
   stall detector but is a hard per-attempt wall-clock deadline that never resets
   on progress, so any value for it silently caps the slowest link that can ever
   finish a chunk — 16 MiB in 2 minutes is a ~1.1 Mbps floor, below which a
   perfectly healthy slow upload fails permanently rather than merely taking a
   while. A genuinely dead peer is already caught underneath: the QUIC transport
   runs keepalives and its own idle timeout (§2.6), with the engine's retry on
   top. It is also a trap to set carelessly, because the timeout must stay well
   under the chunk retry deadline or the retry it exists to trigger can never
   run — the deadline timer starts when the chunk starts and is only checked
   between attempts.
7. **M7 — Path↔ID index persistence.** ✅ Shipped. On by default
   (`drivel mount -index FILE`, `-index ""` to disable), because everything it adds
   either saves work or declines to act.

   The milestone was scoped as "promote the in-memory path↔fileID index to bbolt so
   a restart doesn't re-walk the remote tree — purely a startup-latency
   optimization". Building it turned up that both halves of that sentence were
   wrong, and the correction is the interesting part.

   **Nothing ever walked the tree.** The pull loop starts from a "now" cursor, so
   the index only ever learned a path when an operation touched it. What a cold
   index actually did was worse than slow. An unknown path meant "does not exist
   remotely", so after a restart the first edit to an existing file ran `Files.Create`
   instead of `Files.Update` — and since Drive permits same-name siblings, the user
   got a **second file beside the real one, under a second copy of every parent
   folder**. The same false "absent" from `Stat` disabled M6's unchanged-content
   gate (a full re-upload of a file Drive already held byte for byte), and in `-lazy`
   mode it made a placeholder written by a previous session unredeemable: `Get` on
   an unknown path failed, and a failed hydration is `EIO` (M5). So M7 is a
   correctness milestone that happens to also be faster.

   Resolution now has **three sources**, tried in cost order:

   1. the in-memory maps — everything this process has already learned;
   2. the persistent index — what a previous process learned;
   3. Drive itself — a `files.list` name query, one path component at a time.

   Only (3) is a source of truth, and adding it is what makes the index
   *self-rebuilding* rather than merely *claimed to be*. It is also the fix for the
   duplicate-create bug on its own: an unresolvable path is now genuinely absent.

   **A persisted entry is a hint, and using one unverified is the one way this
   could lose data that the in-memory index never could.** While drivel was down,
   another client may have moved, renamed, replaced or deleted that object. The
   stored ID still resolves — to a different file, in a different place. Handing it
   to `Files.Update` overwrites a file the user never touched, with no conflict copy
   and no event to notice it by. So an entry is checked before first use (still
   exists, not trashed, still carries that name, still under the parent the path
   names) and dropped when it fails, and a failed directory takes its **whole
   subtree** with it: if a folder is not where we left it, nothing recorded beneath
   it is trustworthy either. Verification costs one metadata `GET` per path per
   session, and because it resolves the parent chain through the same path, the
   ancestors are verified once and then free.

   This is the same shape as M5's xattr-over-DB and M6's hash-against-`Stat`: the
   cheap local record is a cache, the remote is the authority, and "unsure" always
   selects the slower correct answer.

   **The index is bound to an identity** — the account's `permissionId` plus the
   concrete root folder ID — and any change to that pair wipes it rather than
   reading one account's paths as another's IDs, which matters as soon as M8 makes
   two accounts routine. `permissionId` rather than the email address, so a file the
   user did not ask to hold an identifiable address does not hold one. Binding needs
   a network round trip and therefore happens lazily at **first index use, not at
   `Open`**: mount must not depend on the network to come up, and nothing on disk is
   read before we know whose it is. Until it succeeds the store reads as empty and
   drops writes — an unidentified index is treated as no index.

   Everything about it degrades to M2–M6 behaviour: a DB that won't open, an
   identity that can't be established, a bbolt error mid-operation — each logs and
   falls back to memory-only. The store lives in `internal/pathindex`, composed
   privately by the provider and kept out of `internal/state`, which stays
   engine-level and provider-agnostic (§2.4). It is a separate DB file for the same
   reason.

   One long-standing bug fell out of testing the parent walk. A file's `parents`
   carry the *concrete* root ID, while `-drive-root` defaults to the alias `root`,
   and the walk compared against the alias — so it climbed past the mount root to My
   Drive, found a folder with no parents, and concluded the object was outside our
   subtree. **Every inbound change to a top-level file had been silently dropped
   since M3.** Resolving the alias to its real ID once, up front, is the fix.

   Deliberately not included here: an initial reconcile that enumerates the remote
   tree. Deciding what to do with what such a sweep finds is a policy question of its
   own, not a cache-warming one — and the sweep is a *one-time* cost rather than a
   per-startup one only because the index and cursor persist, which is what this
   milestone put in place. That is **M7b** below, which shipped next and made the
   sweep double as this index's warm-up.

8. **M7b — Initial enumeration & reconcile.** ✅ Shipped. The other half of M7's
   story, and the piece that makes a large pre-existing Drive usable.

   M3's pull loop starts from a "now" cursor, so a Drive that existed before the
   first mount was invisible: nothing enumerated it, and the backing tree only ever
   learned about objects that changed while we were running. M7 made any path
   *resolvable* on demand. M7b makes the tree *present*.

   **The milestone splits in two, because the two halves differ in cost by orders
   of magnitude.**

   - *Enumeration* — build the path↔ID index and a baseline record of remote state.
     One flat listing, roughly **one request per 1000 objects**, no content
     transferred and no local files created. Cheap enough to be the default, and it
     is: a mount with no cursor yet sweeps before it starts tailing.

     **The unit of that cost is the account, not the mount, and this entry failed
     to say so for three milestones.** `listPage` asks for `trashed = false` across
     the whole of `spaces=drive` and filters to the mount root *afterwards*, by
     parking objects on parents it has not seen yet — so a mount of one folder
     inside a large Drive pays for every object in the Drive. MC-11 measured the
     shape (11 129 objects listed to keep 10 101) and read it as a tidiness problem
     about leftovers from other scenarios; it is not, it is the scaling law. At 10^6
     objects it is ~1000 *strictly sequential* pages — page tokens cannot be
     prefetched — and N mounts on one account each pay it in full, on every first
     run, every dead cursor and every `-sweep-interval`. **M7c** replaces it for
     subfolder mounts.
   - *Materialisation* — create local entries for remote objects that have no local
     counterpart. Under `-lazy` these are placeholders: metadata only, effectively
     free, and the whole Drive becomes visible for the price of the sweep. In eager
     mode the same operation is a full download of everything, so it sits behind
     `-materialize` and is never a silent side effect of mounting.

   **Seam.** A new optional capability, in the established shape (§2.5): providers
   that cannot enumerate omit it and M7b is a no-op for them.

   ```go
   // Optional capability: a complete listing of everything under the mount root.
   type Enumerator interface {
       Enumerate(ctx, cursor string) (files []RemoteFile, next string, err error)
   }
   ```

   `RemoteFile` is path-addressed, so the id→path assembly happens *below* the seam
   — which means the sweep doubles as index warm-up, populating `internal/pathindex`
   as it goes (batched, one bbolt commit per page rather than one per object), and
   the engine never learns what a fileID is. `cursor` resumes an interrupted sweep;
   `next == ""` means complete. For `gdrive` this is one flat `files.list`
   (`q: trashed = false`, `spaces=drive`, `pageSize=1000`, the §2.5 projection plus
   `parents`), with the tree assembled locally: a flat listing has no
   parent-before-child guarantee, so an object whose parent has not been seen yet is
   **parked on that parent's ID** and released the moment the parent arrives
   (recursively, so a strictly child-first listing still costs one pass). Anything
   still parked when the sweep ends never reached our root and is dropped — the same
   rule `pathForIDLocked` applies to the change feed, and what keeps a subfolder
   mount correct while listing the whole account.

   Resolution during a sweep is deliberately **local**: a complete sweep sees every
   non-trashed object, so a parent missing from it is genuinely absent rather than
   merely unseen, and walking parents by ID over the network would turn a cheap
   sweep into a per-object quota disaster. The exception is a *resumed* sweep, whose
   earlier pages this process never saw; there the persistent index stands in for
   them, verified per M7 before it is believed. With no usable index there is
   nothing to stand in, so the sweep **restarts** rather than silently omitting a
   subtree — an omission the reconcile above would read as "deleted remotely".

   One failure is fatal rather than empty: if the concrete root folder ID cannot be
   resolved, `Enumerate` errors out. Reporting an empty tree instead is the single
   most dangerous thing a sweep can do, because then *every* previously-synced path
   looks remotely deleted. (This is the same alias trap M7 fixed in the parent walk:
   a file's `parents` carry the concrete ID, never the `root` alias.)

   **Snapshot, then tail — the ordering is not negotiable.** Take the `changes.list`
   start token *before* the sweep begins and hand it to the pull loop only after the
   sweep completes. The overlap replays some changes, which is harmless (they are
   idempotent, and §4 echo suppression drops them); the other order loses everything
   that changed while the sweep was running. A sweep of a large Drive will be
   interrupted, so the token, the sweep cursor and a generation marker are persisted
   together (`internal/state`, `sweep` bucket) and a restart resumes mid-sweep rather
   than starting over. The downloader owns all of this because it owns the cursor:
   one owner means one place where the ordering can be got wrong.

   **Reconcile needs a baseline, and this is the actual hard part.** For each path
   the decision is three-way — last-known × local-now × remote-now:

   | last-known | local | remote | action |
   |---|---|---|---|
   | — | — | present | materialise locally (new remotely) |
   | — | present | — | push (new locally) |
   | present | — | present | delete remotely (deleted locally while we were off) |
   | present | present | — | delete locally (deleted remotely while we were off) |
   | present | present | differs | §6 last-writer-wins + conflict copy |
   | present | present | same | nothing |

   Without the last-known column, "created remotely" and "deleted locally" are
   *indistinguishable* — both are "present on one side only" — and guessing wrong
   deletes the user's data. **§4's echo records are the baseline**, not a manifest
   alongside them: an echo says "we have synced this content at this path", which is
   exactly what the column means, and a second record would only be a second thing to
   keep in sync. What the echoes lacked was a way to ask "which of these did the
   sweep *not* see", so M7b adds the per-generation `seen` marks and the
   `EachUnseenEcho` join over them. The rule, unchanged: **a delete may be inferred
   only from a baseline, never from absence alone.**

   The remote-present rows needed no new code at all. They are exactly what the pull
   loop's `apply` already does — echo match ⇒ nothing, identical local bytes ⇒
   nothing, divergent local ⇒ §6 conflict copy, absent local ⇒ materialise — with the
   echo serving as the baseline in both. The sweep adds only what it must not
   materialise (below) and the seen mark.

   **Four guards make the delete rows safe**, and each exists because of a specific
   way the inference can be wrong:

   1. **Deletes run only after a sweep completes**, and only from that sweep's own
      generation of marks. "Not seen anywhere" is not knowable per page.
   2. **The baseline must predate the sweep.** A file created locally *while the
      sweep ran* has an echo (the uploader recorded it) and no mark (its page was
      listed before it existed) — it looks exactly like a remote deletion, and
      deleting it would destroy something the user just made.
   3. **A local copy that diverged from its baseline is never deleted.** Those bytes
      are the only remaining version of that work, so it is kept and pushed back
      instead. A directory is removed only if empty, so a subtree can only disappear
      one accounted-for file at a time.
   4. **`-max-deletes` (default 100) caps the whole pass, and exceeding it abandons
      the pass rather than trimming it.** The shapes that produce a huge count — a
      state DB reused against a different `-drive-root`, a fresh empty `-data` dir, a
      mount pointing somewhere new — are ones where the *premise* is broken, not
      where there are genuinely 4000 deletions. This matters more on the remote side
      than the local one: `Store.Remove` on Drive is a permanent delete, not a move
      to the trash.

      The cap is applied *during* the candidate walk, not after it, because the
      input that most needs refusing is also the largest: a state DB whose every
      path is absent from this remote would otherwise be materialised in full
      before anything got to refuse it. Past the cap the walk stops retaining
      candidates and only keeps counting — the count costs nothing inside a scan
      already in progress, and it is what makes the refusal actionable, since
      "4231 deletions" tells an operator something that "more than 100" does not.

      **A refused pass does not retry itself, and the message has to say so.** The
      sweep around it still completed — it enumerated the whole tree and ran every
      other row — so it records its completion stamp and clears its resume record.
      The next mount therefore finds a valid cursor, no interrupted sweep and, until
      `-sweep-interval` falls due, no reason to enumerate again; raising the cap and
      restarting changes nothing observable, which from the operator's side is
      indistinguishable from having fixed it. Recovery is `-resync` *and* a higher
      cap, so the refusal names both.

   The fail-safe direction is the same one M5 and M6 use — when the baseline is
   missing or ambiguous, keep and materialise rather than delete, because deletion is
   the irreversible half. One consequence is worth stating as a rule rather than
   leaving it to fall out of the table: **the first-ever run performs no deletions at
   all.** Every path is baseline-absent, so remote-only materialises, local-only
   pushes, and nothing is removed on either side.

   Worth noting what a sweep that *under*-reports costs, since that is the residual
   risk: in the local direction a re-download (we only delete a local file whose
   content still matches the baseline, so those bytes exist remotely), and in the
   remote direction the deletion the user already performed locally. It is the sweep
   that reports *nothing* which is dangerous, which is why the root-resolution
   failure above is an error rather than an empty result.

   **The local walk resolves an open question the spec left.** Row 2 ("new locally")
   needs one — nothing else knows about a file the mount never saw created, whether
   from an edit made while drivel was down or, in in-place mode, a file dropped into
   the directory between runs. It ships **on, unflagged**: the walk is local I/O and
   its only action is a push, which never destroys anything. Two exclusions are
   load-bearing rather than cosmetic: §6 **conflict copies are skipped**, because
   they are local-only by policy and uploading them would publish the losing side of
   every conflict drivel has ever resolved; and **placeholders are skipped**,
   because a placeholder is remote-born by definition and pushing one is the M5
   catastrophe.

   **Pushes go through the Engine** (`syncengine.Pusher`, satisfied by `*Engine`),
   never straight to the store, so a file a sweep discovers takes exactly the path a
   file written through the mount takes: M5's placeholder guard, M6's gates, echo
   recording, and the retry policy. A second, subtly different "upload this" is how
   the guards get skipped.

   **Cursor expiry wires into the same path.** Before M7b, `Downloader.resumeCursor`
   had no expired-token case: Drive answers a dead page token with 410, the loop
   logged it and retried at the slow cadence indefinitely, and inbound sync was
   silently dead. The fix belongs here because the recovery *is* a resync —
   classified below the seam (`provider.ErrCursorExpired`, following the
   `ErrNotExist`/`IsRetryable` convention, and covering both the 410 and the
   malformed-token 400) and answered by taking a fresh token and re-enumerating.
   That converts a permanently stuck loop into a self-healing one. A provider with no
   `Enumerator` still recovers, by restarting the feed from "now" and saying in the
   log what the gap cost.

   **Operationally:** it runs off the FUSE path in the downloader's goroutine, so the
   mount comes up immediately and stays usable while the sweep proceeds;
   materialisation is applied per page rather than as one transaction, so an
   interrupted sweep leaves a partially populated but consistent tree; and it logs
   pages, objects and elapsed time, because a cost the user cannot see is a cost they
   will assume is a hang. It runs automatically when there is no cursor (first run),
   when a sweep was interrupted, or when the cursor is dead, with `-resync` to force
   it.

   **On a schedule, too (`-sweep-interval`, default 24h).** Those four triggers all
   fire at startup, which leaves a long-lived mount never re-enumerating, and a
   short-lived one re-enumerating only when something has already gone wrong. The
   change feed reports only what happens while we are watching it, so everything
   that happened while drivel was *not* running — a file deleted offline, a push
   that exhausted its retries, a pass `-max-deletes` refused — stays unreconciled
   until the next sweep, and the baselines for those paths stay in the state store
   describing content that exists on neither side. **The sweep is the only pass that
   can establish that, so it is also the only correct place to prune a baseline.**

   That is worth stating as a rule, because the obvious alternative is wrong:
   pruning a baseline and inferring a delete are the *same decision*, made from the
   same evidence. A cheaper prune — walk the echoes, drop the ones whose path is
   absent locally — cannot tell "deleted on both sides already" from "deleted
   locally while we were down, remote still has it", and dropping the second loses a
   pending delete: the next sweep finds a remote file with no baseline and puts it
   back. Age-based (TTL) and recency-based (LRU) eviction fail for the same reason —
   neither says anything about whether the path still exists — with the added
   problem that they evict silently. Anything cheap enough to skip enumeration has
   to guess at that fork; anything that resolves it *is* the delete pass, minus the
   four guards above.

   The schedule is measured from the last **completed** sweep, persisted in the
   state store, rather than from process start: someone who mounts for an hour a day
   would never reach an interval counted from startup, and offline activity is
   precisely their case. A state store with no completion stamp counts as overdue —
   that is a first run, a DB written before the stamp existed, or one whose every
   sweep was interrupted, and in each nothing has ever finished reconciling this
   tree. A periodic sweep goes through the same `beginSweep`, so "snapshot, then
   tail" holds by construction; the cursor it replaces is older than the one in
   hand, which replays changes the feed already delivered — idempotent, and dropped
   by §4.

   **The other open questions, decided.** Google-native Docs/Sheets/Slides have no
   `md5Checksum` and no byte size because they have no byte stream — they are
   exported, not downloaded — so there is no honest apparent size for a placeholder
   and no digest to compare. They are **reported but not materialised**:
   `RemoteFile.ExportOnly` says so, the sweep marks them seen (so their absence
   locally is never read as a deletion) and records **no echo** (recording one would
   claim we hold content we do not), and the pull loop skips them with a log instead
   of failing a download on every report. Shared-with-me files stay out of scope by
   construction: they are not under the My Drive root, so a root-scoped sweep excludes
   them — a decision, not an accident.

   **Sharding the sweep is no longer speculative, and this entry's reason for
   deferring it has expired.** It said that one sequential pagination "has not been
   measured to be too slow" — the right test to set, and the wrong answer to assume
   in the meantime. It has now been measured, on a Drive holding 10^5–10^6 files:
   the sweep costs minutes, per mount, repeated for every folder mounted from the
   same account. The work moves to **M7c**, which is that same "list folders first,
   then fan out" plus the scoping that is most of why it is worth doing. What this
   entry got right is the caution — a sweep that fans out is a sweep that can cost
   more, and M7c keeps the flat listing for the case where it still wins.

   **Not in scope:** dedup, periodic full scans (the cursor feed stays the steady
   state), and any content transfer in lazy mode.
9. **M7c — Scoped enumeration.** ✅ Shipped, and owed a live measurement. M7b made
   a pre-existing Drive visible; M7c makes the bill for that proportional to what
   was actually mounted.

   **The defect M7b shipped with.** `listPage` asks Drive for `trashed = false`
   across the whole of `spaces=drive` and sorts the result into the mount root
   afterwards, by parking each object on a parent it may not have seen yet. So the
   unit of cost is the *account*: a mount of one folder inside a large Drive pays
   one request per 1000 objects in the Drive, in strictly sequential pages (a page
   token cannot be prefetched), and every other mount of that account pays it again
   in full — on every first run, every dead cursor and every `-sweep-interval`. At
   10^6 objects that is ~1000 round trips, measured in minutes. MC-11 saw the shape
   and filed it as untidy test leftovers; it is the scaling law.

   **Drive offers nothing cheaper to ask for.** There is no recursive "everything
   below this folder" query — `'ID' in parents` returns direct children only —
   which is why the flat listing was the reasonable first answer and why the fix is
   a client-side descent rather than a better query.

   **What M7c does.** A breadth-first walk from the mount root: one listing per
   folder, with the frontier fanned out over `enumFanout` (8) concurrent requests
   per `Enumerate` call. A folder holding more children than one page goes back on
   the frontier rather than being drained in place, so no single call is unbounded
   — MC-52 watches exactly that. Against a subfolder mount the unit tests measure
   **1 listing against the flat sweep's 12** for the same fixture; the shape of that
   ratio, not the number, is the claim.

   Two things fall out that are worth more than the speed.

   - **The descent is parent-first by construction**, so none of M7b's parking
     machinery applies: a child is only discovered by listing its parent, so its
     path is known the moment it appears. `waiting`, the parked count and the
     "still parked ⇒ outside the mount" rule are the flat path's alone. This
     removes the subtlest code in M7b from the common case rather than adding to it.
   - **Resume stops mattering.** M7b persists a sweep cursor because losing a
     full-account sweep is expensive, and needs the M7 index to stand in for pages a
     previous process consumed. A descent of the folder actually mounted is short
     enough to restart, which is what a cursor arriving with no frontier behind it
     does. One less thing that can be half-right after a crash.

   **It is not a free win, and the mode is therefore selectable.** A descent costs
   about one request per *folder*; the flat sweep costs one per 1000 *account
   objects*. So a subtree with more folders than the account has thousands of
   objects is cheaper to sweep flat — a deep tree of near-empty directories inside a
   small account is the losing shape. `sweep-mode` (`-drive-sweep-mode`) takes
   `auto`, `flat` or `scoped`; **auto descends whenever `-drive-root` names a
   concrete folder and lists the account when it names the whole Drive**, which is
   the one case where the descent has no subtree to save and would pay per folder
   for the privilege. An unreadable value is refused at open, which is M8 rule 6
   applied to a value rather than a key. A descent that has spent far more requests
   than it has found objects says so once, and names `flat` — the "no silent caps"
   rule: a cost the user cannot see is one they report as a hang.

   **The rejected alternative, recorded because it is the obvious one.** Google's
   `drive.file` scope ("per-file access to files created or opened by the app")
   would fix this at the source, since `files.list` under it returns only what the
   app can see. drivel already offers it — `drivel login -scope drive.file`, stored
   per account — and it is **untested; no test in the tree references it.** It is
   nonetheless the wrong tool for this problem, for a reason that is structural
   rather than a matter of effort: under `drive.file` an app reaches files it
   *created*, or that the user hands it **through the Google Picker**, which is a
   JavaScript component. `drivel mount` is non-interactive and `drivel login` is a
   loopback/paste flow with no browser surface to host one, so there is no mechanism
   by which a user could grant drivel access to a folder that already exists. Their
   files would simply be invisible, which converts a slow sweep into no sync at all.
   There is a second blocker underneath: `resolveRootLocked` resolves the `root`
   alias with `Files.Get("root")`, and root-folder access is documented as
   unavailable under that scope. Where `drive.file` *is* right is a folder drivel
   creates and owns, for a user who wants the narrower grant — that case deserves a
   live test it has never had, and it is not this milestone.

   **What M7c does not fix.** `changes.list` has no folder filter, so the steady
   state is still one account-wide feed per mount, filtered client-side. At one poll
   per 30 s that is a quota cost rather than a latency one, and the fix — one poller
   per account, fanning out to the mounts whose root contains each change — is
   cross-mount coupling of exactly the kind M8's guards exist to prevent. It waits
   for evidence that the quota actually bites.

   **Tested, and what is still owed.** There are two claims here and they need
   different instruments. The *request-count* claim is a ratio a fake can answer
   directly: 1 listing against the flat sweep's 12, for one file in a 23-object
   account. The *wall-clock* claim could not be answered there at all until the fake
   was changed — every request in it serialises behind one mutex, so a sweep that
   fans out was indistinguishable from one that does not, which made that claim
   untestable rather than merely unmeasured. With a round trip injected on the
   child-listing shape only, and taken *before* that mutex, the same fixture at
   fanout 1 and fanout 8 measures **1.03 s against 165 ms — 6.3×**, where the ideal
   for 33 requests eight at a time is ~5 batches. Both halves were confirmed
   load-bearing by breaking them: making `listBatch` sequential collapses the ratio
   to 1.0× and fails on the concurrency assertion, and dropping the injected delay
   fails on the separate one saying the delay never arrived.

   **Throttling was the open question, and asking it found a defect.** Drive answers
   "too fast" with 403 and a `userRateLimitExceeded` reason, which `isTransient`
   already classified as retryable — so the first test, one that throttles whole
   batches, passed immediately: the sweep finishes, every path is found, and the
   error reaches `Downloader.start` as retryable rather than stranding inbound sync.
   The second test is the one that mattered. A throttle landing on *part* of a
   fanned-out batch made the descent put the batch back **whole**, discarding up to
   seven listings that had already succeeded. At fanout 8 a 20% refusal rate spoils
   ~83% of batches, so re-issuing all eight fed the throttle it was reacting to:
   **97× the ideal request count**, measured, and close to a livelock.

   Two changes, and the second exists because the first is not enough:

   - **Keep what succeeded, re-queue only what failed** (at the front, so a failed
     folder is retried before the sweep goes deeper). This alone takes the same
     fixture from 97× to **1.2×**.
   - **Move the concurrency toward what the provider tolerates** — halve on any
     throttled listing, grow back by one on a clean batch, floor 1, ceiling
     `Drive.fanout`. Request-count amplification cannot see this and the first
     change already bounds it; what it prevents is a descent still firing eight at
     a time at a provider that is saying no. Measured peak concurrency: **8
     unthrottled, 2 when one listing in three is refused**, recovering to 8 once a
     burst passes. Backing off fast and recovering slowly is the asymmetry that
     keeps it from oscillating, and the recovery half is asserted separately
     because without it one transient 403 pins the rest of a sweep at one listing
     at a time — which on a large tree costs more than the throttle did.

   So `Drive.fanout` is a **ceiling, not a rate**: eight is where a healthy sweep
   sits, and the descent finds its own level below that without being told the
   account's budget. Every claim above was confirmed load-bearing by breaking it —
   sequential `listBatch`, no injected delay, whole-batch re-issue, no backoff, no
   recovery — and each mutation fails the assertion that names it and no other.

   What is still owed is real Drive: whether it throttles at eight concurrent
   listings **at all**, which is now a tuning question rather than a correctness
   one. MC-11 established that a single writer cannot burst hard enough to reach a
   limit, so this is new territory for the codebase. It belongs in the Tier B matrix
   beside MC-52, whose "enumeration scales with the account" finding is this one
   seen from the other side.


10. **M8 — Multi-account & multi-provider mounts.** ✅ Shipped. §2.8 has the design;
   what is worth recording here is what building it changed about the plan.

   - *Multi-account.* One process, N mounts, each with its own credentials, token,
     state DB, index and engine, described by a TOML config file
     (`$XDG_CONFIG_HOME/drivel/config.toml`). `drivel login -account NAME` scopes
     credentials to a 0700 directory of their own and appends the account to the
     config. The flags still work and now synthesize a one-entry config, so there is
     one code path below them rather than two that drift.
   - *Multi-provider (framework only).* A `provider.Registry` mapping a kind name to
     a `Factory`, with the provider's own settings crossing it undecoded. As
     planned, no second real backend.

   Three things the milestone was scoped as, that it turned out not to be.

   **It was not a refactor of anything below the wiring.** The expectation was that
   two accounts in one process would surface Drive-shaped assumptions; a sweep for
   process-global mutable state found none — every package-level `var` in the tree
   is an interface assertion, a sentinel error, a bbolt bucket name or a default. The
   seam held, which is the result M9 was waiting on, and the proof is a test rather
   than a shipped pseudo-provider: `newFakeDrive` already builds a real `*Drive`
   against an httptest server, so two independently-configured Drive stores cost
   nothing to run and never reach a user's binary.

   **The registry wanted to be a value, not a package.** `init()`-time registration
   would have added the first process-global mutable state in the tree, in the
   milestone whose entire premise is that everything is per-instance.

   **The interesting work was the guards, not the plumbing.** Several mounts can
   corrupt each other in ways one never could, and each failure mode otherwise
   surfaces as a deadlock, an opaque bbolt timeout, or a file synced to the wrong
   account — see §2.8. One of them turned out to need no second mount at all: a state
   DB inside a backing tree syncs itself to the cloud and its own writes generate the
   events that cause more, which every version of drivel until now documented in a
   flag's help string and enforced nowhere.

   Two smaller corrections fell out. `drivel login` had always prompted for an OAuth
   scope and then discarded it — `gdrive` hardcoded `ScopeDrive` — so a read-only
   account asked for full access. And `-sweep-interval` existed as a flag with a
   documented default that nothing was reading into `ReconcileOptions`; the mount
   spec now carries it.
11. **M9 — Plugin architecture.** Let third parties add providers (and eventually
   mount backends) without forking. The seam already exists — `provider.Store` +
   optional `ChangeSource`/`RangeGetter` — so M9 is about the *loading* mechanism
   and its blast radius, not the interface. Go's `plugin` package is a poor fit
   (Linux-only, exact-toolchain-match, no unload); the realistic options are an
   out-of-process plugin protocol (gRPC over a unix socket, hashicorp/go-plugin
   shape) or a WASM host. Either way, M9 must settle: capability scoping (a plugin
   should not inherit the mount's ambient credentials), failure isolation (a
   crashing plugin must not take down the mount), and versioning of the seam
   itself. Depends on M8 having proven the seam with a second registered provider.
12. **M10 — Platform parity (macOS, then FreeBSD).** Independent of M9; nothing
   waits on either. The tree already cross-compiles for both (§2.9), so this is not
   a port — it is closing the two gaps that make a build that *runs* differ from a
   build that *works*. Ordered by ratio of user value to effort:

   **Status: both xattr implementations are written and neither has been run on its
   own platform.** What remains is a live run per platform and the two FUSE-T
   questions below, so M10 is not closed. Writing them also settled a question this
   entry had been asking the wrong way round: the danger was never "off Linux", it
   was "a backing filesystem that cannot hold the marker", which includes drvfs
   under WSL2 and a tmpfs `/tmp` on FreeBSD, on kernels that are otherwise fine. And
   it corrected a claim these documents had repeated for three milestones — that
   losing `drivel-state.db` was what turned a placeholder into an empty file.
   `IsPlaceholder` reads the marker and nothing else; the DB's hydration entries
   cache present ranges and no code has ever consulted them for this. Without the
   attribute there is no placeholder record at all, immediately, and `app.Mount`'s
   warning now says that instead of advising the user to keep a database that was
   never protecting them.

   - **macOS native xattrs. ✅ Written, not yet verified on a macOS host.** The
     `ENOTSUP` stubs no longer compile there: `hydrate/xattr_unix.go` implements
     `get/set/removexattr` against `golang.org/x/sys/unix` for **linux and darwin
     together**, with `xattr_linux.go` and `xattr_darwin.go` reduced to the two
     things that genuinely differ — the errno for a missing attribute (`ENODATA`
     vs `ENOATTR`) and the native-store probe. Sharing one body rather than writing
     a parallel darwin file is the whole trick for a platform nobody here can run:
     the untested delta is two symbols, and every Linux CI run exercises the rest of
     the code macOS will execute. x/sys/unix absorbs the system-call differences
     (the `position` argument, the options word), so there was nothing to translate.
     This is the item that makes `-lazy` safe on macOS: without it the state DB is
     the sole authority for the placeholder marker and M5 invariant 1 is inverted
     (§2.9.1).

     One thing the port turned up that the plan did not predict. **A successful
     `setxattr` is weaker evidence on macOS than on Linux**: on a volume with no
     native extended attributes the kernel emulates them in an AppleDouble `._name`
     sidecar, so the probe passes while the marker becomes a file *in the backing
     tree* — syncable, materialisable on another client as garbage, and detachable
     from the file it describes, which is M5 invariant 2 with extra steps.
     `xattrNative` looks for the sidecar after the probe write and reports emulation
     as "no xattrs", so such a volume gets the warning it should always have had.

     `internal/hydrate/xattr_unix_test.go` is new and is the acceptance test for
     exactly this layer — round trip, overwrite, and the missing-attribute
     classification that the differing errno decides — separately from
     `hydrate_test.go` and `syncengine/lazy_test.go`, because on a kernel drivel has
     never run on, "the syscalls behave" and "placeholders behave" are different
     diagnoses. `internal/vfs/xattr_linux_test.go` became `xattr_unix_test.go` and
     builds on darwin and freebsd too, so the mountpoint-refusal guards run wherever
     a FUSE mount can be made. FreeBSD needed a client-side shim to get there
     (`xattrclient_unix_test.go` / `xattrclient_freebsd_test.go`), because that test
     pokes the mountpoint the way an ordinary program would and so has to be spelled
     in the platform's own interface: passing `"user.test"` to `extattr_set_file`
     would set an attribute literally *called* `user.test` in the user namespace — a
     different attribute, silently asserted. The tests therefore name attributes
     without a namespace and let the shim place it. `internal/testenv` already turns
     a missing-xattr environment into a failure rather than a skip under
     `DRIVEL_REQUIRE_TESTENV=xattr`, which is what a darwin runner needs to hold the
     line.

     **What is left is the running, and it splits in two.** The xattr half needs
     only a macOS kernel and a filesystem, so a VM is a complete answer for it: the
     hydrate suites are userspace against APFS, and virtualisation does not change
     what `getxattr` does. The mount half needs macFUSE, which is a kernel
     extension, and that is where a VM stops being equivalent — a guest that has to
     run with reduced security to load a kext is not the machine a user has, and on
     an Apple-silicon host a macOS guest cannot load third-party kexts at all. Run
     the xattr and lazy suites in the VM, and treat the end-to-end mount tests as
     still owed to real hardware before the platform is called supported.

     One thing the FreeBSD run says about macOS in advance: darwin shares the
     `!linux` reply path in go-fuse, so it shares the mechanism behind the `EBADF`
     that run turned up (§2.1). Whether macFUSE's kernel ever reads through a write
     handle is unknown and only a live run answers it — but the fix is unconditional,
     so if it does, the case is already covered rather than waiting to be discovered
     on the machine drivel does not have.
   - **FreeBSD `extattr_*`. ✅ Written, and verified on FreeBSD 15.1 on 2026-09-06.**
     A real port rather than a translation, as this entry expected:
     `hydrate/xattr_freebsd.go` implements the three calls over
     `extattr_get_file`/`extattr_set_file`/`extattr_delete_file` in
     `EXTATTR_NAMESPACE_USER`, and it cannot join the shared linux+darwin body
     because the namespace is an argument rather than a name prefix.

     **The marker name became per-platform, which this entry predicted and the macOS
     half did not deliver.** It is `drivel.placeholder` here against
     `user.drivel.placeholder` elsewhere — the same attribute in the same namespace,
     spelled for two different interfaces. `hydrate.XattrName` is now assembled from
     a per-platform `xattrName`, so the only thing that changed above the seam is
     that no caller may write the Linux form out.

     Two things the port turned up that were not in the plan. **x/sys types the
     extattr buffer as a `uintptr`**, so the address of a Go slice crosses a function
     boundary as an integer — which does not keep the array alive and, worse, is not
     rewritten when a goroutine's stack is copied to grow it, and the wrappers
     allocate twice (`BytePtrFromString`) before reaching the kernel. Escape analysis
     confirmed the exposure rather than merely suggesting it: without a pragma the
     compiler reports `setxattr`'s buffer as not escaping, i.e. free to stay on the
     stack. Two one-line wrappers carrying `//go:uintptrescapes` fix it by forcing
     the converted pointer to the heap for the duration of the call; `runtime.KeepAlive`
     would not have, since it addresses liveness and not stack copying. And
     **`extattr_set_file` reports a byte count** where Linux and macOS succeed
     wholesale, so a short write is a case only this platform can produce; it is an
     error rather than a success, because a truncated marker is unparseable JSON,
     which `Marker` reads as "placeholder, contents unknown" — safe, and permanently
     unpushable.

     `internal/hydrate/xattr_unix_test.go` covers freebsd too (the primitives are
     named the same on every platform), so the acceptance test is
     `DRIVEL_REQUIRE_TESTENV=xattr go test -race ./internal/hydrate/ ./internal/syncengine/`
     with `TMPDIR` on UFS or ZFS — **tmpfs has no extended attributes**, so a tmpfs
     `/tmp` makes the whole suite report the facility missing, which is the correct
     answer and not the one you want to be testing. Unlike macOS, a FreeBSD VM
     settles the mount half as well: fusefs is in base, so `kldload fusefs` and
     `vfs.usermount=1` make `DRIVEL_REQUIRE_TESTENV=all` a real end-to-end run.

     **That run happened, and the interesting part is what it caught.** The whole
     suite passes on FreeBSD 15.1 under `-race` with nothing skipped and `TMPDIR` on
     ZFS. Both of this entry's own predictions held and neither fired: no short write
     from `extattr_set_file`, and no sign under `-race` of the `//go:uintptrescapes`
     wrappers failing. What failed was four tests in `internal/vfs`, none of them
     about M5 — every one wrote through an `O_WRONLY` handle and got `EBADF`, because
     the kernel reads through a write handle to fill a cache block (§2.1). The
     milestone's own surface was right and the layer nobody was worried about was
     not, which is the argument for `=all` over `=xattr` wherever the platform can
     run it.

     The independent check is worth repeating for the macOS run, because it is the
     part that does not merely ask drivel whether drivel is happy: `lsextattr` and
     `getextattr` were asked what the marker actually is. `user` namespace, name
     `drivel.placeholder`, JSON byte-identical, nothing in `system`, and the
     placeholder genuinely sparse.
   - **Verify FUSE-T's xattr behaviour before either lands** (§2.9.3). FUSE-T is
     NFS-backed; if it does not carry `user.*` xattrs, the macOS item delivers
     nothing under FUSE-T and the documentation has to say which macOS FUSE
     implementations `-lazy` is actually safe on. This is a research task with a
     one-line answer, and it gates what M10 is allowed to claim.
   - **Then measure, then decide about FUSE-T support at all.** §2.9.3 predicts
     macFUSE is faster and explicitly does not assert it. If FUSE-T is close enough,
     kext-free operation is worth upstream work in go-fuse; if it is not, the honest
     documentation is "macFUSE, and here is why."

   Explicit non-goals: in-place mode off Linux (it needs an fd-relative `*at`
   loopback — §2.7, still future work) and Windows in any form (§2.9.4).

13. **M11 — Deduplicating local backend (`dedup`).** A `provider.Store` whose
   "cloud" is a directory on this machine: content-addressed chunks plus a manifest
   per path, with the sync engine driving it exactly as it drives Drive. Mount a
   directory, write to it, and what lands in the store is one copy of each distinct
   chunk. This is the repo's original name arriving through the provider seam
   rather than through a rewrite of the mount — which is also why it is a milestone
   and not a re-scoping: nothing above §2.5 changes.

   **Decided: it is a provider, not a layer under the mount.** The obvious
   framing — an arbitrator sitting between the mount and the directory it writes
   to — puts it in `internal/vfs`, on the FUSE path. Everything drivel already has
   argues against
   that. On the provider side it inherits debounce and coalescing (M4), the M6
   gates, echo suppression, conflict copies, the M7b sweep and the drain on
   shutdown — and it stays off the FUSE path, which is the §2.1 rule that keeps
   `write(2)` from waiting on a hash. On the mount side it would inherit none of
   it, would have to re-implement crash consistency for a store that is now in the
   write path, and would make every read a chunk-assembly. The cost of the provider
   framing is that eager mode stores the bytes twice — once in the backing dir,
   once in the store — which is the next paragraph.

   **`-lazy` is what makes it a deduplicating filesystem rather than a backup
   target.** With M5 the backing dir holds placeholders and the store holds the only
   full copy, so "mount 4 TB of deduplicated data on a 200 GB disk" is the existing
   lazy path over a local provider, with no network. A local provider also makes
   the two things that are awkward against Drive trivial: `RangeGetter` is a seek,
   so hydration is genuinely partial, and `RangePutter` *is* implementable (splice
   the changed extents by re-chunking that span and rewriting the manifest), which
   makes M11 the first backend that exercises the M6 range-write path that Drive
   deliberately declines. `ContentHasher` is native — the store hashes everything
   anyway — so M6's unchanged-content gate becomes exact and free.

   **Data model, and the parts that are decided by physics rather than taste.**

   - *Chunking.* Content-defined (FastCDC/Rabin, e.g. 512 KiB min / 1 MiB avg /
     8 MiB max) rather than fixed blocks: fixed blocks lose all dedup after a
     single-byte insertion shifts a file, which is the case dedup exists for.
     Note the interaction with M6, which describes dirty extents on a fixed block
     grid: the manifest translates an extent to the chunks it touches, and a write
     re-chunks that span plus the tail up to the next boundary the chunker
     re-synchronises on. Nothing about M6's contract changes — `Dirty == nil` still
     means "push everything" — but the store must round *outward* to chunk
     boundaries, the same asymmetry `ranges.MarkCovering` already encodes.
   - *Manifest.* path → ordered chunk hashes + sizes. Flat in bbolt is enough for a
     local store. Making the manifest itself content-addressed (file = hash of its
     chunk list, directory = hash of its entries) is the Merkle DAG, and it buys
     things a flat table cannot: whole-subtree dedup, "did anything under here
     change?" as one hash comparison, and integrity verification that covers the
     structure and not just the bytes. It costs a rewrite of the spine to the root
     on every write, which is contention on exactly one key. **Recommendation:**
     flat manifests first; adopt the Merkle spine when something needs to *compare
     two trees it cannot both hold*, which is M13's problem, not M11's.
   - *The chunk index is not a hand-built trie.* The natural way to write "branch on
     the next N bytes of the hash" is a radix trie or a HAMT, and it is the right
     structure — but bbolt is already a B+tree over ordered keys and badger is
     already an LSM over ordered keys, so keying either directly by the chunk hash
     gets the prefix locality a trie would provide, with the crash consistency
     written and tested. A hand-rolled trie *above* one of them is a second index
     to keep consistent with the first.
   - *Fanout belongs to the on-disk layout, and 3 bytes is far too many.* If chunks
     are individual files (git-object style), the fanout exists to stop one
     directory holding millions of entries — and 3 bytes is 16.7 M directories,
     which trades a big directory for an inode and dentry-cache problem that is
     strictly worse, with almost all of them empty. Git uses one byte (256); two
     nested single-byte levels (65 536) is the usual ceiling. **Better still, skip
     the question:** write chunks into pack files of a few MiB with an index
     entry of (pack, offset, length), the way restic and borg do. A 4 KiB filesystem
     block per 1 KiB chunk is a 4× space loss that dedup then has to win back, and
     packs also turn a restore into sequential reads.
   - *Garbage collection is the actual project.* Everything above is a weekend; a
     CAS that can delete safely is not. Refcounts are fast and wrong at the first
     crash mid-update; mark-and-sweep is correct but must walk every manifest and
     must not race a concurrent write, which needs generations or a write barrier.
     Reclaiming space from packs is a third problem (compaction, rewriting live
     chunks out of half-dead packs). Note this changes what `Store.Remove` means:
     unlink is a manifest edit, and space comes back later, or never, depending on
     the answer here.
   - *bbolt or badger.* bbolt is what the tree already uses twice, is a single file,
     has no compaction, and is excellent for read-mostly data. Its weak case is
     precisely this one: bulk insertion of uniformly random keys (a hash index is
     the definition of random) splits pages relentlessly and grows the file, and a
     write transaction is single-writer and fsyncs. Badger's LSM absorbs random
     writes far better and is built for tens of millions of keys, at the cost of
     background compaction, more memory, a value log to reason about, and a second
     storage engine in the tree. **Recommendation:** bbolt behind a small interface
     first, with a benchmark that inserts 10^7 random keys as the acceptance
     criterion; swap to badger if it fails, which `internal/pathindex` shows is a
     contained change.

   **What is genuinely new.** A local provider has no `changes.list` to poll, but it
   can offer something better: since we own the store, a monotonic sequence number
   per commit makes `ChangeSource` exact rather than an approximation, and
   `Enumerate` is a walk of the manifest table. It also breaks an assumption worth
   naming — until now "the provider" has meant "somewhere else", so a second mount
   pointing at the same store is a case `app.Validate` has never had to consider.

   **It also settles M9's precondition properly.** M8 proved the §2.5 seam with two
   Drive stores in one process, which is a good test and a weak proof — both sides
   were the same implementation. A second provider that is not a cloud at all, is
   not ID-addressed, has an exact change feed and *does* implement `RangePutter`
   exercises every optional interface in the seam in the opposite direction from
   Drive. Whatever survives that is what M9's plugin API should be shaped like.

   **Decided: the store is private to one machine.** One host, one store, one
   writer — which is what makes mark-and-sweep GC tractable, flat manifests
   sufficient and locking unnecessary, i.e. it is what makes the whole entry above
   the size it is. A store several machines sync into brings back cross-client GC
   (a sweep cannot run while another host is writing), leases, and the Merkle spine
   as a requirement rather than an option; that is M13 wearing a local filesystem
   as a disguise, and it should be built there or not at all.

14. **M12 — Encrypting backend (`crypt`).** Same shape as M11 with a different
   transform, and one structural difference that makes it more interesting: it is
   most valuable *stacked over another provider*, not over a local directory.
   `crypt` over `gdrive` is client-side end-to-end encryption for Drive — the thing
   rclone's crypt remote and gocryptfs exist for — and it is a strictly better
   answer than "encrypt a local directory", which the operating system already does.

   **Decided: it is a decorator over another provider**, with "encrypt into a local
   directory" available as the degenerate case of wrapping a plain-directory store
   rather than as a second backend.

   **The new capability it needs is provider stacking**, and it is small: a
   decorator provider has to open its inner store, so `provider.Params` must carry
   a way back into the `Registry` (an `Open(kind string, p Params) (Store, error)`
   hook), and a provider's config table must be allowed to name another kind and
   nest its settings. `internal/config` still learns nothing — the nested table is
   opaque to it exactly as a Drive folder ID is (§2.8, rule 4). This is also the
   cleanest possible prompt for M9's plugin seam: a decorator is where a plugin API
   either composes or does not.

   **Where the seam fights back, all of which is findable before writing code.**

   - *Filenames are the hard part, because the seam is path-addressed.* Encrypting
     a name must be **deterministic** within its parent (SIV/EME-style) or path
     lookup stops working — a random nonce per name means the only way to resolve
     `a/b/c` is to list and trial-decrypt, which makes the M7 index authoritative
     and breaks that milestone's invariant 2. Deterministic names leak equality
     (the same name in the same directory is the same ciphertext) and leak length
     unless padded. Both are documented rclone/gocryptfs trade-offs, not novel risk.
   - *Sizes stop matching, and M6 notices.* AEAD per block adds a nonce and a tag,
     so the ciphertext is longer than the plaintext. M6's gate 2 requires the remote
     object to exist "at exactly the local size", and M5's placeholders need the
     *plaintext* size to be honest. The crypt layer therefore has to translate sizes
     in both directions and own that arithmetic; forgetting it disables M6 silently
     (the good failure) or writes wrong-sized placeholders (the bad one).
   - *Random access requires the block layout to be part of the format.* Fixed
     plaintext blocks (32–64 KiB) with a nonce derived from (file key, block index)
     keeps `GetRange` a seek and `PutRange` a block rewrite. A stream cipher over
     the whole file makes M5 and M6 both impossible. Deriving the nonce from the
     index rather than randomly is what makes rewrite-in-place safe *only* if the
     file key changes on rewrite, or the same (key, nonce) encrypts two plaintexts —
     the one cryptographic mistake in this design that is fatal rather than
     embarrassing.
   - *`ContentHasher` has to be answered deliberately.* M6's gate 3 compares a local
     digest with what `Stat` reports the remote holds — which is now a digest of
     ciphertext. Either the layer hashes the ciphertext it *would* produce
     (deterministic, and it must be, for this to work), or it keeps a plaintext MAC
     in a file header and compares that. The header is simpler and does not force
     determinism on content.
   - *Dedup and encryption do not compose for free.* Encrypt-then-dedup dedups
     nothing; dedup-then-encrypt with convergent keys (chunk key = hash of chunk)
     dedups across users and thereby leaks whether you hold a given file — the
     known confirmation-of-file attack. If M11 and M12 are ever stacked, the honest
     default is dedup within one key holder's data only.
   - *Key handling collides with an existing rule.* `drivel mount` is
     non-interactive by design (CLAUDE.md; it must never prompt on stdin), so the
     passphrase cannot be prompted at mount time. That means a keyfile with
     enforced 0600 through `gauth.writeSecret`, an agent, or the OS keyring — and
     `login` is the interactive command where a passphrase *may* be entered.
     Argon2id for the KDF, a master key wrapped per file, and no key material in
     `config.toml`.

15. **M13 — Block-level filesystem over a distributed database (`nosql`).** Clients
   talk to the database directly; files are blocks, metadata and directory entries
   are rows, and several machines mount the same tree. This is the largest item on
   the roadmap by a wide margin, and the first one where the honest answer starts
   with a question about which of two products it is.

   **The fork in the road, which decides everything else.**

   - *(a) Another provider.* The database is the sync target, each client keeps its
     backing dir as the local source of truth, and the existing engine syncs to it
     asynchronously. Everything in v1 and v2 applies unchanged: no distributed
     locking, last-writer-wins with §6 conflict copies, echo suppression, the
     `journal` table as a real `ChangeSource`. Weeks of work, not years, and it
     composes with M11 (the block/chunk tables *are* M11's CAS, remote).
   - *(b) A cluster filesystem.* The database is the filesystem; there is no local
     source of truth; a read that misses cache blocks on the network. This
     contradicts the §2.1 rule that FS operations never block on the network, and it
     is what forces locking, leases, cache coherence and ACID commits into the
     design. It is a different program that would share drivel's mount layer and
     very little else — which is fine, but it should be *decided*, not discovered
     halfway through the schema.

   **Decided: (a) first, and (b) is not a later phase of it.** The first release is
   another provider behind the existing engine — it makes the schema, the block
   store and the journal real while the consistency model stays the one already
   shipped and tested, and it is the version that can exist this year. What it must
   not do is quietly pretend to be (b): a client that keeps a local source of truth
   and resolves races with conflict copies is not a cluster filesystem, and saying
   so plainly in the documentation is part of the milestone. If (b) is ever wanted
   it is a second program sharing the mount layer, and the honest cost of that
   decision is visible below; the parts of (a) that survive into it are the schema
   and the block store, which is precisely why the schema is written for a
   transactional engine even though (a) does not need one.

   **On Cassandra specifically: it is the wrong tool for the metadata half.** It is
   an excellent tool for the block half. The reasons are structural, not a matter of
   tuning. There are no multi-partition transactions, so any operation touching two
   rows — `rename`, `link`, a `create` that updates a parent and an inode — has no
   atomic form; lightweight transactions are per-partition Paxos at roughly four
   round trips, which is a slow way to be correct on the subset of operations that
   fit. Conflict resolution is last-write-wins by cell timestamp, which silently
   resolves races that a filesystem must not resolve silently. Deletes write
   tombstones, and a block-level filesystem is a delete-and-overwrite workload, so
   the partitions you scan (a directory listing, a file's block range) accumulate
   exactly the thing that makes Cassandra reads fall over. And large cell values are
   discouraged; blocks want to be ≤ 1 MiB, which is fine, but compaction then
   rewrites every block repeatedly.

   **What to use instead, in the order I would consider them.**

   - **FoundationDB.** The specific answer to "a general-purpose schema I can adapt":
     an ordered key space with strictly serialisable multi-key transactions, which
     is what a filesystem's metadata layer wants and what nothing else on this list
     offers as directly. Directory listings are range reads; `rename` is one
     transaction. Constraints shape the design honestly (5 s / 10 MB per
     transaction, so blocks live outside it and only their references are
     transactional) and it is designed to be layered on. Its operational cost is
     real and its client is C-with-Go-bindings, which is a mark against a pure-Go
     tree — worth weighing against writing consistency by hand.
   - **TiKV** — the same transactional ordered-KV shape, Raft-based, gRPC client,
     no cgo. The closest thing to FoundationDB that keeps the build pure Go.
   - **MongoDB** — multi-document ACID transactions since 4.0, a driver everyone
     has, and a data model that maps to inodes and dirents without contortion.
     Cross-shard transactions are expensive, so shard by inode and keep a rename's
     rows co-located. The boring option, which is a compliment.
   - **ScyllaDB** — if Cassandra's model is wanted anyway, this is Cassandra without
     the JVM and with much better tail latency. It does not fix the transaction
     model, so it belongs in the block half.
   - **CockroachDB / Postgres** — outside the "NoSQL" framing but exactly this shape,
     and worth naming because "we need transactions over an ordered key space" is a
     description of a SQL database with extra steps.
   - **Redis / Memcached: cache and leases only, never the authority.** Specifically,
     do not build the correctness of a lock on Redis: the Redlock argument is that
     a lock held across a GC pause or a clock jump can be believed by two clients at
     once, so a lease must be checked *at the point of use* by a fencing token the
     storage layer validates. Drivel already has the right instinct written down —
     `internal/pathindex` is a cache that is never authoritative and is verified
     before it is acted on (M7, invariant 1) — and a metadata cache here needs the
     same rule with the same enforcement.

   **Schema sketch, kept engine-neutral on purpose.** Every engine above can express
   an ordered KV model; Cassandra expresses it worst (ordering exists within a
   partition, not across). Tables: `inode` (inode_id → mode, owner, size, times,
   nlink, generation), `dirent` ((parent_inode, name) → child_inode, type — so a
   listing is one ordered range read and a lookup is a point read), `block`
   ((inode_id, block_index) → content hash, or inline bytes below a threshold),
   `content` (hash → bytes or pack reference, refcount) which is M11's CAS again,
   `lease` (inode_id → owner, expiry, fencing token), and `journal` (a per-tree
   ordered log of committed operations). The journal is worth building even in
   variant (a), because it is simultaneously the crash-recovery record, the cache
   invalidation channel, and a `provider.ChangeSource` — one structure paying for
   three things drivel already knows how to use.

   **Split the two interfaces.** A *metadata engine* and a *block store*, chosen
   independently: FoundationDB or TiKV metadata over S3 or a Cassandra block store
   is a shape that works, and it is what JuiceFS does (pluggable metadata engine,
   objects in object storage) — which is the closest existing implementation of what
   M13 describes and the best available source of schema decisions already made
   under load. SeaweedFS and CephFS are the other two worth reading before writing:
   Ceph's MDS exists because "put the metadata in the database and let clients race"
   is where every such design ends up, and knowing *why* it grew a metadata server
   is cheaper than rediscovering it.

   **The unglamorous things that decide whether it works:** `O_APPEND` and shared
   mutable files (POSIX says two writers interleave atomically per write; a block
   store says they do not), `fsync` semantics against an eventually-consistent
   store, partial-block writes (read-modify-write needs the read to be consistent
   with the write), and what happens to an open file handle when another client
   deletes the inode. Answering those is the milestone; the schema is the easy part.

16. **M14 — Control & status API.** A local IPC surface so software this repo does
   not ship can ask what drivel is doing and tell it to stop doing it: per-path and
   per-directory sync status, per-backend statistics, online/offline per backend,
   pause/resume, and shutdown. Depends on nothing but M8 — which is what makes it
   multi-mount from the first line, since every question here is asked *of a named
   mount* — so it is schedulable independently of M9–M13.

   It is a milestone rather than a flag because it is the first **compatibility
   surface with code we do not control**. Every other seam in the tree can be
   redesigned in an afternoon; a field a file-manager extension binds to cannot.
   Most of the work is therefore deciding what the API is allowed to promise, and
   the answer to almost every "could we also expose…" is no unless the state
   already exists for its own reasons.

   **Decided: a unix socket, HTTP/1.1 + JSON, one listener per process.** The
   surface can enumerate every synced path, stop syncing and stop the process, so
   it must have an owner rather than a port number: a socket at
   `$XDG_RUNTIME_DIR/drivel/control.sock` (directory `0700`, socket `0600`) is
   authenticated by the filesystem, and `SO_PEERCRED` backs that up on platforms
   where the mode bits on a socket are advisory. A stale socket after a crash is
   *connected to first* and only unlinked on `ECONNREFUSED` — blind removal steals
   a running daemon's socket. HTTP+JSON over `net.Listen("unix", …)` rather than
   gRPC because the audience is third-party tools in unknown languages: it adds no
   dependency and no codegen, `curl --unix-socket` is a working client, and the one
   thing gRPC would buy (a typed schema, streaming) is covered by a documented
   schema and one streaming endpoint. **Do not merge this with M9's plugin
   protocol** if M9 lands on gRPC. The trust directions are opposite — a plugin is
   code we load into our address space, a control client is a user we serve — and
   one transport serving both is how a plugin ends up able to shut down the daemon.
   Paths are versioned (`/v1/…`), additive within a version.

   **Decided: the API is a view, never an authority.** Same rule that keeps
   `internal/pathindex` non-load-bearing (M7, invariant 2): no sync decision may
   take an input from the control layer, and nothing in the engine may read its own
   state back out through it. The moment a status is load-bearing, a third-party
   tool holding a connection changes what gets uploaded.

   **The status read must not touch the engine's run loop.** `Engine.Run` owns the
   coalescer and its timers by goroutine confinement — there is no mutex, on
   purpose — and `dispatch` blocks when a worker's queue fills, which is exactly how
   backpressure reaches the mount. Serving status *from* the loop would therefore
   hang precisely when a user asks the question they most want answered ("why is it
   stuck?"). The loop publishes an immutable snapshot on each transition and the
   API reads that pointer. For the same reason a directory query is **one request
   for the directory**, bounded, with a `truncated` flag — the load pattern is a
   file manager repainting a folder, and per-file requests turn that into a
   thundering herd against the very loop that must not be disturbed.

   **`unknown` is a first-class status, and the API is worthless without it.**
   The values all come from state that already exists: `synced` (a §4 echo matching
   the local content), `pending` (the coalescer holds the path), `uploading`,
   `retrying` (with attempt count and next attempt), `error` (with the last one),
   `placeholder` (M5, read from the authoritative xattr), `conflict` (a §6 copy
   sits beside it) — and `unknown`, which means we have no record, not that
   everything is fine. Reporting "synced" for a path we have never observed is the
   same class of lie as mistaking a placeholder for an empty file, and it is the
   lie a status API is most tempted to tell because it makes the screenshots look
   better. Answering a status query must also **never call the provider**: local
   state only, or repainting a folder becomes a billed `files.list`.

   **Pause stops dispatch, never consumption — the obvious implementation breaks
   the filesystem.** `vfs.node.emit` blocks rather than drops, deliberately, because
   losing a mutation loses a sync operation. So a pause that stops draining the
   event channel fills it and then blocks FUSE: `cp` hangs until someone resumes.
   The pause therefore lives at the flush, with the run loop still consuming and
   still coalescing. That bounds memory by *distinct paths touched while paused*
   rather than by write volume, which is what the coalescer is for, but it does not
   bound it absolutely, so the milestone owes an overflow rule. **Discarding the
   pending set is not one of the options**, and the reason is not obvious: a sweep
   does not repair it. `reconcile.pushLocalOnly` skips every path that already has
   an echo, so a lost *modification* to an already-synced file is never re-pushed —
   the next sweep reaches it through `reconcileRemote` instead, where the user's
   edit is demoted to a §6 conflict copy and the path reverts to the remote's
   bytes. The recommendation is a cap on pending paths that **auto-resumes and says
   so loudly**, with the count and the cap in the status output; persisting the
   pending set to the state store is the sanctioned upgrade if that proves too
   blunt, and it is new schema, not a tweak. Pausing must also hold the pull loop
   and the sweep. A long pause is safe by construction: if the change cursor dies
   while paused, resume takes the existing `ErrCursorExpired` path — fresh token,
   then a sweep.

   **Pause is not persisted, and it should expire.** A mount that stays paused
   across restarts is a mount that looks mounted and silently never syncs, which is
   the failure mode this project exists to avoid; a restart resumes. If a durable
   pause is ever wanted it belongs in the config file, where the user can see it,
   not in a runtime bit nobody can find. The request should carry a duration
   (`for=30m`, with a ceiling) so the two real uses — a metered link, a large local
   reorganisation — heal themselves when the user forgets.

   **Online/offline is observed, never probed.** A health-check ping spends quota,
   can succeed while the operation the user cares about fails, and invents a
   failure mode no user operation would have produced. Everything needed is already
   flowing past: the outcome of the last call classified by `provider.IsRetryable`,
   the time of the last success, the consecutive-failure count, the age of the
   change cursor. `reachable` is derived from those and has three values, because
   an idle backend we have not spoken to in an hour deserves `unknown` rather than
   a guess. No traffic, no opinion.

   **The statistics are most of the implementation, because today they are log
   lines.** Per mount, monotonic counters read through the same snapshot: bytes and
   operations each way, uploads by outcome, retries, queue depth and pending paths,
   hydrations and bytes faulted in (M5), conflict copies (§6), the sweep numbers
   `sweepStats` already computes, cursor age, last completed sweep. Include the
   three M6 gates individually — placeholder declined, range put, unchanged-content
   skip — because they are the milestone's entire value proposition and nothing
   currently counts them. Counters are process-lifetime except where the state DB
   already makes them durable; a Prometheus text endpoint is a short adapter over
   the same snapshot, worth having and emphatically not worth letting define the
   internal shape.

   There is already a consumer waiting: the multi-client rig
   (`docs/dev/multiclient-test-plan.md`, Tier B) counts uploads and downloads by
   grepping log lines, which is why a push that logged nothing at all could look
   exactly like a push that never happened. A measurement surface that a test
   harness can read is the same surface a file manager wants.

   **Shutdown runs the SIGINT path, not `os.Exit`.** `POST /v1/shutdown` cancels the
   same context a signal cancels, so the three-phase `Close` and the bounded drain
   still protect queued uploads; it returns immediately with the drain deadline and
   the caller watches the socket close. It must be distinguishable from a signal in
   the log, because "what stopped my sync daemon" is an operational question with a
   bad answer if the log cannot tell.

   **One streaming endpoint, or everyone polls.** Newline-delimited JSON of status
   transitions, with the rules that make a producer safe: a bounded per-client
   buffer, a slow client **dropped rather than allowed to block**, transitions
   coalesced per path on the uploader's own debounce (an overlay does not want 400
   events for one write), and an explicit `resync` marker so a client that was
   dropped re-reads the directory instead of assuming it missed nothing.

   **Three things are not settled, and all three are policy, not implementation.**
   *(a) On by default, or off?* `-pprof`'s precedent is off, and this endpoint hands
   out path names and can stop the process; but an integration that requires the
   user to add a flag is an integration nobody writes. The tempting middle — a
   read-only surface on by default, the mutating verbs behind a flag — is two
   surfaces to test and only pays if the read side is harmless, which it is not
   quite, since path names are user data. The recommendation is off for the first
   release and revisit with a real client in hand: loosening later is easy.
   *(b) Authorisation beyond the socket mode.* Peer-uid is enough for one user's own
   tools and not for a sandboxed (Flatpak, snap) client or a shared machine, where
   the usual answer is a token file. Don't build it before something needs it, but
   accept and ignore an `Authorization` header from day one so the shape can grow
   one. *(c) Does status answer for a **tree**?* Per-file is cheap; "is this folder
   fully synced?" is the question a file manager actually asks, and the daemon has
   no rolled-up index to answer it. Aggregating on demand is a walk; maintaining a
   per-directory dirty count is a new index with its own invariants and its own
   drift bug. This one has to be decided before the response schema, not after.

   **Explicit non-goals.** Not a remote-control API — no TCP, and adding one is a
   different milestone with an authentication design. Not a config editor: M8 rule 5
   says the file is only ever appended to, and an API that re-serialised it would
   silently drop every comment. Not a way to invoke arbitrary provider calls. And
   not a place to add state to the engine: if a field cannot be read from something
   that already exists for another reason, it does not belong in v1 of the schema.

   **The profiling endpoint's remaining hardening belongs here, not to `-pprof`.**
   Three of the five things worth doing to it were done when the question was asked
   (2026-09-08): `/debug/pprof/cmdline` is not registered, because argv names the
   credentials file, the token, the backing tree and the account; a non-loopback
   bind is refused rather than warned about, and takes an explicit
   `-pprof-allow-remote`; and `IdleTimeout` is set, which it had to be, since it
   falls back to an unset `ReadTimeout` and an idle keep-alive was therefore held
   forever. The two that remain are this milestone's shape rather than that flag's.
   **Splitting the heavy routes** — `profile` and `trace` are what let a caller make
   a busy mount slower on request, and MC-53 uses neither, so a default set of
   `heap`/`goroutine`/`allocs` would cost the soak nothing — is a second surface
   with its own default, which is question (a) above wearing different clothes. And
   **serving profiles over the unix socket** removes the port altogether: filesystem
   permissions become the authorisation, `go tool pprof` reads a file that `curl
   --unix-socket` wrote, and nothing is left for a container's port forwarder to
   publish. That last one is not hypothetical — an editor's auto-forwarder will
   publish a loopback bind off the host, which is precisely the case the old warning
   never fired for.

   **Shape and acceptance.** `internal/control` holds the server (provider-agnostic,
   above `app`, driven by `App` because `App` owns the mounts), the engine and
   downloader grow read-only snapshot accessors, and the versioned artefact is
   `docs/dev/control-api.md` — the schema, not the Go types. The first client ships in
   tree as `drivel status` / `drivel pause` / `drivel resume` / `drivel stop`,
   because an API whose only consumers are hypothetical drifts within one release,
   and because the CLI is the acceptance test: it must be writable with no access
   the socket does not give a stranger. Three tests carry the milestone — status
   served while the run loop is blocked on a full worker queue, a pause held under
   a write storm without the mount blocking, and an API shutdown draining byte for
   byte like a signal.


17. **M15 — Special files, POSIX metadata, and the mount's safety options.**
   **Items 1–3 shipped (2026-09-08); 4 and 5 remain, in that order.** This is MC-13
   (`docs/dev/multiclient-test-plan.md` §3, Group II) promoted from "define the
   behaviour and write it down" to a set of decisions, because an audit found half
   of them already true by accident and the other half one-line changes that close
   a security surface.

   **What shipping 1–3 changed about the entry below, all of it found by running
   the tests rather than by reading the code.** The one-line changes were one-line
   changes and the platform was not. Three things:

   - **The compulsory options cannot be one list.** FreeBSD's kernel dropped
     `MNT_NODEV` — only devfs may hold device nodes, so the property holds there by
     construction — and `mount_fusefs` parses `-o` against a fixed table and
     **fails the mount** on an option it does not know: `mount_fusefs: -o dev:
     option not supported`, and no mount at all. Item 1 as written would have made
     drivel unmountable on FreeBSD. `compulsoryOptions()` is therefore per platform
     (`internal/vfs/mountopts_*.go`), which is also the honest place to say that
     macOS has the flag and no maintainer has run it.
   - **On FreeBSD a special file cannot be created through the mount at all**, so
     item 3's log line has nothing to describe there. Two independent causes:
     fusefs sends `rdev = ~0` on the MKNOD for a fifo while FreeBSD's `mknod(2)`
     accepts `S_IFIFO` only when `dev == 0`, so go-fuse's loopback — which passes
     rdev through verbatim — yields `EINVAL` before drivel's override decides
     anything; and FreeBSD's `mknod(2)` refuses `S_IFREG` outright, on an ordinary
     ZFS directory as much as through a mount. Neither is drivel's to fix here and
     neither risks data (a loud `EINVAL`, no file), so the tests **assert** that
     rather than skipping, and the platform behaviour is pinned.
   - **`Mknod` had to grow a case the entry did not have.** `mknod(2)` with no type
     bits creates a *regular* file, which must sync like any other, or the mount
     and the sweep disagree about the same file — the walk pushes whatever is
     regular, so syncing would depend on which syscall created it. That is MC-12's
     shape (§4.14 of the test plan) arriving by a second route, and it is a bug
     wherever it appears, so the regular-file branch emits `OpCreate`.

   Item 4 is unchanged and still waits on the provider-metadata capability; item 5
   still waits on item 4.

   **What the audit found, which is most of the answer.** The M7b local walk
   already skips everything that is not a regular file (`reconcile.go`,
   `!e.Type().IsRegular()`), and `internal/vfs` overrides `Create`, `Open`,
   `Mkdir`, `Rmdir`, `Unlink`, `Rename` and `Setattr` but *not* `Symlink`, `Link`
   or `Mknod` — so those three fall through to go-fuse's `LoopbackNode`, are
   created correctly in the backing store, and emit no `fsevent` at all. A symlink
   or a fifo made through the mount therefore works locally and is never pushed,
   which is rclone's default behaviour arrived at by two independent routes,
   neither of them a decision anyone recorded. The gap is that drivel is **silent**
   where rclone warns unless told not to be (`--skip-links`, `--skip-specials`),
   and silence is what turns "unsupported" into a support question.

   1. **`nodev` and `nosuid` are compulsory mount options, with no flag to disable
      them. ✅ Shipped.** This is a cloud-storage client: a device node or a setuid binary
      arriving from a remote is never something a user asked for, and there is no
      legitimate case to weigh against refusing it. On the unprivileged path they
      are already forced — fusermount mounts FUSE filesystems `nodev,nosuid` by
      default and only a privileged user can override that — but go-fuse also has a
      direct-mount path that translates the strings in `MountOptions.Options` into
      `MS_NODEV` and `MS_NOSUID`, and drivel currently passes no options at all.
      Setting them explicitly is what makes the guarantee independent of which
      mount path was taken and of who ran the process.

      **It does not protect the backing store, and that half is the one worth
      writing down.** In separate-dir mode `-data` is an ordinary directory on an
      ordinary filesystem, reachable by path without going through the mount at
      all, so a setuid bit restored there from remote metadata is live however the
      mountpoint is flagged. The mount option narrows the blast radius; it does not
      discharge §10.4's rule that permission metadata from a remote source is
      executable trust and that setuid/setgid are masked by default.

   2. **Hard links are refused, with `EPERM`. ✅ Shipped.** No provider on the roadmap can
      represent them, and there is no benefit in inventing a representation: what a
      hard link buys — two names, one inode, one copy of the bytes — is precisely
      what a path-addressed remote cannot express. `link(2)` documents `EPERM` as
      "the filesystem containing oldpath and newpath does not support the creation
      of hard links", so `ln` and `cp -l` produce the right diagnostic with no
      special case. (rclone answers `ENOSYS`; `EPERM` is the more standard spelling
      of the same refusal.)

      **Refusing is strictly safer than what happens today.** `Link` is not
      overridden, so a hard link is created in the backing store with no event —
      and then, because both names are regular files, the M7b local walk pushes
      **both**, as two independent remote objects that diverge from each other from
      the first write. The user gets rclone's duplicate-copy outcome having first
      been told the link succeeded. Revisit if enough users ask for it; this
      decision is reversible in a way that silently-broken links are not.

   3. **Non-regular files are skipped, and the skip is logged once per path.
      ✅ Shipped.** Fifos, sockets and device nodes have no byte stream to sync, so the local file
      stays and the remote never learns of it — the behaviour the walk already has.
      What is added is *saying so*, at both sites that make the decision (the mount,
      when it declines to emit; the sweep, when it steps over one), plus a note in
      the man page. A cost the user cannot see is a cost they report as a bug.

   4. **POSIX mode, ownership and ACLs get two interchangeable channels, chosen per
      mount.** *(Not started; a milestone of its own.)* — the provider's own metadata where it has some, or a sidecar in the
      backing store. Both are needed and neither is a default for everyone: putting
      permissions into a cloud API is a disclosure some users will refuse outright,
      and a sidecar is a file in the tree that syncs like any other. This is §10
      moving from a design note to scheduled work, and it is what makes `setfacl` /
      `getfacl` survive a round trip.

      **Drive is the provider that shows why the sidecar cannot be dropped.**
      Drive's metadata surface is `btime`, `content-type`, `description`, `labels`,
      `mtime`, `owner`, `permissions`, `starred`, `viewed-by-me` and
      `writers-can-share` — no `mode`, no `uid`, no `gid`, no `rdev`. rclone's
      metadata framework hits the same wall: `-M` against Drive carries none of it.
      Anything drivel does here on Drive means `appProperties`, which is a
      deliberate use of a general-purpose key-value channel and exactly the
      disclosure the per-mount choice exists to let a user decline.

      **The record must be bound to what it describes, or it is not metadata.** A
      sidecar is the AppleDouble hazard drivel already refuses on macOS (§2.9.1): a
      file inside the backing tree describing another file, syncable and separable
      from it. A §6 conflict copy of one and not the other, a rename that moves one
      and not the other, or a partial sync is then enough to graft one file's ACL
      onto another file's bytes. So the record names the path it describes **and**
      carries a content digest, and one that does not match what it is attached to
      is discarded rather than applied. Fail closed, per §10.4: a permission that
      fails to restore is an inconvenience, and a permission restored onto the
      wrong bytes is the failure this entry exists to prevent.

      **This partially reverses §11's build-tag gate, deliberately.** §11 keeps
      virtual xattrs out of release builds because a virtual store turns synced
      content into filesystem metadata with no boundary anywhere. The difference
      here is that the channel is explicit, opt-in per mount, bound to its subject,
      and refuses `user.drivel.*` and every privileged namespace at the seam in both
      directions — which are the conditions §11.3 asks for. §11 itself stays gated;
      it does not inherit an exemption by resembling this.

   5. **Symlinks are deferred, and the reasoning is recorded so it is not
      re-derived.** They are the one case with real user demand and no clean answer,
      and the work belongs after the multi-client hardening rather than before it.

      The attractive idea is to represent a symlink as a small text file with a
      human-readable header, so a user meeting it in the Drive web UI is told what
      it is and where it points — a real improvement on rclone's bare `.rclonelink`
      payload. **The body is right and the identity is wrong.** Deciding "this is a
      symlink" from the *content* fails three ways, and the first is structural
      rather than a matter of care:

      - **It breaks enumeration.** The M7b sweep is one flat listing, roughly one
        request per 1000 objects, with **no content transferred**. If symlink-ness
        lives in the bytes, classifying the tree means downloading every small file
        in it. Under `-lazy` it is worse: a remote pointer file materialises as a
        placeholder, so `readlink(2)` cannot be answered without a fetch, and
        knowing to always-hydrate it in full (the §6 conflict-copy rule) requires
        knowing it is a symlink before fetching it. Circular.
      - **Content-as-identity is a symlink injection channel.** Anything that can
        write a *text file* — the web UI, a phone client, another sync tool — can
        make every other client create a symlink at that path, pointing anywhere.
        The converse loses data: a legitimate text file matching the header is
        restored as a link and its content is gone.
      - **The header becomes a permanent compatibility contract**, and one a user
        can edit in a web UI.

      Every production system that does this uses **two** signals, and the
      out-of-band one is primary because it is the cheap filter: Cygwin marks its
      `!<symlink>` files with the DOS System attribute and only opens files
      carrying it, and Git LFS pairs a strict pointer format with a `.gitattributes`
      declaration of which paths are pointers. The shape for drivel is therefore the
      marker in provider metadata where the provider has some (on Drive,
      `appProperties` — invisible in the web UI, surviving a rename, unforgeable
      from the content channel), a name suffix where it does not, and the
      human-readable body as both payload and fallback parser. That is the same
      capability decision 4 needs, so the two ship together or not at all.

      Two guards are not optional whatever the format, both borrowed from rclone:
      **never write through a link arriving from a remote**, and refuse targets that
      escape the mount root in either direction. `filepath.WalkDir` does not follow
      symlinks, so the sweep is already safe — keep it that way. Reading
      `.rclonelink` on materialisation costs almost nothing and makes an existing
      rclone tree work, which is worth doing when the rest lands.

   **Sequencing.** 1–3 were small, independent of everything else, and belonged
   *before* the next Tier B round rather than after it: they change what a fleet
   does with a file it cannot represent, which is a thing the fleet tests observe.
   They landed on 2026-09-08, so MC-13 is now answered for everything except
   metadata, and the Tier B rounds can be read against the behaviour above. 4 is a
   milestone of its own and waits on the provider-metadata capability. 5 waits on
   both.

### M0 — Test & CI (cross-cutting, always open)

Not a numbered milestone: it has no completion date and nothing waits on it. It is
listed here because the test suite is load-bearing and was, until now, tracked
nowhere.

**Where it stands.** Every package has tests, green under
`go test -race ./...`. The invariants whose failure mode is *data loss*
rather than inconvenience each have a dedicated file: `syncengine/lazy_test.go`
(M5, never push a placeholder), `rangewrite_test.go` (M6's three gates, especially
"decline on a diverged remote"), `conflict_test.go` (§6), `reconcile_test.go`
(M7b's four delete guards), and `pathindex` + `gdrive/index_test.go` (M7
verify-before-believe). Those are the tests to be most reluctant to weaken.

**A test that samples a live tree must tolerate it changing** — one flake, found in
M10 and worth writing down because the diagnosis is not the obvious one.
`TestFleetPropagatesABulkDelete` failed about two runs in three, and the failure was
never in the product: `peer.manifest` and `peer.conflicts` walk a peer's backing
directory *while the pull loop is deleting from it*, so `os.ReadFile` could hit a
path that the readdir had listed a moment earlier, and the helper turned that into
`t.Fatalf`. They are samplers inside a poll loop, so an entry disappearing mid-walk
is what "not converged yet" looks like from outside; both now skip `fs.ErrNotExist`
and fail on anything else. The general form: a helper that reads a tree the code
under test is mutating has to distinguish "this is the race I am waiting out" from
"this store is broken", and the default of treating every error as the second is
what makes an intermittent red that nobody can reproduce on demand.

**Done.** *No test may silently not run.* The M5 xattr tests and the real-FUSE
mount test skip when the machine lacks user xattrs or `/dev/fuse` — correct on a
laptop, dangerous anywhere automated, because a runner missing both reports green
while the data-loss guards never execute. `internal/testenv` turns those skips
into failures for any facility named in `DRIVEL_REQUIRE_TESTENV` (`fuse`, `xattr`,
or `all`), which is what CI must set.

**The list, in order of what a regression would cost.** Every item is now struck
and kept here with what it taught; 1 landed before M8, 2 and 4 during it, 3, 5 and
6 after it. What the list does not cover, and no local item could, is the
authorization-code exchange and the Drive calls themselves — those are Tier B's
job (`docs/dev/multiclient-test-plan.md`), which is where the live testing work now
is:

1. ~~**CI.**~~ ✅ Done. Every gate is a `make` target that the git hooks and the
   GitHub Actions workflow both call, so "passed locally" and "passed in CI" cannot
   mean different things. The test job sets `DRIVEL_REQUIRE_TESTENV=all` and
   installs `fuse3`, without which it would buy much less than it appears to.
2. ~~**`cmd/drivel` is untested.**~~ ✅ Done in M8, which needed the same refactor:
   the wiring moved to `internal/app`, and `cmd/drivel` keeps flag parsing and the
   flag-to-spec mapping, which is what the tests cover.
3. ~~**Property tests for `internal/ranges`.**~~ ✅ Done. `ranges/property_test.go`
   and `syncengine/coalescer_property_test.go` state the invariants against
   generated op sequences over a fixed seed range, so the suite is deterministic
   and a failure prints the seed and the spans that produced it. Two things are
   worth keeping.

   **The oracle must not share the implementation's shape.** The properties are
   stated as relations to the *input* — marked bytes ⊆ the spans Mark was given,
   marked bytes ⊇ the spans MarkCovering was given, marked bytes ⊆ the blocks
   those spans touch — checked against a one-bool-per-byte model. An oracle built
   out of `clamp` and `blockSpan` would have agreed with a broken `Set` for
   exactly the wrong reason. The soundness half also needs its liveness partner
   or it passes vacuously: a set told about every byte must report `Complete`.

   **They were checked by breaking the code.** `Mark` rounded outward, then
   `MarkCovering` inward, then `Union` made to ignore a grid mismatch; then the
   coalescer made to alias the caller's set, to let a known event revive a
   poisoned accumulator, and to swallow `Union`'s refusal. Each mutation failed
   the property that names it, and nothing else. A property test nobody has seen
   fail is a property test nobody knows the strength of.
4. ~~**One end-to-end test.**~~ ✅ Done in M8: `internal/app` threads mount → write
   → push → pull → reconcile through one in-memory provider. It taught two things
   worth keeping. The downloader writes *below* FUSE rather than through it, so a
   pull generates no mount event at all — the loop §4 breaks is our own push coming
   back down, not a pull going back up. And for identical content the three loop
   breakers (the echo record, `apply`'s on-disk hash comparison, M6's
   unchanged-content gate) are redundant by design, so no end-to-end assertion can
   isolate one: deleting the echo write leaves every observable unchanged. The test
   asserts what it can show — that they compose into a system which settles rather
   than ping-pongs, and that no conflict copy appears where both sides agree — and
   leaves isolating the gates to the unit tests that can.
5. ~~**`gauth` at 18%.**~~ ✅ Done — 84%, and it found two defects rather than
   merely covering the code.

   **Neither writer could tighten a file that already existed.** `os.WriteFile`
   and `os.OpenFile` apply their mode only when they *create* the file, so a
   `token.json` or `credentials.json` restored from a backup, copied between
   machines, or written under a permissive umask kept its mode through every
   rewrite — a refresh token for the user's whole Drive, left readable by every
   local account, by a function whose doc comment said 0600. Both now go through
   `writeSecret`, which narrows through the descriptor (no path race) on every
   write.

   **`Login` closed the loopback server out from under its own handler.** The
   handler writes the page the user is looking at and *then* signals; the signal
   returned `Login`, whose deferred `srv.Close()` cut the connection before the
   response was flushed. On the denied and state-mismatch paths — exactly where
   the user needs to be told what happened — the browser could show a transport
   error instead. It is a graceful `Shutdown` now, on a detached context because
   cancellation is one of the ways we get there.

   The interactive half is still not harnessed, but less of it is interactive than
   it looked: the loopback server (CSRF state check, denial reporting, the 404 for
   a stray `/favicon.ico`, a busy port, a blank pasted line) is driven directly
   over `127.0.0.1`, and `WhoAmI` runs against an `httptest` server because
   oauth2 takes its base client from the context — the same seam `gdrive` uses to
   put HTTP/3 underneath the token source. Only the code exchange itself needs
   Google.
6. ~~**`internal/fsevent`** has no tests.~~ ✅ Done, and the exercise was deciding
   what is even assertable about a package of types. Three things, each load-
   bearing elsewhere: the `Op` strings are the log format (`[sync] %-7s path`)
   that the multi-client rig's counters are grepped out of, so renaming one or
   adding a longer one silently changes every measurement taken from a log;
   `Dirty` must stay a *pointer*, because the fail-safe default is carried by the
   type — as a plain `ranges.Set` the zero value would read as "nothing changed"
   and push no bytes at all; and `Dirty` is shared by pointer with whoever
   produced it, which is why `vfs`'s tracker hands over a `Clone`.

---

## 10. POSIX metadata on Drive (design note, unscheduled)

Can we carry mode bits, POSIX ACLs, extended attributes, and SELinux contexts
alongside the content? Yes — but "securely" splits into three separate problems,
and only one of them is about where the bytes live.

### 10.1 Where Drive can hold it

| Mechanism | Size budget | Who can read it | Travels to a collaborator? |
|---|---|---|---|
| `appProperties` | 30 keys/file/app, **124 bytes per key+value pair** (UTF-8, combined) → ~3 KB ceiling | Only requests bearing *our* OAuth client ID | **No** (see below) |
| `properties` | 30 public keys/file, same 124-byte rule (100 properties/file total from all sources) | Every app with file access | Yes |
| `appDataFolder` space | Effectively unbounded | Only our client ID | **No** — it is per-user, per-app |
| Sidecar object next to content | Unbounded | Anyone with file access | Yes |

The 124-byte-per-property rule is the binding constraint: a 12-byte key leaves
112 bytes of value. A full ACL + xattr set + an SELinux context does not fit in
one property, so anything rich needs chunking across keys or a sidecar.

**The BYO-credentials consequence.** Drivel deliberately has every user create
their own Cloud project and OAuth client (see `docs/user/google-cloud-setup.md`).
`appProperties` are private to the *requesting app*, keyed by OAuth client ID —
so two Drivel users sharing a Drive file each write metadata the other cannot
see. `appProperties` and `appDataFolder` therefore work for **one user across
many machines** (the main use case) and are useless for **shared content**. Only
`properties` or a sidecar object crosses the user boundary, and both are
world-readable to anyone with file access. Pick deliberately; don't assume the
single-user design generalises.

### 10.2 Why "secure" is the wrong first question

Metadata that describes permissions is not data — it is *executable trust*. If
Drivel reads `mode=04755, owner=root` from Drive and applies it, then whoever can
write that metadata can escalate privilege on every machine that syncs. The
write-capable set includes anyone the file is shared with, anyone holding the
OAuth token, and anyone who compromises the Google account. Confidentiality is
the least interesting of the three properties:

- **Integrity** — a tampered blob must be *detected and rejected*, not applied.
- **Binding** — the blob must be cryptographically tied to the file and content
  revision it describes, or an attacker moves a permissive blob onto a sensitive
  file, or replays an old one after permissions are tightened.
- **Confidentiality** — SELinux contexts and ACLs leak usernames, group names,
  roles, and the shape of the local security policy. Real, but secondary.

### 10.3 If it gets built

- **One AEAD, not a hash plus a cipher.** XChaCha20-Poly1305 (or AES-256-GCM)
  gives integrity and confidentiality in one primitive.
- **Key stays local.** A per-vault master secret in `~/.config/drivel/vault.key`
  (0600), never uploaded, provisioned to the user's other machines out of band;
  HKDF per-file subkeys. A key that is synced through Drive protects nothing from
  an attacker who has Drive.
- **AAD = fileID ‖ content hash ‖ schema version.** This is what buys binding; it
  is not optional decoration.
- **Fail closed.** A blob that does not authenticate is dropped with a log line
  and the file gets default ownership and mode. Never partially apply a metadata
  record — half an ACL is a hole.
- **Layout.** gzip → AEAD → base64url; chunk across `drivel.md.0..N`
  `appProperties` while it fits, else write a sidecar and keep only the digest in
  a property (the digest still needs the AEAD binding).

### 10.4 Policy rules that outrank the crypto

Even perfectly authenticated metadata should not be applied verbatim.

- **Never sync uid/gid numerically.** UID 1000 is a different person on each host.
  Store names, resolve on apply, fall back to the mounting user when unresolvable.
- **Mask setuid/setgid/sticky by default.** Require an explicit opt-in flag to
  honour them. This single rule removes most of the privilege-escalation surface.
- **Treat the `security.*` xattr namespace as hostile.** That is where
  `security.capability` (file capabilities), `security.selinux`, and IMA/EVM
  signatures live — the namespaces the *kernel* treats as authoritative. Default
  to round-tripping them opaquely (store and restore only to the same host
  identity) rather than applying them cross-host.
- **SELinux contexts are restore-only.** Apply only when the local policy already
  admits the context, the way `tar --selinux` behaves; a valid-but-wrong context
  is a policy hole, not an error.
- **Recompute POSIX ACLs, don't copy raw bytes.** The on-disk
  `system.posix_acl_access` encoding is host-endian and uid/gid-numeric.
- **Store per-namespace and degrade.** Apply what the host supports, keep the rest
  opaque so a Linux→macOS→Linux round trip does not silently drop attributes.

None of this is novel — it is the threat model `rsync -AX` and `bsdtar` already
live with. The difference is that Drivel's metadata channel is writable by a
remote party, which `tar` archives generally are not.

---

## 11. Virtual xattrs (design note, unscheduled, build-time opt-in)

**The idea.** Emulate extended attributes for the mount instead of proxying them:
keep the attributes in a file that lives *in the backing store*, so they are stored
and synced like any other content, and serve `getxattr`/`setxattr`/`listxattr`/
`removexattr` at the FUSE layer out of that. Two things fall out of it that a
passthrough cannot do. A backing filesystem that cannot store `user.*` attributes
at all — exFAT, an SMB share, a `/mnt/c` drvfs path under WSL2, most network mounts
— would still present working xattrs at the mountpoint. And because the store is
just a file in the tree, the attributes travel: set one here, see it on the other
machine after a sync, which is §10's "POSIX metadata over Drive" arriving through
the data plane rather than through provider-specific metadata.

**It is off unless the build says otherwise.** A build tag (`drivel_virtual_xattr`)
that is absent from every release build, compiling to a package whose default form
is a no-op — not a runtime flag alone, and not a flag defaulting to false. The
reason for reaching for the compiler is §11.2: several of the failure modes are
not "a user turned on something risky", they are "this binary can be made to apply
attacker-supplied metadata", and the cheapest sound answer to that is that the code
is not in the binary. Assume a runtime flag on top, since a build that *can* do it
should still not do it by default.

### 11.1 Where the attributes would live

Two layouts, and the choice is the whole design.

- **One store per mount** — a single file (bbolt, or an append-only log) in the
  backing tree, mapping path → attribute set. Compact, one object, no per-file
  overhead, and a directory listing costs nothing extra. But it is a single
  synced object mutated by every client, so it collides constantly: §6's
  last-writer-wins policy on it means one client's conflict copy silently loses
  *every* attribute another client set, not just the contested one. It is also
  exactly the shape M8's `validate.go` refuses for the state DB — a database inside
  a backing tree whose own writes generate the events that cause more writes — so
  it would need the same event suppression the state DB gets by living outside.
- **One sidecar per file** — `f.txt` carries `.f.txt.drivel-xattr` beside it, or a
  parallel `.drivel-xattr/` tree. Conflicts stay per file and merge the way content
  does; the loop problem is bounded (a sidecar write is one more event, not a
  rewrite of a shared index). The costs are real too: it doubles the object count
  in the provider (Drive counts files, and both quota and API cost follow), rename
  and delete must move or remove two objects atomically-enough, and in in-place
  mode the sidecars are visible in the user's own directory. Hiding them from
  `readdir` makes them invisible locally and still visible in the Drive web UI,
  which is its own kind of lie.

Whichever wins, `rename` is the operation that decides whether it works: attributes
have to follow a file across a rename and a cross-directory move, and a crash
between the two writes has to leave a state the next mount can name.

### 11.2 Why this is a build-time decision and not a preference

The security argument is not that xattrs are dangerous. It is that a virtual store
turns *synced file content* into *filesystem metadata*, which reverses the trust
direction of everything in §10 — and does it one layer lower, where the checks in
§10.4 do not exist yet.

1. **It re-opens M5's data-loss path, and adds a remote actor.** §2.1 refuses xattr
   passthrough because `user.drivel.placeholder` is drivel's own control metadata
   and passthrough makes it forgeable by anything that can write to the mount. A
   virtual store makes it forgeable *by anything that can write to the synced
   store*, which includes every other client and anyone the Drive folder is shared
   with: injecting a placeholder marker into someone else's mount makes their next
   read overwrite local content, and stripping one makes their next push upload
   zeros over the remote file. So the drivel namespace can never be served from,
   or accepted into, the virtual store — the real marker stays a real xattr (or the
   state DB where there are none), and `user.drivel.*` is refused at the boundary
   in both directions. That rule is load-bearing and belongs in code, not in docs.
2. **The privileged namespaces must not be storable at all.** `security.capability`,
   `security.selinux`, IMA/EVM signatures and `system.posix_acl_access` are
   namespaces the *kernel* treats as authoritative (§10.4). A virtual store that
   accepted them would let a synced file carry file capabilities or an SELinux
   label onto every machine that mounts the tree, with no signature and no
   provenance — remote code execution's boring cousin. `user.*` only, and even
   then the kernel's own rules (`security.*` needs privilege, `trusted.*` needs
   `CAP_SYS_ADMIN`) have no analogue in a plain file: whatever mode bits the store
   carries *are* the permission model, and they are weaker than the real one.
3. **It is a metadata channel with no size limit and no accounting.** Real xattrs
   are bounded (Linux: typically 64 KiB per attribute and often one filesystem
   block for all of them). A file-backed store has whatever bound we invent, and
   anything unbounded here is a covert channel that syncs — a place to park data
   that does not appear in any file's size.
4. **Two sources of truth for the same question.** `getfattr` on the backing file
   and `getfattr` on the mountpoint would disagree by construction. Every tool that
   copies a tree (`rsync -X`, `cp -a`, `tar --xattrs`, a backup agent) reads one of
   them, and which one it read is invisible in the result.

### 11.3 What would have to be settled first

- Whether the virtual store is served **only** when the backing filesystem cannot
  do the real thing (a fallback, matching `XattrsUsable`), or always when built and
  enabled (a feature). The fallback framing is much easier to defend and much
  harder to reason about: the same tree then behaves differently on two machines.
- Whether attributes sync at all, or stay local. Local-only removes most of §11.2
  and most of the point.
- How it interacts with §10 if §10 is ever built: two mechanisms carrying the same
  metadata over the same provider, with different trust models, is one too many.
- What `listxattr` reports for a file whose sidecar has not been hydrated yet in
  lazy mode. Faulting content in to answer a metadata question would make `ls -l`
  with a `getfattr` in the loop download the tree.
