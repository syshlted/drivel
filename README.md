# Drivel

**Your remote storage, as an ordinary folder.**

Drivel turns a folder on a storage provider into an ordinary directory on
your machine. Open, edit and save files with the tools you already use — Drivel
proxies every operation to a real local directory and syncs it with the provider
in the background, in both directions. Reads and lookups never touch the network,
so your file manager never spins waiting on the cloud.

The provider sits behind a small path-addressed interface, and everything
peculiar to one lives behind its own prefix or its own config table. **Two
backends ship today**: **Google Drive**, and **[SFTP](docs/user/sftp.md)** — any
machine you have an SSH account on, with nothing to install at the other end.

It is a **client you run yourself**: there is no Drivel service, and it ships no
credentials of its own. You supply the ones for whatever backend you point it at,
so Drivel reaches your storage under an identity that is yours to inspect and
revoke.

Free software under the [Mozilla Public License 2.0](#license). What the project
is trying to be, and what that costs: **[MANIFESTO.md](MANIFESTO.md)**.

## Quickstart

```sh
sudo apt install fuse3                                    # or: dnf install fuse3
go install github.com/syshlted/drivel/cmd/drivel@latest
go install github.com/syshlted/drivel/cmd/drivel-provider-gdrive@latest  # the backend

drivel login -account personal                            # one-time OAuth wizard
drivel mount                                              # serves your config file
```

A single mount needs no config file at all:

```sh
drivel mount -mount ./mnt -data ./data \
  -credentials credentials.json -token token.json
```

Omit `-credentials` and Drivel runs log-only: it mounts and prints what it *would*
sync, without contacting the provider.

Syncing to a server you can `ssh` to needs no login step and no credentials of its
own — it is your existing SSH key. See **[SFTP](docs/user/sftp.md)**.

On Linux a mount can also be an `/etc/fstab` line, brought up by `mount -a` or at
boot with no terminal attached — see **[mounting from
/etc/fstab](docs/user/fstab.md)**.

Full walkthrough: **[docs/user/quickstart.md](docs/user/quickstart.md)**. You will
need credentials for your provider — for Google Drive, [your own Google API
credentials](docs/user/google-cloud-setup.md), free and about five minutes.

## Documentation

**[Using Drivel](docs/user/)** — [quickstart](docs/user/quickstart.md) ·
[installing](docs/user/install.md) ·
[Google credentials](docs/user/google-cloud-setup.md) ·
[SFTP](docs/user/sftp.md) ·
[configuration](docs/user/configuration.md) ·
[lazy mode](docs/user/lazy-mode.md) ·
[data safety](docs/user/data-safety.md) ·
[troubleshooting](docs/user/troubleshooting.md) ·
[platform support](docs/user/platforms.md) ·
[fstab mounts](docs/user/fstab.md) ·
[`drivel(1)`](docs/user/drivel.1)

**[Developing Drivel](docs/dev/)** — [architecture](docs/dev/architecture.md) ·
[workflows](docs/dev/workflows.md) ·
[storage schema](docs/dev/schema.md) ·
[dependencies](docs/dev/dependencies.md) ·
[conventions](docs/dev/conventions.md) ·
[building](docs/dev/building.md) ·
[testing](docs/dev/testing.md) ·
[writing a provider](docs/dev/new-provider.md) ·
[glossary](docs/dev/glossary.md)

**What Drivel stands for** — [MANIFESTO.md](MANIFESTO.md) ·
[why MPL-2.0](docs/project/licensing.md)

Release history is in [CHANGELOG.md](CHANGELOG.md). Contributions:
[CONTRIBUTING.md](CONTRIBUTING.md). Vulnerabilities:
[SECURITY.md](SECURITY.md).

## What it does

- **Bidirectional background sync** — edit locally or in the cloud; both
  converge.
- **Instant filesystem operations** — nothing blocks on the network, ever.
- **Lazy mode** ([`-lazy`](docs/user/lazy-mode.md)) — a whole remote tree visible
  as placeholders, content fetched on first read. A directory listing costs
  nothing.
- **Smart uploads** — an unchanged rebuild or an editor rewriting an identical
  buffer sends nothing; large files upload in resumable chunks.
- **Conflict-safe** — concurrent edits produce a
  [conflict copy](docs/user/data-safety.md#conflicts), never silent loss.
- **Guarded deletions** — inferred only from a sync baseline, so a first run
  deletes nothing and a suspicious count [refuses the whole
  pass](docs/user/data-safety.md#deletion).
- **In-place mode (Linux)** — mount a directory onto itself; the files just stay
  put when Drivel exits.
- **Several accounts at once** — N mounts in one process, each with its own
  credentials and state, validated against each other before any of them opens.
- **HTTP/3** (QUIC) transport with automatic HTTP/2 fallback.
- **Pluggable backends** — the cloud side is a narrow path-addressed interface
  rather than a Drive-shaped one. Google Drive is the provider that ships today.
- **Bring your own credentials** — Drivel ships none of its own; you supply the
  credentials for the backend you point it at.

## Platform support

| Platform | Status |
| --- | --- |
| **Linux** (all architectures) | **Supported** — built and tested; what CI exercises. |
| **FreeBSD** | **Supported** — full suite run on 15.1. Needs `fusefs`; `-data` required. |
| **macOS** (Intel, Apple Silicon) | **Builds; never run.** Needs macFUSE; `-data` required. Treat as unverified. |
| **Windows** | **Not supported, not planned.** Use WSL2, or Google Drive for Desktop. |

Details, and what changes off Linux — in-place mode, extended attributes,
case-insensitive filesystems, macFUSE and licensing — are in
[docs/user/platforms.md](docs/user/platforms.md).

## Status

Bidirectional sync, lazy hydration, smart uploads, restart-safe path resolution,
initial enumeration and multi-account mounts are all shipped. See
[CHANGELOG.md](CHANGELOG.md) for what landed when, and what is planned next.

There are no tagged releases yet; build from source with `make build`, or
`go install …@latest` — the command and at least one `drivel-provider-*` backend.

## Building

```sh
make build            # or: go build -o ./bin/drivel ./cmd/drivel
make check            # everything CI runs, in CI's order
```

`make help` lists every target. See
[docs/dev/building.md](docs/dev/building.md).

## License

Copyright (C) 2026 SystemHalted and Jeremy Melanson.

Drivel is free software licensed under the **Mozilla Public License, version
2.0** — see [LICENSE](LICENSE). MPL is copyleft *per file*, with an explicit
patent grant: ship a modified Drivel source file and that file's source stays
open; everything you build around it stays yours. Commercial products, linking
into proprietary software, and closed-source backends written against the
provider interface are all fine and always will be. There is no CLA.

**What we ask, and deliberately do not require.** The licence reaches Drivel's
own files and stops. Past that line we can only ask, so we do: if you write a
`drivel-provider-*` backend, publish it; if you run Drivel somewhere we cannot
test, tell us what happened. The provider seam is public precisely so backends
can live outside this tree — a fix that stays in your fork is a fix the next
person has to find again. That is a request, not a term, and nothing in it
conditions your rights under the licence.

The name is the exception. The licence covers the code, not the trademark, so
please don't ship a fork calling itself Drivel. Call it something else and we'll
cheer for it.

Drivel bundles no application secrets. The provider credentials you supply are
yours; how you use a storage provider through them is governed by that provider's
own terms, not by this project.
