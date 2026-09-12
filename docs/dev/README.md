# Developing Drivel

Drivel is a Go FUSE filesystem that mounts a local directory as an
**interceptor**: every operation is proxied to a backing directory (the source of
truth) and asynchronously, bidirectionally synced with a cloud provider. Google
Drive is the first provider, via the Drive API's `changes.list` cursor feed — not
webhooks, not the Workspace Events API.

## Reading order

**New here?** [Architecture](architecture.md) for what talks to what, then
[Workflows](workflows.md) for how a write and a remote change actually travel.
Fifteen minutes gets you oriented.

**About to change sync?** [Conventions](conventions.md) first. Echo suppression is
the load-bearing correctness concern in this codebase and it is easy to regress
without any test going red.

**Writing a provider?** [Writing a provider](new-provider.md). You should not need
to touch anything outside your own package.

**Just want it to build?** [Building](building.md) and [Testing](testing.md).

| | |
| --- | --- |
| [Architecture](architecture.md) | Runtime components, package dependency graph, and what the seams enforce. |
| [Workflows](workflows.md) | Sequence and decision diagrams: outbound push, inbound pull, the sweep, hydration, shutdown. |
| [Storage schema](schema.md) | The two bbolt databases, bucket by bucket, and the authority ladder. |
| [Dependencies](dependencies.md) | Every direct dependency, why it is there, what was rejected, and what a change would cost. |
| [Conventions](conventions.md) | The rules that carry correctness — read before touching sync, hydration or uploads. |
| [Building](building.md) | Make targets, toolchain, lint, hooks, CI, running and debugging. |
| [Testing](testing.md) | The suite, the kernel facilities it needs, platform runs, and the property tests. |
| [Writing a provider](new-provider.md) | `provider.Store` and its optional interfaces, with a checklist. |
| [Glossary](glossary.md) | Echo, baseline, sweep, placeholder, seam, generation. |
| [Multi-client test plan](multiclient-test-plan.md) | The fleet-convergence campaign and its rig. |

## The two documents that are not here

`DESIGN.md` and `CLAUDE.md`, both at the repo root, are **internal working
documents** rather than published documentation. `DESIGN.md` is the long-form
architectural record — every decision, its alternatives, and why they lost — and
the pages here cite it by section (`DESIGN.md §4`) when you need the argument
behind a rule rather than the rule. `CLAUDE.md` is the conventions file the
repo's agents and contributors work from.

Neither is maintained to the standard of the two published collections. Read them
for reasoning, not for interface documentation.

## Layout

| Package | Role |
| --- | --- |
| `cmd/drivel` | Entry point: flags, the flag→spec mapping, signal context. |
| `internal/app` | The composition root below `main`: one mount's lifecycle, N of them per process, and the cross-mount guards. |
| `internal/config` | The TOML config file. Knows nothing about any provider. |
| `internal/mount` | The mount-backend seam: `Backend`, `Options`, `ResolveBacking`. |
| `internal/vfs` | The go-fuse mount backend — loopback proxy that emits one event per mutation. |
| `internal/fsevent` | Backend-neutral change `Event` / `Op` types. |
| `provider` | **Public.** The backend seam: `Store`, the optional capabilities, the registry, and capability negotiation. |
| `ranges` | **Public.** Leaf value package: the block bitmap. No I/O. Public because `RangePutter` names it. |
| `plugin` | **Public.** Loading a backend that runs in its own process: discovery, the host-side proxy, and `Serve`. |
| `cmd/drivel-provider-*` | The backends, each a three-line `main` around `plugin.Serve`. Not commands a user runs. |
| `internal/provider/gdrive` | Google Drive. The only package that knows what a file ID is. |
| `internal/provider/gdrive/gdconf` | Drive's settings table and its two enumerated types — the vocabulary, without the SDK. |
| `internal/provider/sftp` | SFTP. Host key verification fails closed. |
| `internal/gauth` | Google OAuth: credential I/O and the interactive login flow. |
| `internal/transport` | HTTP/3 (QUIC) client with HTTP/2 fallback. |
| `internal/syncengine` | Outbound push (`Engine`), inbound pull (`Downloader`), and the enumeration sweep (`reconcile.go`). |
| `internal/state` | Engine-level bbolt state: cursor, echo records, sweep marks. |
| `internal/pathindex` | Provider-private bbolt path↔ID cache. |
| `internal/hydrate` | Lazy hydration: placeholders, the xattr marker, fault-in on first I/O. |
| `internal/testenv` | Test-only: turns a silently-skipped kernel facility into a failure in CI. |
