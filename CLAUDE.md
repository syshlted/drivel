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
  Since M23 it **bundles** every shipped backend (`bundle_on.go`'s
  `bundledBackends`; `bundle_off.go` empties it behind the `nobundle` tag) and
  launches one by re-executing itself as `drivel plugin-serve <kind>`, so
  `newRegistry` is that map plus a scan of the plugin search path for anything
  else. It is the **only** package above the seam allowed to name a provider, and
  `internal/app/seam_test.go` asserts that. `cmd/drivel-provider-gdrive` and
  `cmd/drivel-provider-sftp` still build those two as installable plugins — not in
  the default build, kept because an out-of-tree backend is built that way and
  because the installed launch path would otherwise rot.
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
- `internal/control` — M14's server: the control socket (`listen.go`, private
  directory and stale-socket rules), the peer-uid check per platform, the routes,
  the NDJSON stream and the Prometheus adapter. Imports `syncengine` for its
  types and **nothing else of ours**: `cmd/drivel` hands it the open mounts behind
  its own `control.Mount` interface. `wire.go` holds the JSON types, deliberately
  separate from the engine's.
- `internal/fsevent` — backend-neutral change `Event`/`Op` types (shared by any
  mount backend and the sync engine).
- `internal/mount` — the mount-backend seam: `Backend` interface + `Options`, and
  `ResolveBacking` (separate-dir vs in-place). Platform bits in `backing_*.go`.
- `internal/vfs` — the go-fuse mount backend: loopback that proxies to the backing
  store and emits an `fsevent.Event` per mutation. Reads/lookups/attrs pass through.
  `special.go` holds M15's refusals and skips; `mountopts_*.go` the compulsory
  `nodev`/`nosuid`, which is per platform for a reason (see "Special files" below).
- `provider` — **public**: the backend seam. `Store` + the seven optional
  capability interfaces, `Registry` (kind → `Factory`, an explicit value rather
  than an `init()`-filled package map, M8), `Config` (a provider's own settings as
  the TOML the user wrote), and M9's capability negotiation (`Capability`,
  `Declarer`, `Capabilities`, `As*`). Public since M9 because an out-of-tree plugin
  has to import it.
- `ranges` — **public**: the block bitmap (`Set`) used two ways — M5's
  present-ranges and M6's dirty-ranges. Pure, no I/O. Public because
  `provider.RangePutter`'s signature names `ranges.Range`. It lives outside
  `hydrate` on purpose: M6 runs in eager mode too, and the default path must not
  import the lazy package to describe a write.
- `plugin` — **public**: M9's out-of-process loading. `Loader` (discovery, the
  safety checks, one `provider.Factory` per kind found), the host-side proxy and
  process supervisor, and `Serve` — the three lines a backend's `main` calls.
  `plugin/internal/pb` is the generated protocol; `proto/` holds the `.proto`.
  `plugin/testdata/drivel-provider-fake` is the config-driven backend the tests
  build; it is under `testdata` so `./...` never matches it.
- `internal/provider/gdrive` — Google Drive impl of the interface.
  `gdrive/gdconf` is a leaf holding `Config` and the two enumerated types, split
  out in M9 so `cmd/drivel` can build a Drive settings table and generate its
  completions without linking the Drive SDK; `gdrive` re-exports them as aliases.
- `internal/provider/sftp` — SFTP impl (M18). Host key verification fails closed.
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
- `internal/claim` — leaf: the `.drivel-backing` marker each mount writes at its
  backing root and holds an flock on, so a second drivel *process* can see it
  (MC-47's cross-process overlap; `app.Validate` covers one process only). Read by
  `app.Open` (refuse inside a live claim) and the sweep's walk (prune a marked
  subtree, and on Linux a foreign `fuse.drivel` mountpoint).
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
  and drawing brief, and `licensing.md` (why MPL-2.0; the decision record).
- Root: `README.md` is a slim landing page, `CHANGELOG.md` is the **externally
  facing** change record (abbreviated, in user terms — not a milestone log),
  `CONTRIBUTING.md`, `SECURITY.md`, and `MANIFESTO.md` — the nine commitments
  that decide arguments, written for someone deciding whether to trust Drivel
  with their files. It is **public-facing and normative**: it is quoted back at
  us, so a change to it is a change of position, not an edit. Its claims are
  bound by the same rule as the rest of the copy — no promise about traffic or
  telemetry, and nothing answering for a backend (see `docs/project/marketing.md`).

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

## Website

**`docs/dev/website.md` is the contributor-facing version of this section** —
the published one, kept to the standard of the two collections. What follows is
the short form; when they disagree, that file is right.

`site/` is a Hugo site (theme: Hextra, pulled as a Hugo *module*, so `site/` has
a `go.mod` of its own that the root module never sees), published to
`syshlted.github.io/drivel/` by `.github/workflows/pages.yml`, which pins
`HUGO_VERSION` — build with that version or don't trust the result.

**It renders `docs/user/` by mounting it, never by copying it.** `hugo.toml`
mounts `../docs/user` as `assets/manual` and `content/docs/_content.gotmpl` turns
each file into a page, lifting the title out of the H1 those files open with
(they are written to be read on GitHub first) and **stripping that H1 from the
body**, because Hextra prints the title itself and the page otherwise carried it
twice. `layouts/_markup/render-link.html` rewrites their relative `*.md` links:
a sibling becomes its published URL, anything reaching outside `docs/user` goes
to the repository, because it has no published counterpart.

Three things about the layouts. **Do not invent `hx:`-prefixed classes** —
Hextra ships a *precompiled* Tailwind sheet holding only the utilities its own
templates use, so an invented one may simply not exist, silently. Drivel's own
CSS is `assets/css/custom.css` (a path Hextra looks for by name), all of it
prefixed `drivel-`. And **two partials are deliberate overrides of the theme's**,
each with its reason in a comment at the top: `favicons.html`, because the
theme's points at Hextra's own mark, and `banner.html`, because the theme's is
dismissible and this one is the data-loss warning.

Two `//go:build ignore` generators make every image the site serves, both run
with `go run` and neither wired into `make`; **`site/` holds no artwork master
of its own.** `contrib/gen-logo-assets.go` derives everything from the official
artwork — the crop, its braille rendition, the 16:9 scaled set, the two-colour
page backdrop, and the background-removed cut-out the site uses as its hero.
`contrib/gen-site-images.go` cuts the icons out of that cut-out. Its crop
rectangle and braille threshold were *recovered* from the 1.0 artefacts rather
than chosen, and `-verify` asserts that (it needs the 1.0 files, which are no
longer in the tree, so it now reports that it cannot run).

**Background removal is connectivity, never a brightness threshold**, and the
trap is specific: a pass to catch background pockets the border fill cannot
reach was written, measured and deleted because at any tolerance loose enough
to find them it also ate drool drips, puddles and an eye highlight. The
surviving pockets are the paper's colour and cost nothing visible. Don't
reintroduce it. The artwork version is named in `site/hugo.toml` and in
`gen-site-images.go` — both, after a redraw. See `docs/project/mascot.md`.

The backdrop is **mounted from `contrib/`, not copied into `site/`**, and its
URL reaches the stylesheet through `_partials/custom/head-end.html` — because
Hextra serves `custom.css` from two different depths, so no relative `url()` in
it is correct in both, and a root-relative one breaks under the `/drivel/` path
prefix. A template is the only thing that can ask Hugo where the asset landed.

**The work-in-progress warning has one source: `docs/user/status.md`.**
Everything else — the site-wide banner (`hugo.toml`'s `params.banner`), the
`README`s, the man page, `CHANGELOG.md` — is a short pointer at it, and stays
short. It is written as a markdown alert (`> [!CAUTION]`) wherever it appears in
the manual, because GitHub and Hextra both style that already, so one form works
in both places. When it stops being true, it comes out of all of them together.

## CLI

`drivel` has two main subcommands (`mount` is the default, so `drivel -mount …
-data …` still works): `drivel login` (interactive OAuth wizard → credentials.json +
token.json) and `drivel mount`. Since M14 four more talk to a running mount over
its control socket — `status`, `pause`, `resume`, `stop` — and do nothing without
`mount -control`. **Both entry points build their registry through
`newRegistry()` in `cmd/drivel/mount.go`** — the flag/config path and the M16 mount
helper — because a backend registered in one but not the other is a config file
that works from the shell and fails at boot. Two more are **hidden and stay
hidden**: `mount-helper` (M16) and `plugin-serve <kind>` (M23, the host as one of
its own backends), both absent from `usage()`, `help` and the completions
deliberately, and the second one with a test asserting it. `drivel login` is Drive-specific by
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
folder was trashed its child still read `trashed = false`, so don't write code
that assumes the flag is set the moment `Remove` returns. **This note said
"nothing depends on the timing" until 2026-10-16, and something does: the change
feed.** A client's own `rm -rf` can be partially undone by its own feed —
`[sync] delete sub1/e20.txt`, three seconds later `[pull] download
sub1/e20.txt`, five seconds after that `[pull] delete sub1/e20.txt`. The page
still reported the child as present, and the delete push had already cleared
that path's echo, so §4's loop breaker had nothing to match and the change read
as an ordinary remote create. **The file heals on the next page; the re-created
directory does not** — the later `[pull] delete` removes only the file, leaving
an empty directory that is permanent at `sweep-interval = 0`, and *with* a sweep
is worse: a local-only directory with no echo is the local walk's "new locally"
row, so the next sweep pushes it back and re-creates remotely a directory the
user deleted. Reproduced in a single-client probe (1 of 8 seed-and-delete cycles
— only the cycle whose poll landed inside the window) after MC-51's N=1 row sat
at `converge = timeout` for an hour. **Fixed 2026-10-16 by §4.5's tombstone**
— see "Deletions and §4" below, and note that the fix is *not* the "inbound
change older than our own deletion" this note first proposed: that compares our
clock against Drive's. See the test plan's MC-23 row. **Verified against real Drive on 2026-09-09**
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
evidence. **Until 2026-10-16 it bought nothing at all on Drive**, because
`gdrive.Put` held the provider's `d.mu` across its transfer, so four workers
queued on one mutex; the sentence above was describing an engine whose provider
undid it. A backend is free to serialise itself and the engine cannot tell — see
"Plugin architecture" rule 1 for the general version of that — so a new backend's
`Put` has to be read for this, not assumed. **The inbound pull loop is still serial and has no knob**, deliberately:
concurrency there is not a parameter but a change to the `forget` map, the
cursor-advance ordering and per-path ordering within a page. Anyone adding a
"download-workers" name must not point it at hydration — the two are different
pools. The right default is ultimately the *provider's* to suggest (they cap
up/down differently); nothing crosses the seam for that yet.

**`-push-delay D` (config `push-delay`, fstab `push-delay=D`, default 300ms) is
*when* a change is pushed, as the two counts above are *how many at once*.** It is
the engine's per-path coalescing window (`syncengine.Config.Debounce`), and three
things about it are load-bearing. **The default is short because what it merges is
already coarse** — `OpWrite` is emitted once per *close*, not once per `write(2)`,
and since 2026-10-18 a *new* file's `OpCreate` is emitted at its handle's first
`Flush` (the close that `close(2)` waits for) instead of by `Create`, so a 50 GB
copy arrives as **one event** — not `Release`, which Linux sends in the
background after close returns and FreeBSD may hold until the vnode goes
inactive, which the FreeBSD fleet run caught as a file never announced — and
the window is merging an edit's few closes rather than batching a transfer; anyone reasoning about this as
if writes streamed will get every trade-off backwards. **A trailing timer re-armed
by every later change cannot bound how long a path is held**, so there is a
max-wait deadline measured from the change that made the path pending
(`maxWaitFactor`, ten times the delay) — without it a path touched inside every
window is never dispatched while the mount is up, and a long delay makes that easy
to reach, which is why the bound is not separable from the flag. And **an explicit
`0` is refused at all three boundaries**, because `New` would substitute the
default behind the user — M8 rule 6, the same reasoning as the pool sizes.

Two things it does not do, and the docs say so rather than implying otherwise. **A
file held open and never closed emits no event at any setting**, so it is invisible
to the push path until it is closed and is otherwise reached only by the M7b sweep
— that is push-on-close, not a tuning gap. And **a longer window makes §6 conflict
copies more likely, not less**, since it leaves more time for the remote to diverge
before the push lands: deferral buys quota and back-pressure headroom, never
conflict resolution. Do not let a later session "simplify" the echo/baseline
machinery on the opposite premise.

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

**A `mount` flag belongs to exactly one of four categories, and the category *is*
the rule about combining it with `-config`.** `mountShapingFlags` describe what to
mount and are refused beside a config file (a precedence rule nobody would
remember). `mountSelectorFlags` (`-name`, `-account`) *choose* among the mounts a
file already describes, so they compose with `-config` and **require** it.
`mountListingFlags` (`-list`, `-full`) read the file and mount nothing.
`mountProcessFlags` (`-debug`, `-pprof`, `-pprof-allow-remote`, `-control`,
`-control-socket`) describe how this *process* reports. `TestFlagCategoriesAreDisjoint` walks `mountFlagSet` and fails
on a flag in two categories or in none, because a flag with no category silently
inherits whatever the fall-through does.

**`-list` prints the config file's mounts and exits; `-full` widens the row.**
Three things are load-bearing. It goes through `config.List`, which is the **one**
resolution — `SpecsFor` and `Specs` are both that function with the config-level
half dropped — because a listing that resolved separately would become a second,
drifting account of what a config file means, and it is read precisely to find out
what the mount will do. It is **not** a validator: `app.Validate`'s cross-mount
guards are deliberately not run, since a listing is what someone reaches for when
a config file is misbehaving and one that refused to print until the file was
correct would be useless at the only moment it is wanted. And the cells that read
`-` are M8 rule 6 as a *display* — a log-only mount still carries `SweepInterval`
and `MaxDeletes`, which never apply, and printing `24h0m0s` for a mount that never
sweeps shows a setting as applied when it is not. `-full` without `-list`, and a
process flag beside `-list`, are both refused for the same reason.

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

M9 plugin architecture **shipped**: every backend runs in its own process,
launched over hashicorp/go-plugin (gRPC on a unix socket), one process per mount.
As M9 shipped it `drivel` linked no backend; **M23 has since bundled the shipped
ones into the host**, which changes how the child is started and nothing below it.
See "Plugin architecture" below for the invariants.

**v2, planned** (DESIGN.md §9 has the detail). **M10** platform parity — both xattr halves are written; **FreeBSD has been
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

M27 the sync path's two silences **shipped 2026-10-12, part 4 added
2026-10-16, part 5 2026-10-17**: back-pressure is reported, an abandoned push is
repaired rather than forgotten, a push *lost* to a crash is found by the next
sweep, and a failed download is retried by the pull loop. See
"The sync path's two silences" below for the invariants.

