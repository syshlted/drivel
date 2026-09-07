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

## Deletion

This is the half of syncing that can lose data, so it is guarded hard.

**A deletion made while Drivel is running** is unambiguous — it saw the operation
— and propagates like any other change.

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

One Drive-specific note: a remote deletion goes through the API's permanent
delete, **not** a move to the trash. That is precisely why the cap exists.

## Files Drivel does not sync

- **Google-native documents** (Docs, Sheets, Slides) have no byte stream, so there
  is no honest size for a placeholder and no content to compare. They are listed
  and counted — so their absence is never mistaken for a deletion — but never
  materialised locally.
- **Non-regular files** — sockets, FIFOs, device nodes — are skipped. Drive has no
  representation for them.
- **Extended attributes** are never synced, in either direction, whatever `-xattr`
  is set to.
- **Conflict copies** and Drivel's own temporary files (`.drivel-*`).

## Same-name siblings

Google Drive permits two files with the same name in the same folder; POSIX does
not. Two clients creating the same path at the same time can produce exactly that.

When it happens, Drivel picks the most recently modified and logs that the others
are now invisible. Be aware of what a fleet then sees: deleting the file removes
only the visible one, so the path can **reappear with an older sibling's
content**, everywhere. Every available mitigation loses something someone wrote or
makes path resolution non-deterministic, so Drivel tells you rather than choosing
for you. Resolve it in the Drive web UI by renaming or deleting the duplicates.

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

## What Drivel never does

- Upload a placeholder over your remote file.
- Splice partial changes into a remote file that has diverged from what it last
  synced — it falls back to a whole-file upload, or a conflict copy.
- Delete anything on the first run.
- Publish a conflict copy.
- Re-serialize your config file, discarding comments.
- Send anything anywhere except Google, under credentials you created. There is no
  telemetry and no hosted service.
