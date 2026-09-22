// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

//go:build linux

package mount

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// In-place mode opens a dirfd to the mountpoint and routes backing I/O through
// /proc/self/fd/N. Without an actual FUSE mount that path simply resolves to the
// same directory — which is exactly the mechanism we rely on: a write through the
// fd path lands in the underlying dir. (Under a real mount it also bypasses the
// overlay; that part needs a mount and isn't unit-testable here.)
func TestResolveBackingInPlaceRoutesThroughFD(t *testing.T) {
	dir := t.TempDir()

	b, err := ResolveBacking(dir, "") // empty dataDir => in-place
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	if !b.InPlace {
		t.Fatal("in-place mode not flagged")
	}
	if !strings.HasPrefix(b.Path, "/proc/self/fd/") {
		t.Fatalf("Path = %q; want a /proc/self/fd/ handle", b.Path)
	}

	// A write via the fd path must appear in the real directory.
	if err := os.WriteFile(filepath.Join(b.Path, "probe.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("write through fd path: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "probe.txt"))
	if err != nil || string(got) != "hi" {
		t.Fatalf("content via real dir = %q, err=%v; want \"hi\"", got, err)
	}
}

// Close releases the held fd.
func TestBackingCloseReleasesFD(t *testing.T) {
	dir := t.TempDir()
	b, err := ResolveBacking(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
