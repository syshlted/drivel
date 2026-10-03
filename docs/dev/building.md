# Building and running

Every gate has a make target, and the Makefile is the single definition of each
one: the git hooks and CI both call these targets, so "it passed locally" and "it
passed in CI" cannot mean different things.

```sh
make help         # every target, plus worked examples — the default goal
make build        # one binary — the command and its backends — into bin/
make run          # build, then mount ./mnt over ./data
make test         # go test -race ./...
make lint         # golangci-lint over the whole tree
make fmt          # apply formatting
make check        # everything CI runs, in CI's order
make hooks        # install the git hooks (once per clone)
```

`make help` is the default goal, so a bare `make` prints it. Its examples are
read out of the Makefile itself — a target's description, and any example of
invoking it, are written where the target is defined, so neither can drift from
the recipe it describes.

## The run loop

`make run` builds first and then serves, because debugging a mount against a
stale binary is an expensive way to spend an afternoon. Three variables shape
the command line, and everything else goes through `ARGS` verbatim:

```sh
make run                                   # ./mnt, backed by ./data
make run ARGS='-debug'                     # ...with FUSE tracing
make run MNT=./dir DATA=                   # in-place: ./dir is its own backing (Linux)
make run MNT= DATA=                        # serve the mounts in the XDG config file
make run ARGS='-credentials creds.json'    # sync to Drive, rather than log-only
```

Empty means omitted rather than passed blank, which is what makes the last two
work: no `DATA` is in-place mode, and no `MNT` at all leaves the field clear for
`-config`, which refuses to be combined with flags describing what to mount.
Without `-credentials` drivel runs log-only — it mounts and logs sync events but
talks to no provider, which is the shape most of the FUSE work is done in. The
flags themselves live in [Configuration](../user/configuration.md) and in
`drivel mount -h`; `make run` adds nothing to them.

The underlying commands are still just the toolchain, and nothing stops you
running them directly:

```sh
go build ./...                          # compile everything
go vet ./...                            # static checks — keep clean
go test -race ./...                     # unit tests (no network required)
go build -o ./bin/drivel ./cmd/drivel   # the CLI binary
```

`go test ./...` runs offline — nothing in the suite touches Drive. For what it
covers, which tests need kernel facilities that a laptop may lack, and how to run
it on another platform, see [Testing](testing.md).

## Static by default, dynamic for packagers

`make build` sets **`CGO_ENABLED=0`**, producing a statically linked binary with
no library dependencies at all.

That is not a size optimisation — it is worth about 200 KB. It is what makes the
shipped artifact match the property DESIGN.md §2.9 rests on. Drivel's own code
uses no cgo, but the *standard library* does: `net` and `os/user` carry cgo
implementations for NSS-backed DNS and user lookup, and Go compiles them in
whenever cgo is available. Since Go defaults `CGO_ENABLED` to 1 on a native build
with a C compiler on `PATH`, leaving it unset silently produces a binary linked
against the build host's glibc — which then refuses to start on any distro whose
glibc is older.

```sh
make build                    # static; runs anywhere
make build CGO_ENABLED=1      # dynamic; links the host's libc
```

**Enabling cgo runs counter to the project's philosophy, and there is exactly one
sanctioned reason to do it: distro-specific packaged builds.** There, linking the
system's libraries is the point — the package tracks the OS's patch level, and
patching those libraries becomes the distribution's responsibility rather than a
reason for us to cut a release. Offloading that is worth the portability cost
*when something downstream is actually doing the patching*. Nowhere else is.

Two things not to do with this. Don't hoist `CGO_ENABLED=0` to a global in the
Makefile — **`go test -race` requires cgo**, so a global setting fails the entire
suite with `-race requires cgo`. And don't reach for it to shrink the binary;
stripping is the lever that matters:

| build | size |
| --- | --- |
| `CGO_ENABLED=1` (the old default) | 28.7 MB |
| `CGO_ENABLED=0` | 28.5 MB |
| `CGO_ENABLED=0 -trimpath -ldflags="-s -w"` | 19.6 MB |

## One binary, or a host plus plugins

`make build` produces a single executable that is `drivel` **and** every backend
the tree ships (M23). A backend still runs in its own process: the host launches
one by re-executing itself as `drivel plugin-serve <kind>`, which reaches the
same handshake, the same unix socket and the same generated protocol an installed
plugin does. The launch mechanism is the only difference between the two, and
that is a requirement rather than an observation — a behaviour reachable on one
path and not the other is a difference nothing tests.

```sh
make build                    # drivel, with gdrive and sftp inside it
make build-plugins            # ...and the backends as drivel-provider-* executables
make build TAGS=nobundle      # drivel alone; every backend comes from the search path
```

