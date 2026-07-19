# dedupfs

A Go FUSE filesystem that mounts a local directory as an **interceptor**: every
operation is proxied to an underlying directory (the source of truth / local cache)
and asynchronously, bidirectionally synced with a cloud-storage provider. First
provider: **Google Drive**.

See [DESIGN.md](DESIGN.md) for the architecture, the sync model, and the
echo/loop-suppression strategy.

> The `-gpu` in the repo name is historical; GPU-accelerated deduplication is out
> of scope for v1.

## Status

Milestone **M1 — passthrough mount** is scaffolded:

- `internal/vfs` — go-fuse loopback that proxies to the underlying dir and emits a
  change `Event` for each node-level mutation (create/mkdir/rmdir/unlink/rename/setattr).
- `internal/provider` — the cloud-backend interface (Drive impl lands in M2).
- `internal/syncengine` — consumes change events (currently logs; uploader/downloader in M2–M4).

Roadmap: M2 Drive auth + push · M3 `changes.list` pull loop + echo suppression ·
M4 full bidirectional (debounce, retries, conflict copies, clean shutdown).

## Build

```sh
go build ./...
go build -o ./bin/dedupfs ./cmd/dedupfs
```

## Run

```sh
./bin/dedupfs -mount ./mnt -data ./data
# operate on ./mnt; changes land in ./data and are logged as sync events.
# Ctrl-C to unmount.
```

**Requires the FUSE mount helper** (`fusermount3`), which ships with the system
`fuse3` package. If you see `exec: "/bin/fusermount": no such file`, install it:

```sh
# Fedora
sudo dnf install fuse3
# Debian/Ubuntu
sudo apt install fuse3
```
