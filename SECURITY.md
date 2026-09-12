# Security

## Reporting a vulnerability

Please report security issues privately, through GitHub's **Report a
vulnerability** button on the repository's Security tab, rather than as a public
issue.

Useful in a report: what an attacker can do, what access they need to do it, and
a reproduction if you have one. Drivel is a small project with no security team
and no bounty; expect a human, not an SLA.

## Scope

Drivel is a **client you run yourself**. There is no hosted service, so there is
no server of ours to report a vulnerability against. What is in scope is the
program on your machine and what it does with your data and credentials.

The security-relevant surfaces, roughly in order of consequence:

**Data loss through sync.** Drivel can overwrite or delete files in the remote
store and in
the backing directory. Several invariants exist specifically to prevent that — a
placeholder is never uploaded, a deletion is inferred only from a sync baseline, a
partial write is never spliced into a remote file that has diverged. A way to
violate one of those is a security issue even if no attacker is involved.

**Storage backends are separate programs.** Drivel finds an executable named
`drivel-provider-<name>` on a search path, launches it, and hands it the
credentials for that mount. A backend is **not sandboxed, and is not meant to
be**: it runs as you, with your files, exactly as it would have if it were
compiled in. Installing one is trusting it, and that is not a vulnerability.

What *is* a vulnerability is anything that lets a file Drivel did not mean to run
become that backend. Drivel refuses a backend that is group- or world-writable or
that sits in a world-writable directory, builds the child's environment instead of
passing on its own, and speaks to it only over a private socket on your machine.
Those checks run against a path that is resolved again when the process is
launched, so they narrow that window rather than closing it.

**The placeholder marker.** In lazy mode, an extended attribute on the backing
file is the only record that a file's content has not been downloaded yet.
Anything that lets an unprivileged local process forge or strip that marker can
cause data loss — this is why extended-attribute passthrough is
[off by default](docs/user/lazy-mode.md#why--xattr-is-off-by-default).

**Credentials.** `credentials.json` and `token.json` are written `0600` and are
gitignored. They are yours: Drivel bundles no application secrets. If yours leaks,
revoke them with the provider — for Google Drive, delete the OAuth client in the
Cloud Console, which invalidates every token minted from it immediately.

**The profiling endpoint.** `-pprof` is off unless you pass an address. It serves
the process heap, which holds synced paths and buffered file content, to anyone
who can reach it. A non-loopback bind is warned about. Do not run it in
production.

**The mountpoint itself.** A FUSE mount is a filesystem; what other users on the
machine can do with it is governed by the usual permissions and by FUSE's
`allow_other` (which Drivel does not enable).

## What has not been looked at

Drivel has had **no systematic security review**. There has been no audit, no
threat model written down end to end, and no fuzzing. The surfaces listed above
are the ones its authors designed for, which is not the same list as the ones that
exist — several of them were found while building something unrelated, and the
honest expectation is that others have not been found yet.

Two areas have had the least adversarial attention, and are where someone looking
should probably start. Data arriving from a remote store — names, paths, sizes,
timestamps — decides what Drivel writes and where it writes it in your backing
directory, which is the classic shape for a traversal or overwrite bug. And the
places where the sync engine's concurrency meets the filesystem's, where a finding
is more likely to read as "this corrupts a file" than as "this grants access".

This is recorded as a fact rather than as a disclaimer. A report against a surface
nobody has reviewed is more useful than one against a surface that has been, not
less.

## Not in scope

- The security of macFUSE, of any storage provider Drivel is pointed at, or of
  your account with one.
- The consequences of running `-lazy` on a filesystem without extended attributes.
  Drivel detects this and warns; proceeding anyway is documented data loss, not a
  vulnerability.
- Anything requiring an attacker who already has write access to your backing
  directory or your credential files — at that point they have your data directly.
