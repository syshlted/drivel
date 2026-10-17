# Conventions

The rules that carry correctness. Most of this file describes places where the
obvious change is wrong and the tests will not tell you.

## Structural

- **Keep the core provider- and FUSE-agnostic.** The sync engine depends only on
  the `provider` and `internal/mount` seams and on `internal/fsevent` — never on a
  concrete Drive or go-fuse type. New backends implement an interface; they do not
  get special-cased upstream. **It is a test, not an inference**
  (`internal/app/seam_test.go`, over `go list -deps`): until M23 the `drivel`
  binary linked no backend at all and the property could be read off the artifact,
  and now that the host bundles its backends the rule has to be asserted on its
  own. `cmd/drivel` is the composition root and is the one place allowed to name
  one — in `bundledBackends` and nowhere else.
- **Ask a store what it can do; never assert it.** `provider.AsChangeSource` and
  its four siblings, not `store.(provider.ChangeSource)`. A backend in another
  process is reached through one proxy type that has every optional method
  regardless of what is behind it, so the assertion answers "yes" for all five and
  the mount polls a change feed that does not exist. The rule that makes the
  alternative safe: a declaration **narrows and can never widen**, so a store can
  never talk its way into a method it does not have.
- **Never block the FUSE path on the network.** Filesystem operations proxy to the
  backing store and, for mutations, emit an `fsevent.Event` on a buffered channel.
  All network work happens off that path.
- **Paths are root-relative slash paths, no leading slash** — the form `fsevent`
  emits and `provider.Store` consumes. `NewPath` is set only on `OpRename`.
- **`context.Context` is threaded for cancellation.** SIGINT/SIGTERM unmounts,
  which makes `server.Wait()` return; `Run` then waits for the inbound loop and the
  engine drains on a **detached** context. With several mounts each runs that
  sequence itself — `App` waits for them, it does not replace it. (`gosec` flags the
  detached context; the waiver is deliberate.)
