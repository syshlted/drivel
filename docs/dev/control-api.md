# Control & status API, v1

The schema of drivel's control socket, the surface other people's software binds
to. **This document is the contract**, not the Go types that implement it
(`internal/control/wire.go` follows this file, never the other way round). The
reasoning behind each rule is DESIGN.md §9, M14; this page says what the API does.

v1 changes only by **adding**: a new endpoint, a new field, a new status value. A
client must ignore fields it does not know and must treat an unknown `status`
value the way it treats `unknown`. Removing or redefining anything is v2.

## Transport

- **HTTP/1.1 + JSON over a unix socket.** No TCP, by design: this API can list
  every synced path and stop the process, so it has an owner rather than a port.
- **Off by default.** `drivel mount -control` turns it on, for the whole process
  (one socket serves every mount the process runs).
- **Location:** `$XDG_RUNTIME_DIR/drivel/control.sock`, or whatever
  `-control-socket PATH` names. With `XDG_RUNTIME_DIR` unset and no
  `-control-socket`, the mount refuses to start rather than inventing a path.
- **Authentication is the filesystem.** The directory must be `0700` and owned by
  the user drivel runs as (an existing directory that is not is refused, not
  fixed); the socket is `0600`. Where the platform reports the peer's credentials
  (`SO_PEERCRED` on Linux, `LOCAL_PEERCRED` on FreeBSD and macOS) a connection
  from another uid is refused with `403`; root is let through.
- **`Authorization` is accepted and ignored.** A token scheme for sandboxed
  clients may be added later; sending a header today is harmless.
- **A stale socket** left by a crash is connected to first and removed only if the
  connection is refused. A live one makes the second drivel refuse to start.
- Any client works: `curl --unix-socket "$XDG_RUNTIME_DIR/drivel/control.sock"
  http://drivel/v1/mounts`. The host part of the URL is ignored.

## Conventions

- Times are RFC 3339 in UTC with fractional seconds. A time that has never
  happened is **omitted**, not sent as zero.
- Paths are **relative to the mount root**, slash-separated, with no leading
  slash: `docs/report.txt`. The root is `""`. A path containing `..`, `.`, or an
  empty segment is refused with `400 bad_path`.
- A mount is addressed by **name**: its `name` in the config file, or, for a
  mount described by flags, the last element of its mountpoint.
- Errors are a status code and a body:

  ```json
  {"error": {"code": "no_such_path", "message": "\"x\" does not exist in this mount and nothing is recorded for it"}}
  ```

  | code | HTTP | meaning |
  |---|---|---|
  | `not_found` | 404 | no such endpoint |
  | `no_such_mount` | 404 | no mount by that name |
  | `no_such_path` | 404 | nothing exists or is recorded at that path |
  | `bad_path` | 400 | not a clean root-relative path |
  | `bad_limit` | 400 | `limit` out of range |
  | `bad_duration` | 400 | `for` unparseable or outside 1s–24h |
  | `log_only` | 409 | the mount has no provider, so there is nothing to pause |
  | `not_running` | 503 | the mount's sync engine has stopped (shutting down) |
  | `busy` | 503 | the engine did not answer within 10s |
  | `forbidden` | 403 | the peer is another user |
  | `not_served` | 404 | a profiling route deliberately not offered here |

## What every answer is made of

**Local state only.** No request causes a call to the provider — a file manager
repainting a folder costs no API quota — and no request waits on the sync
engine's run loop, so status is served while uploads are saturated, which is
when it is most wanted.

**A view, never an authority.** Nothing a client does through this API, other
than pause, resume and shutdown, changes what drivel syncs.

## Endpoints

### `GET /v1/version`

```json
{"api": 1, "pid": 4242, "started": "2026-10-17T09:00:00.123Z", "drain_timeout_seconds": 30}
```

### `GET /v1/mounts`

One summary per mount.

```json
{"mounts": [{
  "name": "work", "mountpoint": "/home/me/Work", "provider": "gdrive", "lazy": false,
  "reachability": "online", "paused": false, "saturated": false,
  "paths_in_motion": 3, "sweeping": false
}]}
```

- `provider` is `""` for a log-only mount.
- `reachability` is `online`, `offline` or `unknown` — see below.
- `saturated` is true while writes through the mount are waiting on uploads
  (back-pressure).
- `paths_in_motion` counts paths with an outbound operation pending, running,
  retrying, or abandoned.

### `GET /v1/mounts/{name}`

The summary fields, plus every counter. Counters are **process-lifetime** and
monotonic (they restart at zero with the process); the `queue` numbers are
gauges.

