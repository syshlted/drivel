# Testing

`go test ./...` runs **offline**. The transport, the event→API mapping, the
provider and the sync helpers are all unit-tested without touching Drive, against
a fake Drive that models the API's real behaviour — including the parts that make
a fleet misbehave.

```sh
make test                                       # go test -race ./...
make test-full                                  # ...with every facility required
make cover                                      # coverage profile
DRIVEL_REQUIRE_TESTENV=all go test -race ./...  # what test-full does
```

## Tests that need the kernel

Two groups of tests need something the machine may not have: `internal/hydrate`
needs a backing filesystem that stores `user.*` xattrs (M5's authoritative
placeholder marker), and `internal/vfs` mounts a real FUSE filesystem. Both
`t.Skip` when the facility is absent, which is right on a developer's laptop and
wrong anywhere automated — a runner without either reports green while the tests
guarding M5's data-loss invariants never execute.

`DRIVEL_REQUIRE_TESTENV` turns those skips into failures:

```sh
DRIVEL_REQUIRE_TESTENV=all go test -race ./...   # fuse + xattr must both work
DRIVEL_REQUIRE_TESTENV=xattr go test ./...       # require only xattrs
```

Accepted facility names are `fuse`, `xattr`, and `all` (see
[internal/testenv](../../internal/testenv/testenv.go)). Any CI job should set `all`
and install the `fuse3` package; without that the job tests less than it looks
like it does. On a machine that genuinely cannot mount, leave the variable unset
and expect the skips.

## Property tests

`ranges/property_test.go` and `syncengine/coalescer_property_test.go` are
property-based. Two rules apply to both:

**A property test nobody has seen fail is a property test nobody knows the
strength of.** Each was checked by mutating the code it covers — rounding `Mark`
outward, letting a known event revive a poisoned accumulator — and confirming
that the property naming the mistake is the one that fails. Do the same for any
new property.

**State properties against the *input*, never against an oracle built from the
same helpers.** An oracle sharing the implementation's helpers agrees with it
about the bugs too.

## Helpers that walk a live tree

A helper that walks a tree the code under test is mutating **must skip
`fs.ErrNotExist`, not fail on it**. `peer.manifest` and `peer.conflicts` in
`multiclient_test.go` did the latter and made `TestFleetPropagatesABulkDelete`
fail two runs in three, with the product innocent — a sampler in a poll loop is
waiting out exactly that race. Every *other* error still fails.

## Testing on macOS or FreeBSD

FreeBSD has been run (15.1, 2026-09-06, everything below green); macOS has not.
Both have real xattr implementations as of M10, so this is the list of what a
machine or VM is actually worth — and, for FreeBSD, what the first run cost.

**Put the backing directory on a filesystem that carries extended attributes**, and
check that first — everything below tests the fallback otherwise. APFS or HFS+ on
macOS; UFS or ZFS on FreeBSD, where **tmpfs has none**, so a tmpfs `/tmp` makes the
whole suite report the facility missing. Never use a shared folder from the host.

```sh
TMPDIR=/path/on/a/real/filesystem \
  DRIVEL_REQUIRE_TESTENV=xattr go test -race ./internal/hydrate/ ./internal/syncengine/
```

That is the whole per-platform surface, and it is **fully settled by a VM**:
virtualisation does not change what `getxattr` does. Read the failures by layer —
`internal/hydrate/xattr_unix_test.go` is the syscall layer (it builds on all three
platforms), and `hydrate_test.go` or `syncengine/lazy_test.go` failing with that one
green is placeholder logic above it.

On macOS the code under test is `hydrate/xattr_unix.go`, the same body Linux runs,
plus two symbols in `xattr_darwin.go`. On FreeBSD it is `hydrate/xattr_freebsd.go`,
which shares nothing with the others: watch particularly for a short write from
`extattr_set_file` (reported as an error, and a case no other platform can produce)
and for anything suggesting the `//go:uintptrescapes` wrappers are not holding.

**The mount layer is where the two platforms differ.** FreeBSD needs no special
arrangement — fusefs is in base — so a VM settles it end to end:

```sh
kldload fusefs && sysctl vfs.usermount=1
DRIVEL_REQUIRE_TESTENV=all go test -race ./...
```

`fusefs` is not loaded by default and `kldload` does not persist, so a fresh boot
that skips it makes every mount test skip — which under `=all` is a failure, and is
the answer you want rather than a quiet pass.

**Run `=all`, not just `=xattr`, wherever the platform can mount.** The FreeBSD run
found nothing wrong with the xattr code it was aimed at and four failures in
`internal/vfs` underneath it: every test that wrote through an `O_WRONLY` handle got
`EBADF`, because fusefs reads through a write handle to fill a cache block and the
backing fd was write-only (DESIGN.md §2.1). It is fixed and there is a regression
test, but the shape of the surprise is the lesson — the milestone's own surface was
fine and the layer nobody suspected was not.

Two things about reading a failure there. A `-debug` trace will log that READ as
`OK` and still fail the write, because go-fuse resolves an fd-backed read result
when it writes the reply rather than when it builds it; and a failed mount test
leaves the mountpoint busy, so clear it with `umount -f` before rerunning —
`mount -t fusefs` will not list it, since the type is `fusefs.drivel`.

macOS needs macFUSE, which is a kernel extension: loading one in a guest means
reduced-security boot, which is not the configuration a user's Mac is in, and a
macOS guest on an Apple-silicon host cannot load third-party kexts at all. FUSE-T is
not an alternative — go-fuse cannot drive it (DESIGN.md §2.9.3). So a green VM run
on macOS is evidence about syscalls rather than about the platform, and "builds"
still does not become "supported" without a live end-to-end run on hardware.

## Multi-client testing

Correctness under several clients against one remote is a separate exercise with
its own plan and rig: [multiclient-test-plan.md](multiclient-test-plan.md), and
the scripts in [`scripts/multiclient/`](../../scripts/multiclient). Tier A is the
in-process fleet harness in `internal/app`; Tier B is real `drivel` processes
against a real Drive.