- **Cancelling a goroutine is not the same as waiting for it.** Anything still
  running when `Run` returns is still running when `Close` shuts the state DB and
  the backing dirfd behind it, and in in-place mode that descriptor's number can be
  reused — so a write through it lands somewhere else. Both of `Run`'s loops are
  therefore waited for, each under its own bound; see
  [workflows §7](workflows.md#7-mount-lifecycle).
- **A `provider.Factory`'s context bounds *opening* the store and nothing after
  it.** Out of process a Factory runs inside the plugin's gRPC `Open` handler,
  which grpc-go cancels the moment that handler returns, so a context retained
  for later use is already dead when the first operation arrives — while in
  process the same argument is the mount's context, alive for the run. A store
  needing a long-lived one builds it with `context.WithoutCancel`, which keeps
  the values and drops the lifetime, and gives it **no deadline either**: one
  context serving every later call means a deadline breaks all of them rather
  than bounding one. `gdrive.refreshContext` is the instance, because oauth2
  holds a context for the life of a token source; inheriting the cancellation
  there meant a Drive mount synced nothing in either direction, reporting only
  `context canceled`, on every run outside the hour after `drivel login`.
- **Logging is per mount** — `e.logf` / `d.logf` / `n.logf`, never `log.Printf`. A
  single mount stays unprefixed, so its output is byte for byte what it was before
  multi-mount support.
- **The registry is a value, not a package-global.** No `init()` registration, no
  import for side effect. It would be the only process-global mutable state in the
  tree, in the subsystem whose whole premise is that there isn't any.
- **Provider config crosses the seam undecoded**, as the TOML text the user wrote
  (`provider.Config`). `internal/config` must never learn what a Drive folder ID
  is. Adding a provider touches neither `config` nor `app`. It is bytes rather
  than a closure because a closure cannot cross a process boundary.
- **A plugin's environment is built, not inherited.** `plugin/env.go` holds the
  allowlist and the reasoning. Adding to it is a security decision, not a
  convenience one: the point is that a backend authenticates with what its
  configuration names, not with whatever was exported in the shell that started
  the mount.

## In-place mode's cardinal rule

**Never touch the backing store by the *mountpoint* path — only via
`backing.Path`** (the `/proc/self/fd/N` handle). Otherwise reads and writes
recurse into the FUSE handler and deadlock. A hung mount almost always means
something violated this. See `DESIGN.md` §2.7.

## Lazy hydration — three invariants

Breaking any of them loses user data.

1. **The xattr is authoritative and the DB is not a fallback.** The marker
   (`hydrate.XattrName` — per platform, never the literal) lives on the backing
   file. `IsPlaceholder` reads the marker and only the marker, so **no marker
   means no placeholder record at all**, immediately. The consequence: on a
   backing filesystem without user xattrs, `-lazy` is unsafe and `app.Open` refuses it.
   The mitigation is eager mode or a different `-data`, never "keep the state DB".
2. **Never push a placeholder.** `syncengine.Placeholders` gates every content
   upload. A placeholder holds zero bytes at full apparent size, so uploading it
   replaces the remote file with nothing. The guard fails **safe**: unsure ⇒
   report "placeholder" ⇒ skip. Do not replace it with a size heuristic — a
   legitimate truncate-to-zero looks identical and must still sync.
3. **Hydrate on first I/O, not on `Open`.** `fileHandle.ensureResident` does it,
   so open/truncate/rewrite fetches nothing. FUSE delivers `O_TRUNC` as a separate
   `Setattr` unless `atomic_o_trunc` is negotiated, so `Open` handles only the
   atomic case and `Setattr` handles truncation.

Also: a failed hydration returns `EIO`, **never a short read** — zeros served as
content are silent corruption. And conflict copies are always downloaded in full,
because their paths exist only locally and could never be hydrated later.

## Range writes — the nil invariant

**`fsevent.Event.Dirty == nil` means "extents unknown", which means push the whole
file.** Anything that cannot account for every changed byte must report nil: a
size change (truncate or `fallocate` poisons the handle's tracker), an event with
no handle behind it, a rename fallback, a union across mismatched block grids, a
file that shrank behind an open handle, an unreadable size. In the coalescer,
unknown **absorbs** known.

Forgetting an extent corrupts a file; sending too much costs bandwidth. The
asymmetry is why the safe direction is nil.

One trap already paid for: a handle only sees its own writes, so its idea of the
file's length stops at the last byte written. `Release` fstats the file (before the
wrapped handle closes the fd) and grows the set to the real size. Skip that and
the engine's size cross-check rejects every set — range writes silently never
fire.

In `ranges`, present-ranges round **inward** (`Mark`) and dirty-ranges
round **outward** (`MarkCovering`). Same bitmap, opposite rounding, and the
asymmetry is the point.

The three gates and their ordering are in [Workflows §2](workflows.md#2-the-three-gates-before-a-content-push).

## Path resolution

`gdrive/index.go` answers "which fileID is this path?" from three sources,
cheapest first: in-memory maps, the persistent index, then a `files.list` name
query. **Only the third is a source of truth.**

1. **A persisted entry is a hint; verify before acting on it.** The remote may
   have moved, renamed, replaced or deleted that object while Drivel was down —
   the ID still resolves, just to the wrong file, and `Files.Update` on it destroys
   data with no conflict copy. `verifyLocked` re-checks name, parent and
   not-trashed; a failed directory drops its whole subtree. This is the price of
   persistence, not an optimisation to remove.
2. **The index must stay non-load-bearing.** Deleting it may cost latency and
   quota, never correctness. The name lookup is what makes that true.
3. **The index is bound to (account permissionId, concrete root ID); a mismatch
   wipes it.** Binding is lazy — at first use, not at `Open` — because mounting
   must not require the network.

Two traps: **a rename must be reported inbound as remove-old + add-new**, because
`provider.RemoteChange` carries no identity and Drive's feed never mentions a path
an object has left. And **a file's `parents` carry the concrete root ID, never the
`root` alias** — comparing against the alias made the parent walk climb past the
mount root and drop every inbound change to a top-level file.

## Enumeration & reconcile

The rules and their reasoning are in [Workflows §4](workflows.md#4-the-enumeration-sweep-and-reconcile).
The short form:

- Snapshot the cursor **before** the sweep, adopt it **after**.
- A delete is inferred **only from a baseline**, never from absence.
- The seen marks and the completion stamp are **persistent**, for two different
  reasons (resume; and measuring the periodic schedule).
- The sweep is the **only** correct place to prune a baseline. No cheaper prune
  pass — "absent locally" cannot distinguish *already gone on both sides* from
  *deleted locally while we were down*.
- The local walk must keep skipping conflict copies (uploading them publishes the
  losing side of every conflict) and placeholders (pushing one is the data-loss
  case above).
- The walk's **second** row re-sends a baseline whose *local* copy diverged — a
  push lost to a crash rather than abandoned by the engine (M27 part 4). Its four
  bounds are not optional: only a local mtime predating this mount's open (newer
  is the running engine's, and a long copy would otherwise be re-uploaded once per
  sweep); never an unfingerprinted baseline (counted, not guessed at); only a path
  the sweep listed; never a path the sweep could not settle or the repair pass
  already claimed.
- Reconcile pushes go through `Engine.Push`, never straight to the store, so they
  get the same gates, echo recording and retries as a write from the mount.
- **A sweep must not report an empty tree it cannot vouch for.** `gdrive`
  therefore errors rather than returning nothing when it cannot resolve its root,
  and *verifies* a configured root ID before using it — Drive answers a listing
  scoped to a parent that names nothing with no results, so a typo, a folder from
  another account, or a folder *path* written where an ID belongs all produce the
  one shape where every synced path looks remotely deleted.
- A remote deletion should be **recoverable where the provider can make it so**
  (`gdrive` trashes by default; `delete = "permanent"` opts out). It is the last
  guard behind the four above, not a replacement for any of them — the cap still
  refuses a pass it cannot justify.

## Special files and mount safety

DESIGN.md §9's M15 items 1–3. Four rules, each of them a decision rather than an
implementation detail:

- **`nodev` and `nosuid` are compulsory**, with no flag to disable them, and the
  list is per platform (`internal/vfs/mountopts_*.go`). FreeBSD gets `nosuid`
  alone: its kernel dropped `MNT_NODEV`, and `mount_fusefs` parses options against
  a fixed table and **fails the mount** on one it does not know — verified, not
  assumed. The options cover the mountpoint and say nothing about the backing
  store, which is an ordinary directory reachable without the mount.
- **Hard links are refused with `EPERM`.** Not a limitation being reported — a
  refusal chosen over the alternative. Falling through created the link, emitted no
  event, and left two regular files the sweep pushed as two diverging remote
  objects, after telling the caller it worked. `EPERM` is what `link(2)` documents
  for a filesystem that cannot make them.
- **Non-regular files are skipped and the skip is logged**, at both sites that
  decide it: `vfs.node.Symlink`/`Mknod` when the mount declines to emit, and
  `reconcile.pushLocalOnly` when the sweep's walk steps over one. The wording is
  duplicated on purpose — `vfs.specialKind` and `syncengine.kindOf` say the same
  words from different input types, because sharing one helper would mean the sync
  core importing the mount backend and pulling go-fuse into every build of it.
  Keep them in step; there is a test on each side.
- **A regular file is regular however it was made.** `Mknod` with no type bits
  creates one, so it emits `OpCreate` like `Create` does. Staying silent there
  would make syncing depend on which syscall wrote the file — the same
  push-path-versus-sweep disagreement MC-12 found for reserved names, which is a
  bug wherever it appears.

Not decided here: POSIX mode/ownership/ACLs (M15 item 4, needs a provider metadata
capability) and symlink *representation* (item 5, needs item 4). Both have their
reasoning recorded in DESIGN.md so it is not re-derived.

## Cross-mount guards

`app.Validate` runs **before anything opens**, on the flag path too. Each guard
exists because the failure is otherwise a deadlock, an opaque bbolt timeout, or a
file synced to the wrong account: shared state DB, either direction of
backing-tree/mountpoint overlap, shared mountpoints, duplicate names, and a state
DB inside a backing tree. That last one needs no second mount to be wrong — the DB
would sync itself, and its own writes would generate more events.

## The control API is a view

DESIGN.md §9/M14; the schema is [control-api.md](control-api.md).

- **Nothing that decides what to sync reads `syncengine.Monitor`**, and nothing
  in `internal/control` writes anything but pause, resume and shutdown.
- **A status read never waits on `Engine.Run` and never calls a provider.** The
  monitor's lock is never held across a send, a provider call or I/O. Status is
  served from local state with zero bbolt commits.
- **`unknown` is never rounded up to `synced`.** A path is `synced` only while
  its baseline's local fingerprint still matches.
- **Pause stops dispatch, never consumption**, and only the run loop shuts the
  workers' gate — after it has stopped sending. Hydration is never paused.
- **The in-tree client imports nothing internal**; it is the test that the
  documented schema is enough.

## bbolt commits are the unit of disk cost

A write transaction in either store fdatasyncs twice and rewrites every page it
touched — **~36 KB on a 10 000-entry DB however small the record is, and flat as
the DB grows**. Disk cost is therefore *commits × a constant*, and the fsyncs are
nearly all of it, so there is no tuning answer: page size is already minimal and
the freelist type is already chosen.

Three rules follow.

**Count commits, not bytes.** `state.Store.Commits` and `pathindex.Store.Commits`
report them; `TestPushCommitsPerFile` pins the budget at one commit per pushed
file, and the per-store tests pin the rest. A byte count depends on the filesystem
underneath and cannot be asserted; a commit count is deterministic. Adding a store
write to a per-file path is a budget change that needs a reason. The one that has
been argued: M6's gate 3 commits a baseline for a push it skipped (M27 part 4),
which brings the skip to the same one commit a push that uploads already costs,
because the alternative is a sweep re-proposing that push on every mount forever.

**Compare before writing, in a read transaction — never an early return inside the
write one.** bbolt advances the meta page on every commit whether a data page
changed or not, so an `Update` storing a byte-identical value costs the full
~36 KB and an `Update` that touches nothing still costs ~20 KB. A `View` costs
zero. This is why `pathindex.Set` and `SetMany` check first: a re-push stored the
same mapping twice for nothing (both of its resolutions end in `rememberLocked`),
an index hit re-stored what it had just read, and a periodic sweep re-offered a
page it already held. A genuinely new path still costs its one commit, so a
create-only burst is the one workload this does not help.

**A half-match is not a match.** `setLocked` evicts the two stale halves a rewrite
leaves behind, so the guard skips the write only when *both* directions of the map
already agree. Skipping on a half-match would leave the buckets as non-inverses,
which is the state that later resolves an ID to a path it no longer occupies.

`DESIGN.md §12` is the decision record, including why group commit (`NoSync` plus a
periodic `Sync`) is measured, coherent and deliberately not built.

## Losing a store is a defined state, and only one of them is fatal

Both databases are derived, and an unreadable one is **moved aside and replaced**
rather than failing — the state DB instead of failing the mount, the index instead
of running from memory for the life of the process. For the index that needs no
argument beyond `pathindex`'s own contract; for the state DB it does. That is safe for a specific reason,
not as a convenience: M7b infers a deletion only from a baseline, so a store with
no echoes reads every path as new and deletes nothing, and M5's marker is an xattr
on the backing file rather than a row in the DB. The cost is a full re-enumeration
and a §6 conflict copy per genuinely divergent path — the correct answer when no
baseline says which side moved.

**Only `ErrInvalid`, `ErrVersionMismatch` and `ErrChecksum` count as unreadable.**
`ErrTimeout` means another process holds the lock, and moving that file aside would
discard a live mount's state; both packages have a
`TestOpenDoesNotReplaceALockedFile` keeping it out.

## Secrets

`credentials.json`, `token.json`, `*.local.json` and the databases are gitignored.
Never commit them.

**Credential files are written through `gauth.writeSecret`, never `os.WriteFile`.**
A mode argument applies only when the call *creates* the file, so a `token.json`
that already exists would keep whatever permissions it arrived with.

## Licence

Drivel is copyright (C) 2026 SystemHalted and Jeremy Melanson, licensed
**Mozilla Public License, version 2.0**. Contributions are accepted under the
same licence — MPL §5 makes inbound match outbound, so there is no CLA and no
separate proprietary edition. [docs/project/licensing.md](../project/licensing.md)
records why this licence and not another.

Three practical consequences. **The copyleft is per file, not per program**
(MPL §1.10, §3.2): a modified Drivel source file must ship with its source, and
a larger work that merely uses Drivel need not. That is what makes a
closed-source backend against the `provider` seam permanently safe, and it is a
boundary to preserve — anything that would require a *caller* to open its source
is outside what this licence asks for. **Every new source file carries the
Exhibit A header**, because without it a file copied out of this tree arrives
with nothing saying what it is, which is precisely where file-level copyleft
leaks. And `LICENSE` is Mozilla's text **unmodified** — never edit it, and never
add a second licence file at the root, which confuses the detector pkg.go.dev
uses. The copyright notice lives in four places that must stay in sync: the
README's licence section, `docs/user/drivel.1`, the `cmd/drivel` package doc
comment, and the usage text `drivel help` prints.
