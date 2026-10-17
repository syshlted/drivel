// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/syshlted/drivel/internal/control"
	"github.com/syshlted/drivel/internal/syncengine"
)

// The client is the control API's acceptance test (M14): it is written against
// the documented schema with types of its own, so this runs it against the real
// server and reads what it prints.

type ctlFakeMount struct {
	mp  string
	mon *syncengine.Monitor
	r   syncengine.StatusReader
}

func (f *ctlFakeMount) Name() string                 { return "work" }
func (f *ctlFakeMount) Mountpoint() string           { return f.mp }
func (f *ctlFakeMount) Provider() string             { return "fake" }
func (f *ctlFakeMount) Lazy() bool                   { return false }
func (f *ctlFakeMount) Stats() syncengine.Stats      { return f.mon.Snapshot() }
func (f *ctlFakeMount) Monitor() *syncengine.Monitor { return f.mon }
func (f *ctlFakeMount) Status(rel string) (syncengine.PathStatus, error) {
	return f.r.Path(rel)
}
func (f *ctlFakeMount) Children(rel, after string, limit int) ([]syncengine.PathStatus, bool, error) {
	return f.r.Children(rel, after, limit)
}
func (f *ctlFakeMount) Pause(_ context.Context, d time.Duration) (time.Time, error) {
	return time.Now().Add(d), nil
}
func (f *ctlFakeMount) Resume(context.Context) error { return nil }

// captureStdout runs fn with os.Stdout redirected and returns what it printed.
func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	runErr := fn()
	os.Stdout = old
	w.Close()
	b, _ := io.ReadAll(r)
	return string(b), runErr
}

func TestTheClientAgainstTheServer(t *testing.T) {
	// The fake mount's "mountpoint" is its backing dir, so a local path the user
	// types resolves to a file that exists.
	data := t.TempDir()
	if err := os.MkdirAll(filepath.Join(data, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "sub", "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	mon := syncengine.NewMonitor("work")
	m := &ctlFakeMount{mp: data, mon: mon, r: syncengine.StatusReader{Monitor: mon, DataDir: data}}

	dir, err := os.MkdirTemp("", "dctl")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "drivel", control.SocketName)
	ln, err := control.Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	stopped := make(chan struct{}, 1)
	srv := control.New([]control.Mount{m}, func(string) { stopped <- struct{}{} }, log.New(io.Discard, "", 0))
	go func() { _ = srv.Serve(ln) }()

	out, err := captureStdout(t, func() error { return runStatus([]string{"-socket", sock}) })
	if err != nil || !strings.Contains(out, "work") || !strings.Contains(out, "idle") {
		t.Errorf("drivel status: %v\n%s", err, out)
	}
	out, err = captureStdout(t, func() error {
		return runStatus([]string{"-socket", sock, filepath.Join(data, "sub")})
	})
	if err != nil || !strings.Contains(out, "unknown") || !strings.Contains(out, "a.txt") {
		t.Errorf("drivel status DIR: %v\n%s", err, out)
	}
	if _, err := captureStdout(t, func() error {
		return runStatus([]string{"-socket", sock, "/definitely/not/a/mount"})
	}); err == nil {
		t.Error("a path outside every mount was reported without error")
	}
	out, err = captureStdout(t, func() error { return runPause([]string{"-socket", sock, "-for", "10m"}) })
	if err != nil || !strings.Contains(out, "work: paused until") {
		t.Errorf("drivel pause: %v\n%s", err, out)
	}
	if err := runPause([]string{"-socket", sock, "-name", "nope"}); err == nil || !strings.Contains(err.Error(), "work") {
		t.Errorf("pausing an unknown mount: %v; want an error naming the mounts that exist", err)
	}

	// stop asks, then waits for the socket to close — which here is the test
	// closing the server once the callback ran, as the process exiting would.
	go func() {
		<-stopped
		_ = srv.Close()
	}()
	if _, err := captureStdout(t, func() error { return runStop([]string{"-socket", sock}) }); err != nil {
		t.Errorf("drivel stop: %v", err)
	}
}

func TestControlSocketNeedsControl(t *testing.T) {
	if err := validateControlFlags(false, true); err == nil {
		t.Error("-control-socket without -control was accepted")
	}
	if err := validateControlFlags(true, true); err != nil {
		t.Error(err)
	}
}
