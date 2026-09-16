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

## Backends

Drivel's storage backends are separate programs it starts for you — see
[Installing](install.md#install-the-command--and-at-least-one-backend).

**`unknown kind: "gdrive" (available: sftp); no drivel-provider-gdrive in …`** —
the backend is not installed. The message lists what *is* available and every
directory Drivel looked in. Install it the same way you installed `drivel`:

```sh
go install github.com/zishmusic/drivel/cmd/drivel-provider-gdrive@latest
```

or, from a checkout, `make build` (which builds all of them) or
`sudo make install`.

**`refusing to run: mode 0777 is writable by everyone`** — the backend is there,
but anyone on the machine could replace it, and it runs with your credentials.
Fix the permissions rather than working around it:

```sh
chmod 755 /usr/local/lib/drivel/plugins/drivel-provider-gdrive
```

The same message naming a *directory* means the directory it sits in is
world-writable. Move the backend somewhere only you (or root) can write.

**`backend exited after …; restarting in …`** — the backend crashed and Drivel is
starting it again. Your mount stays up throughout; pending uploads are retried
once it is back. If it repeats, the lines just above it in the log are the
backend's own output and are where the reason will be.

**Drivel is running a backend you did not expect.** With two installed copies of
the same backend, the first on the search path wins and Drivel logs the other:
`plugin gdrive: using /usr/bin/drivel-provider-gdrive; also found …`. The search
order is: next to the `drivel` binary, then `~/.local/share/drivel/plugins`, then
`/usr/local/lib/drivel/plugins`, then `/usr/lib/drivel/plugins`. Setting
`DRIVEL_PLUGIN_PATH` replaces that list entirely.

**A backend cannot find something it could find when you ran it by hand.** Drivel
gives a backend a deliberately small environment — `PATH`, `HOME`, `TMPDIR`,
locale, TLS roots, proxy settings and `SSH_AUTH_SOCK` — and nothing else. In
particular, ambient cloud credentials (`GOOGLE_APPLICATION_CREDENTIALS` and its
equivalents) are not passed on: a backend authenticates with what its own
configuration names, so that which account a mount uses is a fact about your
config file and not about the shell you started it from. Put the path in the
config.

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

**Copying many files in starts fast, then the filesystem itself slows down.**
Drivel keeps a queue of pending uploads. A bulk copy of many small files can fill
it faster than they upload, and once it is full the filesystem waits rather than
dropping anything — so the copy proceeds at the speed of your uplink. Raising
[`-push-delay`](configuration.md#when-a-change-is-pushed) drains that queue on a
calmer cycle and makes it far less likely; `-upload-workers` does not help, since
the limit is the link rather than the pool. A single large file is unaffected:
Drivel notices it once, when it is closed.

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

**`ln: failed to create hard link: Operation not permitted`.** Deliberate. Drivel
refuses hard links because no cloud store it can talk to represents them, and
permitting one produced two files that silently diverged. Symlinks work; see
[data safety](data-safety.md#hard-links).

**A symlink, FIFO or socket is not appearing on my other machines.** It never
will. Those have no content to upload and no representation on Drive, so they stay
in the backing directory on the machine that made them. Drivel logs a
`skip … no remote representation, stays local` line for each one, from the mount
and from the sweep. On FreeBSD, creating one on the mount fails outright with
`EINVAL` — see [platforms](platforms.md#what-changes-off-linux).

**A file was deleted and you want it back.** Deletions go to the Drive trash, so
look in [drive.google.com](https://drive.google.com/drive/trash) and restore it
there; it comes back down to your machines as an ordinary change. Drive purges the
trash after 30 days, and `-drive-delete permanent` skips it entirely — with that
set there is nothing to recover from. If a *sweep* deleted more than you expected,
read [data safety](data-safety.md#deletion) before re-mounting: the same premise
that produced the first pass will produce the second.

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

**Deleting files does not free space.** Deletions go to the Drive trash by
default, and a trashed file counts against your quota until the trash is emptied.
Empty it from the web UI, or run the mount with `-drive-delete permanent` if
reclaiming space as you delete matters more to you than being able to undo one.

**The same file is uploaded again and again.** Drivel uploads a file once it has
gone [`-push-delay`](configuration.md#when-a-change-is-pushed) without changing,
300ms by default, so a file written repeatedly — an editor autosaving, a log, a
build artefact — is uploaded once per save. Raising the delay coalesces those into
one upload per window. Note that a file which is *appended to* is re-uploaded
whole each time on backends that cannot patch byte ranges, Drive among them, so
the saving here can be large.

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
