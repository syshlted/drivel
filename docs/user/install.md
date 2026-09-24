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

## Install the command — and at least one backend

Drivel is two kinds of program. `drivel` is the command: it mounts the
filesystem, watches for changes, and keeps the local state. A **backend** is a
separate executable that talks to one storage service — `drivel-provider-gdrive`
for Google Drive, `drivel-provider-sftp` for an SFTP server — which `drivel`
starts for you when a mount needs it.

They are separate on purpose. Nothing that goes wrong inside a backend can take
your filesystem down: if it crashes, the mount stays up and Drivel starts it
again. It also means you only install the backends you actually use.

**You need at least one.** A `drivel` with no backend installed can still mount a
directory, but it has nothing to sync with.

```sh
go install github.com/syshlted/drivel/cmd/drivel@latest
go install github.com/syshlted/drivel/cmd/drivel-provider-gdrive@latest
```

Everything lands in `$(go env GOBIN)`, or `$(go env GOPATH)/bin` if `GOBIN` is
unset. Make sure that is on your `PATH`. Drivel looks for backends next to itself
first, so installing them the same way is all it takes.

If you want SFTP as well:

```sh
go install github.com/syshlted/drivel/cmd/drivel-provider-sftp@latest
```

To see what Drivel can find:

```sh
drivel mount -h        # errors from a mount name the backends that are installed
```

## Build from source

```sh
git clone https://github.com/syshlted/drivel
cd drivel
make build
```

`make build` produces `bin/drivel` and every backend beside it, which is what CI uses and produces a **statically linked** binary
with no library dependencies — it runs on any Linux of the same architecture,
whatever glibc that machine has. `make help` lists every target; contributors
should read [the developer build guide](../dev/building.md) instead of this page.

If you are **packaging Drivel for a distribution**, build with `make build
CGO_ENABLED=1` instead. That links the system's C library, so the package tracks
your distribution's patch level and its security updates arrive through your
package manager rather than waiting on a Drivel release.

Cross-compiling works for every Linux architecture, `darwin/amd64`,
`darwin/arm64` and FreeBSD. Remember the backends:

```sh
GOOS=darwin GOARCH=arm64 go build ./cmd/drivel
GOOS=darwin GOARCH=arm64 go build ./cmd/drivel-provider-gdrive
```

Windows is not supported — see [Platform support](platforms.md#windows).

## Installing system-wide

From a checkout, one target places everything:

```sh
sudo make install         # PREFIX=/usr/local  MANDIR=$PREFIX/share/man  SBINDIR=/sbin
```

| What | Where |
| --- | --- |
| The binary | `$PREFIX/bin/drivel` |
| The backends | `$PREFIX/lib/drivel/plugins/drivel-provider-*` |
| The man page | `$MANDIR/man1/drivel.1` |
| The `mount(8)` helper | `$SBINDIR/mount.fuse.drivel` and `$SBINDIR/mount.drivel`, both symlinks to the binary |
| Shell completions | `$PREFIX/share/bash-completion/completions/drivel`, `$PREFIX/share/zsh/site-functions/_drivel` |

The backends go under `lib` rather than `bin` because they are not commands you
run: started from a shell, one prints a handshake line and exits. Drivel refuses
to start a backend that is group- or world-writable, or one whose directory is, so
install them as root and leave them `0755` — a backend runs with your credentials,
and a file anyone could replace is a file anyone could replace *with*.

`SBINDIR` defaults to `/sbin` rather than to something under `PREFIX` because
`mount(8)` searches `/sbin` and `/usr/sbin` only. Those two symlinks are what let
a mount be an `/etc/fstab` line — see [mounting from /etc/fstab](fstab.md). They
are placed on every platform but only work on Linux.

`sudo make uninstall` removes exactly what this placed, and nothing else.

## Shell completions

`sudo make install` places them, so if you installed that way there is nothing to
do — start a new shell and `drivel mount -<TAB>` works.

If you installed some other way — a downloaded binary, `go install`, a copy you
built — **Drivel prints its own completion script**, so there is no file to fetch
from anywhere:

```sh
drivel completion bash
drivel completion zsh
```

To try it in the shell you are in right now:

```sh
eval "$(drivel completion bash)"
```

That works, but it runs Drivel every time a shell starts. For a permanent install,
write the script to the place your shell already looks:

```sh
# bash — per-user
drivel completion bash > ~/.local/share/bash-completion/completions/drivel

# bash — system-wide
drivel completion bash | sudo tee /usr/share/bash-completion/completions/drivel >/dev/null

# zsh — into a directory on your $fpath; the filename must be _drivel
mkdir -p ~/.zsh/completions
drivel completion zsh > ~/.zsh/completions/_drivel
autoload -Uz compinit && compinit
```

For zsh the file is the better route rather than merely the tidier one: `_drivel`
is an autoloaded function file, so the `eval` form only works if it comes *after*
`compinit` in your `~/.zshrc`.

The script is **generated from Drivel's own flag definitions** at the moment you
run the command, so it knows every flag your binary does — including which take a
directory and which take one of a fixed set of words. `-drive-delete <TAB>` offers
`trash` and `permanent`.

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
| `~/.local/share/drivel/plugins/` | Backends you installed for yourself, if you did not install them system-wide. |

Both credential files are written `0600`. Neither the state database nor the
path index is a credential, and neither is authoritative: deleting them costs API
round trips, never data. See [Data safety](data-safety.md#the-databases-are-caches).

Without `-account`, the flag defaults put `credentials.json`, `token.json`,
`drivel-state.db` and `drivel-index.db` in the working directory instead.

## Uninstalling

```sh
fusermount3 -u ~/drive                    # unmount first (or just Ctrl-C the process)

sudo make uninstall                       # if you installed with `sudo make install`
rm "$(command -v drivel)"                 # if you installed with `go install`
rm ~/go/bin/drivel-provider-*             # ...and the backends it installed beside it

rm -rf ~/.config/drivel ~/.local/state/drivel ~/.local/share/drivel
```

`make uninstall` takes the backends, the man page, the `mount(8)` helper symlinks
and the shell completions with it; `rm "$(command -v drivel)"` removes only the
one binary, which is why the backends need the line after it. Remove any `/etc/fstab`
lines of type `fuse.drivel` or `drivel` as well.

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
