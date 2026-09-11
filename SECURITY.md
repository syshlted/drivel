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

## Not in scope

- The security of macFUSE, of any storage provider Drivel is pointed at, or of
  your account with one.
- The consequences of running `-lazy` on a filesystem without extended attributes.
  Drivel detects this and warns; proceeding anyway is documented data loss, not a
  vulnerability.
- Anything requiring an attacker who already has write access to your backing
  directory or your credential files — at that point they have your data directly.
