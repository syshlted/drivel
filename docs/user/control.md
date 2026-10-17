# Status, pause and stop

A running `drivel mount` can answer questions about what it is doing — is this
file synced yet, why is the mount slow, is the backend reachable — and take three
instructions: pause, resume, stop. It does both over a **control socket**, which
is off unless you turn it on.

```sh
drivel mount -control …            # or with -config: drivel mount -config … -control
```

Then, from another terminal:

```sh
drivel status                      # one line per mount
drivel status ~/Drive/report.txt   # one file
drivel status ~/Drive/projects     # what is in a folder, one line per entry
drivel pause -for 2h               # hold all syncing for two hours
drivel resume
drivel stop                        # unmount and finish uploads, as Ctrl-C would
```

Each command takes `-name NAME` to act on one mount of several, and `-socket
PATH` if the mount was started with `-control-socket`. With `-name`, the
commands also look for a socket of that mount's own, which is how an
[fstab mount](fstab.md#the-control-socket) is reached. `drivel status -json`
prints the raw answer.

## Why it is off by default

The socket can list every file you sync and stop the process, so it is opt-in,
like the profiling endpoint. It is reachable only by you: it lives at
`$XDG_RUNTIME_DIR/drivel/control.sock`, in a directory only you can open, and a
connection from another user is refused. If `XDG_RUNTIME_DIR` is not set (some
minimal sessions, some containers), the mount refuses to start with `-control`
until you name a path with `-control-socket` — in a directory that is yours and
mode `0700`; Drivel checks.

One socket serves every mount in the process. Two separate `drivel mount`
processes need two different `-control-socket` paths; the second one refuses to
take over a socket the first is still answering on.

A mount from `/etc/fstab` is its own process, so it gets a socket of its own,
named after the mount — see [the fstab page](fstab.md#the-control-socket).

## Reading `drivel status`

```
synced       report.txt
pending      draft.md
unknown      notes.txt     no sync record: never pushed or pulled by this mount
local-only   draft (conflict 2026-10-17 09-30-00).md   a conflict copy: kept locally and never uploaded
conflict     draft.md      conflict copies: draft (conflict 2026-10-17 09-30-00).md
```

| status | what it means |
|---|---|
| `synced` | Drivel has a record of syncing this, and the file has not changed since |
| `pending` | a change is waiting to go out — in the [push delay](configuration.md#when-a-change-is-pushed), queued behind other uploads, or held by a pause |
| `uploading` | going out now |
| `retrying` | the last attempt failed; another is scheduled |
| `error` | A failure is recorded: either Drivel gave up uploading it and the next sweep re-sends it ([when an upload is abandoned](configuration.md#when-an-upload-is-abandoned)), or a remote change to it failed to apply here and is being retried ([when a download fails](configuration.md#when-a-download-fails)). The detail line says which |
| `placeholder` | [lazy mode](lazy-mode.md): the file's content has not been fetched yet |
| `conflict` | a conflict copy sits beside it — see [data safety](data-safety.md) |
| `local-only` | deliberately not synced: a conflict copy, a pipe, socket or symlink, or one of Drivel's own temporary files |
| `unknown` | **Drivel cannot vouch for it.** The note says why |

`unknown` is the one to read carefully. It is **not** "probably fine": it means
Drivel has no record that would let it say anything else — the file was never
synced by this mount, the mount has no backend, or the file is different from
what was last synced and nothing is queued to send it (it was changed with the
mount down, or behind its back). Drivel prefers to say "I don't know" over
showing a reassuring status it cannot back up.

**A folder's status is about the folder, not its contents.** `drivel status
DIR` lists what is inside instead, which is the answer to "is this folder
synced?". Use `-d` to see the folder's own line, as with `ls -d`.

None of this asks your backend anything. Status comes from what Drivel already
knows locally, so it costs no API quota, and it is answered even while uploads
are backed up — which is usually when you want to ask.

`drivel status` with no path shows each mount's state (`idle`, `syncing`,
`paused`, `saturated` when writes are waiting on uploads, `sweeping`) and its
**backend** reachability: `online`, `offline`, or `unknown`. Drivel never pings
your backend to find out; it reports how its own recent requests went, and says
`unknown` when it has not made any for an hour. A [lazy](lazy-mode.md) mount's
downloads of files you open count, so a lazy mount that you only read from still
reports its backend. An idle SFTP mount is usually `unknown`, and that is
accurate.

## Pausing

`drivel pause` holds uploads, the check for remote changes and the sweep. You
can keep working in the mount: writes are accepted as normal and go out when the
pause ends. Two things still happen while paused:

- an upload that was already running finishes;
- in lazy mode, opening a file that has not been fetched still fetches it —
  refusing would make the file unreadable.

**A pause always ends on its own.** After `-for` (default one hour, at most 24
hours); or when it is holding more than about ten thousand changed files, at
which point it resumes and logs a warning rather than let the backlog grow
without limit; or when the mount stops. A pause is never remembered across a
restart — a mount that came back up silently paused would look fine and sync
nothing.

## Stopping

`drivel stop` does exactly what Ctrl-C or `SIGTERM` does: unmount, then finish
the uploads already queued (for up to 30 seconds), then exit. It waits until the
process has gone. The log says the stop came from the control socket, so
"what stopped my sync?" has an answer afterwards.

## For other tools

The socket speaks HTTP and JSON, so anything can use it — a file-manager
extension, a status-bar widget, a monitoring scraper (`/v1/metrics` is
Prometheus format):

```sh
curl --unix-socket "$XDG_RUNTIME_DIR/drivel/control.sock" http://drivel/v1/mounts
```

There is also a stream of status changes to follow instead of polling. The full
schema — every endpoint, field and rule, and what may change in future versions —
is the [control API reference](../dev/control-api.md).
