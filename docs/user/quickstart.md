# Quickstart

From nothing to a mounted Drive folder. Three steps; the middle one is a one-time
Google setup.

## 1. Install

You need Go 1.27+ and a FUSE mount helper.

```sh
# Linux: the mount helper (fusermount3)
sudo apt install fuse3        # Debian/Ubuntu
sudo dnf install fuse3        # Fedora

go install github.com/zishmusic/drivel/cmd/drivel@latest
```

That puts `drivel` in `$(go env GOPATH)/bin`. See [Installing](install.md) for
building from a checkout, macOS and FreeBSD, and shell completions.

## 2. Get Google credentials

Drivel ships no application identity, so you create your own OAuth client in a
Google Cloud project you own. It is free and takes about five minutes:
**[Google credentials](google-cloud-setup.md)**.

You end up with a **client ID** and **client secret**.

## 3. Log in and mount

```sh
drivel login -account personal
```

The wizard asks for the client ID/secret and a scope, prints an authorization URL,
and captures the result — either automatically, via a loopback server on port
53682, or by you pasting the redirect URL back into the prompt. It writes
credentials and a token under `~/.config/drivel/personal/`, and appends an
`[account.personal]` block to `~/.config/drivel/config.toml`.

It also prints a `[[mount]]` block to go with it. Add it to the config file:

```toml
[[mount]]
account = "personal"
path    = "~/drive"                       # where the folder appears
data    = "~/.cache/drivel/personal"      # where the files really live
```

Then:

```sh
mkdir -p ~/drive ~/.cache/drivel/personal
drivel mount
```

`~/drive` is now your Drive. Edit files with anything; changes upload in the
background, and changes made elsewhere appear within seconds. Ctrl-C unmounts.

## Without a config file

A single mount needs no config at all — the flags are enough, and `mount` is the
default subcommand:

```sh
drivel login                              # writes ./credentials.json, ./token.json
drivel mount -mount ./mnt -data ./data \
  -credentials credentials.json -token token.json
```

Omit `-credentials` and Drivel runs **log-only**: it mounts and proxies, and
prints the changes it *would* sync without contacting Google. Useful for seeing
what it does before you point it at real data.

## What just happened

`~/drive` is a FUSE mount. Every operation on it is proxied to the backing
directory (`~/.cache/drivel/personal`), which holds the real files and is the
source of truth. Mutations become events on a queue that a sync engine drains in
the background, so no filesystem operation ever waits on the network. In the other
direction a poll loop follows Drive's change feed and applies remote edits to the
backing directory.

## Next

- If your Drive already has files in it, the first run enumerates them. In the
  default (eager) mode Drivel does **not** download them all unless you pass
  `-materialize`; in [lazy mode](lazy-mode.md) the whole tree appears immediately
  and costs nothing until you read a file.
- Before you point this at data you care about, read
  **[Data safety](data-safety.md)** — particularly what Drivel does when two
  machines edit the same file, and the guards on deletion.