**M14** control & status API **shipped 2026-10-17**: `drivel mount -control`
opens a unix socket (`internal/control`, HTTP/JSON, `/v1/…`), and `drivel
status|pause|resume|stop` is the first client. See "Control & status API" below
for the invariants. The schema is `docs/dev/control-api.md` — **that file is the
contract, not the Go types**. Off by default; no tree roll-up (per-path status
plus a bounded, name-paged listing of a directory's children). The fstab helper
has it as `-o control`, one socket per line — see rule 6.

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

**M23** bundled backends & re-exec launch **shipped 2026-10-02** (except the
external-plugin authentication below, which M24 inherits): the host binary carries
its backends' *code* and launches one by re-executing itself as `drivel
plugin-serve <kind>`. **It is a supply-chain milestone rather than an isolation
one**: it retires the discovery surface (`DRIVEL_PLUGIN_PATH`, the five-directory
search path, shadowed kinds, and `safeToRun`'s check-then-exec race, which cannot be
closed while a plugin is named by a path) **for the kinds the host carries, which is
every backend that ships, and for no others** — the search path and the mode checks
still exist and still run for a kind the binary does not have, so don't write "M23
removed the search path". It changes nothing about what a plugin may do once
running — that lever is Landlock/seccomp and is a different piece of work. It
supersedes M9's "pinned digest per kind" note **for a bundled backend only**; an
external plugin is still a file reached by a path, so pinning stays live there and
merges with M24's signing question.

**What landed.** `cmd/drivel/bundle_on.go`'s `bundledBackends` (kind →
`provider.Factory`, `gdrive` + `sftp`), emptied by `bundle_off.go` under the
`nobundle` tag — that tag *is* the build option, `TAGS=nobundle` through the
Makefile. `drivel plugin-serve <kind>` is the child entry point, dispatched in
`main` beside `mount-helper`. In `plugin`, one `launchSpec` describes both paths and
`open` is the only thing that consumes it, so the exec is the single place they
differ; `Loader.Bundled` carries the precedence. **Precedence is bundled-wins per
kind and the losing file is named in the log** — a bundled kind is never looked for
on the path and never mode-checked, since refusing a file nobody will launch would
register a Factory that fails. `make build-nobundle` is a `check` gate and a CI step;
`internal/app/seam_test.go` is the import-graph test the milestone owed. Measured
**33.8 MB bundled against 76.4 MB as three files** — and **a bigger binary costs
less memory, not more**: host and child map one inode, so combined PSS is 33.8 MB
against 45.9 MB for host-plus-plugin as separate files, with 18.2 MB shared-clean
against none. DESIGN.md §9/M23 has the table and the two things not to misread off
it (residency is not execution; private-dirty, 5.3/8.4 MB, is what scales per
mount). What did *not* ship: the switch
that would disable the external path wholesale — precedence shipped, the opt-in
waits on authentication, because "allow external at all" and "allow *unsigned*
external" must not be met one at a time.

**Re-exec replaced an earlier memfd+zip design on 2026-10-01, and the record of
that design is kept in DESIGN.md as a working fallback** (`memfd_create` + all four
seals + `ExtraFiles` + `/proc/self/fd/3`; `archive/zip` reads a prefixed archive
natively). Re-exec won on five axes and four are not about size: **30.7 MB for one
link unit against 48.8 MB for host-plus-zip and 72.6 MB for three files** (measured,
because the runtime, gRPC, protobuf and `plugin`/`provider` link once instead of
three times); **one implementation on every platform** where the zip needed three,
which dissolves FreeBSD's unverified `shm_open(SHM_ANON)`+`fexecve` and macOS's
weaker extract-and-unlink path; no `vm.memfd_noexec` sysctl trap; **the build graph
no longer inverts** and a wrong-arch payload is impossible rather than needing to be
refused by name; and the embed-versus-zip question dissolves because there is no
payload. The pattern is M16's — argv0 dispatch, a hidden subcommand, a re-exec of
`/proc/self/exe` — applied a second time, not a new one.

Four things are decided. **gRPC is retained and behavioural equivalence is the
requirement**: a bundled backend reaches the host through the same socket,
handshake and generated protocol, so the launch mechanism is the only difference
between the two paths. go-plugin's `net/rpc` protocol would work unmodified and is
rejected — no streaming, while `Put`/`Get` are streams and a 50 GB copy arrives as
one `OpCreate` plus one `OpWrite` — and `net/rpc` is frozen. **The bundled binary is
the default build**, so a fresh checkout is usable the moment it compiles; the
separate `cmd/drivel-provider-*` targets stay, because they are how an out-of-tree
backend is built and because `plugin/testdata/drivel-provider-fake` is a real
on-disk executable, so the suite keeps exercising both launch paths rather than
letting the external one rot — which is also why the bundled path has its own tests
(`plugin/bundled_test.go` for the host half, `cmd/drivel/bundle_on_test.go` reading
go-plugin's handshake out of the built binary for the child half). **§2.9's
cross-compile proof survives** — it is a claim about the tree, not a link set, and
both providers plus `plugin` build clean for `windows/amd64` where only go-fuse
fails — but the *inference* that nothing above the seam imports a provider stopped
being readable off the host binary, which is what `internal/app/seam_test.go` now
asserts directly. **`govulncheck` reporting shifts** from `drivel-provider-gdrive`
to `drivel` for a Drive SDK advisory; same risk, accepted.

Two consequences are easy to get wrong. **M9 rule 4 was reworded, not violated** —
there is no filename to be the kind, so the kind is the argument the *host* passed,
which is still not anything the plugin says about itself; one `plugin-serve <kind>`
subcommand rather than one per kind. And **the hidden subcommand stays out of
`usage()`, `help` and the completions**, now by a test (`cmd/drivel/bundle_test.go`)
rather than by habit, the way `completionApp` already refuses a flag with no hint
entry; direct invocation is inert anyway because `MagicCookieKey` is
`DRIVEL_PLUGIN`.

**An external plugin proves what produced it with a pinned Ed25519 signature —
not OpenPGP, not a CA** (decided 2026-10-01; M24 inherits it). The signature covers
a `(kind, version, sha256)` manifest whose digest is the number `SecureConfig`
already checks, so it adds provenance to an existing integrity check. What decides
the format is that verification happens **offline at mount startup**, which removes
OCSP, CRL fetching and shelling out to `gpg(1)` before any comparison: OpenPGP fails
on the *verifying* side (`x/crypto/openpgp` deprecated and frozen since 2021, the
maintained route a third-party fork) and a CA costs an operated PKI for delegation
nobody needs yet, with **certificate expiry as an active hazard** — a lapsed cert
breaks every mount using that plugin at a moment nobody chose. `crypto/ed25519` is
stdlib; pin a **list** so rotation needs no CA. Precedent is the Go toolchain
pinning `sum.golang.org`'s key.

**Whose key is the harder half: two paths, never one.** Signing someone else's
binary with drivel's key is the trap (we either build it, needing reproducible
builds, or we attest to provenance we never verified). So **drivel signs only what
drivel builds**, and a third-party plugin is checked against *its author's* key,
pinned by the user at install. **That TOFU happens at install and never at mount** —
the SFTP host-key rule again (M18: a boot mount has nobody to answer a prompt, so it
refuses). A signature proves provenance only: not that code is good, and **not that
it outranks what is bundled**. **Overrides are three switches** — allow external at
all; allow *unsigned* external; name a custom location — because folding the third
into the second means someone who wanted a custom directory silently accepted
unsigned code, and a boot mount must reach neither of the last two by default.

**Unsettled, in order.** Whether a *newer* authenticated external plugin should
outrank the bundled one automatically: the recommendation is **no** — it
reintroduces the invisible precedence this milestone removes, cannot read a version
without launching the thing it is deciding about, turns an install into a mount
outage (a bundled backend is protocol-matched by construction and an external one is
not, so a newer plugin speaking the next `ProtocolVersion` is refused at handshake),
and makes a signed artifact with a high version number a permanent override. The
goal behind it is right, and the alternative is that an external plugin wins because
a **record** says so (kind, version, digest, key, written at install), making "a
newer one exists" something `drivel plugin list` reports rather than something that
happens silently. Then **replace vs precede** (it decides whether out-of-tree
backends, the reason `provider` is public, remain possible, and whether M24 has an
install target at all). Then whether `Loader`'s `refused`/`shadowed` machinery
survives as the external path or is deleted with the search path it serves.

**M24** plugin registry — a named, versioned, verifiable way to install a
`drivel-provider-*` executable, modelled on `registry.terraform.io`. Unscheduled,
not started. **Protocol and client first, service possibly never**: Terraform's
registry protocol is implementable by a static file tree over plain HTTPS, which is
what makes private and air-gapped ones work, so `drivel plugin install acme/s3` is
testable against `testdata` long before anyone operates a server — and operating one
is a supply-chain target with an abuse policy attached, not a coding task. It serves
metadata and URLs, never bytes. **The catalogue and the install protocol are two
systems and only one is on the install path** — Terraform's `/browse/providers` and
`search.opentofu.org` are search UIs over a database and a scraper (OpenTofu's is a
Cloudflare Worker over Postgres), and `terraform init` never touches them; the
install protocol beside them is two unauthenticated GETs returning cacheable JSON,
which OpenTofu serves by pre-generating every possible response into
an R2 bucket with **no API server at all**. Build that half — the catalogue is
optional and is where the operating cost lives. **Four divergences from Terraform's
version are decided**: no `.well-known` indirection, a base URL from config as
Terraform's own *network mirror* protocol does, because drivel's config names a bare
`kind` and never a hostname; every path ends in `.json`, so `python3 -m http.server`
over `testdata` is a conforming registry; **the digest is of the executable**, not of
an archive, because that is the number `SecureConfig` checks before exec and two
different numbers make the pinning decoration; and `protocol` is one number used as a
*filter* — selection takes the newest version whose protocol this host speaks, or
`install` succeeds and the handshake fails at boot. Four more things decide the rest
and are in DESIGN.md §9/M24.
**M9 made the filename the kind**, so a namespaced registry name (`acme/s3`) and a
bare local kind (`s3`) need an explicit mapping plus a collision *refused at install
time naming both* — never a resolution rule, because first-on-the-path-wins is
tolerable today only because nobody can install two by accident. **There is no
version solver** — one provider per mount, no dependencies between plugins, so an
exact version or the newest, and record what you got. **Checksum pinning stops being
optional** (go-plugin's `SecureConfig`) — M23's "moot" note covers bundled backends
only, and an installed plugin is the external case it explicitly does not cover — and
the trust anchor must *not* be the registry itself — that is the one part of Terraform's design
not to copy, and it is circular for everyone but HashiCorp, whose key is pinned in
the binary. What works over there is `.terraform.lock.hcl`; drivel's version is an
**install manifest beside the binary** recording name, version, source registry and
the executable's `sha256` — trust-on-first-use with a durable record, no cryptography
beyond a hash, shippable before any signature scheme exists, and the same record a
collision refusal needs in order to name the incumbent — and since M23 decided the
signature scheme, the manifest records the author's **key** too, which is what turns
"the same bytes as last time" into "the same author as last time". The registry
carries the signature and **never** the key: the key is pinned at install, because a
registry that served both would be the circular trust anchor this entry already
rejects. And **it is a consumer of M23's
external-plugin flag**: an installed plugin lives in the very search path M23
proposes to retire, so if bundled-only ever wins, M24 has no install target and is
withdrawn rather than reconciled. **M23 also shrinks it**: a bundled default build
answers "installing a backend needs a Go toolchain" for every backend drivel ships,
so what is left for the registry is third-party backends — which moves the
authentication question to the front, since M23's external path and M24's install
path need one answer between them. **The
licence question is answered and was dissolved rather than resolved**: under MPL-2.0
a plugin may carry any licence whether it imports `provider` or speaks only protobuf,
because the copyleft reaches drivel's own files and no further. The registry records a
declared licence per plugin as metadata, not as a gate.

**M25** logging — levels + structured output, destinations + rotation, and what a
log line may name. Unscheduled, not started. **A durable record of what synced is
M14, not this**; a log is read once by a human, sync history is state a tool
queries. Four things are decided and in DESIGN.md §9/M25. **drivel has no
verbosity control of its own** — `-debug` is go-fuse's `MountOptions.Debug`, a
FUSE protocol trace from another program, and it must never come to mean two
things. **The per-mount `logf` methods are the interface**: ~124 call sites already
go through `e.logf`/`d.logf`/`n.logf`/`m.logf` rather than `log.Printf`, so levels,
a handler and a redaction filter land behind them without touching callers — and
per mount stays, because one stream carrying two accounts' filenames is M8's
cross-mount coupling arriving as an output format. **`log/slog` is not the
dependency `docs/dev/dependencies.md` declined** (that row rejects a *third-party*
logger, and the judgement stands); rewrite the row rather than overrule it.
And **redaction has a boundary it cannot enforce**: paths and filenames are logged
verbatim today, a §6 conflict copy's *name* contains the original path, and a
plugin's stderr is forwarded onto the mount's logger verbatim (`plugin.lineWriter`
→ `process.logf`) — so either say so where the filter is documented or prefix that
line and never filter it. Rotation is the one with no default at all: M16 rule 3
already sends the fstab daemon's stdio to `logfile=` or `/dev/null`, so the mount
that runs longest writes a file nothing rotates; reopen-on-`SIGHUP` first, because
it forecloses neither internal rotation nor a syslog/journald destination.

**M26** credentials in a secret service — an opt-in, per-account alternative to an
OAuth refresh token in a `0600` file: `login` writes it to the desktop secret
service, a mount reads it back. Unscheduled, not started, and **deferred
2026-10-06** pending the master-passphrase question below. **It is secret-at-rest
and not isolation**, and that has to be said in the docs: it keeps the token out
of a dotfile backup, a synced home directory and a pasted config, and it does
*nothing* against another process running as the same user, which can ask the
service exactly as it could have read the file. **There is no per-application
access control and the protocol has no room for one** — no caller identity in any
signature, gnome-keyring had ACLs keyed on `/proc` exe paths and dropped them as
unenforceable between processes of one uid, KWallet's app name is caller-supplied,
and Flatpak's per-app secrets are enforced by the *sandbox* and not the keyring
(M9 rule 8's built-environment-is-not-a-sandbox, in a second place). Seven things
are decided and in DESIGN.md §9/M26. **KWallet 5+ and gnome-keyring are one
backend** (both implement the freedesktop Secret Service D-Bus interface), so
there is no desktop detection to get wrong. **The token is already read-only at
mount**, which is what makes this small: `gauth.SaveToken` has one caller
(`login`), the provider only `LoadToken`s and lets `oauth2` refresh in memory
without writing back, so the feature needs read at mount and write at login — no
write-back across the seam, no protocol change. **The host does the lookup and
`plugin/env.go` must not grow an entry**: D-Bus is deliberately unreachable below
the seam (every `XDG_*` dropped, no `DBUS_SESSION_BUS_ADDRESS`), and admitting it
would be strictly worse than the `SSH_AUTH_SOCK` exception it would cite — an
agent signs with ssh keys, the secret service hands over *everything* the user has
stored, to every backend including an out-of-tree one. **A boot mount cannot reach
it and must refuse, naming the account** — no session bus, no unlocked keyring,
nobody to answer a prompt (M18 rule 6 again) — and **falling back to the file is
the trap, not the kindness**, because then the file still has to exist and the
feature bought nothing while appearing to work. **GPG is declined**: pinentry must
prompt, a daemon has no tty and may have no display, and a passphrase-less key is
a file again. **`godbus/dbus/v5` over `zalando/go-keyring`** — the latter shells
out to `secret-tool`, so the secret transits a child process and an external
binary becomes a requirement; macOS Keychain is deferred because it needs cgo or
`security(1)` and §2.9's pure-Go build is worth more. And **it is the third
consumer of `provider.Params`** — the host cannot know which provider settings are
secret-valued (M8 rule 4 cuts both ways), which is the same declaration M12 needs
for its inner store and the M17–M21 group needs for local-path validation; build
it once.

Two things came out of the 2026-10-06 discussion and are why it is deferred rather
than scheduled. **systemd credentials are the complement for the boot case the
keyring must refuse**, and the only mechanism in play whose granularity is
actually enforced — `LoadCredentialEncrypted=` puts a TPM- or host-sealed secret
in `$CREDENTIALS_DIRECTORY` scoped to one unit, and `gauth.LoadToken` already
takes a path, so the read side is nearly free; it cannot be expressed in an fstab
`-o` line and its interaction with M16's `run-as=` drop is unchecked. And **a
master passphrase in rclone's shape may replace the whole milestone**: it is
backend-neutral (so it answers "only Drive wants a keyring" instead of inheriting
it), needs no session service, and its password-command *is* M18 rule 6's
delegation — which makes the keyring and systemd creds **sources** for it rather
than rivals. The cost is M8 rule 5, so the version that fits drivel encrypts
**secret values, not the config file**, which stays plaintext and commented. The
older first question — whether any of this belongs above the seam, since **SFTP
already solved it differently and correctly** (M18 rule 6: no password, no
passphrase, agent instead) — is downstream of that one now.

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
(`docs/dev/multiclient-test-plan.md`), whose Tier A covers Groups I–IV plus **Group
V, failure and degradation** (`internal/app/failure_test.go` over the per-peer fault
injection in `faultstore_test.go`): MC-41, MC-43, MC-44b and MC-46 run in-process,
MC-45's premise turned out not to hold, **MC-47 ran for real on 2026-10-10 over two
processes and a local sshd** (see M8 rule 2), **MC-40 ran on 2026-10-13 over that
same rig** and produced three product fixes — a plugin child that outlived its
host (M9 rule 10), an SFTP temp name two writers shared (SFTP rule 7), and an
upload byte count taken before the transfer — plus the finding that **a crash was
not an outage**: M27 repaired an abandoned push and not a lost one, which is what
M27 part 4 then closed (2026-10-16, Tier A only — the live re-run is owed).
**MC-44 ran the same day and completed**: its
transport half found that a transient UDP block downgraded the transport
permanently and fixed it, and its throughput half then measured the two protocols
against live Drive (both in "Transport" below). Six of the group's findings are products of
running it and not of the plan: an **abandoned push and a failed download were
both repaired only by a sweep**, which `-sweep-interval 0` disables — the push
half is M27, and so since 2026-10-17 is the download half (part 5, no sweep); the
shutdown gap in M8 rule 1 above; **an eager SFTP mount without
`-materialize` never receives anything** (SFTP rule 2); the orphaned plugin
child (M9 rule 10); **one `cp` of a large file costing two full uploads**,
because `OpCreate` and `OpWrite` can only coalesce for a write that finishes
inside the debounce window — which is also the answer to the duplicate upload
MC-11 recorded and the plan had sent to Tier A to be chased, and is **fixed
2026-10-18** by emitting a new file's one event at close (`vfs` `fileHandle.created`,
`TestANewFileIsOneEventAtClose`); a new file held open is now invisible until
closed, which is push-on-close applied to creates too — **and to the sweep**: the
walk's "new locally" row would otherwise find the open, unannounced file and push
it part-written (the live SFTP run did exactly that at a 2s poll), so the mount
publishes its open new files as `fsevent.Writing` and the walk skips them; and the permanent
HTTP/3 downgrade in "Transport" below.

**Groups II, III, IV and V are finished and Group VI is partly done** (ran
2026-10-13/15; §5 of the test plan is the authority and every row carries its own
result). What is left: **MC-52 and MC-53, both declined for wall clock with the
arithmetic stated** — 100 000 files is ~39 hours of pushing at the 0.71 uploads/s
MC-50 measured, and a 24-hour soak cannot be compressed without destroying what it
measures — **MC-10's 5 GiB half and MC-10b**, declined together (~19 min of
uploading plus ~19 min per puller, 20 GiB of local disk, for an assertion the
1 GiB run already answers). **Everything else in Group VI is now run**: MC-51's
three shapes at N = 1, 2, 3 and 5 finished 2026-10-16, holding the traffic law
exactly at a gigabyte — 1 upload + (N−1) downloads of the whole file, 1.00
transfers per client, up to **5120 MiB for a 4 KiB edit**, with no 403 and no
abandoned push anywhere — and turning up the delete-resurrection bug recorded
under `-drive-delete` above. **MC-60's three were finished the same day** by
`scripts/multiclient/mc60-mixed.sh` and found nothing wrong: 151 feed deletes
went through a `-max-deletes` of 100 with zero refusals (the cap bounds
reconcile inference, not the feed), a file rename lands on a lazy peer as a
placeholder at the new path, and the MC-26b directory shape reproduces there too
and is repaired by a forced sweep. What they *did* change is the cost model —
see "Lazy hydration" below.

**Groups II and III (2026-10-14/15) found the two worst defects this exercise has
produced, and both were latent in shipped code**: a folder-scoped sweep that
truncated to two `Enumerate` calls and then ran its delete pass on the prefix
(see "Enumeration & reconcile" rule 6), and M27's back-pressure report flapping
235 times in one burst (see "The sync path's two silences"). The lesson that
outlives both: **MC-01 is the only live cold sweep this plan records, and it ran
two days before M7c existed** — so a feature whose only live evidence predates it
has no live evidence, and any Group I–III result recorded before 2026-09-07 should
be read with that date in mind. The third finding is MC-27's: **renaming a
directory of more than ~100 files doubled that subtree on every other client and
did not heal**, because a folder move was left to the sweep and the sweep's repair
then exceeded `-max-deletes` and refused the whole pass — the only case on record
where two correct guards compose into a stuck state. **Fixed 2026-10-16** by the
seam change in "Directory moves" below; the live re-run is owed. **Three findings from those groups outrank the rest and are below in their
own sections**: §6's conflict copy protects *unsynced local work* and not content
(MC-31, MC-60), inbound sync stalls while a client drains an outbound backlog
(MC-36, MC-50), and §12's ~36 KB per commit is now confirmed in the field. One
thing about the remaining order is worth keeping: Group VI's constraint
is wall clock and storage quota, **not** API rate — MC-11 recorded zero 403s at
1.09 uploads/s, a single writer cannot burst past ~1 request/s, and storage is
per Google *account*, so a fresh Cloud project buys per-project request
attribution for MC-50/MC-51 and nothing else.

Five rules the last rounds left behind. **A commit budget is a disk-cost
regression gate, and the mistakes it catches are invisible otherwise**: the two
redundant `pathindex.Set` calls per re-push stored exactly the right value, so no
correctness test could ever have failed on them — see "Store durability & write
amplification". **A property test nobody has seen fail is a
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
fails. And the last, from Group V: **a predicate that answers "has this ever
happened?" cannot be used to wait for it happening again.** `testLog.saw` searches a
peer's whole log, which accumulates across that peer's restarts — so every case that
restarts a peer and then waits for a line its previous life already printed gets a
wait that returns on the first poll and assertions that race the new process.
`mark`/`sawSince` is the fix; it was written twice before being noticed, once as a
test that failed for the wrong reason and once as a test that passed for one.

DESIGN.md §10 is a design note on carrying POSIX metadata (mode, ACLs, xattrs,
SELinux) over Drive — unscheduled. If you touch it, the rule is that permission
metadata from a remote source is *executable trust*: authenticate it, bind it to
the file, fail closed, and mask setuid/setgid by default.

## Control & status API (M14)

The first compatibility surface with code we do not control. Seven things carry
it; DESIGN.md §9/M14 has the reasoning and the shipped note.

1. **A view, never an authority.** `syncengine.Monitor` is written by the engine,
   the pull loop and the sweep, and read only by `internal/control`. No sync
   decision may read it; if one wants a value that lives there, the value belongs
   in the deciding component and the monitor gets a copy.
2. **A status read never touches the run loop and never calls a provider.** The
   monitor's lock is never held across a send, a provider call or I/O; readers
   take it once per batch; `StatusReader` reads the monitor, `state.Lookup` (one
   read transaction, **zero commits** — §12), an `lstat`, the parent directory for
   conflict copies, and the placeholder xattr. Never a hash.
   `TestStatusIsServedWhileTheRunLoopIsBlocked` is the assertion.
3. **`unknown` is first-class and `synced` is narrow**: a baseline whose M18
   fingerprint still matches. No record, no fingerprint, or a diverged file with
   nothing queued are all `unknown`, with a `detail` saying which. `local-only` is
   the one added value (conflict copies, special files, `.drivel-` temporaries).
   A directory's status is about the directory; there is no roll-up and adding
   one is a new index with its own drift bug — it was decided against.
4. **Pause stops dispatch, never consumption.** While paused the run loop still
   reads and coalesces; content stays in the debounce map (no timers armed),
   structural ops go to an ordered held list, and workers wait at a gate **the
   run loop shuts after it has stopped sending** — the only reason that wait
   cannot back up into `vfs.node.emit` and hang FUSE. A pause request is also
   answered from inside a blocked dispatch. The pull loop, the sweep (between
   pages) and `Engine.Push` wait at the same gate; **hydration does not** (a read
   is a user on the FUSE path), and an in-flight upload finishes. Ends on expiry
   (`for`, default 1h, at most 24h, refused out of range), on overflow
   (`DefaultPauseCap` 10000, loudly), or at shutdown (resume first, then drain).
   Never persisted. **Discarding the held set is not an option**, whatever the
   memory pressure argument.
5. **Shutdown cancels the context the signals cancel** (`context.WithCancelCause`
   under `signal.NotifyContext` in `runMount`), so the three-phase `Close` and
   the bounded drain run unchanged; it answers `202` before draining and logs the
   peer. `TestAnAPIShutdownDrainsLikeASignal` runs both paths over a real mount.
6. **The socket's directory is the authentication.** `$XDG_RUNTIME_DIR/drivel/`
   or `-control-socket`, **no `/tmp` fallback**, an existing directory that is
   not ours and `0700` is refused (never chmod'ed), socket `0600`, peer uid
   checked where the platform reports it (root allowed). A stale socket is
   *connected to first* and unlinked only on `ECONNREFUSED`. `Authorization` is
   accepted and ignored so a token scheme can grow later. **An fstab line gets a
   socket per line, named after the mount**, because each line is its own
   process: `/run/drivel/<uid>/<name>.sock` when the helper *started* as root
   (root makes the directory and chowns it to the `run-as=` account before the
   drop — boot has no `XDG_RUNTIME_DIR`, and `/run/user/<uid>` vanishes at
   logout), else `$XDG_RUNTIME_DIR/drivel/<name>.sock`. Only the parent knows
   which, so it passes the path to the child in `DRIVEL_MOUNT_HELPER_CONTROL`.
7. **The client imports nothing internal.** `cmd/drivel/ctl.go` decodes into its
   own types from the documented schema — it is the acceptance test that a
   stranger's tool can do everything the in-tree one does. If it needs something
   the API does not offer, change the API, not the client.

Reachability is **observed at call sites** (engine attempts, `Changes`,
`StartCursor`, `Enumerate`, the downloader's `Get`, and hydration's `Get` and
body read through `Hydrator.ReportOutcomes` — never its local write) and never
probed; a non-retryable error counts as contact, an hour of silence is
`unknown`. Do not wrap the store to observe it — a wrapper must re-declare every
capability, which is M9 rule 1's trap. **Not merged with the plugin protocol**,
whose trust runs the other way. Profiling over the socket serves the snapshot
profiles only; `profile`, `trace` and `cmdline` are refused there.

## Lazy hydration (M5)

Three invariants; breaking any of them loses user data.

1. **The xattr is authoritative and the DB is not a fallback.** The marker
   (`hydrate.XattrName` — per platform since M10, never the literal) lives on the
   backing file; the `RangeSet` in `internal/state` caches present *ranges* and
   nothing else. `IsPlaceholder` reads the marker and only the marker, so **no
   marker means no placeholder record at all**, immediately — not "after the DB is
   lost", which is what these notes wrongly said until M10. The practical
   consequence is invariant 2's: on a backing filesystem with no user xattrs
   (exFAT/FAT, a tmpfs `/tmp` on FreeBSD) `-lazy` is unsafe and `app.Open` refuses it (a warning until 2026-10-17);
   the mitigation is eager mode or a different `-data`, never "keep the state
   DB". **The test is the filesystem, not the platform** — see "Platform support"
   for why WSL2's drvfs was wrongly on this list.
2. **Never push a placeholder.** `syncengine.Placeholders` gates every content
   upload. A placeholder holds zero bytes at full apparent size, so uploading it
   replaces the remote file with nothing. The guard fails **safe**: unsure ⇒ report
   "placeholder" ⇒ skip. Do *not* replace it with a size heuristic — a legitimate
   truncate-to-zero looks identical and must still sync.
3. **Hydrate on first I/O, not on `Open`.** `fileHandle.ensureResident` does it, so
   open/truncate/rewrite fetches nothing. FUSE delivers `O_TRUNC` as a separate
   `Setattr` unless `atomic_o_trunc` is negotiated, so `Open` handles only the
   atomic case (dropping the mark), and `Setattr` handles truncation.

**Hydration is not durable, and that decides what lazy mode is for** (MC-60,
2026-10-16). A hydrated file is an ordinary file with no marker, so the next
inbound change for that path takes the eager branch: `statMD5` hashes the whole
local copy, finds it equal to its baseline, and `materialize` replaces it with a
fresh placeholder — `[pull] placeholder`, not `[pull] restamp`, which is how to
tell the two apart in a log. Measured on a 1 GiB file: the lazy peer held 4096 B
against the eager peer's gigabyte, read it in 52s on demand, and the next 4 KiB
edit threw that copy away. So **lazy mode trades bandwidth for disk and latency,
and only for files nobody opens** — a client that reads a file after every change
pays exactly what an eager one pays, on the read's critical path, plus a full
local hash per change. Nothing here is a bug: the discarded copy was synced, so
there was nothing for §6 to protect. Don't "fix" it by keeping hydrated content,
which would make every lazy client an eager one with extra steps.

Also: a failed hydration must return `EIO`, never a short read (zeros served as
content are silent corruption); and §6 conflict copies are always downloaded in
full, because their paths exist only locally and could never be hydrated later.
**Both halves of that last sentence were verified live by MC-60** (2026-10-14):
on a lazy client the conflict copy holding a remote 1 MiB came down complete —
on-disk equal to apparent size, no marker — while everything else on that mount
was a placeholder.

**§6's conflict copy protects unsynced local work, not content, and the
difference is where the fleet loses data** (MC-31 and MC-60, 2026-10-14). A
client that has already pushed has a local file matching its own echo, so there
is nothing local for §6 to protect and an incoming remote change applies
cleanly — which is correct, and which means two clients editing one file
concurrently ended with the later *pusher*'s bytes and **no conflict copy
anywhere**. The loss happened on the winner's whole-file `Put`, which replaced a
remote that moved since its own baseline. On Drive the loser survives in the
revision history, which drivel neither creates nor exposes; on SFTP and every
M17–M21 backend it was simply gone. Do not write a test or a doc that asserts
"no content is lost" without saying which interleaving it means — three
expectations in the multiclient plan said it and all three were wrong.

**Fixed 2026-10-17 on the push side, narrowed rather than closed**
(`syncengine/diverged.go`, DESIGN.md §6). Before a whole-file `Put` of a path
with a baseline, the engine checks that the remote still matches the echo, and
on a mismatch downloads it into a §6 copy before uploading — named for the
remote's mtime, so the pull's "local newer" copy of the same version is reused,
not duplicated. Three things are load-bearing. **The pusher still wins the
path**; nothing about who wins changed, only whether the loser survives. **A
copy that cannot be fetched fails the push** (the engine's retry decides), never
falls through to an overwrite. And **a local copy equal to its baseline is not
pushed at all** over a newer remote — a touch used to push stale bytes over
someone's edit — which needs the provider's digest, so hashless backends keep a
copy instead. What remains: two pushes whose `Stat`s both precede either `Put`
still lose one edit (no shipped backend has a conditional write; SFTP's
`Create` closes it for two *first* pushes only). It costs one `Stat` per
small-file rewrite, which `TestSmallFileChecksTheRemoteOnlyWhenItHasABaseline`
pins. **A path with no baseline is guarded at every size since 2026-10-18**,
which was the cost decision this paragraph used to leave open: the first push
goes through `provider.Creator`, whose refusal *is* the check, so on Drive (the
`Put` already resolves the path) and SFTP it costs nothing, and a store without
`Creator` pays one `Stat`. `conflicts = "overwrite"` skips it. MC-31a
(`TestFleetConcurrentEditsKeepTheOverwrittenEdit`) asserts the copy, ordered
with a pause because a race would decide nothing.

**A feed deletion deletes only what matches its baseline** (fixed 2026-10-18;
`Downloader.applyRemoval`). It used to be one `os.RemoveAll`, which took an
unpushed edit with a remotely deleted file and every never-synced file with a
remotely deleted directory — the sweep's `deleteGone` had the guard all along and
the feed did not. Now a file is removed only when `matchesBaseline` says it is
unchanged (or it is a placeholder). Anything else is kept, its baseline dropped
inline, and it is re-pushed unless it is still open (`fsevent.Writing`) or in a
private conflicts folder. A directory goes child by child, deepest first, and
survives if anything was kept. Edit beats delete, as in the sweep.
`removal_test.go`, 5 mutation checks.

## Conflict policy (where the loser goes)

**Who wins a conflict is fixed; where the loser goes is the user's** (2026-10-18,
DESIGN.md §6). `syncengine.Conflicts` is one value per mount, handed to the
engine, the pull loop and the status reader, and every resolution — the pull's
two branches, the push's divergence check, a refused first `Create` — goes
through `keepLocal` or `keepRemote` on it. `conflicts` = `dir` (default) |
`local-trash` | `remote-trash` | `overwrite`. `conflicts-dir` = `both`
(default) | `local` | `remote`, for `dir` only, and refused beside anything
else (M8 rule 6). `syncengine.ResolveConflicts` is the one parser all three
boundaries call. Five things carry it.

1. **`.drivel-conflicts` is a reserved component, and `Conflicts.Syncs` is the
   only answer to "does this mount sync this path?".** The push, the pull, the
   sweep's remote pass, its delete pass and its walk all ask it. One that
   disagreed would upload a copy and then infer it deleted, or the reverse. A
   rename across the edge of a private folder is the one event with meaning: out
   is a restore (push the file), in is a removal (remove it remotely).
2. **A loser that cannot be kept is never replaced.** keepLocal/keepRemote
   return an error and the caller fails the apply or the push, so the retry
   machinery decides. Every keep is a **local step then a remote one**: the
   loser lands in the local folder (or the home trash) first, and only that
   step can fail the caller. The remote half — upload for `both`/`remote`,
   upload-then-`Trash` for `remote-trash` — is logged and leaves the copy in
   the local folder if it fails, because it is preserved; with `both` the walk
   uploads it later. **On the pull side the remote half runs off the pull
   loop** (`conflictEnv.later` → `Downloader.sendConflictCopy`, at most
   `conflictSenders` at once, after the pause gate, waited for by `Run`),
   because inline it held up every inbound change behind a large loser and
   ignored a pause. The engine runs it inline: its workers are already off the
   event path and already gated.
3. **Default `both` reverses "conflict copies are local-only".** That rule
   existed so the walk would not publish sibling copies by accident; those
   (made before 2026-10-18) are still recognised by `conflictRe` and still
   skipped. A folder copy is uploaded on purpose with its baseline recorded,
   so the feed's echo of it is recognised and its later edits sync as content.
4. **One conflict is met twice**: the pull ("local newer") and then the push of
   that edit find the same remote version. `Conflicts.kept` (path → version, in
   memory) stops the second copy. The local folder is also checked by name. A
   restart costs at most one duplicate in a trash or on the remote.
5. **`remote-trash` uploads the loser and trashes it; it does not move the
   remote object.** Moving the original away would show every other client a
   deletion followed by a create at the path. The cost is a download and an
   upload of the loser. Drive's `files.copy` would avoid that and is not
   plumbed. `local-trash` writes the freedesktop.org home trash
   (`internal/trash`, `.trashinfo` naming the *mountpoint* path so Restore
   writes through the mount) and is refused when the trash would be inside the
   backing tree or mountpoint.

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

   **A skip here records the baseline it just verified** (M27 part 4), at one
   commit — a digest match against the remote's current state is stronger
   evidence than the echo a successful `Put` writes, and dropping it left the
   sweep's lost-push row proposing the same push on every mount for any file
   whose mtime moved without its content. Do not "optimise" that back out.

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

In `ranges`, present-ranges round **inward** (`Mark`) and dirty-ranges
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

Also: **a rename must be reported inbound, and the shape depends on what moved.**
Drive's feed never mentions a path an object has left, so `vacatedPathLocked`
derives it from the in-memory reverse map — **not** the persistent index, since
acting on a hint here means moving or deleting a local file. A **file** is
reported as remove-old + add-new; a **directory** sets
`provider.RemoteChange.MovedFrom`, which is the fourth field and the one M9's
protocol gained for it. Without either, every rename duplicates on every other
client until a sweep. See DESIGN.md §2.5 and "Directory moves" below.

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
the path reappearing with an older sibling's bytes — everywhere. `Enumerate`
reports the path once per sibling, and until 2026-10-17 `reconcileRemote`
applied them in listing order, so after a sweep the local file (and its
*baseline*) could describe a different sibling than the one the path resolves
to; `applyPage` now runs `standsForPath`, which applies only the most recently
modified — `lookupChildLocked`'s own tie-break — and logs the shared path once
per sweep with a summary count. That is detection plus agreeing with the
resolver, not a mitigation. No mitigation is implemented on purpose: each candidate
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
6. **A continuation cursor must advance, and a sweep that ends early must infer
   nothing.** `runSweep` reads its own cursor handed back as a provider that
   cannot advance and ends the sweep — so `provider.Enumerator` now *states* that
   a non-empty `next` differs from the cursor it was given, and `gdrive`'s scoped
   descent returns `scoped:N` rather than the bare constant it returned from M7c
   until MC-12 ran it live (2026-10-14). It had been truncating every scoped
   sweep to two calls — the mount root plus one `enumFanout` batch, 477 of 1157
   objects on the rig — because the two halves had never run together: the
   provider's own tests feed the cursor back without the loop-breaker, and the
   only live cold sweep on record predates scoped mode by two days. The *other*
   half is the one that cost data: that branch fell through to `pushLocalOnly`,
   `applyDeletes` and `FinishSweep` over a listing that was a **prefix** of the
   tree, making every unlisted path a delete candidate under `-max-deletes` alone.
   A stalled sweep now keeps its pages, runs neither completeness-dependent pass,
   writes no completion stamp, leaves the in-progress record for the next mount,
   and logs `reconcile INCOMPLETE`. Anything added to this loop later inherits
   that rule: **a partial listing may materialise and may not conclude.**

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

**Since M27 part 4 the same walk carries a second row, and the two are not
interchangeable**: "new locally" is a path with no baseline, `repushDiverged` is a
path with one whose *local* copy no longer matches it — a push lost to a crash
rather than abandoned by the engine. It is in this walk because the walk is
already stat-ing every file; what it needs beyond that is the `openedAt` boundary,
the refusal to act on an unfingerprinted baseline, and the `noRepush` set. See
"The sync path's two silences" item 4 for all four, and do not relax any of them
to make the row catch more.

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

## Store durability & write amplification

Both bbolt stores (`internal/state`, engine-level and in the host; `internal/pathindex`,
provider-private and in the backend process since M9) keep bbolt's **default
durability** — two fdatasyncs per write transaction, nothing buffered. `DESIGN.md
§12` is the decision record; `docs/dev/conventions.md` has the short rules. Four
things carry it.

1. **A commit is the unit of disk cost, and it is a constant.** ~36 KB of disk
   writes per commit on a 10k-entry DB however small the record, **flat as the DB
   grows** (36.0 KB at 20k keys, 36.3 at 40k). So cost is *commits × a constant*,
   and **the fsync is the cost, not the data**: `NoSync` plus a `Sync()` after
   every commit reproduces the default total byte for byte, while one `Sync()` at
   the end leaves 1.5% of the bytes. There is no tuning answer — page size is
   already the 4 KiB minimum, the hashmap freelist is already chosen, and
   `NoFreelistSync` is worth 11%. Don't spend a day on those.
2. **Count commits, never bytes.** `state.Store.Commits` / `pathindex.Store.Commits`
   report them; `syncengine.TestPushCommitsPerFile` pins one commit per pushed
   file and the gdrive/pathindex tests pin the rest. A byte count depends on the
   filesystem underneath and is not assertable. These budgets are a ratchet:
   lowering one is an improvement, raising one needs a reason in the message.
3. **Compare before writing, in a *read* transaction — never an early return
   inside the write one.** bbolt advances the meta page on every commit whether a
   data page changed or not, so an `Update` storing a byte-identical value costs
   the full ~36 KB and an `Update` that touches nothing still costs ~20 KB; a
   `View` costs zero. That is why `pathindex.Set`/`SetMany` check first. **A
   half-match is not a match** — `setLocked` evicts the stale halves a rewrite
   leaves behind, so both directions must already agree or the buckets stop being
   inverses, which is what resolves an ID to a path it no longer occupies.
   **What this does and does not buy**: a re-push went 3 commits → 1 and a cold
   resolve off a warm index 2 → 0, but **a genuinely new path still costs its one
   commit, so a create-only burst gains nothing** — which is precisely MC-11's
   workload, so don't cite it as the fix for that measurement.
4. **An unreadable DB is replaced, not fatal — both of them** — `.corrupt` suffix,
   most recent kept, `Store.Recovered()` reported so the caller logs it: loudly
   from `app.Mount` for state, briefly from `gdrive` for the index (where the old
   fallback, run-from-memory, was correct but permanent and nearly silent).
   Safe for a *specific* reason and not as a convenience: M7b infers a deletion
   only from a baseline, so no echoes ⇒ every path reads as new ⇒ **nothing is
   deleted**; M5's marker is an xattr on the backing file, so no placeholder
   becomes an empty file; no cursor ⇒ the downloader enumerates before tailing,
   which is the reconstruction wanted and needs no wiring. It costs a full
   re-sync's requests and a §6 conflict copy per genuinely divergent path — the
   correct answer with no baseline, not a side effect. **Only `ErrInvalid`,
   `ErrVersionMismatch` and `ErrChecksum` count as unreadable**; `ErrTimeout`
   means another process holds the lock and replacing that file would discard a
   live mount's state (`TestOpenDoesNotReplaceALockedFile`).

**Group commit (`NoSync` + a periodic `Sync`) is measured, coherent and
deliberately NOT built** — 8× fewer bytes at a 100-commit interval, 67× at
close-only, and transparent to §4 because bbolt readers see committed-but-unsynced
transactions, so deferring the *fsync* is invisible where deferring the *commit*
is not. Three things hold it, and the second is the one that grew the prerequisite
past what the win justifies. The failure mode is **corruption, not a lost tail**
(the meta page can reach disk before the pages it references), and it needs power
loss — a process crash, panic or OOM kill is harmless, the page cache survives.
**bbolt does not checksum data pages, so the failure can be a panic rather than an
error**: open-time validation covers only the two meta pages, `db.meta()` panics
outright if both fail (its comment says that is unreachable "because we do fsync()
on every write" — the assumption `NoSync` removes), and a never-written page reads
as zeros whose page type matches nothing. For the index that is worse than for
state, because M9 rule 6 relaunches a dead backend, so a panicking index DB is a
crash loop. And **`Close()` does not sync** — bbolt munmaps and closes the fd, so
any such design must fsync explicitly at `Close` and at the existing barriers.
Revisit when a backend makes commits the rate limiter rather than a wear cost:
Drive at ~1.09 uploads/s hides them entirely, but M11, M17 `localfs` and M18 SFTP
over a LAN all beat 749 commits/s. **The cheaper thing first** is batching the
*inbound* echo writes at the page boundary the downloader already has
(`flushForget`/`SetCursor` are there and already ordered) — full durability, no
panic handling, but it needs its own reasoning about a path appearing twice in one
page.

## Directory moves (MovedFrom)

**A folder move was the one mutation the seam could not express, and leaving it to
the sweep was measured and found to be the worse failure, not the lesser one**
(MC-27 live 2026-10-14, fixed 2026-10-16; DESIGN.md §2.5). A file move had been
reported as remove-old + add-new since M7. A directory could not be: a removal
above the seam is a *recursive* local delete and Drive reports nothing for the
children of a moved folder, so the subtree would be deleted with nothing in the
feed to bring it back. So the feed said nothing — and at 1100 files every peer
downloaded the subtree a second time, kept the old one as orphans, and then had
its repair pass propose 1101 deletions against a `-max-deletes` of 100, refusing
the whole thing while logging `reconcile complete`, with nothing scheduled to
retry. 2200 files, permanently. Six things carry the fix.

1. **`provider.RemoteChange.MovedFrom` is additive in both skew directions, and
   that is why the protocol version does not move.** It is only ever set where the
   change already describes the object at its *new* path, so a host that ignores
   it does exactly what every host did before it existed. That is also why a
   **file** move keeps its pair instead of being converted: both forms converge
   for a file, and the pair is the one an older host needs in order to converge at
   all. Setting both on one change would tell a host to rename a path it had just
   been told to delete.
2. **It is advice and the host validates it.** `moveLocally` declines an unclean
   or escaping path, either path inside the other, an absent source, and an
   **occupied destination** — `os.Rename` replaces a file silently and what it
   would replace may be local work nobody pushed, which is §6's question to answer
   with a conflict copy and not a move's to answer by overwriting. Every decline
   falls through to the pre-existing behaviour, which is what makes the whole
   thing safe. The path validation is above the seam on purpose: a plugin is
   trusted code (M9 rule 8) but this is the first provider-supplied value that
   drives a destructive operation on a *subtree*.
3. **A provider may set it only for a move of the same object with unchanged
   content**, because the host carries the §4 baseline across it. A backend whose
   `Move` is copy-and-delete must leave it empty — identity is exactly what such a
   move does not preserve. SFTP sets it nowhere: it has no feed at all, so the
   sweep is its inbound path and a rename there is already just two listings.
4. **`state.RenameSubtree` is the load-bearing half, and it is one commit however
   large the subtree.** It carries `echo`, `hydration`, `repair` and `refetch`; `tombstone`
   and `seen` deliberately stay put (a tombstone describes the object that *left*
   a path, and seen marks belong to a generation whose listing predates this).
   A baseline is not a cache — it is §4's loop breaker, M6's gate, M7b's delete
   evidence and M27's divergence test — so a dropped one is what turned the old
   subtree into orphans. `commitsPerRename` is 2 and was 2 before, because the
   re-key subsumes the `Forget` it replaces.
5. **The sending side was broken the same way and has nothing to do with the
   protocol.** An outbound rename dropped the record for the renamed path only, so
   *the renaming client's own* next sweep found every descendant's baseline keyed
   under a path that no longer existed and proposed the identical 1101 deletions.
   `Engine.renameRecords` re-keys instead. Anyone reading MC-27 as a
   receiving-client bug will fix half of it.
6. **The delete-before-write ordering in `RenameSubtree` guards nothing
   reachable, and the comment says so.** With the nesting guard above it the old
   and new key ranges cannot collide; it is kept because it is the half that
   survives someone relaxing that guard, and because `pathindex.Rename` does the
   same. It was mutation-tested, survived, and the claim was corrected rather than
   the code deleted — a sibling rename (`a` -> `ab`) is a *scan* question, which
   the `wideload.txt` sibling in `TestRenameSubtreeMovesTheWholeSubtree` is the
   assertion for.

14 mutation checks, one per condition, in `internal/state/rename_test.go`,
`internal/syncengine/move_test.go`, `internal/provider/gdrive/changes_test.go`
and the fake plugin's feed (which is a real out-of-process executable, so the
field is proven across the socket and not only in the converter).

**Confirmed against real Drive on 2026-10-17** (`scripts/multiclient/
mc27-dirmove.sh`, three eager clients, 150 files — the smallest size that
reproduces the *stuck* state, since the repair must exceed the default
`-max-deletes` of 100 to be abandoned). From the feed alone: **one `[pull] move
wide -> wide-renamed (151 record(s) re-keyed)` per peer, 0 downloads**, old
directory empty, new one complete on all three, applied **2s and 5s** after the
push against the 8m37s of re-downloading MC-27 measured. Then a forced `-resync`
on all three: `151 listed, 151 reconciled locally, 0 pushed` in ~670ms, **zero
refusals** — and **c1 is the row that matters**, because that is the sending-side
half, where pre-fix its own sweep found 151 baselines under a path that no longer
existed and refused its whole delete pass. `[pull] move` is the line that tells a
fix from a coincidence; `logcount.sh` counts it. One residue left alone on
purpose: a peer still logs `[pull] mkdir` after the move, because the ordinary
apply runs afterwards and a no-op `MkdirAll` still reports itself.

## Deletions and §4 (the tombstone)

**A delete push drops the one record that can recognise a stale report of what it
deleted, and that is how a client's own `rm -rf` came back** (found live
2026-10-16, fixed the same day; DESIGN.md §4.5). §4's loop breaker compares an
inbound change against the baseline for that path — so when the baseline is
*gone*, Drive's lagging `trashed` flag on a trashed folder's child reads as an
ordinary remote create. A deletion therefore leaves a **tombstone** (`internal/
state`, its own bucket: the `Echo` it removed plus when it removed it), and the
inbound path applies §4's own test to it for `tombstoneWindow` (5 minutes).
Five things carry it.

1. **Identity, not time.** The obvious rule — drop an inbound change older than
   our deletion — compares *our* clock against the *provider's*, so it is wrong
   by the skew between them in the direction that resurrects a file. The record
   we kept is a better witness: the test is `Echo.Matches`, exactly as the live
   echo's is. A re-creation carrying different content is applied whatever either
   clock says; one carrying the same content is suppressed and then found by the
   next sweep — a delay, against a resurrection.
2. **Its own bucket, not a flag on the echo.** Four passes read the echo bucket
   (M7b's delete baseline, M6's gate 2, the local walk's "new locally" row, M27's
   lost-push row) and a tombstone must be invisible to every one of them. A
   separate bucket is invisible by construction; a flag is invisible only until
   someone forgets to check it. `TestATombstoneIsNotABaseline` is the assertion.
3. **One commit, so the §12 ratchet is argued rather than crossed.**
   `ForgetDeleted` is `Forget` plus the record in one transaction, `SetEcho` drops
   any tombstone in *its* transaction (a sync and a deletion are contradictory
   claims about one path, and at most one is ever stored), and `ForgetMany` drops
   them too — which is how the commonest retirement, the feed agreeing the object
   is gone, costs nothing. `TestUnlinkCommitsPerFile` now runs both delete shapes.
4. **A baseline with no identity is not entombed** — it could match nothing, so
   storing it would spend a record to say nothing. That is the directory case on a
   provider reporting no version for one, and it costs nothing here because
   **nothing reported the directory in the first place**: the resurrected
   directory was created by the *download* as its parent, so suppressing the file
   is what keeps it away. The probe log is the evidence — `pull=2 mkdir=0`.
5. **The sweep asks the same question and prunes the records.** A listing can be
   taken inside the same window, so `reconcileRemote` consults it too (before
   `wouldMaterialize`, so the summary does not count a suppression as a file it
   reconciled), and the sweep is where expired records go. A mount that never
   completes one keeps a record per deleted path, of a size with the baseline that
   path already had; that residual is deliberate, because pruning anywhere else
   would be a second answer to "is this deletion still live?".

What it does **not** cover: `-max-deletes` is still the only guard on an inferred
deletion, a tombstone is not a trash, and the §6 cases above are unchanged.
`internal/syncengine/tombstone_test.go` carries the five mutation-checked
conditions; `scripts/multiclient/mc23-selfresurrect.sh` is the live repro.

**Confirmed against real Drive on 2026-10-17, and the run corrected two things.**
8 cycles, one client: **4 entered the window** (against 1 of 8 on the run that
found the defect — the race is commoner than that suggested), 12 suppressions, **0
downloads, 0 mkdirs, the directory gone in all eight.** The trace is the pre-fix
one with its middle line replaced — `[sync] delete sub2/e14.txt`, `[pull] ignore
sub2/e14.txt (deleted here 0s ago; …)`, then `[pull] delete sub2` six seconds
later. **The lag is now measured rather than bounded**: the twelve ages run 0s to
5s, so `tombstoneWindow` really is an order of magnitude above the observed
maximum. And **rule 4's directory case is belt-and-braces on Drive, not the only
mechanism** — a Drive folder carries a `version`, so its baseline has an identity,
so the deletion entombs it and the stale report of the *folder* is suppressed
directly (`[pull] ignore sub2`, in two of the four cycles). Suppressing the child
is what keeps the directory away on a provider reporting no version for one; here
both hold. Do not read rule 4 as "the directory is never entombed".

## The sync path's two silences (M27)

Two places the engine did the right thing and told nobody, and — since part 4 —
one place it did the right thing and nothing else could tell. Each fix lives
where the fact already was; none is a verbosity knob (M25 rule 1 — `-debug` is
go-fuse's protocol trace and must never come to mean two things). Items 1–3 are
the shipped milestone; 4 closed its one known gap on 2026-10-16.

1. **Back-pressure is reported on the state *transition*, and hysteresis is what
   makes that possible.** `backpressure` in `engine.go` is driven from
   `dispatch`, because that is **where the blocking happens** — a depth sampled
   in `vfs.node.emit` cannot tell a full worker queue from a slow provider. One
   line entering an episode (depth, the 1024 + workers×64 bound,
   `-upload-workers`, `-push-delay`), one leaving it. **In steady saturation
   every dispatch finds room the instant a worker frees a slot and then refills
   it**, so a report keyed on "did this send block?" is the ten-thousand-line log
   this replaces — a mutation removing the episode check produced **101 reports
   where 1 was wanted**. **The event that ends an episode is the one that never
   arrives** — the copy finishes, the queues drain, nothing dispatches again — so
   a sampler (`satSample`, 1s) is armed *only while saturated*. It samples; it
   never reports on a tick. And **1280 is the best case, not the bound a workload
   meets**: paths are hashed to workers, so a burst landing mostly on one blocks
   at `eventBuffer + 64` while the others idle — it printed "646 of 1280" the
   first time it ran, which is why the line reports the depth reached rather than
   claiming the queue is full.

   **The episode ends when the coalescer has nothing left to dispatch and no
   dispatch has waited since the last tick — not at a fraction of the bound, and
   that is MC-12's correction (2026-10-14).** The shipped rule ended an episode
   below a quarter of the bound, which a bulk copy is *always* under: a burst
   plateaus at ~254 queued against the 320 that rule wants, because the backlog
   is not in the queues at all — it sits in the coalescer's debounce map, which
   `depth` did not look at. So the clear condition held for a whole 35-minute
   drain and the report flapped: **113 episodes and 112 clears over 413
   uploads**, every "cleared" line naming 253 events still queued. The new first
   condition cannot oscillate (nothing to dispatch ⇒ nothing can block until new
   events arrive) and the second keeps a slow provider inside one episode; the
   entering line now reports both halves ("256 of 1280 event(s) queued, 846
   awaiting dispatch"), and the second number is the one worth reading. **A slow
   trickle against a saturated pool still reports repeatedly, and truthfully** —
   those are separate episodes.

   Two things not to repeat. **MC-36's "exactly one line per client" does not
   survive this** — the same shape flaps under the shipped rule (15 clears in
   the regression test), its logs are gone, and nothing in the harness was
   counting the lines, which `logcount.sh` now does. And **the one test that
   drove the episode check used an unbuffered event channel**, the single
   geometry where a fraction-of-the-bound rule behaves, because it made the lone
   worker queue the bound; it is buffered as production is now. A log assertion
   is only as good as the geometry it runs in.

2. **An abandoned push records its PATH, and a bare flag would have repaired
   nothing.** This is the correction worth keeping: DESIGN.md's original entry
   said the repair already existed because "the local walk re-pushes a file that
   diverged from its baseline". It does not. `pushLocalOnly` is the *new locally*
   row and **skips every path that has an echo**, while an abandoned push is
   almost always a rewrite of a file that has one — whose baseline still
   describes exactly what the remote holds, so the remote pass matches the echo
   and correctly does nothing. Measured before the fix; pinned by
   `TestSweepRepushesAnAbandonedWriteToAnAlreadySyncedFile`, which fails with
   `pushes = []` the moment the pass is removed.

   So `internal/state` has a `repair` bucket (path → when first abandoned),
   written by `Engine.oweRepair` and consumed by `pushOwedRepairs`, which runs in
   `runSweep` **before** the walk — a repair that succeeds writes a fresh echo,
   so the walk then skips the path for the ordinary reason. Four rules:
   `OweRepair` **compares before writing** (the non-retryable branch gives up
   after *one* attempt, so a permission failure over a large tree abandons as
   fast as the engine can push); the record is cleared **before** the push, never
   after, because `Push` is synchronous and re-records its own debt if it gives
   up again; the record is **a path, not an operation and not content**, which is
   what keeps it from being the persistent outbound queue the design declines —
   `repairEvent` re-derives the event from the backing file (gone ⇒ drop,
   directory ⇒ `OpMkdir`, placeholder or special ⇒ drop, else whole-file
   `OpWrite` with nil extents); and **a rename is owed at its destination**.

**Inbound sync stalls while a client drains a large outbound backlog**, measured
twice (MC-36 at 1000 files per client, MC-50 at 300, 2026-10-13/14). During the
23.5 minutes its own uploads took, each client fetched about **20** of its peers'
2000 files; within seconds of its last upload landing, inbound resumed and took
the remaining ~1980 in 15 minutes. An idle peer with 3000 files to fetch and
nothing to push converged **twelve minutes earlier**. Three things about it. It
is **not a deadlock** — it clears unaided, so a supervisor is not the answer.
**Nothing is logged**, so from outside the process a suspended inbound loop and
an idle one are indistinguishable, which is §2.6's "content equality, not log
silence" in a third place. And **one mechanism is now identified and fixed, by
reading `Put` rather than by the goroutine dump this note asked for**
(2026-10-16): `gdrive.Put` took `d.mu` and held it with a `defer` across the
whole upload, and `d.mu` is what every other operation needs for a moment —
`Get`/`GetRange` to resolve a path, `Changes` to resolve each entry in a page,
`Enumerate` to register a page's folders. So while a client was pushing, its own
inbound half could not take a step, **and neither could a push of any other
path**. It is a per-path lock now (`lockPath`), with `d.mu` held only for the
index work at either end; `internal/provider/gdrive/putlock_test.go` has both
halves and both were mutation-checked. Two things not to over-read. **The other
two candidates are not excluded** — the state DB's single writer and the plugin
process's call serialisation are untouched, and the arithmetic says the mutex
alone should have cost ~4s per inbound file rather than the ~70s measured, so
run the rig with `-pprof` anyway. **Re-measured live 2026-10-17 and the stall is
gone** (`scripts/multiclient/mc36-interleave.sh`, two clients × 300 files written
at one instant): inbound transfers per outbound transfer during the push phase
went from **0.02 to 0.86** — c1 pushed 300 files in 96s while fetching **264** of
its peer's 300, c2 300 in 95s while fetching **254** — and the time series is the
evidence rather than the endpoint, because both columns climb together from the
first sample where pre-fix the download column was flat at ~0 for 23 minutes.
Fleet convergence 121s. **The arithmetic never closed and no longer needs to**: at
0.36s per inbound file there is no gap left for the two other candidates to
explain *at this scale*, so they are unneeded rather than excluded — a run large
enough to make commits the rate limiter could still find them. Do **not** read
3.1 uploads/s against MC-36's 0.71 as a 4× throughput win: two writers, not
three, and 13-byte files.

3. **Nothing starts a sweep on the spot.** `armRepairSweep` sets a deadline
   `repairDelay` (5m) out and says so; a repair sweep that leaves the debt
   standing doubles it to a ceiling of 80 minutes, so an enumerable-but-failing
   backend is not swept every five minutes forever. **`-sweep-interval 0` honours
   a repair sweep** — `0` means "do not poll", not "never repair" — which is why
   `runWithoutFeed` no longer returns at interval 0 but idles, waking at
   `repairDelay` to re-read the bucket. A poll rather than a notification on
   purpose: the two halves share the state store and nothing else, the record
   must be read after a restart anyway, and a callback would write `nextSweep`
   from a worker goroutine when it is single-goroutine state by design.

4. **A push LOST is not a push abandoned, and the boundary between them is
   process start** (part 4, 2026-10-16). MC-40 measured the gap on 2026-10-13:
   a path is recorded when a push *gives up*, and giving up is code — `kill -9`
   runs none, so the repair bucket is empty, and a rewrite of an already-synced
   file then has an echo that `pushLocalOnly` skipped on and that the remote pass
   matches, so no row of the reconcile table proposed it. 44 sweeps over 7
   minutes changed nothing, which made **a crash strictly worse than an
   outage**. Recording the debt *before* the push would close it and stays
   declined: that is the persistent outbound queue §12 prices, and it doubles
   every push's commit count. What shipped is `repushDiverged`, a second row in
   the walk reading the fingerprint M18 already stores
   (`state.Echo.LocalSize`/`LocalMTime`, which `matchesBaseline` reads for the
   delete rows) — one `os.Stat` on a walk that already visits every file, and it
   covers the power cut, the OOM kill, the drain that timed out, the backing
   file written while the mount was down, and a §6 *local newer* resolution,
   which leaves the local edit for an uploader a restart has forgotten. One comparison serves both directions
   (`fingerprintDiverged`), because a push row and a delete row disagreeing about
   whether a file changed is a bug waiting to be written.

   Four things narrow it and none is optional. **The local mtime must predate
   `Downloader.openedAt`** — a write this process received is the engine's to
   finish (debounce map, worker queue, in flight, repair bucket) and nothing is
   silently lost inside one process lifetime, since `vfs.node.emit` blocks rather
   than drops; without the bound the row re-uploads a file a long copy is still
   writing once per sweep, which on a feedless backend is once per *poll*
   interval, and push-on-close would stop deciding when a held-open file is sent.
   Its cost is that an out-of-band write under a live mount waits for a restart or
   `-resync`; a quiet period was weighed instead and had no defensible constant.
   **A baseline with no fingerprint is neither a match nor a mismatch** — counted
   in the summary and skipped, because guessing "diverged" costs an upload and a
   commit for every path in the tree where the delete rows' guess is bounded by
   `-max-deletes`. **Only a path the sweep listed** (an unlisted one is the delete
   pass's, which has more to go on). **Never a path the sweep could not settle**
   — a failed download or conflict copy leaves the remote side unknown, and a
   whole-file `Put` over it publishes one side of a conflict with no copy of the
   other — nor one `pushOwedRepairs` already claimed, which is why `applyPage`
   and the repair pass both feed the same `noRepush` set.

   **M6 gate 3 now records the baseline it verified, and without that the row
   cannot converge.** A metadata-only change (a touch, a save of identical
   content) moves the mtime without moving a byte, so the row proposes a push,
   the gate sends nothing, and the mismatch used to survive for the next sweep:
   one full-file hash per mount forever, and a whole upload per mount below
   `hashSkipMinSize`. One commit per skipped push settles it — the same budget a
   push that uploads already has, so §12's ratchet is argued rather than crossed.
   `TestARepushedMetadataChangeSettlesAfterOneSweep` is the only place both
   halves are visible at once.

   **The row only runs inside a sweep, and until 2026-10-17 nothing started one
   after a crash on a provider with a feed.** `startFeed` swept for a resume, no
   cursor, `-resync`, an owed repair or an overdue interval — none of which a
   `kill -9` produces — so on Drive a lost push waited up to `-sweep-interval`
   and at `0` waited forever; MC-40b's legs never saw it because SFTP sweeps on
   every start. The fix is an **unclean-shutdown mark** (`state.SetRunning`/
   `ClearRunning`, the `running` key in the sweep bucket): `Mount.Run` sets it
   before anything can emit and clears it only when the inbound loop stopped in
   time **and** `Engine.DrainedCleanly` — a drain that hit its deadline aborted
   uploads, which are lost pushes too. `Open` reads it (zero commits) into
   `ReconcileOptions.Unclean`. Two commits per mount *lifetime*, which is why it
   is not §12's persistent outbound queue. After a *clean* stop an out-of-band
   write to the backing tree still waits for a scheduled sweep or `-resync`; that
   is deliberate, since sweeping every start is the cost the mark exists to avoid.
   `internal/app/unclean_test.go` is the pair, and the first fails with the
   trigger removed.

   **Re-run live on 2026-10-16, on both backends, and it is the A/B that is the
   evidence** — the defect is a *silence*, so one leg establishes nothing.
   `scripts/multiclient/mc40b-lostpush.sh` takes the build that crashes and the
   build that comes back up as separate arguments and was run three times over a
   tmpfs SFTP rig, killing a 256 MiB rewrite at a measured 25% of its upload:
   pre-fix, **39 sweeps and no repair**; fixed, **the first sweep, 3s**; crashed
   on the old build and restarted on the new one, **2s** — the shape a user
   meets, and the one that shows the row reading a state DB an older build wrote,
   which it can because `LocalSize`/`LocalMTime` have been stored since M18.
   Exactly **one** repush in each repaired leg, and the repair bucket empty in
   all three, which is the premise. `mc40b-drive.sh` carries the two halves only
   Drive can answer, both passing: the re-push **updates the object** rather than
   growing a second one at the path — one object, not two, which answers MC-40's
   duplicate half for the repair path — and a metadata-only divergence **settles
   after one pass** (one repush, one gate-3 skip, no upload, the remote
   untouched), with a genuinely changed file re-pushed in the same run as the
   control. Both rigs were interrupted mid-transfer, and the evidence is
   per backend: the temporary's size on SFTP, and 32.6 MiB of a 64 MiB rewrite
   already taken by the backend on Drive. Two things the rigs cost to learn: an orphaned backend must be
   identified by **parentage**, since every live client has a `plugin-serve` of
   its own and a command-line pattern also matches the shell doing the matching;
   and `/proc/<pid>/io` cannot see a Drive upload at all, because QUIC sends with
   `sendmsg` and `wchar` counts the write family — a complete 64 MiB upload moved
   it 0.10 MiB, while the backend's `rchar` tracked it exactly.

5. **A failed inbound apply is retried by path, from the pull loop, with no
   sweep** (part 5, 2026-10-17 — MC-46's finding). The cursor still advances
   past a page with a failure in it, **and must**: holding it for one change
   that can never apply (a full disk, a refused name) stops inbound sync for
   every path, for good. So the path goes into `internal/state`'s `refetch`
   bucket and `refetch.go` retries it `refetchDelay` (1m) later, doubling to
   64m, at once after a restart. Four rules. **A retry `Stat`s the path and
   applies what the remote holds *now*** through the feed's own `apply` —
   never a replay of the failed change, which is why the record holds no
   version. **A removal is never recorded**, nor does a retry that finds the
   path gone touch anything local: re-applying a deletion later deletes
   whatever is there by then, which may be unpushed work — part 2's
   abandoned-deletion rule from the inbound side. **It runs on the pull loop's
   goroutine, between polls**, so it cannot interleave with a feed apply.
   And **`SetEcho`, `ForgetMany` and `ForgetDeleted` discharge it inside the
   transaction they already commit**, so a retry that downloads costs one
   commit and §12's ratchet does not move; a push's `SetEcho` discharging it
   is right too, since the retry would have found that echo and done
   nothing. `RenameSubtree` carries it. Real ENOSPC ran on 2026-10-18 over
   SFTP (a full tmpfs backing store, byte-identical once it grew), but there
   the sweep retries first, so the feed path's live half is still unrun.

**One thing M27 does not close, and it is not an oversight.** An abandoned
**deletion** is not repaired — no local content to re-send, and inferring it
later has to go through `-max-deletes`, which is the delete pass's machinery, so
`repairPathOf` declines to record one at all.

**A shutdown now overtakes a blocked dispatch (2026-10-17)**, which this section
listed as the second thing M27 did not close. The send into a worker queue is
still unconditional *while running* — dropping is what the bound exists to
prevent — but `Engine.BeginShutdown` arms an abort `drainTimeout` after the
shutdown *request* (`app.Run` calls it from `ctx.Done()`; `Run` calls it at loop
exit too), and a send still blocked then gives up. The request and not
`close(events)` is the only usable start: with the run loop parked, FUSE
requests park in `vfs.node.emit`, so the unmount cannot complete and
`close(events)` is never reached. What is shed goes to the repair bucket in **one**
commit (`state.OweRepairs` — a stuck provider can leave a thousand events) and
makes `DrainedCleanly` false, so the unclean-shutdown mark stays and the next
start sweeps. It needed a second fix below it that nothing had seen: **`vfs.Serve`
tried `Unmount` exactly once**, and the EBUSY from a request still in emit made
it final, so `server.Wait` waited forever even after the engine let go — it now
retries every second while the mount is still being served.
`TestAStopEndsWhileTheProviderIsNotAnswering` is the whole chain over a real
mount (1435 events shed, unmounted on attempt 3, 4.5s);
`TestShutdownOvertakesABlockedDispatch` and `TestABusyUnmountIsRetried` are the
halves, each mutation-checked. **An in-flight provider call that ignores its
cancelled context is bounded too**, without the per-attempt deadline §6
declines: after the drain deadline cancels the workers, Run waits
`abandonGrace` (5s) more and then returns with them still running, the stop
unclean (the mark stays, so the next start sweeps), and a plugin backend's call
ended when `Close` kills the backend.
`TestADrainDoesNotWaitForeverOnACallThatIgnoresCancellation` is the assertion.

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

   **The second consequence is `-materialize`, and it changes meaning on a feedless
   backend**: `wouldMaterialize` gates the sweep's "new remotely" row *only*, so
   with a feed the flag governs the content that predates the first cursor and
   nothing else — anything created later arrives through `d.apply` regardless. With
   no feed it governs **everything inbound, forever**, so an eager SFTP mount
   without it is permanently upload-only. Found by running two of them (MC-47's
   rig, 2026-10-10): both pushed, neither received, and the sweep said
   `1 not fetched (add -materialize to download them)` every three seconds — the
   product reporting itself correctly against docs that had never mentioned the
   flag. `docs/user/sftp.md` owns that fact now. M19–M21 inherit it; it is not a
   reason to change the default, because a flag that means "download the whole
   remote tree" must stay a decision rather than a side effect of mounting.
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
7. **`Put` is write-to-temp-then-rename, and the temp name is unique per call.**
   The whole-file `Put` is the fallback behind every M6 gate so it runs constantly,
   and an interrupted in-place write would leave the remote truncated with no
   digest gate to ever notice. `Enumerate` skips the `.drivel-upload.` prefix so a
   temporary stranded by a dropped connection is neither materialised locally nor
   later inferred as a deletion. `conn.rename` takes `posix-rename@openssh.com`
   when the session advertises it — the fallback is not just slower, it has a
   window where the destination is absent.

   **Uniqueness is the correctness half and it was missing until MC-40**
   (2026-10-13). The name was `tempPrefix + path.Base(abs)` — a function of the
   destination alone — so two `Put`s of one path opened one server-side file,
   wrote at their own offsets, and the loser went on writing through its handle
   into the file the winner had already renamed into place: a destination holding
   a mixture of two uploads, with an echo recorded saying it holds one of them.
   Reproduced at 4 MiB in `TestConcurrentPutsOfOnePathDoNotShareATemporary`,
   which reports the mixture's composition and fails every run against the old
   name. **Two writers is not a rare shape**: within one mount the reconciler
   pushes through `Engine.Push` while the worker pool may be pushing the same
   path, and across mounts the server-side name is a *shared namespace*, so two
   clients collide however carefully either one is ordered. `crypto/rand` and not
   a counter, for that second reason. The destination's name is kept but capped
   (`tempBaseMax`), because a stranded upload should say what it was and because
   prefix-plus-a-long-name used to exceed the 255-byte limit on its own.

   **What it costs is self-cleaning, and that is now the normal case rather than
   a rarity.** A stranded temporary used to be overwritten by the next `Put` of
   the same path and renamed away with it. It is not any more — and the plugin
   liveness fix (M9 rule 10) makes one *likelier*, because the backend now exits
   promptly instead of unwinding, so the `Put`'s own `Remove` of its temporary
   does not run. It stays invisible (`Enumerate` skips it) and costs quota.
   **Reclaimed since 2026-10-17**: `Enumerate` removes a temporary whose mtime is
   older than `staleTempAge` (24h). The clock that matters is *time since the last
   write*, not upload duration — SSH_FXP_WRITE moves the server's mtime, so a live
   upload of any length stays young and only a dead one ages — which is why the
   threshold need not exceed the longest plausible upload after all, only
   comfortably exceed clock skew between client and server. A failed `Remove` is
   ignored; the next sweep tries again.

   **`Create` (provider.Creator) links the temporary instead of renaming it**
   when the session offers `hardlink@openssh.com`, because `link(2)` refuses an
   occupied name, which makes create-if-absent atomic: two clients creating one
   path cannot both win. Without the extension it is a check and then a rename.
   `TestCreateLosesARaceWithoutReplacing` writes the destination mid-upload, and
   it fails with the link swapped for a rename.
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

    **The pull's §6 check had the same hole and kept it until 2026-10-18**:
    `applyTo` ran the conflict test only when the remote reported a hash, so
    on every hashless backend an unpushed local edit was overwritten by the
    next remote change with no copy — the user docs promised one. Found by
    MC-31 over a real sshd, where it left c2 holding c1's bytes and the remote
    c2's. `hashlessLocal` now answers it: `fingerprintDiverged` against a
    fingerprinted baseline (the third caller, so all three agree), else the
    bytes — different length is a conflict, equal length fetches and compares,
    which costs nothing the apply was not going to spend. `hashless_test.go`.
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
   per-mount ordering is load-bearing: unmount → **wait for the inbound loop** →
   `close(events)` → wait for the engine's bounded drain, engine on a **detached
   context**. `App` aggregates around that; it must never collapse it into one
   cancellation. (gosec flags the detached context — the waiver is deliberate, not an
   oversight.)

   **The inbound wait is newer than the rest and is a fix** (Group V, 2026-10-09):
   `Run` used to return once the engine had drained, leaving `down.Run` to notice
   the cancelled context on its own, so `Close` shut the state DB under a live sweep
   — `[sweep] delete pass: database not open`, which abandoned the delete pass and
   skipped `FinishSweep`, parking a sweep that had actually finished. **Cancelling a
   goroutine is not the same as waiting for it**, and the version that costs data is
   in-place mode: the backing path is `/proc/self/fd/N`, and once `Close` has closed
   that descriptor the number can be reused, so a download still writing through it
   writes into whatever took it. Both waits are bounded and the inbound one is the
   shorter (`inboundDrainTimeout`, 10s against the engine's 30s) because an
   unfinished sweep is resumed and an unfinished download is retried, while the
   engine holds uploads nothing will re-offer.
2. **`app.Validate` runs before anything opens, on the flag path too.** Each guard
   exists because the failure is otherwise a deadlock, an opaque five-second bbolt
   timeout, or a file synced to the wrong account: shared state DB (each engine
   reads the other's echoes as its own baseline — which is also M7b's *delete*
   baseline), either direction of backing-tree/mountpoint overlap (§2.7's cardinal
   rule, generalised), shared mountpoints, duplicate names, and **a state DB inside
   a backing tree** — the one that needed no second mount to be wrong, since the DB
   syncs itself and its own writes generate more events.

   **Every one of those guards is intra-process, and MC-47 measured what that
   costs** (2026-10-10, two real processes over a local sshd). The shared state DB
   is the one shape with a second line of defence: bbolt's flock refuses the
   newcomer in 5s, the live file is untouched, and `state.unreadable` excludes
   `ErrTimeout` so it is never moved aside — the fix there was the *message*, which
   named the operation twice and the cause not at all. The overlap shapes have
   nothing: two drivels whose backing trees nest multiply content without bound,
   one level per sweep interval in each direction, with **no component
   misbehaving** — A's walk correctly pushes a local-only file, B correctly
   materialises a remote one, and the paths just keep getting longer. What does
   *not* happen is a delete storm, and the reason is structural: `applyDeletes`
   walks `EachUnseenEcho`, so a path hidden *locally* (an emptied mountpoint) is
   the "would materialise" row and never a delete row.

   **Guarded since 2026-10-17 by `internal/claim`**, the test plan's §4.18 options
   built as written. Each mount writes `.drivel-backing` at its backing root and
   holds an exclusive **flock** on it from `Open` to `Close` — flock and not fcntl,
   because an fcntl lock drops when *any* descriptor on the file closes, so the
   probe would release the holder's own claim. Three readers, and the split is
   the point: `Open` walks **up** from its backing root (exclusive), mountpoint
   and state-DB directory and refuses a *live* claim (the child started second),
   and `Acquire` refuses a second process on the same directory; the sweep's walk
   prunes any subdirectory holding a marker (the parent started second, which no
   up-walk can see) and, on Linux, any `fuse.drivel` mountpoint from mountinfo
   (shape (c), deliberately narrower than "do not cross filesystems", which is
   the user's call). **The lock is what "live" means**: a stale marker is a note,
   never a refusal, or an old directory becomes a permanent veto. Another
   process's *state DB* inside this tree when this mount starts second (an
   ordinary file of any name, so no up-walk sees it) is caught at the push
   instead: `pushContent` declines a file someone holds an exclusive **flock**
   on (`claim.HeldByAnother`), which bbolt does for every database open for
   writing. A lock taken *through* a FUSE mount stays in the kernel and never
   reaches the backing file, so this sees only processes working on the backing
   tree directly. Not covered: a write *through* this mount into
   the nested tree (bounded at one level, because the nested mount's copy lands
   under the marker the walk prunes). **The marker name is reserved in both
   directions** (`syncengine.reservedPath`, at any depth): pushed, a nested
   drivel's marker written *through* this mount reaches every client and their
   walks prune that subtree for good; pulled, a remote object of the name
   overwrites the live marker, and a remote deletion of it removes it.
3. **The registry is a value, not a package-global.** No `init()` registration, no
   import for side effect. It would be the only process-global mutable state in the
   tree, in the milestone whose premise is that there isn't any — and the explicit
   form is what lets a test register `gdrive` twice and run two Drive stores at
   once, which is M8's seam proof. **The pseudo-provider is test-only and stays
   that way**; nothing shipped registers a duplicate.
4. **Provider config crosses the seam undecoded** — since M9 as the TOML text the
   user wrote (`provider.Config`, whose `Decode` fills a provider-defined struct),
   because a closure cannot cross a process boundary. `internal/config` must never
   learn what a Drive folder ID is. Adding a provider touches neither package.
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

## Plugin architecture (M9, M23)

Every backend runs in its own process, talking to the host over gRPC on a unix
socket (hashicorp/go-plugin). **One plugin process per mount** — two mounts are two
sets of credentials, which is the same per-mount rule that keeps their state DBs,
engines and worker pools apart.

**Two launch paths, differing in one step.** A backend the tree ships is *bundled*
(M23): its code is in `drivel`, which re-executes itself as `drivel plugin-serve
<kind>`. Anything else is an installed `drivel-provider-<kind>` found on the search
path, which is how M9 shipped. `newRegistry` is `bundledBackends` plus a directory
scan for the rest, and precedence is **bundled-wins per kind, with the ignored file
named in the log**. Everything after the exec is identical, and that is a
requirement rather than an observation — a behaviour reachable on one path and not
the other is a difference nothing tests, which is why both are tested separately
(`plugin/bundled_test.go`, `cmd/drivel/bundle_on_test.go`, and the fake plugin for
the installed path).

Ten things carry the correctness.

1. **Capability detection is negotiated, not asserted, and this is the load-bearing
   change.** The host-side proxy is *one type serving every backend*, so it
   implements all seven optional interfaces whatever is behind it — a type assertion
   would answer "yes" seven times for every plugin, and the mount would poll a change
   feed that does not exist and splice extents into a store that cannot patch. Ask
   through `provider.AsChangeSource`/`AsEnumerator`/`AsRangeGetter`/`AsRangePutter`/
   `AsContentHasher`; never `store.(provider.X)`. The invariant is **a declaration
   narrows and can never widen** — `provider.Capabilities` intersects the method set
   with the declaration, so a store cannot talk its way into a method it does not
   have. In-process backends implement what they can honour and do *not* implement
   `Declarer`; only the proxy and test doubles need it.
2. **The capability set is fixed at the first Open and held across restarts.** The
   engine wires itself to the answer once — whether this mount has a pull loop at
   all is decided at mount time — so a set that changed underneath it would leave a
   downloader polling a feed that is gone. A restart reporting something different
   is logged loudly and ignored.
3. **"Unknown kind" has to say where to install one.** `provider.Registry.Hint`
   is set by `Loader.Register` so a config naming an uninstalled backend gets
   "no drivel-provider-gdrive in …" appended to the registry's own message. The
   registry must not learn what a plugin is — it holds factories and nothing else
   — so the layer that knows contributes the sentence.
4. **The kind is the host's word for a backend and nothing the plugin says.** For
   an installed plugin that is its filename; for a bundled one it is the map key
   and the argument the host passes itself (M23 reworded this rule rather than
   violating it). A plugin that named itself could contradict its filename, and two
   files could then claim one kind with a resolution order that is either a
   directory listing or last-install-wins. Two files claiming one kind *by filename*
   resolve first-on-the-path-wins and the loser is named in the log.
5. **Four error classifications are rebuilt on the far side, and each one lost is
   silent.** The plugin classifies (only it can see its backend's error types) and
   sends the classification as a gRPC status detail. `ErrExist` lost ⇒ a first
   push over an unknown remote fails instead of keeping a conflict copy. `ErrNotExist` lost ⇒ renames
   stop syncing. `ErrCursorExpired` lost ⇒ inbound sync stops forever, which is the
   bug the sentinel exists for. `IsRetryable` lost ⇒ the first 429 is permanent. The
   concrete error *type* is deliberately not preserved.
6. **A dead backend is a retryable error, not a dead mount.** The next call
   relaunches behind an exponential backoff and the engine's M4 retry loop is what
   waits, so a crash costs a deferred push. A process that lived ≥ 60s has its
   failure counter reset, so a backend that dies hourly does not inherit a crash
   loop's delay. Do not add a supervisor; the retry already is one.
7. **`PutRange` reads backwards through a Content service, and both narrowings are
   deliberate.** `provider.RangePutter` takes an `io.ReaderAt` *because* an
   implementation may seek in whatever order its protocol prefers, so the file
   cannot be streamed into the call — that would change the contract for a backend
   loaded as a plugin while leaving it intact for the same backend constructed
   directly, which is how its own tests run it. The channel is opened **once per
   connection** (go-plugin's `AcceptAndServe` returns only when the whole broker
   shuts down, so a per-call listener accumulates one per range write for the life
   of the mount), and a **handle is registered immediately before the call and
   dropped immediately after**, so the plugin can ask for bytes of the file the host
   chose and never for a path.
8. **The environment is built, not inherited, and this is not a sandbox.**
   `plugin/env.go` holds the allowlist: `PATH`, `HOME`, `TMPDIR`, locale, TLS roots,
   proxy settings, plus `SSH_AUTH_SOCK` — the one entry that *is* a credential, kept
   because the SFTP backend documents the agent as an auth method and has a setting
   for declining it, so scrubbing it would leave that setting silently doing nothing
   (M8 rule 6 again). Everything else is dropped: ambient-credential conventions,
   all `XDG_*` (M16's privilege-drop lesson), all `DRIVEL_*` so a plugin cannot load
   plugins. A plugin still runs as the same user with that user's whole filesystem —
   say so plainly in any doc that describes it. What *is* enforced is narrower: a
   group- or world-writable binary, or a non-sticky world-writable directory, is
   refused, because a binary anybody can replace between install and launch is a
   stranger's code holding the user's credentials.

9. **The host computes content digests itself, and identifies the algorithm by
   observation rather than by declaration.** Proxying `ContentHasher` naively
   streams a whole file across the socket for thirty-two bytes back — and it is
   the call that runs *most*, because M6 gate 3 precedes most content pushes and
   `gdrive` declines `RangePutter`, so gate 2 always falls through to it. A changed
   1 GB file crossed the socket twice: once to be hashed, once in `Put`. On the
   first hash of a mount, `matchLocalHash` asks the backend to digest two fixed
   vectors and adopts a local implementation only if one reproduces **both**
   exactly (`plugin/hash.go`). Three things are load-bearing. **No match is an
   ordinary outcome** — a digest this build cannot compute, or the same one in
   another encoding, keeps streaming, and that fallback is what makes the whole
   thing safe. **Identification cannot be lied about**, which is why it is a probe
   and not a declared name: the answer is checked against the backend's own
   output, the same reasoning as rule 1's negotiated capabilities. And
   **mis-identification fails benign** — a wrong algorithm yields digests that
   never equal what `Stat` reports, so gate 3 declines and the file is pushed; the
   failure mode is a gate that stops helping, never a push that is wrongly
   skipped. Two vectors, not one, because a single input that two functions agree
   on would be adopted for every file after it. `HashContent` takes an `io.Reader`
   and nothing else, so a digest is structurally a function of the bytes alone —
   that is what makes a two-vector probe sufficient. An in-process backend never
   reaches the proxy and is unaffected.

10. **A backend must not outlive its host, and go-plugin has no mechanism for
    that** — the mirror of rule 6, found live by MC-40 (2026-10-13). go-plugin ends
    a plugin by having the *host* call `Client.Kill`, which `process.Close` does;
    a host killed by a signal runs no code, and nothing on the plugin side
    notices, because the child inherits the host's own stdin rather than a pipe to
    it and a gRPC server goes on listening after its last client disconnects. So
    `kill -9` on a mount left `plugin-serve` running indefinitely, holding that
    account's credentials and its connection to the provider, one more per
    crash-restart cycle. `plugin/liveness.go` is a pipe the host holds open for
    its own lifetime and the child reads: a clean EOF means every writer is gone,
    which the kernel guarantees for any way a host can die. Three things about it.
    **Only a clean EOF counts** — any other read error means the descriptor is
    unusable, which is not evidence about the host and must not take a working
    backend down. **A plugin with no pipe is left alone**, which is what makes
    `watchHost` safe to call unconditionally from `Serve` and keeps an
    older out-of-tree plugin working. And **it is cleanup, not a boundary**: a
    plugin is code drivel launched and trusted (rule 8), and one that wanted to
    survive could simply not read the pipe. `Pdeathsig` was rejected — Linux and
    FreeBSD only, and in Go it fires when the forking *thread* exits, so the
    runtime retiring that thread kills a healthy backend. **The child's command
    line is `/proc/self/exe plugin-serve <kind>` and its `comm` is `exe`**, so
    `pgrep -af plugin-serve` finds it and `pkill -x drivel` does not.

    **The mirror image is a backend that dies *before* its host, and SIGTERM did
    exactly that** (found reading the M16 stop path, 2026-10-17). `systemctl stop`
    signals the unit's whole cgroup, the backend included; the host shrugs SIGTERM
    off (it only cancels the context the unmount already ended), but go-plugin
    traps SIGINT and nothing else, so the backend died while the host was still
    draining through it. `watchHost` therefore also ignores SIGTERM — **only once
    the pipe is confirmed**, because the pipe is what still ends it; a plugin run
    by hand keeps the default. A drained `signal.Notify` rather than
    `signal.Ignore`, since `SIG_IGN` is inherited across exec.
    `TestAWatchedBackendSurvivesSIGTERM` fails with it removed.

**Versioning is one number.** `plugin.ProtocolVersion` is in the handshake and a
mismatch refuses the launch naming both. Adding a protobuf field does **not** bump
it — protobuf is already compatible both ways and that is the mechanism for anything
additive, and a capability name the host does not recognise is dropped with a log
line rather than refused. Removing a field or changing its meaning does bump it. A
compatible *range* is what to avoid: an almost-compatible plugin answers most calls
right and loses a sentinel or an mtime in the middle, which surfaces as data not
syncing.

**The protocol is generated and committed.** `make proto` regenerates
`plugin/internal/pb` from `proto/`; `make proto-check` is in `make check` and fails
on drift — the same arrangement the completions use, for the same reason (a distro
package has no protobuf toolchain). The toolchain is pure Go: `buf` is the compiler
as well as the driver, so **there is no protoc anywhere** and adding one would be a
step backwards.

**`gdconf` exists for one reason.** The `-drive-*` flags are the host's, so the host
must build a Drive settings table and generate completions for the two enumerated
values — and completions rule 3 says those words must be the program's own
constants. `internal/provider/gdrive/gdconf` is the leaf that carries the vocabulary
without the SDK; `gdrive` re-exports `Config`/`SweepMode`/`DeleteMode` as aliases, so
nothing below the seam changed. A new backend with enumerated flags needs the same
split, and a backend with no host-side flags needs nothing.

**Do not merge this protocol with M14's control socket.** The trust directions are
opposite — a plugin is code drivel launches and trusts, a control client is a user
drivel serves — and one transport serving both is how a plugin ends up able to stop
the daemon.

**Still compiled in: mount backends.** The `mount.Backend` seam would take the same
treatment, nothing is asking for it, and a FUSE connection is not a thing to hand
across a process boundary casually. **Not implemented: checksum pinning** —
go-plugin offers `SecureConfig`; the mode and directory checks are what ships, and a
pinned digest per kind in the config file is the obvious next step **for an
installed plugin**. M23 made it moot for a bundled one: a digest of code you are
already executing is a digest of yourself.

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

**systemd: verified 2026-09-29**, on WSL2's Debian (systemd as PID 1), together
with `mount`, `mount -a` (`-T`) and `umount`, which were already verified for real.
`systemd-fstab-generator` turns an `fuse.drivel` line into
`/run/systemd/generator/<esc>.mount`; `systemctl start` mounts it, `findmnt`
reports `fuse.drivel` (rule 1, from the other side), `systemctl stop` unmounts
cleanly — and **the daemon is in the unit's cgroup**, which is what this note
feared and could not check: `CGroup: /system.slice/mnt-<x>.mount └─ /sbin/
mount.fuse.drivel …`. Reached directly from a shell it lands in `/init.scope`
instead. **Mounting at boot ran on 2026-10-18** (same host, `wsl --shutdown`
then a cold start, a line *without* `noauto`, `_netdev,x-systemd.requires=` a
systemd-run sshd): the unit came up on its own, `Before=remote-fs.target`,
`After=` the sshd unit, and synced both ways. The sshd unit was `Type=simple`,
so it counted as started before it listened — the first enumeration met
`connection refused` and the sweep's retry recovered 30s later, while the mount
itself never waited, which is §2.1 holding at boot.

**The drain-on-stop question was answered on 2026-10-17, and the premise was
wrong**: systemd does *not* kill the cgroup on `stop`. A mount unit's stop is
`umount`, which returns in ~0.2s; the unit goes inactive and the journal says
`Unit process N (exe) remains running after unit stopped` — for the host *and*
the backend. So the drain runs **outside the unit's lifetime**, completes (40 MiB
over a throttled SFTP loopback, remote byte-identical), and nothing waits for
it. What *does* signal every leftover is system shutdown (systemd-shutdown's
kill-all, and WSL's idle stop does the same), which is where the backend dying on
SIGTERM mattered — fixed, see M9 rule 10; the A/B with a SIGTERM to both
processes mid-drain is "connection refused → relaunch → drain deadline" before
and a completed upload after. A shutdown that still cuts the drain short leaves
the unclean-shutdown mark, so the next start sweeps (M27 item 4). Anyone who
needs `stop` to *wait* should use a service unit (`drivel mount -config`,
`TimeoutStopSec`): measured, `stop` returned after the 20s drain with nothing
left running; `docs/user/fstab.md` carries the unit. The same run found Serve's
unmount goroutine re-unmounting an already-unmounted filesystem at exit and
printing the failure to stdout; it now exits with the mount and logs per mount.

## Shell completions

**The binary prints its own** — `drivel completion bash|zsh`, to stdout. Nothing
is committed under `completions/` and there is no `make completions`. Four things
carry it.

1. **The flags are the source.** `mountFlagSet`/`loginFlagSet` exist so the
   program and the renderer walk one `flag.FlagSet`; that is why flag definition
   is a function rather than a block inside `runMount`. Nothing in
   `completions.go` lists a flag by name.
2. **It stopped being build-tagged, and that was a reversal to make on purpose.**
   The tag kept `internal/completion` out of release binaries and was justified as
   layering rather than size. It was overturned because a **portable single
   binary** has no `make install` behind it to have placed a file in
   `/usr/share`, so the only completion that can exist for it is the one the
   binary prints. The old rationale for committing the files — "a distro package
   has no Go toolchain" — is *better* served, not violated: a package build that
   can run the binary it just built needs no toolchain either. `completions_off.go`
   and the `runExtraCommand` seam are gone with it; `completion` is an ordinary
   subcommand in `main`'s switch.
3. **`completionHints` is the only hand-written part, and it fails both ways.** A
   flag with no entry fails `completionApp`; an entry naming a flag that no longer
   exists fails it too. The kind is cross-checked against `IsBoolFlag`, and enum
   values are the program's own constants (`gdconf.DeleteTrash`, …) so a renamed
   mode cannot leave the completions offering a word the binary refuses. The
   summary is deliberately *not* the flag's usage string — usage is a paragraph,
   a menu line is a line.
4. **What `make completions-check` guarded is now a test**
   (`cmd/drivel/completions_test.go`), and the move is not cosmetic: that target
   compared two committed files against the flags, so it could only ever catch
   "someone forgot to regenerate" — a check on an artefact that no longer exists.
   The four real mistakes live in `completionApp`, which the test runs.
   `App.Validate` is the fourth of them: it refuses one flag name meaning two
   different things across subcommands, because bash completes a value from the
   previous word alone and cannot tell which subcommand it is in.

5. **The zsh renderer is driven, not just parsed.** `TestZshCompletes` runs a
   real zsh through `zsh/zpty` — a completion function only runs inside a
   completion widget, a widget only runs under ZLE, and ZLE needs a tty, so zsh
   cannot be handed `COMP_WORDS` the way bash can. Both install forms are
   exercised because the last line behaves differently in each, and the harness
   asserts `$_comps[prog]` first: with nothing registered every completion below
   falls back to filenames and the case "passes" for the wrong reason. No sleeps
   are needed — ZLE handles keys in order, so the TAB has been processed by the
   time the dump widget runs. `TestZshEvalFormIsSilent` covers the same bug
   without a terminal, so it still guards where zpty is unavailable. Both were
   confirmed to fail against the unguarded tail.

`make install` generates at install time by running the binary it just built,
which means **cross-compiling and then installing needs an emulator or a second
native build**. That is the trade, and it is the same one every other Go CLI makes.

The `eval "$(drivel completion bash)"` form is documented but is deliberately not
the only advice: it runs drivel at every shell start, and for zsh it works only
*after* `compinit`, which is what defines `compdef`. Writing the file is what the
man page and `install.md` lead with.

**The zsh script's last line has to do two different things and cannot know which
at render time**, so it branches on `$funcstack[1]`: autoloaded from `$fpath` the
whole file *is* the body of `_drivel`, so it must end by calling the function it
just defined; `eval`'d or sourced, nothing has registered it and calling it runs
`_arguments` outside a completion widget — `can only be called from completion
function`, which is what an unguarded call shipped for two weeks. The else arm
ends in `false` and **not `return 1`**: a `return` inside an `eval` in `.zshrc`
returns from the *sourced file*, silently abandoning the rest of someone's shell
configuration. Neither half of this is visible to `zsh -n`, which is why the zsh
renderer is now tested by completing with it (see rule 5).

The renderers are tested by **running** the output — bash sources the rendered
script and answers a real completion; zsh parses it. An assertion that
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

**The downgrade is per host and it expires — `h3Retry`, 5 minutes — and the
expiry is the part that was missing.** Remembering a host QUIC could not reach is
necessary, because without it every request pays a failed dial; remembering it
*forever* meant one transient block downgraded the process permanently, which
MC-44 measured on 2026-10-13: UDP/443 dropped for forty seconds and restored, and
all 15 requests in the 29 seconds afterwards stayed on HTTP/2, with HTTP/3 never
retried again. Over an fstab mount that runs for weeks, that is a blip defeating a project
requirement. Three things are load-bearing. **Only a failure re-arms the window**,
so recovery needs no second mechanism — a "clear the mark on success" path was
written, mutation-tested and deleted, because nothing could assert it: once the
window lapses, a successful probe simply leaves it lapsed. **The window is a cost
ceiling, not a guess** at how long a network stays broken: a re-probe costs at most
one failed dial per window per host, bounded by `h3HandshakeIdle`, and no FUSE
operation is behind it because §2.1 keeps the network off that path. And **`now` is
injectable** so the window is testable without sleeping through it. **Recovery
latency is "the rest of the current window", not a function of when the network came
back** — verified at the shipped 5 minutes, where a block lifted 285s into a window
gave HTTP/3 back 15.4s later.

**How fast the fallback fires depends on whether a connection already existed, and
the two answers are six times apart.** A `drop` on the local output hook is reported
to the sender as `EPERM`, so it fails in milliseconds — that is the 203ms the cgroup
run measured, and it is a property of the block, not of the transport. The blackhole
shape ran on 2026-10-13 (two netns across a veth, the drop on the far side, so the
packets leave and nothing answers) and split in two:

- **mid-session, 30.1s** — the in-flight request is on an established connection,
  nothing tells quic-go the peer stopped hearing it, and it retransmits until the
  idle timer fires. That is `MaxIdleTimeout`, which this transport had left to
  quic-go's `protocol.DefaultIdleTimeout`.
- **at startup, 5.0s** — `HandshakeIdleTimeout`, and the same for every re-probe,
  which is a fresh dial by then: holding the blackhole for 320s cost 5.02s at
  startup, 5.01s for the one re-probe and nothing for the other 101 requests.

Either way the request *completes* over HTTP/2 and nothing above the transport sees
an error: in a 433-second run exactly one request of 131 took longer than 100ms.
These notes claimed the handshake number for both shapes until the run. All three
timeouts are now named constants (`h3HandshakeIdle`, `h3MaxIdle`, `h3KeepAlive`)
pinned by `TestNewPinsTheQUICTimeouts` — `h3MaxIdle` is the 30s that was already in
force, so **this pins a measured number rather than changing one**, and a quic-go
default bump can no longer move it silently. Lowering it is the only direction with
any effect (the live value is `min(ours, the peer's)`) and it costs keep-alive
packets on every healthy mount, because the ping interval is
`min(KeepAlivePeriod, idleTimeout/2)` — so `h3KeepAlive` is a ceiling on the rate,
15s in practice, not the rate itself.

**What HTTP/3 costs here is CPU and not throughput, and the ratio is ten to one**
(MC-44's throughput half, 2026-10-13: five alternating pairs of a 64 MiB upload
and download through `gdrive.Put`/`Get` against the live rig, protocol forced by
dropping UDP/443 for the run's cgroup and *attributed* by nftables counters rather
than assumed — `scripts/multiclient/mc44-throughput.sh`). Upload cost **2.00
ms/MiB on HTTP/2 against 20.50 on HTTP/3**, download 6.10 against 13.10, and these
do not overlap across the ten runs where the throughput columns overlap entirely:
upload medians were 4.16 against 4.07 MiB/s with 64 MiB taking 15–17s every single
time, so **Drive is the limiter and the protocol is invisible in the transfer
rate**. Three things not to misread. **The download spread is Drive's and not the
protocol's** — it varies twofold between consecutive runs of *one* protocol
(25.50–53.98 against 14.80–43.31), which is why the harness alternates and reports
a median of five; a single sample each reads as a clean 2:1 win for whichever one
got lucky, and the first run of this did exactly that. **The difference, not the
ratio, is the protocol's** on the download leg, because the probe's own MD5 is a
constant in both: 18.5 ms/MiB up and 7.0 down. And **this prices the requirement
rather than overturning it** — §2.6's rationale is connection reuse, 0-RTT and
head-of-line blocking across *many small requests*, which a two-stream bulk
transfer does not test; what the run removes is any bulk-throughput advantage to
point at, so the argument for HTTP/3 has to be made on the poll loop. 1.3s of CPU
per 64 MiB upload is ~8% of one core at the 4 MiB/s Drive allows, against 0.8%.

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
   — exFAT/FAT, a tmpfs `/tmp` on FreeBSD. `setxattr` fails, `CreatePlaceholder`
   continues unmarked, and there is then no placeholder record at all, because
   **`IsPlaceholder` reads the marker and nothing else**. The state DB does *not*
   stand in: its hydration entries cache present ranges (`Hydrator.Ranges`, which
   nothing outside tests reads) and no path has ever consulted them for
   placeholder-ness. Docs said otherwise until M10 — "don't delete the state DB" was
   never the mitigation; "don't run `-lazy` there" is.

   **WSL2's drvfs was on that list until 2026-09-29 and should not have been**, and
   the correction is worth more than the entry was: the thing that decides is the
   **underlying Windows filesystem**, not the drvfs layer. `/mnt/c` mounts as 9p
   with `aname=drvfs` and maps Linux user xattrs onto NTFS extended attributes, so
   the marker **set, read back, listed, survived `sync`, and survived a full
   `wsl --shutdown`** — measured, not assumed. The same mount over exFAT would still
   fail, which is exactly the point. Do not put drvfs back on the list; put the
   *filesystem* on it. `/mnt/c` is still a poor `-data` for an unrelated and
   stronger reason — NTFS is case-insensitive, which is rule 3 below.
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

**Licensing is not in conflict with macFUSE, and under MPL-2.0 barely arises.** The
copyleft is per file and never reaches a separate program, so there is no combined-work
question; go-fuse also contains no cgo and does not link libfuse, which is why the same
answer held under AGPL. The only rule this imposes is a distribution one: **never
bundle macFUSE, never ship an installer that fetches it.** macFUSE 4.x's own non-free
terms are a burden on the user, not a licence conflict — don't restate them as one.
§2.9.2 has the argument and flags it as belt-and-braces now.

**Windows is a decided non-goal, not a gap** (§2.9.4): WSL2 covers the audience,
Drive for Desktop covers the rest, WinFsp/cgofuse forfeits the pure-Go build, and
NTFS case-insensitivity opens a *new* variant of the MC-30 same-name-sibling problem
(Drive is case-sensitive; `Foo.txt` and `foo.txt` collide into one local path). Keep
the seam clean anyway — the cross-compile shows that costs nothing.

## Build / test / run

```sh
make help                                      # every gate as a target, plus examples
make check                                     # everything CI runs, in CI's order
make build                                     # ONE binary: drivel with its backends inside
make build TAGS=nobundle                       # ...or the host alone, backends off the search path
make build-plugins                             # ...the backends as installable drivel-provider-*
make proto                                     # regenerate plugin/internal/pb after editing proto/
drivel completion bash                         # print the bash completion script
make hooks                                     # install the git hooks (once per clone)
make run ARGS='-debug'                         # build, then mount ./mnt over ./data

go build ./...
go vet ./...
go test -race ./...                            # what `make test` runs
DRIVEL_REQUIRE_TESTENV=all go test -race ./... # ...and nothing silently skipped

# One build is the whole program since M23 — the backends are in it, and it starts
# one by re-executing itself. bin/ is still first on the plugin search path, so a
# `drivel-provider-*` built there is found for any kind the binary does not carry
# (a bundled kind wins over a file of its own name, and logs that it did).
go build -o ./bin/drivel ./cmd/drivel
./bin/drivel mount -mount ./mnt -data ./data   # separate backing dir; -debug for FUSE tracing
./bin/drivel mount -mount ./dir                # in-place: ./dir is its own backing (Linux)

sudo make install                              # + /sbin/mount.fuse.drivel (plugins only for TAGS=nobundle)
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
- Keep the FS layer provider-agnostic — depend on `provider`, never on a concrete
  Drive type. Nothing above the seam may import `internal/provider/gdrive` or
  `internal/provider/sftp` — **`internal/app/seam_test.go` is what says so** since
  M23, over `go list -deps`, because the host now links both and the property
  stopped being readable off the binary. `cmd/drivel` is the composition root and
  the one exception, in `bundledBackends` and nowhere else.
- Ask a store what it can do with `provider.As*`, never with a type assertion. See
  "Plugin architecture" rule 1 for what the assertion answers wrongly.
- FS operations must not block on the network; sync happens off the FUSE path via
  the buffered event channel.
- Secrets (`credentials.json`, `token.json`, `*.local.json`) are gitignored —
  never commit them.
- **MPL-2.0, copyright SystemHalted and Jeremy Melanson** (AGPLv3 until 2026-09-21;
  `docs/project/licensing.md` records why it changed and what the alternatives cost).
  `LICENSE` is Mozilla's verbatim text — never edit it, never append to it, and never
  add a second license file at the root (both break the `licensecheck` detector
  pkg.go.dev uses; see `docs/project/publishing.md`). **The copyleft is per file**, so
  a closed-source backend against the `provider` seam is permanently fine and anything
  that would oblige a *caller* to open its source is outside what this licence asks —
  that boundary is the reason for the choice, not a side effect of it. **Every `.go`
  file carries the Exhibit A header**; without it a file copied out of this tree says
  nothing about what it is, which is exactly where file-level copyleft leaks. The
  copyright notice lives in four places that must stay in sync: README §License,
  `docs/user/drivel.1`, the `cmd/drivel` package doc comment, and the `usage()` text
  `drivel help` prints. What the licence cannot require, `README` §License *asks* —
  publish your backends, report your platforms; keep that a request and never let it
  drift into sounding like a term.
