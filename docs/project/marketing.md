# Drivel — marketing copy

Short, reusable paragraphs for the website, a repo tagline, and social blurbs.
Everything here is factual to the current implementation — no vaporware.

This is a *source* for copy, not a description of the product: when the two
disagree, [the README](../../README.md) and [docs/user/](../user/) are right and
this file needs updating.

## Tagline options

- **Your Drive, as a folder. Nothing phones home.**
- **A folder that syncs to Google Drive — and only Google.**
- **Mount your Drive. Own your credentials. Sync in the background.**

## Hero paragraph

**Drivel turns a Google Drive folder into an ordinary directory on your machine.**
Open, edit, and save files with the tools you already use — Drivel intercepts
every operation, keeps a real local copy as the source of truth, and syncs
changes with Drive in the background, in both directions. Reads and lookups are
instant because they never touch the network; uploads and downloads happen off
to the side, so your file manager never spins waiting on the cloud. It's the
convenience of a cloud drive without the write-through latency.

## Trust / privacy paragraph

**Drivel is a client you run yourself — there is no Drivel service.** It bundles
no shared application identity and collects no analytics or telemetry; nothing
ever phones home. You create your own Google Cloud OAuth credentials in a project
you control, so every byte of Drive traffic goes directly between your machine
and Google under an app identity that is yours to inspect and revoke. It's free
software under the GNU AGPLv3, so the code you run is the code you can read.

## Technical-audience paragraph

**Under the hood, Drivel is a Go FUSE loopback that proxies to a backing
directory and drives a cursor-based sync engine.** Outbound, each filesystem
mutation becomes a change event handled off the FUSE path — debounced per path,
dispatched through a path-hashed worker pool, and retried with backoff on
transient provider errors. Inbound, a poll loop follows Drive's `changes.list`
cursor feed and applies remote edits locally, with echo/loop suppression so a
change you just made doesn't ricochet back and overwrite itself. All Drive
traffic runs over HTTP/3 (QUIC) with automatic HTTP/2 fallback. The cloud backend
sits behind a small path-addressed interface, so Drive is the first provider, not
the only possible one.

## Feature bullets

- **Bidirectional background sync** with Google Drive — edit locally or in the
  cloud, both converge.
- **Instant reads** — filesystem operations never block on the network.
- **Lazy mode** — a whole Drive visible as zero-byte placeholders, content
  fetched on first read. A Drive larger than the disk becomes usable.
- **Smart uploads** — an unchanged rebuild sends nothing; large files upload in
  resumable chunks.
- **Guarded deletions** — inferred only from a sync baseline, so a first run
  deletes nothing and a suspicious count refuses the whole pass.
- **Several accounts at once** — N mounts in one process, validated against each
  other before any of them opens.
- **In-place mode (Linux)** — mount a directory onto itself; files just stay put
  when Drivel exits, with no separate cache directory.
- **Bring your own credentials** — no shared app identity, no hosted service, no
  telemetry.
- **HTTP/3 transport** with HTTP/2 fallback.
- **Conflict-safe** — concurrent edits produce a conflict copy rather than silent
  data loss.
- **Clean shutdown** — Ctrl-C unmounts and drains in-flight uploads before
  exiting.
- **Free and open** under the GNU AGPLv3.

## One-liner for a repo description

> A Go FUSE filesystem that mounts a Google Drive folder as a local directory and
> syncs it bidirectionally in the background — bring your own credentials, no
> telemetry, HTTP/3.
