# CLAUDE.md

Guidance for Claude Code working in this repository.

## What this is

A Go FUSE filesystem that mounts a local directory as an **interceptor**: every
operation is proxied to an underlying directory (the source of truth / local
cache) and asynchronously, bidirectionally synced with a remote provider. Two
providers ship: **Google Drive**, via the Drive API v3 `changes.list` cursor feed
— **not** the Workspace Events API and **not** webhooks — and **SFTP** (M18), over
an ordinary SSH account, which has no change feed at all and is therefore polled
by the M7b sweep.

Read [DESIGN.md](DESIGN.md) before making architectural changes. The
echo/loop-suppression model (§4) is the load-bearing correctness concern for
bidirectional sync — don't regress it.

> The `-gpu` in the repo/dir name is historical. Deduplication itself is now on the
> roadmap as **M11**, a provider over a content-addressed local directory — not a
> layer inside the mount, and not started. **GPU-accelerated hashing stays out of
> scope**: it optimises a component that does not exist yet. Don't add either unless
> asked.

## Layout

- `cmd/drivel` — entrypoint; flag parsing, the flag→spec mapping, signal context.
  `fstab.go` is the M16 mount(8) helper — argv0 dispatch, the `-o` option table
  and the spec mapping (portable, so its tests run everywhere); `fstab_linux.go`
  holds the privilege drop and the daemonize handshake.
- `internal/app` — the composition root below `main` (M8): `Mount` (Open/Run/Close
  for one mount) and `App` (N of them, with the cross-mount guards in
  `validate.go`). Provider-agnostic; it names a provider *kind*, never a type.
- `internal/config` — the TOML config file (M8): accounts, mounts, XDG paths, and
  the append-only account writer `drivel login` uses. Knows no provider.
- `internal/completion` — leaf, pure: renders bash/zsh completion scripts from a
  description of a program. Imported **only** by `cmd/drivel/completions.go`,
  which is behind the `completions` build tag, so no release binary contains it.
  See "Shell completions" below.
- `internal/fsevent` — backend-neutral change `Event`/`Op` types (shared by any
  mount backend and the sync engine).
- `internal/mount` — the mount-backend seam: `Backend` interface + `Options`, and
  `ResolveBacking` (separate-dir vs in-place). Platform bits in `backing_*.go`.
- `internal/vfs` — the go-fuse mount backend: loopback that proxies to the backing
  store and emits an `fsevent.Event` per mutation. Reads/lookups/attrs pass through.
  `special.go` holds M15's refusals and skips; `mountopts_*.go` the compulsory
  `nodev`/`nosuid`, which is per platform for a reason (see "Special files" below).
- `internal/provider` — cloud-backend interface (the seam). Drive impl is M2. M8
  adds the `Registry` (kind → `Factory`), an explicit value rather than an
  `init()`-filled package map.
- `internal/provider/gdrive` — Google Drive impl of the interface.
- `internal/provider/sftp` — SFTP impl (M18): the first path-addressed backend, so
  it carries no path index and no `ChangeSource`, and it is the first `RangePutter`
  in the tree. See "SFTP" below.
- `internal/gauth` — Google OAuth: credentials.json/token.json I/O and the
  interactive login flow (loopback redirect + manual paste, rclone-style).
- `internal/transport` — HTTP/3 (QUIC) client with HTTP/2 fallback, injected into the
  Drive provider (M2). See DESIGN.md §2.6.
- `internal/syncengine` — outbound push (`Engine`) + inbound pull loop
  (`Downloader`): `changes.list` cursor feed → backing dir, with §4 echo
  suppression and adaptive cadence. M4 adds per-path debounce + a path-hashed
  worker pool with retry/backoff on the push side, §6 conflict copies on the pull
  side, and a bounded drain on shutdown. M6 adds three gates before every content
  push (placeholder → unchanged-content hash → range write), each falling through
  to the whole-file `Put`. M7b adds `reconcile.go`: the initial enumeration sweep
  and the three-way reconcile, owned by the `Downloader` because it owns the cursor.
- `internal/pathindex` — leaf package: a persistent bbolt path↔native-ID map for
  ID-addressed providers (M7). Provider-private (composed by `gdrive`, invisible
  above the seam) and a *cache*, never an authority — see "Path resolution" below.
- `internal/state` — engine-level bbolt sync state: the change-feed cursor and the
  echo-suppression records shared by the up/down paths (DESIGN.md §2.4, §4), plus
  the M5 hydration bitmap cache (opaque bytes; the encoding belongs to `ranges`)
  and the M7b sweep record + per-generation `seen` marks.
- `internal/hydrate` — M5 lazy hydration: placeholder creation, the authoritative
  `hydrate.XattrName` xattr marker (per platform since M10), and whole-file
  hydrate-on-first-I/O with
  singleflight. Provider-agnostic.
- `internal/ranges` — leaf value package: the block bitmap (`Set`) used two ways —
  M5's present-ranges and M6's dirty-ranges. Pure, no I/O. It lives outside
  `hydrate` on purpose: M6 runs in eager mode too, and the default path must not
  import the lazy package to describe a write.
- `internal/testenv` — test-only leaf: turns "this machine has no user xattrs / no
  `/dev/fuse`" from a silent `t.Skip` into a failure when `DRIVEL_REQUIRE_TESTENV`
  names the facility (`fuse`, `xattr`, `all`). Skipping is right on a laptop and
  wrong in CI, where the tests that skip are the M5 data-loss guards.

## Documentation

Two published collections, split by audience, plus repo housekeeping:

- `docs/user/` — quickstart, install, google-cloud-setup, sftp, configuration,
  lazy-mode, data-safety, troubleshooting, platforms, fstab, and `drivel.1`.
  Written for someone who wants to *use* drivel; it never cites DESIGN.md.
  Per-provider facts live on that provider's page: `sftp.md` owns the poll-interval
  warning and the no-trash warning, and `configuration.md` links to it rather than
  restating either.
- `docs/dev/` — architecture (structure), workflows (sequence/decision diagrams),
  schema (the two bbolt DBs), dependencies (why each one), conventions (the
  load-bearing rules), building, testing, new-provider, glossary, and the
  multiclient test plan. These *do* cite `DESIGN.md §N`, for reasoning.
- `docs/project/` — publishing to pkg.go.dev, marketing copy, the mascot's lore
  and drawing brief.
- Root: `README.md` is a slim landing page, `CHANGELOG.md` is the **externally
  facing** change record (abbreviated, in user terms — not a milestone log),
  `CONTRIBUTING.md`, `SECURITY.md`.

**`DESIGN.md` and this file are internal working documents** and are deliberately
not part of either collection. Don't link them from `docs/user/`, and don't slim
DESIGN.md to match a docs page — it is the long-form record, and the docs cite it.

Four rules. A user-visible behaviour change lands in `docs/user/`, the man page
**and** `CHANGELOG.md`. A fact lives in exactly one place and everything else
links to it — platform support is `docs/user/platforms.md`, flags are
`docs/user/configuration.md`, the invariants are `docs/dev/conventions.md`. And
mermaid diagrams are validated by parsing them, not by eye.

