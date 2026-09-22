// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package app

import (
	"path/filepath"
	"strings"
	"testing"
)

// spec builds a valid separate-backing-dir mount rooted under dir.
func spec(dir, name string) MountSpec {
	return MountSpec{
		Name:       name,
		Mountpoint: filepath.Join(dir, name, "mnt"),
		DataDir:    filepath.Join(dir, name, "data"),
		StateDB:    filepath.Join(dir, name, "state.db"),
	}
}

func TestValidateAcceptsIndependentMounts(t *testing.T) {
	dir := t.TempDir()
	if err := Validate([]MountSpec{spec(dir, "a"), spec(dir, "b")}); err != nil {
		t.Fatalf("independent mounts rejected: %v", err)
	}
}

func TestValidateRejectsNothingToDo(t *testing.T) {
	if err := Validate(nil); err == nil {
		t.Fatal("an empty mount list was accepted")
	}
}

func TestValidateGuards(t *testing.T) {
	dir := t.TempDir()

	cases := []struct {
		name  string
		specs func() []MountSpec
		want  string
	}{
		{
			// Two engines on one state DB read each other's echo records as their
			// own — §4 loop suppression and M7b's delete baseline both live there.
			name: "shared state DB",
			specs: func() []MountSpec {
				a, b := spec(dir, "a"), spec(dir, "b")
				b.StateDB = a.StateDB
				return []MountSpec{a, b}
			},
			want: "share the state DB",
		},
		{
			name: "shared mountpoint",
			specs: func() []MountSpec {
				a, b := spec(dir, "a"), spec(dir, "b")
				b.Mountpoint = a.Mountpoint
				return []MountSpec{a, b}
			},
			want: "share the mountpoint",
		},
		{
			name: "duplicate names",
			specs: func() []MountSpec {
				a, b := spec(dir, "a"), spec(dir, "b")
				b.Name = a.Name
				return []MountSpec{a, b}
			},
			want: "both named",
		},
		{
			// DESIGN.md §2.7's cardinal rule, generalised: reading a's backing
			// through b's FUSE handler recurses into our own filesystem.
			name: "backing inside another mountpoint",
			specs: func() []MountSpec {
				a, b := spec(dir, "a"), spec(dir, "b")
				a.DataDir = filepath.Join(b.Mountpoint, "nested")
				return []MountSpec{a, b}
			},
			want: "would recurse",
		},
		{
			name: "mounted inside another backing tree",
			specs: func() []MountSpec {
				a, b := spec(dir, "a"), spec(dir, "b")
				a.Mountpoint = filepath.Join(b.DataDir, "nested")
				return []MountSpec{a, b}
			},
			want: "sync to both",
		},
		{
			// Needed no second mount to be wrong: the DB would sync itself to the
			// cloud, and its own writes would generate the events causing more.
			name: "state DB inside its own backing tree",
			specs: func() []MountSpec {
				a := spec(dir, "a")
				a.StateDB = filepath.Join(a.DataDir, "state.db")
				return []MountSpec{a}
			},
			want: "sync itself to the cloud",
		},
		{
			name: "backing dir set to its own mountpoint",
			specs: func() []MountSpec {
				a := spec(dir, "a")
				a.DataDir = a.Mountpoint
				return []MountSpec{a}
			},
			want: "omit it to select in-place mode",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(tc.specs())
			if err == nil {
				t.Fatalf("%s was accepted", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q should mention %q", err, tc.want)
			}
		})
	}
}

// In-place mode makes the mountpoint its own backing store, which must stay
// legal — the guard is about a backing dir written out to equal the mountpoint,
// not about in-place mounts.
func TestValidateAllowsInPlace(t *testing.T) {
	dir := t.TempDir()
	a := MountSpec{
		Name:       "inplace",
		Mountpoint: filepath.Join(dir, "mnt"),
		StateDB:    filepath.Join(dir, "state.db"),
	}
	if err := Validate([]MountSpec{a}); err != nil {
		t.Fatalf("in-place mount rejected: %v", err)
	}
	// ...but its state DB still may not live inside it.
	a.StateDB = filepath.Join(a.Mountpoint, "state.db")
	if err := Validate([]MountSpec{a}); err == nil {
		t.Fatal("state DB inside an in-place mount was accepted")
	}
}

// A single misconfiguration must be reported once, not once per direction of
// each cross-mount check.
func TestValidateReportsSharedMountpointOnce(t *testing.T) {
	dir := t.TempDir()
	a, b := spec(dir, "a"), spec(dir, "b")
	b.Mountpoint = a.Mountpoint
	err := Validate([]MountSpec{a, b})
	if err == nil {
		t.Fatal("shared mountpoint accepted")
	}
	if n := strings.Count(err.Error(), "share the mountpoint"); n != 1 {
		t.Errorf("reported %d times; want 1:\n%v", n, err)
	}
}

func TestWithin(t *testing.T) {
	cases := []struct {
		child, parent string
		want          bool
	}{
		{"/a/b", "/a", true},
		{"/a", "/a", true},
		{"/a", "/a/b", false},
		{"/ab", "/a", false}, // prefix match without a separator is not containment
		{"/b", "/a", false},
	}
	for _, tc := range cases {
		if got := within(tc.child, tc.parent); got != tc.want {
			t.Errorf("within(%q, %q) = %v; want %v", tc.child, tc.parent, got, tc.want)
		}
	}
}
