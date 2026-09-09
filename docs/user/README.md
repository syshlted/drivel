# Using Drivel

Drivel mounts a folder that is really two things at once: an ordinary local
directory you can open, edit and save in, and a Google Drive folder that syncs in
the background. Reads never touch the network. Uploads and downloads happen off
to the side.

Start here:

1. **[Quickstart](quickstart.md)** — install, log in, mount. About ten minutes,
   most of it waiting on Google.
2. **[Installing](install.md)** — the longer version: building from source, the
   FUSE helper, shell completions, the man page, and how to uninstall.
3. **[Google credentials](google-cloud-setup.md)** — Drivel has no shared app
   identity, so you create your own OAuth client. One-time, free.

Then, as you need them:

- **[Configuration](configuration.md)** — every flag, every config-file key, and
  how the two relate.
- **[Lazy mode](lazy-mode.md)** — make a whole Drive visible without downloading
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

It is a **client you run yourself**. There is no Drivel service, no shared
application identity, and no analytics or telemetry — nothing phones home. Drive
traffic goes directly between your machine and Google under credentials you
create and can revoke.

It is not a caching layer over a network filesystem: the backing directory holds
real files, and they stay readable whether or not Drivel is running, whether or
not you still have the program. See [getting your data out](install.md#getting-your-data-out).
