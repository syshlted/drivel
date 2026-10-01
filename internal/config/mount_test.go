// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The whole point of writing a mount rather than printing it: what login appends
// has to be a file Load and Specs accept, provider settings included.
func TestAppendMountRoundTrips(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	path := filepath.Join(dir, AppName, "config.toml")

	if err := AppendAccount(path, "personal", []Setting{{Key: "provider", Value: "gdrive"}}); err != nil {
		t.Fatalf("AppendAccount: %v", err)
	}
	if err := AppendMount(path, MountEntry{
		Account:  "personal",
		Path:     "./mnt",
		Data:     "./data",
		Provider: []Setting{{Key: "root", Value: "abc123"}},
	}); err != nil {
		t.Fatalf("AppendMount: %v", err)
	}

	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	specs, err := c.Specs()
	if err != nil {
		t.Fatalf("Specs: %v", err)
	}
	if len(specs) != 1 {
		t.Fatalf("got %d mounts; want 1", len(specs))
	}
	if want := filepath.Join(dir, AppName, "mnt"); specs[0].Mountpoint != want {
		t.Errorf("mountpoint = %q; want %q", specs[0].Mountpoint, want)
	}
	if specs[0].Provider != "gdrive" {
		t.Errorf("provider = %q; want gdrive", specs[0].Provider)
	}

	// The [mount.provider] sub-table has to reach the provider, or the mount is
	// pointed at the account root rather than the folder the user logged in for.
	var got struct {
		Root string `toml:"root"`
	}
	if err := specs[0].ProviderConfig.Decode(&got); err != nil {
		t.Fatalf("decoding provider config: %v", err)
	}
	if got.Root != "abc123" {
		t.Errorf("root = %q; want abc123", got.Root)
	}
}

// A second [[mount]] header closes the first one's [mount.provider] sub-table,
// which is what makes appending safe on a file that already has mounts in it.
// Get this wrong and mount two's settings land on mount one.
func TestAppendMountAfterAMountWithProviderSettings(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	path := filepath.Join(dir, "config.toml")

	if err := AppendAccount(path, "a", []Setting{{Key: "provider", Value: "gdrive"}}); err != nil {
		t.Fatal(err)
	}
	for _, m := range []MountEntry{
		{Account: "a", Name: "one", Path: "./one", Provider: []Setting{{Key: "root", Value: "first"}}},
		{Account: "a", Name: "two", Path: "./two", Provider: []Setting{{Key: "root", Value: "second"}}},
	} {
		if err := AppendMount(path, m); err != nil {
			t.Fatalf("AppendMount %s: %v", m.Name, err)
		}
	}

	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	specs, err := c.Specs()
	if err != nil {
		t.Fatalf("Specs: %v", err)
	}
	if len(specs) != 2 {
		t.Fatalf("got %d mounts; want 2", len(specs))
	}
	for i, want := range []string{"first", "second"} {
		var got struct {
			Root string `toml:"root"`
		}
		if err := specs[i].ProviderConfig.Decode(&got); err != nil {
			t.Fatalf("mount %d: %v", i, err)
		}
		if got.Root != want {
			t.Errorf("mount %d root = %q; want %q", i, got.Root, want)
		}
	}
}

// app.Validate refuses two mounts over one directory, so an appender that let
// one through would write a config file that cannot boot.
func TestAppendMountRefusesADuplicateMountpoint(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	m := MountEntry{Account: "a", Path: "./mnt"}
	if err := AppendMount(path, m); err != nil {
		t.Fatalf("first AppendMount: %v", err)
	}

	err := AppendMount(path, MountEntry{Account: "b", Path: "./mnt"})
	if !errors.Is(err, ErrMountExists) {
		t.Fatalf("second AppendMount = %v; want ErrMountExists", err)
	}
	b, _ := os.ReadFile(path)
	if strings.Count(string(b), "[[mount]]") != 1 {
		t.Errorf("refused append still wrote a block:\n%s", b)
	}
}

// The same directory written two ways is the same mount to everything
// downstream, so the comparison expands before it compares.
func TestHasMountComparesExpandedPaths(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := AppendMount(path, MountEntry{Account: "a", Path: "./mnt"}); err != nil {
		t.Fatal(err)
	}

	for _, spelling := range []string{"./mnt", "mnt", filepath.Join(dir, "mnt"), "./sub/../mnt"} {
		ok, err := HasMount(path, spelling)
		if err != nil {
			t.Fatalf("HasMount(%q): %v", spelling, err)
		}
		if !ok {
			t.Errorf("HasMount(%q) = false; want true", spelling)
		}
	}
	if ok, _ := HasMount(path, "./other"); ok {
		t.Error("HasMount matched an unrelated mountpoint")
	}
}

func TestHasMountOnMissingFile(t *testing.T) {
	ok, err := HasMount(filepath.Join(t.TempDir(), "absent.toml"), "./mnt")
	if err != nil {
		t.Fatalf("HasMount on a missing file: %v", err)
	}
	if ok {
		t.Error("HasMount invented a mount")
	}
}

// Comments are most of why the format was chosen; an append must not disturb
// anything already in the file.
func TestAppendMountPreservesComments(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	original := "# keep me\n[account.a]\nprovider = \"gdrive\" # and me\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := AppendMount(path, MountEntry{Account: "a", Path: "./mnt"}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(b), original) {
		t.Errorf("append disturbed what was there:\n%s", b)
	}
}

func TestAppendMountToFileWithoutTrailingNewline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("[account.a]\nprovider = \"gdrive\""), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := AppendMount(path, MountEntry{Account: "a", Path: "./mnt"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("Load after appending to a file with no trailing newline: %v", err)
	}
}

func TestAppendMountRejectsBadInput(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	if err := AppendMount(path, MountEntry{Account: "a"}); err == nil {
		t.Error("AppendMount accepted a mount with no path")
	}
	// A name becomes a directory component under $XDG_STATE_HOME.
	if err := AppendMount(path, MountEntry{Name: "../escape", Account: "a", Path: "./mnt"}); err == nil {
		t.Error("AppendMount accepted a name that is not usable as a directory")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Error("a rejected mount created the config file anyway")
	}
}

// Empty fields are omitted rather than written as "", which would override the
// derived default with a value that means something different.
func TestMountBlockOmitsEmptyFields(t *testing.T) {
	got := MountBlock(MountEntry{Account: "a", Path: "./mnt"})
	for _, absent := range []string{"name", "data", "[mount.provider]"} {
		if strings.Contains(got, absent) {
			t.Errorf("block names %q for a mount that has none:\n%s", absent, got)
		}
	}
	if !strings.Contains(got, `account = "a"`) || !strings.Contains(got, `path    = "./mnt"`) {
		t.Errorf("block is missing what it was given:\n%s", got)
	}
}
