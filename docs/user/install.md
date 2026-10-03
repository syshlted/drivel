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
go install github.com/syshlted/drivel/cmd/drivel@latest
```

That is everything. The binary lands in `$(go env GOBIN)`, or
`$(go env GOPATH)/bin` if `GOBIN` is unset; make sure that is on your `PATH`.

One file carries the command and both of the backends that ship with it — Google
Drive and SFTP. A **backend** is the part that talks to one storage service, and
Drivel still runs it as a **separate process**: it starts a copy of itself to be
the backend a mount asked for. That is not a detail you have to manage, but it is
why nothing that goes wrong inside a backend takes your filesystem down. If it
crashes, the mount stays up and Drivel starts it again.

To see which backends a `drivel` has:

```sh
drivel mount -h        # errors from a mount name the backends available
```

### Backends that do not ship with Drivel

A backend can also be its own executable, named `drivel-provider-<kind>`, which
Drivel finds and launches. That is how a backend somebody else wrote reaches you,
and how a distribution can package each one separately. Drivel looks for them
next to itself, then in `~/.local/share/drivel/plugins`,
`/usr/local/lib/drivel/plugins` and `/usr/lib/drivel/plugins`.

A backend built into Drivel wins over an installed file of the same name, and the
mount's log says which file it ignored — so there is never a question about which
one you are running.

Drivel refuses to start an installed backend that is group- or world-writable, or
one whose directory is: a backend runs with your credentials, and a file anyone
could replace is a file anyone could replace *with*.

## Build from source

```sh
git clone https://github.com/syshlted/drivel
cd drivel
make build
```

`make build` produces `bin/drivel`: one **statically linked** binary with no
library dependencies and both backends inside it, which runs on any Linux of the
same architecture whatever glibc that machine has. `make help` lists every
target; contributors should read [the developer build
guide](../dev/building.md) instead of this page.

Two variations exist and neither is the usual choice. `make build-plugins` builds
the shipped backends as separate `drivel-provider-*` executables as well, which
is what an out-of-tree backend is built like. `make build TAGS=nobundle` builds a
`drivel` with no backend inside it at all, which only makes sense alongside
those files — it is the shape a distribution wants when it ships one package per
backend.

If you are **packaging Drivel for a distribution**, build with `make build
CGO_ENABLED=1` instead. That links the system's C library, so the package tracks
your distribution's patch level and its security updates arrive through your
package manager rather than waiting on a Drivel release.

Cross-compiling works for every Linux architecture, `darwin/amd64`,
`darwin/arm64` and FreeBSD, and one command is the whole of it:

```sh
GOOS=darwin GOARCH=arm64 go build ./cmd/drivel
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
| The man page | `$MANDIR/man1/drivel.1` |
| The `mount(8)` helper | `$SBINDIR/mount.fuse.drivel` and `$SBINDIR/mount.drivel`, both symlinks to the binary |
| Shell completions | `$PREFIX/share/bash-completion/completions/drivel`, `$PREFIX/share/zsh/site-functions/_drivel` |

There is no backend to install: they are in the binary. With
`make install TAGS=nobundle` they are separate files instead, and land in
`$PREFIX/lib/drivel/plugins/` — under `lib` rather than `bin` because they are not
commands you run, since started from a shell one prints a handshake line and
exits. Install those as root and leave them `0755`, or Drivel will refuse to
start them.

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
eval "$(drivel completion bash)"                    # bash
autoload -Uz compinit && compinit                   # zsh: once, if you have not
eval "$(drivel completion zsh)"                     # zsh
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

For zsh the file is the better route rather than merely the tidier one, and the
`eval` form has one ordering rule: it must come *after* `compinit` in your
`~/.zshrc`, because `compinit` is what defines the `compdef` the script uses to
register itself. Put it earlier and it prints a line saying so and does nothing.

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
| `~/.local/share/drivel/plugins/` | Backends you installed for yourself as separate executables, if you installed any. |

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
rm ~/go/bin/drivel-provider-*             # ...and any separate backends, if you installed some

rm -rf ~/.config/drivel ~/.local/state/drivel ~/.local/share/drivel/plugins
```

> [!CAUTION]
> **Do not `rm -rf ~/.local/share/drivel`.** A mount `drivel login` set up backs
> onto `~/.local/share/drivel/mounts/NAME`, and that directory holds your files,
> not a copy of them — see [Getting your data out](#getting-your-data-out) below.
> The line above deletes the plugin directory only.

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
  want to keep (`find ~/.local/share/drivel/mounts/personal -type f -exec cat {} +
  >/dev/null`) or fetch it from Drive directly.
- **In-place mode has no separate directory** — the mounted directory *is* the
  backing store, and the files simply stay in it when Drivel exits.

Conflict copies (files named `NAME (conflict 2026-09-07 12-34-56).ext`) are
local-only by design and were never uploaded. They are yours to keep or delete.
