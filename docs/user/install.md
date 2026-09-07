# Installing Drivel

## Requirements

| | |
| --- | --- |
| **Go** | 1.27 or newer, to build. There are no pre-built binaries yet. |
| **A FUSE mount helper** | `fusermount3` on Linux (the `fuse3` package), macFUSE on macOS, the `fusefs` kernel module on FreeBSD. |
| **A Google account** | And your own OAuth client — see [Google credentials](google-cloud-setup.md). |

Drivel is pure Go with no cgo, so a build needs nothing but the toolchain. The
FUSE helper is a **runtime** requirement: the binary builds and runs without it
and fails at mount time with `exec: "fusermount3": executable file not found`.

## The FUSE helper

```sh
sudo apt install fuse3        # Debian, Ubuntu
sudo dnf install fuse3        # Fedora, RHEL
sudo pacman -S fuse3          # Arch
```

`libfuse3-dev` is headers only and is **not** what you need.

On **macOS**, install [macFUSE](https://macfuse.github.io/). Drivel does not
bundle it and will not install it for you; FUSE-T is not a substitute (see
[Platform support](platforms.md#macos)).

On **FreeBSD** the module is in base but is not loaded by default:

```sh
kldload fusefs
sysctl vfs.usermount=1
```

Add `fusefs_load="YES"` to `/boot/loader.conf` to make it persist.

## Install the command

```sh
go install github.com/zishmusic/drivel/cmd/drivel@latest
```

The binary lands in `$(go env GOBIN)`, or `$(go env GOPATH)/bin` if `GOBIN` is
unset. Make sure that is on your `PATH`.

## Build from source

```sh
git clone https://github.com/zishmusic/drivel
cd drivel
go build -o ./bin/drivel ./cmd/drivel
```

Or `make build`, which does the same thing and is what CI uses. `make help` lists
every target; contributors should read [the developer build
guide](../dev/building.md) instead of this page.

Cross-compiling works for every Linux architecture, `darwin/amd64`,
`darwin/arm64` and FreeBSD:

```sh
GOOS=darwin GOARCH=arm64 go build ./cmd/drivel
```

Windows is not supported — see [Platform support](platforms.md#windows).

## Shell completions

They live in [`completions/`](../../completions):

```sh
# bash — system-wide, or source it from ~/.bashrc
sudo cp completions/drivel.bash /usr/share/bash-completion/completions/drivel

# zsh — into a directory on your $fpath; the filename must be _drivel
cp completions/_drivel ~/.zsh/completions/_drivel
autoload -Uz compinit && compinit
```

## Man page

```sh
man ./docs/user/drivel.1                          # read it in place
sudo install -m0644 docs/user/drivel.1 /usr/local/share/man/man1/drivel.1
```

## Where Drivel keeps things

With `-account NAME`, everything follows the XDG base directories:

| Path | Holds |
| --- | --- |
| `~/.config/drivel/config.toml` | Accounts and mounts. |
| `~/.config/drivel/NAME/credentials.json` | Your OAuth client ID and secret. **Secret.** |
| `~/.config/drivel/NAME/token.json` | The access/refresh token. **Secret.** |
| `~/.local/state/drivel/NAME/state.db` | Sync bookkeeping — the change cursor and echo records. |
| Wherever `data =` points | **Your files.** |

Both credential files are written `0600`. Neither the state database nor the
path index is a credential, and neither is authoritative: deleting them costs API
round trips, never data. See [Data safety](data-safety.md#the-databases-are-caches).

Without `-account`, the flag defaults put `credentials.json`, `token.json`,
`drivel-state.db` and `drivel-index.db` in the working directory instead.

## Uninstalling

```sh
fusermount3 -u ~/drive                    # unmount first (or just Ctrl-C the process)
rm "$(command -v drivel)"
rm -rf ~/.config/drivel ~/.local/state/drivel
```

Revoke the OAuth client from the [Google Cloud
Console](https://console.cloud.google.com/apis/credentials) if you are done with
it — deleting the client immediately invalidates every token minted from it.

## Getting your data out

**You do not need Drivel to read your files.** The backing directory — whatever
`data =` or `-data` points at — is a plain directory of plain files. Unmount and
it is still there, and everything in it opens with any program.

Two things to know before you walk away:

- **In [lazy mode](lazy-mode.md), placeholders are not files yet.** A file that
  has never been read is a zero-byte hole marked with an extended attribute; its
  content is still only in Drive. Before uninstalling, either read everything you
  want to keep (`find ~/.cache/drivel/personal -type f -exec cat {} + >/dev/null`)
  or fetch it from Drive directly.
- **In-place mode has no separate directory** — the mounted directory *is* the
  backing store, and the files simply stay in it when Drivel exits.

Conflict copies (files named `NAME (conflict 2026-09-07 12-34-56).ext`) are
local-only by design and were never uploaded. They are yours to keep or delete.
