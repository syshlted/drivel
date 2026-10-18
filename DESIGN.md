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
- Multi-provider abstraction beyond a thin interface. Drive was the only backend
  when this was written; the instruction that came with it — don't hard-code
  Drive into the FS layer — is the one that mattered.
- Conflict *resolution* UI. v1 uses a deterministic policy (last-writer-wins by
  mtime, with conflict copies) and logs conflicts.
- Real-time collaborative editing semantics.

**This section is v1's scope, kept as it was written.** Read it as the aim, not
as the current state: v2 has since built most of what the second non-goal ruled
out. Two backends ship and neither is hard-coded anywhere above the seam (§2.5),
each runs in its own process (§2.10), and one process serves several accounts at
once (§2.8). The thin interface is why that cost no change to the FS layer,
which is the part this section got right.

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
                                 │ provider.Store (§2.5)
                                 │
                          ┌──────▼────────────────────┐
                          │  Plugin client (§2.10)     │  gRPC over a unix socket
                          └──────┬────────────────────┘  (hashicorp/go-plugin)
          ═══════════════════════╪═══════════════════════ process boundary (M9)
          ┌────────────────────  │  ─────────────────────────────────────────┐
          │  Backend process     ▼        one per mount                       │
          │               ┌──────────────┐                                    │
          │               │  Provider    │  Google Drive API v3               │
          │               │  (Drive)     │  - files.* (CRUD)                  │
          │               └──────┬───────┘  - changes.list (pull cursor)      │
          │                      │ injected *http.Client                      │
          │               ┌──────▼───────────────────┐                        │
          │               │  Transport (§2.6)         │  HTTP/3 preferred,    │
          │               │  HTTP/3 → HTTP/2 fallback  │  HTTP/2 if UDP is    │
          │               └───────────────────────────┘  blocked; OAuth wraps │
          └──────────────────────────────────────────────────────────────────┘
```

Since M9 the provider is the one component that is not in the process. The seam it
sits behind is unchanged — everything above it holds a `provider.Store` and does not
know whether the implementation is a Go value or another program — but the backend
itself, and every dependency it drags in (the Drive SDK, OAuth, QUIC, the SSH stack),
lives in an executable drivel launches. §2.10 has the mechanism and what it costs.

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

The optional interfaces are all *accelerators that may decline*. Every one of
them has a correct, slower answer available if the provider omits it or a call
fails, and the engine is written so that "unsure" always selects that answer.
`Enumerator` is the one whose absence costs a *feature* rather than speed — without
it a pre-existing remote tree stays invisible (§9, M7b) — but its absence is still
safe, because nothing infers anything from a sweep that never ran.

Two were added with configurable conflict handling (2026-10-18) and follow the
same rule. `Creator` is `Put` refusing an occupied path, so a first push learns
that the remote already holds something at no cost on a store whose `Put` already
resolves the path. Without it the engine pays a `Stat`. `Trasher` is a removal
the user can undo whatever `Remove` is configured to do. Its absence costs one
setting, `conflicts = "remote-trash"`, which is refused at open rather than
quietly given a permanent deletion.

**Which of them a store offers is asked, not assumed** (M9). Until the backend could
live in another process the question was a Go type assertion, which is exact for a
value whose method set you can see. A plugin's host-side proxy is one type serving
every backend, so it has all five methods regardless — and type assertion would have
answered "yes" five times for every plugin. So the question moved into the seam:

```go
// A store may declare what it offers, instead of leaving the answer to its methods.
type Declarer interface{ Capabilities() CapabilitySet }

// Capabilities returns the INTERSECTION of the method set and the declaration.
func Capabilities(s Store) CapabilitySet

