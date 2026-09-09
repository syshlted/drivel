# Drivel

**Your Drive, as a folder. Nothing phones home.**

Drivel turns a Google Drive folder into an ordinary directory on your machine.
Open, edit and save files with the tools you already use — Drivel proxies every
operation to a real local directory and syncs it with Drive in the background, in
both directions. Reads and lookups never touch the network, so your file manager
never spins waiting on the cloud.

It is a **client you run yourself**. There is no Drivel service, no shared
application identity, and no analytics or telemetry — nothing phones home. You
create your own Google Cloud OAuth credentials, so every byte of Drive traffic
goes directly between your machine and Google under an app identity you own and
can revoke.

Free software under the [GNU AGPLv3](#license).

## Quickstart

```sh
sudo apt install fuse3                                    # or: dnf install fuse3
go install github.com/zishmusic/drivel/cmd/drivel@latest

drivel login -account personal                            # one-time OAuth wizard
drivel mount                                              # serves your config file
```

A single mount needs no config file at all:

```sh
drivel mount -mount ./mnt -data ./data \
  -credentials credentials.json -token token.json
```

Omit `-credentials` and Drivel runs log-only: it mounts and prints what it *would*
sync, without contacting Google.

On Linux a mount can also be an `/etc/fstab` line, brought up by `mount -a` or at
boot with no terminal attached — see **[mounting from
/etc/fstab](docs/user/fstab.md)**.

Full walkthrough: **[docs/user/quickstart.md](docs/user/quickstart.md)**. You will
need [your own Google API credentials](docs/user/google-cloud-setup.md) — free,
about five minutes.

## Documentation

**[Using Drivel](docs/user/)** — [quickstart](docs/user/quickstart.md) ·
[installing](docs/user/install.md) ·
[Google credentials](docs/user/google-cloud-setup.md) ·
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

Release history is in [CHANGELOG.md](CHANGELOG.md). Contributions:
[CONTRIBUTING.md](CONTRIBUTING.md). Vulnerabilities:
[SECURITY.md](SECURITY.md).

## What it does

- **Bidirectional background sync** with Google Drive — edit locally or in the
  cloud; both converge.
- **Instant filesystem operations** — nothing blocks on the network, ever.
- **Lazy mode** ([`-lazy`](docs/user/lazy-mode.md)) — a whole Drive visible as
  placeholders, content fetched on first read. A directory listing costs nothing.
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
- **Bring your own credentials** — no shared app identity, no hosted service, no
  telemetry.

## Platform support

| Platform | Status |
| --- | --- |
| **Linux** (all architectures) | **Supported** — built and tested; what CI exercises. |
| **FreeBSD** | **Supported** — full suite run on 15.1. Needs `fusefs`; `-data` required. |
| **macOS** (Intel, Apple Silicon) | **Builds; never run.** Needs macFUSE; `-data` required. Treat as unverified. |
| **Windows** | **Not supported, not planned.** Use WSL2, or Google Drive for Desktop. |

Details, and what changes off Linux — in-place mode, extended attributes,
case-insensitive filesystems, macFUSE and AGPL — are in
[docs/user/platforms.md](docs/user/platforms.md).

## Status

Bidirectional sync, lazy hydration, smart uploads, restart-safe path resolution,
initial enumeration and multi-account mounts are all shipped. See
[CHANGELOG.md](CHANGELOG.md) for what landed when, and what is planned next.

There are no tagged releases yet; build from source or `go install …@latest`.

## Building

```sh
make build            # or: go build -o ./bin/drivel ./cmd/drivel
make check            # everything CI runs, in CI's order
```

`make help` lists every target. See
[docs/dev/building.md](docs/dev/building.md).

## License

Copyright (C) 2026 SystemHalted and Jeremy Melanson.

Drivel is free software licensed under the **GNU Affero General Public License,
version 3** — see [LICENSE](LICENSE). In short: you may use, modify and
redistribute it, but derivative works — **including software you offer to others
over a network** — must be made available under the same license. There is no CLA
and no separate proprietary edition.

Drivel bundles no application secrets and collects **no analytics or telemetry**.
The Google API credentials you supply are yours; how you use Google Drive through
them is governed by Google's own terms, not by this project.
