# Drivel — marketing copy

Short, reusable paragraphs for the website, a repo tagline, and social blurbs.
Everything here is factual to the current implementation — no vaporware.

This is a *source* for copy, not a description of the product: when the two
disagree, [the README](../../README.md) and [docs/user/](../user/) are right and
this file needs updating.

One subject to keep out of all of it: **where data goes, and what is or is not
collected.** No "nothing phones home" — Drivel's entire job is talking to a cloud
provider, so to a careful reader the phrase contradicts the product and to a
careless one it promises an offline tool. But no narrower version either: a
promise about traffic or telemetry is a promise about every dependency in the
build, made in copy nobody re-checks when one changes, and in a tagline or a
bullet there is no room to qualify it. Say how the program is *built* instead —
it ships no credentials of its own, you supply your own — and let
[SECURITY.md](../../SECURITY.md) and the code carry the rest.

Drivel takes no position on what a *backend* does, either. Where that matters, it
is disclosed on that backend's own page —
[google-cloud-setup.md](../user/google-cloud-setup.md#what-google-sees) for
Drive — because it is a property of the provider the user picked, not of this
program. A blanket statement in the hero paragraph would be answering for
software we did not write.

And the second subject to keep out: **treating Google Drive as the product.**
Drivel is a filesystem with a provider seam; Drive is the backend that ships
today. Copy that says what Drivel *is* stays backend-neutral, and everything
peculiar to one provider stays behind its prefix (`-drive-root`) or on its page.
Naming the shipping backend is honest — implying it is the only conceivable one
is not, and neither is implying there are others already.

## Tagline options

- **Your cloud storage, as an ordinary folder.**
- **Mount your storage. Own your credentials. Sync in the background.**
- **A cloud drive without the write-through latency.**

For a channel that is specifically Drive users, *"Your Drive, as a folder"* is
fair game — but never in copy that says what Drivel **is**. Drive is the backend
that ships, not the product.

## Hero paragraph

**Drivel turns a folder on a cloud storage provider into an ordinary directory on
your machine.** Open, edit, and save files with the tools you already use — Drivel
intercepts every operation, keeps a real local copy as the source of truth, and
syncs changes with the provider in the background, in both directions. Reads and lookups are
instant because they never touch the network; uploads and downloads happen off
to the side, so your file manager never spins waiting on the cloud. It's the
convenience of a cloud drive without the write-through latency.

## Trust / privacy paragraph

**Drivel is a client you run yourself — there is no Drivel service.** It ships no
credentials of its own: you supply the ones for whatever backend you point it at,
so Drivel reaches your storage under an identity that is yours to inspect and
revoke. It's free software under the MPL-2.0, so the code you run is the code you
can read.

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

- **Bidirectional background sync** — edit locally or in the cloud, both
  converge. Google Drive is the backend that ships today.
- **Instant reads** — filesystem operations never block on the network.
- **Lazy mode** — a whole remote tree visible as zero-byte placeholders, content
  fetched on first read. A store larger than the disk becomes usable.
- **Smart uploads** — an unchanged rebuild sends nothing; large files upload in
  resumable chunks.
- **Guarded deletions** — inferred only from a sync baseline, so a first run
  deletes nothing and a suspicious count refuses the whole pass.
- **Several accounts at once** — N mounts in one process, validated against each
  other before any of them opens.
- **In-place mode (Linux)** — mount a directory onto itself; files just stay put
  when Drivel exits, with no separate cache directory.
- **Pluggable backends** — the cloud side is a narrow path-addressed interface
  rather than a Drive-shaped one.
- **Bring your own credentials** — Drivel ships none of its own; you supply the
  credentials for the backend you point it at.
- **HTTP/3 transport** with HTTP/2 fallback.
- **Conflict-safe** — concurrent edits produce a conflict copy rather than silent
  data loss.
- **Clean shutdown** — Ctrl-C unmounts and drains in-flight uploads before
  exiting.
- **Free and open** under the Mozilla Public License 2.0.

## One-liner for a repo description

> A Go FUSE filesystem that mounts cloud storage as a local directory and syncs
> it bidirectionally in the background — Google Drive today, behind a pluggable
> provider interface. Bring your own credentials. HTTP/3.
