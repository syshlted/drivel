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
settings. A **mount** references an account and says what to mount from it. Their
provider settings merge, with the mount's winning, so one account can be mounted
several times with different roots.

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
| `-sweep-interval D` | `sweep-interval` | `24h` | How often to re-enumerate. `0` disables it. |
| `-debug` | `debug` | off | FUSE-level tracing. Very verbose. |
| `-pprof ADDR` | — | off | Serve Go profiling endpoints, e.g. `localhost:6060`. Process-wide. |
| `-config FILE` | — | — | The config file to read. |

`max-deletes` and `sweep-interval` are stored as optional values so that writing
an explicit `0` survives: merging "zero" with "unset" would silently uncap the
delete guard.

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
