# Configuration

Drivel is configured two ways, and they do not mix. **Flags** describe one mount
and need no file. **A config file** describes any number of mounts, which one
`drivel` process serves together. `-config` cannot be combined with any flag that
says *what* to mount — there would have to be a precedence rule, and nobody would
remember it. `-debug` and `-pprof` are process-wide and compose with either.

With no flags at all, `drivel mount` reads
`$XDG_CONFIG_HOME/drivel/config.toml` (`~/.config/drivel/config.toml`).

## The config file

An **account** is a set of credentials: a provider kind plus that provider's
settings. Two kinds ship: `gdrive` and [`sftp`](sftp.md). A **mount** references an
account and says what to mount from it. Their provider settings merge, with the
mount's winning, so one account can be mounted several times with different roots.

The kind names a **backend**, which is a separate program Drivel starts for you —
`provider = "gdrive"` needs `drivel-provider-gdrive` installed. See
[Installing](install.md#install-the-command--and-at-least-one-backend); if it is
missing, the mount fails with a message naming every directory that was searched.

```toml
[account.personal]
provider    = "gdrive"
credentials = "/home/you/.config/drivel/personal/credentials.json"
token       = "/home/you/.config/drivel/personal/token.json"

[[mount]]
account = "personal"
path    = "~/drive"
data    = "~/.cache/drivel/personal"
lazy    = true

[mount.provider]
root = "root"                    # or a folder ID, to mount one subtree
```

`drivel login -account NAME` appends the `[account.NAME]` block for you and prints
a matching `[[mount]]` to paste in. **Drivel only ever appends to this file** — it
is never re-serialized, so your comments and layout survive. If an account already
exists, `login` prints the new values for you to reconcile by hand rather than
overwriting them.

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
| — | `name` | the account name, else the mountpoint's basename | A label for this mount, used in log lines and to derive default paths. |
| — | `account` | — | Which `[account.*]` supplies credentials. |
| `-state FILE` | `state` | `drivel-state.db` | The sync-state database (change cursor and echo records). Must not sit inside a backing tree. |
| `-lazy` | `lazy` | off | [Lazy hydration](lazy-mode.md): show remote files as placeholders, fetch on first read. |
| `-xattr` | `xattr` | off | Serve extended attributes through the mountpoint. Off is a [safety property](lazy-mode.md#why--xattr-is-off-by-default). |
| `-resync` | `resync` | off | Force a full enumeration and reconcile at startup. |
| `-materialize` | `materialize` | off | In eager mode, download remote files that have no local copy. Implied by `-lazy`, where it costs only a placeholder. |
| `-max-deletes N` | `max-deletes` | `100` | Cap on deletions one reconcile may infer, in either direction. `0` is unlimited. See [Data safety](data-safety.md#deletion). |
| `-sweep-interval D` | `sweep-interval` | `24h` | How often to re-enumerate. `0` disables it. On a backend with no change feed — [SFTP](sftp.md#how-changes-reach-you) — this is the *poll interval*, and the default is far too slow. |
| `-upload-workers N` | `upload-workers` | `4` | How many files this mount uploads at once. See [Transfer concurrency](#transfer-concurrency). |
| `-hydrate-workers N` | `hydrate-workers` | `8` | How many placeholders this mount fetches at once under `-lazy`. See [Transfer concurrency](#transfer-concurrency). |
| `-debug` | `debug` | off | FUSE-level tracing. Very verbose. |
| `-pprof ADDR` | — | off | Serve Go profiling endpoints on a loopback address. A bare port means `127.0.0.1`. Process-wide. See [Profiling](#profiling). |
| `-pprof-allow-remote` | — | off | Let `-pprof` bind something other than loopback. See [Profiling](#profiling). |
| `-config FILE` | — | — | The config file to read. |

`max-deletes` and `sweep-interval` are stored as optional values so that writing
an explicit `0` survives: merging "zero" with "unset" would silently uncap the
delete guard. The two worker counts are optional for the opposite reason — `0` is
not a pool size, so an explicit one is refused rather than read as "use the
default".

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
rather than moving more bytes. Against that:

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
run.

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
| `-account NAME` | — | Scope this login: files under `~/.config/drivel/NAME`, and an `[account.NAME]` block appended to the config. |
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

Logging is per mount and prefixed with the mount name — except when there is only
one, where the output is unprefixed.

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