The fourth is newer and the easiest to lose: **copy that says what drivel *is*
stays backend-neutral.** Drive is the backend that ships, not the product — the
reference docs already had this right (`drivel.1` says "the first and currently
only provider", the flags are namespaced `-drive-*`) while the README and
marketing copy had collapsed the program into a Drive client. Naming the shipping
backend is honest; implying it is the only conceivable one is not, and neither is
implying the others exist yet. Anything true of one provider only — its
credentials mechanism, what it does with the data it receives — is **disclosed on
that provider's page** and nowhere else, because drivel cannot answer for
software it did not write; `docs/user/google-cloud-setup.md` §"What Google sees"
is the pattern. And drivel makes **no product-level claim about traffic or
telemetry at all** — see `docs/project/marketing.md` for why the narrower
versions fail too.

## CLI

`drivel` has two subcommands (`mount` is the default, so `drivel -mount … -data …`
still works): `drivel login` (interactive OAuth wizard → credentials.json +
token.json) and `drivel mount`. **Both entry points build their registry through
`newRegistry()` in `cmd/drivel/mount.go`** — the flag/config path and the M16 mount
helper — because a backend registered in one but not the other is a config file
that works from the shell and fails at boot. `drivel login` is Drive-specific by
nature; an SFTP account needs no login step and is written by hand. From M8 both are account-aware: `drivel login
-account NAME` scopes credentials to `$XDG_CONFIG_HOME/drivel/NAME` and appends
`[account.NAME]` to the config file, and `drivel mount` with no flags reads
`$XDG_CONFIG_HOME/drivel/config.toml` and serves every `[[mount]]` in it.
`mount` is **non-interactive**: it requires a token from `login` and never
prompts on stdin. The login loopback server uses
rclone's port **53682**; forward it into the container for auto-capture, else use
the paste fallback. When adding stdin prompts, share ONE bufio reader — multiple
readers on os.Stdin race and swallow buffered lines.

`-xattr` on `mount` (config: `xattr = true`) serves extended attributes through the
mountpoint; **off by default, and the default is a safety property**. M5's
authoritative marker is a `user.*` xattr on the backing file, so passthrough
publishes it at the mountpoint: stripping it from a placeholder makes the uploader
push zeros over the remote file, attaching it to a resident file makes the next
read overwrite local content. Nothing below the mount needs it — the hydrator uses
the backing path — and no xattr is ever synced. Two guards, both required:
`DisableXAttrs` covers only GETXATTR/LISTXATTR, so `internal/vfs/xattr.go`
overrides all four node ops (SETXATTR/REMOVEXATTR would otherwise reach the
backing file). See DESIGN.md §2.1.

M7b flags on `mount`: `-resync` (force an enumeration sweep), `-materialize` (eager
mode only — download remote files that have no local copy; `-lazy` always
materialises, as placeholders), `-max-deletes N` (cap on reconcile-inferred
deletions, default 100, 0 = unlimited), `-sweep-interval D` (re-enumerate this
often, default 24h, 0 disables). M7c adds `-drive-sweep-mode` (`auto`|`flat`|
`scoped`, config `sweep-mode` in the provider table).

**`-drive-delete` (`trash`|`permanent`, config `delete`, default `trash`) decides
what a removal does to the remote object, and the default is the safety property.**
A trashing removal is `files.update{trashed:true}` and is indistinguishable above
the seam — every `gdrive` query carries `trashed = false`, `changes()` already
reads a trashed file as `Removed`, `verifyLocked` already rejects a trashed index
hint, and Drive's `trashed` covers a trashed parent's whole subtree, so the
recursion `Store.Remove` documents still holds — **but that last one propagates
asynchronously**, which the live test found and no fake would have: right after a
folder was trashed its child still read `trashed = false`. Nothing depends on the
timing (an `rm -rf` unlinks the children first, and a sweep racing it parks the
child on a missing parent and drops it), so don't write code that assumes the flag
is set the moment `Remove` returns. **Verified against real Drive on 2026-09-09**
via `TestLiveRemoveTrashes` (`DRIVEL_LIVE_RIG`, gated and skipped by default,
self-cleaning); the fake models the settled state. It defaults to the recoverable
form because **not every deletion drivel performs was typed by a user**: reconcile
*infers* them from a baseline, every input that makes that wrong is an accident
rather than a corruption, and this was the one operation with no fallback — M5
refuses to push a placeholder, M6 falls back to a whole-file `Put`, M7 verifies a
hint, M7b abandons a pass it cannot justify. `permanent` exists and is not
vestigial: a trashed object still costs quota, so a mount that is how someone
reclaims space needs it. An unreadable value is refused at open, M8 rule 6 applied
to a value like `sweep-mode` — and here the misspelling gives the operator the
*opposite* of what they asked for in the direction that loses files. It **is** in
the M16 option table (`-o drive-delete=permanent`), together with the three
shaping flags that had drifted out of it; see the fstab section below.

**Transfer concurrency is two numbers, both per mount**: `-upload-workers`
(config `upload-workers`, default `syncengine.DefaultWorkers` = 4) sizes the
push pool, `-hydrate-workers` (config `hydrate-workers`, default
`hydrate.DefaultWorkers` = 8) caps concurrent lazy fetches. Three things are
load-bearing. **Per mount is a security property, not a layering preference** —
two mounts are two accounts, and a limiter they shared would let either starve the
other and let each time the other's activity off the contention, which is exactly
the cross-mount coupling `app.Validate` exists to prevent; there is no process-wide
budget and adding one would be the only process-global mutable state in the tree.
**An explicit `0` is refused at both boundaries**, because `max-deletes = 0` means
"no limit" in the same config file, so reading `upload-workers = 0` as "use the
default" is M8 rule 6's failure in a new place — and because `hydrate.New` must
clamp `<= 0` regardless, since a zero-capacity channel blocks the first fetch
forever. And **the hydration bound is a fix, not a knob**: before it, a `grep -r`
over a lazy tree issued one download per file simultaneously, so bounding it
changes lazy-mode behaviour by design.

The push pool's own concurrency is still `workerFor(ev.Path, …)` — same path, same
worker, ordered — so no setting makes same-path writes concurrent. Raising the
count buys latency-hiding and not bandwidth (one HTTP/3 connection, one congestion
window), costs a chunk buffer per in-flight upload, and past the provider's
throttle threshold costs quota for nothing — the M7c 97x measurement is the
evidence. **The inbound pull loop is still serial and has no knob**, deliberately:
concurrency there is not a parameter but a change to the `forget` map, the
cursor-advance ordering and per-path ordering within a page. Anyone adding a
"download-workers" name must not point it at hydration — the two are different
pools. The right default is ultimately the *provider's* to suggest (they cap
up/down differently); nothing crosses the seam for that yet.

From M16 `drivel` is also the **mount(8) helper**, when reached through
`/sbin/mount.fuse.drivel` (argv[0] dispatch) or as `drivel mount-helper` — so a
mount can live in `/etc/fstab` and come up at boot. **Linux only.** `make install`
places the symlinks. See "fstab" below and `docs/user/fstab.md`.

`-pprof ADDR` on `mount` serves `net/http/pprof` for the process (not per mount),
off unless given. It is a debug endpoint that hands out the heap — synced paths
and, in some buffer, contents — so it **binds loopback only**: anything else is
refused at startup and needs `-pprof-allow-remote` (a bare `-pprof 6060` means
`127.0.0.1:6060`), `/debug/pprof/cmdline` is deliberately not registered because
argv names the credentials file, the token, the backing tree and the account, and
a port it cannot bind is a *startup error*: the reason to run it is to be measuring, and
a soak that produced nothing silently is worse than one that refused to start. Its
`WriteTimeout` is deliberately 0, for the same reason `ChunkTransferTimeout` is
unset — a write deadline never resets on progress, so any value becomes the
longest profile the build can ever take.

M8 adds `-config FILE` to `mount` and `-account NAME` / `-config FILE` to `login`.
`-config` cannot be combined with any flag describing *what* to mount — that would
need a precedence rule nobody would remember — but `-debug` and `-pprof` compose.
Flags without `-config` synthesize a one-entry config, so there is one code path
below them.

## Milestones

**v1, shipped.** M1 passthrough mount · M2 transport (HTTP/3→HTTP/2) + Drive auth +
push-on-close (incl. file-handle write capture for `OpWrite`) · M3 `changes.list`
pull loop + echo suppression + `internal/state` bbolt cursor/echo store · M4 full
bidirectional: per-path debounce + worker pool, retries/backoff, §6 conflict
copies, bounded drain on shutdown.

**v2, in progress.** M5 lazy hydration **shipped**: `drivel mount -lazy` (opt-in;
default is unchanged eager mode). See "Lazy hydration" below for the invariants.
M6 range writes **shipped**: always on, no flag. See "Range writes" below.
M7 path↔ID index persistence **shipped**: on by default (`-index`, `""` disables).
See "Path resolution" below. M7b initial enumeration & reconcile **shipped**: the
sweep runs automatically on a first run, a resumed sweep or a dead cursor, with
`-resync` to force it. M7c scoped enumeration **shipped**: a subfolder mount
descends its own subtree instead of listing the account, which is what makes the
sweep's cost proportional to what was mounted. See "Enumeration & reconcile" below.

M8 multi-account & multi-provider **shipped**: N mounts per process from a TOML
config, a provider registry, account-scoped login. See "Multi-account" below.

M18 SFTP **shipped**: the second provider, path-addressed, no change feed, and the
tree's first real `RangePutter`. See "SFTP" below.

