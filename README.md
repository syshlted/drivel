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

Milestones **M1 (passthrough mount)** and **M2 (transport + Drive push)** are done:

- `internal/vfs` — go-fuse loopback that proxies to the underlying dir and emits a
  change `Event` per mutation, including `OpWrite` on close (file-handle capture).
- `internal/transport` — HTTP/3 (QUIC) client with HTTP/2 fallback (unit-tested).
- `internal/provider` + `internal/provider/gdrive` — the cloud interface and its
  Google Drive implementation (OAuth desktop flow, transport folded under auth).
- `internal/syncengine` — pushes local changes to the provider (event→API mapping
  unit-tested); log-only when no credentials are given.

Roadmap: M3 `changes.list` pull loop + persistent state store + echo suppression ·
M4 full bidirectional (debounce, retries, conflict copies, clean shutdown).

## Build

```sh
go build ./...
go build -o ./bin/dedupfs ./cmd/dedupfs
```

## Authenticate (rclone-style wizard)

```sh
./bin/dedupfs login
```

The wizard prompts for your OAuth **client ID/secret** (from a Google Cloud
*Desktop app* credential with the Drive API enabled), a **scope** (full / readonly /
drive.file), and an optional **root folder ID**. It writes `credentials.json`, prints
an authorization URL to open in your browser, and captures the result two ways:

- **Auto-capture** — a loopback server on port **53682** receives the browser
  redirect. In a container, forward the port: `docker run -p 127.0.0.1:53682:53682 …`.
- **Paste fallback** — if the browser can't reach that address, paste the full
  redirect URL (or just the code) from the address bar into the prompt.

It then saves `token.json` and confirms the signed-in account. Non-interactive flags
(`-client-id`, `-client-secret`, `-scope`, `-port`, `-open`) are available too.

## Run

```sh
# Log-only (no cloud): operate on ./mnt; changes land in ./data and log as events.
./bin/dedupfs mount -mount ./mnt -data ./data

# With Google Drive sync (push), after `dedupfs login`:
./bin/dedupfs mount -mount ./mnt -data ./data \
  -credentials credentials.json -token token.json -drive-root <folderID>
# Ctrl-C to unmount.
```

`-drive-root` is the Drive folder ID the mount root maps to (`root` for My Drive).
The `mount` subcommand is the default, so the older `dedupfs -mount … -data …` form
still works.

**Requires the FUSE mount helper** (`fusermount3`), which ships with the system
`fuse3` package. If you see `exec: "/bin/fusermount": no such file`, install it:

```sh
# Fedora
sudo dnf install fuse3
# Debian/Ubuntu
sudo apt install fuse3
```
