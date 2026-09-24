// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package app

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/syshlted/drivel/internal/testenv"
	"github.com/syshlted/drivel/plugin"
	"github.com/syshlted/drivel/provider"
)

// The same path the in-process end-to-end test covers, but with the backend in
// another process — which since M9 is how every shipped backend runs.
//
// Everything below the wiring is unchanged by the plugin seam, and the seam
// itself is tested directly in the plugin package. What is only testable here is
// the composition: that a mount opened through a discovered plugin serves a
// filesystem, that the sweep materialises what the backend already held, and
// that a write through the mountpoint reaches the backend's own process. Each of
// those crosses code that the other two tests each see only half of.
func TestEndToEndOverAPluginBackend(t *testing.T) {
	if _, err := os.Stat("/dev/fuse"); err != nil {
		testenv.Unavailable(t, testenv.FUSE, "no /dev/fuse: "+err.Error())
	}

	dir := t.TempDir()
	journal := filepath.Join(dir, "journal")

	spec := baseSpec(t)
	spec.Provider = "fake"
	spec.Materialize = true // eager mode: materialising is a real download
	spec.ProviderConfig = provider.MustEncodeConfig(map[string]any{
		"capabilities": []string{"enumerator", "content-hasher"},
		"seed":         []string{"existing.txt=was here first"},
		"journal":      journal,
	})

	m, err := Open(t.Context(), spec, pluginRegistry(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer m.Close() //nolint:errcheck // best effort in cleanup

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Errorf("Run did not return; try: fusermount3 -u %s", spec.Mountpoint)
		}
	}()

	// The backend has no change feed, so the M7b sweep is the whole inbound path
	// — which is exactly the shape every filesystem backend in the M17–M21 group
	// has, and the shape a plugin makes ordinary.
	existing := filepath.Join(spec.DataDir, "existing.txt")
	if !waitFor(20*time.Second, func() bool {
		b, rerr := os.ReadFile(existing)
		return rerr == nil && string(b) == "was here first"
	}) {
		t.Fatalf("the sweep never materialised existing.txt from the plugin backend")
	}

	// Outbound: a write through the mountpoint has to reach the other process.
	if err := os.WriteFile(filepath.Join(spec.Mountpoint, "written.txt"), []byte("through the mount"), 0o644); err != nil {
		t.Fatalf("writing through the mount: %v", err)
	}
	if !waitFor(20*time.Second, func() bool {
		return strings.Contains(readFileOrEmpty(journal), "put written.txt 17")
	}) {
		t.Fatalf("the write never reached the backend process; journal:\n%s", readFileOrEmpty(journal))
	}
}

// A backend that offers no capabilities at all must still mount, push, and say
// so. This is the case that would regress silently if capability negotiation
// were ever replaced by a type assertion again: the proxy implements every
// optional interface, so the mount would come up with a pull loop polling a
// change feed that does not exist.
func TestPluginBackendWithNoCapabilitiesStillMounts(t *testing.T) {
	if _, err := os.Stat("/dev/fuse"); err != nil {
		testenv.Unavailable(t, testenv.FUSE, "no /dev/fuse: "+err.Error())
	}

	dir := t.TempDir()
	journal := filepath.Join(dir, "journal")

	var out logSink
	spec := baseSpec(t)
	spec.Provider = "fake"
	spec.Logger = log.New(&out, "", 0)
	spec.ProviderConfig = provider.MustEncodeConfig(map[string]any{
		"journal": journal,
	})

	m, err := Open(t.Context(), spec, pluginRegistry(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer m.Close() //nolint:errcheck // best effort in cleanup

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Errorf("Run did not return; try: fusermount3 -u %s", spec.Mountpoint)
		}
	}()

	waitMounted(t, spec)
	if err := os.WriteFile(filepath.Join(spec.Mountpoint, "x.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("writing through the mount: %v", err)
	}
	if !waitFor(20*time.Second, func() bool {
		return strings.Contains(readFileOrEmpty(journal), "put x.txt 2")
	}) {
		t.Fatalf("the write never reached the backend; journal:\n%s\nlog:\n%s", readFileOrEmpty(journal), out.String())
	}
	// The launch line names the capability set, because whether this mount has a
	// pull loop at all is the first thing an operator needs from the log.
	if !strings.Contains(out.String(), "capabilities: none") {
		t.Errorf("the mount did not report the backend's capabilities:\n%s", out.String())
	}
}

// waitMounted blocks until the filesystem is actually serving.
//
// Without it a test that writes to the mountpoint too early writes to the
// *directory underneath* it instead — the write succeeds, no event is emitted,
// and the assertion fails for a reason that has nothing to do with what is being
// tested. The probe goes into the backing dir and is looked for through the
// mount, because that pair can only both be true once the mount is up.
func waitMounted(t *testing.T, spec MountSpec) {
	t.Helper()
	probe := ".mount-probe"
	if err := os.WriteFile(filepath.Join(spec.DataDir, probe), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !waitFor(20*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(spec.Mountpoint, probe))
		return err == nil
	}) {
		t.Fatalf("the filesystem never came up at %s", spec.Mountpoint)
	}
}

// pluginRegistry builds the fake provider plugin and returns a registry that
// discovers it, the same way drivel's own does.
func pluginRegistry(t *testing.T) *provider.Registry {
	t.Helper()
	dir, err := fakePluginDir()
	if err != nil {
		t.Fatalf("%v", err)
	}
	reg := provider.NewRegistry()
	if err := plugin.NewLoader([]string{dir}, log.New(os.Stderr, "", 0)).Register(reg); err != nil {
		t.Fatalf("registering the plugin: %v", err)
	}
	return reg
}

// fakePluginDir builds the fake backend once per test binary, into a directory
// laid out the way the loader expects.
var fakePluginDir = sync.OnceValues(func() (string, error) {
	dir, err := os.MkdirTemp("", "drivel-app-plugin-")
	if err != nil {
		return "", err
	}
	src := filepath.Join("..", "..", "plugin", "testdata", plugin.BinaryPrefix+"fake")
	out := filepath.Join(dir, plugin.BinaryPrefix+"fake")
	cmd := exec.Command("go", "build", "-o", out, src)
	if combined, berr := cmd.CombinedOutput(); berr != nil {
		return "", fmt.Errorf("building the fake plugin: %w\n%s", berr, combined)
	}
	return dir, nil
})

func readFileOrEmpty(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

// logSink collects a mount's log output for assertions, safely: the mount writes
// from several goroutines.
type logSink struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (l *logSink) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logSink) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}
