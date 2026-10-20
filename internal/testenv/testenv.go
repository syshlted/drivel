// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

// Package testenv decides whether a missing kernel facility should skip a test
// or fail it.
//
// Several of drivel's tests need something the machine may not provide: a
// backing filesystem with user xattrs (M5's authoritative placeholder marker) or
// a mountable /dev/fuse. Skipping when those are absent is right on a developer's
// laptop and wrong in CI, where a runner without them would report green while
// the tests guarding the M5 data-loss invariants never ran at all.
//
// Setting DRIVEL_REQUIRE_TESTENV inverts the default for the facilities named in
// it, so an environment that is supposed to be complete fails loudly instead.
package testenv

import (
	"os"
	"strings"
)

// EnvVar names the environment variable holding a comma-separated list of
// facilities whose absence must fail rather than skip. The value "all" requires
// every facility.
const EnvVar = "DRIVEL_REQUIRE_TESTENV"

// Facilities that tests can require.
const (
	FUSE  = "fuse"  // /dev/fuse present and mountable (needs the fuse3 helper)
	Xattr = "xattr" // backing filesystem stores user.* extended attributes
)

// TB is the part of *testing.T that Unavailable needs. It exists so the skip and
// fail branches can themselves be tested; testing.TB cannot be implemented
// outside the testing package.
type TB interface {
	Helper()
	Fatalf(format string, args ...any)
	Skipf(format string, args ...any)
}

// Unavailable reports that facility is missing from this environment. It skips
// the calling test, unless facility is required via EnvVar, in which case it
// fails. reason should describe what was actually observed.
func Unavailable(t TB, facility, reason string) {
	t.Helper()
	if Required(facility) {
		// Not merely a t.Fatalf followed by a fallthrough: *testing.T.Fatalf
		// stops the goroutine, but nothing in the TB contract promises that.
		t.Fatalf("%s unavailable: %s (required by %s=%q)", facility, reason, EnvVar, os.Getenv(EnvVar))
		return
	}
	t.Skipf("%s unavailable: %s", facility, reason)
}

// Required reports whether the absence of facility must fail tests rather than
// skip them.
func Required(facility string) bool {
	for _, f := range strings.Split(os.Getenv(EnvVar), ",") {
		switch f = strings.TrimSpace(f); {
		case f == "":
		case strings.EqualFold(f, "all"), strings.EqualFold(f, facility):
			return true
		}
	}
	return false
}

// Folding is a directory on a filesystem that folds letter case — a casefold
// tmpfs or ext4 directory on Linux, any directory on macOS's default volume —
// which the case-insensitive backing store tests need and no ordinary test
// machine has. It is named, not probed (FoldingDirVar), and "all" does not
// cover it, because CI has no such directory; naming "folding" explicitly in
// EnvVar does make its absence a failure.
const Folding = "folding"

// FoldingDirVar names the case-folding directory Folding's tests run in.
const FoldingDirVar = "DRIVEL_CASEFOLD_DIR"

// FoldingDir returns the directory FoldingDirVar names, or skips the test — or
// fails it, when EnvVar names Folding itself.
func FoldingDir(t TB) string {
	t.Helper()
	if d := os.Getenv(FoldingDirVar); d != "" {
		return d
	}
	for _, f := range strings.Split(os.Getenv(EnvVar), ",") {
		if strings.EqualFold(strings.TrimSpace(f), Folding) {
			t.Fatalf("%s unavailable: %s names no case-folding directory (required by %s=%q)", Folding, FoldingDirVar, EnvVar, os.Getenv(EnvVar))
			return ""
		}
	}
	t.Skipf("%s unavailable: %s names no case-folding directory", Folding, FoldingDirVar)
	return ""
}
