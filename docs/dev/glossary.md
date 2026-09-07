# Glossary

Words this codebase reuses with a specific meaning. Where a term has an obvious
everyday reading that is *wrong* here, that is called out.

### backing store / backing directory

The real directory behind a mount — the source of truth and the local cache, both.
Set by `-data`, or the mountpoint itself in in-place mode. Files in it are
ordinary files; the mount is a view of it.

**Not** a cache in the discardable sense: it holds content that may not have
reached the cloud yet.

### baseline

The record that Drivel has previously synced particular content at a particular
path — physically, an [echo](#echo) record. It is what makes a deletion
inferrable: absence with a baseline means "it went away", absence without one
means "it is new". No baseline ⇒ no deletion, ever.

### conflict copy

The losing side of a two-sided edit, preserved as a sibling file named
`NAME (conflict 2026-09-07 14-22-08).ext`. **Local-only by policy** and never
uploaded — publishing them would broadcast the losing side of every conflict to
every device. The sweep's local walk skips them for the same reason.

### coalescer

The per-path debounce in `Engine.Run` that merges a burst of writes into one
upload. It also merges [dirty ranges](#dirty-ranges), where **unknown absorbs
known**.

### cursor

The opaque resume token for a provider's change feed (Drive's `changes.list`
`pageToken`). Persisted in the state DB. Google expires them; the expiry is
recognised and recovered from by taking a fresh token and running a
[sweep](#sweep).

### dirty ranges

The byte extents a write touched, carried on an `fsevent.Event` as `Dirty`. **`nil`
means "unknown", which means upload the whole file** — the only safe default.
Rounded **outward** (`MarkCovering`), the opposite of [present ranges](#present-ranges).

### drain

The bounded window after unmount in which the engine finishes in-flight uploads.
Runs on a **detached** context, so the Ctrl-C that stopped the mount does not also
abort the uploads.

### eager mode

The default: the backing directory holds real content for everything synced. The
opposite of [lazy mode](#lazy-mode--hydration).

### echo

A record of "we synced *this* content at *this* path", holding a content hash
and/or an opaque provider version. Two jobs, and conflating them is a bug:

1. **Loop suppression** — when our own upload comes back through the change feed,
   the echo is what identifies it so it is not re-applied. This is the
   load-bearing correctness concern in the codebase (`DESIGN.md` §4).
2. **Baseline** — the same record is the evidence the [sweep](#sweep) uses to
   infer deletions and detect divergence.

### enumerate / enumeration

Listing the provider's whole tree, via the optional `provider.Enumerator`. What a
[sweep](#sweep) does. Distinct from the change feed, which reports only what
changed after a cursor was taken.

### export-only

A Google-native document (Docs, Sheets, Slides). It has no byte stream, so there
is no honest size for a placeholder and no digest to compare. Reported and marked
[seen](#seen-marks) — so its absence is never read as a deletion — but never
materialised and never given an echo.

### fan-out

How many folder listings a [scoped sweep](#scoped--flat) issues concurrently.
A **ceiling, not a rate**: it halves on a throttled listing and grows back on a
clean one, floor 1.

### generation

The identifier stamping one sweep's [seen marks](#seen-marks), so a later sweep's
marks cannot be mistaken for an earlier one's.

### hydrate

Fetch a [placeholder](#placeholder)'s real content and fill it in place, dropping
the marker. Triggered by the **first I/O**, not by `open`.

### in-place mode

Mounting a directory onto itself, so it is its own backing store and the files
stay put when Drivel exits. Linux-only, via a dirfd opened before mounting and
reached through `/proc/self/fd/N`. Its cardinal rule: **never touch the backing
store by the mountpoint path**, or you recurse into the FUSE handler and deadlock.

### lazy mode / hydration

`-lazy`: remote files materialise as [placeholders](#placeholder) and content is
fetched on first I/O.

### log-only mode

No `-credentials`: Drivel mounts and proxies, and prints the events it *would*
sync without contacting the provider. The fastest way to exercise the FUSE and
event layers in isolation.

### materialise

Create a local representation of a remote object — a real download in eager mode,
a placeholder in lazy mode. In eager mode, materialising something with no local
counterpart is gated behind `-materialize`, because it means downloading
everything.

### parking

**Flat sweeps only.** A flat listing has no parent-before-child guarantee, so an
object whose parent has not been seen yet is parked on that parent's ID and
released when it arrives. Whatever is still parked at the end is outside the mount
and dropped. A [scoped](#scoped--flat) descent is parent-first by construction and
has no parking — do not unify them.

### placeholder

A zero-byte file that reports its full remote size, marked with the
`hydrate.XattrName` extended attribute on the backing file. **The attribute is the
sole record** — no marker means no placeholder record at all, immediately. Never
uploaded, on pain of replacing a remote file with nothing.

### present ranges

Which blocks of a partially-hydrated file are resident. Cached in the state DB.
Rounded **inward** (`Mark`), the opposite of [dirty ranges](#dirty-ranges) — same
bitmap, opposite rounding, and the asymmetry is the point.

Note the two answer different questions: present ranges say *which bytes are
here*, never *is this a placeholder*.

### reconcile

Deciding what changed on each side while Drivel was not running, from the results
of a [sweep](#sweep). A three-way comparison between the remote listing, the local
tree, and the [baselines](#baseline).

### scoped / flat

The two enumeration strategies. **Scoped** descends from the mount root, ~1 request
per folder in the mounted subtree. **Flat** lists the whole account, ~1 request per
1000 account objects. Neither dominates: a folder-dense subtree is cheaper flat.
`auto` picks by whether the configured root names a concrete folder.

### seam

An interface that a whole category of implementation sits behind, so the code
above it never names a concrete type. There are three: `mount.Backend` (the FUSE
frontend), `provider.Store` (the cloud backend), and `fsevent.Event` (the language
between them). "Below the seam" means inside a provider or mount backend, where
the rest of the tree cannot see.

### seen marks

Per-generation records of which paths one sweep observed remotely. **Persistent**,
because an in-memory set would report every page a *previous* process consumed as
remotely deleted. Marked *before* acting on an object, and for every object —
including ones deliberately not materialised.

### sweep

One enumeration plus reconcile. Runs on a first run, a resumed sweep, a dead
cursor, an elapsed `-sweep-interval`, or `-resync`. Its cursor is snapshotted
**before** it starts and adopted **after** it finishes. It is the only correct
place to prune a baseline.

### three gates

The checks before every content upload: [placeholder](#placeholder), range write,
unchanged content. Anything that does not clear one falls through to a whole-file
upload, which is never wrong — only slower. See
[Workflows §2](workflows.md#2-the-three-gates-before-a-content-push).
