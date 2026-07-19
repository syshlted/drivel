# CLAUDE.md

Guidance for Claude Code working in this repository.

## What this is

A Go FUSE filesystem that mounts a local directory as an **interceptor**: every
operation is proxied to an underlying directory (the source of truth / local
cache) and asynchronously, bidirectionally synced with a cloud provider. First
(and currently only) provider: **Google Drive**, via the Drive API v3
`changes.list` cursor feed — **not** the Workspace Events API and **not**
webhooks.

Read [DESIGN.md](DESIGN.md) before making architectural changes. The
echo/loop-suppression model (§4) is the load-bearing correctness concern for
bidirectional sync — don't regress it.

> The `-gpu` in the repo/dir name is historical. GPU-accelerated deduplication
> is **out of scope for v1**; don't add it unless asked.

## Layout

- `cmd/dedupfs` — entrypoint; flag parsing, mount, signal-based unmount.
- `internal/vfs` — go-fuse loopback; proxies to the `-data` dir and emits a
  change `Event` per mutation. Reads/lookups/attrs pass straight through.
- `internal/provider` — cloud-backend interface (the seam). Drive impl is M2.
- `internal/syncengine` — consumes change events; uploader/downloader land in M2–M4.

## Milestones

M1 passthrough mount (done) · M2 Drive auth + push-on-close (incl. file-handle
write capture for `OpWrite`) · M3 `changes.list` pull loop + echo suppression ·
M4 full bidirectional (debounce, retries, conflict copies, clean shutdown).

## Build / test / run

```sh
go build ./...
go vet ./...
go build -o ./bin/dedupfs ./cmd/dedupfs
./bin/dedupfs -mount ./mnt -data ./data   # -debug for FUSE tracing
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
