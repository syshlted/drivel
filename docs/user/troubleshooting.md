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

**`state db … was unreadable and has been replaced`.** The sync-state database was
damaged — typically a power cut mid-write, or a disk going bad. Drivel kept the old
file with `.corrupt` appended and started with an empty one, which is safe:
**nothing is deleted**, because a deletion is only ever inferred from a record of
what was previously synced and there are now none.

Expect a full re-enumeration of the remote tree, and a conflict copy beside any
file whose local and remote copies differ — with no history, keeping both is the
only honest answer. Lazily hydrated files are unaffected. See
[Data safety](data-safety.md#the-databases-are-caches).

If this recurs, the disk is the suspect, not Drivel. The `.corrupt` file is kept
for inspection and is safe to delete.

**`path index: … was unreadable and has been replaced`.** The same thing, to the
Drive path index, and with nothing to do about it: that file is purely a cache of
which remote object each path refers to, and every entry is re-checked before use.
Expect a few extra lookups while it warms up.

**`open state db …: timeout`.** Another process already has that database open —
almost always a second Drivel still running against the same mount. Check with
`pgrep -a drivel`. Drivel deliberately does *not* treat this as damage; replacing
the file would destroy the running mount's state.

## Backends

Drivel runs a backend as a separate process. The two that ship with it are
inside the binary, and a backend from anywhere else is an executable Drivel finds
and launches — see [Installing](install.md#install-the-command).

**`unknown kind: "thing" (available: gdrive, sftp); no drivel-provider-thing in …`**
— nothing provides that kind. The message lists what *is* available and every
directory Drivel looked in. Check the spelling in your config file first; if the
backend really is one you meant to add, install `drivel-provider-thing` into one
of those directories.

**`unknown kind: "gdrive" (available: sftp)`, from a Drivel that should have
both** — this is a `drivel` built without its backends (`TAGS=nobundle`), and it
needs the matching `drivel-provider-*` executables installed beside it. A
`go install …/cmd/drivel@latest` build has both inside it.

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

**Drivel is running a backend you did not expect.** A backend built into Drivel
wins over an installed file of the same name, and Drivel names the file it
ignored: `plugin gdrive: using the backend bundled in this binary; ignoring
/usr/lib/drivel/plugins/drivel-provider-gdrive`. Among *installed* copies the
first on the search path wins, and Drivel logs the other the same way: `plugin
gdrive: using /usr/bin/drivel-provider-gdrive; also found …`. The search order
is: next to the `drivel` binary, then `~/.local/share/drivel/plugins`, then
`/usr/local/lib/drivel/plugins`, then `/usr/lib/drivel/plugins`. Setting
`DRIVEL_PLUGIN_PATH` replaces that list entirely.

Either way the log is the answer, and it is worth reading once after a change:
the point of those lines is that "which backend am I running?" is never something
you have to work out from a directory listing.

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

**`the configured Drive root "…" names nothing this account can see`.**
[`-drive-root`](configuration.md#google-drive-settings) (`root` in a config file)
takes a Drive **folder ID**, not a folder path and not a folder name. The ID is
the last part of the folder's address in the Drive web UI — open the folder and
copy the segment after `/folders/` in `drive.google.com/drive/folders/ID`. The
same message appears for a folder belonging to another account, and there are two
near relatives of it for a root that names a file rather than a folder, or one
sitting in the Drive trash. Drivel refuses all four rather than mounting, because
a root it cannot list looks exactly like a Drive where everything was deleted.

**Copying many files in starts fast, then the filesystem itself slows down.**
Drivel keeps a queue of pending uploads. A bulk copy of many small files can fill
it faster than they upload, and once it is full the filesystem waits rather than
dropping anything — so the copy proceeds at the speed of your uplink. Raising
[`-push-delay`](configuration.md#when-a-change-is-pushed) drains that queue on a
calmer cycle and makes it far less likely. `-upload-workers` is worth a try for
many *small* files — until 16 October 2026 the Drive backend uploaded one at a
time no matter what you set, and now it does not — but for anything large the
limit is your uplink and not the pool. A single large file is unaffected: Drivel
notices it once, when it is closed.

Drivel says so when it happens, once each way:

```
[sync] back-pressure: writes through the mount are now waiting on uploads
       (256 of 1280 event(s) queued, 846 awaiting dispatch; -upload-workers 4, -push-delay 300ms)
[sync] back-pressure: cleared after 41m12s — 8104 write(s) waited, 12 event(s) still queued
```

Those two lines are the whole report, however long the copy runs: they mark the
start and the end, not the backlog from moment to moment. Seeing the first
without the second means it is still going on.

The first count can be well short of the total — 256 of 1280 above — and that is
not a contradiction. Each file is handled by one of the upload workers, chosen
from its path, so a burst that lands mostly on one worker fills that worker's
share and stops there while the others still have room. Raising
`-upload-workers` raises the total, but not the share any one worker gets.

The second count is the rest of the backlog: changes Drivel has accepted from the
filesystem and not yet handed to a worker. For a bulk copy that is where almost
all of it sits, so it is the number that tells you how much is left.

**A file came back after I deleted it.** Most likely [same-name
siblings](data-safety.md#same-name-siblings): the path names more than one Drive
object, deleting removed the visible one, and an older one is now visible. Resolve
the duplicates in the Drive web UI.

There was a second cause, fixed on 16 October 2026: a file could be re-downloaded
seconds after you deleted it, from Drive's own report of the deletion being still
in progress, and the folder it was in then stayed behind for good. If you are on
an older build and have an empty folder you deleted, remove it again — and if a
sweep has since pushed it back to Drive, remove it there too. See [what Drivel
does about the race](data-safety.md#deletion).

**A folder I renamed appears twice on my other machines.** Fixed on 16 October
2026. Before that, a rename of a folder reached your other machines as an empty
new folder beside the full old one; a sweep repaired it for small folders, and for
anything over about a hundred files the repair hit `-max-deletes` and refused
itself, leaving the folder duplicated for good. On a current build you should see
one `[pull] move` line per rename and no downloads.

If you are on an older build and have a doubled folder, delete the stale copy
through the mount — it is the one whose files are no longer on the remote — then
re-run with `-resync` and a `-max-deletes` above the number of files involved
(or `0`), which is the same recipe as the refusal below.

**`[sweep] REFUSING to delete: … over the -max-deletes limit`.** Drivel inferred more
deletions than the cap allows and abandoned the entire pass rather than performing
part of it. Check first that the mount is pointed where you think it is — a wrong
`-drive-root`, or an empty `-data`, produces exactly this. A folder renamed by an
older build is the other common cause, and the entry above has the recipe. If the
deletions are genuine, re-run with **both** `-resync` and a higher
`-max-deletes`; the cap alone changes nothing until the next scheduled sweep.

**`[sync] push write … failed after 5 attempt(s)`.** Drivel tried, backed off
between attempts, and gave up — a few seconds of retries in total. A
non-retryable error (a permission failure, a quota refusal) gives up after the
first attempt instead, which the count tells you. The file on your machine is
fine; it is the copy on the remote that is now behind.

The line ends in `(recorded for repair by the next sweep)`, and that is what
happens: Drivel remembers the path and re-sends it from the next enumeration,
which it schedules within a few minutes rather than waiting for
[`-sweep-interval`](configuration.md#when-an-upload-is-abandoned). **This works
with `-sweep-interval 0` too** — `0` means "do not poll", not "never repair". You
will see the repair in the log as `[sweep] repush  PATH (an earlier push was
abandoned)`, and the sweep's closing summary counts them.

If the failures keep coming, fix the cause rather than waiting: the repair is
re-attempted on a sweep that backs off as it keeps failing, out to 80 minutes
between tries, so it will not fix a wrong credential or a full account for you.

**Drivel was killed while uploading, and the file never went up.** It does now, on
the next start. Drivel records the size and timestamp of every file at the moment
it syncs it, and a sweep re-sends anything whose local copy no longer matches:

```
[sweep] repush  notes/todo.md (local copy diverged from its baseline; an earlier push never landed)
```

Before this it did not, and the reason is worth knowing because it explains why
waiting did not help: a sweep compares the remote against what Drivel last
recorded, the crash left both of those agreeing, and the only thing that had
changed was your local file — which nothing was looking at. Sweeping repeatedly
could not fix it.

The bounds are in [configuration](configuration.md#when-drivel-is-killed-mid-upload),
and the one that decides what to do here is that only files modified **before the
mount started** are considered: this is repair for what happened while Drivel was
down, not a second uploader running beside the first. So if you suspect a file is
still behind, restart the mount — or run `-resync` to sweep without waiting.

**A file I deleted is still on the remote, and nothing retried the deletion.**
This one is not repaired automatically. A push that is abandoned is re-sent from
the local file, and a deletion has no local file to re-send from — so if the
`[sync] push unlink …` line failed, delete it again, or from the other machine.
Drivel never infers that deletion for you, because the remote object is still
there and still looks like a file you have.

**`[pull] apply PATH: …` — a remote change did not arrive.** The download or
the local write failed. Drivel retries it on its own about a minute later
(`[pull] refetch PATH`), then less and less often if it keeps failing, out to
about an hour, so a cause that does not clear on its own — a full disk, a name
the backing filesystem will not accept — needs fixing, and the next retry then
succeeds. `drivel status PATH` shows such a file as `error` until it does. See
[when a download fails](configuration.md#when-a-download-fails).

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
