# Releasing

A release is a signed `v*` tag. Pushing one runs
[`.github/workflows/release.yml`](../../.github/workflows/release.yml), which
builds everything and leaves a **draft** GitHub release for a person to read and
publish. Nothing is built by hand, and nothing becomes public until someone
presses Publish — except the tag itself, which is public the moment it is
pushed.

Getting the module listed on pkg.go.dev, and retracting a bad version, are in
[Publishing to pkg.go.dev](../project/publishing.md); this page links there
rather than restating either.

## What a release contains

`make dist` builds it, into `dist/`, by running
[`scripts/dist.sh`](../../scripts/dist.sh):

| File | For |
| --- | --- |
| `drivel-VERSION-OS-ARCH.tar.gz` | Every platform: the binary, `drivel.1`, both completion scripts, `LICENSE`, `README.md`, `CHANGELOG.md`. |
| `drivel_VERSION_ARCH.deb` | Debian and Ubuntu. Installs `/usr/bin/drivel`, the two `mount(8)` helper links in `/usr/sbin`, the man page and the completions; depends on `fuse3`. |
| `drivel-VERSION-1.ARCH.rpm` | Fedora and other RPM distributions. The same files, at each family's paths. |
| `SHA256SUMS` | A checksum of every file above. |

The packages are made by [nfpm](https://nfpm.goreleaser.com), pinned in the
Makefile beside the other tools and pure Go like them, so the build machine needs
neither `dpkg-deb` nor `rpmbuild`. Their description, dependencies and file list
are [`packaging/nfpm.yaml`](../../packaging/nfpm.yaml). FreeBSD gets the tarball
only: nfpm does not make FreeBSD's `pkg` format.

Each file also gets a **build-provenance attestation**, signed with the
workflow's own identity through Sigstore. A checksum says a download is intact;
the attestation says this repository's release workflow built it, from which
commit. A user checks one with:

```sh
gh attestation verify drivel_0.1.0_amd64.deb --repo syshlted/drivel
```

### Three properties, each checked rather than assumed

- **The version comes from the tag, and nothing passes it in.** Go records the
  module version in every binary it builds: the tag on a tagged commit, a
  pseudo-version otherwise, `+dirty` for a tree with uncommitted changes.
  `dist.sh` reads it back out of each binary with `go version -m` — which works
  for a binary this machine cannot run — and `make dist RELEASE_TAG=v0.1.0`
  refuses unless every one of them says exactly `v0.1.0`. So a release cannot be
  built from the wrong commit, or from a tree with changes in it, and
  `drivel version` cannot disagree with the file name.
- **A release passed the gates on the commit it was built from.** The workflow
  runs the whole of [`ci.yml`](../../.github/workflows/ci.yml) on the tagged
  commit first (it is a reusable workflow for this reason). CI has usually run
  on that commit already, but a tag can name any commit.
- **The same commit builds the same bytes.** `-trimpath` keeps the build
  machine's paths out of the binary, and the archives and packages carry the
  commit's time rather than the time of the build, so building one commit twice
  gives identical files, packages included. That is what lets anyone check a
  release against their own build of the tag.

### The platform list is the support claim

`DIST_PLATFORMS` in the Makefile is the list, and it is short on purpose. A
platform goes in once the test suite has **run on it**, not once it compiles —
the same line [platform support](../user/platforms.md) draws. Today that is
`linux/amd64` and `freebsd/amd64`. Linux arm64 and FreeBSD arm64 wait for a run
on arm64 hardware, and macOS for a run on macOS with macFUSE. Adding one is a
word in that variable, plus `platforms.md`, in the same commit as the evidence.

## Cutting one

1. **Write the changelog section.** Rename `## Unreleased` in `CHANGELOG.md` to
   `## vX.Y.Z — YYYY-MM-DD` and start a new, empty `## Unreleased` above it. The
   release notes *are* that section, prefixed with the changelog's
   work-in-progress warning ([`scripts/release-notes.sh`](../../scripts/release-notes.sh)),
   and the workflow refuses a tag that has no section of its own.
2. **For the first release only**, the docs that say there are no releases stop
   being true in the same commit: `README.md`, `docs/user/install.md`,
   `docs/user/quickstart.md`, the site's front page and the top of
   `CHANGELOG.md`. Downloads come first; `go install …@latest` stays, and now
   means *the newest release* — **after the first tag, `@latest` no longer
   follows master**, so a fix on master reaches `go install` users only at the
   next tag. `@master` is the spelling for the tip.

   `install.md` also gains the section the website's releases page links to for
   "which file to choose, how to install it and how to check it": which download
   suits which system, `apt install ./drivel_….deb` and `dnf install
   ./drivel-….rpm`, the tarball's layout, and the two checks —
   `sha256sum -c SHA256SUMS --ignore-missing` and `gh attestation verify`.
3. **Commit, push, and wait for CI on master.**
4. **Tag and push the tag.** Signed, like the commits (the GPG agent has to be
   unlocked):

   ```sh
   git tag -s vX.Y.Z -m "drivel vX.Y.Z"
   git push origin vX.Y.Z
   ```

5. **Read the draft.** Releases → the draft named `drivel vX.Y.Z`: the notes,
   and the file list against the table above. Then Publish. Marking a `v0.x`
   release as a *pre-release* is a choice made here, not by the workflow.
   Publishing redeploys the website, whose [releases
   page](https://syshlted.github.io/drivel/releases/) lists it a few minutes
   later — see [the website](website.md#the-releases-page-reads-github-through-the-workflow).
6. **Prime the module proxy**, as [publishing](../project/publishing.md)
   describes, so `go install …@vX.Y.Z` and pkg.go.dev see it promptly.

**If the draft is wrong, delete the draft and the tag, fix, and tag again** —
but only while it is still a draft. Once a release is published the tag is in
the module proxy for good, so the fix is a new patch version, never a moved tag.

```sh
gh release delete vX.Y.Z --yes
git push --delete origin vX.Y.Z && git tag -d vX.Y.Z
```

## Trials

A release recipe nobody runs is one that is broken on the day it is needed, so
the workflow also runs without a tag: on any push to master that touches the
recipe (`packaging/`, the two scripts, the Makefile or the workflow), and by
hand (`gh workflow run Release`). A trial builds exactly what a release would,
skips the attestations and the release, and keeps `dist/` as a workflow artifact
for a week — which is how to try a package on a machine that is not this one.

Locally, `make dist` is the same build. From a checkout that is not a clean tag
it names everything after the pseudo-version and warns about `+dirty`; that is a
trial, and only `RELEASE_TAG` makes it a candidate.
