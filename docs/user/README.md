# Using Drivel

Drivel mounts a folder that is really two things at once: an ordinary local
directory you can open, edit and save in, and a folder on a cloud storage
provider that syncs in the background. Reads never touch the network. Uploads and
downloads happen off to the side. Two backends ship today: **Google Drive** and
**SFTP** — any server you have an SSH account on.

Start here:

1. **[Quickstart](quickstart.md)** — install, log in, mount. About ten minutes,
   most of it waiting on Google.
2. **[Installing](install.md)** — the longer version: building from source, the
   FUSE helper, shell completions, the man page, and how to uninstall.
3. **Credentials for your backend** — Drivel ships none of its own. For Google
   Drive you create your own OAuth client, which is one-time and free:
   **[Google Cloud setup](google-cloud-setup.md)**. For **[SFTP](sftp.md)** you
   already have them — it is your SSH key.

Then, as you need them:

- **[Configuration](configuration.md)** — every flag, every config-file key, and
  how the two relate.
- **[SFTP](sftp.md)** — sync to any machine you can `ssh` to. No server-side
  software, and no host key trusted on first use.
- **[Lazy mode](lazy-mode.md)** — make a whole remote tree visible without downloading
  it, and the filesystem requirement that makes that safe.
- **[Data safety](data-safety.md)** — what happens when two machines edit the same
  file, when Drivel will delete something, and when it refuses to.
- **[Troubleshooting](troubleshooting.md)** — the errors you are most likely to
  hit, and what each one means.
- **[Platform support](platforms.md)** — Linux, macOS, FreeBSD; what is verified
  and what is not.
- **[Mounting from /etc/fstab](fstab.md)** — describe a mount as an fstab line and
  bring it up with `mount -a` or at boot, with no terminal attached. Linux only.

The [`drivel(1)` man page](drivel.1) is the complete reference for both
subcommands: `man ./docs/user/drivel.1`.

## What Drivel is not

It is a **client you run yourself**: there is no Drivel service, and it ships no
credentials of its own. You supply the ones for the backend you point it at, and
they stay yours to inspect and revoke.

It is not a caching layer over a network filesystem: the backing directory holds
real files, and they stay readable whether or not Drivel is running, whether or
not you still have the program. See [getting your data out](install.md#getting-your-data-out).
