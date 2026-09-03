# Drivel

A Go FUSE filesystem that mounts a local directory as an **interceptor**: every
operation is proxied to an underlying directory (the source of truth / local cache)
and asynchronously, bidirectionally synced with a cloud-storage provider. First
provider: **Google Drive**.

See [DESIGN.md](DESIGN.md) for the architecture, the sync model, and the
echo/loop-suppression strategy.

> The `-gpu` in the repo name is historical; GPU-accelerated deduplication is out
> of scope for v1.

Drivel is a **client you run yourself**. It has no hosted service, no shared
application identity, and **no analytics or telemetry** — nothing phones home.
You bring your own Google Cloud OAuth credentials (see
[Set up your own Google API credentials](#set-up-your-own-google-api-credentials)),
so all Drive traffic goes directly between your machine and Google under an app
identity you control. Licensed under the [GNU AGPLv3](#license).

## Status

**v1 is feature-complete** (M1–M4): Drivel mounts, proxies, and syncs
bidirectionally with Google Drive. On top of it, **M5 (lazy hydration)** has
landed as an opt-in mode, and **M6 (smarter uploads)**, **M7 (restart-safe path
resolution)** and **M7b (initial enumeration & reconcile)** as always-on
behaviour.

- `internal/vfs` — go-fuse loopback that proxies to the underlying dir and emits a
  change `Event` per mutation, including `OpWrite` on close (file-handle capture).
- `internal/mount` — the mount-backend seam and in-place mode (Linux).
- `internal/transport` — HTTP/3 (QUIC) client with HTTP/2 fallback (unit-tested).
- `internal/provider` + `internal/provider/gdrive` — the cloud interface and its
  Google Drive implementation (OAuth desktop flow, transport folded under auth).
- `internal/state` — bbolt store for the change-feed cursor and echo records.
- `internal/pathindex` — bbolt path↔ID index for ID-addressed providers (M7); a
  verified cache, never authoritative.
- `internal/hydrate` — M5 lazy hydration: placeholders and hydrate-on-read.
- `internal/ranges` — the byte-extent bitmap shared by M5's present-ranges and
  M6's dirty-ranges.
- `internal/syncengine` — outbound push (per-path debounce, path-hashed worker
  pool, retry with backoff) and the inbound `changes.list` pull loop (echo
  suppression, adaptive cadence, conflict copies); log-only without credentials.

### Lazy hydration (`-lazy`)

    drivel mount -mount ./mnt -data ./data -credentials creds.json -lazy

Remote files appear immediately with their real name, size, and mtime but occupy
no space; content is fetched the first time something reads (or partially writes)
the file. A directory listing costs nothing. Opt-in — without `-lazy` the backing
directory holds full content, exactly as before.

### Smarter uploads (M6)

Always on, no flag. When a large file changes and the provider supports writing
byte ranges, only the changed extents go out. Otherwise, before re-uploading,
Drivel checks whether the bytes actually differ from what the remote holds — if
not, nothing is sent at all, which covers an editor rewriting an identical buffer
or a rebuild producing the same artifact. Small files skip both checks and upload
as they always did, since the round-trip to check costs about what the upload
would.

Google Drive is not such a provider: its API replaces file content wholesale, with
no way to patch a range, so on Drive a genuine edit to a large file is still a full
re-upload. What Drivel does there is make that upload survivable — chunked
resumable sessions with per-chunk timeouts, retries, and a checksum Drive verifies
before committing. See [DESIGN.md §9](DESIGN.md) for the full reasoning.

### Restart-safe path resolution (M7)

Always on. Google Drive addresses files by opaque ID, not by path, so Drivel keeps
a path↔ID index — now persisted (`-index`, default `drivel-index.db`) so a restart
starts warm instead of relearning every mapping through the API.

The index is a **cache, never an authority**. A stored mapping is checked against
Drive before it is used — the object may have been moved, renamed or deleted while
Drivel was not running — and dropped if it no longer matches. Anything unknown or
rejected is looked up by name against Drive itself, so a deleted index rebuilds on
demand: losing the file costs API round trips and nothing else.

That lookup also closes a gap that predates the index. Previously a path Drivel had
not yet learned counted as "not on Drive", so a restart followed by an edit uploaded
a **duplicate** file beside the real one instead of replacing it, skipped M6's
already-uploaded check, and left `-lazy` placeholders from an earlier session
unreadable.

### Seeing a Drive that was already there (M7b)

Always on. The change feed only reports what changes *after* Drivel first runs, so
a Drive full of existing files used to be invisible to it. Now the first run (and
`-resync`, and a recovery from an expired cursor) enumerates the whole remote tree
once — one request per thousand objects, no content transferred — and reconciles it
against the backing directory.

With `-lazy` the entire Drive becomes visible immediately as placeholders. Without
it, materialising means downloading, so that stays behind `-materialize`.

Reconciling means deciding what changed on each side while Drivel was not running,
and the dangerous half of that is deletion. Drivel infers a delete **only from a
baseline** — a record that it previously synced that exact path — never from a file
being missing on one side. A path it has never synced is *new*, whichever side it
is on, which is why **the first run never deletes anything**. On top of that: a
local file that changed since the baseline is kept and pushed back rather than
deleted; a directory is only removed once it is empty; and `-max-deletes` (default
100) abandons the whole delete pass if the count looks like a broken setup rather
than a real cleanup — a state database pointed at the wrong Drive folder, say, or a
fresh empty backing directory.

The same machinery fixes a silent failure: Google expires change cursors, and
Drivel used to retry a dead one forever with inbound sync quietly stopped. It now
recognises the expiry and recovers by re-enumerating.

Roadmap (v2), in [DESIGN.md §9](DESIGN.md): **M8** multi-account and
multi-provider mounts · **M9** plugin architecture for third-party providers.

## Build

```sh
go build ./...
go build -o ./bin/drivel ./cmd/drivel
```

## Set up your own Google API credentials

Drivel does **not** ship with a shared Google application identity, and there is no
hosted Drivel service. You create your own OAuth **client ID and secret** in a Google
Cloud project that you own, and Drivel uses them to talk to your Drive on your behalf.
You are responsible for these credentials — treat them as secrets, don't commit them
(they are gitignored), and revoke them from the Cloud Console if they leak.

This is a one-time setup (~5 minutes). It's free; the Drive API has generous
per-project quotas for personal use.

1. **Create (or pick) a Google Cloud project.**
   Go to the [Cloud Console](https://console.cloud.google.com/), open the project
   picker in the top bar, and click **New Project**. Name it anything (e.g. `drivel`)
   and create it, then make sure it's the selected project.

2. **Enable the Drive API.**
   In [APIs & Services → Library](https://console.cloud.google.com/apis/library),
   search for **Google Drive API** and click **Enable**.

3. **Configure the OAuth consent screen.**
   Under [APIs & Services → OAuth consent screen](https://console.cloud.google.com/apis/credentials/consent):
   - Choose **External** user type (unless you have a Workspace org, in which case
     **Internal** avoids the verification prompts below).
   - Fill in the required app name and your own email for the support/developer
     contact fields. You do **not** need a homepage, privacy policy, or an app domain
     — leave those blank. Nobody but you uses this app.
   - On the **Scopes** step you can skip adding scopes here; Drivel requests them at
     login time.
   - On **Test users**, add the Google account(s) you'll sync. Keeping the app in
     **Testing** status is fine for personal use — you never have to submit it for
     Google verification. (Refresh tokens for unverified test apps can expire after
     ~7 days; just re-run `drivel login` if that happens.)

4. **Create an OAuth client ID.**
   Under [APIs & Services → Credentials](https://console.cloud.google.com/apis/credentials),
   click **Create Credentials → OAuth client ID**, choose application type
   **Desktop app**, name it, and create. Copy the **Client ID** and **Client secret**
   from the dialog (or download the JSON) — you'll paste them into `drivel login`.

That's it. The client ID/secret identify *your* project to Google; the login wizard
below exchanges them for a token scoped to your account.

## Authenticate (rclone-style wizard)

```sh
./bin/drivel login
```

The wizard prompts for the OAuth **client ID/secret** you created
[above](#set-up-your-own-google-api-credentials), a **scope** (full / readonly /
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
# Separate backing dir: operate on ./mnt; changes land in ./data and log as events.
./bin/drivel mount -mount ./mnt -data ./data

# In-place (Linux): the directory is its own backing store, so files just stay put
# in ./dir when Drivel exits — no separate data dir. Omit -data to select it.
./bin/drivel mount -mount ./dir

# With Google Drive sync (push), after `drivel login`:
./bin/drivel mount -mount ./mnt -data ./data \
  -credentials credentials.json -token token.json -drive-root <folderID>
# Ctrl-C to unmount.
```

**In-place mode** mounts a directory onto itself: a directory handle opened before
mounting lets Drivel reach the underlying files (via `/proc/self/fd/N`) while the
FUSE overlay is active, so nothing is copied and the files remain in place after
unmount. Linux-only for now; elsewhere use a separate `-data` dir.

`-drive-root` is the Drive folder ID the mount root maps to (`root` for My Drive).
The `mount` subcommand is the default, so the older `drivel -mount … -data …` form
still works.

**Requires the FUSE mount helper** (`fusermount3`), which ships with the system
`fuse3` package. If you see `exec: "/bin/fusermount": no such file`, install it:

```sh
# Fedora
sudo dnf install fuse3
# Debian/Ubuntu
sudo apt install fuse3
```

## License

Drivel is free software licensed under the **GNU Affero General Public License,
version 3** — see [LICENSE](LICENSE). In short: you may use, modify, and redistribute
it, but derivative works — **including software you offer to others over a network** —
must be made available under the same license. There is no CLA and no separate
proprietary/commercial edition.

Drivel bundles no application secrets and collects **no analytics or telemetry**. The
Google API credentials you supply are yours; how you use Google Drive through them is
governed by Google's own terms, not by this project.