`TAGS` is threaded through `vet`, `test`, `test-fast` and `test-full` as well, so
`make test TAGS=nobundle` runs the suite against the unbundled host.

**It is a supply-chain change, not an isolation one.** Nothing about what a
backend may do once it is running is different — see the security notes on
`plugin.Loader`, which are unchanged. What it removes is the trust needed to get
there: `DRIVEL_PLUGIN_PATH` and the five-directory search path (for a shipped
backend), shadowed kinds, and `safeToRun`'s check-then-exec window, which cannot
be closed while a plugin is named by a path and is closed by construction when
the image is `/proc/self/exe`.

**Precedence is bundled-wins**, and the loader logs the file it ignored. A kind
the binary does not carry still comes from the search path exactly as before,
which is what keeps an out-of-tree backend working, so `provider` stays public
for the reason it was made public.

Two things hold the pair of paths in place:

- **`make build-nobundle` is a `check` gate** (and a CI step). It compiles the
  host with no backend linked in, because a build nobody performs is a build that
  stops working — and the unbundled shape is the one a distribution packaging each
  backend separately needs.
- **`internal/app/seam_test.go` asserts the import graph.** Until M23 "nothing
  above the seam depends on a concrete provider" was readable off the host binary
  having linked no backend. The property is unchanged in the source and the
  inference is gone, so it is now a test over `go list -deps` — which is the
  better statement of the rule anyway. §2.9's cross-compile proof is unaffected:
  it is a claim about the tree, and both backends plus `plugin` still build clean
  for `windows/amd64`, where only go-fuse fails.

The `cmd/drivel-provider-*` targets stay for two reasons that are not symmetric:
they are how an out-of-tree backend is built at all, and
`plugin/testdata/drivel-provider-fake` is a real on-disk executable, so the suite
exercises both launch paths rather than letting the external one rot.

One hidden subcommand comes with it. `drivel plugin-serve <kind>` is how the host
re-enters itself as a backend, and it is deliberately absent from `usage()`,
`drivel help` and the completions — `cmd/drivel/bundle_test.go` asserts that
absence, so it stays deliberate. Running it by hand is inert: go-plugin refuses
to serve without the handshake cookie in the environment.

## Shell completions are printed by the binary

Nothing is committed and nothing is generated at build time. `drivel completion
bash` and `drivel completion zsh` render the script from the same `flag.FlagSet`s
the program parses, on demand:

```sh
go run ./cmd/drivel completion bash
```

That is what makes a downloaded single binary able to install its own completion,
with no repository, no Makefile and no Go toolchain behind it — which is also why
the renderer is no longer behind a build tag. `make install` runs the binary it
just built rather than copying a checked-in file, placing the two scripts under
`$PREFIX/share/bash-completion/completions` and `$PREFIX/share/zsh/site-functions`;
cross-compiling and then installing therefore needs an emulator or a second
native build.

The renderers are in `internal/completion`; the flag→completion table is
`completionHints` in [cmd/drivel/completions.go](../../cmd/drivel/completions.go).
There is no `completions-check` target: what it guarded is now
`cmd/drivel/completions_test.go`, because the failure it catches — a flag with no
hint, a hint for a flag that has gone — is in the program rather than in a file
that could drift from it.

Adding a flag therefore means adding one line to `completionHints` naming what
its value looks like (a file, a directory, one of a fixed set of words, or
opaque). Forgetting is not possible in the quiet way it used to be: the generator
fails on a flag with no hint *and* on a hint for a flag that no longer exists, and
`make check` runs it. Enum values come from the program's own constants
(`gdconf.SweepModes`, `gdconf.DeleteModes`), so a mode that is renamed — or added,
or withdrawn — cannot leave the completions offering a word the backend refuses.
That leaf package exists for this: since M9 the Drive backend is a separate
executable, and `gdconf` is how the `drivel` binary keeps the vocabulary of the
`-drive-*` flags without linking the Drive SDK.

## The plugin protocol is generated too

Backends run in their own process (M9) and speak gRPC, so `plugin/internal/pb` is
generated from [proto/drivel/plugin/v1/provider.proto](../../proto/drivel/plugin/v1/provider.proto):

```sh
make proto        # regenerate after editing the .proto
make proto-check  # what `make check` runs: fails if the committed output is stale
```

The toolchain installs itself into `bin/` on first use and is pinned in the
Makefile. It is **pure Go** — `buf` is the compiler as well as the driver — so
there is no `protoc` and no C++ anywhere in the build, which is the same
constraint that keeps cgo out. The generated files are committed, because a
tarball build has no protobuf toolchain — and unlike the completions above, there
is nothing the finished binary could render them from.

