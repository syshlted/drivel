# Changelog

What has changed in Drivel, in plain terms. The long-form reasoning behind each
decision lives in the repo's design notes; this file is the summary.

> [!CAUTION]
> **Drivel is a work in progress and may lose data.** Do not use it for anything
> you cannot afford to lose. See [project status](docs/user/status.md).

There are **no tagged releases yet**. Everything below is on the master branch.
Install with `go install github.com/syshlted/drivel/cmd/drivel@latest`, or build
from source with `make build`.

## Unreleased

### Two machines editing one file now always agree on the result — 2026-10-18

When a conflict was found while this machine was already uploading the same
file, the upload could finish with the losing version, so the remote and your
other machines ended up holding the loser while this machine held the winner.
Both versions were kept, but the file itself never agreed again. Drivel now
notices that the file changed under the upload and sends the right one. See
[conflicts](docs/user/data-safety.md#conflicts).

### A deletion on another machine no longer takes your unsaved work — 2026-10-18

When a file was deleted on another machine, Drivel removed the local copy even
if it had been edited here and not uploaded yet. A deleted folder took every
file in it that had never been synced. Now only files that still match what was
last synced are removed; the rest are kept and uploaded again. See
[deletion](docs/user/data-safety.md#deletion).

### You choose where conflict copies go — 2026-10-18

The losing side of a conflict now goes into a `.drivel-conflicts` folder in the
same directory, instead of a copy beside the file. **By default that folder
syncs like any other, so conflict copies now reach the remote and your other
machines.** Before, they stayed on the one machine where the conflict happened.
Two new settings, `conflicts` and `conflicts-dir`, choose otherwise:

- the folder kept on this machine only, or on the remote only;
- your desktop trash;
- the backend's trash (Google Drive);
- or nowhere.

Also, a file's first upload now checks for a file already at that path whatever
its size, and keeps it. Before, only files of 4 MiB or more were checked. On
Google Drive and SFTP the check costs no extra request. `conflicts = "overwrite"`
turns it off. A copy is saved locally before it is uploaded, and the upload
runs in the background, so incoming changes don't wait for it and `drivel pause`
holds it. On Google Drive, a losing version kept on the remote is copied by
Drive itself, with no download or upload. Copies made before this release stay
where they are. See
[where the loser goes](docs/user/data-safety.md#where-the-loser-goes).

### SFTP no longer overwrites an edit that has not uploaded yet — 2026-10-18

On SFTP, a change arriving from the server replaced the local file even when it
had been edited here and not yet uploaded, and no conflict copy was made. Drivel
now keeps both, as it always has on Google Drive. A first sync of a folder that
already matches the server compares the files and leaves identical ones alone.
See [conflicts](docs/user/data-safety.md#conflicts).

### Copying a large file uploads it once — 2026-10-18

A new file that took longer than the push delay (300ms by default) to write was
uploaded twice: once while it was still being written, and again when it was
closed. A new file is now uploaded once, when it is closed. Like any file still
being written, a new one held open is not uploaded until it is closed.

### SFTP cleans up abandoned upload temporaries — 2026-10-17

An upload that was cut off (a killed mount, a power cut) left a hidden
`.drivel-upload.*` file on the server indefinitely. A sweep now removes one that
nothing has written to for 24 hours. See [SFTP](docs/user/sftp.md).

### Duplicate file names on Google Drive are reported by sweeps too — 2026-10-17

Google Drive lets two files in one folder share a name. Drivel uses the most
recently modified one; a sweep now does the same (it used to take whichever the
listing gave last, so you could end up with a different one than Drivel would
update) and says which paths are affected. See
[same-name siblings](docs/user/data-safety.md#same-name-siblings).

### Two Drivels pointed at nesting folders no longer multiply files — 2026-10-17

Drivel already refused mount layouts that overlap within one configuration.
Two separate Drivel processes could not see each other, so placing one's backing
folder inside the other's made files copy themselves one folder deeper on every
sweep, indefinitely. Each mount now marks its backing folder while it runs; a
second Drivel refuses to start inside a running one's, and a sweep leaves another
mount's folder alone. The marker is never uploaded or downloaded, and neither is a
file another program holds locked for writing in the backing folder, such as
another Drivel's live state database. See
[several Drivel processes](docs/user/configuration.md#several-drivel-processes).

### Unmounting no longer hangs when the backend stops answering — 2026-10-17

If the remote stopped responding while files were waiting to upload, stopping
the mount could hang until it was killed: the queue of waiting uploads held up
the filesystem, and the unmount gave up after its first attempt. Drivel now
stops within about 35 seconds of being asked, even if an upload is stuck, records whatever it could not send so the next
start sends it, and keeps retrying the unmount until the mountpoint is free.

### Two machines editing one file no longer lose an edit — 2026-10-17

When two machines changed the same file within seconds and both uploaded their
copy, the second upload replaced the first and the first edit was gone, with no
conflict copy anywhere. Drivel now checks, before uploading over a file it has
synced before, whether someone else has changed it since; if so it saves their
version as a conflict copy first. If your copy is unchanged since the last sync,
it no longer uploads it over the newer one at all. A file of 4 MiB or more that
Drivel has never synced is also kept as a conflict copy if one already exists
remotely at that path. Two uploads that start at practically the same instant
can still pass each other. See
[data safety](docs/user/data-safety.md#when-both-machines-upload).

### fstab mounts and `systemctl stop` — 2026-10-17

Stopping an fstab mount with systemd detaches the filesystem at once and leaves
Drivel finishing its uploads in the background; it does not wait for them. The
[fstab page](docs/user/fstab.md#what-systemctl-stop-waits-for) now says so and
shows a service unit to use if you want `stop` to wait. A spurious
`unmount … failed: Invalid argument` message after a clean stop is gone.

### Stopping an fstab mount no longer cuts its uploads short — 2026-10-17

When systemd stops a mount it signals every process belonging to it. Drivel's
backend process took that signal as an order to exit, even though the main
process was still sending the last of your changes through it, so those uploads
failed and waited for a later repair. The backend now ignores that signal and
stops when Drivel itself is finished with it.

### A crash is followed by a sweep — 2026-10-17

Drivel can find an upload that a crash, a `kill -9` or a timed-out shutdown cut
short, by comparing each file against what it last synced. But it only looks
during a sweep, and on Google Drive nothing started one after a crash: the next
was the scheduled one, a day away by default and never with
`sweep-interval = "0"`. A mount now notes when it starts and when it has finished
sending everything at unmount; if the second never happened, the next start
sweeps. A clean restart costs nothing extra. See
[when Drivel is killed mid-upload](docs/user/configuration.md#when-drivel-is-killed-mid-upload).

### Lazy mode refuses to start where it cannot be safe — 2026-10-17

Lazy mode records which files are placeholders in an extended attribute on the
backing file. On a filesystem that cannot store one (exFAT, FAT, some network
shares) there is no such record, and a placeholder could be uploaded over your
real remote copy. Drivel used to print a warning and mount anyway. It now refuses
to start a lazy mount there and says why. Eager mode is unaffected. See
[lazy mode](docs/user/lazy-mode.md).

### A download that fails is tried again — 2026-10-17

If a remote change could not be applied on this machine (the connection dropped
partway through a download, or the disk was full), the file used to stay out of
date until the next full sweep. That is a day away by default for Google Drive,
and never with `sweep-interval = "0"`. Drivel now remembers the file and tries
again a minute later, backing off to about once an hour if it keeps failing. A
restart retries straight away. Each retry fetches what the remote holds at that
moment, not the version that failed.

A remote *deletion* that fails to apply is not retried this way: by the time a
retry ran, the file at that path might be new work of yours. The next sweep
still handles it, as before.

`drivel status FILE` reports a file that is waiting for a retry as `error`, and
`drivel status` counts these failures and how many a retry has since fixed.

### Status for fstab mounts, and for lazy mounts you only read from — 2026-10-17

A mount from `/etc/fstab` can now have a control socket: add `control` to its
options. Each line gets a socket of its own, named after the mount, and
`drivel status -name NAME` finds it. See
[the fstab page](docs/user/fstab.md#the-control-socket).

A lazy mount's downloads of the files you open now count when `drivel status`
reports whether the backend is reachable. Before, a lazy mount you only read
from said `unknown` all day, and did not notice the backend going away.

### Ask a running mount what it is doing — 2026-10-17

`drivel mount -control` opens a control socket, and four new commands use it:

- `drivel status` shows each mount's state and whether its backend is reachable;
  `drivel status FILE` says whether a file is synced, waiting, uploading,
  retrying, or given up on; `drivel status FOLDER` lists a folder that way.
- `drivel pause -for 2h` holds all syncing — uploads, checking for remote
  changes, the periodic sweep — while you keep working in the mount. Nothing you
  write is lost or blocked; it goes out when the pause ends. A pause always ends
  on its own and is never remembered across a restart.
- `drivel resume` ends it early.
- `drivel stop` unmounts and finishes queued uploads, exactly as Ctrl-C does, and
  the log records that the stop came from the socket.

Status never asks your backend anything, so it costs no quota and answers even
while uploads are backed up. When Drivel has no record that would let it vouch
for a file, it says **`unknown`** — not "synced" — and says why.

The socket is off by default (it can list every file you sync and stop the
process), reachable only by you, and speaks plain HTTP and JSON, so other tools —
a file-manager extension, a status-bar widget, a Prometheus scraper — can use it
too. See [Status, pause and stop](docs/user/control.md).

### Renaming a folder no longer duplicates it on your other computers — 2026-10-16

Rename a folder on one machine and, on every other machine syncing the same
account, you got the new folder *empty* beside the old one still full. That much
was cosmetic and a sweep tidied it up — but only up to about a hundred files.
Above that, the tidy-up proposed more deletions than `-max-deletes` allows, so it
refused the whole thing, logged that it had finished, and never tried again. A
folder of 1000 files ended up on your other machines as 2000 files, permanently,
and every one of them had been downloaded a second time to get there.

Drivel now recognises a renamed folder as a rename. Your other machines rename
their own copy — no downloads, nothing deleted, nothing to tidy up afterwards —
and the record of what has already been synced moves with it, which is what
stopped the deletions being proposed in the first place. The machine that *did*
the rename had a matching problem with its own records and no longer has it
either.

Three things worth knowing. Renaming a **file** was already handled and is
unchanged. If your other machine already has something at the new name, Drivel
leaves both alone rather than overwriting it — that is a conflict and is treated
as one. And this needed a change to how backends talk to Drivel: a backend that
does not report folder renames (SFTP has no way to know about them) behaves
exactly as before, so nothing here changes for SFTP mounts.

### Downloads no longer wait for uploads — 2026-10-16

While a mount was uploading, it did almost nothing else. In testing, a client
with 1000 files to send and 2000 to fetch spent 23 minutes sending and collected
about **20** of the 2000 in that time — then took the rest in the quarter of an
hour after its last upload landed. A client with nothing to send and 3000 files
to fetch finished twelve minutes earlier, with 50% more to download.

The Google Drive backend was holding a single internal lock for the whole
duration of each upload, and everything else it does needs that lock for a
moment: working out which Drive file a path refers to, reading the list of recent
changes, walking a folder during a sweep. So an upload in progress blocked the
whole mount — **including other uploads**, which is why `-upload-workers` made no
difference to Drive no matter what you set it to.

Uploads of one path still exclude each other, which is what stops two of them
creating two files with the same name. Everything else now runs alongside a
transfer. If you raised `-upload-workers` and saw nothing change, try the default
first: it should behave differently now.

### A file you deleted could come back, and the folder it was in stayed — 2026-10-16

Delete a folder full of files and, occasionally, one of them reappeared a few
seconds later — downloaded by Drivel from your own Drive, from a change report
describing the file you had just removed:

```
[sync] delete   notes/scratch.txt
[pull] download notes/scratch.txt      (three seconds later)
[pull] delete   notes/scratch.txt      (five seconds after that)
```

Google Drive moves a folder's contents to the trash along with the folder, but
not instantly, so for a few seconds its change feed still describes the files as
present. Drivel had no way to tell that report from somebody else creating a file
at that path, because the record it compares against is dropped by the deletion
itself.

The file above is tidied up by the next report, as the third line shows. **The
folder is not.** Nothing mentioned the folder at all — it was re-created only as
somewhere to put the download — so nothing later removed it, and it sat there
empty. Worse, the next sweep saw a local folder that the remote did not have,
concluded it was new local work, and pushed it back to Drive, where it then
appeared on every other machine you sync with. A folder you deleted came back
everywhere.

Drivel now remembers what it deleted for five minutes, and ignores a report
carrying the same content at the same path within that window:

```
[pull] ignore  notes/scratch.txt (deleted here 3s ago; the remote is still reporting what we deleted)
```

A file somebody genuinely re-creates at that path is unaffected, because the
check is on the content and not on the clock: different content is applied
straight away, and identical content is picked up by the next sweep. Nothing
about `-max-deletes` or the trash changes.

### A change that was uploading when Drivel was killed is now re-sent — 2026-10-16

If Drivel gives up on an upload it remembers the path and the next sweep re-sends
it (the 12 October entry below). If Drivel is *killed* mid-upload, it never gets
to remember anything — and the file was then left behind permanently. Your copy
held the new version, the remote held the old one, and nothing ever proposed
sending it again: the remote still matched what Drivel last recorded, so every
later sweep looked at the pair and correctly concluded there was nothing to do.
In testing, 44 sweeps over seven minutes changed nothing. A crash was worse than
an outage, which is the wrong way round.

A sweep now also compares each local file against the size and timestamp it had
when it was last synced, and re-sends the ones that no longer match:

```
[sweep] repush  notes/todo.md (local copy diverged from its baseline; an earlier push never landed)
```

That covers a `kill -9`, a power cut, an out-of-memory kill, a shutdown that ran
out of time to finish, and a file written into the backing directory while Drivel
was not running. Two things about it:

- It only considers files last modified **before this mount started**. A file
  modified since then is one Drivel is already dealing with — queued, uploading,
  or waiting out the retry — and a file an application holds open and keeps
  writing to is still only sent when it is closed, as before.
- A file last synced by a build older than 10 September 2026 has no recorded size
  and timestamp to compare against. Those are counted in the sweep's closing
  summary (`N baseline(s) with no fingerprint to check`) and left alone: guessing
  would mean re-uploading a whole tree once, and the record fills itself in the
  next time anything syncs the file.

Separately, a push that Drivel skips because the remote already holds exactly
those bytes now records that fact, where before it discarded it. Without that, a
file whose timestamp changed but whose contents did not would be offered for
upload on every single start, forever.

### A folder-scoped sweep only looked at part of your Drive — 2026-10-14

**This is the one to read.** When Drivel is pointed at a folder rather than at
the whole of My Drive, it walks that folder itself. That walk was stopping after
two steps — the folder you mounted plus eight of its subfolders — and then
reporting that it had finished.

What that cost depends on which client you were. A new client fetched part of the
tree and stopped: in testing, 472 files of 1114, with nothing scheduled to look
again. An established client was worse off, because the part of the tree the walk
never reached looks exactly like a part of the tree somebody deleted, and the
walk's next job is to apply deletions. The `-max-deletes` limit (100 by default)
is what stood between that and a large number of local files being removed; set
it to `0` for "no limit" and nothing did.

Both halves are fixed. The walk now continues to the end — the same test tree
goes from 477 objects listed to all 1157 — and, separately, a walk that *does*
end early for any reason now keeps what it found but refuses to infer a single
deletion from it, does not count itself as a completed sweep, and says
`reconcile INCOMPLETE` so the next start tries again. A backend that cannot
advance its own listing can no longer cause a deletion.

This only ever affected a mount scoped to a folder (`-drive-root <folder>`, the
usual setup) and only the sweep: ordinary sync through the change feed was never
involved, which is why clients that were already up to date stayed correct.

### The back-pressure report repeated itself all through a long copy — 2026-10-14

When a bulk copy fills Drivel's upload queue it says so, once when the
filesystem starts waiting and once when it stops — the entry from 12 October
below has the two lines. They were meant to be two lines. Copying 1100 files
into one folder produced **88 of them in nine minutes**: Drivel decided the
condition was over every second or so, said so, and then immediately said it had
started again — each "cleared" line reporting a queue that was still full.

The reason is worth stating, because the number in the line was wrong in the same
way. Changes wait in two places — a small queue in front of each upload worker,
and a larger set of files whose upload has not been scheduled yet — and Drivel
was only counting the first. So it reported a quarter of its capacity in use when
in fact everything was waiting, and the test for "the backlog has drained" was
true the whole time. A copy now produces one line when it begins to wait and one
when it stops, as described, and both lines count the whole backlog.

### A network blip no longer costs you HTTP/3 until you restart — 2026-10-13

Drive traffic uses HTTP/3 (QUIC, over UDP port 443) and falls back to HTTP/2 when
that is blocked — on a guest network, behind a restrictive firewall, or on a VPN
that does not pass UDP. The fallback works well: it happens on the next request,
nothing fails, and sync carries on.

What did not work was coming back. Drivel remembered that HTTP/3 had failed and
never checked again, so a mount that hit a forty-second blip stayed on HTTP/2 for
as long as it ran — which for a mount started at boot means weeks. Drivel now
re-checks every five minutes and goes back to HTTP/3 when it can. Nothing to
configure, and the re-check costs at most one failed connection attempt per five
minutes while the block lasts.

### Three fixes from killing a mount mid-upload — 2026-10-13

Testing what happens when a mount is killed outright, rather than asked to stop,
turned up three separate problems.

**A killed mount left a process behind.** Drivel runs each backend in its own
process. When you stop a mount normally, Drivel shuts that process down; when the
mount is killed — by `kill -9`, an out-of-memory kill, a crash — it was never told,
and it stayed running indefinitely, still holding your account's credentials and
its connection to the server, with nothing driving it. Every crash left another
one. A killed mount now takes its backend with it.

**Two uploads of the same file at the same time could corrupt it, on SFTP.**
Drivel uploads to a temporary name and renames it into place, so that an
interrupted upload never leaves a half-written file at the real path. The
temporary's name was built from the destination's name alone, so two uploads of
one file — which happens when a scheduled re-scan offers a file the mount is
already sending, and which can also happen between two machines sharing one
server — used the same temporary and overwrote each other inside it. The result
could be a file holding part of each upload. Temporary names are now unique per
upload.

One consequence of the first fix meets the second: because a killed mount's
backend now stops immediately, it no longer gets to tidy up, so an upload
interrupted that way leaves a `.drivel-upload.*` file on the server. Drivel
ignores these — it will not download one or treat it as a deleted file — but
nothing removes them yet, so delete them by hand if they accumulate.

**Upload sizes in the log were wrong for a file still being written.** A line like
`[sync] upload  big.bin (372768768 B)` reported the file's size when the upload
*started*, not what was actually sent, so copying a large file into a mount
under-reported by however much arrived in the meantime. It now reports the bytes
transferred.

### An upload Drivel gave up on is now repaired, not forgotten — 2026-10-12

When an upload fails, Drivel retries it a few times over about seven seconds and
then stops — and an error the backend calls permanent, such as a permission
refusal, is not retried at all. Stopping is right; retrying a rejected upload
forever would spin. What was wrong is that Drivel then *forgot about the file*.
Nothing recorded it, nothing counted it, and the only thing that could ever put
it right was the next full re-scan of the remote tree, which is a day away by
default and which `sweep-interval = 0` switches off entirely.

Drivel now remembers the path and brings the next re-scan forward a few minutes
to re-send it, logging both halves — `(recorded for repair by the next sweep)` on
the failure, and `[sweep] repush  PATH` when it goes out. **This works with
`sweep-interval = 0`**, which means "do not poll on a schedule", not "never
repair". Repeated failures back the repair off, doubling out to 80 minutes, so a
backend that is genuinely broken is not re-scanned every five minutes while it
stays that way.

Two things this does not do. It does not retry an abandoned *deletion* — there is
no local file to re-send, so delete it again if you see one fail. And it is not a
queue of unsent work: Drivel records the path and nothing else, then re-sends
whatever is on disk when it gets there.

### Drivel now says when writes are waiting on uploads — 2026-10-12

Copying a lot of files into a mount can fill the queue of pending uploads, and
once it is full the filesystem waits rather than dropping anything — so the copy
runs at the speed of your uplink, which on Google Drive is about one file per
second. That is correct behaviour and it stays: dropping a change would lose a
file. But nothing said it was happening, so from the outside a working mount
under load and a broken one looked identical.

There are now two log lines, one when the queue fills and one when it drains,
naming how deep it is and the two settings that govern it:

```
[sync] back-pressure: writes through the mount are now waiting on uploads
       (256 of 1280 event(s) queued, 846 awaiting dispatch; -upload-workers 4, -push-delay 300ms)
[sync] back-pressure: cleared after 41m12s — 8104 write(s) waited, 12 event(s) still queued
```

Two lines per episode, not two per second: they mark the start and the end of the
condition however long it lasts. A mount that is keeping up prints neither.

### Two mounts sharing one sync database now say so — 2026-10-10

Each mount keeps its own small database of what it has already synced, and two
mounts must not share one. Drivel already refused to start in that case — the
second one stopped, and the first was left completely untouched — but it reported
the refusal as `open state db: open state db /path/to/state.db: timeout`, which
names the problem twice and the cause not once. "Timeout" reads like a network
problem, and this is the one failure that cannot be: the file is on your disk, and
what is holding it is another copy of Drivel.

It now says the file is already in use by another process, that two mounts need
separate ones, and that nothing in the file was changed. Only the message changed;
the protection was already working.

### Clearer documentation for SFTP mounts that receive nothing — 2026-10-10

An SFTP server has no way to tell Drivel that something changed, so Drivel finds
out by walking the tree on a schedule. That means a second setting matters on this
backend that barely matters elsewhere: in the default mode, Drivel downloads a file
that exists only on the server **only** if you ask it to, with `materialize = true`
(or `-materialize`, or by using lazy mode). On Google Drive that setting governs
the files that were already there when you first mounted, and nothing else. On
SFTP it governs everything arriving from the server, for as long as the mount
exists — so without it, a mount uploads and never receives.

Drivel already said so in its sweep summary (`1 not fetched (add -materialize to
download them)`). The [SFTP page](docs/user/sftp.md) now says it too, which is
where someone setting a mount up would look first.

### Re-syncing a file writes a third as much to your disk — 2026-10-10

Drivel keeps two small bookkeeping databases per mount: what it has already
synced, and (for Google Drive) which remote object each path refers to. Both are
written safely — every update is flushed to disk before it is acknowledged — and
that safety has a cost, because flushing rewrites a few pages of the database each
time, whatever the size of the record. On a large tree that adds up.

It turned out that updating a file that already existed stored the same
path-to-object mapping twice over, byte for byte, having already stored it in an
earlier session. Checking first costs nothing — reading a database does not write
to it — so Drivel now checks. Updating an existing file went from three database
writes to one, and starting up against an existing index went from two per path to
none. Nothing about how safely the data is written has changed.

Creating a *new* file still costs the same, because the mapping it records really
is new; this is about the repeated writes, not the first one.

You are unlikely to notice it in sync speed, since transfers are limited by the
network rather than the disk. It matters for SSD wear on large trees, and it will
matter for speed on faster storage backends later.

### Fixed: a damaged sync database no longer stops Drivel from starting — 2026-10-10

If the file recording what Drivel had already synced became unreadable — a power
cut at the wrong moment, a failing disk — Drivel refused to mount at all, and the
only way out was to find the file and delete it by hand. For a mount that comes up
at boot from `/etc/fstab`, there is nobody to do that.

Drivel now moves the damaged file aside (adding `.corrupt` to its name, so you can
still look at it) and starts with an empty one, saying so clearly in its log. It
can do this safely because an empty database is a state it already knows how to
handle: it is the same situation as a brand-new mount, and **a brand-new mount
never deletes anything** — without a record of what was synced before, Drivel
treats every file it finds as new rather than as deleted.

What you will see afterwards: Drivel re-examines the whole remote tree, which
takes as long as a first sync, and any file that differs between your computer and
the remote gets a conflict copy alongside it, because with no history there is no
way to tell which side changed. Lazily-hydrated files are unaffected — what marks
one is stored on the file itself, not in this database.

The index of which remote object each path refers to is treated the same way, and
there it has no consequences at all: every entry is re-checked against the remote
before it is used, so a fresh one costs a few extra lookups. Previously Drivel
carried on without it for as long as that mount stayed up.

A database that cannot be opened for any other reason behaves as before. In
particular, one already in use by another Drivel process is never touched.

### Fixed: unmounting could cut a sync short instead of letting it finish — 2026-10-09

Drivel's two sync directions are not symmetrical on shutdown. The outbound half
has always been allowed to finish: on Ctrl-C the filesystem is unmounted first,
and the uploads already in hand are given time to complete. The inbound half —
the change-feed poll loop and the periodic re-scan of the remote tree — was only
asked to stop, and Drivel did not wait to hear that it had, so it could still be
working when the databases behind it were closed.

Mostly that cost a log line (`[sweep] delete pass: database not open`) and a
re-scan that reported itself as interrupted even though it had finished, so the
next start repeated it. The case that could have cost data never came up in
practice and is why this is a fix rather than a tidy-up: in in-place mode (no
`-data`), Drivel reaches its files through an open handle to the mount directory,
and once that handle is closed a download still writing through it could write
somewhere else entirely.

Both directions are now waited for, each with its own time limit. Shutdown may
take a few seconds longer than it did; nothing else about it changes.

### A Drive folder ID that names nothing is now refused, not mounted — 2026-10-09

`-drive-root` (`root` in a config file) takes a Drive **folder ID**. Drive does
not complain about one that names nothing — it answers a listing scoped to it
with no results — so a typo, a folder belonging to another account, or a folder
*path* written where an ID belongs produced a mount that came up, reported
success and stayed empty. Worse on a later run than a first one: an empty remote
tree makes every file Drivel has already synced look deleted, with
`-max-deletes` the only thing in the way.

Drivel now checks the folder the first time it needs it and refuses the mount,
naming the value you wrote and what it should look like. A root that names a file
rather than a folder, or a folder in the Drive trash, is refused the same way and
for the same reason. A root that is merely unreachable for now — no network yet,
Drive returning a 5xx — is not: that one is retried, as before.

### Fixed: a Google Drive mount synced nothing after the first hour — 2026-10-09

A Drive mount started, reported `gdrive sync enabled`, and then synced nothing in
either direction, logging only:

```
[pull] initial reconcile failed: context canceled; continuing with the change feed alone
[pull] cannot start change feed: context canceled
```

Nothing had been cancelled. Drivel runs each backend in its own process, and the
Drive backend was keeping the short-lived context it was started with for its
OAuth token refreshes — so the first API call that needed a fresh access token
refreshed against a context that had already been retired. A cached access token
lasts an hour, which made the window after `drivel login` work and everything
after it fail. Mounting, reading and writing local files were unaffected
throughout; only sync was dead.

Cancellations crossing the backend boundary also arrived carrying nothing but the
words `context canceled`, which is what made this almost impossible to diagnose
from the log. They now keep the backend's own message.

### See what your config file describes, without mounting it — 2026-10-06

`drivel mount -list` prints the mounts your config file describes and exits,
having mounted none of them:

```console
$ drivel mount -list
4 mounts in /home/you/.config/drivel/config.toml
NAME     ACCOUNT   PROVIDER  MOUNTPOINT
docs     personal  gdrive    /home/you/drive/docs
photos   personal  gdrive    /home/you/drive/photos
backups  nas       sftp      /home/you/nas
scratch  -         -         /home/you/scratch
```

`NAME` is what `-name` selects on — including the name Drivel derives when you
set no `name` key — and `ACCOUNT` is what `-account` selects on, so this is how
you find out what to type. `PROVIDER` is the backend each mount actually talks
to, which your file says once in an `[account.*]` table and afterwards refers to
by key: with both a Drive mount and an SFTP mount in one file, nothing until now
put the two side by side. A `-` means the mount has no account and syncs with
nothing, which is worth seeing when you are wondering why a directory never
uploads.

`-list -full` adds the backing directory, the sync-state DB (derived from the
mount's name, so it appears nowhere in your file unless you set `state`
yourself), whether content is resident or faulted in, the sweep interval and the
delete cap.

Every row is **resolved** the way mounting would resolve it — relative paths
expanded, defaults filled in, an account's provider settings merged with the
mount's own — so what you see is the mount you would get. It is not a
configuration checker, though: it prints a file that two mounts share a
mountpoint in rather than refusing to, because a listing is what you want
precisely when something is wrong.

It composes with `-name` and `-account`, so `drivel mount -list -account nas`
shows exactly what `drivel mount -account nas` would serve.

### Mount one of the mounts in your config file — 2026-10-05

`drivel mount` serves every `[[mount]]` in your config file. Two new flags narrow
that to a subset, so bringing up a single mount no longer means restating it on
the command line:

```sh
drivel mount -name photos         # just that one mount
drivel mount -account personal    # every mount belonging to that account
```

`-name` matches the name a mount reports in its log lines, and names are unique,
so it picks exactly one. `-account` matches the account a mount belongs to, so it
picks however many use it. With one mount per account the two do the same thing;
they differ once an account has several.

They **choose** among the mounts your config file already describes rather than
describing one, which is why they are the only mount flags you may use together
with `-config`, and why they need a config file to choose from: `-name` alongside
`-mount` or `-data` is an error rather than a flag that gets ignored. A name or
account matching nothing is a startup error listing what the file does define,
because mounting nothing and reporting success is worse than refusing.

One thing to keep straight: `-account` on `mount` *selects* an account, while
`-account` on `login` *creates* one.

### One binary: the backends ship inside Drivel — 2026-10-02

**Installing Drivel is one command again.** `go install
…/cmd/drivel@latest` now gives you a `drivel` that can mount Google Drive and
SFTP, with nothing to install beside it:

```sh
go install github.com/syshlted/drivel/cmd/drivel@latest    # that is all of it
```

Until now the command and each backend were separate executables, and a `drivel`
without `drivel-provider-gdrive` installed somewhere it could find could mount a
directory but had nothing to sync with. The backends are now compiled into the
binary, and Drivel starts one by **re-executing itself**. `make build` produces
that one file, and a release is one artifact rather than a binary plus a plugin
directory to populate.

**Nothing changes about how a backend runs.** It is still a separate process,
reached over the same private socket, speaking the same protocol — so a backend
that crashes still cannot take your filesystem down, and Drivel still starts it
again underneath a mount that stays up. What changes is only how that process is
started, which removes the parts that were worth removing: there is no directory
anyone can write a `drivel-provider-gdrive` into and be handed your credentials,
and no gap between Drivel checking a file and running it.

**A backend from somewhere else still works exactly as before.** An executable
named `drivel-provider-<kind>` on the search path is found and launched the same
way, which is how a backend Drivel does not ship reaches you. A backend built
into Drivel **wins** over an installed file of the same kind, and the mount's log
names the file it ignored, so "which backend am I running?" is always answered in
the log rather than by a directory listing.

If you installed the separate backends, you can remove them —
`rm ~/go/bin/drivel-provider-*` — or leave them; they will be ignored with a log
line. For packaging, `make build TAGS=nobundle` builds a `drivel` with no backend
inside it and `make build-plugins` builds them as separate executables, which is
the shape a distribution shipping one package per backend wants.

### `login` writes the mount block, and the backing store leaves the cache — 2026-09-30

**`drivel login -account NAME` now adds the `[[mount]]` to your config file**
instead of printing it for you to paste. It writes a mount at `~/drive-NAME`,
with the Drive folder you logged in for — a starting point to edit, not a guess
at what you wanted. Nothing else changed about how the file is treated: Drivel
still only ever appends to it, so your comments and layout survive, and if
something already claims that mountpoint the block is printed for you to
reconcile by hand rather than written.

**The backing store it suggests moved out of the cache directory**, from
`~/.cache/drivel/NAME` to `$XDG_DATA_HOME/drivel/mounts/NAME` (by default
`~/.local/share/drivel/mounts/NAME`). The backing directory holds your files
rather than a copy of them — in eager mode it is the only local copy, and even
in lazy mode it holds anything not yet uploaded and every conflict copy — while
`~/.cache` advertises itself as safe to delete at any time. The documentation
examples moved with it. **Existing mounts are unaffected**: nothing rewrites a
config file you already have, and a `data =` you have already got keeps working
exactly as before. The `mounts/` component is there so that an account named
`plugins` cannot back a mount onto the plugin search path.

If you want to move an existing mount, unmount it, move the directory, and
update `data =` to match — the state database tracks paths inside the backing
store, not the path to it.

### A config-file mount no longer fails on its first run — 2026-09-30

**A mount described in the config file failed the first time you ran it**, with
`open state db …: no such file or directory`. The sync-state database defaults
under `$XDG_STATE_HOME/drivel/NAME/`, and nothing created that directory — bbolt
creates the database file, but not the path to it. Only the config-file route was
affected: `drivel mount` with flags defaults the database to the directory you
are standing in, and an fstab mount already created it for itself. Drivel now
creates it wherever the mount came from, mode `0700`, because the echo records
inside name every path that has ever synced.

### zsh completions work from `eval` — 2026-09-29

**`eval "$(drivel completion zsh)"` failed with
`_arguments:comparguments:327: can only be called from completion function`, and
completion did not work.** Dropping the quotes silenced the error but still left
completion dead, because without them the whole script collapses onto one line
that zsh reads as a comment. Only the documented route — writing the script to a
file named `_drivel` on your `$fpath` — ever worked.

The generated script now ends by asking how it was reached: autoloaded from
`$fpath` it calls itself, as that form requires, and sourced or `eval`'d it
registers itself with `compdef` instead of running the completion machinery
outside a completion widget. Both routes work, and the file route is unchanged.

The one rule is ordering, and it was already documented: the `eval` has to come
after `autoload -Uz compinit && compinit`, which is what defines `compdef`.
Placed earlier it now prints one line saying so and leaves the rest of your
`~/.zshrc` running. The reason given for that rule was wrong, though — it is not
that `_drivel` is an autoloaded function file, it is that the script needs
`compdef` to register itself — so `install.md` and the man page say it correctly
now.

### Windows: two caveats tested, one of them withdrawn — 2026-09-29

Drivel was run on Windows 11 for the first time, under WSL2. Nothing about the
software changed; two things the documentation had been *saying* changed, because
they were finally checked instead of reasoned about.

**Lazy mode on Windows drives is not the hazard we described.** The manual said a
`-data` directory on `/mnt/c` could not hold drivel's placeholder marker, and that
`-lazy` was therefore unsafe there. That was wrong. What decides is the Windows
filesystem underneath, not WSL: over NTFS the marker is stored, reads back, and
survives a full `wsl --shutdown`; over exFAT it still is not. The guidance to keep
`-data` on the Linux filesystem inside WSL stands, but for a different and better
reason — NTFS is case-insensitive, so `Foo.txt` and `foo.txt` collide into one
local path, and unlike the attribute question that has no workaround.

**Mounting from `/etc/fstab` under systemd now has a real result behind it.** The
fstab documentation carried a warning that the systemd path had never been run,
because the machine drivel was developed on has no systemd. It has now been run:
systemd generates the mount unit, `systemctl start` brings the filesystem up,
`systemctl stop` unmounts it cleanly, and the background process stays inside the
unit — it is not killed the moment the mount finishes. Two gaps remain and are
written down where they matter: mounting at boot has not been exercised, and it is
not yet known whether pending uploads finish flushing if systemd stops a unit whose
mount is wedged.

### `drivel --help` answers the question you asked — 2026-09-29

`drivel --help`, `drivel -h` and `drivel -help` printed the mount command's flag
list under the heading `Usage of mount:`. Only the bare word `drivel help`
reached the real top-level usage — the one that names the subcommands and says
where to go next. The flag spellings were recognised in the dispatch but could
never be reached, because anything beginning with a dash was handed to `mount`
before the check ran.

All four spellings now print the same top-level usage. A help flag *after* a
subcommand is unchanged and still belongs to it, so `drivel mount -h` lists the
mount options — which now head their list with `Usage of drivel mount:` rather
than naming the subcommand as though it were the program.

### A work-in-progress warning, said out loud — 2026-09-26

Drivel now says on every page of its website, at the top of its README, in the
man page and in a manual page of its own that **it is unfinished, under-tested,
and will probably lose data for somebody** — and that you should keep backups you
have actually restored from.

Nothing about the software changed. What changed is that the documentation was
describing a finished program: it explained the deletion guards and the conflict
policy accurately and in detail, and never once said how little of that had been
run against a real service by anybody who did not write it. A reader was left to
infer maturity from the confidence of the prose, which is exactly the inference
the manifesto's sixth commitment exists to prevent.

[Project status](docs/user/status.md) is the single place that statement lives.
It also names what *has* been exercised and what has not, and the sharp edges
that are documented rather than fixed.

### Relicensed to the Mozilla Public License 2.0 — 2026-09-21

Drivel was AGPLv3 and is now **MPL-2.0**. Nothing about the software changed; what
changed is what you are permitted to build on it.

MPL is copyleft per *file*. If you modify one of Drivel's source files and ship it,
that file's source stays open — which is the part worth protecting, since it is the
sync engine where a subtle mistake loses somebody's data. Everything you build
*around* Drivel is yours: commercial products, proprietary integrations, and
closed-source backends written against the provider interface are all permitted and
always will be. Under AGPLv3 none of that was clear, and a backend importing the
public `provider` package may have had to be AGPL itself — which discouraged exactly
the third-party backends the plugin architecture exists to make possible.

There is no CLA; MPL §5 makes contributions inbound-match-outbound. If you were using
Drivel under AGPLv3 you may continue to, or move to MPL-2.0; MPL §3.3 keeps it
compatible with the GPL family either way.

The reasoning is recorded in `docs/project/licensing.md`, and `MANIFESTO.md` states
what the project is trying to be.

### Control how soon a change is uploaded — 2026-09-15

**`drivel mount -push-delay D`** (config `push-delay`, fstab `push-delay=D`) sets
how long a file must go unchanged before Drivel uploads it. The default is
`300ms`, which is what every previous version did.

The window coalesces, so a file you save three times in a row is uploaded once.
Raising it is worth doing in two situations:

- **You keep re-saving the same files.** A longer window means one upload per
  window instead of one per save, which costs less quota.
- **You are copying a large number of small files in.** Drivel keeps a queue of
  pending uploads, and a bulk copy can fill it — at which point the filesystem
  itself slows down to the speed of your uplink. A longer window drains that
  queue on a calmer cycle and makes the stall far less likely.

```sh
drivel mount -push-delay 30s        # coalesce aggressively
```

Three things it deliberately does not do. It cannot hold a file forever — one
that changes again inside every window is still uploaded, on a bound derived from
the delay. It has no effect on a file that is held open and never closed, such as
a database or a VM disk image, whose changes Drivel does not see until the file is
closed and otherwise picks up on the periodic sweep. And it does not reduce
conflicts: a longer window leaves *more* time for the remote to change underneath
you, so a conflict copy becomes marginally more likely, not less.

`0` is refused rather than quietly meaning "the default", the same way the worker
counts are.

### Drivel prints its own shell completions — 2026-09-13

**`drivel completion bash` and `drivel completion zsh`** write a completion
script to standard output, generated from the flags your binary actually has. To
try it straight away:

```sh
eval "$(drivel completion bash)"
```

That runs Drivel at every shell start, so for a permanent install write the script
where your shell already looks instead:

```sh
drivel completion bash > ~/.local/share/bash-completion/completions/drivel
```

For zsh the file is the better route rather than just the tidier one: `_drivel` is
an autoloaded function file, so the `eval` form works only if it comes after
`compinit` in your `~/.zshrc`.

**Why it changed.** The completions used to be two files shipped in the
repository and placed by `make install`. That left anyone running a downloaded
binary — no repository, no `make` — with no way to get them at all. A binary that
can print its own works everywhere, and packagers get a better deal too: they run
the command instead of needing a Go toolchain.

`sudo make install` still places them for you, so nothing changes if that is how
you install.

### Storage backends are now separate programs — 2026-09-11

**Drivel no longer contains its storage backends; it starts them.** Google Drive
lives in `drivel-provider-gdrive`, SFTP in `drivel-provider-sftp`, and `drivel`
launches whichever a mount needs, talking to it over a private socket.

**This changes how you install it.** Install at least one backend alongside the
command — a `drivel` with none can still mount a directory, but has nothing to
sync with:

```sh
go install github.com/syshlted/drivel/cmd/drivel@latest
go install github.com/syshlted/drivel/cmd/drivel-provider-gdrive@latest
```

From a checkout, `make build` builds all of them and `sudo make install` places
them in `/usr/local/lib/drivel/plugins`. Nothing else changes: your config file,
your flags, your credentials and your backing directories are all as they were.

**What you get for it.** A backend that crashes no longer takes your filesystem
with it — the mount stays up, Drivel restarts the backend behind it, and anything
that was waiting to upload is retried. And you carry only the backends you use:
the Drive client, its OAuth stack and its HTTP/3 transport are no longer in the
binary of someone who only syncs to SFTP.

*Superseded on 2026-10-02 as far as installing goes — the shipped backends are
back inside the binary and there is nothing to install beside it. A backend is
still a separate process, which is what the paragraph above is about.*

**It also means Drivel can be extended without being forked.** A backend is an
ordinary Go program in its own repository: implement the storage interface, call
`plugin.Serve`, build it as `drivel-provider-<name>`, and a config file can name
it. See [writing a provider](docs/dev/new-provider.md).

Four things worth knowing:

- **Drivel refuses to start a backend that is group- or world-writable**, or one
  sitting in a world-writable directory. A backend runs with your credentials, so
  a file anyone could swap out is a file anyone could swap out *with*. The
  refusal names the file and the problem.
- **Drivel talks to a backend over a private socket on your own machine, and
  nothing else.** A backend that asks to be reached over the network — even on
  this machine's own loopback address — is refused before anything connects to
  it. A loopback port is reachable by every user on the machine; the socket
  Drivel uses is readable only by you.
- **A backend gets a deliberately small environment** — `PATH`, `HOME`, `TMPDIR`,
  locale, TLS roots, proxy settings and `SSH_AUTH_SOCK`, and nothing else.
  Ambient cloud credentials such as `GOOGLE_APPLICATION_CREDENTIALS` are not
  passed on, so which account a mount uses is a fact about your config file
  rather than about the shell you started it from.
- **This is not a sandbox**, and it is not described as one. A backend runs as
  you, with your files. Installing one is trusting it, exactly as much as if it
  had been compiled in.

New troubleshooting for all of this is on the [troubleshooting
page](docs/user/troubleshooting.md#backends).

### Drivel now syncs to SFTP servers — 2026-09-10

**Any machine you have an SSH account on can be a Drivel backend.** There is
nothing to install at the other end — SFTP is a subsystem of OpenSSH — and no
credentials to create: it is your existing SSH key.

```toml
[account.server]
provider = "sftp"
host     = "files.example.com"
user     = "you"
key      = "~/.ssh/id_ed25519"
root     = "/srv/share"

[[mount]]
account        = "server"
path           = "~/share"
data           = "~/.cache/drivel/server"
sweep-interval = "5m"
```

What this gives you over `sshfs` is the thing Drivel is for: a write lands in the
local backing directory and returns immediately, with the upload happening behind
it and retrying if the link is down. With `lazy = true` a large remote tree is
browsable at full size while holding almost none of it, and two machines editing
one file produce a conflict copy instead of a silent loss.

**Editing part of a large file now uploads only that part.** SFTP writes at an
offset, so changing one block of a 4 GB file transfers one block. Google Drive
cannot do this at all — it replaces the whole object — so this is the first
backend where that saving is real.

Three things behave differently from Drive, and all three are documented on the
[SFTP page](docs/user/sftp.md):

- **`sweep-interval` is the poll interval.** SFTP has no change notification of
  any kind, so remote changes are found by walking the tree on a schedule. The
  24-hour default is tuned for a backend that also has a live feed and is wrong
  here — set it to the inbound delay you can live with, e.g. `"5m"`.
- **A deletion has no undo.** There is no trash on an SFTP server and Drivel does
  not invent one, so `-max-deletes` is not the guard in front of a safety net —
  it is the only guard.
- **The server's host key must already be in `known_hosts`.** Drivel never trusts
  a key on first use and there is no option to make it: a mount brought up from
  `/etc/fstab` at boot has nobody to answer a prompt. `ssh-keyscan -H host >>
  ~/.ssh/known_hosts`, after checking the fingerprint.

There is no password or passphrase option, and there will not be one. Use an
agent for an encrypted key.

**Inbound sync no longer requires a provider with a change feed.** Backends
without one previously mounted upload-only, silently; they are now polled by the
enumeration sweep, which already carries the guards that make an inferred
deletion safe.

**Deletions made on a backend with no content checksum now reach you.** Drivel
decides whether to apply a remote deletion by asking whether your local copy is
still the one it last synced, and it could only answer that from a checksum the
provider supplied — which SFTP servers do not. The file was kept and re-uploaded
instead, so a file you deleted on the server came back on the next sweep, every
time. Drivel now also records the local file's size and modification time when it
syncs, and uses those when there is no checksum. A file you really did edit is
still kept and pushed back, unchanged. Google Drive is unaffected.

### Every mount flag now works in /etc/fstab, and completions are generated — 2026-09-09

Four flags described a mount but had no fstab option, so a line using one failed
with `unknown option` and nothing said why: **`drive-delete=`**,
`drive-sweep-mode=`, `upload-workers=` and `hydrate-workers=`. All four work now.
The first is the one that mattered — a mount that comes up at boot is exactly the
one nobody is watching, and `config=` is a poor answer for a line that is
otherwise complete.

```
mydrive /home/you/drive fuse.drivel data=/home/you/.cache/drivel,run-as=you,drive-delete=permanent 0 0
```

A test now checks that every mount-describing flag has an option, so this
particular gap cannot come back. `-pprof` is deliberately still absent: it opens a
debug endpoint for the process, which an unattended boot mount should not be
doing.

**The shell completions are generated from Drivel's own flag definitions**
(`make completions`) rather than maintained by hand beside them. The hand-written
files had drifted — they were missing `-upload-workers`, `-hydrate-workers` and
`-pprof-allow-remote` — and the generator now fails the build if a flag has no
completion entry, or if an entry names a flag that no longer exists. Values that
come from a fixed set are taken from the program's own constants, so
`-drive-delete <TAB>` cannot offer a word the binary would refuse.

`sudo make install` now installs them too, to
`$PREFIX/share/bash-completion/completions/drivel` and
`$PREFIX/share/zsh/site-functions/_drivel`.

### Deleting a file now sends it to the Drive trash — 2026-09-09

When Drivel removes a file from Google Drive it now **moves it to the trash**
instead of deleting it outright. A deletion is recoverable from
[drive.google.com](https://drive.google.com/drive/trash) for 30 days, after which
Drive purges it.

This matters most for the deletions you did not personally ask for. An
enumeration sweep can *infer* one — you deleted a file on another machine while
this one was offline, and the sweep works that out from a baseline — and if the
premise is wrong (a mount pointed at the wrong folder, a backing directory that
was emptied) the inference is wrong with it. Every other guard in Drivel fails
towards keeping your bytes; this was the one operation that did not.

Nothing above the change is different: the path stops existing, the removal
reaches your other machines the same way, and deleting a folder still takes its
contents with it. Two things do change. Trashed files **still count against your
Drive quota** until you empty the trash, and a file you delete and immediately
recreate leaves the old copy in the trash.

If you would rather have the old behaviour — a mount that reclaims space as you
delete — ask for it:

```sh
drivel mount ... -drive-delete permanent
```

```toml
[account.work.provider]  # or [mount.provider]
delete = "permanent"
```

`-max-deletes` is unchanged and still on by default: a sweep that wants to delete
more than 100 files still refuses the whole pass. Filling your trash with a
thousand files you did not mean to delete is better than deleting them, but it is
still not what you wanted.

### Mount from /etc/fstab, and at boot — 2026-09-09

On Linux, Drivel is now a `mount(8)` helper as well as a command. Install it as
`/sbin/mount.fuse.drivel` (`sudo make install` puts it there) and a mount can be an
ordinary fstab line, brought up by `mount /your/mountpoint`, by `mount -a`, or by systemd at
boot with no terminal attached:

```
mydrive  /home/you/drive  fuse.drivel  data=/home/you/.cache/drivel,run-as=you,lazy  0 0
```

Three things are worth knowing. The type is **`fuse.drivel`**, not `drivel` —
that is what the kernel reports for the mount, and systemd compares the two, so an
fstab line saying `drivel` describes a mount that never appears under that name
(`mount.drivel` is installed as an alias anyway). **`run-as=NAME`** drops root to
the account that owns the token and the backing directory, which is what makes a
boot-time mount work at all; it is spelled `run-as` and not `user` because `user`
already means something else in fstab. And the helper's output goes to `logfile=`
or nowhere — never to whoever ran `mount`, because an inherited pipe would hang
until the filesystem was unmounted.

Full guide: [docs/user/fstab.md](docs/user/fstab.md). Unknown options fail the
mount rather than being ignored, unless `mount(8)` passes `-s`.

### Hard links are refused, and the files Drivel skips now say so — 2026-09-08

Mounts are now always `nodev` and `nosuid`, with no option to turn them off. A
device node or a setuid binary arriving from a remote is never something you asked
for. This covers the mountpoint; with `-data`, the backing directory is an
ordinary directory and is unaffected either way. FreeBSD requests only `nosuid`,
because its kernel has no `nodev` flag to request.

**Hard links now fail with `EPERM`** — the error `link(2)` defines for a
filesystem that cannot create them. Before, the link succeeded and the outcome was
worse than the error: both names were ordinary files, so both were uploaded, as
two independent remote objects that diverged on the first write. You were told it
worked. Symlinks are unaffected.

Symlinks, FIFOs, sockets and device nodes are still created locally and still
never synced — but Drivel now **logs one line naming each one**, when you create
it and when the enumeration sweep walks past it, and the sweep's summary counts
them. Silence was how "Drivel does not sync symlinks" reached people as "Drivel
lost my file".

One inconsistency fixed while writing that down: a regular file created with
`mknod(2)` rather than `open(O_CREAT)` emitted no event, so whether it synced
depended on which syscall made it and on whether Drivel was watching. It now syncs
like any other regular file.

On FreeBSD, creating a FIFO or a device node through the mount fails with
`EINVAL`. That is the platform, below anything Drivel controls; nothing is lost,
because none of these ever synced.

### The profiling endpoint stops trusting the network — 2026-09-08

`-pprof` now binds **loopback only**. A non-loopback address is refused at
startup and takes a new `-pprof-allow-remote` to proceed; before, it was served
with a warning, which arrives after the heap is already exposed. `-pprof 6060`
(a bare port) now means `127.0.0.1:6060` instead of failing to parse.

`/debug/pprof/cmdline` is no longer served at all. It returned the process's
command line, which names your credentials file, your token, your backing
directory and your account — a map to the secrets rather than the secrets.

Idle connections to the endpoint are now closed after a minute rather than held
open indefinitely. Nothing else about profiling changes: it is still off unless
you ask for it, still one endpoint for the whole process, and a port it cannot
bind is still a startup error.

### Tunable transfer concurrency, and a cap on lazy fetches — 2026-09-08

Two new per-mount settings: `-upload-workers` / `upload-workers` (default 4) for
how many files are uploaded at once, and `-hydrate-workers` / `hydrate-workers`
(default 8) for how many placeholders are fetched at once in lazy mode.

The upload pool was always four and could not be changed. The fetch pool did not
exist: a recursive read over a lazy tree faulted in every file simultaneously, so
a `grep -r` across a thousand placeholders opened a thousand downloads. It is now
bounded by default, which is a behaviour change for lazy mounts — a large parallel
read is paced rather than issued all at once.

They are two numbers rather than one because the directions are not alike: a
typical link's downlink is several times its uplink, the pools bound different
work, and a provider may cap the two differently. Both are per mount and nothing
budgets across mounts — a shared limit would let one account's traffic starve
another's and let each infer when the other was busy.

Raising the upload count buys less than it looks like it should, and
[the configuration guide](docs/user/configuration.md#transfer-concurrency) says
why. Writes to the same path stay serialised whatever you set. The inbound change
feed is unaffected: it still applies remote changes one at a time.

### Release builds are now statically linked — 2026-09-08

`make build` produces a binary with no library dependencies, so it runs on any
Linux of the same architecture regardless of that machine's glibc version.
Previously the default build linked the build host's C library, which meant a
binary built on a recent distro would refuse to start on an older one.

Distribution packagers can opt back in with `make build CGO_ENABLED=1`, which is
the point: a distro package should link the system's libraries so its security
updates arrive through the package manager rather than waiting on a Drivel
release.

### Cheaper first sync for folder mounts — 2026-09-07

If you mount a *folder* rather than your whole Drive, the initial scan now
descends that folder instead of listing your entire account. The cost is
proportional to what you mounted, not to how much is in the Drive.

Previously every mount listed the whole account, page by page — minutes on a
large Drive, repeated on every first run, every `-resync`, every recovery from an
expired cursor and every scheduled re-scan.

`-drive-sweep-mode` picks the strategy: `auto` (default) descends unless you
mounted the whole Drive, `scoped` always descends, `flat` always lists the
account. Neither is always cheaper — a subtree with very many directories is
cheaper `flat`, and Drivel says so in the log when it sees that shape.

Under Drive throttling the descent keeps the listings that succeeded, retries only
the ones that failed, and reduces its own concurrency rather than hammering.

### Extended attributes on macOS and FreeBSD — 2026-09-06

The marker that lazy mode uses to tell a not-yet-downloaded file from an empty one
is now implemented natively on all three platforms. **FreeBSD is now a tested
platform** — the full suite ran green on FreeBSD 15.1 with nothing skipped. macOS
still builds and has never been run; treat it as unverified.

That first FreeBSD run also found a real bug affecting every platform: writes
through a write-only file handle could fail with `EBADF` when the kernel read back
part of a block before modifying it. Fixed.

### Extended attributes are no longer passed through by default — 2026-09-05

A Drivel mount now answers extended-attribute operations the way a filesystem
without attribute support does, unless you pass `-xattr`.

This is a safety change. Drivel's placeholder marker lives in an extended
attribute on the backing file; passing attributes through the mountpoint published
it, letting anything with write access to the mount strip it (making Drivel upload
zeros over your real file) or forge it (making the next read overwrite your local
content). Nothing inside Drivel needs the passthrough.

Also: `-pprof ADDR` for profiling a running mount, and credentials and tokens are
now always written `0600`.

### Several accounts in one process — 2026-09-04

One `drivel` process can serve any number of mounts, each with its own account,
credentials, sync state and backing directory, described in a TOML config file at
`~/.config/drivel/config.toml`.

```sh
drivel login -account personal
drivel login -account work
drivel mount                      # serves everything in the config
```

`login` appends the account block for you — the file is only ever appended to, so
your comments and layout survive. Mounts are checked against each other **before
any of them opens**, because several mounts can break each other in ways one
cannot: sharing a sync-state database, or nesting one mount's backing directory
inside another's mountpoint, are now startup errors naming both mounts rather than
a hang or a file synced to the wrong account.

Also in this batch: an inbound rename no longer duplicates the file on every other
client, and deleting a large directory tree is dramatically faster.

### Seeing a Drive that was already there — 2026-09-03

The change feed only reports what changes *after* Drivel first runs, so a Drive
full of existing files used to be invisible. Now the first run — and `-resync`,
and recovery from an expired cursor — scans the whole remote tree once and
reconciles it against your backing directory. It repeats on a schedule
(`-sweep-interval`, default 24h) to catch anything that happened while Drivel was
not running.

**Deletion is guarded.** Drivel infers a deletion only from a record that it
previously synced that exact path — never from a file simply being missing on one
side. So **the first run never deletes anything**; a locally modified file is kept
and pushed back rather than deleted; and `-max-deletes` (default 100) abandons the
entire delete pass if the count looks like a broken setup rather than a real
cleanup.

With `-lazy` the whole Drive becomes visible immediately as placeholders. Without
it, materialising means downloading, so that stays behind `-materialize`.

This also fixed a silent failure: Google expires change cursors, and Drivel used
to retry a dead one forever with inbound sync quietly stopped.

### Restart-safe path resolution — 2026-09-03

Drive addresses files by opaque ID, not by path, and Drivel's path↔ID map is now
persisted so a restart starts warm. It remains a **cache, never an authority**:
every stored mapping is re-checked against Drive before use, because the object
may have been moved or replaced while Drivel was down. Deleting the index costs
API round trips and nothing else.

This closed a real bug that predated the index: a path Drivel had not yet learned
counted as "not on Drive", so a restart followed by an edit uploaded a **duplicate**
beside the real file.

### Lazy mode and smarter uploads — 2026-09-02

**Lazy hydration (`-lazy`, opt-in).** Remote files appear immediately with their
real name, size and modification time but occupy no space; content is fetched the
first time something reads the file. A directory listing costs nothing, so a Drive
larger than the disk is usable. Requires a backing filesystem that supports
extended attributes.

**Smarter uploads (always on).** Before re-uploading a changed file, Drivel checks
whether the bytes actually differ from what Drive already holds — covering an
editor rewriting an identical buffer, or a rebuild producing the same artifact.
Where a provider can write byte ranges, only the changed extents go out; Drive
cannot, so large files there get chunked, resumable uploads with retries instead.
Small files skip both checks, since the round-trip costs about what the upload
would.

### Bidirectional sync — 2026-07-22

Full two-way sync. Uploads are debounced per path and dispatched through a worker
pool with retries and backoff; concurrent edits on two machines produce a
**conflict copy** rather than silent data loss; and Ctrl-C unmounts and drains
in-flight uploads before exiting.

### Inbound sync — 2026-07-21

Changes made in Drive, or on another machine, now appear in the mounted folder.
Drivel follows Drive's change-feed cursor and suppresses the echo of its own
uploads so a change does not ricochet back and overwrite itself.

Licensed AGPLv3 from this point.

### Google Drive, HTTP/3, and the login wizard — 2026-07-20

Uploads on close, an rclone-style interactive OAuth wizard (`drivel login`) using
your own Google credentials, and all Drive traffic over HTTP/3 (QUIC) with
automatic HTTP/2 fallback.

**In-place mode (Linux):** mount a directory onto itself, so the files just stay
in it when Drivel exits and nothing is ever copied.

Renamed from `dedupfs` to `drivel`.

### First mount — 2026-07-17

A FUSE filesystem that proxies every operation to a backing directory and emits a
change event per mutation.

## Planned

None of these is scheduled, and none is started.

- **Third-party providers** — a plugin architecture, out-of-process or WASM, so a
  backend need not be compiled into Drivel.
- **macOS** — the code is written and cross-compiles; what it needs is a live run
  on real hardware.
- **A deduplicating local backend** — a content-addressed local store which, with
  `-lazy`, makes Drivel a deduplicating filesystem.
- **Client-side encryption** — a layer that stacks over any other backend, so
  Drive holds ciphertext.
- **A block-level filesystem over a distributed database.**
- **A control socket** — so other tools can read per-path sync status, pause and
  resume a mount, and stop the daemon; with `drivel status|pause|resume|stop` as
  the first client.
- **POSIX metadata and mount safety** — carrying mode and ownership, refusing hard
  links explicitly rather than mishandling them, and logging the special files
  Drive cannot represent instead of skipping them silently.

Deduplication is on this list; **GPU-accelerated hashing is not** — it would
optimise a component that does not exist yet. The `-gpu` in the repository's
directory name is historical.
