# Configuration

Drivel is configured two ways, and they do not mix. **Flags** describe one mount
and need no file. **A config file** describes any number of mounts, which one
`drivel` process serves together. `-config` cannot be combined with any flag that
says *what* to mount — there would have to be a precedence rule, and nobody would
remember it. `-debug`, `-pprof` and `-control` are process-wide and compose with
either.

With no flags at all, `drivel mount` reads
`$XDG_CONFIG_HOME/drivel/config.toml` (`~/.config/drivel/config.toml`).

## The config file

An **account** is a set of credentials: a provider kind plus that provider's
settings. Two kinds ship: `gdrive` and [`sftp`](sftp.md). A **mount** references an
account and says what to mount from it. Their provider settings merge, with the
mount's winning, so one account can be mounted several times with different roots.

The kind names a **backend**, which Drivel runs as a separate process. Both
shipped backends are inside the `drivel` binary, so there is nothing to install
for either; a backend somebody else wrote is an executable Drivel finds and
launches, and a mount naming one that is absent fails with a message listing the
kinds that are available and every directory that was searched. See
[Installing](install.md#install-the-command).

```toml
[account.personal]
provider    = "gdrive"
credentials = "/home/you/.config/drivel/personal/credentials.json"
token       = "/home/you/.config/drivel/personal/token.json"

[[mount]]
account = "personal"
path    = "~/drive"
data    = "~/.local/share/drivel/mounts/personal"
lazy    = true

[mount.provider]
root = "root"                    # or a folder ID, to mount one subtree
```

**A `[[mount]]` has two halves, and the sub-table is the remote one.** The keys
written directly under `[[mount]]` — `path`, `data`, `lazy`, `state` — describe
the filesystem on *this* machine. The keys in `[mount.provider]` are the
**backend's** own settings, the same vocabulary an account table uses, and none
of them says anything about the local side: `root` there is a Drive folder or an
SFTP directory, never a local path.

They live in two places because they answer two questions. Credentials belong to
the account, so they are written once; *what to mount from it* belongs to the
mount, so a second mount can point the same account somewhere else. Drivel lays
the mount's table over the account's and the mount wins key by key — a
`[mount.provider]` naming a folder ID replaces the account's `root` and leaves
its `credentials` untouched. Drivel itself never learns what any key in either
table means; it hands them to the backend, which reports anything it does not
recognise.

Note the single brackets. `[[mount.provider]]` is a TOML *array of tables* and is
not this shape. It is refused rather than ignored, but the message names the keys
inside it rather than the brackets, so it is worth getting right the first time.

`drivel login -account NAME` writes both blocks for you: the `[account.NAME]`
table, and a `[[mount]]` at `~/drive-NAME` to go with it, backed by
`$XDG_DATA_HOME/drivel/mounts/NAME`. Edit them afterwards — the mount it writes
is a starting point, not a guess at what you wanted.

The backing store goes under your **data** directory and not your cache
directory, because it holds your files rather than a copy of them: in eager mode
it is the only local copy, and even under `lazy = true` it holds anything not
yet uploaded and every [conflict copy](data-safety.md). Nothing prunes it, and
nothing should.

**Drivel only ever appends to this file** — it is never re-serialized, so your
comments and layout survive. The flip side is that it can only ever *add*:
changing or removing an entry is yours to do by hand. So if the account already
exists, or something already claims that mountpoint, `login` prints the block it
would have written and leaves the file alone.

An unrecognised key is an **error**, not a silent no-op: `lazzy = true` doing
nothing is the same failure as a flag that stopped being read. The free-form
regions — an account's provider settings and `[mount.provider]` — are exempt,
because carrying provider-defined keys is their whole purpose.

Relative paths resolve against the **config file's own directory**, so a config
directory can be moved or kept in a dotfiles repo. Inside a provider table a value
is treated as a path only when it is written like one (`~/`, `./`, `../`) — this
layer cannot know which keys name files, and silently rewriting a Drive folder ID
would be corruption.

## Mount options

Every key mirrors a flag. Both are listed together below; `drivel mount -h` and
[`drivel(1)`](drivel.1) are the authoritative reference.

| Flag | Config key | Default | What it does |
| --- | --- | --- | --- |
| `-mount DIR` | `path` | — | Where the filesystem appears. Required. |
| `-data DIR` | `data` | — | The backing directory: where the files really live. Omitting it selects [in-place mode](#in-place-mode) (Linux only). |
| `-name NAME` | `name` | the account name, else the mountpoint's basename | As a key, a label for this mount, used in log lines and to derive default paths. As a flag, it [serves only that one mount](#serving-some-of-the-mounts) from the config file. |
| `-account NAME` | `account` | — | As a key, which `[account.*]` supplies credentials. As a flag, it [serves only the mounts belonging to that account](#serving-some-of-the-mounts). |
| `-state FILE` | `state` | `drivel-state.db` | The sync-state database (change cursor and echo records). Must not sit inside a backing tree. |
| `-lazy` | `lazy` | off | [Lazy hydration](lazy-mode.md): show remote files as placeholders, fetch on first read. |
| `-xattr` | `xattr` | off | Serve extended attributes through the mountpoint. Off is a [safety property](lazy-mode.md#why--xattr-is-off-by-default). |
| `-resync` | `resync` | off | Force a full enumeration and reconcile at startup. |
| `-materialize` | `materialize` | off | In eager mode, download remote files that have no local copy. Implied by `-lazy`, where it costs only a placeholder. |
| `-max-deletes N` | `max-deletes` | `100` | Cap on deletions one reconcile may infer, in either direction. `0` is unlimited. See [Data safety](data-safety.md#deletion). |
| `-sweep-interval D` | `sweep-interval` | `24h` | How often to re-enumerate. `0` disables the *schedule*, not the [repair sweep](#when-an-upload-is-abandoned). On a backend with no change feed — [SFTP](sftp.md#how-changes-reach-you) — this is the *poll interval*, and the default is far too slow. |
| `-push-delay D` | `push-delay` | `300ms` | How long a file must go unchanged before it is uploaded. See [When a change is pushed](#when-a-change-is-pushed). |
| `-upload-workers N` | `upload-workers` | `4` | How many files this mount uploads at once. See [Transfer concurrency](#transfer-concurrency). |
| `-hydrate-workers N` | `hydrate-workers` | `8` | How many placeholders this mount fetches at once under `-lazy`. See [Transfer concurrency](#transfer-concurrency). |
| `-conflicts A` | `conflicts` | `dir` | Where the losing side of a conflict goes: `dir`, `local-trash`, `remote-trash` or `overwrite`. See [Where the loser goes](data-safety.md#where-the-loser-goes). |
| `-conflicts-dir P` | `conflicts-dir` | `both` | With `conflicts = "dir"`, where the `.drivel-conflicts` folders live: `both`, `local` or `remote`. Refused beside any other `conflicts`. |
| `-debug` | `debug` | off | FUSE-level tracing. Very verbose. |
| `-pprof ADDR` | — | off | Serve Go profiling endpoints on a loopback address. A bare port means `127.0.0.1`. Process-wide. See [Profiling](#profiling). |
| `-pprof-allow-remote` | — | off | Let `-pprof` bind something other than loopback. See [Profiling](#profiling). |
| `-control` | — | off | Serve the control socket, so `drivel status`, `pause`, `resume` and `stop` can reach this process. Process-wide. See [Status, pause and stop](control.md). |
| `-control-socket PATH` | — | `$XDG_RUNTIME_DIR/drivel/control.sock` | Where `-control` puts the socket. Its directory must be yours and mode `0700`. Refused without `-control`. |
| `-config FILE` | — | — | The config file to read. |

`max-deletes` and `sweep-interval` are stored as optional values so that writing
an explicit `0` survives: merging "zero" with "unset" would silently uncap the
delete guard. The two worker counts and `push-delay` are optional for the opposite
reason — `0` is neither a pool size nor a window, so an explicit one is refused
rather than read as "use the default".

## When a change is pushed

A file is uploaded once it has gone `push-delay` without changing again. The
window exists to coalesce: a file saved three times in quick succession is
uploaded once, not three times.

The default of 300ms is short because the events it merges are already coarse.
Drivel notices a write when the file is **closed**, not on every `write(2)`, so
copying a 50 GB file produces two events rather than millions — the window is
merging those two, not batching a transfer.

Raising it trades promptness for quota and for a calmer queue:

- A file that is rewritten repeatedly — a document you keep saving, a build
  artefact — is uploaded once per window instead of once per save.
- The outbound queue drains on a longer cycle. During a bulk copy of many small
  files Drivel can otherwise fill its internal queue, at which point the
  filesystem itself slows to the speed of your uplink. A longer window makes that
  far less likely.

What it will **not** do:

- It cannot hold a file forever. A file changed again inside every window is
  still uploaded, on a bound derived from the delay, so a busy file cannot starve.
- It has no effect on a file that is held open and never closed — a database, a
  VM disk image, a log an application keeps open, or a new file still being
  written. Drivel does not see those
  changes until the file is closed; until then they are picked up by the periodic
  sweep (`sweep-interval`), not by this.
- It changes nothing about conflicts. A longer window means a longer period in
  which the remote can change under you, so it makes [conflict
  copies](data-safety.md) slightly more likely, not less.

Anything still queued when you unmount is flushed before Drivel exits, and
anything lost to a crash is found again by the next sweep.

## When an upload is abandoned

Every push is retried with a growing delay between attempts, five attempts over
roughly seven seconds; an error the backend says is permanent — a permission
failure, a quota refusal — is not retried at all. Past that Drivel gives up on
that event and logs it:

```
[sync] push write notes/todo.md failed after 5 attempt(s): … (recorded for repair by the next sweep)
```

Giving up is deliberate. Retrying a rejected upload forever would spin, and
keeping a durable queue of unsent work would be a second copy of your data to
keep safe. What Drivel does instead is **remember the path**, and bring the next
enumeration forward to a few minutes' time so that it re-sends the file from
whatever is on disk by then:

```
[sweep] a push was abandoned: a repair sweep falls due in 5m0s
[sweep] repush  notes/todo.md (an earlier push was abandoned)
```

Three things follow from that, and the last one is the one worth knowing:

- It is the **next** sweep, not an immediate one. A sweep is the most expensive
  thing Drivel does, so one transient failure does not buy you one; the delay
  also folds a whole outage's worth of failures into a single sweep.
- Repeated failures back off, doubling out to 80 minutes, so a backend that is
  genuinely broken is not swept every five minutes while it stays broken.
- **`-sweep-interval 0` does not switch this off.** `0` means "do not poll on a
  schedule"; a file that failed to upload is still repaired.

A *deletion* that is abandoned is the exception: there is no local file to
re-send, so Drivel does not retry it and does not infer it later. Delete it
again if you see one fail.

## When Drivel is killed mid-upload

Remembering the path only works when Drivel gets to run the code that remembers
it. A `kill -9`, a power cut, an out-of-memory kill or a shutdown that ran out of
time records nothing — so the sweep has a second way to find the same file.
Alongside the size and timestamp of every file it has synced, it compares each
local file against them and re-sends the ones that no longer match:

```
[sweep] repush  notes/todo.md (local copy diverged from its baseline; an earlier push never landed)
```

**A start after an unclean shutdown always runs that sweep.** Drivel marks the
state database when a mount starts serving and clears the mark only once it has
finished sending everything at unmount, so a run that was killed — or whose
shutdown ran out of time — leaves the mark behind, and the next start sweeps:

```
[sweep] the run started at 2026-10-17T09:00:00Z did not shut down cleanly: re-enumerating the remote tree to find pushes it lost
```

Before 17 October 2026 nothing did that on Google Drive: with a change cursor
already stored, the next sweep was the one `sweep-interval` scheduled — a day
away by default, and never at `0`. SFTP sweeps on every start anyway.

The same pass covers a file written into the backing directory while Drivel was
not running — but after a *clean* shutdown nothing forces a sweep, so such a file
waits for the next scheduled one; run `-resync` to have it picked up at once.
Three things bound the pass:

- Only files last modified **before this mount started** are considered. A file
  modified since then is one Drivel is already dealing with — queued, uploading,
  or waiting out its retries — and re-sending it from here would duplicate the
  transfer. The practical consequence is that this repairs what happened while
  Drivel was down, and a restart (or `-resync`) is what makes it look again.
- A file held open and never closed is unaffected, exactly as under
  `push-delay`: its timestamp keeps moving, so it is never treated as a lost
  upload. It is sent when it is closed.
- A file whose recorded size and timestamp are missing — anything last synced by
  a build older than 10 September 2026 — is counted in the sweep summary as
  `N baseline(s) with no fingerprint to check` and left alone, rather than
  re-uploaded on a guess. The record fills itself in the next time anything syncs
  that file.

## When a download fails

The other direction has its own failure: a remote change that cannot be applied
on this machine — the connection drops partway through a download, the disk is
full, the backing filesystem refuses the name. Drivel cannot simply stop and
wait for that one file, because then nothing else would come in either, so it
moves on and **remembers the path**:

```
[pull] apply notes/todo.md: … connection reset by peer
[pull] a remote change could not be applied: retrying in 1m0s
[pull] refetch notes/todo.md (an earlier apply failed)
```

A retry asks the remote what the file is *now* and applies that, with every
check an ordinary download gets, so a file edited again in the meantime arrives
at its newest version and a local edit still gets its conflict copy. No sweep is
involved, so this works at any `-sweep-interval`, including `0`. Retries that
keep failing back off from one minute to about an hour, and a restart retries
at once.

Two cases are deliberately left alone. A file that no longer exists remotely by
the time of the retry is simply dropped from the list — the deletion reaches
you the ordinary way. And a remote **deletion** that fails to apply is not
retried, because by the time a retry ran, the file at that path could be new
work of yours that has not been uploaded yet; the next sweep deals with it,
with the safeguards it applies to every deletion.

## Transfer concurrency

Two numbers, because the two directions are not alike. `upload-workers` sizes the
pool that pushes local changes; `hydrate-workers` caps how many placeholders are
being fetched at once in [lazy mode](lazy-mode.md). A typical link's downlink is
several times its uplink, and a provider may well cap the two differently, so one
shared number would be wrong at one end or the other.

Both are **per mount**, and deliberately so. Two mounts are usually two accounts;
a limit they shared would let either one starve the other and let each infer, from
how long it waited, when the other was busy. Nothing in Drivel budgets across
mounts.

Raising `upload-workers` buys less than it looks like it should. Every push shares
one HTTP/3 connection and therefore one congestion window, so extra workers
overlap the per-file round trips — auth, the unchanged-content check, metadata —
rather than moving more bytes. How much that is worth has not been measured: it
could not be until 16 October 2026, because the Drive backend held one lock for
the length of each upload and the pool ran one at a time whatever this was set
to. Against it:

- Each in-flight upload can hold a chunk buffer (16 MiB on Drive), so 32 workers
  can be half a gigabyte of buffers.
- Past the point the provider starts refusing requests, more workers cost quota
  and gain nothing. Drivel retries with backoff, so this shows up as slower sync
  rather than as errors.
- Writes to the *same path* are serialised whatever you set, because concurrent
  uploads of one file can land out of order. Concurrency here is across files.

`hydrate-workers` is the one that bounds a `grep -r` over a lazy tree: without it
every file faulted at once. It sits on the read path, so a caller is blocked on
every fetch it governs — setting it to 1 turns a parallel read into a queue.

Neither number governs the inbound change feed, which applies remote changes one
at a time and has no knob.

## Google Drive options

These belong to the provider, so on the command line they are prefixed `-drive-`
and in the file they live in an account table or `[mount.provider]`.

| Flag | Config key | Default | What it does |
| --- | --- | --- | --- |
| `-credentials FILE` | `credentials` | `""` | OAuth client secret JSON. Without it Drivel runs **log-only**: it mounts and prints what it would sync, and never contacts Google. |
| `-token FILE` | `token` | `token.json` | The cached token written by `drivel login`. |
| `-drive-root ID` | `root` | `root` | The Drive folder mapped to the mount root. `root` means all of My Drive. |
| `-index FILE` | `index` | `drivel-index.db` | The persistent path↔file-ID cache. `""` disables persistence, which costs API round trips and nothing else. |
| `-drive-sweep-mode M` | `sweep-mode` | `auto` | How the enumeration sweep walks Drive: see below. |
| `-drive-delete M` | `delete` | `trash` | What removing a file does remotely: see below. |
| — | `scope` | `drive` | The OAuth scope the token was granted, as `login` recorded it. |

### `-drive-sweep-mode`

The sweep is how Drivel discovers a Drive that existed before you first mounted
it. Two strategies, and neither is always cheaper:

- **`scoped`** descends from your mount root, one listing per folder, eight at a
  time. The bill is proportional to **the folder you mounted**.
- **`flat`** lists the whole account, one request per thousand objects, page after
  page, and sorts it out afterwards. The bill is proportional to **the whole
  Drive** — on a million-object account, minutes, repeated for every mount of that
  account and on every re-sweep.
- **`auto`** (the default) descends when `-drive-root` names a concrete folder and
  lists the account when it names the whole Drive.

Override it when your subtree holds a very large number of directories: a descent
spends roughly one request per folder against flat's one per thousand objects, so
a deep tree of near-empty directories is cheaper `flat`. Drivel notices that shape
and says so in the log rather than leaving you to find out.

An unknown value is refused at startup rather than quietly falling back.

### `-drive-delete`

- **`trash`** (the default) moves the file to the Drive trash. You can restore it
  from [drive.google.com](https://drive.google.com/drive/trash) for 30 days, after
  which Drive purges it.
- **`permanent`** unlinks it outright, which is what Drivel did before. There is
  no undo, from anywhere.

The default is the recoverable one because not every deletion Drivel performs is
one you asked for: an enumeration sweep can *infer* a deletion from a baseline,
and a wrong premise — a mount pointed at the wrong folder, an emptied backing
directory — makes the inference wrong with it. See
[data safety](data-safety.md#deletion).

Choose `permanent` when the mount is how you reclaim space: a trashed file still
counts against your Drive quota until the trash is emptied. An unknown value is
refused at startup.

The setting applies to every removal this mount makes — the ones you type and the
ones a sweep infers alike. It does not affect deletions made *to* you: a file
another client removes is deleted from your backing directory whichever mode you
run, as long as it still holds what was last synced — an edit you have not
uploaded yet is kept (see [data safety](data-safety.md#deletion)).

## SFTP options

An account with `provider = "sftp"` takes a different set of keys — `host`,
`user`, `key`, `root` and a few more. They are listed on the
**[SFTP page](sftp.md#options)**, together with the two things that differ most
from Drive and will bite otherwise: `sweep-interval` is the *poll interval* on a
backend with no change feed, and a removal has no trash to be recovered from.

There are no `-sftp-*` flags. The mount flags are Drive-shaped by history, so an
SFTP mount is configured through the file.

## Login options

```sh
drivel login -account NAME
```

| Flag | Default | What it does |
| --- | --- | --- |
| `-account NAME` | — | Scope this login: files under `~/.config/drivel/NAME`, and an `[account.NAME]` block plus a matching `[[mount]]` appended to the config. |
| `-config FILE` | `~/.config/drivel/config.toml` | Which config file to append to. |
| `-credentials FILE` | `credentials.json` | Where to read/write the OAuth client secret JSON. |
| `-token FILE` | `token.json` | Where to write the token. |
| `-client-id`, `-client-secret` | prompted | Supply them non-interactively. |
| `-scope NAME` | prompted | `drive`, `drive.readonly`, or `drive.file`. |
| `-port N` | `53682` | Loopback port for the OAuth redirect. `0` picks a free one. |
| `-open` | off | Try to open the authorization URL with the OS browser handler. |
| `-project-id ID` | — | Optional; recorded only. |

`mount` never prompts on stdin — it requires a token that `login` already wrote.

## In-place mode

Omitting `-data` mounts a directory onto **itself**: it becomes its own backing
store, so the files simply stay in it when Drivel exits and nothing is ever
copied.

```sh
drivel mount -mount ~/drive
```

This works by opening a directory handle *before* mounting and routing backing I/O
through `/proc/self/fd/N`, so it is **Linux-only**; elsewhere `-data` is required.

## Several mounts at once

```sh
drivel login -account personal
drivel login -account work
drivel mount                     # serves every [[mount]] in the config
```

Each mount gets its own credentials, sync state, index and engine. Before any of
them opens, they are **validated against each other**, because several mounts can
break each other in ways one cannot. These are startup errors naming both mounts:

- two mounts sharing a **sync-state database** — each engine would read the
  other's records as its own baseline, which is also what reconcile infers
  deletions from;
- a **backing directory inside another mount's mountpoint**, in either direction —
  its reads would be routed back through Drivel;
- two mounts sharing a **mountpoint**, or a duplicate **name**;
- a **state database inside a backing tree** — it would sync itself to the cloud,
  and its own writes would generate the events causing more.

### Several Drivel processes

Those checks compare the mounts *one* process was given. Two separate Drivels —
two fstab lines, two service units, a second `drivel mount` in another terminal —
cannot see each other's configuration, and pointing them at nesting directories
used to multiply files without bound: one syncs the other's backing directory as
ordinary content, the other downloads that copy one level deeper, and so on every
sweep.

So each mount leaves a small marker file, `.drivel-backing`, at the root of its
backing directory and holds a lock on it while it runs. A mount refuses to start
when its backing directory, mountpoint or state database is inside the backing
directory of another Drivel that is **running**, or when that other Drivel is
already serving the same directory:

```
mount b: the backing store /srv/a/bdata is inside /srv/a, the backing store of the running drivel mount "a"; …
```

If the other mount is *not* running, Drivel only notes it — the directory is
then just an old directory. And when the outer mount is the one started second,
its sweep leaves the inner directory alone and says so:

```
[sweep] skip    bdata/ (another drivel mount's backing store: its contents are that mount's to sync)
```

On Linux the sweep also skips another Drivel's *mountpoint* inside its tree. And
Drivel refuses to upload a file another program holds locked for writing
directly in the backing directory — which is what another Drivel's state database
looks like while it runs:

```
[sync] skip theirs.db: another process holds it locked for writing (a live database?); not uploading it mid-write
```

The marker is never synced, in either direction: a file of that name is not
uploaded, and a remote one is not downloaded over it. The marker file stays
behind when a mount stops; it is harmless, and deleting it
only removes the protection. Keep the layouts apart anyway: the outer mount
still syncs the marker's directory if someone deletes the marker.

Logging is per mount and prefixed with the mount name — except when there is only
one, where the output is unprefixed.

### Serving some of the mounts

`drivel mount` serves every `[[mount]]` in the config file. Two flags narrow that
to a subset, which is what you want for a mount you bring up by hand, or for one
service unit per mount:

```sh
drivel mount -name photos         # just that one mount
drivel mount -account personal    # every mount belonging to that account
```

`-name` matches the name a mount reports in its log lines — the `name` key, or the
default it falls back to — and names are unique, so it selects exactly one mount.
`-account` matches the `account` key, so it selects however many mounts use that
account: in a config with one mount per account the two flags do the same thing,
and they diverge once an account has several. You cannot combine them, since a
name already identifies a single mount.

Because `-name photos` leaves one mount running, its log output is unprefixed —
the same as a config file that described only that mount.

They **choose** among mounts rather than describing one, which is why they are the
only mount flags that may be combined with `-config`. The same reasoning says they
need a config file to choose from: `-name` together with `-mount`, `-data` or any
other flag describing a mount is an error rather than a flag that gets ignored.

A selector that matches nothing is a startup error listing the names or accounts
the file does define. Mounting nothing and exiting successfully is the failure an
[unrecognised key](#the-config-file) is an error for.

> [!NOTE]
> `-account` on `mount` *selects*; `-account` on [`login`](#login-options) *creates*,
> and `account=` in [an fstab line](fstab.md) names where credentials come from for
> the mount that line describes. An fstab line selects from a config file with
> `name=` instead, because one line is one mount.

### Listing the mounts

`-list` prints the mounts your config file describes and exits, having mounted
nothing:

```console
$ drivel mount -list
4 mounts in /home/you/.config/drivel/config.toml
NAME     ACCOUNT   PROVIDER  MOUNTPOINT
docs     personal  gdrive    /home/you/drive/docs
photos   personal  gdrive    /home/you/drive/photos
backups  nas       sftp      /home/you/nas
scratch  -         -         /home/you/scratch
```

The three columns before the mountpoint are the ones you cannot read off the file
at a glance. `NAME` is what `-name` matches, including the one Drivel *derives*
when a mount sets no `name` key. `ACCOUNT` is what `-account` matches. `PROVIDER`
is the backend that mount will actually talk to — which the file states once, in
an `[account.*]` table the mount then refers to by key, so a file holding both
Drive and SFTP mounts never shows you the two side by side. A `-` in either means
the mount has no account and so syncs with nothing: it mounts and logs, which is
worth seeing when you are wondering why a directory never uploads.

`-full` adds five more columns:

```console
$ drivel mount -list -full
NAME   ACCOUNT   PROVIDER  MOUNTPOINT            DATA                  STATE                   MODE  SWEEP    MAX-DELETES
docs   personal  gdrive    /home/you/drive/docs  …/drivel/mounts/docs  …/drivel/docs/state.db  lazy  24h0m0s  100
```

`DATA` is the backing directory, or `(in place)` for an
[in-place mount](#in-place-mode). `STATE` is the sync-state DB, which is derived
from the mount's name and so appears nowhere in your config file unless you set
`state` yourself. `MODE` is `lazy`, `eager` or `log-only`. `SWEEP` is the
re-enumeration interval — on [SFTP](sftp.md) that is the **poll interval**, and
the 24-hour default is wrong for every SFTP mount — and `MAX-DELETES` is the cap
on deletions one reconcile may infer. The last two read `-` for a log-only mount,
which never enumerates and so never applies either.

Every row is **resolved** rather than echoed back: relative paths are expanded
against the config file's own directory, defaults nobody typed are filled in, an
account's provider settings are merged with the mount's, and the name is the
derived one where there is no `name` key. That is the point — it is the same
resolution mounting performs, so the listing describes the mount you would get.

It is not a config check, though. `-list` fails on a mount it cannot resolve at
all, but it does **not** apply the guards that refuse two mounts sharing a
mountpoint or a state DB. A listing is what you reach for when a config file is
misbehaving, and one that refused to print until the file was already correct
would be no use at the moment you want it.

`-list` composes with the selectors, so `drivel mount -list -account nas` shows
exactly what `drivel mount -account nas` would serve. It needs a config file for
the same reason they do, and it refuses the flags that describe a mount — along
with `-debug`, `-pprof` and `-control`, which have nothing to act on in a command
that exits without mounting anything.

## Profiling

`-pprof ADDR` serves Go's profiling endpoints for the whole process — one
endpoint, not one per mount — and is off unless you give it an address:

```sh
drivel mount … -pprof localhost:6060      # or just: -pprof 6060
go tool pprof http://localhost:6060/debug/pprof/heap
```

**It only binds loopback.** A non-loopback address is refused at startup, and
takes `-pprof-allow-remote` to proceed. That is deliberate and it is not
paranoia: the endpoint hands whoever reaches it this process's heap, which for a
filesystem means the paths — and in a buffer somewhere the contents — of the
files you are syncing, and it lets them start a CPU profile, which is a way to
make a busy mount slower on request. Bind it to localhost and tunnel (`ssh -L`)
instead; reach for the flag only when you have decided who else is on that
network.

Two smaller things. `/debug/pprof/cmdline` is **not** served — it would return
the command line, which names your credentials file, your token, your backing
directory and your account. And a port that cannot be bound is a startup error
rather than a warning, because the reason to run this is to be measuring, and a
long run that quietly produced nothing is worse than one that refused to start.

**In a dev container, watch the port forwarder.** VS Code and similar editors
forward ports they see, which turns a loopback bind into something reachable
from the machine running the editor — the one case the loopback rule does not
cover.
