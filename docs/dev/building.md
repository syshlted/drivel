# Building and running

Every gate has a make target, and the Makefile is the single definition of each
one: the git hooks and CI both call these targets, so "it passed locally" and "it
passed in CI" cannot mean different things.

```sh
make help         # every target, plus worked examples — the default goal
make build        # the CLI binary, into bin/
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

## Shell completions are generated

`completions/drivel.bash` and `completions/_drivel` are **not written by hand**.
They are rendered from the same `flag.FlagSet`s the program parses:

```sh
make completions        # rewrite them after adding or renaming a flag
make completions-check  # what `make check` runs: fails if they have drifted
```

The renderers are in `internal/completion`; the flag→completion table is
`completionHints` in [cmd/drivel/completions.go](../../cmd/drivel/completions.go),
which is compiled **only under the `completions` build tag** — a released binary
has no reason to carry a shell-script renderer, and that file is the only thing
that imports `internal/completion`.

Adding a flag therefore means adding one line to `completionHints` naming what
its value looks like (a file, a directory, one of a fixed set of words, or
opaque). Forgetting is not possible in the quiet way it used to be: the generator
fails on a flag with no hint *and* on a hint for a flag that no longer exists, and
`make check` runs it. Enum values come from the program's own constants
(`gdrive.SweepAuto`, `gdrive.DeleteTrash`, …), so a mode that is renamed cannot
leave the completions offering a word the binary refuses.

The generated files are committed because whoever installs from a tarball or a
distro package has no Go toolchain to run the generator with; `make install`
places them under `$PREFIX/share/bash-completion/completions` and
`$PREFIX/share/zsh/site-functions`.

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

The one exception to (1) is `lefthook`, whose build toolchain is pinned
separately (`LEFTHOOK_GOTOOLCHAIN`) because it does not compile on Go 1.27. That
pin is safe precisely because lefthook never parses Go source — it only shells
out to make targets. Drop it when a lefthook release builds on current Go.

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
make hooks     # once per clone, and again after bumping a tool version
```

`make hooks` also writes `.lefthook-rc.sh` (gitignored), which points
`LEFTHOOK_BIN` at the pinned binary in `bin/`. It is not optional: without it the
generated hook searches PATH, `node_modules`, bundler and half a dozen other
package managers, and when it finds none of them it prints "Can't find lefthook
in PATH" and **exits 0** — a hook that silently passes.

Installs [lefthook](https://lefthook.dev) from [lefthook.yml](../../lefthook.yml).
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

