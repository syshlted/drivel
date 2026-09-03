# Drivel — Design

A Go FUSE filesystem that mounts a local directory as an **interceptor**, proxies
all operations to an underlying directory (the source of truth / local cache), and
**asynchronously and bidirectionally syncs** that directory with a cloud-storage
provider. First (and currently only) provider: **Google Drive**.

> GPU-accelerated deduplication is explicitly **out of scope for v1** (the repo name
> is historical). v1 is a clean interceptor → Drive sync engine.

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
  token, how far the sweep got, and the generation the marks below belong to
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

### 2.6 Transport — HTTP/3 (QUIC) with HTTP/2 fallback
All Drive API traffic goes over **HTTP/3**. Rationale: QUIC's connection reuse and
0-RTT resumption suit our access pattern (a frequent `changes.list` poll loop plus
bursty uploads) — repeated TLS/TCP handshakes are avoided, and head-of-line blocking
across concurrent transfers is eliminated.

Implementation facts that shape the design:
- **Go 1.26's `net/http` has no HTTP/3 client** (HTTP/1.1 + HTTP/2 only). HTTP/3 comes
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
- **Deduplication / GPU**: content-defined chunking + hashing; shelved (the repo
  name is historical).
- **`changes.watch` push** as a latency optimization behind an optional relay.
- **POSIX metadata preservation** (mode bits, POSIX/extended ACLs, xattrs, SELinux
  contexts) carried alongside content — see §10. Not scheduled; the security
  analysis is the blocker, not the plumbing.
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
   `UnseenEchoes` join over them. The rule, unchanged: **a delete may be inferred
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

   **The other open questions, decided.** Google-native Docs/Sheets/Slides have no
   `md5Checksum` and no byte size because they have no byte stream — they are
   exported, not downloaded — so there is no honest apparent size for a placeholder
   and no digest to compare. They are **reported but not materialised**:
   `RemoteFile.ExportOnly` says so, the sweep marks them seen (so their absence
   locally is never read as a deletion) and records **no echo** (recording one would
   claim we hold content we do not), and the pull loop skips them with a log instead
   of failing a download on every report. Shared-with-me files stay out of scope by
   construction: they are not under the My Drive root, so a root-scoped sweep excludes
   them — a decision, not an accident. Sharding the sweep (list folders first, then
   fan out) remains unbuilt, because one sequential pagination has not been measured
   to be too slow and building for that on speculation is how a cheap sweep becomes an
   expensive one.

   **Not in scope:** dedup, periodic full scans (the cursor feed stays the steady
   state), and any content transfer in lazy mode.
9. **M8 — Multi-account & multi-provider mounts.** Two separable pieces:
   - *Multi-account (real, proof-of-concept).* One `drivel` process serving several
     mounts, each with its own credentials, token, state DB, and engine. Requires
     making `gauth` token storage account-scoped rather than one global
     `token.json`, and a config file — the flag surface stops scaling here.
   - *Multi-provider (framework only).* Prove the §2.5 seam actually holds by
     registering more than one provider *kind* and selecting per mount. The proof
     is a **pseudo-provider**: `internal/provider/gdrive` re-registered under a
     second name, mounted alongside the real one. If a Drive-shaped assumption has
     leaked above the seam, two independently-configured Drive stores in one
     process will surface it. No second real backend in this milestone.
10. **M9 — Plugin architecture.** Let third parties add providers (and eventually
   mount backends) without forking. The seam already exists — `provider.Store` +
   optional `ChangeSource`/`RangeGetter` — so M9 is about the *loading* mechanism
   and its blast radius, not the interface. Go's `plugin` package is a poor fit
   (Linux-only, exact-toolchain-match, no unload); the realistic options are an
   out-of-process plugin protocol (gRPC over a unix socket, hashicorp/go-plugin
   shape) or a WASM host. Either way, M9 must settle: capability scoping (a plugin
   should not inherit the mount's ambient credentials), failure isolation (a
   crashing plugin must not take down the mount), and versioning of the seam
   itself. Depends on M8 having proven the seam with a second registered provider.

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
their own Cloud project and OAuth client (see `docs/google-cloud-setup.md`).
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