// Callers ask through these rather than asserting.
func AsChangeSource(s Store) (ChangeSource, bool)   // …and four siblings
```

The invariant is one line and it is what makes the mechanism safe: **a declaration
narrows and can never widen.** A store cannot talk its way into a capability it has
no method for, so the worst a wrong declaration can do is cost a feature — never
produce a call into a method that is not there. An ordinary in-process backend
implements exactly what it can honour, which is already an exact answer, and does
not implement `Declarer`; `gdrive` and `sftp` do not, and should not start.

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

**Remove semantics — the same class of capability difference, resolved towards the
recoverable answer.** The seam says `Remove(ctx, path) error` and nothing about
what becomes of the bytes, because providers genuinely differ: Drive has a trash,
S3 has versioning or nothing depending on the bucket, a local CAS backend (M11)
has whatever its GC policy says. Drive's implementation therefore owns the choice,
and takes the trash by default (`files.update` with `trashed = true`), with
`delete = "permanent"` restoring the outright `files.delete` that M1–M16 always
did.

The default is the recoverable one for a reason that is about drivel rather than
about Drive: **not every deletion the engine performs was typed by a user.** A
reconcile pass *infers* deletions from a baseline (§9, M7b), and every input that
makes that inference wrong — a state DB reused against a different `-drive-root`,
a `-data` directory that came up empty, a mount pointing somewhere new — is an
accident rather than a corruption, which means the guards can only make the pass
refuse, never make it right. Everything else in the tree fails towards keeping
bytes: M5 refuses to push a placeholder, M6 falls back to a whole-file `Put`, M7's
index is verified before it is believed, M7b abandons a delete pass it cannot
justify. Remove was the one operation with no such fallback, and the provider had
one available for free.

It is nonetheless a real removal, and deliberately indistinguishable from one
above the seam. Every query `gdrive` issues carries `trashed = false`, so a
trashed object stops resolving, stops appearing in a sweep, and cannot be revived
by a stale index hint (`verifyLocked` already rejects `Trashed`); `changes()`
already reads a trashed file as `Removed`, so the fleet applies it as the deletion
it is; and Drive's `trashed` means "explicitly, or from a trashed parent folder",
so trashing a directory carries the subtree exactly as the recursive delete did —
**asynchronously**, which a live run against real Drive found and no fake would
have (a child read `trashed = false` immediately after its parent was trashed, and
inherited the flag shortly after). Nothing depends on that timing: `rm -rf`
unlinks the children first, so each is trashed in its own right, and a sweep that
raced the propagation would park a still-listed child on a parent the listing no
longer holds and drop it as outside the mount.
What differs is one thing only: the bytes are somewhere a user can reach them for
30 days. What it costs is also one thing: they still count against the account's
quota until the trash is emptied, which is why the permanent mode exists rather
than being an option nobody would pick.

**Reporting a move *inbound* is the seam's hardest case, and it is the provider's
job.** A provider whose native feed is keyed by object identity (Drive's
`changes.list` is: one entry per fileID, carrying that object's metadata *now*) is
never told the old path by the API, and never says it either unless it works it
out from its own path↔ID mapping. Left unsaid, a rename on one client duplicates
the file on every other, because the new path is downloaded and nothing ever
removes the old one — until an enumeration sweep infers the delete, up to
`-sweep-interval` later. `gdrive.vacatedPathLocked` is where that is worked out,
and **only the in-memory mapping is consulted, never the persistent index**:
acting on it here means moving or deleting local files, and M7's first rule is
that a persisted entry is a hint to be verified before it is believed. A move the
in-memory map cannot account for falls back to the sweep, which is slower and
never wrong.

**What the seam says about it is two shapes, and which one a provider uses depends
on what moved.** A *file* is reported as a removal of the old path followed by an
addition of the new one; `RemoteChange`'s original `{Path, File, Removed}` can
express that and nothing else, and for a file both forms converge. A *directory*
cannot be: a removal above the seam is a recursive local delete, and this feed
reports nothing at all for the children of a moved folder — their own metadata did
not change — so the subtree would be deleted locally with nothing to bring it
back. Until MC-27 the answer was therefore to say nothing about a moved folder and
leave it to the sweep, as the lesser failure.

**MC-27 measured that choice at 1100 files and it is not the lesser failure.** The
renaming client pays one request. Every other client is told only that an empty
directory appeared under the new name, keeps the entire old subtree as orphans
present nowhere remotely, and then has its repair sweep download all 1100 files a
second time *and* offer 1101 deletions — which `-max-deletes` refuses, abandoning
the whole pass, with `reconcile complete` in the log and (per MC-25) nothing
scheduled to retry. **Renaming a directory of more than ~100 files doubled that
subtree on every other client permanently.** It is the only case on record where
two individually correct guards compose into a stuck state.

So `RemoteChange` carries a fourth field, **`MovedFrom`**, and a directory move is
reported as a move. Five things about it.

- **It is strictly additive, in both skew directions.** It is only ever set where
  the change already describes the object at its new path, so a host that ignores
  the field is left doing precisely what every host did before it existed — place
  the object and leave the old path to the sweep. That is why the protocol version
  does not move (§9, M9: adding a protobuf field is the mechanism for anything
  additive) and why a *file* move keeps the pair rather than being converted: the
  pair is what an older host needs in order to converge at all.
- **A provider may set it only for a move of the same object**, content unchanged,
  because a host carries the §4 baseline across it. A provider whose `Move` is a
  copy-and-delete must leave it empty — identity is exactly what such a move does
  not preserve.
- **It is advice, never an instruction.** The host validates the paths, declines
  for any reason it likes, and falls back to the ordinary apply. `moveLocally`
  declines an unclean or escaping path, either path inside the other, a source
  that is absent locally, and — the one that protects data — **an occupied
  destination**, because `os.Rename` replaces a file silently and what it would
  replace may be local work nobody has pushed. That is §6's question to answer
  with a conflict copy, not a move's to answer by overwriting.
- **The records move with the object, in one commit, on both sides.**
  `state.RenameSubtree` re-keys the echo, hydration and repair buckets for the
  whole subtree; tombstones and seen marks deliberately stay put. A baseline is
  not a cache — it is §4's loop breaker, M6's gate, M7b's delete evidence and
  M27's divergence test — so dropping one costs a re-download on the receiving
  side and a delete candidate on the sending side, which is the refusal threshold
  above 100 files.
- **The sending side needed the same fix and had nothing to do with the
  protocol.** An outbound rename dropped the record for the renamed path only, so
  the renaming client's own next sweep found every descendant's baseline keyed
  under a path that no longer existed and proposed exactly the same 1101
  deletions. `Engine.renameRecords` re-keys instead of dropping, at the same
  commit count.

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

**The cache expires, and MC-44 is why** (measured 2026-10-13, UDP/443 dropped for a
running process by a cgroup-scoped nftables rule and then restored). Two results.

The fallback is better than the case assumed: it fires on the **very next request**
— last HTTP/3 response at 18.9s, the rule added, the request at 21.1s carried over
HTTP/2 in 203ms, with no error reaching the caller and nothing retried above the
transport. So "under load" has the same answer as "at startup", and the per-host
memory is doing its job. One caveat on that 203ms, because it is a property of how
the block was made rather than of the transport: an output-hook `drop` is reported
to the local sender as `EPERM`, so the write fails immediately.

**The blackhole shape was then run, and it has two different answers** (2026-10-13,
two network namespaces joined by a veth with the drop on the far side, so the
client's packets leave normally and nothing ever replies). The fallback still
works, still surfaces no error and still loses no request — but *how long* it takes
depends on whether a QUIC connection already existed, and the two numbers are six
times apart:

- **A block arriving mid-session costs 30.1s.** The request in flight is on an
  established connection, and nothing tells quic-go the peer has stopped hearing
  it, so it retransmits until the idle timer fires. That timer is `MaxIdleTimeout`,
  which this transport had left to quic-go's `protocol.DefaultIdleTimeout` of 30s.
  One request stalled; the other 130 in the run were unaffected.
- **A block already in place at startup costs 5.0s**, which is
  `HandshakeIdleTimeout`, and so does every `h3Retry` re-probe afterwards, because
  by then the connection has been torn down and the attempt is a fresh dial. A
  second run held the blackhole for the whole 320s and measured exactly that: 5.02s
  at startup, 5.01s for the one re-probe, and 0ms for the other 101 requests — 1.7%
  of the wall clock at a 3s cadence, which is what "a cost ceiling" means in
  numbers.

This section claimed the handshake number for both shapes until the run. The
timeouts are now named constants here (`h3HandshakeIdle`, `h3MaxIdle`,
`h3KeepAlive`) with `TestNewPinsTheQUICTimeouts` asserting them: `h3MaxIdle` is set
to the 30s that was already in force, so nothing changes behaviourally, but the
fallback latency of a transport §2.6 makes a requirement is now a number in the
tree rather than a dependency's default. Two things govern changing it. The
effective timeout is `min(ours, the peer's advertised max_idle_timeout)`, so
raising it may do nothing while lowering it always bites; and lowering it costs
keep-alive packets on every idle mount, since quic-go derives the ping interval as
`min(KeepAlivePeriod, idleTimeout/2)` — 15s today, which makes `h3KeepAlive` a
ceiling on the rate rather than the rate. 30s stands: it is one stalled request per
episode, off the FUSE path by §2.1, against a permanent cost on every mount that
is working fine.

The restore was worse: **nothing brought HTTP/3 back.** `down` was a
`map[string]bool`, written on the first failure and never cleared, so all 15
requests in the 29 seconds after the rule came off stayed on HTTP/2 — and would
have for the life of the process. A mount runs for weeks, usually from
`/etc/fstab`, so a forty-second blip was permanently defeating the requirement at
the top of this section — and silently, since HTTP/2 works.

So the map records *when* a host was last found unusable, and `isDown` treats a
mark older than `h3Retry` (5 minutes) as lapsed. Three things about it. **Only a
failure re-arms the window**, which is what makes recovery need no second
mechanism: once a mark lapses it stops being consulted, a successful probe leaves
it lapsed, and nothing writes again until QUIC breaks. A "clear the mark on
success" path was written and then deleted, because a mutation removing it changed
no behaviour — it was an assertion that could not fail. **The window is a cost
ceiling rather than an estimate** of how long a network stays broken: the re-probe
costs at most one failed dial per window per host — `h3HandshakeIdle`, the 5.0s
above and not the 30s, since a re-probe has no connection to lose — nothing at all
when the block is local, and no FUSE
operation is behind it either way because §2.1 keeps the network off that path —
the worst case delays a sync the engine would have retried. **`now` is injectable**,
so the window is tested by advancing a clock rather than by sleeping through it.
Verified live at a 20-second window: HTTP/3 returned on the first request past the
re-arm, 12.5s after the restore. The blackhole run then verified it at the window
that ships: mark written at t=45.1s, the block lifted at t=330.0s, and HTTP/3 back
at t=345.4s — the first request after 45.1 + 300, so 15.4s after the restore
because that is where in the window the lift happened to land. **Recovery latency
is therefore "the rest of the current window", up to five minutes, and not a
function of when the network came back** — which is worth knowing before reading a
run as the fix not working.

**The comparison MC-44 was written for then ran** (2026-10-13, five alternating
HTTP/2 and HTTP/3 pairs of a 64 MiB upload and a 64 MiB download through this
provider's `Put`/`Get` against the live rig; `scripts/multiclient/mc44-throughput.sh`).
The protocol is *attributed* rather than assumed, because there is no knob that
selects one: UDP/443 is dropped for the run's cgroup to force the fallback, and
nftables counters on the output hook say what actually carried the bytes — every
HTTP/2 run read 0.0 MiB on udp443 and ~66 MiB on tcp443, every HTTP/3 run exactly
the reverse. The answer is in two halves and only one of them is interesting.

**Throughput is indistinguishable, because Drive is the limiter.** Upload came out
at a median 4.16 MiB/s on HTTP/2 against 4.07 on HTTP/3, with per-run ranges of
4.07–4.28 and 3.72–4.18 — overlapping, and 64 MiB took 15–17s on every single run
of either protocol. Download medians were 33.46 against 30.83 MiB/s, but the
per-run spread was 25.50–53.98 and 14.80–43.31: it varies **twofold between
consecutive runs of one protocol**, so that column measures what Drive was doing
that minute and not what the transport costs. A single sample of each would have
read as a clean 2:1 win for whichever one got lucky, which is why the harness
alternates and reports a median of five.

**CPU is where they differ, and they differ by an order of magnitude.** Upload cost
2.00 ms/MiB on HTTP/2 against **20.50 on HTTP/3**, download 6.10 against 13.10 —
and unlike the throughput columns these do not overlap at all across the ten runs
(1.9–2.0 against 20.2–22.8; 6.1–6.9 against 12.6–15.0). The download figures
include the MD5 the probe computes while streaming, which is a constant common to
both, so the *difference* — 18.5 ms/MiB up, 7.0 down — is the protocol's and the
ratio understates it. In absolute terms it is 1.3s of CPU to upload 64 MiB, about
8% of one core at the 4 MiB/s Drive allows, against 0.8% for HTTP/2. That is the
price of doing in userspace what the kernel does for TCP, measured rather than
assumed, and it is a per-client cost that scales with transfer volume.

**This prices the requirement at the top of this section; it does not overturn
it.** The rationale there is connection reuse, 0-RTT resumption and the absence of
head-of-line blocking across concurrent transfers — claims about a frequent poll
loop and many small requests, which a two-stream bulk transfer does not test in
either direction. What the run does establish is that there is no bulk-throughput
*advantage* to point at on this path, so anything defending HTTP/3 has to defend it
on the poll loop. One honest limit: 4 MiB/s of upload is slow enough to hide a
protocol difference that a faster path might expose, and this is one container on
one network.

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
shutdown ordering inside `Run` is load-bearing: unmount (which flushes pending FUSE
events), then wait for the inbound loop, then `close(events)`, then wait for the
engine's bounded drain, with the engine on a detached context (§7). `App`
aggregates; it must never replace that sequence with one cancellation.

The inbound wait was added after the Group V work (`docs/dev/multiclient-test-plan.md`
§5.6) and is a correctness fix rather than tidiness. `Run` used to return once the
engine had drained, leaving `down.Run` to notice the cancelled context on its own —
so `Close` closed the state DB under a live sweep, which abandoned the delete pass
and skipped `FinishSweep`, parking a sweep that had in fact finished. The version
that would have cost data never fired: in in-place mode the backing path is
`/proc/self/fd/N`, and once `Close` has closed that descriptor its number is
available for reuse, so a download still writing through it writes into whatever
took the number. Both waits are bounded; the inbound one is the shorter of the two,
because an unfinished sweep is resumed and an unfinished download is retried, while
the engine is holding uploads nothing else will re-offer. `Run` also *cancels* the
inbound loop itself, on a context of its own derived from the caller's, rather than
waiting on the caller's: `Serve` returns on a mount that never came up as well as on
an unmount, and in that first case the caller's context is still live — so a loop
waiting on it alone would still be polling when `Close` arrives, which is the failure
this paragraph describes reached by the shorter route.

**The registry is an explicit value, not a package-level map filled by `init()`.**
The `database/sql` shape would introduce the only process-global mutable state in
the tree — in the very milestone whose point is that everything is per-instance —
and would hide the dependency behind an import for side effect. It also would not
allow the thing M8 exists to prove: a test registering `gdrive` under two names and
running both, so the seam is exercised by two independently-configured Drive stores
without a pseudo-provider ever reaching a user's binary.

A provider's own configuration crosses the registry **undecoded**. It was a
`Params.Decode` callback filling a provider-defined struct; since M9 it is
`provider.Config` — the TOML text the user wrote — and the provider decodes it on
its own side, because a closure cannot cross a process boundary. The rule is
unchanged either way: `internal/config` never learns what a Drive folder ID is, and
adding a provider touches neither package. TOML rather than a second encoding
because it is what the user typed, so the error for a misspelt key names the key as
it appears in their file. `Params` is a struct rather than bare arguments so that
giving providers something new — the per-mount logger it already carries, a metrics
sink later — does not churn every `Factory` signature.

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

This is a claim about the **tree**, not about any one binary's link set, and the
distinction matters because M23 changed the latter: the default build links every
shipped backend into `drivel` itself. The claim above survives that unchanged —
`internal/provider/gdrive`, `internal/provider/sftp` and `plugin` all build clean
for `windows/amd64`, where only go-fuse's internal packages fail (measured
2026-10-01) — but one thing read off it is no longer available. That nothing above
the mount seam depends on a concrete provider type used to be *inferable* from the
host having linked no backend; it is now asserted directly, over `go list -deps`,
in `internal/app/seam_test.go`. That is the better statement of the rule anyway: a
property the source has, rather than a consequence of a link set.

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
   exFAT or FAT, a tmpfs `/tmp` on FreeBSD — `setxattr` fails,
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

#### 2.9.2 macOS: licensing and macFUSE

Under MPL-2.0 this question is close to vacuous. It is recorded anyway so a later
session does not re-derive the AGPL-era argument and mistake it for a live concern.
MPL's copyleft attaches to **files** (§1.10, §3.2): the obligation is to publish
modified Drivel source files, and it does not reach a separate program under any
theory, so there is no combined-work analysis to perform in either direction.

The arms-length reasoning this section used to carry is still true and is why the
same question was answerable under AGPL: go-fuse contains no cgo and does not link
libfuse — it implements the FUSE kernel protocol in pure Go, and its entire
interaction with macFUSE is to `exec` the `mount_macfuse` helper and then read and
write a file descriptor received over a unix socketpair (`fuse/mount_darwin.go`).
Two programs communicating at arms length are separate works, not a combined one.
That is now belt and braces rather than the load-bearing argument.

One rule survives, and it is about macFUSE's terms rather than ours: **never bundle
macFUSE, never ship an installer that fetches it, never publish a combined image.**

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

**WSL2: measured on 2026-09-29, and it corrects what this section used to say.**
drivel's ordinary `linux/amd64` build runs unmodified — WSL 2.7.14, kernel
`6.18.33.2-microsoft-standard-WSL2`, the full suite green under `-race` with
`DRIVEL_REQUIRE_TESTENV=all` and **nothing skipped**, so `/dev/fuse` and the M5
marker both genuinely worked, `internal/vfs` included. One test fails there and the
product is innocent: `TestScopedSweepRecoversConcurrencyAfterThrottling` wants all
eight listings in flight inside a 3 ms window, and nested-virt sleep overshoot
measures 1.59 ms mean / 3.62 ms worst against the host's 156 µs — the jitter is the
size of the window. It passes without `-race` in the same VM, and with `-race` on
the host even at `GOMAXPROCS=4`.

This section previously said `/mnt/c` is drvfs and "does not carry Linux user
xattrs", making it the M5 catastrophe. **That is wrong, and the reason it was wrong
is instructive:** drvfs is a transport, not a filesystem. `/mnt/c` mounts as 9p with
`aname=drvfs` and maps user xattrs onto NTFS extended attributes, so the marker set,
read back, listed, survived `sync` and survived a full `wsl --shutdown`. The thing
that decides is the **Windows filesystem underneath** — the same mount over exFAT
would still fail, and that is the form the caveat should have taken all along.

Keep `-data` on the ext4 filesystem inside the WSL2 VHD anyway, for a different and
better reason: NTFS is case-insensitive, so `Foo.txt` and `foo.txt` collide into one
local path. That is §2.5 / MC-30 arriving by the route this document already flags
for APFS, and unlike the xattr question it has no mitigation.

### 2.10 Plugin loading — backends in their own process (M9, M23)

A backend is a separate **process**. `drivel` launches it, talks to it over gRPC on
a unix socket, and restarts it if it dies. The seam it implements is §2.5 unchanged;
what follows is only how the implementation gets there.

There are two ways it gets there, and they differ in *one* step. A backend the tree
ships is **bundled** — its code is in the `drivel` binary, which launches it by
re-executing itself as `drivel plugin-serve <kind>` (M23, below). Any other backend
is an **installed executable** named `drivel-provider-<kind>` that `drivel` finds on
a search path, which is how M9 shipped and is what keeps an out-of-tree backend
possible. Everything after the exec — the handshake, the socket, the generated
protocol, capability negotiation, error classification, the relaunch — is identical,
and that is a requirement rather than an observation: a behaviour reachable on one
path and not the other is a difference nothing tests.

```
  drivel                                     drivel-provider-gdrive
  ──────                                     ──────────────────────
  Loader.Discover()   scan the search path
  Registry.Register() one Factory per kind
       │
  Factory(ctx, Params)
       │  fork/exec, handshake on the child's stdout
       ├──────────────────────────────────────────►  plugin.Serve(gdrive.Factory)
       │  Open{config TOML, content broker id}
       ├──────────────────────────────────────────►  factory(ctx, Params{Config, Log})
       │                         {capabilities}   ◄──  provider.Capabilities(store)
       │
       │  Put / Get / Stat / Changes / Enumerate …
       ├──────────────────────────────────────────►  the real backend
       │                                          ◄──  errors + their classification
       │
       │  PutRange{extents, source handle}
       ├──────────────────────────────────────────►
       │            ReadAt{handle, off, len}      ◄──  io.ReaderAt over the local file
       │
  Close() then Kill()
```

**Where a bundled backend comes from.** `cmd/drivel/bundle_on.go`'s
`bundledBackends`, a map from kind to `provider.Factory`, read twice: by the host to
register what it can open, and by `drivel plugin-serve <kind>` in the child to pick
the factory to serve. The host re-executes `/proc/self/exe` where it exists —
the running inode rather than a name for it, so there is nothing to replace between
deciding to launch and launching — and `os.Executable()`'s path elsewhere, where
anyone able to replace it could replace drivel itself. The build option is the
`nobundle` tag, which empties that map; `make build-nobundle` keeps it compiling and
is a `check` gate, because the unbundled shape is what a distribution packaging each
backend separately needs and nothing else here exercises it.

The **kind is the argument the host passed**, which is M9's rule with the filename
replaced: still not anything the backend says about itself. One
`plugin-serve <kind>` subcommand rather than one per kind, so dispatch is a single
entry rather than a list to keep in step — and it is kept out of `usage()`, `drivel
help` and the completions deliberately, with a test asserting the absence. Run by
hand it is inert: go-plugin refuses to serve without `DRIVEL_PLUGIN` in the
environment.

**Where an installed backend is found.** `drivel-provider-<kind>`, on a search path
that is the running binary's own directory, then `$XDG_DATA_HOME/drivel/plugins`,
then `/usr/local/lib/drivel/plugins`, then `/usr/lib/drivel/plugins`;
`DRIVEL_PLUGIN_PATH` replaces that list entirely rather than extending it, so "which
plugin am I running?" has one visible answer. The binary's own directory comes first
so that a build from a checkout finds the plugins built beside it with no
configuration at all. The **kind is the filename** and nothing the plugin says about
itself — §9/M9 has the reasoning, and the short version is that a self-naming plugin
can contradict its filename and two files can then claim one kind.

**Precedence is bundled-wins, and the losing file is named.** A kind in
`bundledBackends` is never looked for on the search path and never launched from a
file, so an installed `drivel-provider-gdrive` beside a bundled `gdrive` is skipped
with one log line naming it. The alternative — a file that merely goes first — is
exactly what `DRIVEL_PLUGIN_PATH` refuses to be, and it would leave in place the
swap-the-file window the bundled path closes by construction. A kind the binary does
not carry is unaffected, so this narrows nothing for an out-of-tree backend. It also
means the mode checks below are not reached for a bundled kind: an unsafe file
nobody is going to launch must not register a Factory that fails.

**What stopped being readable off the binary.** Until M23 the host linked no backend
at all, so "nothing above the seam depends on a concrete provider type" could be
read off the artifact. The property is unchanged in the source and the inference is
gone, so it is asserted directly — `internal/app/seam_test.go`, over `go list -deps`
— which is a better statement of the rule than a consequence of it was. §2.9's
cross-compile proof is untouched, being a claim about the tree rather than about a
link set. What does change is `govulncheck` attribution: a Drive SDK advisory is now
a finding against `drivel`. Same risk, accepted deliberately.

**What a missing backend says.** `provider.Registry` holds factories and must not
learn what a plugin is, so the loader installs a hint (`Registry.Hint`) and an
uninstalled kind reports both halves: `unknown kind: "gdrive" (available: sftp);
no drivel-provider-gdrive in …`. On its own the first half is accurate and
unactionable — since M9 the commonest cause of it is not a typo.

**What is checked before launching.** That the file is a regular executable, that it
is not group- or world-writable, and that its directory is not world-writable unless
sticky. This is not a sandbox and is not described as one: a plugin runs as the same
user with that user's whole filesystem, and drivel launching it is drivel trusting
it. The check is the narrower claim that the thing being trusted is the thing the
administrator installed — a binary anybody can replace between install and launch is
a stranger's code holding the user's credentials, and an M16 boot mount is where that
happens unobserved.

**What it is given.** Its own configuration, as the TOML text the user wrote
(`provider.Config`), and an environment that is **built rather than inherited**:
`PATH`, `HOME`, `TMPDIR`, locale, TLS roots and proxy settings, plus `SSH_AUTH_SOCK`
because the SFTP backend documents the agent as an auth method. Everything else is
dropped — every ambient-credential convention, all of `XDG_*` (M16's lesson about a
privilege drop and root's config directory), and all of `DRIVEL_*` so that a plugin
cannot load plugins.

**What happens when it dies.** The next call notices, relaunches it behind an
exponential backoff, and returns a *retryable* error meanwhile — so the engine's
existing M4 retry loop is what waits, and a crash costs a deferred push rather than
a failed one. The capability set is captured at the first open and held across
restarts, because the engine wires itself to that answer once.

**What happens when the *host* dies, which is the harder direction.** go-plugin
ends a plugin by having the host call `Client.Kill`, which `process.Close` does on
an orderly shutdown. A host killed by a signal runs no code at all, and until
2026-10-13 nothing covered that: there is no liveness mechanism on the plugin side
of go-plugin — the child is given the host's *own* stdin rather than a pipe to it,
and a gRPC server goes on listening after its last client disconnects. MC-40 found
the consequence by being the case that looks for it: `kill -9` on a mount left
`plugin-serve` running indefinitely, holding that account's credentials and its
connection to the provider with nothing driving it, and one more accumulated per
crash-restart cycle.

The fix is the mechanism M16 already uses for its daemonize handshake. The host
creates a pipe, passes the read end as the child's fd 3, and keeps the write end
for its own lifetime; the child reads it, and a read that ends *cleanly* means
every writer has closed — which the kernel guarantees for any way a host can die,
including the ways that execute nothing. Four properties carry it. **Only a clean
EOF counts**: any other read error means the descriptor is unusable, which is not
evidence about the host and must not take a working backend down with it. **A
plugin with no pipe is left alone**, which is what makes the call unconditional in
`Serve` and what keeps an out-of-tree plugin built against an older `plugin`
working. **The host releases its end on exactly the two transitions where a
backend stops being its own** — a relaunch and `Close` — because holding it would
leak a descriptor per crash in the component whose purpose is surviving crashes.
And **it is cleanup rather than a boundary**: a plugin is code drivel launched and
trusted (rule 8 again), so one that wanted to outlive its host could simply not
read the pipe. What this buys is that an honest backend stops when there is
nothing left to serve.

Three alternatives were weighed. `SysProcAttr.Pdeathsig` is Linux and FreeBSD
only, and in Go the signal is delivered when the *thread* that forked exits rather
than the process, so the runtime retiring that thread kills a healthy backend
(golang/go#27505). Polling `getppid` is portable and simple but up to a tick late,
and has to reason about PID 1 and subreapers to decide what "reparented" even
means. Watching the gRPC connection would make the plugin care about its own
transport and still say nothing about a host that is alive but wedged.

One consequence is worth naming because it is a regression in a smaller thing.
The child now exits promptly instead of unwinding, so an in-flight `Put` does not
run its own cleanup — on SFTP that means the server-side temporary is *stranded*
where before the orphan lived long enough to remove it. It is invisible
(`Enumerate` skips the prefix, M18 rule 7) and costs quota; reclaiming old
temporaries is the open follow-up, and its age threshold has to exceed the longest
plausible upload, because a young temporary may be another client's.

**What it costs.** One process per mount, and a local RPC hop on every byte of every
transfer. Against a network backend the hop is noise; against the local backend M11
proposes it is a real cost and that milestone should measure it. See §9/M9.

**What does not cross, and why it took work.** `provider.ContentHasher` is the one
call whose naive proxying is absurd: it streams a whole file across the socket to
get thirty-two bytes back. It is also the one that runs most — M6's gate 3 precedes
most content pushes, and Drive declines `RangePutter` deliberately, so gate 2 always
falls through and gate 3 runs on every content push at or above one block. A changed
1 GB file therefore crossed the socket *twice*: once to be hashed, then again in
`Put`.

The host now computes the digest itself, and the interesting part is how it is
allowed to. It may not assume an algorithm — the seam's whole reason for asking the
provider is that the provider owns that choice — and a declared name would be a
claim, checkable only by trusting it. So it is **identified by observation**: on the
first hash of a mount, the host asks the backend to digest two fixed vectors it can
check for itself, and adopts a local implementation only if one reproduces both
exactly. Nothing matching is an ordinary outcome, not a failure, and the content
streams as before — a backend using a digest this build cannot compute, or the same
one in a different encoding, simply keeps the old path.

Two properties make this safe rather than clever. `HashContent` takes an
`io.Reader` and nothing else, so a backend's digest is structurally a function of
the bytes alone — there is no path, size or salt it could vary by that the probe
would miss. And a mis-identification fails *benign*: a wrong algorithm or encoding
produces digests that never equal what `Stat` reports, so gate 3 declines and the
file is pushed. The failure mode is a gate that stops helping, never one that skips
a push it should have made. Two vectors rather than one because a single input two
functions happened to agree on would be adopted for every file after it.

A backend loaded in process is untouched by all of this: it never reaches the proxy,
and its `HashContent` is the local call it always was.

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
5. **The baseline outlives the deletion that drops it** (added 2026-10-16, after
   the field found the hole). Items 1 and 4 both compare against a record, and a
   *delete* push removes the record — so for a short time after a deletion the one
   thing that can recognise a stale report of that object is gone. Drive's
   `trashed` flag reaches a trashed folder's children asynchronously, so its feed
   reports the child still present with exactly the content the deletion removed,
   §4 has nothing to match, and the change reads as an ordinary remote create. A
   deletion therefore leaves a **tombstone** — the baseline it removed, plus when
   it removed it (`state.Tombstone`) — and the inbound path applies item 4's test
   to it for `tombstoneWindow` afterwards. Two things about its shape:

   - **Identity, not time.** The obvious discriminator is "is this change older
     than our deletion?", and it is the wrong one: our deletion time is this
     machine's clock, the change's timestamp is the provider's, and the comparison
     is wrong by the skew between them in the direction that decides whether
     somebody's file comes back. The record we kept is a better witness. A
     re-creation carrying genuinely different content is applied whatever either
     clock says, and one carrying the same content is suppressed and then found by
     the next sweep — a delay, where the alternative is a resurrection.
   - **A bucket of its own, not a flag on the echo.** M7b's delete baseline, M6's
     gate 2, the sweep's "new locally" row and M27's lost-push row all read the
     echo bucket, and a tombstone must be invisible to every one of them. A
     separate bucket is invisible by construction; a flag would be invisible only
     for as long as nobody forgot to check it.

   Why the file heals and the directory does not, which is what makes this a data
   bug rather than wasted traffic: the re-downloaded file is deleted again by the
   next page, but nothing in the pre-fix trace reported the *directory* at all —
   the download created it as a parent — so no later page removed it. It was then
   permanent at `-sweep-interval 0`, and with a sweep worse, because a local-only
   directory with no baseline is the local walk's "new locally" row: the next
   sweep pushes it back up and re-creates, on every other client, a directory the
   user deleted.

   **Confirmed live against real Drive on 2026-10-17**, by the same probe that
   found it (`scripts/multiclient/mc23-selfresurrect.sh`, one client, 8
   seed-and-delete cycles of 20 files). **4 of the 8 cycles entered the window**
   — against 1 of 8 on the run that found the defect, so the race is commoner
   than that run suggested — and produced **12 suppressions, 0 downloads, 0
   mkdirs, and the directory gone in all eight cycles**. The trace is the pre-fix
   one with its middle line replaced: `[sync] delete sub2/e14.txt`, then
   `[pull] ignore sub2/e14.txt (deleted here 0s ago; the remote is still
   reporting what we deleted)`, then `[pull] delete sub2` six seconds later when
   the feed caught up.

   Two things the run added. **The lag is measured, not just bounded**: the ages
   in those twelve lines run from 0s to 5s, so `tombstoneWindow`'s five minutes
   is the order of magnitude above the observed maximum that it claims to be.
   And **on Drive the directory is entombed on its own merits**, which the
   paragraph above did not predict — a Drive folder carries a `version`, so the
   baseline has an identity, so the deletion leaves a tombstone for it and the
   stale report of the folder is suppressed directly (`[pull] ignore sub2`, in
   two of the four cycles). Suppressing the *child* is what keeps the directory
   away on a provider that reports no version for a directory; on this one both
   mechanisms hold, and the belt is the one that was reasoned about.

This checksum/version-reconciliation approach (rather than trying to perfectly track
event provenance) is what real sync engines converge on, because FS and cloud events
are both lossy and racy.

---

## 5. Outbound (push) path

1. FUSE op succeeds against underlying dir → enqueue `LocalChange{op, path}`.
2. Uploader coalesces rapid events per path (debounce writes; a burst of `Write`s +
   `Release` becomes one upload) and serializes per-path to avoid reordering.
   The window is `-push-delay` (config `push-delay`, default 300ms). It is short
   by default because what it merges is already coarse: `OpWrite` is emitted once
   per *close*, not once per `write(2)` (§2.1), and a new file's `OpCreate` is
   emitted by the creating handle's close rather than by `Create` (2026-10-18),
   so a 50 GB copy arrives as one event rather than as millions — or as the two
   it used to be, which the window could merge only for a copy finishing inside
   it, so every larger one was uploaded twice (MC-40). Raising it
   buys quota — a file rewritten repeatedly uploads once per window — and drains
   the outbound queue on a longer cycle, which is what keeps a bulk import of
   small files from filling the event channel and back-pressuring the FUSE
   handler (`vfs.node.emit` blocks rather than drops, by design).
   **The trailing timer is re-armed by every later change, so the window alone
   cannot bound how long a path is held.** A max-wait deadline, measured from the
   change that made the path pending rather than from the last one, is what does:
   `syncengine.maxWaitFactor` puts it at ten times the delay. Without it a path
   touched again inside every window is never dispatched while the mount is up,
   and the longer the delay is set the easier that is to reach — which is why the
   bound landed with the flag rather than after it.
   Two things it does *not* fix, and neither should be implied of it. A file held
   open and never closed emits no event at any setting, so it is invisible to the
   push path entirely until it is closed and is otherwise picked up only by the
   M7b sweep. And a longer window makes §6 conflict copies *more* likely, not
   less: it leaves more time for the remote to diverge before the push lands.
   Deferral is a quota and back-pressure trade, never a conflict-resolution one.
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
- **The push side applies it too (2026-10-17).** The rule above runs on the pull,
  so it only fired when the remote change arrived before our push; when both
  clients pushed, the second whole-file `Put` replaced a remote that had moved
  since its baseline and the first edit existed nowhere (MC-31, MC-60). Before a
  whole-file `Put` of a path with a baseline, `syncengine.preserveDivergedRemote`
  now `Stat`s the remote (reusing the M6 gates' `Stat` where one was taken) and,
  on a mismatch with the echo, downloads the remote into a conflict copy named
  for *its* mtime — the name the pull's "local newer" branch would have used, so
  the two never make the same copy twice — before uploading. Who wins the path is
  unchanged (the pusher); only the loser's survival is new. Two refinements, both
  needing the provider's digest: local bytes equal to the remote ⇒ record the
  baseline and skip; local bytes equal to the *baseline* ⇒ the push carries no
  edit, so it is dropped and the pull applies the remote. A copy that cannot be
  downloaded fails the push, never the copy. Costs one `Stat` per rewrite of a
  small file with a baseline (creates still cost none); narrows the race to the
  `Stat`→`Put` gap, which only a conditional write could close. **The pull
  half needed the hashless case too (2026-10-18)**: it ran only when the remote
  reported a digest, so on SFTP an unpushed local edit was simply overwritten;
  it now asks the M18 local fingerprint, and with no baseline compares bytes. A path with
  **no** baseline (a replaced state DB, two clients creating one path) is
  guarded on the same terms only when the gates already paid for the `Stat` —
  files of a block or more — since a small create would otherwise buy a round
  trip it never paid, and on Drive a second name query per create.
  **Superseded 2026-10-18**: a first push now goes through `provider.Creator`
  when the store offers it, a `Put` that refuses an occupied path. Drive's `Put`
  already resolves the path, so the refusal costs nothing there. SFTP links the
  temporary into place, which also makes the refusal atomic. A store without
  `Creator` pays one `Stat`. So files of every size are guarded.
- **Where the loser goes is configurable (2026-10-18)**, and who wins is not.
  `syncengine.Conflicts` is one value per mount, shared by the engine, the pull
  loop and the status reader, and every resolution calls its `keepLocal` (the
  loser is the local file) or `keepRemote` (the loser is the remote object).
  `conflicts` picks the destination: `dir` (default), `local-trash` (the
  freedesktop.org home trash, `internal/trash`), `remote-trash`
  (`provider.Trasher`, refused at open without it) or `overwrite`, which also
  skips the first-push check. For `dir`, `conflicts-dir` places the
  per-directory `.drivel-conflicts` folder on `both` sides (default), `local` or
  `remote`. Three things carry it. **The folder is recognised by its name**, and
  `Conflicts.Syncs` is the single answer to "does this mount sync this path?",
  asked by the push, the pull, the sweep's remote pass, its delete pass and its
  walk. One pass disagreeing would upload a copy and then infer it deleted.
  **A loser that cannot be kept fails the operation that would replace it**
  (pull: the refetch pass retries; push: the engine retries). Only the *local*
  step of keeping it can fail that way — into the local folder or the home
  trash — and the remote step after it (an upload, or an upload and a `Trash`)
  is logged and leaves the copy in the local folder when it fails. On the pull
  side that remote step runs on its own goroutine behind the pause gate, so a
  large loser no longer holds up the inbound changes queued behind it.
  **One conflict is usually met twice**, by the pull ("local newer") and then by
  the push of that edit, so `Conflicts` remembers the remote version it kept, per
  path, in memory. A copy in the local folder is found by name anyway, and a
  restart costs at most one duplicate in a trash. Default `both` reverses the
  "local-only by policy" rule above: that rule existed so the walk would not
  upload sibling copies by accident, and those still stay local. A copy in the
  folder is uploaded on purpose, by `keepRemote`/`keepLocal` itself, with its
  baseline recorded.

---

## 7. Concurrency model
- One goroutine for the FUSE mount (go-fuse manages its own pool).
- Uploader: a bounded worker pool consuming a channel of `LocalChange`, keyed/
  serialized per path.
- Downloader: single goroutine running the poll loop; applies changes sequentially
  (parallelizable later).
- State store guarded by bbolt transactions; the `pending`/echo maps are the only
  shared mutable state between up/down paths — accessed only through the store.
- `context.Context` threaded everywhere for clean shutdown; on unmount, wait for
  the downloader's goroutine and drain the uploader queue, each bounded, before
  exit. **Cancelling the context is not the same as waiting for the goroutine**: a
  downloader still running when `Run` returns is still running when `Close` shuts
  the state DB and the backing dirfd, and in in-place mode that descriptor's number
  can be reused under it.
- **A provider must not hold a lock across a transfer, and the engine cannot tell
  whether it does** (2026-10-16). Everything above is the *engine's* concurrency,
  and a backend that serialises itself silently undoes all of it: `gdrive.Put`
  held the one mutex guarding the path index for the length of each upload, so a
  pushing mount could not resolve a path for the pull loop, could not read a
  change page, and could not push any other path — which is MC-36 and MC-50's
  measured inbound stall, and is separately why `-upload-workers` bought nothing
  on Drive. The rule is the §2.5 seam's shape applied to locks: index work under
  the lock, the network call outside it, and exclusion no wider than the thing it
  protects (here one path, so that resolve-then-create stays atomic and two
  concurrent creates cannot make same-name siblings). It belongs in the review of
  any new backend, because no test above the seam can observe it.

  **Measured live on 2026-10-17**, and the number is a ratio rather than a rate so
  it survives the difference in shape: inbound transfers per outbound transfer
  during a client's push phase went from **0.02 to 0.86** (two clients, 300 files
  each written at one instant — `scripts/multiclient/mc36-interleave.sh`). What
  the fix removed is visible in the series and not in the endpoint: the download
  column climbs with the upload column from the first sample, where before it was
  flat at ~0 for 23 minutes and then did 1980 files in the quarter hour after the
  last upload landed. The arithmetic that never closed — one upload's wait per
  acquisition predicting ~4s per inbound file against ~70s measured — is moot at
  0.36s per inbound file: the state DB's single writer and the plugin's call
  serialisation are *unneeded* to explain anything at this scale rather than
  excluded, and a workload large enough to make commits the rate limiter could
  still surface them.

---

## 8. Open questions / future
- **A security review has not happened**, and the gap is recorded in `SECURITY.md`
  rather than only here because the file that invites reports should not imply its
  own list is complete. Nothing in this document has been checked adversarially end
  to end; the invariants exist because a *correctness* argument demanded them, and
  the two are not the same audit. Three notes for whoever does it. The plugin seam
  looks like the obvious target and is mostly not one — a backend running as the
  user with the user's credentials is a deliberate trust delegation (§2.10), so the
  defect-shaped part of it is narrow: **discovery**, meaning how a file on disk
  becomes the backend, where the mode checks narrow a check-then-exec window that
  M23 closed for every backend drivel ships and left open for an installed one. The surface with the least adversarial attention is the
  opposite one — **remote-controlled data driving local filesystem operations**,
  since a provider supplies the paths and names that decide what gets written where
  in the backing tree, and §4's echo records and §7b's baseline are keyed by those
  same strings. And the review should treat the §9/M0 rule as applying to itself: a
  guard nobody has seen fail is a guard of unknown strength.
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
- **Backends that are network filesystems**: SFTP (shipped, M18), SMB/CIFS, NFS
  and WebDAV, plus the plain-directory provider they are all measured against —
  M17–M21 in §9, written as one design because they share almost everything. They are the mirror
  image of Drive: path-addressed, so no M7 index; no change feed, so the M7b sweep
  *is* the inbound path and `-sweep-interval` becomes a poll interval; and a native
  write-at-offset, so M6's range-write path finally runs against something real
  while the unchanged-content gate declines. What they add over simply mounting the
  share is §2.1 — an FS operation that never blocks on the network — plus lazy
  hydration and conflict copies, and §9 declines a backend that cannot point at
  that. One shared precondition is a correctness item: a provider configured with a
  *local* path is outside `app.Validate`'s overlap guards, which only know about
  paths belonging to mounts.
- **Google Photos is not a filesystem** and §9's M22 recommends against it as a
  provider rather than sketching one: three of the six required `Store` methods
  have no implementation (the API cannot delete from a library, cannot replace an
  item's content, and has no stable path to move), and since 2025 an app can only
  see media it uploaded itself. A one-way backup target is buildable and is a
  different program.
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
   underlying dir, with §4 echo suppression and adaptive cadence (§3.5). Engine-level
   state (cursor + echo records) persisted in `internal/state` (bbolt). ✅ The
   provider-internal path↔ID index stays in-memory (self-rebuilding, §2.5); its bbolt
   persistence lands in M7.
4. **M4 — Full bidirectional** with debounce, retries, conflict copies (§6), clean
   shutdown (drain the uploader queue; the downloader is cancelled *and waited for*
   — "stops on ctx cancel" was this note's wording, and cancelling without waiting
   was the gap §2.8 and §7 now describe). ✅
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
   ever stores the all-or-nothing cases. That is deliberate: **per-block faulting**
   — which has never been scheduled and is deliberately not given a milestone
   number here — and M6 (dirty ranges) inherit the schema rather than migrating
   it.

   Three decisions carry the correctness:

   - **The marker is an xattr, not a database row.** `user.drivel.placeholder` on
     the backing file is authoritative; the state-store bitmap is a cache. The
     failure this prevents is data loss, not slowness: a placeholder mistaken for
     a genuinely empty file gets uploaded as zero bytes over the remote content it
     was standing in for. The xattr survives losing `drivel-state.db`. Where the
     backing filesystem has no user xattrs, the mount refuses to start in lazy mode
     (it warned until 2026-10-17, and the "fallback to the state store" this line
     once promised never existed — the store caches ranges, not placeholder-ness).
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
   them is gone too, so there is nothing left to go stale. `ranges` holds
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
      than the local one: a remote deletion is not something the local tree can be
      re-derived from. `Store.Remove` on Drive is a move to the trash (§2.5), so a
      refused-too-late pass is now recoverable for 30 days — but the trash is the
      last guard, not a licence to weaken this one, and `delete = "permanent"`
      turns it off.

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

   **The first live run found that none of the above was reachable: the descent
   stopped after two calls** (MC-12, 2026-10-14, and it had been doing so since
   this shipped on 2026-09-07). `enumerateScoped` returned the constant
   `scopedCursor` for as long as the frontier was non-empty — correct on its own
   terms, since the frontier is in memory and nothing about it is worth
   persisting. `runSweep` ends a sweep whose provider hands back *the cursor it
   was given*, as a loop-breaker against a provider that cannot advance. Call 1
   takes `""` and returns `"scoped"`; call 2 takes `"scoped"` and returns
   `"scoped"`; the sweep ends. So every scoped sweep saw the mount root plus one
   batch of `enumFanout` folders: **477 of 1157 objects** on the rig, and a cold
   client that stopped at 472 files of 1114 with the sweep recorded as complete.

   **Three places were wrong, and fixing one would not have been enough.**

   - `provider.Enumerator` did not *say* that a continuation cursor must differ
     from the one it was handed. It does now, with the consequence named, because
     the requirement is the engine's and an out-of-tree backend cannot infer it.
   - The descent returns `scoped:N`, counted per call rather than per listing — a
     partly-throttled batch issues no new request for the folders it re-queued,
     and a token that repeated there would read as a stall under exactly the
     throttling the re-queue exists to survive. Resume is unchanged: any
     non-empty cursor still means "restart".
   - **The loop-breaker fell through to the passes that need completeness.** It
     set `next = ""` and then ran `pushLocalOnly`, `applyDeletes`, `SetCursor`
     and `FinishSweep` over a listing that was a *prefix* of the tree — so every
     unlisted path was a delete candidate, bounded only by `-max-deletes`, and
     unbounded at `max-deletes = 0`. A stalled sweep now keeps its applied pages
     and runs neither remaining pass, writes no completion stamp, leaves the
     in-progress record in place so the next mount starts fresh, and logs
     `reconcile INCOMPLETE`. The regression test fails against the old code on
     the delete, not on the log line.

   **Why nothing caught it.** `gdrive`'s own tests drive `Enumerate` in a loop
   that feeds the cursor straight back, without the engine's loop-breaker in the
   way — so the helper now asserts the contract, and two older scoped tests fail
   against a constant cursor as a result. And the only live cold sweep on record
   is MC-01's, which ran **2026-09-05**, two days before scoped mode existed: it
   was a flat sweep, and the flat path returns Drive's own page tokens, which
   advance. The feed path was never involved, which is why a fleet that was
   already converged stayed correct throughout.


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
11. **M9 — Plugin architecture.** ✅ Shipped. Every storage backend now runs in its
   own process, launched by drivel over hashicorp/go-plugin: gRPC on a unix socket,
   one plugin process per mount. As M9 shipped it, `drivel` itself linked no
   backend at all and every one was a `drivel-provider-*` executable found on a
   search path; M23 has since bundled the shipped backends into the host, which
   changes how the child is started and nothing else about anything below.

   The milestone was scoped as "let third parties add providers without forking",
   and the seam it would need already existed — `provider.Store` plus the optional
   interfaces beside it, designed in M2 and proven against two independently
   configured stores in M8. So M9 was expected to be about the *loading* mechanism
   and nothing else. Two of those three expectations held. The interface did not
   change and the shipped backends did not change. **Capability detection did, and
   it had to.**

   **The one thing the seam could not do across a process boundary was answer
   "what can this backend do?"** Above the seam that question had always been a Go
   type assertion:

   ```go
   if cs, ok := store.(provider.ChangeSource); ok { … }
   ```

   which is exact as long as a store is a value whose method set is visible. The
   host side of a plugin is not: it is one generated proxy type serving every
   backend, so it implements `ChangeSource`, `Enumerator`, `RangeGetter`,
   `RangePutter` and `ContentHasher` whether the backend behind it has any of them
   or not. Type assertion would have answered yes to all five for every plugin —
   a mount over SFTP polling a change feed that does not exist, the engine
   splicing extents into a store that cannot patch, M6's gate 3 hashing gigabytes
   to compare against a digest nobody computes.

   So capability detection moved into the seam. `provider.Capability` is a bit per
   optional interface, a store may implement `provider.Declarer` to say which it
   offers, and callers ask through `provider.AsChangeSource` and its four
   siblings. There are six call sites in the tree and they all changed. What
   makes it safe is the one-line invariant §2.5 states — **a declaration narrows
   and can never widen** — which is recorded there, with the seam it governs,
   rather than a second time here.

   In-process backends are unaffected: `gdrive` and `sftp` implement exactly what
   they can honour, which is already an exact answer, and neither implements
   `Declarer`. The one place it is needed is the proxy — and, usefully, the one
   other place a method set stops being the truth is a test double, which is how
   the fake backend under `plugin/testdata` offers a different capability set on
   each test without thirty-two variants of itself.

   It also settles something M12 would have hit: a `crypt` decorator over another
   provider has the same problem, since a wrapper either forwards a capability or
   it does not, and with type assertions it would need one wrapper type per subset
   of the inner store's capabilities. With a declaration it forwards one value.

   **Decided: gRPC over go-plugin, not WASM, and not `net/rpc`.** WASM would have
   given a real sandbox, which go-plugin does not, but it would also have given a
   backend no sockets, no filesystem and no OAuth — i.e. nothing a storage backend
   is made of — so the sandbox would have had to be opened up for precisely the
   things that make it worth having. go-plugin's older `net/rpc` mode needs no
   code generation, but it is gob, which locks every plugin to Go, and streaming a
   file's content over it means hand-rolling what a gRPC stream already is.

   The codegen that gRPC costs is paid the way the shell completions are paid:
   `make proto` regenerates, `make proto-check` fails if the committed output has
   drifted, and the generated files are committed because whoever builds from a
   tarball has no protobuf toolchain. The toolchain itself is pure Go — `buf` is
   the compiler as well as the driver — so there is no protoc and no C++ anywhere
   in the build.

   **Decided: the shipped backends are plugins only.** `cmd/drivel` no longer
   imports `gdrive` or `sftp`, and the registry it builds is a scan of the plugin
   search path with nothing compiled into it. That is not a side effect of the
   milestone, it is the point of it: the Drive SDK, OAuth, the QUIC transport and
   the SSH stack are now in the process that needs them, so a failure in any of
   them is a failure of one process that the mount survives.

   Keeping an in-process path "for convenience" was considered and rejected for
   the reason M8 rejected an `init()`-filled registry: it would be a second way to
   reach the same backend, with a precedence rule between them that nobody would
   remember, and the path that is not the default is the path that rots.

   What that cost is one genuine complication. The `-drive-*` flags are Drive-shaped
   by history and the host still owns them, so it still has to build a Drive
   settings table and still has to generate shell completions for
   `-drive-sweep-mode` and `-drive-delete` — and completions rule 3 says those
   words must be the program's own constants, not copies. The answer is
   `internal/provider/gdrive/gdconf`, a leaf holding `Config` and the two
   enumerated types, which `gdrive` re-exports as aliases so that nothing below
   the seam changed. The host links the vocabulary; the plugin links the
   implementation.

   **Decided: one plugin process per mount.** Two mounts are two sets of
   credentials, which is already why they get separate engines, state stores,
   index files and worker pools (§2.8). Sharing one backend process between them
   would put both accounts' traffic in one address space and let either starve the
   other — the cross-mount coupling `app.Validate` exists to prevent, reintroduced
   one layer down.

   **Decided: the kind comes from the filename.** `drivel-provider-gdrive` provides
   `gdrive`, and a plugin does not get to announce what it is. A plugin that named
   itself could contradict its filename, and two files could then claim one kind —
   resolved either by directory order (so the answer depends on a listing) or by
   last-wins (so installing anything can silently replace a backend). Two files
   that *do* claim one kind by filename are resolved first-on-the-path-wins and
   the loser is named in the log, because an operator who installed something they
   are not running should be told.

   **Failure isolation is the engine's existing retry loop, not a new supervisor.**
   A dead process surfaces as a retryable error; the next call relaunches it
   subject to an exponential backoff, and the engine — which has backed off and
   retried transient provider failures since M4 — is what waits. A crash therefore
   costs a deferred push rather than a failed one. Two details keep it honest: a
   process that lived at least a minute has its failure counter reset, so a backend
   that dies hourly does not inherit a crash loop's delay; and the capability set
   is captured at the *first* open and held across restarts, because the engine
   wires itself to that answer once — whether this mount has a pull loop at all is
   decided at mount time — so a set that changed underneath it would leave a
   downloader polling a feed that is no longer there.

   **Capability scoping is an environment allowlist, and the limit is stated rather
   than glossed.** A plugin's environment is built, not inherited:
   `GOOGLE_APPLICATION_CREDENTIALS`, `AWS_*` and every other ambient-credential
   convention are dropped, as is `XDG_*` — which M16 already learned the hard way,
   where an inherited `XDG_CONFIG_HOME` surviving a privilege drop left a mount
   running as the user while reading root's account — and as is `DRIVEL_*`, so that
   a plugin cannot load plugins. What survives is what a program needs to run
   correctly on the machine it is on: `PATH`, `HOME`, `TMPDIR`, locale, TLS roots,
   proxy settings. `SSH_AUTH_SOCK` survives too, and it is the one entry that is a
   credential: the SFTP backend documents the agent as an authentication method and
   has a setting for declining it, so scrubbing the socket would leave that setting
   silently doing nothing (M8 rule 6's failure again) and break the commonest
   working SFTP configuration there is.

   **This is not a sandbox and the documentation says so in those words.** A plugin
   runs as the same user, with that user's whole filesystem; scrubbing the
   environment stops a credential being handed over by accident, it does not stop a
   plugin that wants one from reading it. What the host does enforce is narrower and
   worth having anyway: it refuses to launch an executable that is group- or
   world-writable, or one whose directory is (unless sticky), because a binary
   anybody can replace between installation and launch is a stranger's code holding
   the user's credentials — and an M16 boot mount is where that happens with nobody
   watching.

   **Versioning is a single number, not a range.** `plugin.ProtocolVersion` is part
   of go-plugin's handshake and a mismatch refuses the launch, naming both numbers.
   Adding a field to the protocol does not bump it, because protobuf already makes
   that compatible both ways and that is the mechanism to use for anything additive
   — a capability the host does not recognise is dropped with a log line rather than
   refused, so a newer plugin loses a feature against an older drivel instead of
   losing the connection. Removing a field or changing what one means does bump it.
   The alternative, a compatible range, fails silently: an almost-compatible plugin
   answers most calls correctly and loses a sentinel error or an mtime somewhere in
   the middle, which surfaces as data not syncing rather than as an error.

   **The three sentinels had to be rebuilt on the far side, and each one lost is a
   silent behaviour change.** A plugin classifies its own error — it is the only
   side that can, since the backend's error types are its own — and the
   classification travels as a gRPC status detail. `ErrNotExist` lost means a
   rename stops syncing (the engine would no longer answer it by pushing the
   destination as fresh content). `ErrCursorExpired` lost means inbound sync stops
   forever, which is the exact bug that made the sentinel exist. `IsRetryable` lost
   means the first rate limit is permanent. What is deliberately *not* preserved is
   the error's concrete type: a host cannot type-assert into a plugin's package,
   and pretending otherwise would be the coupling the seam exists to prevent.

   **`PutRange` is the one call that runs backwards, and it is why there is a
   second service.** `provider.RangePutter` takes an `io.ReaderAt` rather than a
   Reader, deliberately, so an implementation may seek to its extents in whatever
   order its wire protocol prefers. Streaming the local file into the call would
   replace random access with a single forward pass — changing the contract for a
   backend loaded as a plugin while leaving it intact for the same backend
   constructed directly, which is the worst possible split, because in-process
   construction is what that backend's own tests use. So the host serves the file.
   The surface is narrowed twice: the channel is opened once per connection rather
   than once per call (go-plugin's `AcceptAndServe` returns only when the whole
   broker shuts down, so a per-call listener would accumulate one per range write
   for the life of the mount), and the plugin cannot name a path — only a handle
   the host registered immediately before the call and dropped immediately after.

   **What it costs, honestly.** Every byte of every transfer crosses a unix socket
   now. Against a network backend that is noise — the socket moves data two to
   three orders of magnitude faster than Drive or an SFTP server will — but against
   the local backend M11 proposes it is a real cost, and that milestone should
   measure it rather than assume it. There is also one process per mount to
   supervise, and a mount can now fail for a reason that has nothing to do with its
   backend: the plugin is missing, or its permissions are wrong. Both are reported
   by name, and a kind whose binary was refused is still *discovered* — it
   registers a factory that fails with the reason — because "unknown kind gdrive
   (known: sftp)" would send someone hunting for a missing install when the file is
   right there and its mode is 0777.

   **Not done, and deliberately.** Mount backends are still compiled in; the
   `mount.Backend` seam is clean enough that the same treatment would work, but
   nothing is asking for it and a FUSE connection is not a thing to hand across a
   process boundary casually. There is no checksum pinning for a plugin binary
   (go-plugin offers `SecureConfig`); the mode and directory checks are what ships.
   A pinned digest per kind in the config file was recorded here as the obvious
   next step; **M23 superseded it for a bundled backend and not for an external
   one** — the host carries those backends, so there is nothing left to pin for
   them, and the launch no longer goes through the path whose check-then-exec
   window cannot be closed. An external plugin is still a separate file reached by
   a path, so pinning stays live there and merges with M24's signing question. And
   the protocol is not merged with M14's control socket — see that entry, where
   the reason is that the trust directions are opposite.
12. **M10 — Platform parity (macOS, then FreeBSD).** Independent of M9; nothing
   waits on either. The tree already cross-compiles for both (§2.9), so this is not
   a port — it is closing the two gaps that make a build that *runs* differ from a
   build that *works*. Ordered by ratio of user value to effort:

   **Status: both xattr implementations are written and neither has been run on its
   own platform.** What remains is a live run per platform and the two FUSE-T
   questions below, so M10 is not closed. Writing them also settled a question this
   entry had been asking the wrong way round: the danger was never "off Linux", it
   was "a backing filesystem that cannot hold the marker", which includes exFAT/FAT
   and a tmpfs `/tmp` on FreeBSD, on kernels that are otherwise fine — but *not*
   WSL2's drvfs, which was measured carrying the marker on 2026-09-29 (§2.9). And
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
   opaque to it exactly as a Drive folder ID is (§2.8, rule 4). M9 has landed, so
   this is a question with a concrete answer rather than a prompt: a decorator is
   where a plugin API either composes or does not, and the piece that decides it is
   capability forwarding. `provider.Declarer` is what makes that tractable — a
   wrapper forwards the inner store's declared set as one value, where type
   assertions would have needed one wrapper type per subset of the optional
   interfaces.

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

   **Shipped 2026-10-17, whole.** `internal/control` (the server, over a socket
   `drivel mount -control` opens), `syncengine.Monitor` and `StatusReader` (the
   observation surface and the status rules), pause and resume in the run loop,
   `state.Store.Lookup` (one read transaction per batch, no commit), and `drivel
   status|pause|resume|stop` as the first client. The schema is
   `docs/dev/control-api.md`; the user page is `docs/user/control.md`. The three
   policy questions at the end of this entry were answered by the user before the
   schema was written — **(a) off by default**, opt-in with `-control`, a process
   flag beside `-pprof`; **(c) no tree roll-up**: per-path status plus one bounded,
   name-paged listing of a directory's direct children, and a directory's own
   status says nothing about its contents; **(b)** is unchanged (peer uid, and an
   `Authorization` header accepted and ignored). Seven things the implementation
   decided, each of which the entry below left open or got slightly wrong:

   - **"An immutable snapshot published on each transition" is realised as a
     mutex no writer holds across anything that can block.** Copying a map of
     every pending path per transition is O(paths) per event; instead each writer
     takes the monitor's lock for a few map operations, never across a send, a
     provider call or I/O, and a reader takes it once per batch. The property the
     entry wanted — status answered while `dispatch` is blocked — holds and is
     `TestStatusIsServedWhileTheRunLoopIsBlocked`. Counters are atomics in an
     array indexed by name, which is what lets every hook be safe on a nil
     monitor (taking a field's address on a nil pointer is not).
   - **Per-path state is three counts, not one value**: debouncing, queued, and
     running. A second write that lands while the first upload runs would
     otherwise read `synced` the moment the first finishes. The entry is deleted
     when all three are zero and no failure is recorded, so the map is the paths
     *in motion*, not every path ever seen.
   - **Two values were added to the status list, both derivable.** `local-only`
     (a conflict copy, a special file, a drivel temporary — exists, deliberately
     not synced; reporting `unknown` there would be wrong in the other
     direction) and, in the stream only, `deleted`. **`synced` means the M18
     fingerprint still matches**, never a hash: a status read does not read file
     contents, so a baseline with no fingerprint is `unknown`, and so is a
     diverged file with nothing queued (changed outside the mount, or a crash —
     M27 part 4's row will push it at the next sweep). An abandonment recorded by
     an earlier process (the `repair` bucket) is `error`.
   - **Pause holds at dispatch with a held list, and the run loop owns the
     gate.** Content changes stay in the coalescer (no timers are armed while
     paused); structural ops, which dispatch immediately, go to an ordered held
     list; workers wait at a gate before each operation. The gate is shut by the
     run loop *after* it has stopped sending, never by the caller, which is the
     whole of why waiting there cannot back up into FUSE. A pause asked for while
     `dispatch` is blocked on a full queue is answered from inside the blocked
     send. Resume sends the held list in order, then flushes the map — per-path
     order, because a structural op flushed its path's earlier content ahead of
     itself. The pull loop waits at the same gate between polls, the sweep between
     pages, and `Engine.Push` (the reconciler's seam) before each push.
     **Hydration is not paused**: a read of a placeholder is a user on the FUSE
     path, and refusing it is an EIO for a file that exists. **An upload already
     in flight finishes.** The mutation that let dispatch send while paused was
     caught only once the storm test included structural ops — content alone
     never reaches dispatch during a pause, so the first version of the test could
     not fail.
   - **The overflow rule is a cap of 10000 held paths and operations**
     (`DefaultPauseCap`, a constant), past which the pause resumes on its own and
     says so loudly. **Expiry**: `for` defaults to 1h, at most 24h, and an
     out-of-range value is refused, not clamped. **Shutdown while paused resumes
     first**, so the drain sends what was held.
   - **Reachability is classified at the call sites**, not by wrapping the store:
     a wrapper would have to re-declare every optional capability and is exactly
     the type-assertion trap M9 rule 1 exists for. The engine's per-attempt
     outcome, the feed's `Changes` and `StartCursor`, the sweep's `Enumerate` and
     the downloader's `Get` are observed; a non-retryable error counts as contact
     (the backend answered), a cancellation counts as nothing, and an hour of
     silence is `unknown`. **Hydration's fetches are observed too** (added the
     same day): `hydrate.Hydrator.ReportOutcomes` takes the monitor's exported
     `Observe`, and reports the `Get` and a failure reading the body — never a
     failure writing it locally, which `io.Copy` returns through the same error
     and is why the body is wrapped. Without it a lazy mount that is only read
     from, which reaches its backend nowhere else, said `unknown` all day and
     missed the backend dying under its reads.
   - **The socket path has no fallback.** `$XDG_RUNTIME_DIR/drivel/control.sock`
     or `-control-socket`; with neither, `-control` is refused, because the
     obvious fallback is under `/tmp`, which anyone can pre-create. An existing
     directory that is not `0700` and ours is refused rather than chmod'ed. The
     socket was at first **not offered to the M16 mount helper**: every fstab
     line is its own process, so one default path is one collision per line.
     **Added the same day as `-o control[,control-socket=PATH]`, one socket per
     line named after the mount** — the name is already a per-line identity
     (the state DB's directory), so it adds no collision of its own, and a
     second line with the same name is refused through the M16 handshake when
     it finds the first one's socket live. Where it goes is decided by whether
     the helper *started* as root. Root (the boot case) cannot use
     `XDG_RUNTIME_DIR`: a mount unit has no session, so it is unset, and
     `/run/user/<uid>` exists only while that account is logged in and is
     removed at logout. So root makes `/run/drivel` (`0755`, root's, refused if
     anyone else can write it) and `/run/drivel/<uid>` (`0700`, chowned to the
     `run-as=` account — checked, never repaired, if it already exists) before
     the privilege drop, the way `logfile=` is opened before it. A helper not
     started as root has no privilege to drop, so its environment is its own
     and `$XDG_RUNTIME_DIR/drivel/<name>.sock` is trustworthy. Only the parent
     knows which case it was, so it resolves the path and hands it to the
     forked child in `DRIVEL_MOUNT_HELPER_CONTROL`; the socket is bound before
     `Run`, so a failure reaches `mount(8)` instead of a mount that reported
     success with its `control` doing nothing. The client finds it with
     `-name`, trying the mount's own socket before the shared one and only a
     path where a socket exists. `stop` and `resume` without `-name` do not
     look, deliberately: `stop -name` would read as "stop that mount" while
     stopping a multi-mount process whole. Verified 2026-10-17 as root with
     `run-as=coder` (socket in `/run/drivel/1000/`, `coder`-owned, found by
     `status -name` with no `XDG_RUNTIME_DIR`, `stop` drained and unmounted)
     and as root alone (`/run/drivel/0/`); the unprivileged half is
     `TestMountHelperServesAControlSocket`.

   The profiling routes moved as the last paragraph of this entry proposed:
   `/debug/pprof/` is served over the socket with `profile`, `trace` and
   `cmdline` refused, so the snapshot profiles need no port. `-pprof` is
   unchanged.

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
   protocol**, which did land on gRPC (§2.10). The trust directions are opposite —
   a plugin is code drivel launches and trusts, a control client is a user drivel
   serves — and one transport serving both is how a plugin ends up able to shut
   down the daemon.
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

   **Three things were not settled, and all three were policy, not implementation**
   (answered 2026-10-17 — see the shipped note at the top of this entry).
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
18. **M16 — fstab & non-interactive mount (Linux).** ✅ `drivel` is a `mount(8)`
    helper as well as a command: installed as `/sbin/mount.fuse.drivel`, a mount can
    be described in `/etc/fstab` and brought up by `mount`, `mount -a` or systemd at
    boot with no terminal attached. `docs/user/fstab.md` is the user-facing half.

    The milestone is small in code and almost entirely about three decisions, each
    of which is a way to get it wrong.

    **The type is `fuse.drivel`, and that is a correctness matter rather than a
    naming one.** go-fuse mounts as `fuse.` + its subtype (`MountOptions.Name`), so
    `/proc/self/mountinfo` reports `fuse.drivel` whatever the fstab line says.
    systemd's fstab-generator compares the fstab type against what the kernel
    reports, so a line saying `drivel` describes a mount that never appears under
    that name. `mount.drivel` is installed as an alias; the docs use the other.
    `MountSpec.FsName` now carries the fstab device field for the same reason —
    `findmnt(8)` and `umount(8)` match on it, and a hard-coded `"drivel"` made every
    mount on a machine indistinguishable. Empty still means `"drivel"`, so nothing
    that existed before this changed.

    **The helper must exit once the filesystem is live, and not one moment before.**
    `mount(8)` does not return until the helper does, so serving the mount in the
    foreground hangs `mount -a` and with it the boot; exiting early is worse, since
    `mount` would report success for a filesystem that may never appear. So the
    parent re-executes itself (`/proc/self/exe`, original argv, a marker in the
    environment), the child mounts and reports through an inherited pipe, and the
    parent exits 0 only on `ok` — carrying the child's failure back as its own
    otherwise. That needs a *signal* that the mount is live, which is why
    `mount.Options.Ready` exists: polling `/proc/self/mountinfo` needs no seam
    change but cannot distinguish "still starting" from "wedged", and has to guess
    which entry is ours.

    Two consequences of the fork are easy to get wrong and both are settled here.
    The daemon's stdout and stderr go to `logfile=` or to `/dev/null`, **never to
    the caller's**: an inherited descriptor stays open for the life of the mount, so
    any caller reading through a pipe — `out=$(mount -a 2>&1)` — would block until
    the filesystem was unmounted. Journal integration is the thing given up, and
    `logfile=` is the replacement. And there is no default timeout: systemd already
    bounds a mount unit's startup, and a second bound here with a different default
    is a second answer to one question. `mount-timeout=` is available, and kills the
    child rather than leaving a daemon that mounts later behind `mount(8)`'s back.

    **The privilege option is `run-as=`, not `user=`.** At boot the helper is root
    while the token, the backing tree and the mount belong to somebody, so
    `run-as=NAME` moves `HOME` and the XDG variables to that account and gives up
    root — setgroups, setgid, setuid, in that order, before anything reads a
    credential. The name matters: `user` is a standard fstab option meaning "a
    non-root user may mount this", and util-linux passes `user=NAME` down to the
    helper to record who did, so treating it as a privilege-drop request would act
    on an option nobody aimed at drivel. **Unsetting the inherited XDG variables is
    the load-bearing half** of the environment change — root's
    `XDG_CONFIG_HOME=/root/.config` surviving the drop would leave the mount running
    as the user while looking for the wrong account's token, which fails looking
    like a login problem rather than an environment one.

    Below that, the helper is a translation layer and nothing more: the option table
    ends in the same `app.MountSpec` the flag and config paths produce, so
    validation, opening and the shutdown ordering keep one implementation. Its
    `config=` mode refuses the shaping options exactly as `-config` does, and derives
    that set from `mountShapingFlags` rather than restating it, so the two cannot
    drift. `ro`, `remount`, `bind` and `move` are **refused** rather than ignored —
    an fstab line claiming a read-only mount over a writable one is the `lazzy =
    true` failure wearing a different hat — and an unknown option is an error unless
    `mount -s` asked otherwise. The state DB defaults under `$XDG_STATE_HOME` rather
    than to the flag path's bare relative filename, because an fstab mount starts
    with `/` as its working directory, where `drivel-state.db` means
    `/drivel-state.db`.

    **Verified on systemd, 2026-09-29**, on WSL2's Debian, which runs systemd as
    PID 1 — the container this was developed in has none, which is what left this
    open. `systemd-fstab-generator` turned an `fuse.drivel` line into
    `/run/systemd/generator/<escaped>.mount`; `systemctl start` mounted it,
    `findmnt` reported the type as `fuse.drivel`, and `systemctl stop` unmounted
    cleanly. The daemon **does** stay in the generated unit's cgroup —
    `CGroup: /system.slice/mnt-<x>.mount └─ /sbin/mount.fuse.drivel …` — and it is
    *not* killed when `ExecMount` finishes, so the fallbacks this entry reserved
    (`x-systemd.automount`, a `drivel@.service` with `x-systemd.requires=`) are not
    needed. Reached directly from a shell the daemon lands in `/init.scope` instead.
    `mount`, `mount -a` and `umount` were already exercised for real.

    **Two things remain unrun, and the second is the one with teeth.** Mounting at
    boot was not tested — the run used `noauto`, so the `local-fs.target` ordering
    is still only reasoned about. And nothing yet establishes whether M8's bounded
    drain completes when systemd tears the cgroup down on `stop`: an ordinary
    `systemctl stop` unmounts first, which is the graceful path, but a unit killed
    with a wedged mount underneath it would not, and that is a lost write rather
    than a slow shutdown.

    Linux only, and by mechanism rather than by omission: the helper protocol, the
    `/sbin/mount.<type>` lookup, the privilege drop and the re-exec handshake are all
    how Linux specifically brings a filesystem up unattended. macOS and FreeBSD have
    their own answers (a launchd agent, an rc.d script) and neither is reached by
    pretending to be a mount helper. The option parser is portable anyway, so its
    tests run everywhere.

**M17–M21 are five faces of one design, and the shared half is written here.**
Everything drivel knows about backends it learned from Drive: an ID-addressed
object store, reached over HTTP, with a change feed and a trash. The next group is
the opposite in every one of those respects — path-addressed byte stores with
POSIX-ish semantics, no identity beyond the path, and (with one exception) no feed
at all. Seven consequences fall out of that shape rather than out of any particular
protocol, so they are stated once and the entries below carry only what differs.

- **The seam fits without argument, and that is the point of §2.5.** These stores
  are natively path-addressed, so `Put`/`Mkdir`/`Move`/`Remove`/`Get`/`Stat` are
  each one protocol call and nothing below the seam has to invent an identity.
  `internal/pathindex` and the whole three-source resolution of M7 stay
  Drive-private, where they belong — an M7 index over a path-addressed store would
  be a cache keyed by its own value. **A provider in this group that grows a path
  index has misunderstood something**; the thing worth caching is metadata, and
  that is `Stat`.
- **No change feed means the sweep *is* the inbound path.** M7b and M7c are what
  make this group possible at all: without `Enumerate` a pre-existing remote tree
  is invisible, and without `ChangeSource` the sweep is the only thing that ever
  looks. This took a seam change that M18 made and the rest of the group inherits:
  before it, the `Downloader` was constructed only for a store implementing
  `ChangeSource`, so a feedless provider was silently upload-only. `src` may now be
  nil and `Run` branches into a sweep-only loop — see M18 for the two guards that
  make it safe, the load-bearing one being that the empty cursor must never be
  persisted. Two things follow that are easy to miss. `-sweep-interval` stops being a
  long-period safety net and becomes the **poll interval**, so its 24 h default is
  wrong for every backend here and each one must say so in its own docs rather
  than quietly inheriting it. And every §4 echo record has to survive between
  sweeps, because with no feed the echo store is the *only* memory of what was
  synced — which is also M7b row 2's delete baseline, so an aggressive prune here
  is a data-loss bug, not a tuning choice.
- **The M6 gates come out the other way round from Drive, and this is the group's
  most useful property.** Drive gets gate 3 (`md5Checksum` in `Stat`) and
  deliberately declines gate 2. Every filesystem here is the reverse: a write at
  an offset is the protocol's native operation, so `RangePutter` is real and M6's
  range-write path finally runs against something other than a fake — while a
  server-computed content digest is unavailable on all of them by default, so gate
  3 declines and the unchanged-content check falls through to a whole-file `Put`.
  That is safe by construction (the fallback is what M1–M5 always did) and it is a
  *cost*, so it belongs in each entry's economics rather than being discovered
  later.
- **`Move` is a real rename.** Server-side, identity-preserving, and atomic-ish —
  the good case §2.5 contrasts with S3's copy-plus-delete. Nothing in the engine
  branches on it; it just stops being the expensive path.
- **A deletion is permanent here, and drivel does not emulate a trash.** Decided
  2026-09-11, and it applies to every backend in this group and to M11 — not to
  SFTP alone, which is merely where the question first came up. Drive answers the
  seam's "prefer the recoverable form" with a real trash it already had; a POSIX-ish
  byte store has nothing of the kind, and manufacturing one is worse than the gap
  it fills. Inside the mount root it is actively broken: the sweep would enumerate
  the graveyard and pull every deleted file back down. Outside it, it is a second
  store with its own lifetime, quota and garbage collection — a feature nobody
  asked for, bolted under the one operation with no fallback above it, where a bug
  destroys exactly the data it was added to protect.

  So on every backend here `-max-deletes` is not the guard in front of a safety
  net; it is the only guard. That is a fact to **disclose** — each backend's user
  page says it in those words — rather than a gap to paper over. Revisitable if
  someone actually wants it, and not by a future session deciding the docs look
  alarming.

- **Case-insensitivity is the MC-30 same-name-sibling problem arriving by a third
  route.** §2.9's macOS note has the second. SMB is case-insensitive by protocol;
  the others inherit whatever the server's filesystem does, which the client cannot
  ask about. Unlike Drive there are no true same-name siblings to resolve — the
  collision happens on the server, silently, and `Foo.txt` overwrites `foo.txt`.
  No mitigation is proposed here for the same reason as MC-30: every candidate
  loses something a user wrote. It is a thing to *detect and report*, which makes
  it a good first customer for M14.
- **And the reason to build any of this.** The kernel already mounts NFS and SMB,
  sshfs and davfs2 already exist, and none of them does the one thing drivel does:
  §2.1's rule that an FS operation never blocks on the network. A `write(2)` into
  an NFS mount whose server has gone away hangs in `D` state; a `write(2)` into a
  drivel mount lands in the backing dir and the engine retries behind it. Add M5
  and the share is browsable at full apparent size while holding almost none of
  it, and M4's conflict copies mean two laptops editing one file over the same
  share produce a `.conflict` rather than a silent loss. **That triple — offline
  tolerance, lazy hydration, conflict copies — is the entire justification for
  this group.** A backend proposal that cannot point at it should be declined, and
  M20 below is declined on exactly those grounds.

One prerequisite is shared and is a correctness item rather than a feature.
**A provider whose configuration names a local path is inside `app.Validate`'s
blind spot.** M8's rule 2 guards overlap between *mounts* — backing trees,
mountpoints, state DBs — because those are the local paths the config describes.
A `localfs` directory (M17), an M11 store, and a kernel-mounted share used as
either are local paths belonging to a **provider**, and nothing checks them today.
Pointing one at a mount's backing tree builds an engine that syncs a tree into
itself; pointing it at a mountpoint breaks §2.7's cardinal rule from below, where
the in-place deadlock has no `/proc/self/fd` handle to save it. The guards
generalise (they are already "does path A contain path B" over a set), but the set
has to grow a provider-supplied member, which means `provider.Params` needs a way
for a provider to *declare* the local paths it will touch, before validation and
before anything opens. M11 noted this gap for one backend; M17 makes it the door
this whole group walks through, so it is a precondition rather than a note.

19. **M17 — `localfs`: a provider over a plain directory.** The degenerate
    backend — a `provider.Store` that mirrors the tree 1:1 into another directory,
    with no transform whatsoever. It is listed first because it is not really a
    fifth network filesystem, it is the *substrate* the next four are measured
    against, and because it is the only entry in this group that needs no protocol
    code at all.

    **It is also, today, the honest answer for NFS and SMB.** Mount the share with
    the kernel — which has spent decades on those two protocols, handles Kerberos,
    and is already installed — and point `localfs` at the mountpoint. What comes
    back is the whole justification above: FUSE operations land in the local
    backing dir and never touch the wire, the engine's retries and backoff absorb
    a server that went away, `-lazy` makes the share browsable without holding it,
    and conflict copies work. The cost is that everything below the seam blocks on
    the kernel mount instead of on an HTTP call, which is exactly where blocking is
    allowed. **Anyone proposing a userspace client for a protocol the kernel
    already mounts has to beat this**, and M19 and M20 below are judged on it.

    Three things it needs beyond the six required methods.

    - *`Enumerate` is a walk, and it is scoped by construction.* There is no flat
      mode and no account-wide listing to fall back to, so M7c's descent is the
      only shape; the parking machinery M7b's flat path needs does not apply, the
      same way it does not apply to the scoped Drive descent. Cheap enough that
      `-sweep-interval` can sensibly be minutes rather than a day.
    - *`ChangeSource` via inotify/kqueue is available and is a trap worth taking
      slowly.* A recursive watch would make the inbound path immediate instead of
      polled, and the overflow case maps onto the seam perfectly:
      `IN_Q_OVERFLOW` means "the changes are gone, re-enumerate", which is
      `provider.ErrCursorExpired` and already recovers through a sweep. What makes
      it a trap is that inotify is per-directory (one watch per subdirectory, a
      per-user limit, and a race between opening the watch and reading the
      directory), that it does not work at all over an NFS or SMB mountpoint — the
      kernel sees only *this* client's writes, which are precisely the echoes §4
      exists to discard — and that a cursor for a watch is not persistable, so a
      restart must sweep. **Recommendation:** ship polled first, with `ChangeSource`
      omitted; add the watch as a latency accelerator for the genuinely-local case
      only, and never let a mount that watched a network mountpoint believe it
      saw everything.
    - *`ContentHasher` is exact here and the algorithm is ours to pick*, which makes
      `localfs` the one backend in the group that gets **both** M6 gates — and
      therefore the reference implementation for the range-write path M11 and M18
      also want. It is not *free*, though: the digest `Stat` reports has to come
      from reading the remote file, so a sweep that hashed everything would be a
      full pass over the store. Key a digest cache on (size, mtime) and treat a
      miss as "no digest", which declines gate 3 rather than guessing — the same
      fail-towards-the-slow-answer shape every optional interface in §2.5 has.

    Its second use is as the seam's honest end-to-end test. The suite currently
    proves the provider contract against a fake, which shares its author's
    assumptions with the code under test; a real store that hits a real filesystem
    is the cheapest way to find the places where those assumptions differ, and it
    runs everywhere with no credentials and no network. That is a reason to build
    it early even if nobody mounts a share with it.

20. **M18 — SFTP.** ✅ **Shipped (2026-09-10.)** The strongest of the four, and the one to build first. One
    dependency pair that is already in wide use (`github.com/pkg/sftp` over
    `golang.org/x/crypto/ssh`, both pure Go, no cgo, so §2.9's cross-compile proof
    survives), a protocol whose operations map onto `Store` almost one-to-one, and
    an audience — anything with an SSH account — that needs no server-side software
    at all.

    - *`RangePutter` is native and this is the milestone's headline.* SSH_FXP_WRITE
      takes an offset; the seam's contract ("replaces the named extents in place,
      may neither create nor resize") is satisfied because the engine already
      checks the object exists at exactly `size` first. M6 gate 2 has been written,
      tested against a fake and never once exercised against a real server; this is
      where that changes.
    - *Gate 3 has no honest implementation and should not be faked.* A
      server-computed digest needs the `check-file` extension from the filexfer
      draft, which OpenSSH's server does not implement; a handful of others
      (ProFTPD's `mod_sftp`, some commercial servers) do. So `Stat` carries a
      digest only when the connection advertised the extension, and `ContentHasher`
      is capability-detected per connection rather than per provider — the first
      place in the tree where an optional interface's availability is a property of
      the *session*. Absent it, gate 3 declines and every content push is a full
      upload, which given gate 2 is usually the smaller cost.
    - *`Move` is `posix-rename@openssh.com` where offered and SSH_FXP_RENAME
      otherwise*, the difference being whether it clobbers an existing destination
      atomically. Take the extension when advertised.
    - *Host key verification fails closed or the milestone is worthless.*
      `ssh.InsecureIgnoreHostKey` must not appear in the tree, not even behind a
      flag with a scary name: a mount that comes up at boot from `/etc/fstab` (M16)
      has nobody to answer a trust-on-first-use prompt, so the key must be in
      `known_hosts` or the mount must refuse to open. Same class of decision as
      `mount` being non-interactive, and the same answer. Agent, key file and
      certificate auth are all reasonable; a password in a config file is not, and
      the option should not exist.
    - *Connection management is the unglamorous risk.* One TCP connection carries
      one SSH transport carries N channels, `-upload-workers` fan-out shares it,
      and a dropped connection has to be re-established underneath in-flight
      operations without the engine seeing anything but a retryable error.
      `pkg/sftp`'s per-file concurrent-request window is what turns a single
      transfer from latency-bound into bandwidth-bound; the default is
      conservative.

    **What building it changed, and one thing it could not deliver.**

    - *The group's central claim needed a seam change before any of it could be
      true.* "No change feed means the sweep IS the inbound path" was written here
      as though it followed from M7b, and it did not: `app.Mount` constructed the
      `Downloader` only for a store implementing `provider.ChangeSource`, so a
      store that could enumerate but not tail got no downloader at all — no sweep,
      no reconcile, silently upload-only. The fix is small and belongs to the
      group rather than to SFTP: `src` may now be nil, `Downloader.Run` branches
      once at the top into a sweep-only loop, and `app` wires a downloader for
      `ChangeSource` **or** `Enumerator`. Two guards make it safe. `beginSweep`
      takes no start token when there is no feed, and `runSweep` must **not**
      persist the empty one — `state.Cursor` would then report `ok=true`, which
      `startFeed` reads as "we are already tailing this remote" and would skip the
      startup sweep on every mount after the first, i.e. skip the whole inbound
      path. Every backend M19–M21 inherits this; none of them needs to rediscover
      it.
    - *Gate 3 has no implementation, and the reason is the library rather than the
      protocol.* The reasoning above stands — OpenSSH's server does not implement
      `check-file` — but `github.com/pkg/sftp` also exposes no way to *send* an
      arbitrary extended request, so the extension is unreachable even against a
      server that offers it. `sftp` therefore does not implement
      `provider.ContentHasher` at all, rather than implementing it conditionally.
      That costs nothing: `syncengine.contentMatches` short-circuits on an empty
      `RemoteFile.Hash` before it reads a local byte, so the absence is free and
      not merely safe. The per-session shape described above is still the right
      one if the capability ever becomes reachable — a wrapper type chosen at dial
      time, never a method added to the store.
    - *With no digest, `Version` carries the echo identity alone*, as mtime and
      size — the rsync heuristic, with the rsync caveat. SFTP reports mtime in
      whole seconds, so a remote edit that preserves a file's exact length and
      lands in the same second as our own last write is indistinguishable from our
      echo and is not pulled until something else touches the file. The
      alternatives are worse (size alone misses far more; hashing means
      downloading every file on every sweep), so it is documented in
      `docs/user/sftp.md` rather than mitigated.
    - *`Put` writes to a temporary name in the destination directory and renames
      it into place.* Not in the plan, and it should have been: the whole-file
      `Put` is the fallback behind every M6 gate, so it runs constantly, and an
      interrupted one writing in place leaves the remote truncated — which, with
      gate 3 declining, nothing downstream would ever notice. The rename uses
      `posix-rename@openssh.com` where the session advertises it and
      remove-then-rename otherwise; the sweep skips the `.drivel-upload.*` prefix
      so a temporary stranded by a dropped connection is never materialised
      locally and never inferred as a deletion.

      **The name is unique per call, and that half was wrong until MC-40**
      (2026-10-13). It was `tempPrefix + path.Base(abs)`, a function of the
      destination alone, so two `Put`s of one path opened one server-side file and
      wrote at their own offsets — and the loser of the race went on writing
      through its open handle into the file the winner had already renamed into
      place, leaving a destination holding a mixture of two uploads with an echo
      recorded saying it holds one of them. Reproduced every run at 4 MiB. Two
      writers is not a rare shape: within one mount the reconciler pushes through
      `Engine.Push` while the worker pool may be pushing the same path, and
      *across* mounts the server-side name is a shared namespace, so two clients
      collide however carefully either one is internally ordered — which is why
      the unique component comes from `crypto/rand` rather than a per-process
      counter. The destination's name is kept but capped, both so a stranded
      upload says what it was and because prefix-plus-a-long-name could exceed the
      255-byte filename limit on its own. The cost is that a stranded temporary is
      no longer reclaimed by the next `Put` of that path, which the plugin
      liveness fix in §2.10 makes a likelier outcome rather than a rarer one.
    - *`Remove` has no recoverable form.* This is where the question first came
      up and it was settled for the whole group rather than for SFTP — see the
      preamble's deletion bullet. `-max-deletes` is the only guard, and the user
      page says so in those words.
    - *Enumeration is a breadth-first descent with the frontier as its cursor*,
      not a flat listing, because SFTP has no recursive list. That makes it
      parent-first by construction, so the flat path's parking machinery (gdrive's
      "park a child on an unseen parent") has no counterpart here — the same
      conclusion M7c reached for scoped mode, arrived at from the protocol rather
      than from cost. A directory that vanished mid-sweep is skipped; **every
      other listing failure abandons the sweep**, because a sweep that quietly
      omits a subtree still reports itself complete and M7b's delete pass would
      then propose deleting everything under it.
    - *Containment is structural.* `path.Join(root, p)` resolves a leading `..` by
      climbing *out* of the root; cleaning against a virtual `/` first collapses
      it instead. Nothing above the seam should produce such a path — this is
      defence in depth, and it was a real bug caught by a test that asserted the
      property rather than the implementation.
    - *A hashless provider could not apply a remote deletion at all, and the live
      run is what found it.* `reconcile.matchesBaseline` answered the question
      "did the local copy diverge from what we last agreed with the remote?" by
      comparing a local MD5 against `state.Echo.Hash` — which is the *remote's*
      digest, and is empty for every backend in this group. So the answer was
      "diverged" for every file forever, and a file deleted on the server was kept
      locally and pushed straight back up on the same sweep: the deletion undone,
      permanently, with the log cheerfully reporting `1 kept (locally modified
      after a remote delete)`. `state.Echo` therefore carries `LocalSize` and
      `LocalMTime`, a fingerprint of the **backing** file at the moment the
      baseline was written, and `matchesBaseline` falls back to it when `Hash` is
      empty. Three things about it are load-bearing. It fingerprints the *local*
      file, whose mtime the kernel keeps to nanoseconds, so it is not the weak
      second-resolution comparison the wire format forces on `Version`. A zero
      `LocalMTime` means **not recorded** and never **matches** — an old state DB,
      a directory, a failed stat — because the wrong answer in that direction
      deletes a file somebody edited. And the push side records it *after* the
      upload, not before: a write that landed mid-upload has already queued
      another event, so the next push re-records the baseline against the newer
      bytes, whereas a pre-push fingerprint would describe bytes that are no
      longer on disk. Drive is unaffected — it has a digest, and takes the same
      branch it always did.
    - *Directories report a constant `Version` (`"dir"`).* Leaving it empty made
      `state.Echo.Matches` answer false for every directory on every pass, so the
      downloader re-created and re-logged each one — invisible at Drive's 24 h
      sweep, and the entire log at a poll interval of seconds. Their mtime would
      be no better, since it moves whenever a child is added.
    - *Verified against OpenSSH.* Run on 2026-09-10 against `sshd` with
      `internal-sftp`, through a real drivel mount: a pre-existing remote tree
      materialised, a local write pushed, a remote edit pulled, rename and delete
      propagated both ways, the diverged-copy guard held, and a 4 MiB edit to a
      64 MiB file transferred 4 MiB and left the two byte-identical — M6 gate 2's
      first run against a server. Still unrun: a server *without*
      `posix-rename@openssh.com` (so `conn.rename`'s remove-then-rename fallback
      is covered by unit tests only), and any non-OpenSSH server. Note for whoever
      sets up a rig: an external `Subsystem sftp /usr/lib/openssh/sftp-server` is
      run through the user's login shell, so anything that shell prints — a
      window-title escape, in the container this was built in — corrupts the
      stream and shows up as `packet too long`. OpenSSH's own client fails
      identically; it is not a drivel bug. `internal-sftp` avoids the shell.

21. **M19 — SMB/CIFS.** The interesting one, because it is the only backend in this
    group with a **real change notification** — and, M11's own sequence number
    aside, the only one anywhere on the roadmap whose feed is not a poll. That
    makes it the chance to test the `ChangeSource` contract against a second,
    structurally different feed.

    SMB2 CHANGE_NOTIFY watches a directory handle, optionally with the
    watch-the-whole-tree flag, and returns the changes since the last call. When
    more happened than the response buffer can hold, the server says so instead of
    truncating — which is `provider.ErrCursorExpired` under a different name, and
    already recovers through the M7b sweep. Nothing about the seam has to change to
    accommodate it, and confirming that is most of the milestone's value. Two
    honest caveats: the cursor is a live handle rather than a persistable token, so
    a restart sweeps; and coverage of what the *server* considers a change varies
    between Samba, Windows and NAS firmware, so a feed here is an accelerator that
    the sweep still backstops — which is the same posture as every other optional
    interface in §2.5.

    - *Case-insensitivity is not a server property here, it is the protocol's.*
      Every SMB share collides `Foo.txt` with `foo.txt`, so the group preamble's
      fifth point is unconditional for this backend and belongs in its user docs on
      day one.
    - *The dependency is the risk, and it should be settled before any code.* The
      pure-Go SMB2/3 client everyone uses (`hirochachacha/go-smb2` and its forks)
      is a small project with a thin maintenance record, it is unclear whether
      CHANGE_NOTIFY is exposed at all, and the alternative — cgo against
      libsmbclient — forfeits the cross-compile §2.9 treats as the proof the seams
      hold.
      **Check the fork situation and the notify support first**; if the answer is
      "vendor and maintain an SMB client", this milestone is much larger than it
      looks and `localfs` over a kernel `cifs` mount is the better trade.
    - *Everything else is unremarkable and that is a compliment.* Write-at-offset
      gives `RangePutter`, server-side rename gives a real `Move`, per-mount
      credentials without root is the concrete thing a userspace client buys over
      the kernel mount, and DFS referrals are the one place a path may not mean
      what it says.

22. **M20 — NFS. Recommended: don't, and the recommendation has a stated expiry.**
    Of the four network filesystems this is the one where a userspace client buys
    the least and costs the most, and the group preamble's justification argument
    is what settles it: everything drivel adds over an NFS mount — offline
    tolerance, lazy hydration, conflict copies — is delivered in full by `localfs`
    (M17) over a kernel NFS mount, with no protocol code, no new dependency, and
    Kerberos already working.

    Against that, a pure-Go client has to answer three things.

    - *There is no mature pure-Go NFS client.* The well-known Go projects in this
      space are servers or partial NFSv3 clients extracted from other tools.
      Writing NFSv3 (with MOUNT, portmap and the RPC layer under it) is a real
      protocol implementation, and NFSv4 — the version worth targeting — is
      substantially larger, with compound operations, state, leases and recovery.
    - *AUTH_SYS is not authentication.* A userspace client asserts its own uid and
      gid, which means the security of the arrangement is the server's IP
      allow-list and `root_squash` and nothing else. The real answer is RPCSEC_GSS
      with Kerberos, in pure Go, which is a project of its own. The kernel client
      already has it.
    - *And there is no feed to gain.* NFSv3 has no change notification at all;
      NFSv4 delegations are a cache-coherence mechanism, not a change stream, and
      directory delegations are rare in practice. So the inbound path is the sweep
      either way — identical to what `localfs` over a kernel mount already gives.

    **What would reopen it:** a maintained pure-Go NFSv4.1 client appearing, or a
    concrete deployment where the kernel mount is unavailable (an unprivileged
    container with no `CAP_SYS_ADMIN` and no way to get one) *and* SFTP or WebDAV
    to the same storage is not an option. Until then the entry stays here, declined
    with reasons, so it does not get re-proposed from scratch every year.

23. **M21 — WebDAV.** The widest reach for the least code, and the natural second
    backend after SFTP: it is what Nextcloud, ownCloud, Synology, Box, Fastmail and
    a long tail of self-hosted storage all speak, and unlike the other three it
    needs nothing but HTTP.

    - *It composes with §2.6 for free.* The transport is already an injected
      `*http.Client` that prefers HTTP/3 and falls back to HTTP/2; almost no WebDAV
      server will speak h3, and the fallback is exactly the code path that
      handles that. This is the first non-Drive user of `internal/transport`, so it
      also proves the transport is not secretly Drive-shaped.
    - *`Enumerate` is `PROPFIND`, and it must be the M7c descent.* `Depth: 1` is
      universally available; `Depth: infinity` is disabled by default on the most
      common server (Apache's `mod_dav`) and unreliable elsewhere, so a one-request
      full listing is not a thing that exists. Per-directory descent with the M7c
      fan-out and its throttle response is the shape, and the 97× measurement
      behind that response is as relevant to a small Nextcloud instance as it was
      to Drive.
    - *This is the one backend in the group that declines `RangePutter`, like
      Drive.* HTTP has ranged reads and no standard ranged write; the partial-`PUT`
      mechanisms that exist (Sabre/DAV's `PATCH` extension, `X-Update-Range`) are
      per-server and not worth branching on at first. So WebDAV gets M5 hydration
      via `Range` requests and falls back to whole-file `Put` on every write —
      which is what Drive does, so nothing is untrodden.
    - *ETags are the echo identity, and there is already a precedent for using an
      opaque one.* `getetag` is not a content hash and must not be presented as
      one, so `ContentHasher` stays unimplemented; but `RemoteFile` carrying the
      ETag lets §4's echo matching work exactly as it already does for Drive's
      Google-native files, which fall back to the opaque `Version` for the same
      reason. Servers offering a real checksum (`oc:checksums` on Nextcloud) can
      light up gate 3 as a per-server capability, the same conditional shape M18
      needs for `check-file`.
    - *Two fail-closed rules.* Basic auth over plain `http://` is refused at open,
      not warned about — the credential is the account. And a server that answers
      `PROPFIND` on the mount root with an empty collection must be an **error**,
      never an empty sweep, for the reason M7b row 4 already gives: an empty
      enumeration makes every synced path look deleted.

24. **M22 — Google Photos. Recommended: not as a `provider.Store`, and the reason
    is the API rather than the effort.** This one is not a filesystem wearing a
    protocol, it is a media database with a search endpoint, and three of the six
    required methods have no implementation at all.

    - *`Remove` does not exist.* The Library API can remove an item from an album
      **the app itself created**; it cannot delete from the user's library. So the
      provider would silently fail to propagate every local `rm`, which the M7b
      sweep would then read as a remote file with no local copy and re-materialise.
      A delete that comes back is worse than a delete that is refused.
    - *`Put` cannot replace.* Every upload creates a *new* media item; there is no
      update-content-in-place. `Put` is specified as create-or-replace, so a second
      write to the same path produces a duplicate rather than a new version — and
      the echo record then names an item the next write will not touch.
    - *`Move` has nothing to move.* There is no path. Filenames are neither unique
      nor stable, an item may belong to many albums or to none, and albums are not
      a tree — so any path space drivel presents is *synthesized* (`2026/09/…`), by
      us, from metadata that can change under us. A path-addressed seam over an
      identity-addressed store with no stable name is the M7 index problem with no
      source of truth to verify against.
    - *And the bytes are not the bytes.* Downloads go through a `baseUrl` that
      expires in about an hour and, without the explicit original-quality suffix,
      hands back a re-render with EXIF and location stripped. There is no ranged
      write of any kind, so M6 never fires; ranged reads are whatever the CDN
      happens to honour on a URL that can expire mid-transfer, which is not a
      capability to build `RangeGetter` on.

    On top of all of that, **the scopes needed to see a user's existing library
    were removed on 2025-03-31** — since then an app reads only the media it
    itself created, plus whatever the user hands it through the Picker, which is a
    browser component `drivel login`'s loopback-and-paste flow has no surface to
    host. That is the same structural blocker as `drive.file` in M7c, and it means
    the obvious product ("mount my photos") is not merely slow or lossy but
    *impossible*. **Verify this before scheduling anything here** — it is the fact
    the whole entry turns on and Google's terms move.

    **What is actually buildable, if someone wants it:** a one-way *backup target*
    — drivel uploads into an album it owns, never reads the library back, never
    deletes, and the local tree stays the only source of truth. That is not a
    `provider.Store` and should not pretend to be one; the seam has no read-only or
    append-only store concept, and inventing one to accommodate a backend that
    cannot delete would weaken a contract five other providers rely on. It is a
    separate tool that happens to share this repo's OAuth code, and it should be
    scoped that way or not at all.

25. **M23 — Bundled backends & re-exec launch.** ✅ **Shipped 2026-10-02**, except
    for the external-plugin authentication below, which M24 inherits. The host
    binary carries its backends' *code*, and launches one by re-executing itself
    with a hidden subcommand naming the kind. It is a **supply-chain milestone, not
    an isolation one**, and the entry leads with that because the obvious reading is
    the wrong one: putting the code in the same binary does not change what the
    plugin process may do once it is running. See "What this does not buy" below
    before reading it as a hardening item.

    **What landed.** `cmd/drivel/bundle_on.go` holds `bundledBackends`, a map from
    kind to `provider.Factory` carrying `gdrive` and `sftp`; `bundle_off.go` empties
    it behind the `nobundle` build tag, which is the whole of the build option.
    `drivel plugin-serve <kind>` is the child entry point — one subcommand for every
    kind, dispatched in `main` beside `mount-helper`, and absent from `usage()`,
    `help` and the completions with a test asserting that absence. In `plugin`, one
    `launchSpec` describes both launch paths and `open` is the single thing that
    consumes it, so the exec is the only place they differ; `Loader.Bundled` is how
    the host declares what it carries, and the precedence below lives there.
    `make build` is one `go build`, `make build-plugins` still produces the
    installable executables, and `make build-nobundle` is a `check` gate and a CI
    step so the unbundled shape cannot quietly stop compiling. Measured on
    2026-10-02: **33.8 MB bundled against 76.4 MB as three files** — the 2026-10-01
    prediction of 30.7 / 72.6 MB, on a tree that has grown a little since.

    **What it removes, which is the whole case for it.** Each of these is a live
    surface in M9's design, not a hypothetical:

    - *The search path.* `DRIVEL_PLUGIN_PATH`, the five-directory default, and the
      per-user `~/.local/share/drivel/plugins` entry that is searched **before**
      the system one. Anyone who can write a directory on that path can name a
      file `drivel-provider-gdrive` and be handed the user's OAuth token — which
      is the same trust the mode checks exist to protect, arriving by a route they
      do not cover.
    - *`safeToRun`'s race.* The mode and directory checks run against a **path**
      and the `execve` resolves that path again afterwards. No amount of checking
      closes the window; it can only be narrowed. Re-executing the host closes it
      by construction: on Linux `/proc/self/exe` is the running inode and cannot
      be swapped, and elsewhere the bytes being launched are the ones already
      running.
    - *Shadowed kinds.* Two files claiming one kind resolve first-on-the-path-wins
      with a log line nobody reads. A subcommand argument has one meaning.
    - *The checksum-pinning item* left open in §2.10 ("not implemented"). For a
      bundled backend it becomes **moot rather than pending**: pinning a digest of
      code you are already executing is a digest of yourself. It does *not* become
      moot for an external plugin — see "Unsettled" below.

    All four are retired **for the kinds the host carries**, which is every backend
    that ships, and for no others: the search path, the mode checks and the shadow
    resolution all still exist and still run for a kind the binary does not have.
    That is the shipped precedence talking — bundled wins per kind, external is
    still enabled — and the reason to state it plainly here is that "M23 removed the
    search path" is a sentence someone will otherwise write.

    **The mechanism.** The host resolves its own image (`os.Executable()`, or
    `/proc/self/exe` on Linux) and runs it as `drivel plugin-serve <kind>`; that
    subcommand is the three lines `cmd/drivel-provider-<kind>/main.go` holds today,
    `plugin.Serve(<kind>.Factory)`. `clientConfig` (`plugin/host.go`) is the tree's
    only `exec` site and changes from naming an installed file to naming the host
    plus two arguments. Nothing else in the launch path moves.

    **This replaced an earlier recommendation, and the numbers are why.** Until
    2026-10-01 this entry proposed carrying the backends as *executables* — an
    appended zip (or `embed.FS`) unpacked into a sealed `memfd` and exec'd through
    `/proc/self/fd/3`. Both halves were prototyped on 2026-09-11 against Linux
    6.19.14 and both work: `memfd_create(MFD_CLOEXEC|MFD_ALLOW_SEALING)` with all
    four seals reading back as `017` and the fd passed through `cmd.ExtraFiles`,
    and `archive/zip` reading an archive behind an arbitrary prefix natively
    because the end-of-central-directory scan computes the base offset. Keep that
    record: it is a working fallback if re-exec ever cannot be used. But re-exec
    dominates it on five axes, and four of them are not about size.

    - *Size, measured 2026-10-01.* Host 24.2 MB, gdrive 27.8 MB, sftp 20.6 MB —
      72.6 MB as three files, and 48.8 MB as host plus a deflated two-backend zip.
      One link unit holding all three is **30.7 MB**, 37% smaller than the zip,
      because the Go runtime, gRPC, protobuf and `plugin`/`provider` are linked
      once instead of three times.
    - *One implementation on every platform, where the zip needs three.* `memfd_create`
      is Linux-only; FreeBSD's `shm_open(SHM_ANON)` + `fexecve` was **never
      verified**; and macOS has no anonymous-image exec at all, so there the honest
      implementation was extract to a `0700` directory, launch, unlink — a *weaker*
      claim that writes to disk. Re-exec is the same code everywhere and the
      guarantee does not vary by platform, so no document has to qualify it.
    - *No `vm.memfd_noexec`.* The Linux ≥ 6.3 sysctl that reads `0` on the
      development container and at `2` would refuse every launch, requiring
      `MFD_EXEC` where the kernel knows it and a startup error naming the sysctl
      where it does not. It stops being reachable.
    - *The build graph does not invert.* The zip made `make build` two-stage —
      plugins first, host embeds them — and made the payload architecture-specific,
      so a wrong-arch zip had to be refused by name at discovery or fail as an
      `exec` error indistinguishable from corruption. Re-exec is one `go build`,
      and wrong-arch is impossible by construction.
    - *The embed-versus-zip question dissolves.* There is no payload, so there is
      nothing to embed or append, and the "built once, plugin set varied
      afterwards" use case that was the only thing justifying the archive is served
      instead by the external-plugin path below.

    **The pattern is already in the tree, which is an argument for it.** M16's mount
    helper dispatches on argv0 (`/sbin/mount.fuse.drivel`), carries a hidden
    `mount-helper` subcommand, and re-execs `/proc/self/exe` with a marker
    environment variable and an inherited pipe on fd 3. This is that mechanism
    applied a second time, not a new one introduced.

    **gRPC is retained, and behavioural equivalence is the requirement rather than
    an aspiration.** A bundled backend must reach the host through the same socket,
    the same handshake and the same generated protocol as an installed one, so that
    the launch mechanism is the *only* difference between them and no behaviour is
    reachable on one path and not the other. go-plugin's original `net/rpc` + gob
    protocol would be available unmodified and is rejected twice over: `net/rpc`
    has no streaming, while `Put` is a client stream and `Get` a server stream of
    `Chunk`s — buffering a whole file in memory is fatal for the 50 GB copy that
    arrives as one event, and hand-rolling chunking over
    the `MuxBroker` reimplements on the data path what gRPC already does — and the
    stdlib package is frozen with its own documentation pointing at gRPC. The
    protocol is also already generated, committed, and guarded by `make
    proto-check`. One thing does simplify: `ProtocolVersion` cannot mismatch for a
    bundled backend, because host and plugin are the same build. The handshake
    check stays regardless, because an external plugin can still be any version.

    **The bundled binary is the default build**, and `TAGS=nobundle` is the way
    out of it. `make build` produces one executable that is host and every shipped
    backend, so a fresh checkout is usable the moment it compiles and a release
    artifact is one file with no plugin directory to populate; the tag exists for a
    distribution that ships one package per backend, which is also the shape that
    keeps a provider's advisories attributed to that provider. `make install`
    installs plugin files only for that build, since a bundled host would ignore
    them and log that it had. The separate `cmd/drivel-provider-*` targets stay,
    for two reasons that are not symmetric: they are how an out-of-tree backend is
    built at all, and `plugin/testdata/drivel-provider-fake` is a real executable
    on disk, so the test suite keeps exercising **both** launch paths rather than
    letting the external one rot unobserved. The bundled path has its own coverage
    for the same reason: `plugin/bundled_test.go` runs the host half — the spec, the
    exec arguments, and a real launch of the fake with the subcommand arguments in
    place — and `cmd/drivel/bundle_on_test.go` the child half, by reading
    go-plugin's handshake line out of the built binary, which needs neither network
    nor FUSE because a factory is not called until `Open`.

    **What it costs.** Two things, and the first is smaller than it looks.

    §2.9's cross-compile proof **survives**, because it is a property of the tree
    and not of the host's link set: `internal/provider/gdrive`, `internal/provider/sftp`
    and `plugin` all build clean for `windows/amd64`, where the only failures are
    go-fuse's own internal packages, so "every cross-compile failure traces to
    go-fuse and nothing else" stays true of a binary that links both backends
    (measured 2026-10-01). What is lost is narrower: the *inference* that nothing
    above the seam depends on a concrete provider type, which could previously be
    read off the host having linked no backend. That property is unchanged in the
    source and is now asserted directly — `internal/app/seam_test.go`, an
    import-graph test over `go list -deps` that fails if any package above the seam
    reaches `internal/provider/*`, with the host command as its positive control so
    it cannot pass by asking the wrong question. That is a better statement of the
    rule than a consequence of it was, and it was the one new test this milestone
    owed.

    `govulncheck` reporting shifts: a Drive SDK advisory was a finding against
    `drivel-provider-gdrive` alone and is now one against `drivel`. The risk is
    identical and the distinction is not worth preserving for a bundled app —
    accepted deliberately, 2026-10-01.

    **What it does not cost is memory, and that is measured rather than argued,
    because the obvious worry says otherwise.** A 2.5× larger binary sounds like
    2.5× the resident set; it is the reverse. `execve` maps the `LOAD` segments and
    faults pages in from the page cache on demand — it never reads the file — and
    of 33.8 MB only 23.2 MB is in any segment at all (10.1 MB text, 11.5 MB rodata
    including `pclntab`, 0.6 MB data). The remaining ~10.6 MB is DWARF and the
    symbol table, in no segment, so it never reaches memory; the RW segment's 34 MB
    BSS reservation is likewise almost entirely untouched.

    The win is not demand paging, though. It is that **host and child map the same
    inode**, so one copy of the Go runtime, gRPC and protobuf serves both.
    Measured 2026-10-02, one `gdrive` mount, host plus backend:

    | | host | backend | combined PSS | shared-clean |
    | --- | --- | --- | --- | --- |
    | bundled, one file | Rss 23.8 MB · Pss 14.7 | Rss 28.2 MB · Pss 19.1 | **33.8 MB** | 18.2 MB |
    | `nobundle`, two files | Rss 20.3 MB · Pss 20.3 | Rss 25.6 MB · Pss 25.6 | **45.9 MB** | 0.0 MB |

    Two separate binaries share nothing, which is where the 12 MB goes, and the
    saving is about one runtime's worth of text per *distinct backend kind* in use
    — several mounts of one kind already shared with each other before M23, because
    that too was one inode.

    **Two things not to misread off those numbers.** Residency is not execution:
    92–95% of text and 77–85% of rodata are resident, and neither figure says that
    much of the code ran. The kernel maps sixteen pages around each fault off a
    page cache the build left warm, and Go's startup touches broadly on its own —
    module data, itabs, `pclntab`, and the `init()` of **every** linked package, so
    a `plugin-serve sftp` child still runs Drive's protobuf registration. Anyone
    hoping bundling would be free because a given child "only uses half the binary"
    has the mechanism wrong; it is cheap for an unrelated reason. And file-backed
    clean pages are page cache attributed to a process rather than consumed by it,
    so the number that actually scales per mount is **private-dirty: 5.3 MB for the
    host and 8.4 MB for a backend**, unchanged by bundling because the same code is
    running either way.

    What is left to reclaim is disk and cold-start page cache, not memory:
    `-ldflags "-s -w"` takes ~10 MB off the file and nothing off the resident set.
    It is not set here, because the debug info is what makes a core file or a
    profile legible, and the cost it carries is a download rather than a mount.

    **M9 rule 4 was reworded rather than violated.** "The kind is the executable's
    filename and nothing the plugin says" has no filename to point at for a bundled
    backend. The replacement is that the kind is the argument the *host* passed,
    which is still not anything the plugin claims about itself, so the rule's
    purpose holds; and the failure mode it was written against — two files claiming
    one kind, resolved by directory order — is one of the things this milestone
    removes. One `plugin-serve <kind>` subcommand rather than one per kind, so
    dispatch is a single entry rather than a list to keep in step.

    **The hidden subcommand is a surface and is kept out of `usage()`, `help` and
    the completions** — by a test (`cmd/drivel/bundle_test.go`), so the exclusion is
    as deliberate as `completionApp`'s refusal of a flag with no `completionHints`
    entry rather than accidental. Direct invocation is inert without help from us —
    `MagicCookieKey` is `DRIVEL_PLUGIN`, so go-plugin prints its "not meant to be
    executed directly" line and exits — so this is surface area and tidiness, not a
    hole.

    **The one real decision was a product decision — do bundled plugins replace
    the external path, or precede it? — and it is half-settled on purpose.**
    `provider` is a public package specifically so that a backend can be built out
    of tree, and bundled-only ends that. What shipped is **bundled wins per kind,
    external still enabled**: a kind in `bundledBackends` is never looked for on the
    search path and never launched from a file, and the file it won over is named in
    one log line; a kind the host does not carry is found exactly as before. That
    buys `PathEnv`'s own property — "which backend am I running?" is answered in the
    log rather than by a directory listing — without closing the door the public
    `provider` package exists to hold open.

    What did **not** ship is the opt-in: there is no switch that disables the
    external path wholesale. The recommendation stands that there should be one, and
    it is the first of the three in the override surface below; it waits on the
    authentication work, because "allow external plugins at all" and "allow
    *unsigned* external plugins" are two settings nobody should meet one at a time.
    It is also what decides whether M24 has an install target at all, so the order
    is: authentication, then the switches, then M24.

    **Self-verification is worth one paragraph of honesty.** For a bundled backend,
    integrity of the plugin *is* integrity of the host binary — one artifact to
    sign, one thing for a package manager or dm-verity to protect — and that is a
    genuine simplification rather than a new guarantee. It says nothing about an
    external plugin, which is a separate artifact and the reason the authentication
    rule below exists at all.

    **An external plugin proves what produced it with an Ed25519 signature checked
    against a key the host pins — not OpenPGP, and not a CA** (decided 2026-10-01;
    M24 inherits this rather than deciding it again). The signature covers a small
    manifest naming `(kind, version, sha256)`, the digest is the executable's, and
    it is the same number go-plugin's `SecureConfig` already verifies before exec —
    so the signature adds *provenance* to an integrity check that exists, rather
    than being a second mechanism beside it.

    The constraint that decides the format is **where verification happens**: at
    plugin launch, during mount startup, which has to work offline. An fstab mount
    at boot has no network and no guarantee that any particular binary is
    installed. That removes OCSP, CRL fetching and shelling out to `gpg(1)` before
    any comparison of merits.

    - *OpenPGP fails on the verifying side, not the signing side.* Signing a release
      with an existing GPG key is fine; the host has to verify **in process**, and
      `golang.org/x/crypto/openpgp` has been deprecated and frozen since 2021. The
      maintained route is a third-party fork, which `docs/dev/dependencies.md` would
      have to justify, for a format whose web of trust buys nothing here because the
      key would be pinned regardless.
    - *A CA costs operation and adds a failure mode nobody wants.* `crypto/x509` is
      stdlib, so verification is not the problem — operating the thing is: offline
      root, key ceremony, intermediate issuance, revocation distribution. Revocation
      cannot be network-based on this path, so it degrades to shipping a new drivel
      with an updated list, which is what a pinned key list already is minus the
      X.509. And **certificate expiry is an active hazard**: a lapsed cert breaks
      every mount using that plugin at a moment nobody chose, which for a program
      holding people's files is worse than having no expiry. The one real benefit is
      delegation — an intermediate per plugin author — and there is nobody to
      delegate to yet. Build it when there is.
    - *Pinned Ed25519 is the right size.* `crypto/ed25519` is stdlib, verification
      is a few lines, there is no new dependency and no ASN.1 surface, and it works
      offline. The precedent is direct: the Go toolchain pins `sum.golang.org`'s
      key, and `signify`/minisign are the same shape. Pin a **list** rather than one
      key, so rotation needs no CA — ship key N and N+1, retire N a release later.

    **Whose key is the harder half, and the answer is two paths rather than one.**
    Signing someone else's binary with drivel's key is the mistake: either we build
    it, which needs reproducible builds or we are attesting to bytes we did not
    produce, or we sign what they send, so the signature attests to provenance we
    never verified. Instead — **drivel signs only what drivel builds**, and a
    third-party plugin is authenticated against **its author's** key, pinned by the
    user on first install. That second path is M24's trust-on-first-use record with
    one field added, and it upgrades the record from "the same bytes as last time"
    to "the same author as last time" without anyone operating a CA. A signature
    proves provenance and nothing else: not that the code is good, and **not that it
    should outrank what is bundled**.

    **The trust-on-first-use happens at install, never at mount**, and this is the
    SFTP host-key rule arriving a second time (§9/M18: a mount that comes up at boot
    from `/etc/fstab` has nobody to answer a prompt, so the key is in `known_hosts`
    or the mount refuses to open). `drivel plugin install` may ask; `drivel mount`
    may not, and must refuse an unknown key rather than accept it — the same answer
    as `mount` being non-interactive generally, for the same reason.

    **The override surface is three switches, because they are three trust
    decisions.** Allow external plugins at all (M23's opt-in); allow *unsigned*
    external plugins; and name a custom location (`DRIVEL_PLUGIN_PATH`, meaningful
    only when the first is on). Folding the third into the second means someone who
    wanted a custom directory silently accepted unsigned code — M8 rule 6's
    reasoning running the other way, a setting that is broader than it looks. And
    **a boot or fstab mount must not reach the second or third by default**: M16's
    privilege drop already had to unset root's inherited `XDG_*` to stop a mount
    reading the wrong account's token, and environment-derived trust goes wrong on
    exactly that path.

    **What this does not buy.** A plugin still runs as the same user with that
    user's entire filesystem; §2.10 already says this and M23 does not change one
    word of it. `plugin/env.go`'s built-not-inherited environment is untouched and
    remains the actual restraint on what a backend is handed — and it still applies,
    because the backend is still a child process with an environment the host
    builds. If the goal is a privilege boundary rather than a supply-chain one, the
    lever is **Landlock or seccomp on the plugin process** — orthogonal to this
    milestone, considerably larger, and it composes with it rather than competing.
    Do not let M23 be recorded as having delivered it.

    **Unsettled, in the order that matters.** First, whether a *newer* authenticated
    external plugin should outrank the bundled one automatically. The recommendation
    is **no**, on four grounds, and it is recorded here rather than decided because
    the goal behind the proposal — not stranding users on a stale bundled backend —
    is right and wants serving some other way. "Newer wins" reintroduces the
    invisible precedence this milestone exists to remove, making "which plugin am I
    running?" depend on a directory *and* a comparison. It cannot read a version
    without launching the thing it is deciding about: the kind is a name, not a
    version, so knowing "newer" first needs a filename convention, an install
    manifest that does not exist yet, or asking the plugin, which is circular. It
    turns an install into a mount outage, because a bundled backend is
    protocol-matched by construction and an external one is not, so a *newer* plugin
    speaking the next `ProtocolVersion` is refused at handshake and the mount fails
    *because* someone upgraded. And it makes a signature a permanent override, since
    anyone able to produce a signed artifact with a high version number wins
    forever — version precedence without pinning is what lockfiles exist to prevent.
    **The alternative that serves the same goal**: an external plugin wins because a
    *record* says so — kind, version, digest, key, written when someone installed it
    — so precedence is explicit and attributable, and "a newer one exists" is
    something `drivel plugin list` reports rather than something that happens
    silently at the next mount. That is M24's install manifest with no new
    machinery. Second, the opt-in half of the replace-versus-precede decision above:
    precedence shipped, the switch that would disable the external path did not.
    Third, whether `Loader`'s `refused`/`shadowed` machinery survives as the
    external path or is deleted with the search path it serves — which stays open
    rather than being answered by what shipped, since that machinery survived
    because the external path did, and it is the second question that decides the
    first.

26. **M24 — A plugin registry.** A named, versioned, verifiable way to find and
    install a `drivel-provider-*` executable, modelled on `registry.terraform.io`.
    Unscheduled, not started. The entry leads with what it is *not*, because the
    word invites three wrong readings: it is **not a package manager** (there is no
    dependency resolution to do — see below), **not a CDN** (it serves metadata and
    URLs, never bytes, exactly as Terraform's does), and **not by itself a
    code-signing authority**.

    **Build the protocol and the client before the service, and the reason is
    Terraform's own best decision.** Their registry protocol is implementable by a
    static file tree behind any HTTPS server — a discovery document, a versions
    list per plugin, and a per-version document naming the artifact URL, its digest
    and its signature — which is why private and air-gapped registries work at all
    and why the CLI needed no special case for them. Copy that shape exactly and
    `drivel plugin install acme/s3` is testable against a directory under
    `testdata` on day one. Whether anyone ever *operates* a public instance is a
    separate and much larger question — abuse policy, takedowns, and being a
    supply-chain target with a maintainer's weekend behind it — and it must not be
    a precondition for the client being useful.

    **The registry is two systems and only one of them is on the install path.**
    Both `registry.terraform.io/browse/providers` and `search.opentofu.org` are the
    *catalogue*: descriptions, download counts, publisher tiers, rendered
    documentation, full-text search. Terraform serves it from an undocumented `/v2`
    JSON:API; OpenTofu serves it from a Cloudflare Worker over a Postgres index
    built by a scraper that extracts docs and schemas out of every published
    repository. Neither is consulted by `terraform init`. The *install* protocol
    beside it is two unauthenticated GETs returning cacheable JSON, and that
    asymmetry is the whole lesson: the catalogue is a service with a database, an
    ingestion pipeline and an abuse policy, and it is **optional**. A drivel
    registry that starts with search has started with the half that cannot be a
    file tree.

    **What Terraform's protocol actually is, since the recommendation rests on its
    size.** A discovery document at `https://HOST/.well-known/terraform.json` maps
    `providers.v1` to a base path; `GET <base>/:namespace/:type/versions` returns
    every version with the protocol versions and platforms it supports; and `GET
    <base>/:namespace/:type/:version/download/:os/:arch` returns one package —
    `filename`, `download_url`, `shasum`, `shasums_url`, `shasums_signature_url`
    and the publisher's `signing_keys`. That is the entire surface. Nothing is
    authenticated, nothing is a POST, and no response contains a byte of the
    artifact: `download_url` points at GitHub releases or `releases.hashicorp.com`.
    OpenTofu implements the same two endpoints by **pre-generating every possible
    response** as a static object in a Cloudflare R2 bucket behind a CDN, with no
    API server anywhere — the existence proof that this shape is a file tree.

    **Four deliberate divergences, each paid for by something drivel does not
    have.**

    - *No discovery indirection; take a base URL from configuration.* Terraform
      needs `.well-known` because a provider is *addressed* by hostname in the
      configuration (`example.com/bar/baz`), so a hostname has to resolve to a
      path. drivel's config names a bare `kind`, and a registry is an install-time
      concern only, so the URL belongs to `drivel plugin install` and its config
      section rather than to the thing being addressed. This is not an invention:
      Terraform's own *network mirror* protocol drops discovery and takes a base
      URL directly, for this reason.
    - *Every path ends in `.json`.* Terraform's `…/download/linux/amd64` carries no
      extension, so the server has to supply `Content-Type` out of band —
      comfortable on R2, awkward on GitHub Pages, impossible with `python3 -m
      http.server`. Suffixing makes *any* static file server a conforming registry,
      which turns the `testdata` fixture into the same code path as production
      rather than a simulation of it. The layout is then
      `<base>/v1/providers/<namespace>/<name>/index.json` for the versions document
      and `<base>/v1/providers/<namespace>/<name>/<version>/<os>_<arch>.json` for
      one package.
    - *The digest is of the executable, not of the archive*, and this is where the
      analogy breaks in drivel's favour. A Terraform provider is a zip of a
      directory, which is why they carry two hash schemes — `zh:` over the
      distributed zip and `h1:`, a Go-modules dirhash over the extracted contents,
      so a provider installed from an unpacked filesystem mirror can still be
      verified. A drivel plugin is one file, so a single `sha256` of the executable
      is simultaneously the download check and the value go-plugin's `SecureConfig`
      verifies before exec. Two different numbers there make the pinning
      decoration: the thing checked is not the thing run. Compression becomes a
      transport detail, and a digest over the compressed artifact is at most a
      convenience.
    - *`protocol` is one number, and it is a filter rather than a display field.*
      §2.10's versioning rule gives drivel a single `plugin.ProtocolVersion`, so the
      versions document carries `"protocol": 1` where Terraform carries `["4.0",
      "5.1"]`. "No version solver" still means selection takes the newest version
      *whose protocol this host speaks*, not simply the newest. Skipping that makes
      `install` succeed and the mount refuse its handshake at boot, which moves a
      resolvable error to the least recoverable place there is.

    **Take OpenTofu's one extension: every platform in one response.** Their
    download document carries a `packages` map keyed `linux_amd64`, `darwin_arm64`,
    … each with that platform's hashes and size. Terraform makes a client issue one
    request per platform (`terraform providers lock -platform=…`) to record a lock
    file that works on more than the machine that wrote it. drivel acquires the same
    need the moment an install manifest is committed to a repository or baked into
    an image for a fleet of mixed architectures, and it costs one field.

    **The central design tension is that M9 made the filename the kind.** §2.10's
    rule 4 is deliberate: the kind is the `drivel-provider-<kind>` filename and
    nothing the plugin says about itself. A registry needs a globally unique name,
    which a bare word cannot be — `acme/s3` and `globex/s3` are two plugins and one
    filename. Three things have to be decided together and none is obvious: what
    the installed file is called, how the config file names a provider (today
    `kind = "gdrive"`), and what happens when both are installed. The
    recommendation is that **the registry name is the source's and the kind stays
    local**: installation writes `drivel-provider-s3`, records `acme/s3 v1.2.0` in
    an install manifest beside it, and a second namespace claiming that kind is
    **refused at install time naming both** — never resolved by a rule.
    First-on-the-path-wins is tolerable for a kind today only because nobody can
    install two by accident; a registry is precisely the machine for doing that.

    **No version solver, and this is a real difference from Terraform rather than a
    simplification of it.** Terraform needs one because one configuration draws on
    many providers and modules with shared constraints. drivel has one provider per
    mount, no dependencies between plugins, and no graph to satisfy — so an install
    takes an exact version or the newest and records what it got. Constraint syntax
    (`~> 1.2`) buys a solver, a lockfile format and a class of "no solution" errors
    in exchange for nothing this program has.

    **Verification stops being optional here, which changes a standing note.**
    §2.10 records checksum pinning as "not implemented" and M23 argues it becomes
    moot for an *embedded* plugin, because a digest of a payload you already carry
    is a digest of yourself. A registry reverses that: the binary now arrives over
    a network from a third party, so a digest recorded at install and re-checked
    before exec is the floor — and it is exactly what go-plugin's `SecureConfig`
    already offers, so the code is small and the decision is the whole cost. The
    signature question is harder, and Terraform's answer is the one part **not** to
    copy: their registry distributes the artifact's signature *and* holds the
    publisher's public key, so compromising the registry compromises the trust root
    it exists to provide. Wherever drivel anchors that key, it must not be the
    registry.

    **What their verification does, and why it is circular for everyone but
    HashiCorp.** The client downloads the artifact, fetches `SHA256SUMS`, fetches
    `SHA256SUMS.sig`, verifies that signature **with the public key the registry
    handed it in the same response**, then checks the artifact against the
    now-trusted sums file. For HashiCorp's own providers the key is pinned in the
    Terraform binary and the chain is anchored outside the registry; for a
    third-party provider the registry supplies both the signature and the key that
    validates it, so compromising the registry compromises both halves. The
    `trust_signature` field exists to break that circle and is in practice empty.
    OpenTofu's anchor is that the publisher opened a GitHub issue from an account
    with public membership of the organisation — worth recording as the *pattern*
    (an anchor outside the registry) rather than as the mechanism.

    **Their defence that works is downstream, and it is the one to copy.**
    `.terraform.lock.hcl` records the hashes observed at first install, and every
    later install is checked against the file in the user's repository — trust on
    first use with a durable record, in a file a human reviews in a diff. drivel's
    version is an **install manifest beside the binary** recording `acme/s3`, the
    version, the source registry and the executable's `sha256`, re-checked before
    every exec through `SecureConfig`. It needs no cryptography beyond a hash; it is
    the same record the naming rule above needs in order to *name the incumbent* in
    a collision refusal; and it could have shipped before any signature scheme
    existed, which is what made the signature question deferrable rather than
    blocking.

    **It is no longer deferred: §9/M23 decided the scheme on 2026-10-01 and this
    milestone inherits it rather than choosing again.** Ed25519 over a
    `(kind, version, sha256)` manifest, verified against a pinned key, with
    `crypto/ed25519` from the standard library — which is the "prefer
    minisign/signify or `ssh-keygen -Y` over GPG" instinct this entry already
    carried, now settled and for a sharper reason: verification happens at mount
    startup, offline, so OpenPGP's deprecated Go support and a CA's network-based
    revocation are both unavailable rather than merely heavy. The consequence here
    is one field: **the install manifest records the author's key alongside the
    digest**, which upgrades trust-on-first-use from "the same bytes as last time"
    to "the same author as last time" and lets a rebuild be accepted without a
    re-prompt.

    **The registry carries the signature and must never carry the key.** The key is
    pinned at install — in drivel's own configuration for a third-party author, or
    compiled into the binary for drivel's own builds — and the response carries a
    `signature_url` at most. A registry serving both halves is precisely the
    circular anchor rejected above, and the whole reason `trust_signature` is empty
    in practice over at Terraform.

    **Digest pinning assumes a published version is immutable, so put that in the
    protocol rather than discovering it.** OpenTofu needed a written immutability
    policy and a `versions_blacklist.json` to hold the line against authors
    retagging releases. A registry where `1.2.0` can change bytes turns every
    recorded digest into a false alarm and trains users to delete the manifest,
    which is the one outcome worse than never having written it.

    **What operating a private one costs is the test of whether the shape is
    right**: `aws s3 sync ./registry s3://bucket`, a GitHub Pages repository, or
    `nginx` over a directory. That falls out of the base URL and the `.json`
    suffixes for free, and it is the entire air-gapped story. Any later change to this protocol that breaks it is
    wrong, whatever else it buys.

    **It depends on M23's one real decision in a way that could kill it, and that
    decision is now half made.** M23 shipped with bundled backends *preceding* the
    external path rather than replacing it — a bundled kind wins, and every other
    kind still comes off the search path — so the install target this milestone
    needs exists today. What M23 did **not** ship is the switch that would disable
    the external path wholesale, which it still recommends; an installed plugin is
    an external plugin, so M24 remains a consumer of that switch. If bundled-only
    ever wins, this milestone has **no install target** and should be withdrawn
    rather than reconciled — a host that must be rebuilt to gain a backend is a
    defensible product, just not one with a registry.

    **And M23 narrows what is left for this milestone to solve.** The registry's
    original motivation was that installing a backend required a Go toolchain; a
    bundled default build answers that for every backend drivel ships, leaving the
    registry to serve *third-party* backends only. That is a smaller and less
    urgent problem, and it moves the authentication question to the front: M23's
    external path and M24's install path need the *same* answer about how a plugin
    proves what produced it, so decide it once for both. The digest decided above
    is half of it — a signature over that digest, verified against a key the host
    pins, is the other half.

    **The licence question this entry carried is answered, and the answer came from
    relicensing rather than from resolving it.** Under AGPLv3 a plugin importing
    `github.com/syshlted/drivel/provider` plausibly had to be AGPL itself, while one
    speaking only protobuf was the §2.9.2 arms-length case — two routes to the same
    seam with different answers, which is intolerable for a registry that hosts what
    other people wrote. MPL-2.0 dissolves it: the copyleft reaches Drivel's own files
    and no further, so importing `provider` obliges an author to publish changes *to
    Drivel's files* and says nothing about their own. Either kind of plugin may carry
    any licence. The registry should still record a declared licence per plugin
    because users need to know what they are installing — but it is metadata now, not
    a gate, and no answer has to be got in writing before the first upload.

    **Unsettled, in the order that matters.** The M23 dependency, because it decides
    whether there is anything to install at all — narrowed by what M23 shipped,
    since the search path survived, so what is left of it is whether the opt-in
    switch ever turns the external path off. Then the naming decision above,
    because it decides the config file's syntax and so becomes a breaking change if
    deferred. Then the trust anchor, now narrowed rather than open: the install
    manifest above is the recommendation and needs no decision to ship, so what is
    left is only whether signatures are in scope at all and, if they are, where the
    key lives. Then whether this protocol should also carry
    the CLI/completion manifest a host needs to describe a plugin's settings — and
    the answer is no: that manifest describes the plugin **you have installed**, has
    to work with no network, and is derived from the executable itself, so it
    belongs to plugin loading and not to distribution.

27. **M25 — Logging.** Unscheduled, not started. Three faces — levels and
    structured output, destinations and rotation, and what a log line is allowed to
    name — grouped because they are one decision about a single stream. They are
    split from the fourth thing the word "logging" is used for: a durable record of
    what synced and when belongs to **M14**, not here. A log line is something a
    human reads once; sync history is state a tool queries, and the two have
    different lifetimes, different schemas and different authorities. Building the
    second one here would break M14's "a view, never an authority" rule by the back
    door, because a log somebody greps to answer "did this sync?" is a status API
    with no schema and no version.

    **What exists is less than the `-debug` flag suggests, and saying so is where
    the milestone starts.** `drivel` has no verbosity control of its own at all.
    `-debug` sets go-fuse's `MountOptions.Debug` (`mount.Options.Debug` →
    `vfs/backend.go`) — a FUSE *protocol* trace from another program, off or
    firehose, that says nothing about sync. Everything drivel says in its own words
    goes to a `*log.Logger` at one implicit level, across ~124 call sites. M16's
    `logfile=` is the only control over *where*, and only on the fstab path.

    **The `logf` methods are the leverage, and the reason this is smaller than it
    looks.** Every one of those sites already goes through a per-mount method —
    `e.logf`, `d.logf`, `n.logf`, `m.logf`, the loader's and the plugin proxy's —
    rather than `log.Printf`, so a level argument, a structured back end and a
    redaction filter can all land behind them without touching the callers. The
    only bare `log.Printf` outside those methods' own nil-logger fallbacks is in
    `cmd/drivel` (the pprof server, the fstab helper), which is process-level by
    construction and stays that way. `docs/dev/conventions.md`'s "never
    `log.Printf`" stops being a tidiness rule here and becomes the interface.
    Whatever is decided below, **per mount stays**, and a single mount's output
    stays byte for byte what it prints today (§2.8): a process-wide logger would
    put two accounts' filenames in one undifferentiated stream, which is the
    cross-mount coupling `app.Validate` exists to prevent, arriving as an output
    format.

    **`log/slog` is not the dependency `docs/dev/dependencies.md` declined.** That
    table's "a logging library" row rejects a *third-party* one as the first
    dependency whose value is a matter of taste, and that judgement stands. slog is
    the standard library, its text handler can be made to print what drivel prints
    now, and its `Handler` interface is the natural seam for both the redaction
    filter and the fan-out to a destination — so the row needs rewriting rather
    than overruling, since what it protects against is a format nobody can change
    and a dependency that owns the process's output, and slog is neither. The cost
    is real and should be recorded with the decision: an attribute-per-line style
    spreads through call sites over time, and `logf("%s: %s", …)` reads better than
    the alternative at every site no machine will ever parse.

    **Redaction is the half whose default is wrong today.** drivel's log names
    synced paths and filenames verbatim — the same data `-pprof` binds to loopback
    for and `/debug/pprof/cmdline` goes unregistered over — except that unlike a
    profiler the log is written to a file someone else may read, shipped to a
    collector, or pasted into a bug report. The question is therefore not whether
    to offer a quiet mode but what the *default* line may contain, and the likely
    answer is a level boundary (paths at debug, counts and outcomes at info) rather
    than a switch nobody sets. Two things are load-bearing and easy to miss. A §6
    conflict copy's *name* contains the original path, so the line announcing one
    leaks in quiet mode too. And a plugin's stderr is forwarded verbatim onto the
    mount's logger (`plugin.lineWriter` → `process.logf`), so a backend this repo
    did not write can put anything it likes into a stream drivel would be claiming
    to have filtered — that forwarding is the boundary where redaction cannot be
    enforced, and the honest options are to say so wherever the filter is
    documented or to prefix the line and never filter it. The second is the same
    rule that governs every other plugin claim: drivel cannot answer for software
    it did not write.

    **Destinations and rotation are where an unattended mount fails.** M16 rule 3
    gave up journal integration deliberately — the daemon's stdio goes to
    `logfile=` or `/dev/null`, because a descriptor inherited by `mount -a` blocks
    until unmount — so a file that nothing rotates is the shipped answer for
    exactly the mount that runs longest and is watched least. Two routes, not
    equivalent. **External rotation** (logrotate, newsyslog) costs a reopen on
    `SIGHUP` and one documented example per platform, and leaves retention where an
    operator's other retention policy already lives; it needs a signal handler in a
    process whose existing signal handling means "unmount", which is the whole risk.
    **Internal rotation** is a dependency (`lumberjack` is the conventional one) and
    needs no configuration anywhere, which is the entire argument for it on a
    desktop mount. A syslog or journald destination sidesteps both, at the price of
    `log/syslog` for the first and a cgo-free journal writer for the second.
    Recommended order: reopen-on-`SIGHUP` first — small, unblocks the packaging
    story, forecloses neither of the others.

    **Unsettled, in the order that matters.** Whether the handler is slog, because
    it decides what a call site looks like and every later choice hangs off that
    `Handler` seam. Then the default level and what a line at that level may name,
    because it is user-visible and becomes a compatibility surface the moment
    anyone greps it. Then rotation. Deliberately out of scope: a log the program
    reads back (M14); per-package level filtering, since the unit a user thinks in
    is the mount and they already have that; and anything that makes `-debug` mean
    two things — if go-fuse's trace ever moves behind a drivel level, the flag keeps
    naming the FUSE trace and the level is a separate name.

28. **M26 — Credentials in a secret service.** Unscheduled, not started, and
    **deferred on 2026-10-06** pending the master-passphrase question below,
    which may replace the whole shape. An opt-in, per-account alternative to
    keeping an OAuth refresh token in a `0600` file: `drivel login` stores it in
    the desktop secret service, and a mount reads it back from there instead of
    from `token.json`. **It is a secret-at-rest change and not an isolation
    one**, which is worth saying first because the obvious reading is the wrong
    one and because this file is where an overclaim would start.

    **What it buys is narrow and must be documented in those terms.** A secret
    that is not a plaintext file in `~/.config` survives the ways a config
    directory escapes: a dotfile backup, a synced or shared home directory, an
    accidental `tar`, a screenshare, a support request with "here is my config".
    What it does **not** do is isolate the token from other processes running as
    the same user — anything that can `cat` the file today can ask the secret
    service tomorrow, and nothing in a session-keyring model changes that. The
    honest sentence is "it is not lying on disk in the clear", and anything
    stronger is the kind of claim `docs/project/marketing.md` refuses on drivel's
    behalf everywhere else. A feature whose documentation overstates it is worse
    than no feature, because a user who believes the stronger claim stops taking
    the precautions the weaker one still requires.

    **There is no per-application access control, and the protocol has no room
    for one.** Recorded because it is the first thing anyone asks, and because
    the answer is structural rather than an implementation gap: the interface is
    Service → Collection → Item, and `SearchItems`, `Unlock` and `GetSecret`
    carry no caller identity in any signature, so there is no field a policy
    could key on. The absence was arrived at rather than overlooked.
    gnome-keyring once had per-application access control, with the "an
    application wants access to the password for…" prompts, and dropped it: the
    caller was identified by its executable path out of `/proc`, which is
    worthless between processes of one uid — ptrace the authorised process,
    `LD_PRELOAD` into it, or ignore the bus and read the unlocked keyring's own
    files. KWallet still prompts per application, but the application *name* in
    that prompt is supplied by the caller, so it is a label and not an
    authenticated identity. Where enforced per-app secrets do exist on Linux the
    keyring is not what enforces them: Flatpak's
    `org.freedesktop.portal.Secret` can give each app its own secret because the
    **sandbox** vouches for the app id. Outside a sandbox there is no identity to
    bind a policy to — M9 rule 8's distinction between a built environment and a
    sandbox, arriving in a second place.

    **GNOME and KDE are one backend, which is most of why this is tractable.**
    KWallet 5 and later implement the freedesktop **Secret Service** D-Bus
    interface, the same one `gnome-keyring` serves, so one client covers both
    desktops and anything else implementing it (KeePassXC does). There is no
    per-desktop code to write and no desktop detection to get wrong; what exists
    on the session bus either answers the interface or it does not.

    **The token is already read-only at mount time, and that is what makes the
    rest small.** `gauth.SaveToken` has exactly one caller, `drivel login`. The
    provider only ever `LoadToken`s and hands the result to
    `oauth2.Config.TokenSource`, which refreshes the access token in memory and
    never writes it back — so a refreshed access token is *already* discarded when
    the process exits, and the only thing that has to persist is the refresh
    token, which Google does not rotate. A secret source therefore needs **read at
    mount and write at login**, and nothing else: no write-back callback across
    the plugin seam, no new protocol method, no change to how refresh behaves. An
    earlier sketch of this assumed the opposite and was much larger.

    **The host resolves the secret, and `plugin/env.go` must not grow an entry for
    this.** The token is read below the seam today, where D-Bus is deliberately
    unreachable: the allowlist drops every `XDG_*` variable (M16's privilege-drop
    lesson, so `XDG_RUNTIME_DIR` and the `$XDG_RUNTIME_DIR/bus` fallback are gone)
    and has no `DBUS_SESSION_BUS_ADDRESS`. Adding one would be **strictly worse
    than the `SSH_AUTH_SOCK` exception it would cite as precedent**: an ssh agent
    signs with ssh keys and nothing else, while the secret service hands over
    every secret the user has stored — other applications' tokens, browser
    passwords, unrelated accounts. One allowlist entry would give every backend,
    including an out-of-tree one, read access to the whole keyring, which turns M9
    rule 8's narrow and documented disclosure into an unbounded one. So the host
    performs the lookup and passes the **value** in the `provider.Config` TOML it
    already builds; the secret crosses a private unix socket to a process the host
    launched a moment earlier, which is barely a change from a plugin opening the
    file itself.

    **A boot mount cannot reach a secret service, and refusing is the only safe
    answer.** Both implementations need a session bus and an unlocked login
    keyring; an fstab mount ordered before `local-fs.target` has neither, and
    there is nobody to answer an unlock prompt — the SFTP host-key rule (M18 rule
    6) in a second place. A keyring-sourced account must therefore **fail at open,
    naming the account**, and must never fall back to a file. The fallback is the
    trap rather than the kindness: if an unreachable keyring silently reads
    `token.json`, the file still has to exist, so the feature has bought nothing
    at all while appearing to have worked. M8 rule 6's shape, with a worse
    consequence than a mistyped key.

    **systemd credentials are the complement, and the one mechanism here with
    granularity that is actually enforced.** `systemd-creds encrypt` seals a blob
    to the host key and optionally to the TPM with PCR binding;
    `LoadCredentialEncrypted=` on a unit has systemd decrypt it into
    `$CREDENTIALS_DIRECTORY` as a `0400` file on a private tmpfs, scoped to that
    unit and reachable from no other. The enforcement is the service manager's,
    which is why it can be granular where a keyring cannot, and a TPM seal means
    the ciphertext does not yield the token off a disk read elsewhere. It costs
    little to accept, because `gauth.LoadToken` already takes a path: the read
    side is a path the unit supplies rather than a second secret source. Two
    things to settle rather than assume. It cannot be said in an fstab `-o` line
    — credentials are a unit setting, so this is a drop-in or a hand-written
    `.mount` unit, which makes it a documentation shape more than a code one. And
    the interaction with M16's `run-as=` privilege drop needs checking, because
    the credential directory's ownership and the moment of the drop have to
    agree. The split it implies is the natural one: a session mount reads the
    keyring, an unattended mount reads its unit's credentials, and neither has to
    pretend to cover the other's case.

    **GPG is declined, and the reason is the daemon.** `gpg --decrypt` needs
    pinentry to prompt, and a mount brought up at boot or from a unit has no tty
    and may have no display; a passphrase-less key is a key file again with more
    moving parts. `gpg-agent` with a pre-cached passphrase works, but that is a
    keyring with worse ergonomics and an extra dependency, reached through a
    subprocess. Nothing here forecloses it — the secret *source* is the extension
    point — but it should not be in the first implementation.

    **The dependency is the real cost and belongs in
    `docs/dev/dependencies.md`.** Two routes. `godbus/dbus/v5` is pure Go, and the
    surface needed above it is roughly one `Lookup` plus unlocking a collection,
    so a small in-tree client keeps §2.9's cross-compile property and adds no
    runtime requirement beyond the bus. `zalando/go-keyring` is less code here but
    shells out to `secret-tool`, which means the secret transits a child process's
    output and the feature depends on an external binary being installed — on a
    program whose whole premise is holding credentials carefully, that is the
    wrong trade. **macOS Keychain is deferred** for the same reason and is not a
    parity gap to close early: it needs cgo or `security(1)`, §2.9's pure-Go build
    is worth more, and the platform is still compile-verified only.

    **It needs the provider-declaration framework, and it is that framework's
    third consumer.** The host cannot know which of a provider's settings name
    secrets — `internal/config` must never learn what a Drive folder ID is, and
    M8 rule 4 cuts both ways — so a provider has to *declare* that one of its
    settings is secret-valued before the host can resolve it. That is the same
    `provider.Params` shape **M12** needs to open an inner store through the
    registry, and the same one the M17–M21 group's shared precondition needs so a
    provider can declare the local paths it will touch before `app.Validate` runs.
    Three unrelated milestones want one piece of framework; building it once, with
    three consumers in view, is a better use of the design than three narrow
    hooks. This milestone is the cheapest of the three to validate it against.

    **A master passphrase is a different shape and may be the better one; it is
    what this entry was deferred on.** rclone's is the reference: one passphrase
    encrypts the config, supplied interactively, through an environment variable,
    or by a `--password-command` that runs something else to produce it. Three
    things make it attractive here. It is **backend-neutral**, which *answers*
    the first unsettled item below instead of inheriting it — one mechanism
    covers Drive's refresh token, an SFTP key passphrase and whatever a later
    backend keeps, where a keyring is wanted by Drive alone. It needs **no
    session service**, so it works over ssh, in a container and at boot, which
    are the three places the keyring path has to refuse. And a password command
    is **delegation, the same move M18 rule 6 makes** when it answers an
    encrypted key with an agent — so the passphrase source can be a keyring,
    `pass`, a TPM or a unit credential, which makes the two designs above *back
    ends of this one* rather than alternatives to it. That is the strongest
    argument for it: it subsumes them instead of competing.

    **What it would cost is M8 rule 5, unless only the values are encrypted.**
    The config file is TOML because comments survive, it is appended to and never
    re-serialised, and it is meant to be hand-edited and hand-read; encrypting
    the whole file ends all three, and rclone does not pay that cost because its
    config is a wizard's output rather than a document. So the shape that fits
    drivel encrypts **secret values and not the file** — a setting whose value is
    ciphertext under the master key, while the config around it stays plaintext,
    commented and editable. That also keeps the blast radius honest: paths,
    folder IDs and hostnames are metadata rather than secrets, and a design that
    encrypted them would be making a promise about a screenshare it cannot keep,
    which is this entry's own first rule. The trade to weigh against the keyring
    is ergonomic and not security: PAM unlocks a login keyring once for a whole
    session, where a passphrase is typed per mount unless it is delegated — and
    delegation lands back on a keyring for the desktop case, so the honest
    comparison is *one mechanism with pluggable sources* against *two mechanisms
    with none*.

    **Unsettled, in the order that matters.** First, master passphrase or secret
    service, because it decides whether anything below this line is a question at
    all: the two paragraphs above are the argument for the former and it is not
    settled. Then whether this is a per-account concern above the seam at all,
    because **SFTP already solves the same problem correctly and differently**:
    M18 rule 6 refuses a password and a passphrase option outright and defers to
    `ssh-agent`, so the only backend that wants a
    keyring is Drive — and the reason is specific, that an OAuth refresh token has
    no agent to defer to while an ssh key does. If the answer is "Drive-shaped",
    this may be a `gdrive` setting rather than an account-level one, which changes
    where the lookup lives and makes the declaration framework unnecessary for
    *this* consumer. Then the item's attributes, because they are a compatibility
    surface the moment anyone reads the item with `secret-tool` or recovers one by
    hand. Then whether `login` writes to the keyring by default once it can: the
    `-pprof` precedent says the surprising thing stays off, and a default that
    silently depends on a running session service would make `drivel login` fail
    over ssh. Then migration of an existing `token.json`, which wants an import
    rather than a fresh consent round trip. Deliberately out of scope: a passphrase
    drivel *persists* anywhere, which is what M18 rule 6 actually forbids — a
    credential in cleartext beside the thing it protects. The master-passphrase
    paragraph above is not that and survives the rule for a stated reason: it is
    prompted for or delegated and stored nowhere, which is the same distinction
    "encrypted key ⇒ agent" draws. Also out of scope: prompting at a moment
    nobody can answer, which is the boot case and is what delegation is for; and
    `credentials.json`, since an installed application's client secret is not
    confidential by Google's own account and protecting it would imply
    otherwise.

29. **M27 — The sync path's two silences.** **Shipped 2026-10-12; part 4 added
    2026-10-16, part 5 (the download half) 2026-10-17.** Two defects that are not bugs in what the engine *does* — it
    behaves correctly both times — but in what it tells the person depending on
    it. They are one milestone because the shape of the fix is the same in each:
    the engine already holds the fact and discards it. Part 4 is the same shape
    a third time, and it closes the one gap this entry had been carrying as
    unscheduled: a push lost to a crash rather than abandoned by the engine.

    **What building it changed.** Part 1 landed as designed. Part 2 did not: the
    cheapest of its three options turned out to repair nothing, which is in
    "What shipped" below and is the one thing to read before touching this
    again. Part 4 landed as the candidate this entry recommended, with two
    additions that only appear once it is wired up — a boundary that keeps the
    sweep out of the running engine's way, and a baseline record at M6's gate 3
    without which the row cannot converge.

    **Part 1 — back-pressure is invisible.** The outbound path is bounded in two
    places, and at the defaults the bound is 1280 events: `eventBuffer` (1024,
    `internal/app/mount.go`) plus `DefaultWorkers` × the 64-slot per-worker queue
    (`engine.go`). Past it the mount blocks, in this order — `dispatch` sends into
    a path-hashed worker queue and waits when that queue is full, which stops the
    coalescer goroutine reading `events`; `events` then fills, and `vfs.node.emit`
    blocks the FUSE handler that is trying to report a close. MC-11 measured a
    1286-deep backlog against that 1280, which is exactly this: the queues full
    and a `write(2)` waiting on Drive.

    **That is correct and must not be removed.** Dropping an event loses a file
    until a sweep; a larger buffer only defers the stall and lengthens the bounded
    drain at unmount. What is wrong is that **nothing in the log says it is
    happening**, so `tar -x` into a mount runs at the provider's upload rate —
    1.09 uploads/s, measured on Drive — and the only available conclusion is that
    drivel is broken. Correct back-pressure, invisible to the one person
    experiencing it.

    The fix is a line on *entering* and *leaving* saturation, naming the depth and
    the settings that govern it (`-upload-workers`, `-push-delay`), rate-limited
    so a two-hour copy logs twice rather than ten thousand times. Three things are
    load-bearing. It must be measured **where the blocking happens**, not where
    the event was created: a depth sampled in `emit` cannot tell a full worker
    queue from a slow provider, and those want different advice. The rate limit is
    on the **state transition**, not on time — "entered saturation at depth N",
    "left it after T" — because a periodic depth report is M14's job and not a log
    line's. And it must not arrive as a verbosity knob: M25's first rule is that
    `-debug` is go-fuse's FUSE protocol trace and must never come to mean two
    things.

    This is the measurement M14 would report and M25 would format, and it waits
    for neither — the line is the fact; an API and a level are presentations of it.

    **Part 2 — an abandoned push is forgotten, not merely abandoned.**
    `executeWithRetry` logs `[sync] push … failed after N attempt(s)` and returns,
    which **drops the event**. Two paths reach that branch and the second is the
    one that surprises: `attempt >= e.retry.max` after
    `defaultRetry{max: 5, base: 500ms, cap: 30s}` — about 7.5s of jittered backoff
    — and `!provider.IsRetryable(err)`, which drops on the **first attempt with no
    retry at all**.

    Giving up is right. Retrying a permission error forever is a spin, and an
    unbounded persistent outbound queue is a second durability surface over the one
    §12 already prices. **What is wrong is that the knowledge goes with it**:
    nothing marks the path, nothing counts it, nothing schedules the repair. The
    repair already exists — M7b's local walk re-pushes a file that diverged from
    its baseline — but it is never *told*, so recovery waits for
    `-sweep-interval`, whose Drive default is 24h and which `0` disables outright.

    **A systemic cause does not bound the loss to one file**, which is the
    tempting reading and the wrong one. Five consecutive transient failures do
    usually mean something larger than the push — but 7.5s is short enough to be a
    wifi handoff or a laptop resume, and if the cause really is systemic then every
    push in flight and every one arriving during the outage is dropped the same
    way. A copy crossing a 60-second outage loses the whole burst, and the engine
    goes on accepting events as though nothing had happened.

    Two things are decided. **Do not raise `max`** — more attempts trade a silent
    loss for a longer stall and leave the forgetting untouched. **Do not build a
    persistent outbound queue** — the point is to re-arm the existing repair, not
    to invent a second one. What is left is a choice of three, in increasing cost:
    abandonment sets a flag that makes the next sweep unconditional, so
    `-sweep-interval` stops being the only trigger; or the path is recorded in
    `internal/state` as owed and the sweep's walk consults it; or both, with a
    count in the sweep summary. The first alone is one key and one bool and is
    enough to make an abandoned push *recoverable*; the second is what buys
    repairing one path without polling a whole tree, which is what a sweep costs on
    a feedless backend (M18 rule 2).

    **None of the three starts a sweep on the spot, and that is deliberate.** The
    multiclient plan's Group V write-up reaches the same question from the failure
    side and declines the obvious answer for a good reason: a sweep is the most
    expensive thing the system does and the most dangerous (MC-25, MC-53), so
    firing one off a transient failure trades a known small loss for an unknown
    large cost. Re-arming the *next* sweep, or recording what that sweep owes, buys
    the repair without buying the risk — which is why the distinction between
    "schedule a sweep" and "make sure the next one covers this" is the whole design,
    not a detail of it.

    **What shipped.**

    *Part 1* is `backpressure` in `engine.go`, driven from `dispatch` — the one
    place that can see the outbound path run out of room, because it is where
    the send blocks. One line on entering an episode (depth, bound,
    `-upload-workers`, `-push-delay`) and one on leaving it (duration, how many
    writes waited, what is still queued). Three things it needs that the entry
    above did not name. **Hysteresis is the feature, not a refinement**: in
    steady saturation every dispatch finds room the instant a worker frees a
    slot and then refills it, so "did this send block?" flips thousands of times
    a minute, and a mutation removing the episode check produced **101 reports
    where 1 was wanted** against the test that pins it. And **the thing that
    ends an episode is the event that never arrives** — the copy finishes, the
    queues drain, and no further dispatch happens to notice — so there is a
    sampler (`satSample`, 1s) armed only while saturated. It samples; it never
    reports on a tick. And **the 1280 bound is the best case, not the bound a
    given workload meets**: paths are hashed to workers, so a burst concentrated
    on one worker blocks once *its* 64 slots are full while the other three sit
    idle, making the practical bound `eventBuffer + 64`. Running it printed
    "646 of 1280" the first time, which is why the line reports the depth it
    reached instead of asserting the queue is full — the wording it shipped with
    was the second one written.

    **What the episode check is keyed on is MC-12's correction (2026-10-14), and
    the sentence above about the bound is why the shipped version could not
    work.** It shipped ending an episode when the depth fell to a quarter of the
    bound (`satLowWater`, now gone). A bulk copy never reaches that state: 1100
    files into one directory plateau at ~254 queued, *below* the 320 a quarter
    of 1280 asks for, and the rest of the backlog — 846 events — sits in the
    coalescer's debounce map, which the depth closure did not look at. So the
    clear condition was true for the entire 35-minute drain, every freed slot
    opened an episode and the next tick closed it: **113 episodes and 112 clears
    over 413 uploads**, each "cleared" line reporting 253 events still queued.
    That is the ten-thousand-line log this part exists to avoid, arriving by a
    route the design had already half-described and not followed through.

    The rule is now two conditions, and the first is what makes it unflappable:
    an episode ends when **the coalescer has nothing left to hand to a worker**
    (its event channel is empty and no path is inside its debounce window) *and*
    **no dispatch waited since the previous tick**. The first is stable by
    construction — with nothing left to dispatch, no further dispatch can block
    until new events arrive, so the condition cannot oscillate; the second is
    what keeps a provider slower than the writer inside one episode while the
    backlog drains. `depth` now reports both halves of the outbound path, so the
    entering line carries the backlog as well as the queue — "256 of 1280
    event(s) queued, 846 awaiting dispatch" — and the second number is the one a
    bulk copy makes large. One shape still reports more than once, honestly: a
    slow trickle of writes against a saturated worker pool is a sequence of
    genuine episodes, because each write really does wait and the pool really
    does empty between them.

    **MC-36's "one line per client and nothing after it" (2026-10-13) does not
    survive this**, and it is worth saying why rather than quietly dropping it.
    That run's shape is the one the regression test now reproduces, and it
    counts 15 clears against the shipped rule; its logs are gone with the rig
    reset, so the likeliest reading is that the number was taken by eye from
    part of a log. `scripts/multiclient/logcount.sh` counts both lines now, for
    exactly that reason — the assertion is the *pair*, and nothing in the
    harness was counting it. Until MC-12 the episode check had only ever been
    driven by tests, and the one test that drove it chose an unbuffered event
    channel, which is the single geometry in which a fraction-of-the-bound rule
    behaves: it made the bound the lone worker queue, so the backlog was visible
    in the depth. The test is buffered as production is now.

    *Part 2* shipped as the **second** option, not the first, and the reason is a
    measurement: **"set a flag that makes the next sweep unconditional" would
    have scheduled a sweep that repaired nothing.** The entry above says the
    repair already exists because "M7b's local walk re-pushes a file that
    diverged from its baseline". It does not. `pushLocalOnly` is the *new
    locally* row and skips every path that has an echo at all; an abandoned push
    is almost always a **rewrite of a file that has one**, whose baseline still
    describes exactly what the remote holds — so the remote pass matches the echo
    and correctly does nothing, the walk skips, and the file stays local forever.
    Confirmed by running it before any of this was written, and now pinned by
    `TestSweepRepushesAnAbandonedWriteToAnAlreadySyncedFile`, which fails with
    `pushes = []` the moment the new pass is removed.

    So the record carries the **path**: a `repair` bucket in `internal/state`
    (path → when it was first abandoned), written by `Engine.oweRepair` from the
    give-up branch, and consumed by `pushOwedRepairs`, a pass that runs in
    `runSweep` *before* the walk — a repair that succeeds writes a fresh echo, so
    the walk then skips the path for the ordinary reason and nothing is pushed
    twice. Four properties are load-bearing, and three of them are reasons from
    elsewhere in the tree arriving in a new place:

    - **`OweRepair` compares before writing** (§12's rule). Re-recording an owed
      path costs no commit, which matters because the non-retryable branch gives
      up after one attempt with no backoff, so a permission failure over a large
      tree abandons as fast as the engine can push.
    - **The record is cleared BEFORE the push, never after.** `Push` is
      synchronous and records a fresh debt if it gives up again; clearing
      afterwards would erase the one thing saying the file is still unsent. The
      cost of that ordering is a crash in between, losing one path's record —
      against a clear-afterwards that loses the repair itself, silently, every
      time it fails twice.
    - **The record is a path, not an operation and not content**, which is what
      keeps it from being the persistent outbound queue this entry declines to
      build. `repairEvent` re-derives what to send from the backing file as it
      stands: gone ⇒ drop the record, a directory ⇒ `OpMkdir`, a placeholder or
      a special file ⇒ drop it (M5, M15), otherwise a whole-file `OpWrite` with
      nil extents (M6's unknown-means-whole-file invariant).
    - **A rename is owed at its destination**, where the bytes now are.

    *Scheduling* is `armRepairSweep`: the first poll that sees a debt sets a
    deadline `repairDelay` (5m) out and says so; `dueSweep` pulls `nextSweep`
    back to it. Nothing sweeps on the spot, which is the objection this entry and
    the test plan's Group V write-up both insisted on keeping. A repair sweep
    that leaves the debt standing doubles the delay, out to 80 minutes, so a
    backend that is enumerable but failing is not swept every five minutes
    forever. **`-sweep-interval 0` honours it**, per the recommendation below —
    which meant `runWithoutFeed` could no longer return at interval 0 and now
    idles instead, waking at `repairDelay` to re-read the bucket. That is a poll
    rather than a notification on purpose: the engine and the downloader share
    the state store and nothing else, the record has to be consulted after a
    restart regardless, and a cross-object callback would write `nextSweep` from
    a worker goroutine, which is single-goroutine state by design.

    **Part 4 — a push LOST is not a push abandoned.** Parts 2 and 3 record a path
    when the engine gives up, and giving up is code that runs; `kill -9` runs
    none. MC-40 (b) measured what that left behind (2026-10-13, two processes and
    a local sshd): after a crash mid-upload of a *rewrite* of an already-synced
    file, the repair bucket is empty and no other row of the reconcile table
    proposes the path either — it has an echo, so `pushLocalOnly` skips it, and
    the echo still describes exactly what the remote holds, so the remote pass
    matches it and correctly does nothing. 44 sweeps over seven minutes, past
    `repairDelay`, changed nothing, with the local file holding the user's
    rewrite and the remote holding the previous content throughout. **A crash was
    therefore strictly worse than an outage**, which is the opposite of what part
    2 reads like on its own.

    Recording the debt *before* the push would close it and stays declined for
    the reason §12 gives: that is the persistent outbound queue, and it doubles
    the commit count of every push. What shipped instead is the candidate this
    entry recommended — **the sweep's local walk consults the baseline
    fingerprint that was already being stored**. `state.Echo.LocalSize`/
    `LocalMTime` were added in M18 for the hashless-provider case and
    `matchesBaseline` already read them for the delete rows; `repushDiverged` now
    reads them for the *push* row, so a path whose local copy no longer matches
    its own baseline is re-sent rather than skipped. No new state, no extra
    commits on the sweep's side, one `os.Stat` per file on a walk that already
    visits every file — and it covers the power cut, the OOM kill, the drain that
    timed out and the backing file written while the mount was down, none of
    which the repair bucket can ever see. It also finishes a §6 *local newer*
    resolution, which deliberately leaves the local edit "for the uploader to
    push" and therefore relied on an in-memory event that a restart has
    forgotten. One comparison serves both directions
    (`fingerprintDiverged`), because a push row and a delete row disagreeing
    about whether a file changed is a bug waiting to be written.

    **Four things narrow the row, and the first is the one that is not obvious
    from the proposal.** *The local mtime must predate this mount's open*
    (`Downloader.openedAt`, taken in `NewDownloader`, which `app.Mount.Open` runs
    before the filesystem is served). A write this process received is one the
    engine is already answerable for — in the debounce map, in a worker queue, in
    flight, or in the repair bucket if it was abandoned — and nothing is silently
    lost inside one process lifetime, because `vfs.node.emit` blocks rather than
    drops. So the divergences worth re-pushing are exactly the ones that predate
    the process, and dating them needs no clock heuristic and no tunable. Without
    that bound the row re-uploads a file a long copy is still writing, once per
    sweep — and on a feedless backend the sweep interval *is* the poll interval,
    seconds, so a single large copy would cost a whole upload per tick. It also
    leaves push-on-close deciding when a *held-open* file is sent, since its
    mtime keeps moving and the row never claims it. The cost is that a divergence
    created out of band during a long-lived mount waits for the next restart or
    `-resync`; writing the backing tree underneath a running mount is outside
    what drivel contracts for, and the alternative was a quiet period whose
    constant could not be defended.

    *A baseline with no local fingerprint is no evidence.* A zero `LocalMTime` is
    an echo written before M18 added the field, or a stat that failed. The delete
    rows already refuse to read that as "matches" (M18 item 12); the push row
    refuses to read it as "differs", for the mirror reason plus one of its own:
    a wrong guess here costs an upload and a commit for **every path in the
    tree**, where the delete rows' wrong guess is bounded by `-max-deletes`. It
    is counted and reported in the sweep summary rather than logged per path, so
    a state DB that answers this for everything says so once per sweep instead of
    silently covering nothing. *Only a path the sweep listed*: a baseline whose
    path the remote did not report belongs to the delete pass, which has strictly
    more to go on. *Never a path the sweep could not settle*: if the download or
    the §6 conflict copy for a path failed, what the remote holds is unknown, and
    a whole-file `Put` over it would publish one side of a conflict while keeping
    no copy of the other — so `applyPage` collects the paths it could not
    reconcile and the walk stands down on them, as it does on the paths
    `pushOwedRepairs` has already claimed.

    **M6's gate 3 now records the baseline it verified, and without that the row
    cannot converge.** A digest match against what `Stat` reports *now* is the
    strongest evidence the engine ever has that the two sides agree — stronger
    than the echo a successful `Put` writes, which is a claim about bytes we sent
    rather than bytes we compared — and it was the one place that evidence was
    discarded. A metadata-only change (a touch, an editor saving an unchanged
    buffer) moves the mtime without moving a byte, so the row proposes a push,
    gate 3 sends nothing, and the mismatch used to survive for the next sweep to
    find again: forever, at one full-file hash per mount, and at one whole upload
    per mount below `hashSkipMinSize`. One commit per skipped push settles it,
    which is the budget a push that uploads already has — §12's ratchet argued
    rather than crossed. `TestARepushedMetadataChangeSettlesAfterOneSweep` is the
    only place both halves are visible at once; it reports two repush lines where
    one is wanted the moment the record is removed.

    **Verified live on 2026-10-16, on both backends, as an A/B rather than a
    run** — the thing being tested is a silence, and a silence is not evidence on
    its own. Over a tmpfs SFTP rig, killing a 256 MiB rewrite at a measured 25%
    of its upload: the pre-fix build left the remote on the previous content
    after **39 sweeps** with no repush of either kind and the peer never
    converging, while the fixed build logged `repush … an earlier push never
    landed` on the **first** sweep after the restart and had the remote and the
    peer on the rewrite three seconds later. A third leg crashed on the old build
    and restarted on the new one — the shape a user actually meets — and repaired
    in two seconds, which also demonstrates the row working off a state DB an
    older build wrote, as it must, since the fingerprint fields have been stored
    since M18. The repair bucket was empty in all three, which is the premise of
    part 4 rather than an aside. Against real Drive the two backend-specific
    halves hold: the re-push **updates the object** rather than growing a second
    one at the path (asked of the API directly, because same-name siblings
    collapse into one local file), and the metadata-only case **settles after one
    pass** — one repush, one gate-3 skip, no upload, the remote untouched — with
    a genuinely changed file re-pushed in the same run as the control. The
    harnesses are `scripts/multiclient/mc40b-lostpush.sh` and `mc40b-drive.sh`.

    **Two things M27 still deliberately does not close.** An abandoned
    **deletion** is not repaired: there is no local content to re-send, and
    inferring it later would have to go back through `-max-deletes`, which is
    the delete pass's machinery rather than a re-push — the remote object is
    still there and still marked seen, so the delete pass never considers it.
    `repairPathOf` therefore declines to record one at all, rather than
    recording a path the repair pass would drop for having no file. And **a shutdown cannot overtake a blocked
    dispatch**: the send into a worker queue is unconditional (dropping is the
    thing this bound exists to avoid), so a cancelled context does not unwind a
    saturated engine, the engine's `drainTimeout` is only armed after the loop
    exits, and `app.Run`'s `<-engineDone` is unbounded. That is pre-existing and
    out of scope here; the honest fix is a per-attempt deadline on the provider
    call, which §6 declines for `ChunkTransferTimeout` for a stated reason. Worth
    writing down because part 1 is precisely about that bound.
    **Closed 2026-10-17 without that deadline**: `Engine.BeginShutdown` arms an
    abort from the shutdown *request*, and only a dispatch still blocked when it
    fires gives up — its event recorded for repair, the stop reported unclean.
    That bounds everything but a provider call ignoring its own cancellation,
    which is the part a per-attempt deadline would still be for. It also needed
    `vfs.Serve` to retry an unmount that failed EBUSY, which it had tried once.

    Unsettled: whether an abandoned push is also a per-path status through M14,
    where `unknown` as a first-class status already points. (M14 settled it:
    `error`, with a `detail`.)

    **Part 5, 2026-10-17: the download half.** MC-46 found that a failed inbound
    apply does not hold the cursor — the page's error is logged, the cursor
    advances, and the feed never offers the object again — so a dropped
    connection mid-download left the file stale until a sweep, which is a day on
    Drive's default and never at `-sweep-interval 0`. Holding the cursor was the
    rejected fix, and the reason is head-of-line blocking: a change that can
    *never* apply (a full disk, a name the backing filesystem refuses) would
    then stop inbound sync for every other path, for good. So the shape is
    part 2's, mirrored — a `refetch` bucket in `internal/state`, path → first
    failure — and four things about it differ from the push side on purpose.

    - **The retry is not a sweep.** A repair has to be a sweep because the walk
      is what re-derives an event from the backing file; a refetch needs only
      the remote's current state at one path, which is one `Stat`. So
      `refetchPass` Stats each owed path and runs the result through the feed's
      own `apply` — §4's echo test, the tombstone, the placeholder branch and
      §6's conflict copy all hold unchanged — on the pull loop's goroutine,
      between polls, so it can never interleave with a feed apply. That is also
      why the delay is a minute (`refetchDelay`) where `repairDelay` is five:
      there is no expensive operation to amortise, only a transient failure to
      let clear. Doubling to 64 minutes; a record from an earlier run is retried
      at once, by part 2's reasoning.
    - **It fetches what the remote holds now, never the change that failed.**
      The record holds no version for exactly that reason. A stale description
      replayed later is the one outcome worse than not retrying.
    - **A removal is not recorded**, part 2's abandoned-deletion rule from the
      other side: re-applying a deletion later deletes whatever is at the path
      by then, which may be unpushed work. A failed removal leaves the local
      copy in place, which is the state every delete guard prefers, and the
      sweep's delete pass has the evidence to finish it. Likewise a path the
      retry finds gone remotely is dropped with nothing local touched — the
      feed reports that removal itself, and if it already has, `ForgetMany`
      discharged the record first.
    - **It is discharged for free.** `SetEcho`, `ForgetMany` and
      `ForgetDeleted` each drop it inside the transaction they already commit —
      a path in agreement, a path gone remotely, a path we removed — so a retry
      that downloads costs the one commit the download costs, and §12's ratchet
      is untouched. A push's `SetEcho` discharging it is correct too: the retry
      would have found that echo matching and done nothing. It follows a
      directory move in `RenameSubtree`, as `repair` does.

    A sweep's failed apply is recorded as well, since the next sweep may be a
    day away; it was already withheld from the lost-push row through
    `noRepush`, and still is. M14 shows the path as `error` and counts
    `inbound.apply_failures` against `inbound.refetched`.
    `internal/syncengine/refetch_test.go` carries it; removing the record call
    in the pull loop and removing the discharge in `SetEcho` were each checked
    to fail the test that names them. **Not run live**: the in-tree fault
    injection fails the read side, and real ENOSPC on the write side is still
    MC-46's Tier B half.



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
at all — exFAT, an SMB share, most network mounts — would still present working
xattrs at the mountpoint. And because the store is
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

---

## 12. Durability of the two bbolt stores (decision record)

Written 2026-10-10, after MC-11's write-amplification finding (multi-client plan
§4.17) turned out to be a question about fsync policy rather than about bbolt
tuning. §12.1 and §12.2 are the measurements and what shipped; §12.3 is the part
that is deliberately *not* built, and why.

### 12.1 What a commit actually costs

There are two stores. `internal/state` is engine-level (the change-feed cursor,
the §4 echo records, the M5 hydration bitmaps, the M7b sweep record and its `seen`
marks) and lives in the host process. `internal/pathindex` is provider-private
(Drive's path↔fileID map, §2.5) and since M9 lives in the backend process. Neither
is shared between mounts; `app.Validate` refuses that.

Measured on bbolt 1.5.0 with the options the tree uses, 10 000 single-key write
transactions, bytes read from `/proc/self/io`:

| configuration | written | txn/s |
|---|---|---|
| default (one commit per key) | **365 MB** — 36 KB/commit | 749 |
| `NoFreelistSync` | 324 MB (−11%) | 746 |
| `NoSync`, `Sync()` after every commit | **362 MB** | 1 304 |
| `NoSync`, `Sync()` every 100 commits | 47 MB | 23 446 |
| `NoSync`, one `Sync()` at close | **5.4 MB** | 35 686 |
| batched 100 keys/commit, full durability | 45 MB | — |
| batched 1000 keys/commit, full durability | 7.7 MB | — |

Three things follow, and the third is the one that decides the design.

**Per-commit cost is a constant, not a function of the data.** 36 KB per commit
for a ~60-byte record, flat as the DB grows (36.0 KB/commit at 20 000 keys, 36.3
at 40 000). Disk cost is *commits × a constant*, so the only lever that matters is
the number of commits — which is why the regression gate counts commits and not
bytes. A byte count also depends on the filesystem underneath and is not
assertable in a unit test.

**It is the fsync, not the write.** Row 3 is the control: forcing one fsync per
commit reproduces the default's total exactly, and removing the fsyncs leaves 1.5%
of the bytes. A commit fdatasyncs twice — data pages, then the meta page that
makes them reachable — so the same handful of hot pages (leaf, branch, root,
freelist, meta) is rewritten every time. Page size is already the 4 KiB minimum,
the hashmap freelist is already chosen (§2.4), and `NoFreelistSync` is worth 11%.
There is no tuning answer here.

**A commit that changes nothing costs nearly as much as one that does.** bbolt
advances the meta page on every commit regardless, so an `Update` storing a
byte-identical value costs the full 36 KB, and an `Update` that touches nothing at
all still costs 20 KB. A `View` costs zero. That asymmetry is what §12.2 exploits.

### 12.2 What shipped: fewer commits, unchanged durability

Both stores keep bbolt's default two-fsyncs-per-commit. What changed is the number
of commits, and **only the redundant ones** — which is a narrower claim than it
first looked, so the table below is the honest version rather than a headline.

Measured against the fake Drive, per operation:

| operation | state | index before | index after |
|---|---|---|---|
| create a path that does not exist remotely | 1 | 1 | **1** |
| re-push an existing path (Stat for the M6 gates, then Put) | 1 | 2 | **0** |
| resolve a path off a warm index (every restart) | 0 | 2 | **0** |
| a sweep page over an unchanged tree | 0 | 1 per page | **0** |

1. **`pathindex.Set` and `SetMany` compare before writing, in a read
   transaction.** Not an early return inside the `Update` — that still costs
   20 KB. A *re-push* stored the same mapping twice for nothing, because both of
   its resolutions end in `rememberLocked`; an index *hit* verifies the stored
   entry and then re-stores exactly what it read; and a periodic sweep re-offers a
   whole page it already holds. A genuinely new path still costs its one commit,
   because that mapping genuinely is new. Both directions of the map must already
   agree before a write is skipped, because `setLocked` evicts the stale halves a
   rewrite leaves behind and a half-match still has work to do.

   **So a create-only burst is the one workload this does not help** — which is
   exactly MC-11's, every file being new. Its remaining per-file cost is the
   `SetEcho`, and the only lever left for that is §12.3's, which is held. The
   workloads this *does* help are the ordinary ones: editing a file that already
   exists, and every cold start against a warm index.
2. **`pathindex.Bind` takes the same read-first path.** Once per mount rather than
   once per file, so the saving is negligible — but re-binding an index to the
   identity it already carries wrote nothing and still cost a commit, and leaving
   the one obvious instance of the rule unapplied inside the package that defines
   it invites the next reader to conclude the rule is optional.
3. **Commits are counted** (`state.Store.Commits`, `pathindex.Store.Commits`) so
   the budget is assertable. M14's per-backend stats is the other consumer.
4. **An unreadable DB is replaced instead of fatal, in both stores** — see §12.4.

The remaining commit is one `state.SetEcho` per push, and it cannot be deduplicated
the way a mapping can: `Echo.At` is when the baseline was taken, which M7b compares
against the sweep's start token to decide whether a local copy predates the sweep.
Holding that timestamp still to save a write would break the delete guard.

### 12.3 What is NOT built: group commit, and why it is held

The obvious remaining lever is `NoSync` plus a periodic `db.Sync()`: 8× fewer
bytes at a 100-commit interval, 67× at close-only, and it needs no call-site
changes, because deferring the *fsync* is invisible to §4 while deferring the
*commit* is not — bbolt readers see committed-but-unsynced transactions, so an
echo is visible to the downloader the moment it commits whether or not it has
reached disk.

It is held, and the reason is sharper than "NoSync is risky".

**The failure mode is corruption, not a lost tail.** With `NoSync` the kernel may
write the meta page before the data pages it points at, so a crash in the sync
window can leave a DB whose valid, checksummed meta page references pages that
were never written. A *process* crash is harmless — the page cache survives
SIGKILL, a panic and an OOM kill. Only power loss, a kernel panic or a VM reset
reaches this.

**bbolt does not checksum data pages, so the failure can be a panic rather than an
error.** Open-time validation covers only the two meta pages, and `db.meta()`
panics outright if both fail (its comment says this is unreachable "because we do
fsync() on every write" — which is exactly the assumption `NoSync` removes). A
page that was never written reads as zeros, whose page type matches nothing, and
bbolt panics on it during a later read. So the prerequisite is not just
"recreate a file that fails to open" — which §12.4 now does — but surviving a
panic from *inside* a read. For the state DB that is a crashed mount; for the path
index it is worse, because M9 rule 6 relaunches a dead backend, so a panicking
index DB is a crash loop rather than a single failure.

**`Close()` does not sync.** bbolt's `Close` munmaps and closes the fd; under
`NoSync` nothing fsyncs on a clean unmount either. Any group-commit design has to
add that explicitly, at `Close` and at the barriers that already exist (the pull
loop's page boundary before `SetCursor`, and sweep completion).

So group commit is a coherent design whose prerequisite — a recover-from-panic
wrapper around every store access, loudly logged, discarding the store — is a
bigger and more invasive piece of work than the amplification it removes
currently justifies, now that the commit count is down to 1 per file. Revisit it
when a backend makes commits the rate limiter rather than a wear cost: against
Drive at ~1.09 uploads/s the fsyncs are invisible in wall clock, but M11's
content-addressed store, M17 `localfs` and M18 SFTP over a LAN all push far faster
than 749 commits/s.

**The cheaper thing to do first, if more is needed:** batch the *inbound* path's
echo writes at the page boundary the downloader already has. `flushForget` and
`SetCursor` are already there and already ordered — echoes durable before the
cursor advances. That keeps full durability and needs no panic handling. It is not
built either, because it is a change to the pull loop's ordering and wants its own
reasoning about what a page means when a path appears in it twice.

### 12.4 Why discarding a store is a defined state

Both stores are reconstructible by design, and the recovery path for each already
existed before this note; what was missing was taking it when the file is
corrupt rather than merely absent.

- **The path index is explicitly non-load-bearing** (§2.5, M7 invariant 2):
  deleting it costs latency and quota, never correctness, because an unknown path
  falls through to a name query against the provider, and a stored entry is
  verified before it is believed. `gdrive` already treated an open failure as
  non-fatal and ran from memory — but that fallback is *permanent* for the life of
  the process and silent beyond one log line, so a single bad write cost every
  later session a cold index. `pathindex.Open` now replaces the file too, on the
  same narrow predicate, and `Bind` was already emptying the store outright on an
  identity change, so nothing new is being claimed about safety here.
- **The state DB's loss is bounded and fails safe.** M7b infers a deletion only
  from a baseline, so no baseline means the path is new on whichever side it is
  found and **nothing is deleted** — the first-ever-run rule, applied to a
  recovered store. M5's placeholder marker is an xattr on the backing file, so no
  placeholder becomes an empty file (the note in §9/M10 corrects an earlier claim
  to the contrary). The hydration bitmaps are a cache. With no cursor the
  downloader enumerates the whole tree before tailing it, which is the
  reconstruction that is wanted, and nothing has to ask for it.

  What it costs is a full re-sync's worth of requests and a §6 conflict copy for
  every path whose two sides genuinely differ — which, with no baseline to say
  which side moved, is the correct answer and not a side effect.

Both stores therefore move an unreadable file aside and start empty, reporting the
fact through `Store.Recovered` so the caller can log it — loudly for the state DB,
because a re-enumeration and conflict copies follow, and briefly for the index,
because nothing follows but name lookups. Refusing protected
nothing: the file is already unreadable, so there is no state left to lose, and
the operator's only recourse was to delete it by hand — the same outcome with
downtime and without a copy kept for diagnosis. An unattended boot mount (§9/M16)
has nobody to do even that.

**The predicate is narrow on purpose.** Only `ErrInvalid`, `ErrVersionMismatch`
and `ErrChecksum` count as unreadable. `ErrTimeout` must never be included: it
means another process holds the file lock, which `app.Validate` refuses but a
hand-written command line still reaches, and moving *that* file aside would
discard a live mount's state from underneath it. `TestOpenDoesNotReplaceALockedFile`
is what says so.