Changing the protocol is a compatibility decision. Adding a field does not bump
`plugin.ProtocolVersion`, because protobuf is already compatible in both
directions and a capability name the host does not recognise is dropped with a log
line rather than refused. Removing a field, or changing what one means, does — a
host refuses to launch a plugin whose version does not match exactly, and the
alternative (a compatible range) fails silently, with an almost-compatible plugin
answering most calls correctly and losing a sentinel error somewhere in the
middle.

## Validating the mermaid diagrams

`docs/dev/` carries fourteen mermaid diagrams, and the rule is that they are
**checked by parsing them, not by eye** — a diagram with a syntax error renders
as an error box on the website and as nothing at all on GitHub, and neither is
visible in a diff.

There is no `make` target for it, deliberately: the parser is JavaScript, and
wiring npm into the gates would put a second toolchain in front of every commit
to buy a check that matters only when a diagram changes. Run it when you touch
one:

```sh
mkdir -p /tmp/mmd && cd /tmp/mmd && npm install mermaid jsdom
```

```js
// check.mjs — extract every ```mermaid block from docs/ and parse it
import { JSDOM } from 'jsdom';
const dom = new JSDOM('<!doctype html><html><body></body></html>');
global.window = dom.window; global.document = dom.window.document;
Object.defineProperty(global, 'navigator',
  { value: dom.window.navigator, configurable: true });
const mermaid = (await import('mermaid')).default;
await mermaid.parse(yourDiagramSource);   // throws on a syntax error
```

The DOM shim is not optional: mermaid sanitises through DOMPurify, which fails
with `DOMPurify.addHook is not a function` in a bare Node process — an error
about the harness, not about the diagram, and easy to mistake for one.

## Upgrading the Go toolchain

Three things move together, and the Makefile enforces the first:

1. **The tool binaries in `bin/`** are stamped with the Go version that built
   them, because a source-processing tool built by an older toolchain cannot
   parse a newer one's sources — it fails with `file requires newer Go version`
   rather than with a finding. Changing toolchains rebuilds them automatically.
2. **The `go` directive in `go.mod`** is what CI installs. Bump it, or CI
   silently keeps building and scanning with the old toolchain.
3. **`make vuln`**, which is usually the reason to upgrade in the first place:
   most of what it reports is stdlib, and a toolchain bump clears it in one move.

`lefthook` is not in `bin/` and is not one of these: it comes from the OS
package manager (see [git hooks](#git-hooks)), so a toolchain bump has nothing to
do with it.

## Lint

`make lint` runs [golangci-lint](https://golangci-lint.run) with the suite
configured in [.golangci.yml](../../.golangci.yml). The selection principle stated
there is worth repeating: enable linters that find *bugs*, not linters that find
opinions. A rule that fires constantly on correct code gets the whole tool
switched off, so the checks that are structurally inapplicable to a filesystem —
`gosec`'s file-permission and variable-path rules, `govet`'s `shadow` — are
excluded with a reason rather than endured.

The tree reports clean, and `make lint` is the blocking gate in both the
pre-push hook and CI. Keep it that way: a backlog is far more expensive to pay
down a second time than to never accumulate.

Where a finding is deliberate, suppress it *narrowly and with a reason* rather
than disabling the linter — `//nolint:gosec // G401: dictated by Drive's
md5Checksum, not a security property`. Those comments are the useful output of
the exercise; several of them document decisions that were previously only
implicit, including one conversion that looks redundant on Linux and is required
on darwin.

`make lint-new` reports only what the current branch introduced, which is handy
when triaging a large branch in isolation:

```sh
make lint-new                        # vs. the merge base with master
make lint-new MAIN_BRANCH=origin/master
```

## Git hooks

```sh
make hooks     # once per clone
```

Installs [lefthook](https://lefthook.dev)'s hooks from
[lefthook.yml](../../lefthook.yml). **lefthook itself comes from your OS package
manager** — `make tools` does not build it, and `make hooks` fails with an
install pointer rather than fetching one. It is a hook runner rather than a
source-processing tool, so no gate depends on its version and there is nothing
to keep in step with the Go toolchain.

[`.lefthook-rc.sh`](../../.lefthook-rc.sh) is checked in and sourced by the
generated hook scripts, and it is not optional: lefthook's own search covers
PATH, `node_modules`, bundler and half a dozen other package managers, and when
it finds none of them it prints "Can't find lefthook in PATH" and **exits 0** —
a hook that silently passes. The rc script makes that case a failure instead.
Split by cost, deliberately: **pre-commit** stays under a few seconds
(`fmt-check`, `vet`, `build`) because a hook slow enough to be annoying gets
bypassed with `--no-verify`, and a hook everyone bypasses is worse than no hook —
it manufactures false confidence.

The expensive gates run at push time instead: `make lint` over the whole tree
and the race suite.

pre-push runs `make test`, not `make test-full`: `/dev/fuse` and user xattrs are
a CI guarantee, not a laptop one, so requiring them here would fail pushes from
machines where skipping is the correct behaviour.

## CI

[.github/workflows/ci.yml](../../.github/workflows/ci.yml) is a thin scheduler
around the same make targets. Three jobs, all blocking: `static` (tidy, format,
vet, build, lint), `test` (`test-full` on a runner with `fuse3` installed), and
`govulncheck`.

CI takes its Go version from the `go` directive in `go.mod`, so that directive —
not whatever is on your machine — is what CI builds and scans with. Bump it when
you upgrade, or CI keeps testing the toolchain you left behind.

The `test` job asserts `/dev/fuse`, `fusermount3` and working `user.*` xattrs
*before* running the suite. Without that assertion a runner missing any of them
reports green while the tests guarding M5's data-loss invariants never execute —
the same failure `DRIVEL_REQUIRE_TESTENV` exists to prevent, one layer out.

Run the workflow locally with [act](https://github.com/nektos/act). The `test`
job mounts a real filesystem, so it needs a privileged container:

```sh
act --privileged                 # whole workflow
act --privileged -j test         # one job
```

Weekly `schedule:` runs exist for `govulncheck`: new CVEs land against unchanged
code, so that job needs a heartbeat independent of commits.

## Running the binary

```sh
# `make build` builds one binary carrying every backend, so `go build
# ./cmd/drivel` is equivalent and nothing needs configuring to find a provider.
# `make build-plugins` additionally writes the backends into bin/, which is first
# on the plugin search path — useful for exercising the installed launch path,
# though a bundled kind wins over a file of the same name and says so in the log.

