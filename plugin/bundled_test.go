// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package plugin

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/syshlted/drivel/provider"
)

// The spec a bundled launch builds has to name *this* program. The property is
// not "a path that looks right" but "the image we are running", which is the
// whole reason the bundled path needs no mode check: there is nothing to replace
// between deciding to launch and launching.
func TestBundledSpecReExecutesThisProgram(t *testing.T) {
	spec, err := bundledSpec("gdrive")
	if err != nil {
		t.Fatalf("bundledSpec: %v", err)
	}
	if !spec.bundled {
		t.Error("a bundled spec must say so; the log line and the error text both read off it")
	}
	if want := []string{ServeCommand, "gdrive"}; !slices.Equal(spec.args, want) {
		t.Errorf("args = %q, want %q", spec.args, want)
	}
	if runtime.GOOS == "linux" && spec.path != "/proc/self/exe" {
		t.Errorf("path = %q; on Linux the running inode is what should be re-executed", spec.path)
	}

	// Whatever it resolved to, it has to be this test binary — which is the host
	// in this test, and is the assertion the string comparison above cannot make.
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("this platform cannot name its own executable: %v", err)
	}
	self, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	image, err := os.Stat(spec.path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(self, image) {
		t.Errorf("%s is not this program (%s)", spec.path, exe)
	}
}

// The arguments are the one thing a bundled launch adds to the command line, so
// they are asserted where they could be dropped: on the exec.Cmd itself.
func TestBundledLaunchPassesTheServeSubcommandToExec(t *testing.T) {
	bundled := &process{launchSpec: launchSpec{
		kind: "gdrive", path: "/opt/drivel", args: []string{ServeCommand, "gdrive"}, bundled: true,
	}}
	cmd := bundled.clientConfig().Cmd
	if cmd.Path != "/opt/drivel" {
		t.Errorf("Path = %q, want the host image", cmd.Path)
	}
	if want := []string{"/opt/drivel", ServeCommand, "gdrive"}; !slices.Equal(cmd.Args, want) {
		t.Errorf("Args = %q, want %q", cmd.Args, want)
	}

	// And an installed plugin is still launched with nothing to say: its filename
	// has already said which kind it is.
	installed := &process{launchSpec: launchSpec{kind: "fake", path: "/x/" + BinaryPrefix + "fake"}}
	if got, want := installed.clientConfig().Cmd.Args, []string{"/x/" + BinaryPrefix + "fake"}; !slices.Equal(got, want) {
		t.Errorf("Args = %q, want %q", got, want)
	}
}

// A failure has to name what was being launched, and the two paths have
// different answers: a file, or this binary.
func TestALaunchFailureNamesWhatItTriedToRun(t *testing.T) {
	installed := launchSpec{kind: "fake", path: "/x/" + BinaryPrefix + "fake"}
	if got := installed.what(); got != installed.path {
		t.Errorf("what() = %q, want the path; which file failed is the first question", got)
	}
	bundled := launchSpec{kind: "gdrive", path: "/proc/self/exe", bundled: true}
	if got := bundled.what(); strings.Contains(got, "/proc/self/exe") {
		t.Errorf("what() = %q; naming the host's own image says nothing a reader can act on", got)
	}
}

// A bundled kind wins, and the file it won over is named. The silence is what
// would be wrong: "which backend am I running?" has to be answerable from the
// log, which is the same reasoning PathEnv is written on.
func TestABundledKindWinsOverAnInstalledPlugin(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	installed := touchExec(t, dir, BinaryPrefix+"gdrive", 0o755)
	touchExec(t, dir, BinaryPrefix+"other", 0o755)

	lb := &logBuf{}
	l := NewLoader([]string{dir}, log.New(lb, "", 0))
	l.Bundled("gdrive")

	if want := []string{"gdrive", "other"}; !slices.Equal(l.Kinds(), want) {
		t.Errorf("Kinds() = %q, want %q", l.Kinds(), want)
	}
	if got := l.found["gdrive"]; got != "" {
		t.Errorf("the installed gdrive was kept as launchable (%s)", got)
	}
	if got := l.overridden["gdrive"]; !slices.Equal(got, []string{installed}) {
		t.Errorf("overridden[gdrive] = %q, want %q", got, []string{installed})
	}
	if out := lb.String(); !strings.Contains(out, installed) || !strings.Contains(out, "bundled") {
		t.Errorf("the log should name the file being ignored:\n%s", out)
	}
	// A kind this binary does not carry is unaffected, which is what keeps an
	// out-of-tree backend working.
	if l.found["other"] == "" {
		t.Error("an unbundled kind stopped being discovered on the search path")
	}
}

// The mode check must not reach a bundled kind. An installed file nobody is
// going to launch is not a reason to refuse the backend: a refusal would
// register a Factory that fails, for a file the host never had to trust.
func TestABundledKindIgnoresAnUnsafeInstalledFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	touchExec(t, dir, BinaryPrefix+"gdrive", 0o757) // world-writable: refused if launched

	lb := &logBuf{}
	l := NewLoader([]string{dir}, log.New(lb, "", 0))
	l.Bundled("gdrive")
	l.Discover()

	if reason, refused := l.refused["gdrive"]; refused {
		t.Errorf("a bundled kind was refused over a file it will never run: %v", reason)
	}
	f, err := l.Factory("gdrive")
	if err != nil || f == nil {
		t.Fatalf("Factory(gdrive) = %v, %v; want the bundled backend", f, err)
	}
}

// The whole launch over the re-exec path, against the real protocol: the fake
// backend started the way a bundled one is, with the subcommand arguments in
// place.
//
// What it covers that the unit assertions above do not is that the extra
// arguments change nothing downstream — the handshake, Open, capability
// negotiation and a round trip are M9 unchanged, which is the requirement the
// milestone is built on rather than a hope. The other half of the chain, that
// `drivel plugin-serve KIND` reaches the right factory, is in cmd/drivel.
func TestABundledLaunchServesTheBackend(t *testing.T) {
	dir := pluginDir(t)
	lb := &logBuf{}
	lg := log.New(lb, "", 0)

	spec := launchSpec{
		kind:    "fake",
		path:    filepath.Join(dir, BinaryPrefix+"fake"),
		args:    []string{ServeCommand, "fake"},
		bundled: true,
	}
	store, err := open(context.Background(), spec, provider.Params{
		Config: provider.MustEncodeConfig(map[string]any{}),
		Log:    lg,
	})
	if err != nil {
		t.Fatalf("launching the fake backend as a bundled one: %v\n%s", err, lb.String())
	}
	t.Cleanup(func() {
		if c, ok := store.(io.Closer); ok {
			_ = c.Close()
		}
	})

	if _, err := store.Put(context.Background(), "x.txt", strings.NewReader("bundled")); err != nil {
		t.Fatalf("Put over a bundled launch: %v", err)
	}
	if _, ok, err := store.Stat(context.Background(), "x.txt"); err != nil || !ok {
		t.Fatalf("Stat over a bundled launch: ok=%v err=%v", ok, err)
	}
	if out := lb.String(); !strings.Contains(out, "loaded the bundled fake backend") {
		t.Errorf("the launch line should say the backend was bundled:\n%s", out)
	}
	// It names no file, deliberately: there is no other copy it could have been.
	if out := lb.String(); strings.Contains(out, spec.path) {
		t.Errorf("the launch line named a file for a bundled backend:\n%s", out)
	}
}
