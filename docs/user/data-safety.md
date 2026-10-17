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
keeps the loser as a conflict copy** beside it:

```
report.txt
report (conflict 2026-09-07 14-22-08).txt
```

Nothing is discarded. The copy is yours to inspect, merge, or delete.

Conflict copies are **local-only by policy** and are never uploaded — uploading
them would publish the losing side of every conflict you have ever had, to every
device you sync. The enumeration sweep skips them for the same reason.

A conflict copy is only created when both sides genuinely diverged from the last
synced state. A remote change to a file you have not touched is simply applied,
and a local change that has not yet uploaded is simply uploaded.

### When both machines upload

The case above is a remote change arriving *before* your edit is sent. The
other order is common too: two machines edit the same file within seconds, and
both send their copy before hearing about the other's. Before uploading over a
file it has synced before, Drivel checks whether the remote still holds what it
last synced. If not, someone else's edit is there, so Drivel saves it as a
conflict copy first and then uploads yours:

```
[sync] conflict report.txt: the remote changed since the last sync; saved it as report (conflict 2026-10-17 09-30-00).txt before uploading the local copy
```

Your edit is the one at the real path everywhere; the other one is in the copy,
on the machine that uploaded second. If your local file is *unchanged* since the
last sync (you only touched it, or saved the same bytes) Drivel does not upload
it at all, and the newer remote version arrives as an ordinary change.

Two limits. **The check and the upload are two steps**, so two uploads that start
within the same instant can still pass each other, and then the first one's edit
is lost; on Google Drive it survives in the file's revision history. And a file
Drivel has never synced before has no last-synced version to compare against.
If one of 4 MiB or more already exists remotely at that path, Drivel keeps it as
a conflict copy too (`the remote holds a file this mount has no record of`);
a smaller one is replaced as it always has been, because checking would cost an
extra request for every new file. Before 17 October 2026 neither machine checked, and the edit that uploaded first was lost
every time.

## Deletion

This is the half of syncing that can lose data, so it is guarded hard.

**A deletion made while Drivel is running** is unambiguous — it saw the operation
— and propagates like any other change.

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
- **Conflict copies** and Drivel's own temporary files (`.drivel-*`).

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
- Publish a conflict copy.
- Create a hard link, or let one become two diverging remote files.
- Re-serialize your config file, discarding comments.
