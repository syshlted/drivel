# Publishing to pkg.go.dev

[pkg.go.dev](https://pkg.go.dev) indexes public Go modules automatically — there
is no manual "submit" form. A module appears once the [module
proxy](https://proxy.golang.org) has fetched a specific version, which happens
the first time anyone (including you) requests it. This page is the checklist to
get `github.com/zishmusic/drivel` listed cleanly, plus the caveats specific to
this repo.

## Prerequisites

1. **A public, fetchable VCS repo at the module path.**
   `go.mod` declares `module github.com/zishmusic/drivel`, so the repo must be
   publicly reachable at `https://github.com/zishmusic/drivel`. The path in
   `go.mod` and the real repo URL must match exactly — the proxy clones the URL
   derived from the module path.

2. **A valid `go.mod` with a real Go version.**
   Already present (`go 1.27.1`). Run `go mod tidy` so `go.sum` is complete and
   committed; the proxy verifies checksums.

3. **A semver tag.**
   pkg.go.dev documents *tagged versions*. Create an annotated, signed-if-you-can
   tag and push it:

   ```sh
   git tag -a v0.1.0 -m "drivel v0.1.0"
   git push origin v0.1.0
   ```

   Use `v0.x.y` while the API is unstable; the first `v1.0.0` is a compatibility
   promise. Modules at `v2+` need a `/v2` suffix on the module path (not relevant
   yet). Without a tag, the site can still show a pseudo-version, but a real tag
   is what you want for a "latest" badge.

4. **An OSI-recognized `LICENSE` file at the repo root.**
   Present: **AGPL-3.0**. pkg.go.dev only renders documentation for modules whose
   license it can detect and recognize (it uses the same detector as
   `licensecheck`). AGPL-3.0 is recognized, so docs will render; the page shows
   the detected license name.

## Trigger indexing

After the tag is pushed, prime the proxy from a machine with network access:

```sh
# Either of these makes the proxy fetch (and thus index) the version:
GOPROXY=https://proxy.golang.org go list -m github.com/zishmusic/drivel@v0.1.0
# or hit the proxy endpoint directly:
curl https://proxy.golang.org/github.com/zishmusic/drivel/@v/v0.1.0.info
```

Within a minute or two the version resolves and
`https://pkg.go.dev/github.com/zishmusic/drivel` populates. `@latest` follows the
highest semver tag.

## What will (and won't) be documented

- **`cmd/drivel`** and the `cmd/drivel-provider-*` commands are `package main` —
  commands, not importable libraries. pkg.go.dev lists them and renders each
  command's doc comment, but there is no importable API surface.
- **Three packages are public and are meant to be**: `provider` (the backend seam),
  `ranges` (the extent types its `RangePutter` names) and `plugin` (the loader and
  the `Serve` a backend's `main` calls). They are outside `internal/` because M9's
  whole point is that a backend can be written out of tree, and an out-of-tree
  backend imports all three. Their doc pages are therefore the API documentation
  for writing a provider, and they carry a compatibility obligation the rest of
  the tree does not: renaming an exported identifier there breaks somebody else's
  build.
- **Everything else under `internal/`** is intentionally hidden. Go tooling and
  pkg.go.dev never expose `internal/` packages to external importers, so the sync
  engine, the mount backend, the transport and the two shipped provider
  implementations will *not* get public doc pages. That is by design — they are
  implementation detail, not a supported API. If you ever want another package
  publicly documented and importable, move it out of `internal/` deliberately, and
  accept the same obligation.
- The repo `README.md` is rendered on the module landing page, and the
  `LICENSE` is linked.

The pages worth reading, then, are the landing page (README + install line) and
the three public packages. `plugin`'s package doc is the entry point for anyone
writing a backend, and is written as one.

## Quality checklist (makes the page good, not just present)

- [ ] `go build ./...` and `go vet ./...` clean.
- [ ] `go mod tidy`; `go.sum` committed.
- [ ] Package doc comment on every package (a `// Package x ...` sentence). Most
      packages here already have one — keep it.
- [ ] Doc comments on exported identifiers follow Go convention (start with the
      identifier name). `go doc ./...` locally to preview.
- [ ] `README.md` opens with a one-line description and an install/usage snippet.
- [ ] Tag pushed; proxy primed as above.

## Install line for users

Once a tag exists, users install the command with:

```sh
go install github.com/zishmusic/drivel/cmd/drivel@latest
go install github.com/zishmusic/drivel/cmd/drivel-provider-gdrive@latest
```

(That places both binaries in `$(go env GOBIN)` or `$GOPATH/bin`. The second one
is the Google Drive backend: since M9 the command carries no backend of its own,
so a `drivel` installed alone mounts but cannot sync. It also still needs the
system `fuse3` helper at runtime — see the README.)

## Removing a bad version

You cannot un-publish from the proxy (versions are immutable and cached). If a
tag is broken, tag a higher version with the fix; `@latest` moves forward. To
retract a version from `go get` resolution, add a `retract` directive to
`go.mod` and release a new patch:

```go
retract v0.1.0 // panics on mount; use v0.1.1+
```
