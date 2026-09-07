# Contributing

Patches welcome. This page is the short version; the detail is in
[docs/dev/](docs/dev/).

## Before you start

Read **[docs/dev/conventions.md](docs/dev/conventions.md)**. Drivel is a
filesystem that holds the only local copy of someone's data, and several of its
rules exist because the obvious change is wrong in a way no test catches —
particularly around echo suppression, placeholders, and inferred deletions.

If you are adding a cloud backend, you should not need to touch anything outside
your own package: see [docs/dev/new-provider.md](docs/dev/new-provider.md).

## The loop

```sh
make hooks            # once per clone — installs the git hooks
make check            # everything CI runs, in CI's order
```

`make check` is `tidy-check`, `fmt-check`, `lint`, `test-full` and `vuln`. The
hooks split the same gates by cost: fast ones at commit time, the whole-tree lint
and the race suite at push time.

Every gate is a make target that the hooks and CI both call, so "it passed
locally" and "it passed in CI" cannot mean different things. See
[docs/dev/building.md](docs/dev/building.md) and
[docs/dev/testing.md](docs/dev/testing.md).

## What we look for

- **Tests, green under `-race`.** Every package has them. If your change touches
  lazy hydration or FUSE behaviour, run
  `DRIVEL_REQUIRE_TESTENV=all go test -race ./...` so nothing skips silently — the
  tests that skip on a laptop are the data-loss guards.
- **A lint suppression is narrow and carries a reason** —
  `//nolint:gosec // G401: dictated by Drive's md5Checksum, not a security
  property` — rather than a disabled linter.
- **The seams stay clean.** The sync engine must not learn a concrete provider
  type; `internal/config` must not learn what a Drive folder ID is.
- **Documentation moves with the code.** A behaviour change that affects users
  belongs in [docs/user/](docs/user/), the man page, and
  [CHANGELOG.md](CHANGELOG.md).

## Licence

Drivel is licensed under the **GNU AGPL version 3** (not "or later"). Contributions
are accepted under that same licence. There is no CLA and no separate proprietary
edition, so a patch you send stays free software for everyone who receives it.

Do not edit `LICENSE` — it is the FSF's text verbatim. The copyright notice lives
in four places that must stay in sync: the README's licence section,
`docs/user/drivel.1`, the `cmd/drivel` package doc comment, and the usage text
`drivel help` prints.

## Reporting a vulnerability

See [SECURITY.md](SECURITY.md). Do not open a public issue for one.
