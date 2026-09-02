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

Provider-internal (below the seam — Drive's private path↔ID translation):
- `pathToID`   : relative path → Drive fileID
- `idToMeta`   : fileID → {path, driveModifiedTime, driveVersion, localMtime, size, md5}

Engine-level (provider-agnostic sync state):
- `cursor`     : the change-feed cursor (single key; opaque provider token)
- `pending`    : in-flight/echo-suppression records, keyed by path + content hash (see §4)

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

The four optional interfaces are all *accelerators that may decline*. Every one of
them has a correct, slower answer available if the provider omits it or a call
fails, and the engine is written so that "unsure" always selects that answer.

`RemoteFile`/`RemoteChange` are keyed by `Path`, not fileID. The Drive implementation
owns a path↔fileID index (in-memory for M2; the bbolt buckets of §2.4 in M3) and is
constructed with the **root folder ID** it maps the mount root to, plus an injected
`*http.Client` (see §2.6) so transport is chosen independently of provider logic.

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
4. **Adaptive cadence**: poll fast (~2–5s) while there's recent local or remote
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
  `md5Checksum`. Today they fall back to the opaque `Version` for echo matching;
  export-on-read (`files.export`) is unexplored.

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
   persistence is a latency optimization deferred to M7.
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
7. **M7 — Path↔ID index persistence.** Promote the Drive provider's in-memory
   path↔fileID index (§2.5) to a bbolt store, so a restart doesn't re-walk the
   remote tree to rebuild it. Purely a startup-latency optimization — the index
   stays self-rebuilding and provider-private, below the seam, and must never
   become a correctness dependency. Note this store belongs to the *provider*, not
   to `internal/state`, which stays engine-level and provider-agnostic.
8. **M8 — Multi-account & multi-provider mounts.** Two separable pieces:
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
9. **M9 — Plugin architecture.** Let third parties add providers (and eventually
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
