# SFTP

Drivel can sync to any server you have an SSH account on. There is nothing to
install at the other end: SFTP is a subsystem of OpenSSH, so if you can `ssh` to a
machine you can already point drivel at it.

What drivel adds over `sshfs` — which is the honest comparison — is that your
filesystem never waits for the network. A write lands in the local backing
directory and returns; the upload happens behind it, and retries if the link is
down. Add [lazy mode](lazy-mode.md) and a large remote tree is browsable at full
apparent size while holding almost none of it locally. And when two machines edit
the same file, you get a conflict copy rather than a silent loss.

## Setting it up

First, install the backend. Drivel runs its storage backends as separate programs,
so the SFTP one has to be there for a mount to use it:

```sh
go install github.com/zishmusic/drivel/cmd/drivel-provider-sftp@latest
```

(or `make build` / `sudo make install` from a checkout, which build every backend).

SFTP is then configured through [the config file](configuration.md#the-config-file).
There are no `-sftp-*` flags; the mount flags are Drive-shaped by history.

```toml
[account.server]
provider    = "sftp"
host        = "files.example.com"
user        = "you"
key         = "~/.ssh/id_ed25519"
root        = "/srv/share"

[[mount]]
account        = "server"
path           = "~/share"
data           = "~/.cache/drivel/server"
sweep-interval = "5m"          # see "How changes reach you" — the default is wrong here
```

Then `drivel mount`.

### Before the first mount

**The server's host key must already be in your `known_hosts` file.** Drivel never
trusts a key on first use and there is no option to make it — a mount can be
brought up by `/etc/fstab` at boot with nobody watching, so there is nobody to
answer a prompt. If you have ever `ssh`'d to the server from this account you are
already done. Otherwise:

```sh
ssh-keyscan -H files.example.com >> ~/.ssh/known_hosts
```

Check the fingerprint against the server before you trust it. Drivel refuses to
mount an unknown host and says which file to add it to, so you will not get this
wrong silently.

## Options

These live in the account table (or `[mount.provider]`, which overrides it).

| Key | Default | What it does |
| --- | --- | --- |
| `host` | — | The server. Required. No port here — use `port`. |
| `port` | `22` | |
| `user` | — | The SSH account to log in as. Required. |
| `key` | `""` | A private key file. Must not be passphrase-protected — see below. |
| `certificate` | `""` | A signed certificate to present with `key`. |
| `agent` | `true` | Use `$SSH_AUTH_SOCK`. Set `false` to ignore any agent. |
| `known-hosts` | `~/.ssh/known_hosts` | Where the server's key is verified against. |
| `root` | login directory | The remote directory the mount maps to. |
| `connect-timeout` | `"30s"` | Bounds connecting, not transferring. A slow upload is not a failure. |
| `concurrent-requests` | library default | Read/write packets in flight per file. Raise it on a long, fat link. |

### Authentication

Keys, certificates and an agent. **There is no password option and no passphrase
option, and this is deliberate** — either one means a credential sitting in
cleartext beside the file it protects, for an account that usually also grants a
shell. Use an agent for an encrypted key:

```sh
ssh-add ~/.ssh/id_ed25519
```

An `/etc/fstab` mount has no agent and no environment, so give it a dedicated
unencrypted key with a restricted `authorized_keys` entry rather than reaching for
a password.

## How changes reach you

**SFTP has no change notification of any kind.** There is no feed to subscribe to,
so drivel finds out what changed remotely by walking the tree — the same
enumeration sweep that reconciles a Drive that existed before you first mounted
it, run on a schedule.

That makes **`sweep-interval` the poll interval**, and its 24-hour default is
tuned for a backend that also has a live feed. It is wrong for SFTP. Set it to
what you can tolerate as inbound latency:

```toml
sweep-interval = "5m"
```

A sweep costs one directory listing per directory under your root, and nothing is
downloaded to perform one. Minutes are reasonable for a normal tree; seconds are
not, on any tree large enough to care about.

Outbound is unaffected — a local change is uploaded within seconds, as always.
`sweep-interval = 0` disables the sweep entirely, which on this backend means
disabling inbound sync entirely.

## Deletion has no undo here

On Google Drive a removal goes to the trash, so a deletion drivel got wrong is
recoverable for 30 days. **A server reached over SFTP has no trash, and drivel
does not invent one** — a hidden directory of deleted files would sit inside the
tree being synced, where the next sweep would enumerate it and pull every deleted
file back down.

So `rm` is `rm`. That matters most for the deletions you did *not* type: a sweep
can *infer* one, from a wrong premise like a mount pointed at the wrong `root` or
a backing directory that was emptied. The guard is
[`-max-deletes`](configuration.md#mount-options), which abandons a whole reconcile
pass rather than trimming it when the count looks wrong — and on this backend it
is not the guard in front of a safety net, it is the only guard. Its default of
100 is a reasonable place to start; lower it if your tree is small.

Keep a real backup of anything that matters, as always. See
[data safety](data-safety.md).

## What it costs, and one thing to know

**Editing part of a large file uploads only that part.** SFTP writes at an
offset, so changing one block of a 4 GB file transfers one block. Drive cannot do
this at all — it replaces the whole object — which makes this the backend where
that optimisation actually runs.

**A file that is rewritten with identical content is uploaded again.** Drivel
skips a redundant upload by comparing a server-computed checksum, and no SFTP
server offers one by default. The upload is correct, just wasted. Editing *part*
of a large file is unaffected — that takes the offset path above, not this one.

**A remote edit can be missed in one narrow case.** With no checksum available,
drivel identifies a remote file by its modification time and size — the same
heuristic `rsync` uses without `--checksum`. SFTP reports modification times in
whole seconds, so a remote edit that keeps a file's byte length *exactly* and
lands in the same second as drivel's own last write to it is indistinguishable
from drivel's own echo, and is not pulled until something else touches the file.
It takes deliberate effort to hit; it is listed because it is real.

**Symbolic links, sockets, devices and named pipes are skipped**, in both
directions, and drivel says how many it stepped over at the end of a sweep. There
is no honest way to carry them through a byte-stream interface, and following a
link during a sweep would let a loop run forever.

**Permissions are not synced.** A file drivel uploads gets whatever mode your SSH
account's umask gives it. Ownership, ACLs and extended attributes do not travel.

## When something is wrong

`drivel mount -debug` logs every operation. The common failures:

- **`no host key for … in ~/.ssh/known_hosts`** — see [before the first
  mount](#before-the-first-mount).
- **`HOST KEY MISMATCH`** — the server offers a key that is not the one you
  recorded. Either it was rebuilt, or something is intercepting the connection.
  Drivel refuses to connect. Confirm the new fingerprint out of band before you
  edit `known_hosts`.
- **`key … is passphrase-protected`** — load it into an agent (`ssh-add`).
- **`no authentication configured`** — set `key`, or start an agent.
- **remote changes never arrive** — `sweep-interval` is still at its 24-hour
  default. See [above](#how-changes-reach-you).

A dropped connection is not an error you need to act on: the operation is retried
and reconnects by itself.

## Platforms

The SFTP backend is pure Go and builds everywhere Drivel does. What limits where
you can *mount* is unchanged — see [platforms](platforms.md).

It is tested against **OpenSSH** — the `sshd` almost everything runs. Other
servers speak the same protocol and should work; they have not been run.