```json
{
  "name": "work", "mountpoint": "/home/me/Work", "provider": "gdrive", "lazy": false,
  "reachability": "online", "paused": false, "saturated": false, "paths_in_motion": 0, "sweeping": false,
  "at": "2026-10-17T09:12:00Z",
  "outbound": {
    "uploads": 120, "upload_bytes": 52428800,
    "range_puts": 0, "range_bytes": 0,
    "unchanged_skips": 4, "unchanged_bytes": 16777216,
    "placeholders_declined": 0,
    "mkdirs": 3, "deletes": 7, "renames": 1,
    "succeeded": 131, "retries": 2, "abandoned": 0
  },
  "inbound": {
    "downloads": 40, "download_bytes": 1048576,
    "placeholders_created": 0, "restamps": 0,
    "remote_deletes": 2, "remote_moves": 0, "conflicts": 0,
    "suppressed_own_deletes": 1,
    "polls": 900, "poll_errors": 0,
    "last_poll": "2026-10-17T09:11:58Z", "last_cursor_advance": "2026-10-17T09:10:02Z"
  },
  "hydration": {"hydrations": 0, "bytes": 0, "failures": 0},
  "queue": {
    "queued": 0, "in_flight": 0, "coalescing": 0, "paths_in_motion": 0,
    "saturated": false, "back_pressure_episodes": 1
  },
  "reach": {
    "state": "online", "last_success": "2026-10-17T09:11:58Z",
    "consecutive_failures": 0
  },
  "sweep": {
    "running": false, "completed": 1,
    "last": {
      "started": "...", "finished": "...", "complete": true,
      "listed": 1157, "reconciled": 1157, "pushed": 0, "not_fetched": 0, "export_only": 0,
      "deleted_locally": 0, "deleted_remotely": 0, "kept": 0, "orphaned": 0,
      "special": 0, "repaired": 0, "repushed": 0, "unverified": 0,
      "suppressed": 0, "tombstones_expired": 0
    }
  },
  "pause": {"paused": false, "held": 0, "cap": 10000}
}
```

The three content gates are counted separately because they are the point:
`placeholders_declined` is a lazy placeholder that was **not** uploaded (it holds
no bytes), `range_puts` sent only the changed extents, `unchanged_skips` sent
nothing because the remote already held the bytes. `uploads` is whole-file
uploads only.

`sweep.last.complete` is `false` for a sweep that ended early (a provider that
could not advance its listing): it materialised what it saw and concluded
nothing. `sweep.started` is present only while `running`.

`pause.last_auto_resume`, when present, says why the most recent pause ended
without being asked to: it expired, it reached `cap`, or the mount shut down.

#### Reachability

**Observed, never probed.** No health-check request is ever made. Each provider
call drivel makes for its own reasons is classified as it returns:

- success, or an error that is not retryable (the backend answered, and said no)
  → contact;
- a retryable error (timeout, 5xx, throttling, a backend process that died) →
  failure.

`state` is `offline` while the most recent outcome was a failure,
`online` while it was contact, and `unknown` when nothing has been observed for
an hour — an idle backend has not been observed, and no traffic means no
opinion. On a backend with no change feed (SFTP) an idle mount is therefore
`unknown` most of the time, which is accurate.

### `GET /v1/mounts/{name}/status?path=P`

One path.

```json
{"path": "docs/report.txt", "status": "synced", "kind": "file", "size": 5120,
 "synced_at": "2026-10-17T08:00:00Z"}
```

| field | |
|---|---|
| `status` | see below |
| `kind` | `file`, `dir`, `symlink`, `other`; absent when the path does not exist locally |
| `size` | the local file's size (0 for a directory; a placeholder reports its full apparent size) |
| `detail` | a sentence, where the bare value would mislead — which kind of `unknown`, why `local-only` |
| `attempt`, `next_attempt`, `error` | while `retrying`, and the last error for `error` |
| `synced_at` | when the sync record for this path was written |
| `conflicts` | names of conflict copies beside this file |

#### Status values

| status | meaning |
|---|---|
| `synced` | a sync record exists and the local file still matches the size and modification time recorded with it. For a directory: a record exists — **which says nothing about what is inside it** |
| `pending` | an upload, delete or rename is waiting: in the push delay, queued, or held by a pause |
| `uploading` | an operation on it is running now |
| `retrying` | the last attempt failed and another is scheduled |
| `error` | drivel gave up; the path is recorded and the next sweep re-sends it |
| `placeholder` | lazy mode: the file exists with its size and no content, which is fetched on first read |
| `conflict` | a conflict copy sits beside it (`conflicts` names them) |
| `local-only` | exists here and is deliberately not synced: a conflict copy, a special file (pipe, socket, device, symlink), or one of drivel's own temporary files |
| `unknown` | **nothing vouches for it.** Never synced by this mount, a mount with no provider, a record too old to carry a fingerprint, or a local copy that differs from the last synced version with nothing queued (changed outside the mount). `detail` says which |