# Separate backing dir: operate on ./mnt; changes land in ./data and log as events.
./bin/drivel mount -mount ./mnt -data ./data

# In-place (Linux): ./dir is its own backing store; files stay put on exit.
./bin/drivel mount -mount ./dir

# With Drive sync, after `drivel login`:
./bin/drivel mount -mount ./mnt -data ./data \
  -credentials credentials.json -token token.json -drive-root <folderID>
```

`./mnt` and `./data` are gitignored scratch dirs. Operate on the mount; watch the
log for sync events. Ctrl-C unmounts (a second Ctrl-C hard-exits).

Without `-credentials`, Drivel runs **log-only**: no auth, no network, just prints
the change events it *would* sync. This is the fastest way to exercise the FUSE
and event layers.

## Environment setup

- **FUSE helper.** Mounting needs `fusermount3` from the system **`fuse3`**
  package (`libfuse3-dev` is headers only). Install: `sudo apt install fuse3`
  (Debian) or `sudo dnf install fuse3` (Fedora). This repo's container is
  privileged with `/dev/fuse` present, so unprivileged mounts work once `fuse3` is
  installed.
- **Stale mount** after a crash: `fusermount3 -u ./mnt`.
- **HTTP/3 UDP buffer.** quic-go warns if the UDP receive buffer is small; raise
  it when testing sync: `sudo sysctl -w net.core.rmem_max=7500000` (and
  `wmem_max`).

## Debugging

- **`-debug`** on `mount` turns on go-fuse's FUSE-level tracing — every VFS call
  in and out. Verbose, but the fastest way to see what the kernel is asking for.
- **`-pprof localhost:6060`** on `mount` serves Go's profiling endpoints for the
  whole process: `go tool pprof http://localhost:6060/debug/pprof/heap` for what
  is retaining memory, `.../goroutine?debug=1` for a leak (a count that climbs
  over a long run is the signature), `.../profile?seconds=30` for CPU. Off unless
  an address is given — it serves the heap, which holds synced file paths and
  contents, to anyone who can reach it. A non-loopback bind is **refused** and
  needs `-pprof-allow-remote`; a bare `-pprof 6060` means loopback; a port it
  cannot bind is a startup error, so a run you started in order to measure never
  quietly produces nothing. `/debug/pprof/cmdline` is not served — argv names the
  credentials file, the token, the backing tree and the account.
- **Log-only mode** (omit `-credentials`) isolates the FS/event layers from sync:
  if a bug reproduces here, it's not in the provider or network path.
- **Sync bugs** are usually echo/loop-suppression (DESIGN.md §4). Inspect the
  state DB — it's bbolt — to see the cursor and echo records. Reproduce inbound
  behaviour by editing a file directly in Drive and watching the downloader poll.
- **Transport.** HTTP/3 is preferred with an HTTP/2 fallback; a QUIC/UDP dial
  failure silently falls back. If you suspect the transport, force the failure
  path or add logging in `internal/transport` — don't assume you're on H3.
- **Deadlock on a mount** almost always means something read the backing store via
  the mountpoint path in in-place mode (the cardinal rule above). Check that every
  backing access goes through `backing.Path`.
- **Stuck/renamed test mounts:** `fusermount3 -u <dir>` before retrying.

