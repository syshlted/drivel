// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package plugin

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/syshlted/drivel/provider"
)

// touchExec writes an executable file with the given mode.
func touchExec(t *testing.T, dir, name string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestKindOfTakesTheKindFromTheFilename(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind string
		ok   bool
	}{
		{"drivel-provider-gdrive", "gdrive", true},
		{"drivel-provider-s3-compat", "s3-compat", true},
		{"drivel-provider-my_thing", "my_thing", true},
		{"drivel-provider-", "", false},
		{"drivel", "", false},
		{"drivel-mount-helper", "", false},
		// The debris a build or an editor leaves beside a real plugin must not be
		// launched. A kind is a name in a config file, so anything that could not
		// be one is not a plugin.
		{"drivel-provider-gdrive.old", "", false},
		{"drivel-provider-gdrive.bak", "", false},
		{"drivel-provider-Gdrive", "", false},
		{"drivel-provider-../etc/passwd", "", false},
	} {
		kind, ok := kindOf(tc.name)
		if ok != tc.ok || kind != tc.kind {
			t.Errorf("kindOf(%q) = %q, %v; want %q, %v", tc.name, kind, ok, tc.kind, tc.ok)
		}
	}
}

// A plugin runs with the user's credentials, so drivel refuses to launch one
// that somebody else could have replaced between installation and now.
func TestSafeToRunRefusesWritableExecutables(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	ok := touchExec(t, dir, "good", 0o755)
	if err := safeToRun(ok); err != nil {
		t.Errorf("a 0755 binary in a 0755 directory was refused: %v", err)
	}

	worldWritable := touchExec(t, dir, "world", 0o757)
	err := safeToRun(worldWritable)
	if err == nil {
		t.Error("a world-writable binary was accepted")
	} else if !strings.Contains(err.Error(), "everyone") {
		t.Errorf("the refusal should say who can write it: %v", err)
	}

	groupWritable := touchExec(t, dir, "group", 0o775)
	if err := safeToRun(groupWritable); err == nil {
		t.Error("a group-writable binary was accepted")
	}

	notExec := filepath.Join(dir, "plain")
	if werr := os.WriteFile(notExec, []byte("x"), 0o644); werr != nil {
		t.Fatal(werr)
	}
	if err := safeToRun(notExec); err == nil {
		t.Error("a non-executable file was accepted")
	}
}

func TestSafeToRunRefusesAWritableDirectory(t *testing.T) {
	dir := t.TempDir()
	bin := touchExec(t, dir, "b", 0o755)
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	err := safeToRun(bin)
	if err == nil {
		t.Fatal("a binary in a world-writable directory was accepted")
	}
	if !strings.Contains(err.Error(), "directory") {
		t.Errorf("the refusal should name the directory: %v", err)
	}

	// Sticky is precisely the bit that says "you may create here but not replace
	// what is not yours", so it is not the same hazard.
	// Go's FileMode spells sticky as a flag bit, not as 0o1000 in the permission
	// word; passing the octal would silently set nothing and the assertion below
	// would pass for the wrong reason.
	if err := os.Chmod(dir, 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	if err := safeToRun(bin); err != nil {
		t.Errorf("a sticky world-writable directory was refused: %v", err)
	}
}

func TestLoaderPrefersTheFirstDirectoryOnThePath(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	want := touchExec(t, first, BinaryPrefix+"thing", 0o755)
	touchExec(t, second, BinaryPrefix+"thing", 0o755)
	touchExec(t, second, BinaryPrefix+"other", 0o755)

	var out strings.Builder
	l := NewLoader([]string{first, second}, log.New(&out, "", 0))
	if got := l.Kinds(); !slices.Equal(got, []string{"other", "thing"}) {
		t.Errorf("Kinds() = %v", got)
	}
	if l.found["thing"] != want {
		t.Errorf("thing resolved to %s; want %s", l.found["thing"], want)
	}
	// Two files claiming one kind is the ambiguity worth reporting: the operator
	// installed something they are not running.
	if !strings.Contains(out.String(), "also found") {
		t.Errorf("the shadowed plugin was not reported:\n%s", out.String())
	}
}

// A refused binary is still a discovered kind. Reporting it as unknown would
// send someone hunting for a missing install when the file is right there and
// its permissions are wrong.
func TestLoaderReportsARefusedPluginWhenItIsUsed(t *testing.T) {
	dir := t.TempDir()
	touchExec(t, dir, BinaryPrefix+"thing", 0o777)

	l := NewLoader([]string{dir}, log.New(io.Discard, "", 0))
	if got := l.Kinds(); !slices.Equal(got, []string{"thing"}) {
		t.Fatalf("Kinds() = %v; a refused plugin should still be discovered", got)
	}
	f, err := l.Factory("thing")
	if err != nil {
		t.Fatalf("Factory: %v", err)
	}
	_, err = f(context.Background(), provider.Params{})
	if err == nil {
		t.Fatal("the refused plugin opened")
	}
	if !strings.Contains(err.Error(), "writable") {
		t.Errorf("the reason was lost: %v", err)
	}
}

func TestLoaderFactoryForAnUninstalledKind(t *testing.T) {
	l := NewLoader([]string{t.TempDir()}, log.New(io.Discard, "", 0))
	_, err := l.Factory("nope")
	if !errors.Is(err, provider.ErrUnknownKind) {
		t.Errorf("err = %v; want it to wrap provider.ErrUnknownKind", err)
	}
	if !strings.Contains(err.Error(), BinaryPrefix+"nope") {
		t.Errorf("the error should name the file it looked for: %v", err)
	}
}

// A directory that does not exist is the normal case for most of the default
// path, and must not be an error.
func TestLoaderIgnoresMissingDirectories(t *testing.T) {
	var out strings.Builder
	l := NewLoader([]string{filepath.Join(t.TempDir(), "nothing-here")}, log.New(&out, "", 0))
	if got := l.Kinds(); len(got) != 0 {
		t.Errorf("Kinds() = %v", got)
	}
	if out.String() != "" {
		t.Errorf("a missing directory should be silent, got:\n%s", out.String())
	}
}

func TestLoaderRegistersIntoARegistry(t *testing.T) {
	dir := t.TempDir()
	touchExec(t, dir, BinaryPrefix+"alpha", 0o755)
	touchExec(t, dir, BinaryPrefix+"beta", 0o755)

	reg := provider.NewRegistry()
	if err := NewLoader([]string{dir}, log.New(io.Discard, "", 0)).Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if got := reg.Kinds(); !slices.Equal(got, []string{"alpha", "beta"}) {
		t.Errorf("registry kinds = %v", got)
	}
}

// The environment variable replaces the default path rather than extending it,
// so that "which plugin am I running?" has one visible answer.
func TestDefaultPathIsReplacedByTheEnvironment(t *testing.T) {
	t.Setenv(PathEnv, "/one:/two")
	got := DefaultPath()
	if !slices.Equal(got, []string{"/one", "/two"}) {
		t.Errorf("DefaultPath() = %v; want exactly what the environment said", got)
	}
}

func TestDefaultPathIncludesTheExecutablesDirectory(t *testing.T) {
	t.Setenv(PathEnv, "")
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("no executable path on this platform: %v", err)
	}
	if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
		exe = resolved
	}
	got := DefaultPath()
	if len(got) == 0 || got[0] != filepath.Dir(exe) {
		t.Errorf("DefaultPath() starts with %v; want the running binary's directory %s", got, filepath.Dir(exe))
	}
}
