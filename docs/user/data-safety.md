# Data safety

What Drivel does when things disagree, and the rules it will not break. Worth
reading once before pointing it at data you care about.

## The backing directory is the source of truth

Everything you do on the mount lands in the backing directory first, as an
ordinary file, and syncs afterwards. Filesystem operations never wait on the
network, and they never fail because Google is slow or unreachable — they succeed
locally, and the upload retries in the background.

The practical consequence: **your files survive Drivel.** Unmount it, delete it,
lose your credentials — the backing directory is still a directory of files.

## Conflicts

Drivel syncs both ways, so the same file can change on two machines between polls.
When a remote change arrives for a file that also changed locally since the last
successful sync, Drivel resolves it **last-writer-wins by modification time and
keeps the loser**. By default the loser goes into a `.drivel-conflicts` folder
in the same directory:

```
report.txt
.drivel-conflicts/report (conflict 2026-09-07 14-22-08).txt
```

The time in the name is the losing version's own modification time. Nothing is
discarded unless you ask for that (see [Where the loser goes](#where-the-loser-goes)).
The copy is yours to inspect, merge, or delete.

A conflict is only recorded when both sides genuinely diverged from the last
synced state. A remote change to a file you have not touched is simply applied,
and a local change that has not yet uploaded is simply uploaded.

On SFTP, which reports no checksums, Drivel tells "changed here" from the size
and modification time it recorded at the last sync, and for a file it has never
synced it compares the bytes. Before 18 October 2026 it did not check on SFTP at
all: a local edit that had not uploaded yet was overwritten by the next remote
change, with no conflict copy.

### Where the loser goes

Two settings, per mount. `conflicts` says where the losing version goes:

| `conflicts` | The loser goes to |
|---|---|
| `dir` (default) | The directory's `.drivel-conflicts` folder. |
| `local-trash` | Your desktop trash (`~/.local/share/Trash`), where the file manager can restore it to where it was. |
| `remote-trash` | The backend's trash: on Google Drive, the Drive trash, kept for 30 days. Refused on SFTP, which has no trash. |
| `overwrite` | Nowhere. The winner replaces it. |

With `conflicts = "dir"`, `conflicts-dir` says where the folders live:

| `conflicts-dir` | The `.drivel-conflicts` folders are |
|---|---|
| `both` (default) | Synced like any other folder: on the remote and on every machine that also syncs them. |
| `local` | Kept on the machine where the conflict happened, and never uploaded. |
| `remote` | Kept on the remote only. Drivel uploads the copy there and keeps no local one, and no machine downloads it. |

```toml
[[mount]]
account       = "personal"
path          = "~/drive"
conflicts     = "dir"
conflicts-dir = "local"
```

The flags are `-conflicts` and `-conflicts-dir`, and in `/etc/fstab` the options
of the same names. Naming `conflicts-dir` beside anything but `conflicts = "dir"`
is refused, because it would have no effect.

A few things follow from those choices:

- **A folder that is not synced stays out of every direction.** Under
  `conflicts-dir = "local"` or `"remote"`, and under the two trash settings,
  nothing inside a `.drivel-conflicts` folder is uploaded from this machine, and
  nothing in one on the remote is downloaded to it. Deleting a reviewed copy
  there does nothing to the remote either.
- **Moving a copy out of the folder restores it.** It becomes an ordinary file
  at its new path and uploads as one. A folder moved out uploads with everything
  in it. Moving a file *into* a folder that is not
  synced removes it from the remote, as deleting it would.
- **A copy is safe on this machine before anything is uploaded.** The upload
  happens in the background, so changes arriving from the remote do not wait
  for it, and `drivel pause` holds it like any other transfer.
- **If uploading a copy fails, it is kept locally.** With the folder on both
  sides, the next sweep uploads it. Otherwise, including under `remote-trash`,
  it stays in the local `.drivel-conflicts` folder and the log says so.
- **A loser that cannot be kept anywhere is not overwritten.** If the remote's
  version cannot be downloaded, or a trash cannot be written, the upload or
  download that would replace it fails and is retried.
- **`overwrite` also skips the first-upload check** described below. Use it for
  a mount nothing else writes to, such as a one-way backup.
- **On Google Drive, keeping the remote's losing version on the remote costs no
  transfer.** Drive copies it itself, for `remote-trash` and for a
  `.drivel-conflicts` folder on the remote or on both sides. `remote-trash`
  trashes that copy rather than the original, because moving the original away
  from its path would look to every other machine like a deletion followed by a
  new file. On a backend that cannot copy, the losing version is downloaded and
  uploaded again, so a large file costs its size twice.

Copies made before 18 October 2026 are beside their file instead, as
`report (conflict 2026-09-07 14-22-08).txt`, and stay local-only. Drivel still
recognises them.

### When both machines upload

The case above is a remote change arriving *before* your edit is sent. The
other order is common too: two machines edit the same file within seconds, and
both send their copy before hearing about the other's. Before uploading over a
file it has synced before, Drivel checks whether the remote still holds what it
last synced. If not, someone else's edit is there, so Drivel keeps it first and
then uploads yours:

```
[sync] conflict report.txt: the remote changed since the last sync; kept it in .drivel-conflicts/report (conflict 2026-10-17 09-30-00).txt (here and on the remote) before uploading the local copy
```

Your edit is the one at the real path everywhere. If your local file is
*unchanged* since the last sync (you only touched it, or saved the same bytes)
Drivel does not upload it at all, and the newer remote version arrives as an
ordinary change.

**A file Drivel has never synced is checked too.** If something already exists
remotely at that path, it is kept as a conflict copy before your file replaces
it (`the remote holds a file this mount has no record of`). Your machine might
simply not have known about it yet: the remote file was created moments ago
elsewhere, or (on an eager mount without `-materialize`) it predates the mount
and was never downloaded. On Google Drive and SFTP the check is part of the
upload itself, so it costs nothing extra. Before 18 October 2026 only files of
4 MiB or more were checked, and a smaller one was replaced without a trace.

**One limit remains: the check and the upload are not one step on every
backend.** Two machines uploading the same path within the same instant can
still pass each other, and then the first one's edit is lost. On an OpenSSH
server a new file is placed with a hard link, which refuses an existing name,
so two first uploads cannot both win there. On Google Drive the edit that lost survives in the
file's revision history. Before 17 October 2026 neither machine checked, and the
edit that uploaded first was lost every time.

## Deletion

This is the half of syncing that can lose data, so it is guarded hard.

**A deletion made while Drivel is running** is unambiguous — it saw the operation
— and propagates like any other change.

**A deletion made on another machine** removes the local copy only if it still
holds what was last synced. A file you changed here and have not uploaded yet,
or one that was never synced, is kept and uploaded again, so an edit wins over a
deletion that was made without knowing about it. A deleted folder goes the same
way, file by file, and stays if anything in it was kept:

```
[pull] keep    notes/draft.txt (deleted remotely, but it was modified locally since)
[pull] delete  notes (kept 1 file(s) changed here or never synced)
```

Before 18 October 2026 a remote deletion removed the local path outright, taking
an unuploaded edit, or every unsynced file in a deleted folder, with it.

For a short while afterwards, though, the backend may not agree that it happened.
Google Drive moves a folder's contents to the trash along with the folder, but not
instantly, so for a few seconds its list of recent changes still describes those
files as present. Drivel remembers what it deleted, and for five minutes it
ignores a report that carries the same content at the same path:

```
[pull] ignore  notes/scratch.txt (deleted here 3s ago; the remote is still reporting what we deleted)
```

Without that, Drivel downloaded the file back — and re-created the folder to put
it in, which then looked like new local work and was pushed back to the remote,
re-appearing on every machine you sync with. The check is on the **content**, so a
file somebody genuinely re-creates at that path still arrives: different content
is applied immediately, and identical content is picked up by the next sweep.

**A deletion inferred while Drivel was not running** is the dangerous case, and
Drivel infers one **only from a baseline**: a record that it previously synced
that exact path. A file being absent on one side is not evidence. Without a
baseline, "created on the other side" and "deleted on this side" are the same
observation, so a path Drivel has never synced is treated as **new**, whichever
side it is on.

That single rule has a consequence worth stating plainly: **the first run never
deletes anything.**

Four further guards:

1. Deletions run only after a **complete** enumeration. An interrupted sweep
   deletes nothing.
2. The baseline must **predate** the sweep — otherwise a file you created locally
   *during* the sweep would look remotely deleted.
3. A local file that **changed since its baseline is kept and pushed back**, never
   deleted. Divergence beats absence.
4. `-max-deletes` (default 100) **abandons the whole delete pass** if the count
   looks wrong, rather than trimming it. A very large count means the premise is
   broken — a state database reused against a different Drive folder, an empty
   backing directory, a mount pointing somewhere unexpected — not that there are
   4000 real deletions.

If the deletions really were genuine, re-run with **`-resync` and a higher cap
together**. Raising the cap alone does nothing until the next scheduled sweep,
because a refused pass still counts as a completed one. Drivel's refusal message
says so.

### Where a deleted file goes

A remote deletion is a **move to the Drive trash**, so it is recoverable from
[drive.google.com](https://drive.google.com/drive/trash) for 30 days. That is the
default because of everything above: a deletion Drivel *inferred* rests on a
premise, and a premise can be wrong. The trash is the last guard, the one that
works after all the others have been satisfied by a mistake.

It is a real removal all the same. The path is gone, your other machines delete
their copies, and a folder takes its contents with it — Drive's trash hides the
whole subtree, not just the folder. What survives is the ability to put it back.

Two consequences: a trashed file **still counts against your Drive quota** until
you empty the trash, and deleting a file and recreating it under the same name
leaves the old copy in the trash beside the new one.

`-drive-delete permanent` restores the old behaviour — the API's outright delete,
no undo anywhere — which is what you want if the mount is how you reclaim space.
The cap in guard 4 applies either way: a thousand files in your trash is better
than a thousand files gone, and still not what you asked for.

## Files Drivel does not sync

- **Google-native documents** (Docs, Sheets, Slides) have no byte stream, so there
  is no honest size for a placeholder and no content to compare. They are listed
  and counted — so their absence is never mistaken for a deletion — but never
  materialised locally.
- **Symlinks, sockets, FIFOs and device nodes** are created normally in the backing
  directory and stay there. They have no byte stream, and Drive has no
  representation for them. Drivel now **logs one line naming each one it steps
  over**, from the mount when you create it and from the enumeration sweep when it
  walks past it, so a file that is not syncing says so instead of just being
  absent on your other machines.
- **Hard links are refused** with `Operation not permitted` — see below.
- **Extended attributes** are never synced, in either direction, whatever `-xattr`
  is set to.
- **Conflict copies**, when their folder is not set to sync (see
  [Where the loser goes](#where-the-loser-goes)), and Drivel's own temporary
  files (`.drivel-*`).

## Metadata Drivel does not preserve

The bytes of a file are what syncs. Three things about a file are not part of
that, because Drive has nowhere to put them, and each is visible on your *other*
machines rather than on the one you wrote from:

- **Permissions.** A file Drivel downloads is created by the same atomic write
  that stops you ever seeing a half-finished file, and that write creates it
  `rw-------` (`0600`). So a script you made executable on one machine arrives
  non-executable on the next, and a `setuid` bit does not survive at all. The
  copy on the machine that wrote it keeps whatever mode you gave it.
- **Modification times.** A downloaded file is stamped with the time the upload
  reached the provider, not the time the file was written. Restore an archive
  through a mount and your other machines will date every file in it to the
  restore. Again, the machine that wrote it keeps the original.
- **Holes in sparse files.** The *size* survives and the sparseness does not: an
  8 MiB file holding 4 KiB of real data uploads 8 MiB, downloads 8 MiB, and
  occupies 8 MiB on every other machine. Nothing is lost, but a sparse file is as
  expensive as a dense one everywhere except where it started.

Carrying permissions and ownership properly needs somewhere to keep them that
cannot drift away from the file it describes; it is planned and is not here yet.

## Hard links

`ln`, `cp -l` and anything else asking for a hard link on the mount fails with
`EPERM` (`Operation not permitted`), which is the error `link(2)` defines for a
filesystem that cannot make them.

This is a refusal, not a bug. What a hard link buys — two names, one inode, one
copy of the bytes — is exactly what a cloud store addressed by path cannot
express. Earlier versions let the link succeed, and it was worse than it looked:
the two names were then ordinary regular files, so both were uploaded, as two
independent objects that diverged from each other on the first write. You were
told the link worked and got two files that quietly stopped agreeing.

Symlinks still work; only hard links are refused.

## Mount safety options

Drivel always mounts `nodev` and `nosuid`, and there is no flag to turn them off.
A device node or a setuid binary arriving from a remote is never something you
asked for, so nothing at the mountpoint can gain privilege or reach a device.

**This covers the mountpoint, not the backing directory.** With `-data` the
backing directory is an ordinary directory on an ordinary filesystem, reachable
without going through Drivel at all — so if you put a setuid binary there
yourself, it is live there. Drivel never sets those bits on your behalf.

## Same-name siblings

Google Drive permits two files with the same name in the same folder; POSIX does
not. Two clients creating the same path at the same time can produce exactly that.

When it happens, Drivel picks the most recently modified and logs that the others
are now invisible. Be aware of what a fleet then sees: deleting the file removes
only the visible one, so the path can **reappear with an older sibling's
content**, everywhere. Every available mitigation loses something someone wrote or
makes path resolution non-deterministic, so Drivel tells you rather than choosing
for you. Resolve it in the Drive web UI by renaming or deleting the duplicates.

You will see it in the log in one of two places. When Drivel looks a path up:

```
[drive] 2 remote files share the name "report.txt" in one folder (notes/report.txt); using the most recently modified (…). …
```

and when a sweep lists the folder, once per path per sweep, with a count in the
sweep's summary:

```
[sweep] warning: more than one remote object is named notes/report.txt; only the most recently modified is synced here, …
```

Both make the same choice, so the file you have locally is the one Drivel
updates when you save it. (Before 17 October 2026 a sweep applied the siblings in
whatever order the listing gave, and could leave you holding a different one.)

## The databases are caches

Drivel keeps two small [bbolt](https://github.com/etcd-io/bbolt) databases: sync
state (the change cursor and the sync baselines) and, for Drive, a path↔file-ID
index. **Neither is authoritative for anything.**

Deleting the index costs API round trips and nothing else — every stored mapping
is re-verified against Drive before it is used anyway, because the object may have
been moved or replaced while Drivel was down.

Deleting the sync-state database costs you the baselines, which means the next run
behaves like a first run: it deletes nothing, re-pushes what it cannot account
for, and re-enumerates. Inconvenient, never destructive.

What is *not* recoverable is the [lazy-mode placeholder
marker](lazy-mode.md#the-one-rule-the-backing-filesystem-must-support-extended-attributes),
which lives on the file itself and has no database fallback by design.

### A damaged sync-state database is replaced, not fatal

Because an empty sync-state database is a state Drivel already handles, it does
not need you to intervene when the file is unreadable — after a power cut
mid-write, say, or a failing disk. Drivel renames it by appending `.corrupt`,
starts with an empty one, and says so in its log. It used to refuse to mount,
which left an `/etc/fstab` mount with nobody to fix it.

Everything in the paragraph above applies: nothing is deleted, because a deletion
is only ever inferred from a baseline and there are now none. What you will see is
a full re-enumeration, and **a conflict copy for every path whose local and remote
copies differ** — with no baseline there is nothing to say which side changed, so
keeping both is the only honest answer.

A database that fails to open for some *other* reason is still an error, and one
already in use by another Drivel process is never touched.

## What Drivel never does

- Upload a placeholder over your remote file.
- Splice partial changes into a remote file that has diverged from what it last
  synced — it falls back to a whole-file upload, or a conflict copy.
- Delete anything on the first run.
- Discard the losing side of a conflict, unless you set `conflicts = "overwrite"`.
- Publish a conflict copy you told it to keep local.
- Create a hard link, or let one become two diverging remote files.
- Re-serialize your config file, discarding comments.
