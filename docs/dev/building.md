# Building and running

Every gate has a make target, and the Makefile is the single definition of each
one: the git hooks and CI both call these targets, so "it passed locally" and "it
passed in CI" cannot mean different things.

```sh
make help         # every target, with a description
make build        # the CLI binary, into bin/
make test         # go test -race ./...
make lint         # golangci-lint over the whole tree
make fmt          # apply formatting
make check        # everything CI runs, in CI's order
make hooks        # install the git hooks (once per clone)
```

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
  contents, to anyone who can reach it. A non-loopback bind is warned about; a
  port it cannot bind is a startup error, so a run you started in order to
  measure never quietly produces nothing.
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