**v2, planned** (DESIGN.md §9 has the detail). **M9** plugin architecture
(out-of-process or WASM; Go's `plugin` package is a poor fit). M8 discharged its
precondition: the §2.5 seam holds under two independently-configured stores in one
process. **M10** platform parity — both xattr halves are written; **FreeBSD has been
run on its own OS (2026-09-06), macOS has not**: one shared linux+darwin
implementation, plus FreeBSD `extattr_*` in its own file (different API, namespace
as an argument, so `hydrate.XattrName` is now per platform — `drivel.placeholder`
there, `user.drivel.placeholder` elsewhere). What remains is the macOS run and the
two FUSE-T questions, so M10 is open. Writing it also corrected a claim these
notes had carried since M5: losing `drivel-state.db` was never what turned a
placeholder into an empty file — no marker means no placeholder record at all.

**The FreeBSD run found nothing wrong with M10 and one real bug underneath it**, so
don't read a green xattr suite as a green platform. `DRIVEL_REQUIRE_TESTENV=all`
on FreeBSD 15.1 passes now; on the first attempt four `internal/vfs` tests failed,
every one of them writing through an `O_WRONLY` handle — see "Platform support"
below for why the backing file is opened readable regardless.

Independent of M9, nothing waits on either.

**M14** control & status API — a unix socket (`internal/control`, HTTP/JSON,
`/v1/…`) so third-party tools can read per-path sync status and per-backend stats,
pause/resume a mount, see whether a backend is reachable, and stop the process,
with `drivel status|pause|resume|stop` as the in-tree first client. Depends only on
M8, so it can be scheduled at any time. Five things in DESIGN.md §9's entry are
load-bearing and easy to get wrong: it is a **view, never an authority** (M7
invariant 2 again — no sync decision may read from it); status is served from a
**snapshot**, never from `Engine.Run`, whose coalescer is goroutine-confined and
whose `dispatch` blocks when a worker queue fills; **pause holds the dispatch, not
the event channel** — `vfs.node.emit` blocks rather than drops, so pausing
consumption hangs FUSE, and discarding the pending set is not a fallback because
`reconcile.pushLocalOnly` skips paths that already have an echo; **`unknown` is a
first-class status**, because reporting "synced" for a path with no record is the
placeholder-vs-empty-file lie in a new place; and shutdown cancels the SIGINT
context rather than exiting, so the three-phase `Close` and its bounded drain still
run. Reachability is **observed, never probed**. Unsettled and deliberately so:
on-by-default vs off (the `-pprof` precedent says off), authorisation beyond the
socket mode, and whether status answers for a whole *tree* — that last one decides
the response schema, so it comes first.

**M15** special files, POSIX metadata & mount safety — **items 1–3 shipped
(2026-09-08); 4 and 5 remain.** See "Special files" below for what landed and the
three things the FreeBSD run corrected. What is left: **POSIX mode/ownership/ACLs
get two per-mount channels** (item 4), provider metadata or a bound sidecar,
because Drive carries no `mode`/`uid`/`gid` at all and because putting permissions
in a cloud API is a disclosure some users refuse; the record must name its path
*and* carry a content digest, or a conflict copy grafts one file's ACL onto
another's bytes. It is a milestone of its own and needs the provider-metadata
capability first. **Symlinks are deferred on purpose** (item 5, and it needs item
4): the human-readable pointer file is a good body and a bad *identity*, because
content-as-identity forces the sweep to download every small file to classify it,
and it is a symlink-injection channel. Cygwin and Git LFS both use two signals with
the out-of-band one primary; that is the shape to copy when it lands.

**M16** fstab & non-interactive mount **shipped** (Linux only): `drivel` is a
`mount(8)` helper as well as a command. See "fstab" below. It was built as "M15"
before that number went to special files; if you meet a stray M15 in an old
branch or note meaning the mount helper, this is it.

**v2, backends on the roadmap** (DESIGN.md §9 has the reasoning; these three are
unscheduled, none is started, and each one's shape is *decided* — the entries say
so, so don't re-litigate them). **M11** deduplicating local backend: a
`provider.Store` over a content-addressed local directory, **private to one
machine**, which with `-lazy` is a deduplicating filesystem. It is the first
non-Drive provider, so it also shapes M9. **M12** encrypting backend: a
**decorator over another provider** (`crypt` over `gdrive` = client-side E2E for
Drive), which needs the group's one piece of new framework — `provider.Params`
letting a provider open its inner store through the registry. **M13** block-level
filesystem over a distributed database, **as another async provider behind the
existing engine**, not a cluster filesystem: the cluster version would abandon
§2.1's "FS ops never block on the network" rule and is a different program.

Three things decide those designs and are not settled: M11's garbage collection
(refcounts vs mark-and-sweep, and reclaiming space from packs), M12's filename
encryption (deterministic or path lookup stops working, which would make the M7
index authoritative and break its invariant 2), and M13's engine — where the
recommendation is emphatically **not Cassandra for metadata** (no multi-partition
transactions, LWW by cell timestamp, tombstone pressure under a delete/overwrite
workload) but FoundationDB or TiKV, with Redis/Memcached as cache and never as the
lock of record.

**v2, filesystem backends on the roadmap** (DESIGN.md §9, M17–M21 — written as
*one* design with five faces, so read the group preamble before any single entry;
all unscheduled, none started). **M17** `localfs`, a provider over a plain
directory: no protocol code, and **today it is the honest answer for NFS and SMB**
— mount the share with the kernel and point `localfs` at it. **M18** SFTP —
**shipped 2026-09-10**, see "SFTP" below. **M19** SMB/CIFS. **M20** NFS,
**declined with a stated expiry**. **M21** WebDAV, widest reach for the least code.

Seven things are true of the whole group and are why it is one design. **A
deletion is permanent on all of them and drivel does not emulate a trash**
(decided 2026-09-11, and it covers M11 too): inside the mount root the sweep would
enumerate the graveyard and pull every deleted file back, and outside it is a
second store to garbage-collect, bolted under the one operation with no fallback
above it. `-max-deletes` is therefore the *only* delete guard on these backends —
a fact to disclose on each one's user page, not a gap for a later session to paper
over because the docs read alarmingly. M18 shipping
already discharged the group's central precondition — **a feedless provider now
gets a `Downloader`** (sweep-only), where before `app.Mount` built one solely for a
`ChangeSource` and a store that could only enumerate was silently upload-only.
M19–M21 inherit that; none needs to rediscover it. They are
path-addressed, so **a provider here that grows an M7-style path index has
misunderstood something** — `internal/pathindex` stays Drive-private. None has a
change feed except SMB, so **the M7b sweep *is* the inbound path**: `-sweep-interval`
stops being a safety net and becomes the poll interval (its 24h default is wrong for
every one of them), and the §4 echo store becomes the only memory of what was
synced — which is also M7b's delete baseline, so pruning it harder is a data-loss
bug, not a tuning choice. **The M6 gates come out the reverse of Drive's**: a write
at an offset is native, so `RangePutter` is real and M6's range-write path finally
runs against something that is not a fake, while no server computes a content digest
by default, so gate 3 declines and content pushes are whole-file. `Move` is a real
server-side rename. Case-insensitivity is **the MC-30 same-name-sibling problem by a
third route** (unconditional on SMB, server-dependent elsewhere) — detect and report,
don't mitigate. And the justification test the whole group is built on: what drivel
adds over simply mounting the share is §2.1's "an FS op never blocks on the network",
plus M5 lazy hydration and M4 conflict copies — **a proposed backend that cannot
point at that triple is declined**, which is exactly why M20 is.

One shared precondition, and it is a correctness item rather than a feature: **a
provider configured with a *local* path is inside `app.Validate`'s blind spot.** M8
rule 2 guards overlap between *mounts*; a `localfs` directory (or an M11 store, or a
kernel-mounted share used as either) is a local path belonging to a **provider**, and
nothing checks it. Pointed at a backing tree it syncs a tree into itself; pointed at a
mountpoint it breaks §2.7's cardinal rule from below. Needs `provider.Params` to let a
provider *declare* the local paths it will touch, before validation and before open.

Per-backend, the things that are decided and easy to get wrong. SFTP is now
shipped, so read the "SFTP" section below instead of this line — the one thing it
corrects is gate 3, which turned out to be unreachable through `pkg/sftp` at all
rather than capability-detected per connection. SMB: it has the group's only real `ChangeSource` (CHANGE_NOTIFY, whose
overflow *is* `provider.ErrCursorExpired`), which makes it the only chance to test that
contract against a second feed — but **settle the pure-Go client's maintenance and
whether it exposes notify at all before writing code**, since cgo against libsmbclient
would forfeit §2.9's cross-compile proof. WebDAV: `Depth: infinity` PROPFIND is off by
default on the commonest server, so enumeration **must** be the M7c descent; the ETag is
the echo identity and is *not* a hash (same opaque-version path Drive's Google-native
files already use); Basic auth over plain `http://` is refused at open, and an empty
PROPFIND on the root is an error, never an empty sweep (M7b row 4).

**M22 Google Photos is recommended *against* as a `provider.Store`**, and the reason
is the API rather than the effort: three of the six required methods have no
implementation — the Library API cannot delete from a library, cannot replace an
item's content, and has no stable path to move — so a local `rm` would silently not
propagate and the sweep would put the file back. Scopes for reading a user's existing
library were removed in March 2025 (an app now sees only what it uploaded, plus
Picker selections, and `drivel login`'s loopback/paste flow has no browser surface to
host a Picker — the `drive.file` blocker again). **Verify that scope situation before
scheduling anything here**; the whole entry turns on it. What *is* buildable is a
one-way backup target, which is not a `Store` and should not pretend to be one —
inventing a read-only or append-only store concept to fit a backend that cannot
delete would weaken a contract five other providers rely on.

**Design notes, unscheduled.** DESIGN.md §10 (POSIX metadata over Drive) and §11
(virtual xattrs: emulate xattrs from a file in the backing store, so they work on
filesystems that have none and sync with the content). §11 is **build-tag gated and
absent from release builds if it is ever written** — a virtual store turns synced
content into filesystem metadata, so `user.drivel.*` and every privileged namespace
have to be refused at the boundary in both directions.

**M0 — Test & CI** is a cross-cutting, always-open track (DESIGN.md §9), not a
numbered milestone. Every package has tests, green under `-race`. CI enforces that
on every push, together with `golangci-lint` and `govulncheck`; every gate is a
`make` target that the git hooks and the workflow both call, so "passed locally"
and "passed in CI" cannot drift apart. M8 closed the `cmd/drivel` and end-to-end
items; the property tests, `gauth` and `fsevent` closed after it, which empties the
numbered list. The live testing work is now Tier B of the multi-client campaign
(`docs/dev/multiclient-test-plan.md`).

Three rules the last rounds left behind. **A property test nobody has seen fail is a
property test nobody knows the strength of**: `ranges/property_test.go` and
`syncengine/coalescer_property_test.go` were each checked by mutating the code they
cover (round `Mark` outward, let a known event revive a poisoned accumulator, …) and
confirming the property that names the mistake is the one that fails. State the
properties against the *input*, never against an oracle built from the same helpers.
**Credential files are written through `gauth.writeSecret`**, never `os.WriteFile`:
a mode argument applies only when the call creates the file, so a `token.json` that
already exists keeps whatever permissions it arrived with. And **a helper that walks
a tree the code under test is mutating must skip `fs.ErrNotExist`, not fail on it**
— `peer.manifest`/`peer.conflicts` in `multiclient_test.go` did the latter and made
`TestFleetPropagatesABulkDelete` fail two runs in three, with the product innocent.
A sampler in a poll loop is waiting out exactly that race; every *other* error still
fails.

DESIGN.md §10 is a design note on carrying POSIX metadata (mode, ACLs, xattrs,
SELinux) over Drive — unscheduled. If you touch it, the rule is that permission
metadata from a remote source is *executable trust*: authenticate it, bind it to
the file, fail closed, and mask setuid/setgid by default.

## Lazy hydration (M5)

Three invariants; breaking any of them loses user data.

1. **The xattr is authoritative and the DB is not a fallback.** The marker
   (`hydrate.XattrName` — per platform since M10, never the literal) lives on the
   backing file; the `RangeSet` in `internal/state` caches present *ranges* and
   nothing else. `IsPlaceholder` reads the marker and only the marker, so **no
   marker means no placeholder record at all**, immediately — not "after the DB is
   lost", which is what these notes wrongly said until M10. The practical
   consequence is invariant 2's: on a backing filesystem with no user xattrs
   (drvfs under WSL2, exFAT, a tmpfs `/tmp` on FreeBSD) `-lazy` is unsafe and the
   mount warns; the mitigation is eager mode or a different `-data`, never "keep
   the state DB".
2. **Never push a placeholder.** `syncengine.Placeholders` gates every content
   upload. A placeholder holds zero bytes at full apparent size, so uploading it
   replaces the remote file with nothing. The guard fails **safe**: unsure ⇒ report
   "placeholder" ⇒ skip. Do *not* replace it with a size heuristic — a legitimate
   truncate-to-zero looks identical and must still sync.
3. **Hydrate on first I/O, not on `Open`.** `fileHandle.ensureResident` does it, so
   open/truncate/rewrite fetches nothing. FUSE delivers `O_TRUNC` as a separate
   `Setattr` unless `atomic_o_trunc` is negotiated, so `Open` handles only the
   atomic case (dropping the mark), and `Setattr` handles truncation.

Also: a failed hydration must return `EIO`, never a short read (zeros served as
content are silent corruption); and §6 conflict copies are always downloaded in
full, because their paths exist only locally and could never be hydrated later.

## Range writes (M6)

Three gates run before every content push, in `syncengine.pushContent`; anything
that doesn't clear one falls through to the whole-file `Put` that M1–M5 always
did. That fallback is never wrong, only slower — which is what makes the whole
feature safe.

1. **Placeholder** (M5) — unchanged, and nothing may get in front of it.
2. **Range write** — `provider.RangePutter`, only when the extents are known, the
   object exists remotely at exactly the local size, **and the remote still
   matches the §4 echo record**. That last one is not optional: a whole-file `Put`
   over someone's edit loses it per documented §6 policy, but splicing extents
   into a diverged remote makes a hybrid file that existed nowhere, with no
   conflict copy. Diverged / no echo / no state store ⇒ decline.
3. **Unchanged content** — hash the local bytes (`provider.ContentHasher`, so the
   engine never assumes an algorithm) and compare to **what `Stat` says the remote
   holds now**, not to the echo. A stale echo (expired cursor, reused state DB,
   missed delete) would otherwise skip the push forever and never restore the
   file. This is the gate that actually helps Drive users.

Gates 2 and 3 share one `Stat` and are both skipped below `hashSkipMinSize` (one
block) — for a small file the round-trip costs what the upload would. Gate 2 runs
before gate 3 on purpose: hashing reads the whole file, so checking "did anything
change?" first would spend a 4 GB read to avoid a 4 MiB upload.

**Drive cannot patch byte ranges** — `files.update` replaces content wholesale and
resumable chunks still carry a complete new body. `gdrive` therefore does *not*
implement `RangePutter`, deliberately; don't "fix" that. What Drive gets from M6 is
gate 3 plus chunked/resumable upload sessions (`mediaOptions`). Session URIs are
not persisted: this survives a flaky network, not a restart. `ChunkTransferTimeout`
is intentionally unset — it is a hard per-attempt deadline that never resets on
progress, so setting it caps the slowest link that can ever finish a chunk, and it
must stay well under `ChunkRetryDeadline` or the retry never fires.

The invariant, and it mirrors M5's: **`fsevent.Event.Dirty == nil` means "extents
unknown", which means push the whole file.** Anything that cannot account for every
changed byte must report nil — a size change (truncate/`fallocate` poisons the
handle's tracker), an event with no handle behind it, a rename fallback, a union
across mismatched block grids, a file that shrank behind an open handle, an
unreadable size. In the coalescer, unknown *absorbs* known. Forgetting an extent
corrupts a file; sending too much costs bandwidth.

One trap already paid for: a handle only sees its own writes, so its idea of the
file's length stops at the last byte written. `Release` fstats the file (before the
wrapped handle closes the fd) and grows the set to the real size. Skip that and the
engine's size cross-check rejects every set — M6 silently never fires.

In `internal/ranges`, present-ranges round **inward** (`Mark`) and dirty-ranges
round **outward** (`MarkCovering`). Same bitmap, opposite rounding, and the
asymmetry is the point.

## Path resolution (M7)

Drive is ID-addressed; the seam above it is path-addressed. `gdrive/index.go`
answers "which fileID is this path?" from three sources, cheapest first: the
in-memory maps, the persistent index (`internal/pathindex`), then a `files.list`
name query against Drive. **Only the third is a source of truth.**

1. **A persisted entry is a hint; verify before you act on it.** The remote may
   have moved, renamed, replaced or deleted that object while drivel was down — the
   ID still resolves, just to the wrong file, and `Files.Update` on it destroys
   someone's data with no conflict copy. `verifyLocked` re-checks name + parent +
   not-trashed before an entry is believed, and a failed directory drops its whole
   subtree. Never "optimize" this away; it is the price of persistence.
2. **The index must stay non-load-bearing.** Deleting `drivel-index.db` may cost
   latency and quota, never correctness. The name lookup is what makes that true,
   so it is not optional garnish — before M7 an unknown path meant "absent", which
   made a restart upload a *duplicate* beside the real file, disabled M6's
   unchanged-content gate, and made M5 placeholders from an earlier session `EIO`.
3. **The index is bound to (account permissionId, concrete root ID); a mismatch
   wipes it.** Binding is lazy — at first index use, not at `Open` — because mount
   must not require the network. Unbound reads empty and drops writes; an
   unidentified index is treated as no index.

Everything here degrades to memory-only on any failure. The store is
provider-private and lives in its own DB file, *not* in `internal/state` (which
stays engine-level and provider-agnostic).

The in-memory half carries a third map, `kids` (parent path → child paths), so
dropping or moving a subtree costs that subtree instead of a scan of every path
we know — under `rm -rf` the scan was one full-map pass per unlinked file, with
`d.mu` held, and measured as clean O(n²). Its invariant is that everything in
`idByPath` is reachable by walking `kids` from the root; it is maintained *only*
by `linkLocked`/`unlinkLocked`, because a `kids` set that drifts from `idByPath`
makes a delete miss a descendant and leave the stale mapping that row 1 above
exists to prevent.

Also: **a rename must be reported inbound as remove-old + add-new**, because
`provider.RemoteChange` carries no identity and Drive's feed never mentions a path
an object has left. `vacatedPathLocked` derives the old path from the in-memory
reverse map — not the persistent index (acting on a hint here means deleting a
local file) and not for directories (a removal above the seam is a recursive
delete, and Drive reports no changes for a moved folder's children, so they would
not come back until a sweep). Without it every rename duplicates the file on every
other client until the next sweep. See DESIGN.md §2.5.

Also: a file's `parents` carry the **concrete** root ID, never the `root` alias
that `-drive-root` defaults to. Comparing against the alias is what made the parent
walk climb past the mount root and drop every inbound change to a top-level file
(fixed in M7 by resolving the alias once, up front).

Also: **a path can name more than one object, and the tie-break is not the whole
story.** Drive allows same-name siblings and two clients creating one path
concurrently produce them (MC-30, covered by `gdrive/siblings_test.go`).
`lookupChildLocked` picks the most recently modified and logs that the others are
now invisible, which is defensible in isolation; what the fleet sees is stranger.
A `Remove` deletes only the visible sibling, so an ordinary `rm` is followed by
the path reappearing with an older sibling's bytes — everywhere. And `Enumerate`
reports the path once per sibling, which `reconcileRemote` applies in listing
order, so after a sweep the local file can hold a different sibling than the one
a push would update. No mitigation is implemented on purpose: each candidate
(create-if-absent, post-create dedup, surfacing siblings as distinct paths) loses
something a user wrote or makes resolution non-deterministic, and the choice is
the user's to be told about, not ours to make silently. See
`docs/dev/multiclient-test-plan.md` §4.2.

Deliberately absent: any startup enumeration of the remote tree. Warming the cache
that way is a long, quota-heavy walk, and what to do with what it finds is a policy
question, not a caching one.

## Enumeration & reconcile (M7b, M7c)

The change feed only reports what changed *after* a cursor was taken, so a Drive
that existed before the first mount was invisible. `Enumerate` (optional
`provider.Enumerator`, one flat `files.list`) makes the tree present;
`syncengine/reconcile.go` decides what to do with it. It runs off the FUSE path,
automatically on a first run / a resumed sweep / a dead cursor / `-sweep-interval`
elapsed, and on `-resync`.

1. **Snapshot, then tail.** Take the change-feed start token *before* the sweep and
   give it to the pull loop only *after* the sweep finishes. The overlap replays
   idempotent changes that §4 drops; the other order loses everything that changed
   during the sweep. The `Downloader` owns both because it owns the cursor.
2. **A delete is inferred only from a baseline, never from absence alone.** The
   baseline is the §4 echo store ("we have synced this content at this path"); the
   per-generation `seen` marks in `internal/state` answer "did this sweep observe
   it?". No baseline ⇒ the path is new, whichever side it is on ⇒ **the first-ever
   run deletes nothing.** Four guards make the delete rows safe, and none is
   optional: deletes run only after a *complete* sweep; the baseline must predate
   the sweep (or a file created locally *during* it looks remotely deleted); a local
   copy that diverged from its baseline is kept and pushed back, never deleted; and
   `-max-deletes` abandons the whole pass rather than trimming it, because a huge
   count means the premise is broken (wrong `-drive-root`, empty `-data`), not that
   there are 4000 real deletions. `Store.Remove` on Drive now trashes rather than
   deletes (see the CLI note on `-drive-delete`), which makes a pass this cap
   failed to catch recoverable for 30 days — the trash is the guard *behind* this
   one, never a reason to soften it, and `delete = "permanent"` turns it off. A
   refused pass still counts as a *completed* sweep, so
   recovery is `-resync` **and** a higher cap — raising the cap alone changes
   nothing until the next `-sweep-interval`, and the refusal message says so.
3. **The marks are persistent, and the reason is resume.** An in-memory seen-set
   would report every page a *previous* process consumed as remotely deleted.
   The sweep *completion* stamp is persistent for a different reason: the periodic
   schedule measures from it, and someone who mounts for an hour a day would never
   reach an interval counted from process start.
4. **A sweep that reports an empty tree is the dangerous shape** — every synced path
   then looks deleted — so `gdrive.Enumerate` *errors* when it cannot resolve the
   concrete root ID rather than returning nothing. Same alias trap as M7.
5. **The sweep is the only correct place to prune a baseline**, because pruning one
   and inferring a delete are the same decision from the same evidence. Do not add a
   cheaper prune pass: "absent locally" cannot distinguish *already gone on both
   sides* from *deleted locally while we were down, remote still has it*, and
   dropping the second loses a pending delete — the next sweep then finds a remote
   file with no baseline and puts it back. TTL and LRU eviction fail the same way and
   silently. `-sweep-interval` exists because the other four triggers all fire at
   startup, so nothing ever pruned a long-lived mount.

Below the seam: a flat listing has no parent-before-child guarantee, so an object
whose parent is unseen is parked on that parent's ID and released when it arrives;
whatever is still parked at the end is outside the mount and dropped. Resolution
stays *local* during a sweep (a complete listing contains every parent), except on
a resume, where the persistent index stands in for the pages this process never
saw — and with no usable index the sweep restarts rather than silently omitting a
subtree. The sweep doubles as index warm-up via `pathindex.SetMany`, one commit per
page.

The local walk (row 2, "new locally") is on and unflagged because its only action
is a push. It **must** keep skipping §6 conflict copies (local-only by policy —
uploading them publishes the losing side of every conflict) and placeholders
(remote-born; pushing one is the M5 catastrophe). Reconcile pushes go through
`Engine.Push`, never straight to the store, so they get the same M5/M6 gates, echo
recording and retries as a write from the mount.

**M7c: the sweep's cost is the mount's, not the account's.** The flat `files.list`
above scales with the *Drive* — it lists everything and sorts it out afterwards, in
strictly sequential pages, repeated by every mount of that account on every first
run, dead cursor and `-sweep-interval`. At 10^6 objects that is minutes each. Drive
has no recursive "everything under this folder" query (`'ID' in parents` is direct
children only), so the fix is a client-side breadth-first descent from the mount
root, `enumFanout` (8) listings at a time, in `enumerate_scoped.go`. The fan-out is
measured, not assumed — 6.3× at fanout 8 against fanout 1 on one fixture — but only
because the fake grew an injected round trip taken *before* its global mutex;
every request in it otherwise serialises, which made the concurrency claim
untestable rather than merely unmeasured. **Under throttling the descent keeps what succeeded and re-queues
only what failed, and moves its own concurrency** (halve on a throttled listing,
grow back on a clean one, floor 1, ceiling `Drive.fanout` — so the field is a
*ceiling*, not a rate). Both are load-bearing: putting a throttled batch back whole
measured **97× the ideal request count** at a 20% refusal rate, because at fanout 8
most batches are spoiled and re-issuing all eight feeds the throttle. Do not
"simplify" either back into a single error return. Whether real Drive throttles at
eight at all is still open, but it is now a tuning question, not a correctness one.

Three things to keep straight. **The descent is parent-first by construction**, so
the parking machinery above (`waiting`, `parked`, "still parked ⇒ outside the
mount") is the *flat path's alone* — do not "unify" them. **Resume stops mattering**
in scoped mode: the frontier is in memory and a cursor arriving without one restarts
the descent, which is cheap by construction, so the M7 index is not needed to stand
in. And **neither mode dominates**: a descent costs ~1 request per folder against
flat's ~1 per 1000 account objects, so a folder-dense subtree is cheaper flat.
`auto` descends when `-drive-root` names a concrete folder and lists the account
when it names the whole Drive; an unknown mode is refused at open (M8 rule 6 applied
to a value), and a folder-dense descent logs once and names `flat`.

**`drive.file` is not the answer to this, and the reason is structural.** It would
scope `files.list` to app-created files, but that scope reaches pre-existing files
only through the Google Picker — a JavaScript component — and `drivel login` is a
loopback/paste flow with no browser surface to host one. A user could not grant
access to a folder they already have, so their files would be invisible rather than
merely slow. `resolveRootLocked`'s `Files.Get("root")` is a second blocker. The
scope is plumbed (`login -scope drive.file`) and **has no test**; it suits a folder
drivel creates and owns, which is a different milestone.

**Still account-wide:** `changes.list` has no folder filter, so N mounts of one
account each poll the whole feed and discard most of it. That is quota, not latency,
and one poller per account would be exactly the cross-mount coupling M8's guards
exist to prevent — so it waits for evidence.

Google-native Docs/Sheets/Slides carry `RemoteFile.ExportOnly`: reported and marked
seen (so their absence is never read as a delete) but **never materialised and
never given an echo** — they have no byte stream, so there is no honest size for a
placeholder and no digest to compare. Cursor expiry (`provider.ErrCursorExpired`,
Drive's 410) recovers through this same path: fresh token, then a sweep.

## Special files & mount safety (M15 items 1–3)

What a path-addressed cloud store cannot hold, and what the mount does about it.
Four rules; the first three are the milestone and the fourth is what writing it
turned up.

1. **`nodev`/`nosuid` are compulsory and the list is per platform.** No flag
   disables them. `internal/vfs/mountopts_{linux,darwin,freebsd}.go` — and it has
   to be three files, because **FreeBSD would fail to mount at all** with `nodev`
   in the list: its kernel dropped `MNT_NODEV` (only devfs holds device nodes, so
   the property holds by construction) and `mount_fusefs` parses `-o` against a
   fixed table and exits non-zero on an unknown option — `mount_fusefs: -o dev:
   option not supported`, verified on 15.1. The options cover the **mountpoint**
   and say nothing about the backing store, which is an ordinary directory
   reachable without the mount; §10.4's mask-setuid/setgid rule still stands alone.
2. **Hard links are refused with `EPERM`** (`vfs/special.go`). Not a limitation
   being reported — a refusal chosen over the alternative. Falling through to the
   loopback created the link, emitted no event, and left two *regular* files the
   sweep pushed as two diverging remote objects, having told the caller it worked.
   `EPERM` is what `link(2)` documents for a filesystem that cannot make them.
3. **Non-regular files are skipped and the skip is logged, at both sites that
   decide it** — `vfs.node.Symlink`/`Mknod` when the mount declines to emit, and
   `reconcile.pushLocalOnly` when the sweep's walk steps over one, with a count in
   the sweep summary. The wording is **duplicated on purpose**: `vfs.specialKind`
   and `syncengine.kindOf` say the same words from different input types, because
   sharing one helper means the sync core importing the mount backend, which drags
   go-fuse into every build of it and costs §2.9's cross-compile proof. Keep them
   in step; each side has a test.
4. **A regular file is regular however it was made.** `Mknod` with no type bits
   creates one, so that branch emits `OpCreate` like `Create` does. Silence there
   would make syncing depend on which syscall wrote the file — MC-12's push-path-
   versus-sweep disagreement arriving by a second route, and a bug wherever it
   appears.

**FreeBSD cannot create a special file through the mount at all**, so item 3's line
has nothing to describe there: fusefs sends `rdev = ~0` on the MKNOD for a fifo
while FreeBSD's `mknod(2)` takes `S_IFIFO` only with `dev == 0`, and its `mknod(2)`
refuses `S_IFREG` outright (on plain ZFS too, not just through a mount). Both are
below drivel, neither risks data — loud `EINVAL`, no file — and the tests **assert**
them rather than skipping, so the platform is pinned. Don't "fix" this by rewriting
rdev; it is go-fuse's loopback, and device nodes are not ours to invent.

## SFTP (M18)

The second provider, and the first that is **path-addressed**. Everything that
makes it different from `gdrive` follows from that plus one absence, so most of
`gdrive`'s machinery has no counterpart here on purpose.

1. **No path index, and adding one would be a mistake.** `Put`/`Mkdir`/`Move`/
   `Remove`/`Get`/`Stat` are each one protocol call over the path the seam already
   speaks. `internal/pathindex` stays Drive-private; an index here would be a cache
   keyed by its own value.
2. **No `ChangeSource`, and the sweep *is* the inbound path.** SFTP has no
   notification of any kind, so `-sweep-interval` is the **poll interval** and its
   24h default is wrong for every SFTP mount — the user docs say so and so does the
   man page. Do **not** synthesize a feed by polling-and-diffing: M7b already is
   that, with the delete guards that make an inferred deletion safe.

   This is what forced the seam change M19–M21 inherit: `Downloader.src` may be
   nil, `Run` branches once into `runWithoutFeed`, and `app.Mount` wires a
   downloader for `ChangeSource` **or** `Enumerator`. **The empty cursor must never
   be persisted** — `state.Cursor` would report `ok=true`, `startFeed` would read
   that as "already tailing this remote", and every mount after the first would
   skip its startup sweep, i.e. skip inbound sync entirely. `runSweep` guards the
   `SetCursor` with `hasFeed()` for exactly that reason.
3. **`RangePutter` is real, and this is the milestone's point.** SSH_FXP_WRITE takes
   an offset, so M6's gate 2 finally runs against a server rather than a fake. The
   contract is re-checked in `PutRange` and not merely trusted: `O_WRONLY` with no
   `O_TRUNC` and no `O_CREATE`, extents validated before the handle is opened, and
   a remote size that disagrees is an error. Failing is cheap — the engine falls
   back to a whole-file `Put`.
4. **`ContentHasher` is absent, and the reason is the library.** Gate 3 needs a
   server-computed digest; OpenSSH's server has no `check-file`, and `pkg/sftp`
   exposes no way to send an arbitrary extended request, so it is unreachable even
   where a server offers it. Costs nothing: `syncengine.contentMatches` returns
   early on an empty `RemoteFile.Hash` before reading a local byte. If it ever
   becomes reachable it is a **per-session** capability — a wrapper type chosen at
   dial time, never a method on `Store`.
5. **`Version` is `mtime:size` and carries the echo identity alone.** The rsync
   heuristic with the rsync caveat: SFTP's mtime is whole seconds, so a remote edit
   preserving the exact byte length *and* landing in the same second as our own
   write reads as our echo and is not pulled until something else touches the file.
   Documented, not mitigated — the alternatives are worse.
6. **Host key verification fails closed and that IS the milestone.**
   `ssh.InsecureIgnoreHostKey` is absent from the tree and
   `TestInsecureIgnoreHostKeyIsAbsentFromTheTree` greps for it (it is a grep because
   the failure it guards against is someone adding the hatch in a package that does
   not exist yet). `Open` additionally proves *offline* that the host is listed, by
   the documented `knownhosts.KeyError.Want`-is-empty idiom, so the commonest
   misconfiguration is a startup error naming the file rather than a retry loop.
   **There is no password and no passphrase option and there must not be one** — a
   credential in cleartext beside the file it protects, for an account that usually
   grants a shell. Encrypted key ⇒ agent.
7. **`Put` is write-to-temp-then-rename.** The whole-file `Put` is the fallback
   behind every M6 gate so it runs constantly, and an interrupted in-place write
   would leave the remote truncated with no digest gate to ever notice. `Enumerate`
   skips the `.drivel-upload.` prefix so a temporary stranded by a dropped
   connection is neither materialised locally nor later inferred as a deletion.
   `conn.rename` takes `posix-rename@openssh.com` when the session advertises it —
   the fallback is not just slower, it has a window where the destination is absent.
8. **`Remove` is permanent and there is nothing behind it** — the group-wide
   decision above, arrived at here first. `-max-deletes` is the only guard, and
   `docs/user/sftp.md` says so in those words. Don't re-open it per backend.
9. **Enumeration is a breadth-first descent whose cursor is the frontier**, since
   there is no recursive list. Parent-first by construction, so gdrive's parking
   machinery has no counterpart. A directory that vanished mid-sweep is skipped;
   **every other listing failure abandons the sweep**, because a sweep that quietly
   omits a subtree still reports itself complete and the delete pass would then
   propose deleting everything under it.
10. **Containment is structural, not checked.** `path.Join(root, p)` resolves a
    leading `..` by climbing *out* of root; `conn.abs` cleans against a virtual `/`
    first so it collapses instead. A test asserts the property — it caught the bug.
11. **Reconnection is discard-and-redial, not a healing wrapper.** A dead session is
    dropped (`Store.drop`, which only clears the conn it was handed, so a slow
    worker cannot tear down its replacement) and the error is returned **retryable**
    so the engine's existing backoff re-drives the operation onto a fresh
    connection. `io.EOF` counts as a dead transport here — safe only because `Get`
    hands the caller the remote handle directly and `Put`/`PutRange` copy from local
    readers, so no ordinary end-of-file reaches `classify`. Getting this wrong made
    a dropped connection permanent and stalled the mount until restart.

12. **A hashless provider could not apply a remote deletion at all, and this was
    found by running it, not by a test.** `reconcile.matchesBaseline` compared a
    local MD5 against `state.Echo.Hash` — the *remote's* digest, always empty here
    — so every file read as "diverged", and a file deleted on the server was kept
    locally and pushed straight back on the same sweep, forever. `state.Echo` now
    carries **`LocalSize`/`LocalMTime`**, a fingerprint of the *backing* file taken
    when the baseline was written, and `matchesBaseline` falls back to it when
    `Hash == ""`. Three things are load-bearing: it fingerprints the **local** file
    (nanosecond mtime, so any write moves it — not the coarse wire mtime `Version`
    is stuck with); **a zero `LocalMTime` means "not recorded", never "matches"**
    (old state DB, directory, failed stat — the wrong answer there deletes
    somebody's edit); and the push side records it **after** the upload, because a
    write that landed mid-upload has already queued another event that will
    re-record it, while a pre-push fingerprint would describe bytes no longer on
    disk. Drive takes the digest branch exactly as before.
13. **Directories report a constant `Version` (`"dir"`).** Empty made
    `Echo.Matches` false for every directory every pass, so the downloader
    re-created and re-logged each one — invisible at Drive's 24h sweep, the entire
    log at a 3s poll. Their mtime is no better: it moves when a child is added.

**The tests run a real SSH server with a real SFTP subsystem in-process over
loopback** (`server_test.go`), against a real temp directory. A hand-written fake
would encode this package's beliefs about the protocol, which is precisely what a
provider gets wrong. It is also what covers the dial, auth and host-key paths.

**Verified against OpenSSH on 2026-09-10** through a real mount (`sshd` +
`internal-sftp`): pre-existing tree materialised, local write pushed, remote edit
pulled, rename and delete both ways, the diverged-copy guard held, and a 4 MiB
edit to a 64 MiB file moved 4 MiB with the two left byte-identical. Two of the
items above (12 and 13) were found by that run and by nothing else — **a green
unit suite is not evidence this backend works**. Still unrun: a server *without*
`posix-rename@openssh.com`, and anything that is not OpenSSH. Setting up a rig: an
**external** `Subsystem sftp .../sftp-server` runs through the login shell, so any
byte that shell prints corrupts the stream and reads as `packet too long` —
OpenSSH's own client fails the same way, it is not a drivel bug. Use
`internal-sftp`.

## Multi-account & multi-provider (M8)

One process, N mounts, each with its own credentials, state DB, index and engine.
Everything below the wiring was already per-instance, so the milestone is about
composition — and about the ways several mounts can corrupt each other.

1. **The three-phase lifecycle is not cosmetic.** `app.Open` acquires, `Run` serves
   then drains, `Close` releases. That split is what lets a process that fails to
   bring up mount 3 of 5 unmount and drain the two that came up. Inside `Run` the
   per-mount ordering is unchanged and still load-bearing: unmount → `close(events)`
   → wait for the engine's bounded drain, engine on a **detached context**. `App`
   aggregates around that; it must never collapse it into one cancellation. (gosec
   flags the detached context — the waiver is deliberate, not an oversight.)
2. **`app.Validate` runs before anything opens, on the flag path too.** Each guard
   exists because the failure is otherwise a deadlock, an opaque five-second bbolt
   timeout, or a file synced to the wrong account: shared state DB (each engine
   reads the other's echoes as its own baseline — which is also M7b's *delete*
   baseline), either direction of backing-tree/mountpoint overlap (§2.7's cardinal
   rule, generalised), shared mountpoints, duplicate names, and **a state DB inside
   a backing tree** — the one that needed no second mount to be wrong, since the DB
   syncs itself and its own writes generate more events.
3. **The registry is a value, not a package-global.** No `init()` registration, no
   import for side effect. It would be the only process-global mutable state in the
   tree, in the milestone whose premise is that there isn't any — and the explicit
   form is what lets a test register `gdrive` twice and run two Drive stores at
   once, which is M8's seam proof. **The pseudo-provider is test-only and stays
   that way**; nothing shipped registers a duplicate.
4. **Provider config crosses the seam undecoded** (`provider.Params.Decode` fills a
   provider-defined struct). `internal/config` must never learn what a Drive folder
   ID is. Adding a provider touches neither package.
5. **The config file is only ever appended to, never re-serialized.** TOML was
   chosen for comments; any encoder round trip drops them all. `login` prints an
   existing account for the user to reconcile rather than replacing it.
6. Unknown config keys are an **error** — `lazzy = true` doing nothing is the same
   failure as a flag that stopped being read. Free-form regions (an account's
   provider settings, `[mount.provider]`) are exempt because that is their purpose.
   `max-deletes`/`sweep-interval` are pointers so an explicit `0` survives; merging
   it with "unset" would silently uncap M7b's delete guard.
7. Inside a provider table a value is path-expanded **only when written like a
   path** (`~/`, `./`, `../`). This layer cannot know which keys name files, and
   rewriting a Drive folder ID would be silent corruption. `login` writes absolute
   paths so the common case never relies on it.
8. **Logging is per mount** and a single mount stays unprefixed, so its output is
   byte for byte what it was pre-M8. New log calls belong on `e.logf`/`d.logf`/
   `d.logf`/`n.logf`, never `log.Printf`.

## fstab & non-interactive mount (M16)

`drivel` is a `mount(8)` helper when reached as `/sbin/mount.fuse.drivel` (or
`drivel mount-helper`), so a mount can live in `/etc/fstab`. **Linux only.**
`docs/user/fstab.md` is the user-facing half. Four things carry the correctness.

1. **The fstab type is `fuse.drivel`, not `drivel`.** go-fuse mounts as `fuse.` +
   `MountOptions.Name`, so `/proc/self/mountinfo` says `fuse.drivel` whatever the
   line says — and systemd's fstab-generator compares the two. `mount.drivel` is
   installed as an alias only. `MountSpec.FsName` carries the fstab device field
   for the same class of reason (`findmnt`/`umount` match on it); empty still means
   `"drivel"`, so nothing pre-M16 changed.
2. **The helper exits once the filesystem is live, and not before.** `mount(8)`
   blocks until the helper returns, so serving in the foreground hangs the boot,
   and exiting early makes `mount` report success for a mount that may never
   appear. The parent re-execs `/proc/self/exe` with the original argv and a marker
   env var; the child mounts and reports `ok` or the error through an inherited
   pipe (fd 3). This is what `mount.Options.Ready` is for — **do not replace it
   with polling `/proc/self/mountinfo`**, which cannot tell "starting" from
   "wedged" and has to guess which entry is ours.
3. **The daemon's stdio goes to `logfile=` or `/dev/null`, never to the caller's.**
   An inherited descriptor stays open for the life of the mount, so a caller
   capturing output through a pipe (`out=$(mount -a 2>&1)`, Go's `CombinedOutput`)
   blocks until unmount — the exact opposite of daemonizing. Journal integration is
   what this gives up; `logfile=` is the replacement. Relatedly there is **no
   default `mount-timeout`**: systemd already bounds a mount unit's startup, and a
   second bound with a different default is a second answer to one question.
4. **The privilege option is `run-as=`, never `user=`.** `user` is fstab's "a
   non-root user may mount this", and util-linux passes `user=NAME` down to the
   helper to record who did — acting on it would act on an option nobody aimed at
   us. `user`/`users`/`owner`/`group` are accepted and ignored. In `runAsEnv`,
   **unsetting the inherited `XDG_*` variables is the load-bearing half**: root's
   `XDG_CONFIG_HOME` surviving the drop leaves the mount running as the user while
   reading the wrong account's token.

**Every mount-shaping flag has an option, and `TestEveryShapingFlagHasAnFstabOption`
is what keeps it that way** — it walks `mountShapingFlags` and fails on "unknown
option", asserting only that the name is *recognised* (the value it passes is
deliberately nonsense, so a type error still counts and the test needs no table of
plausible values to drift). It was added because four flags had already gone
missing: `drive-delete`, `drive-sweep-mode`, `upload-workers`, `hydrate-workers`.
`-pprof`/`-pprof-allow-remote` are the deliberate exception — a debug endpoint on
the *process* is not something an unattended boot mount should open — and they are
not shaping flags, so the test does not ask for them.

Below that it is a translation layer and must stay one: the option table ends in
the same `app.MountSpec` the flag and config paths build, so validation, opening
and the shutdown ordering keep one implementation. `config=` refuses the shaping
options exactly as `-config` does, and **derives that set from
`mountShapingFlags`** rather than restating it. `ro`/`remount`/`bind`/`move` are
*refused*, not ignored — a line claiming a read-only mount over a writable one is
`lazzy = true` in a new place — and an unknown option is an error unless `mount
-s` asked otherwise. The state DB defaults under `$XDG_STATE_HOME`, because an
fstab mount's working directory is `/`, where the flag path's `drivel-state.db`
would mean `/drivel-state.db`.

**Untested: systemd.** The daemon stays in the generated `.mount` unit's cgroup
and no systemd host was available. `mount`, `mount -a` (`-T`) and `umount` are all
verified for real. Don't promote it to "works under systemd" without a run.

## Shell completions

`completions/drivel.bash` and `completions/_drivel` are **generated, not written**
— `make completions`, gated in `make check` by `make completions-check`. Four
things carry it.

1. **The flags are the source.** `mountFlagSet`/`loginFlagSet` exist so the
   program and the generator walk one `flag.FlagSet`; that is why flag definition
   is a function rather than a block inside `runMount`. Nothing in the generator
   lists a flag by name.
2. **The generator is build-tagged and the reason is layering, not size.**
   `cmd/drivel/completions.go` is `//go:build completions`; `completions_off.go`
   is its `!completions` half, holding the `runExtraCommand` stub. That pair is
   the same shape `mountopts_{linux,darwin,freebsd}.go` uses — no `init()`, no
   package-global registry to mutate (M8 rule 3), the compiler decides. It is also
   what keeps `internal/completion` out of every release binary.
   `runExtraCommand` returns `(handled, err)` and not just `err`: a generator that
   ran and failed must report why, not fall through to "unknown command".
3. **`completionHints` is the only hand-written part, and it fails both ways.** A
   flag with no entry fails the generator; an entry naming a flag that no longer
   exists fails it too. The kind is cross-checked against `IsBoolFlag`, and enum
   values are the program's own constants (`gdrive.DeleteTrash`, …) so a renamed
   mode cannot leave the completions offering a word the binary refuses. The
   summary is deliberately *not* the flag's usage string — usage is a paragraph,
   a menu line is a line.
4. **The generated files are committed** (a distro package has no Go toolchain)
   and installed by `make install`. `App.Validate` refuses one flag name meaning
   two different things across subcommands, because bash completes a value from
   the previous word alone and cannot tell which subcommand it is in.

The renderers are tested by **running** the output — bash sources the generated
script and answers a real completion; zsh parses the file. An assertion that
cannot fail is worse than none: the opaque-value case only discriminates when the
half-typed value starts with a dash, since with an empty one the flag branch
declines too and the test passes either way.

## Transport

All Drive traffic goes over **HTTP/3** (QUIC), required by project decision. Go 1.27
stdlib has no HTTP/3 client — use `github.com/quic-go/quic-go` (`http3.Transport`).
It's HTTP/3-*preferred*: fall back to HTTP/2 when the QUIC/UDP dial fails. The
transport sits **below** OAuth — pass the client via `option.WithHTTPClient` and fold
the token source into `oauth2.Transport{Base: ...}`; do **not** also pass
`WithTokenSource` (conflicts). See DESIGN.md §2.6.

## Mount modes

`drivel mount -mount DIR -data BACKING` uses a separate backing directory
(portable; required off Linux). Omitting `-data` selects **in-place mode**: the
mount dir is its own backing store, so files remain in it after Drivel exits. It
works by opening a dirfd to the mountpoint *before* mounting and routing backing
I/O through `/proc/self/fd/N` (Linux-only). **Cardinal rule:** in in-place mode,
never access the backing store by the mountpoint path — only via `backing.Path`
(the `/proc/self/fd/N` handle) — or reads/writes recurse into our FUSE handler and
deadlock. See DESIGN.md §2.7. New mount backends implement `mount.Backend`; keep
everything below the seam provider- and FUSE-agnostic.

## Platform support

Tested on Linux and FreeBSD; macOS is compile-verified only. The whole tree
*compiles* for `darwin/{amd64,arm64}`,
`freebsd/*` and every `linux/*` arch; on `windows/amd64` everything builds except
`internal/vfs` (and `app`/`config`/`cmd`, which merely import it). **Every
cross-compile failure on every target traces to go-fuse and nothing else** — which
is the strongest available proof that the §2.5/§2.7 seams hold. Adding a platform is
a `mount.Backend`, never a port. DESIGN.md §2.9 has the full matrix and reasoning;
don't re-derive it, and don't promote "builds" to "supported" without a live test.

Four things differ off Linux. The second is now a *filesystem* matter rather than a
platform one, and the fourth is not a degradation at all — it is a kernel behaviour
the mount layer has to accommodate:

1. **In-place mode is Linux-only** — `/proc/self/fd/N` has no portable equivalent;
   `openInPlace` refuses elsewhere. `-data` is mandatory there.
2. **M5's xattr marker is implemented on all three platforms (M10) and absent on any
   backing store that cannot hold it.** `hydrate/xattr_unix.go` is one body for linux
   **and** darwin over `golang.org/x/sys/unix`; `xattr_freebsd.go` is separate
   because `extattr_*` takes the namespace as an argument; `xattr_linux.go` /
   `xattr_darwin.go` hold only the missing-attribute errno (`ENODATA` vs `ENOATTR`)
   and the native-store probe. Keep the linux+darwin body shared — it is what makes a
   Linux test run cover the macOS code on a platform no maintainer can run. **The
   freebsd file is verified on its own OS (2026-09-06, FreeBSD 15.1, `=all`, nothing
   skipped, `TMPDIR` on ZFS); the darwin half is not — don't promote it.** Neither
   thing that file feared appeared on FreeBSD: no short write from
   `extattr_set_file`, and no sign of the `//go:uintptrescapes` wrappers failing.

   **The marker name is per platform**: `user.drivel.placeholder` on linux/darwin,
   `drivel.placeholder` in `EXTATTR_NAMESPACE_USER` on freebsd. Always
   `hydrate.XattrName`, never the literal.

   **Where the backing filesystem has no user xattrs, `-lazy` is unsafe, full stop**
   — drvfs under WSL2, exFAT/FAT, a tmpfs `/tmp` on FreeBSD. `setxattr` fails,
   `CreatePlaceholder` continues unmarked, and there is then no placeholder record at
   all, because **`IsPlaceholder` reads the marker and nothing else**. The state DB
   does *not* stand in: its hydration entries cache present ranges (`Hydrator.Ranges`,
   which nothing outside tests reads) and no path has ever consulted them for
   placeholder-ness. Docs said otherwise until M10 — "don't delete the state DB" was
   never the mitigation; "don't run `-lazy` there" is.
3. **A case-insensitive backing filesystem is a correctness surface**, and macOS
   defaults to one (APFS and HFS+). Drive is case-sensitive, so `Foo.txt` and
   `foo.txt` collide into one local path — the MC-30 sibling problem arriving by a
   second route. Nothing handles it; a live macOS run should find out what happens.
4. **A kernel may read through a write handle, so `node.Open` opens the backing file
   readable whatever the caller asked for.** FreeBSD's fusefs fills a buffer-cache
   block before writing part of it and falls back to the write filehandle for that
   read-modify-write, so a READ arrives on a handle opened `O_WRONLY`; the backing
   fd is then write-only, go-fuse preads it to serialise the fd-backed `ReadResult`
   when it writes the reply, and the `EBADF` surfaces as the *write* failing. It is
   deliberately unconditional rather than build-tagged, so Linux CI covers the path
   — the `xattr_unix.go` argument again — and the fallback to the caller's flags is
   not decoration: write permission does not imply read permission. Found by running
   `internal/vfs` on FreeBSD; darwin shares the same go-fuse reply path, so if
   macFUSE does the same thing the case is already covered. `Create` needs no
   equivalent: its handle only ever needs blocks it authored.

5. **The fstab mount helper (M16) is Linux-only**, by mechanism rather than
   omission: the `/sbin/mount.<type>` lookup, the helper argument protocol, the
   privilege drop and the re-exec handshake are all how Linux brings a filesystem
   up unattended. `runMountHelper` refuses elsewhere and says to run `drivel
   mount`. The *option parser* is deliberately in the portable file, so its tests
   run on every platform.

**On macOS a successful `setxattr` does not mean the filesystem stored it.** On a
volume without native EAs (exFAT, FAT, some SMB/NFS) macOS emulates them in an
AppleDouble `._name` sidecar, which would put M5's marker in a *file inside the
backing tree* — synced like any other file and detachable from what it describes.
`xattrNative` (darwin) probes for the sidecar and reports emulation as "no xattrs".
Don't drop that check for looking redundant; keep macOS backing dirs on APFS/HFS+.

**On FreeBSD the `//go:uintptrescapes` wrappers are load-bearing, not decoration.**
x/sys types the extattr buffer as a `uintptr`, which neither keeps the array alive
nor survives a stack copy, and escape analysis confirms the buffer would otherwise
stay on the stack. `runtime.KeepAlive` does not fix this one. Also: `extattr_set_file`
returns a byte count, so a short write is a real case there and must stay an error.

**macOS specifics.** go-fuse probes exactly two paths (`mount_macfuse`,
`mount_osxfuse`) — verified in v2.10.1 *and* v2.11.0 — so **FUSE-T does not work**,
and it is not a build-tag away: go-fuse speaks the raw FUSE protocol rather than
linking libfuse, which is FUSE-T's integration point. If that is ever fixed, the
selection must be **runtime detection, not build tags** — which helper exists is a
property of the running machine.

**AGPLv3 is not in conflict with macFUSE.** go-fuse contains no cgo and does not
link libfuse; it `exec`s the mount helper and talks over an fd, which is arms-length
communication between separate programs, not a combined work. The only rule this
imposes is a distribution one: **never bundle macFUSE, never ship an installer that
fetches it.** macFUSE 4.x's own non-free terms are a burden on the user, not a
licence conflict — don't restate them as one. §2.9.2 has the argument.

**Windows is a decided non-goal, not a gap** (§2.9.4): WSL2 covers the audience,
Drive for Desktop covers the rest, WinFsp/cgofuse forfeits the pure-Go build, and
NTFS case-insensitivity opens a *new* variant of the MC-30 same-name-sibling problem
(Drive is case-sensitive; `Foo.txt` and `foo.txt` collide into one local path). Keep
the seam clean anyway — the cross-compile shows that costs nothing.

## Build / test / run

```sh
make help                                      # every gate as a target, plus examples
make check                                     # everything CI runs, in CI's order
make completions                               # regenerate completions/ after a flag change
make hooks                                     # install the git hooks (once per clone)
make run ARGS='-debug'                         # build, then mount ./mnt over ./data

go build ./...
go vet ./...
go test -race ./...                            # what `make test` runs
DRIVEL_REQUIRE_TESTENV=all go test -race ./... # ...and nothing silently skipped
go build -o ./bin/drivel ./cmd/drivel
./bin/drivel mount -mount ./mnt -data ./data   # separate backing dir; -debug for FUSE tracing
./bin/drivel mount -mount ./dir                # in-place: ./dir is its own backing (Linux)

sudo make install                              # + /sbin/mount.fuse.drivel for fstab
```

`./mnt` and `./data` are gitignored scratch dirs; create them (the binary
mkdirs them too). Operate on `./mnt`; changes land in `./data` and log as sync
events. Ctrl-C unmounts.

## Environment notes

- Mounting needs the `fusermount3` helper from the **`fuse3`** package (Debian);
  `libfuse3-dev` alone is only headers. Install: `sudo apt-get install -y fuse3`.
- This runs in a **privileged Docker container** with passwordless `sudo` and
  `/dev/fuse` present, so unprivileged FUSE mounts work once `fuse3` is installed.
- If a run leaves a stale mount: `fusermount3 -u ./mnt`.
- HTTP/3/QUIC wants a larger UDP receive buffer or quic-go logs a warning; raise it
  with `sudo sysctl -w net.core.rmem_max=7500000` (and `wmem_max`) when testing sync.

## Conventions

- `context.Context` threaded for cancellation/shutdown; the mount unmounts on
  SIGINT/SIGTERM, which makes `server.Wait()` return.
- Change events use root-relative paths (no leading slash); `NewPath` is set only
  for `OpRename`.
- Keep the FS layer provider-agnostic — depend on `internal/provider`, never on a
  concrete Drive type.
- FS operations must not block on the network; sync happens off the FUSE path via
  the buffered event channel.
- Secrets (`credentials.json`, `token.json`, `*.local.json`) are gitignored —
  never commit them.
- **AGPLv3, copyright SystemHalted and Jeremy Melanson.** `LICENSE` is the
  verbatim FSF text — never edit it, and never add a second license file at the
  root (two of them confuse the `licensecheck` detector pkg.go.dev uses; see
  `docs/project/publishing.md`). The copyright notice lives in four places that must stay
  in sync: README §License, `docs/user/drivel.1`, the `cmd/drivel` package doc comment,
  and the `usage()` text `drivel help` prints. The grant is version 3, *not*
  "or later" — promoting it is a licensing decision, not an editorial one.
