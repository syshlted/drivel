// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
)

// TestVersionText pins each shape of build the version command can meet. The
// first line is the one the release workflow compares with the tag, so it is
// asserted exactly; the rest is for a human and is asserted by content.
func TestVersionText(t *testing.T) {
	t.Parallel()

	vcs := func(rev, when, modified string) []debug.BuildSetting {
		return []debug.BuildSetting{
			{Key: "vcs", Value: "git"},
			{Key: "vcs.revision", Value: rev},
			{Key: "vcs.time", Value: when},
			{Key: "vcs.modified", Value: modified},
		}
	}
	for _, tc := range []struct {
		name     string
		info     *debug.BuildInfo
		backends []string
		first    string
		has      []string
		hasNot   []string
	}{
		{
			name:     "a tagged build from a checkout",
			info:     &debug.BuildInfo{Main: debug.Module{Version: "v0.1.0"}, Settings: vcs("0123abcd", "2026-10-23T12:00:00Z", "false")},
			backends: []string{"gdrive", "sftp"},
			first:    "drivel v0.1.0",
			has:      []string{"commit 0123abcd (2026-10-23T12:00:00Z)\n", "go1.27.3 linux/amd64\n", "backends: gdrive, sftp\n"},
			hasNot:   []string{"uncommitted"},
		},
		{
			name:     "a modified tree says so twice",
			info:     &debug.BuildInfo{Main: debug.Module{Version: "v0.1.0+dirty"}, Settings: vcs("0123abcd", "2026-10-23T12:00:00Z", "true")},
			backends: []string{"gdrive", "sftp"},
			first:    "drivel v0.1.0+dirty",
			has:      []string{", with uncommitted changes\n"},
		},
		{
			// go install …@v0.1.0 builds from a module zip: a version, no VCS.
			name:     "go install has no commit line",
			info:     &debug.BuildInfo{Main: debug.Module{Version: "v0.1.0"}},
			backends: []string{"gdrive", "sftp"},
			first:    "drivel v0.1.0",
			hasNot:   []string{"commit"},
		},
		{
			name:     "outside a checkout is Go's own (devel)",
			info:     &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}},
			backends: []string{"gdrive", "sftp"},
			first:    "drivel (devel)",
		},
		{
			name:     "no build information at all",
			info:     nil,
			backends: []string{"gdrive", "sftp"},
			first:    "drivel unknown",
		},
		{
			name:  "a nobundle host names where its backends come from",
			info:  &debug.BuildInfo{Main: debug.Module{Version: "v0.1.0"}},
			first: "drivel v0.1.0",
			has:   []string{"backends: none built in (TAGS=nobundle)"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := versionText(tc.info, "go1.27.3", "linux/amd64", tc.backends)
			if first, _, _ := strings.Cut(got, "\n"); first != tc.first {
				t.Errorf("first line = %q, want %q\nfull output:\n%s", first, tc.first, got)
			}
			for _, s := range tc.has {
				if !strings.Contains(got, s) {
					t.Errorf("output lacks %q:\n%s", s, got)
				}
			}
			for _, s := range tc.hasNot {
				if strings.Contains(got, s) {
					t.Errorf("output contains %q:\n%s", s, got)
				}
			}
		})
	}
}

// TestVersionOfABuiltBinary is the half the table cannot state: that Go really
// does record a version in a binary built from this checkout, which is the
// premise of having no -ldflags variable. A build that lost it would print
// "drivel unknown" here and in every release.
func TestVersionOfABuiltBinary(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	t.Parallel()

	bin := filepath.Join(t.TempDir(), "drivel")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	out, err := exec.Command(bin, "version").Output()
	if err != nil {
		t.Fatalf("drivel version: %v", err)
	}
	first, _, _ := strings.Cut(string(out), "\n")
	version, ok := strings.CutPrefix(first, "drivel ")
	if !ok || version == "" || version == "unknown" {
		t.Fatalf("first line = %q, want \"drivel VERSION\"", first)
	}
	if !strings.Contains(string(out), "backends: ") {
		t.Errorf("no backends line:\n%s", out)
	}
}
