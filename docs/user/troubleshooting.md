# Troubleshooting

## Mounting

**`exec: "fusermount3": executable file not found`** — the FUSE helper is not
installed. `sudo apt install fuse3` / `sudo dnf install fuse3`. `libfuse3-dev` is
headers only and does not help. See [Installing](install.md#the-fuse-helper).

**`fusermount3: mount failed: Operation not permitted`** — unprivileged FUSE
mounts are disabled, or `/dev/fuse` is not present. In a container, run it
privileged or pass `--device /dev/fuse`.

**The mountpoint is busy after a crash.**

```sh
fusermount3 -u ~/drive          # Linux
umount -f ~/drive               # macOS, FreeBSD
```

On FreeBSD `mount -t fusefs` will not list it — the type is `fusefs.drivel`.

**`-data` is required on this platform** — you are on macOS or FreeBSD, where
in-place mode does not exist. Pass a separate backing directory.

**A mount hangs and nothing responds.** In in-place mode, something read the
backing store through the mountpoint path, which recurses into Drivel's own
handler. Nothing in a normal setup does this; it usually means a script or backup
tool is pointed at the mounted directory *as if* it were the backing directory.

## Login and credentials

**`403 access_denied`, "app is being tested"** — the account you signed in with is
not in the OAuth client's **Test users** list. Add it in the Cloud Console, or use
an Internal app type if you have Workspace.

**`403 accessNotConfigured` on Drive calls** — the Drive API is not enabled for
the project, or you enabled it in a *different* project than the OAuth client
belongs to.

**`redirect_uri_mismatch`** — the OAuth client is the wrong type. It must be
**Desktop app**, which permits loopback redirects.

**Login worked, then stopped after about a week.** Refresh tokens for apps in
**Testing** status expire after roughly seven days. Re-run `drivel login`. To
avoid it, publish the app (which adds a Google review) or use an Internal
Workspace app.

**The loopback capture never fires** (container, headless, remote box) — forward
the port (`docker run -p 127.0.0.1:53682:53682 …`), or use the paste fallback:
copy the redirect URL out of the browser's address bar into the prompt. `-port 0`
picks a free port if 53682 is taken.

**`drivel mount` asks for nothing and fails on credentials.** That is deliberate:
`mount` is non-interactive and never prompts. Run `drivel login` first.

**A credential leaked.** Delete the OAuth client in the Cloud Console — that
invalidates every token minted from it immediately — and create a new one.

## Syncing

**Nothing syncs and there are no errors.** You are probably in log-only mode: with
no `-credentials`, Drivel mounts and prints the changes it *would* sync without
contacting Google. Check the log for `[sync]` lines that never turn into uploads.

**My existing Drive files are not here.** In eager mode the first sweep does not
download the whole Drive unless you ask: add `-materialize`, or use
[`-lazy`](lazy-mode.md), where the whole tree appears as placeholders for free.

**A file came back after I deleted it.** Most likely [same-name
siblings](data-safety.md#same-name-siblings): the path names more than one Drive
object, deleting removed the visible one, and an older one is now visible. Resolve
the duplicates in the Drive web UI.

**`[sweep] REFUSING to delete: … over the -max-deletes limit`.** Drivel inferred more
deletions than the cap allows and abandoned the entire pass rather than performing
part of it. Check first that the mount is pointed where you think it is — a wrong
`-drive-root`, or an empty `-data`, produces exactly this. If the deletions are
genuine, re-run with **both** `-resync` and a higher `-max-deletes`; the cap alone
changes nothing until the next scheduled sweep.

**Inbound sync stopped silently.** Google expires change cursors. Drivel
recognises the expiry and recovers by re-enumerating; if you suspect it has not,
`-resync` forces the sweep.

**A file is empty that should not be.** If you are running `-lazy`, check that the
backing filesystem supports extended attributes — this is the failure mode that
rule exists to prevent. See [lazy mode](lazy-mode.md#the-one-rule-the-backing-filesystem-must-support-extended-attributes).

**Everything is slow to appear.** Inbound changes arrive by polling, on an
adaptive interval: about every 2 seconds while the feed is active, backing off
toward 30 seconds once it goes quiet. There is no push channel — Drivel follows
Drive's change-feed cursor, not webhooks — so a first change after an idle period
can take up to half a minute to show up.

## Quota and cost

**The sweep is expensive on a large account.** If you mount a folder rather than
the whole Drive, make sure the sweep is descending it rather than listing the
account: `-drive-sweep-mode scoped`. See
[configuration](configuration.md#-drive-sweep-mode).

**Several mounts of one account each poll the whole change feed.** Drive's change
feed has no folder filter, so this is inherent, not a misconfiguration. It costs
quota, not latency.

## Getting more detail

```sh
drivel mount … -debug                  # FUSE-level tracing: every call in and out
drivel mount … -pprof localhost:6060   # Go profiling endpoints
```

`-debug` is extremely verbose but is the fastest way to see what the kernel is
actually asking for. `-pprof` serves the process heap — which holds synced paths
and buffered content — so bind it to loopback only.

If you are filing a bug, the useful attachments are the log around the failure,
the flags or config block, the backing filesystem type (`stat -f -c %T DIR`), and
whether `-lazy` was on.