`unknown` is not an error state and not "probably fine". It is the honest answer
when drivel has no record that would let it say anything else. A client that
renders it as synced is wrong.

**There is no roll-up.** A directory's status is about the directory. "Is this
folder fully synced?" is answered by listing it.

### `GET /v1/mounts/{name}/children?path=P&limit=N&after=NAME`

The direct children of directory `P`, **sorted by name**, as one request — the
shape a file manager repaints a folder with.

```json
{"path": "docs", "entries": [ {…as status…}, … ], "truncated": true, "next_after": "m.txt"}
```

- `limit` defaults to 1000, at most 10000.
- `truncated` means there are more; pass `next_after` as `after` for the next
  page. Paging is by name, so a page boundary is stable while the directory
  changes, and an entry created behind the cursor is simply not seen until the
  next listing.

### `POST /v1/mounts/{name}/pause?for=D`

Hold the mount's uploads, its change-feed polling and its sweep for `D` (a Go
duration: `30m`, `2h`; default `1h`, from `1s` to `24h`).

```json
{"paused": true, "until": "2026-10-17T10:00:00Z"}
```

- **Writes through the mount are still accepted** and the filesystem never
  blocks. Changes accumulate, coalesced per path.
- An upload already running when the pause lands is allowed to finish.
- Opening a placeholder in lazy mode still fetches it: a read is a user waiting.
- Pausing a paused mount moves its expiry.
- **A pause always ends**: when `D` elapses; when it is holding more than `cap`
  paths and operations (10000), in which case it resumes on its own and logs a
  warning; or when the mount shuts down, which sends everything held. It is never
  kept across a restart.
- `409 log_only` for a mount with no provider.

### `POST /v1/mounts/{name}/resume`

```json
{"paused": false}
```

Resuming a mount that is not paused is not an error.

### `POST /v1/shutdown`

Stop the process exactly as `SIGTERM` would: unmount every mount, drain queued
uploads within the drain deadline, exit.

```json
{"shutting_down": true, "drain_timeout_seconds": 30}
```

It answers `202` **before** draining. A caller that needs the uploads finished
waits for the socket to stop accepting connections; that is when the process is
done. The log records that the shutdown came from the socket, and from which uid
and (on Linux) pid.

### `GET /v1/events[?mount=NAME]`

Newline-delimited JSON (`application/x-ndjson`), for as long as the client stays
connected.

```json
{"type":"resync","reason":"stream start: read the current state, then apply what follows","at":"…"}
{"type":"status","mount":"work","path":"a.txt","status":"uploading","at":"…"}
{"type":"status","mount":"work","path":"a.txt","status":"synced","at":"…"}
{"type":"mount","mount":"work","status":"paused","at":"…"}
{"type":"resync","mount":"work","reason":"a directory moved: old -> new","at":"…"}
```

| `type` | |
|---|---|
| `status` | `path` now has `status` — the values above, plus `deleted` |
| `mount` | the mount itself changed: `paused`, `resumed` |
| `resync` | **re-read what you display.** More changed than was itemised (a directory moved, a sweep finished), or this is the start of the stream |

Rules a client can rely on:

- **The first line is always `resync`.** Connect, then read the state you need,
  then apply what follows.
- Transitions are **coalesced per path** over the mount's push delay, so a burst
  arrives as the status it settled on.
- A client that falls more than 1024 distinct paths behind is **disconnected**,
  with a final `resync` line saying so. Nothing a client does can slow drivel
  down; reconnect and re-read.
- A client that stops reading entirely is disconnected after 10 seconds.

### `GET /v1/metrics`

The counters above in Prometheus text format, one series per mount
(`mount="name"`), plus `drivel_reachability{state=…}` one-hot. A convenience
adapter over the same numbers; the JSON is the schema.

### `GET /debug/pprof/…`

Go's snapshot profiles (`heap`, `goroutine`, `allocs`, `block`, `mutex`,
`threadcreate`), so `go tool pprof` can read a profile `curl --unix-socket`
fetched with no port open anywhere. `profile` and `trace` are **not** served
here — they let a caller slow a busy mount down on request — and nor is
`cmdline`, which names the credential files. `-pprof` still offers the first two.

## Non-goals

Not a remote-control API (no TCP; that is a different milestone with an
authentication design). Not a config editor (the config file is only ever
appended to, and an API that re-serialised it would drop every comment). Not a
way to make provider calls. Nothing here may grow a field that is not read from
something the engine already maintains for its own reasons.
