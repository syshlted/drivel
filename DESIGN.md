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
- **Uploader**: consumes local change events, applies them to Drive, records the
  resulting fileID + version in the state store.
- **Downloader**: runs a `changes.list` poll loop against Drive, applies remote
  deltas to the underlying directory, updates the state store.
- Both coordinate through the **state store** and the **echo-suppression** logic
  (§4) so neither re-processes the other's writes.

### 2.4 State store (bbolt)
Single embedded key/value DB ([`go.etcd.io/bbolt`](https://github.com/etcd-io/bbolt)).
Buckets:
- `pathToID`   : relative path → Drive fileID
- `idToMeta`   : fileID → {path, driveModifiedTime, driveVersion, localMtime, size, md5}
- `cursor`     : the Drive `startPageToken` (single key)
- `pending`    : in-flight/echo-suppression records (see §4)

### 2.5 Provider interface
Thin seam so the FS/sync layers don't hard-code Drive:

```go
type Provider interface {
    StartCursor(ctx context.Context) (string, error)
    Changes(ctx context.Context, cursor string) (changes []RemoteChange, next string, err error)
    Upload(ctx context.Context, parentID, name string, r io.Reader) (RemoteFile, error)
    Update(ctx context.Context, fileID string, r io.Reader) (RemoteFile, error)
    Mkdir(ctx context.Context, parentID, name string) (RemoteFile, error)
    Delete(ctx context.Context, fileID string) error
    Move(ctx context.Context, fileID, newParentID, newName string) (RemoteFile, error)
    Download(ctx context.Context, fileID string) (io.ReadCloser, error)
}
```

The Drive implementation is constructed with an injected `*http.Client` (see §2.6),
so the transport is chosen independently of provider logic.

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
- **Lazy hydration**: v1 assumes underlying dir has full content. A cache-on-demand
  mode (download-on-open, placeholder files) is a natural v2.
- **Deduplication / GPU**: content-defined chunking + hashing; shelved.
- **`changes.watch` push** as a latency optimization behind an optional relay.
- **Multi-account / multiple provider mounts.**
- **Partial-file / range writes** to Drive (Drive replaces whole files; large-file
  edits are expensive — chunked upload + resumable sessions later).

---

## 9. Milestones
1. **M1 — Passthrough mount.** go-fuse loopback proxying to underlying dir. No cloud.
2. **M2 — Transport + Drive auth + one-shot push.** `internal/transport` HTTP/3→HTTP/2
   client (§2.6), OAuth folded on top, upload a file on close.
3. **M3 — Pull loop.** `changes.list` cursor loop → underlying dir, with §4 echo
   suppression.
4. **M4 — Full bidirectional** with debounce, retries, conflict copies, clean shutdown.
