# Mounting drivel from /etc/fstab

Drivel is a `mount(8)` helper as well as a command. Installed as
`/sbin/mount.fuse.drivel`, it can be described in `/etc/fstab` and brought up by
`mount`, by `mount -a` and by systemd at boot, with no terminal attached and
nothing to type.

**Linux only.** The helper protocol, the `/sbin/mount.<type>` lookup, the
privilege drop and the startup handshake are all specific to how Linux mounts a
filesystem. On macOS and FreeBSD, run `drivel mount` from a launchd agent or an
rc.d script instead.

## Install

```sh
sudo make install          # PREFIX=/usr/local SBINDIR=/sbin
```

That places the binary, the man page, and two symlinks back to it:

```
/sbin/mount.fuse.drivel -> /usr/local/bin/drivel     # fstab type: fuse.drivel
/sbin/mount.drivel      -> /usr/local/bin/drivel     # fstab type: drivel
```

They are the same binary; `mount(8)` selects a program by the filesystem type,
and the name it was reached by is the whole difference. `SBINDIR` defaults to
`/sbin` rather than to something under `PREFIX` because `mount(8)` searches
`/sbin` and `/usr/sbin` only.

**Prefer the `fuse.drivel` type.** go-fuse mounts as `fuse.` + its subtype, so
that is what `/proc/self/mountinfo` reports whichever type you write — and
systemd's fstab-generator compares the fstab type against what the kernel says.
`drivel` is installed as an alias for people who want it.

## A first line

Authenticate once, interactively, as the user who will own the files:

```sh
drivel login -account work
```

Then, in `/etc/fstab`:

```
work  /home/j/Drive  fuse.drivel  _netdev,nofail,run-as=j,account=work,data=/var/cache/drivel/work  0 0
```

```sh
sudo mount /home/j/Drive        # or: sudo mount -a
findmnt /home/j/Drive
sudo umount /home/j/Drive
```

`drivel mount` never prompts, so nothing about this is interactive — but it does
need a token, and `drivel login` is the only thing that can write one. A mount
whose account has never logged in fails at `mount`, which is the right place for
it to fail.

## The fields

| field | meaning |
|---|---|
| 1, device | Names the mount. In `config=` mode it selects the `[[mount]]` entry; otherwise it is the mount's name and the source `findmnt(8)` shows. `drivel` and `none` are treated as filler, and the mountpoint's basename is used instead. |
| 2, dir | The mountpoint. Must be absolute — at boot the working directory is `/`. |
| 3, type | `fuse.drivel` (preferred) or `drivel`. |
| 4, options | Below. |
| 5, 6 | `0 0`. There is nothing to dump and nothing to fsck. |

## Options

Two ways to say what to mount, and they cannot be mixed — the same rule
`-config` enforces on the command line, for the same reason: a precedence rule
between them is one nobody would remember.

**Config-file mode.** Everything lives in the TOML file, and the fstab line only
points at it:

```
work  /home/j/Drive  fuse.drivel  _netdev,nofail,run-as=j,config=/home/j/.config/drivel/config.toml  0 0
```

The device field selects the `[[mount]]` whose `name` matches (or `name=` does).
A file with exactly one `[[mount]]` needs neither. The entry's `path` must equal
the fstab mountpoint: if the two disagree, the fstab line is what `umount`,
`mount -a` and systemd's unit all key on afterwards, so mounting the other path
would leave a mount nothing can address.

**Option mode.** The `drivel mount` flags, one for one:

| option | flag | notes |
|---|---|---|
| `data=PATH` | `-data` | Backing directory. Omit for in-place mode. |
| `account=NAME` | — | Credentials and token from `$XDG_CONFIG_HOME/drivel/NAME`, as `drivel login -account NAME` wrote them. |
| `credentials=PATH` | `-credentials` | Explicit, instead of `account=`. Without either, the mount is log-only: no network, no sync. |
| `token=PATH` | `-token` | |
| `state=PATH` | `-state` | Defaults to `$XDG_STATE_HOME/drivel/NAME/state.db`. |
| `index=PATH` | `-index` | Defaults beside the state DB. `index=` (empty) disables it. |
| `drive-root=ID` | `-drive-root` | |
| `drive-delete=MODE` | `-drive-delete` | `trash` (default) or `permanent`. See [data safety](data-safety.md#where-a-deleted-file-goes). |
| `drive-sweep-mode=MODE` | `-drive-sweep-mode` | `auto` (default), `scoped` or `flat`. |
| `name=NAME` | — | Overrides the device field. |
| `lazy` | `-lazy` | Needs `account=` or `credentials=`. |
| `xattr` | `-xattr` | Off by default, and [that default is a safety property](../DESIGN.md#21-fuse-layer). |
| `resync`, `materialize` | same | |
| `max-deletes=N` | `-max-deletes` | |
| `sweep-interval=D` | `-sweep-interval` | A Go duration: `24h`, `90m`. |
| `upload-workers=N` | `-upload-workers` | Defaults to 4. `0` is refused, not read as "the default". |
| `hydrate-workers=N` | `-hydrate-workers` | Defaults to 8. Only matters with `lazy`. |

Every flag that describes a mount has an option here; a test enforces that, so a
flag that grows on the command line cannot go missing from fstab. `-pprof` and
`-pprof-allow-remote` are the deliberate exception — they open a debug endpoint
for the *process*, which is not something an unattended boot mount should be
doing. Run `drivel mount` by hand when you need to profile one.

Process and mount options work in **both** modes:

| option | meaning |
|---|---|
| `run-as=NAME` | Run the daemon as this account. See below. |
| `foreground` | Do not fork. For debugging; an fstab line must not use it. |
| `logfile=PATH` | Where the daemon logs. Without it the log goes to `/dev/null` — see below. |
| `mount-timeout=D` | Give up if the filesystem is not live within `D`. Off by default. |
| `debug` | FUSE tracing. |
| `fsname=NAME` | Override the source column. Defaults to the device field. |
| `allow_other` | Let other users reach the mount. Unless the daemon is root — so, with `run-as=` — `fusermount3` refuses this without `user_allow_other` in `/etc/fuse.conf`. |
| `allow_root`, `default_permissions`, `nosuid`, `nodev`, `noexec`, `noatime`, `sync`, `dirsync` | Passed to the mount backend verbatim. |

`defaults`, `auto`, `noauto`, `_netdev`, `nofail`, `user`, `users`, `owner`,
`group`, `rw`, the `atime` family, `comment=` and every `x-*` option are handled
by `mount(8)` or systemd and are accepted and ignored here.

**`ro` is refused**, along with `remount`, `bind` and `move`. Drivel has no
read-only mode, and a line that says `ro` over a writable mount would be a lie.
An unknown option is an error too — pass `-s` (which `mount -s` sends) to
downgrade that to a warning.

## Running as a user, not as root

At boot the helper runs as root, but the token, the backing tree and the mount
belong to somebody. `run-as=NAME` resolves that account, moves `HOME` and the
XDG variables to it, and gives up root — groups first, then gid, then uid —
before anything reads a credential.

```
work  /home/j/Drive  fuse.drivel  _netdev,nofail,run-as=j,account=work,data=/var/cache/drivel/work  0 0
```

Files created through the mount are then owned by `j`, and
`$XDG_CONFIG_HOME/drivel/work` resolves to `j`'s token rather than root's.

Note the option is **`run-as=`, not `user=`**. `user` is a standard fstab option
meaning "a non-root user may mount this", and util-linux passes `user=NAME` down
to the helper to record who did — so treating it as a privilege-drop request
would act on an option nobody aimed at drivel.

Without `run-as=`, a boot mount runs as root and every backing file is
root-owned. `allow_other` makes the tree reachable by other accounts, but it
opens it to *every* account on the machine; prefer `run-as=`.

The lookup reads `/etc/passwd` (drivel is built without cgo), so an account that
exists only in LDAP or SSSD will not be found. The mount fails rather than
running as the wrong user.

## systemd

`_netdev` orders the mount after the network — drivel needs it — and `nofail`
keeps a failure from blocking boot. Both are systemd's, not drivel's.

For a mount that should come up on first access rather than at boot:

```
work  /home/j/Drive  fuse.drivel  _netdev,nofail,x-systemd.automount,x-systemd.idle-timeout=10min,run-as=j,account=work,data=/var/cache/drivel/work  0 0
```

The daemon's own log is not in the journal — see `logfile=` below; only the
helper's startup failures reach `mount`, and through it systemd.

The helper forks a daemon that outlives it, and that daemon stays in the
generated `.mount` unit's cgroup. **This has not been verified on a systemd host**
— the container this was developed in has no systemd. If the daemon turns out to
be killed when `ExecMount` finishes, the answer is `x-systemd.automount` as
above, or a `drivel@.service` with `x-systemd.requires=`. Please report what you
see.

## When it does not come up

`mount` reports the helper's failure directly, because the helper does not exit
until the filesystem is live:

```sh
sudo mount -v /home/j/Drive          # -v: log the resolved mount
sudo mount -f /home/j/Drive          # -f: validate everything, mount nothing
```

`-f` is the fast way to check an fstab line, and it checks everything a real
mount would: the options, the `run-as=` account, the config entry, and the
cross-mount validation that refuses (say) a state DB inside a backing tree. What
it does not do is touch the filesystem, drop privileges or create the log file —
a dry run should leave nothing behind.

**Set `logfile=` on any line you may need to debug.** Once the helper has
returned, the daemon's stdout and stderr go to `/dev/null` unless `logfile=`
names somewhere else. That is deliberate and it is what every other FUSE helper
does: a backgrounded daemon holding the caller's stdout open blocks anything
capturing it through a pipe — `out=$(mount -a 2>&1)` would hang until the
filesystem was unmounted. Nothing is lost where it matters, because a failure
*before* the mount is live comes back through `mount` itself.

`umount` stops the daemon; there is no separate process to kill.

```sh
sudo umount /home/j/Drive
fusermount3 -u /home/j/Drive         # if it is busy and you mounted it yourself
```

To test an fstab line without editing `/etc/fstab`:

```sh
sudo mount -a -T ./my-fstab
```

(`umount` has no `-T`; unmount by path.)
