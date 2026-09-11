# Dependencies

Drivel has **seven direct dependencies**. The bar for adding an eighth is high:
this is a filesystem that holds the only local copy of someone's data, and every
dependency is code running with access to it.

Two constraints shape the whole list. **The build is pure Go** — no cgo anywhere,
which is what makes `GOOS=darwin go build` work from a Linux box and what keeps
the cross-compile matrix honest. And **nothing may be a process-global**: the
registry is a value, not an `init()`-populated package map, so two independently
configured providers can run in one process.

The first constraint is enforced at build time rather than assumed: `make build`
sets `CGO_ENABLED=0`, because the *standard library* reaches for cgo even when
your code does not. The single exception — distro packaging, where linking the
system's libraries deliberately offloads their security patching to the
distribution — is in [Building](building.md#static-by-default-dynamic-for-packagers).

## Direct

### `github.com/hanwen/go-fuse/v2` — the FUSE binding

The only Go FUSE library that speaks the kernel protocol directly instead of
binding libfuse. That is the entire reason it is here: **libfuse means cgo**, and
cgo would forfeit cross-compilation, complicate every build, and pull a C library
into the process that owns the data.

What it costs:

- **It is the only thing that does not cross-compile.** Every cross-compile
  failure on every target traces to go-fuse and nothing else — which is also the
  strongest available evidence that the seams hold, since everything else builds
  for Windows.
- **It decides which platforms exist.** macOS support means whatever go-fuse
  probes for (`mount_macfuse`, `mount_osxfuse`), which is why FUSE-T does not
  work and is not a build flag away.
- **Its reply path has sharp edges.** It resolves an fd-backed read result when
  it *writes* the reply, so a `-debug` trace can log a read as `OK` and still
  fail the operation. This is how the FreeBSD `O_WRONLY` bug hid.

Replacing it would mean writing a FUSE protocol implementation. Adding a second
mount backend beside it is the supported path — that is what `mount.Backend` is.

### `github.com/quic-go/quic-go` — HTTP/3

HTTP/3 over QUIC is a project decision (DESIGN.md §2.6), and Go's standard library
has no HTTP/3 client, so `http3.Transport` from quic-go is the only option. Drivel
is HTTP/3-*preferred*, not HTTP/3-only: a failed QUIC/UDP dial falls back to
HTTP/2 automatically, so the dependency is never a single point of failure for
connectivity.

It sits **below** OAuth — the client is passed via `option.WithHTTPClient` with
the token source folded into `oauth2.Transport{Base: …}`. Passing
`WithTokenSource` as well conflicts; do not.

The one operational wart: quic-go warns when the UDP receive buffer is small.
That is a `sysctl`, not a bug.

### `go.etcd.io/bbolt` — embedded key/value store

Two databases need to survive restarts: the sync state and the path index. bbolt
is chosen for what it *is not* — no server, no cgo, no schema migration story, no
background compaction. A single mmap'd B+tree file with one writer and MVCC
readers, which is exactly the access pattern here: the uploader, downloader and
reconciler all write, and bbolt serialises them so the code needs no locking of
its own.

**SQLite was not chosen** because every Go binding is either cgo (breaking the
pure-Go build) or a reimplementation large enough to be its own risk, and nothing
in either database needs a query language — every access is a key lookup or a
prefix scan.

The design leans on two bbolt properties, so they are not incidental: keys sort
bytewise, which is what makes a directory's children a contiguous run and turns
subtree operations into a seek-plus-walk; and a zero-length key is rejected,
which is why index keys carry a leading slash. See [the schema](schema.md).

### `google.golang.org/api` — the Drive client

The generated Drive v3 client. Used for `files.*` and `changes.list`, and it is
what supplies chunked resumable uploads (`mediaOptions`). Confined entirely to
`internal/provider/gdrive` — nothing above the provider seam imports it, which is
what makes a non-Drive provider a matter of writing one package.

Two of its behaviours are load-bearing and easy to misconfigure:
`ChunkTransferTimeout` is deliberately left unset (it is a hard per-attempt
deadline that never resets on progress, so any value caps the slowest link that
can ever finish a chunk), and resumable session URIs are not persisted — the
feature survives a flaky network, not a restart.

It brings the largest indirect tail in the module (gRPC, OpenTelemetry,
protobuf), which is the honest cost of a generated Google client.

### `golang.org/x/oauth2` — token handling

The desktop OAuth flow and automatic token refresh. Google's own client would
pull it in regardless. Drivel folds it into the transport rather than letting the
API client manage it, so that HTTP/3 sits underneath the token layer.

### `golang.org/x/sys` — raw syscalls

Extended attributes, and the platform bits the standard library does not expose.
`hydrate/xattr_unix.go` is one body shared by Linux and macOS over `unix.*`;
FreeBSD gets its own file because `extattr_*` takes the namespace as an argument.

On FreeBSD this package's typing is a trap worth knowing: it types the extattr
buffer as a `uintptr`, which neither keeps the array alive nor survives a stack
copy. The `//go:uintptrescapes` wrappers are what make it correct, and
`runtime.KeepAlive` does not substitute.

### `github.com/pkg/sftp` and `golang.org/x/crypto` — the SFTP backend

The SSH transport and the file-transfer subsystem on top of it (M18). Both are
pure Go, so §2.9's cross-compile proof survives — an SFTP mount builds for every
target `internal/vfs` does, and for the several it does not.

`pkg/sftp` is used for three things nothing else in the tree needs. `File.WriteAt`
takes an offset, which is what makes `provider.RangePutter` real on this backend
and gives M6's range-write path its first run against a server instead of a fake.
`Client.HasExtension` reports what a *session* advertises, which is how
`posix-rename@openssh.com` is taken when offered and fallen back on when not.
And `NewServer` — the library's server half — is what the tests run against, so
they exercise the real protocol rather than this package's beliefs about it.

`x/crypto/ssh/knownhosts` is load-bearing rather than incidental: host key
verification fails closed, and `ssh.InsecureIgnoreHostKey` appears nowhere in the
tree, with a test that greps for it. An fstab mount at boot has nobody to answer a
trust-on-first-use prompt.

One thing the library does not offer, and the absence is visible in the provider:
there is no API for sending an arbitrary extended request, so the `check-file`
digest extension is unreachable. `sftp` therefore does not implement
`provider.ContentHasher`, which costs nothing — the engine's unchanged-content
gate short-circuits on an empty `Hash` before it reads a local byte.

`github.com/kr/fs` arrives with it, as the walker behind `Client.Walk`. Drivel's
enumeration does its own breadth-first descent (it needs a resumable cursor), so
nothing in the tree calls it.

### `github.com/BurntSushi/toml` — the config file

TOML was chosen over YAML and JSON because the config is **hand-edited and
commented**: JSON has no comments, and YAML's implicit typing is a hazard in a
file where a Drive folder ID is a string that may look like a number.

BurntSushi's decoder is used for one specific capability: `toml.Primitive`, which
holds a provider's settings **undecoded** until the provider itself decodes them.
That is what lets `internal/config` carry Drive configuration without ever
learning what a Drive folder ID is. Its `MetaData.Undecoded` is what makes an
unknown key an error rather than a silent no-op.

The corresponding rule: **the config file is only ever appended to, never
re-serialized.** Any encoder round trip drops every comment, which would defeat
the reason TOML was chosen.

## What is deliberately absent

| Not used | Why |
| --- | --- |
| **A logging library** | The standard `log` package, one logger per mount. Structured logging would be the first dependency whose value is a matter of taste. |
| **A CLI framework** | `flag`, plus a hand-written subcommand switch. Two subcommands do not justify a framework, and the flag→config mapping has to be explicit anyway. |
| **A test framework** | Standard `testing` only. No assertion DSL. |
| **A metrics library** | Counters are plain fields. If a control API lands (M14), it serves a snapshot, not a Prometheus registry. |
| **cgo, anywhere** | It would end the cross-compile matrix and the pure-Go build. This is the constraint that rules out libfuse, WinFsp/cgofuse, and every cgo SQLite. Distro packaging may enable it for the system libc alone; nothing in the tree may *require* it. |
| **A cloud SDK beyond the ones in use** | A new provider brings its own client, inside its own package, below the seam — as `gdrive` and `sftp` each do. |

## Upgrading

`make vuln` (govulncheck) runs weekly in CI, on a schedule independent of commits,
because new CVEs land against unchanged code. Most of what it reports is the
standard library, and a toolchain bump clears it in one move — see [Building →
upgrading the Go toolchain](building.md#upgrading-the-go-toolchain).

The tool binaries in `bin/` are stamped with the Go version that built them and
rebuild automatically when the toolchain changes. `lefthook` is the one exception,
pinned separately, and that pin is safe only because lefthook never parses Go
source — it shells out to make targets.
