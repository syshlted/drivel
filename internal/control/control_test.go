// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package control

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/syshlted/drivel/internal/fsevent"
	"github.com/syshlted/drivel/internal/syncengine"
	"github.com/syshlted/drivel/provider"
)

// fakeMount answers from a StatusReader over a temp dir and a real monitor, so
// the server is exercised against the same resolution app.Mount uses.
type fakeMount struct {
	name   string
	mon    *syncengine.Monitor
	r      syncengine.StatusReader
	mu     sync.Mutex
	paused bool
	pauseD time.Duration
}

func newFakeMount(t *testing.T, name string) *fakeMount {
	dir := t.TempDir()
	mon := syncengine.NewMonitor(name)
	return &fakeMount{name: name, mon: mon, r: syncengine.StatusReader{Monitor: mon, DataDir: dir}}
}

func (f *fakeMount) Name() string                                     { return f.name }
func (f *fakeMount) Mountpoint() string                               { return "/mnt/" + f.name }
func (f *fakeMount) Provider() string                                 { return "fake" }
func (f *fakeMount) Lazy() bool                                       { return false }
func (f *fakeMount) Stats() syncengine.Stats                          { return f.mon.Snapshot() }
func (f *fakeMount) Monitor() *syncengine.Monitor                     { return f.mon }
func (f *fakeMount) Status(rel string) (syncengine.PathStatus, error) { return f.r.Path(rel) }
func (f *fakeMount) Children(rel, after string, limit int) ([]syncengine.PathStatus, bool, error) {
	return f.r.Children(rel, after, limit)
}

func (f *fakeMount) Pause(_ context.Context, d time.Duration) (time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paused, f.pauseD = true, d
	return time.Now().Add(d), nil
}

func (f *fakeMount) Resume(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paused = false
	return nil
}

// socketDir returns a private directory short enough for a unix socket path
// (108 bytes on Linux; t.TempDir under a long test name can exceed it).
func socketDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "dctl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return filepath.Join(d, "drivel")
}

type harness struct {
	sock     string
	client   *http.Client
	shutdown chan string
	m        *fakeMount
}

func serve(t *testing.T) *harness {
	t.Helper()
	h := &harness{sock: filepath.Join(socketDir(t), SocketName), shutdown: make(chan string, 1), m: newFakeMount(t, "work")}
	ln, err := Listen(h.sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := New([]Mount{h.m}, func(r string) { h.shutdown <- r }, log.New(io.Discard, "", 0))
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	h.client = &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", h.sock)
	}}}
	return h
}

func (h *harness) req(t *testing.T, method, path string, out any) int {
	t.Helper()
	r, err := http.NewRequest(method, "http://drivel"+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Accepted and ignored from day one, so a token scheme can be added later.
	r.Header.Set("Authorization", "Bearer anything")
	resp, err := h.client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("%s %s: decoding: %v", method, path, err)
		}
	}
	return resp.StatusCode
}

func TestTheSocketIsPrivate(t *testing.T) {
	h := serve(t)
	fi, err := os.Stat(h.sock)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("socket mode %04o; want 0600", fi.Mode().Perm())
	}
	di, err := os.Stat(filepath.Dir(h.sock))
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Errorf("socket directory mode %04o; want 0700", di.Mode().Perm())
	}
}

// A live socket is never stolen; a dead one is replaced.
func TestListenConnectsBeforeRemoving(t *testing.T) {
	h := serve(t)
	if _, err := Listen(h.sock); err == nil || !strings.Contains(err.Error(), "already serving") {
		t.Fatalf("a second Listen on a live socket: %v; want a refusal", err)
	}

	dir := socketDir(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, SocketName)
	ln, err := net.Listen("unix", stale)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close() // the file stays and nothing answers: a crashed daemon's socket
	ln2, err := Listen(stale)
	if err != nil {
		t.Fatalf("Listen over a stale socket: %v", err)
	}
	ln2.Close()

	notSock := filepath.Join(dir, "plain")
	if err := os.WriteFile(notSock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(notSock); err == nil {
		t.Error("Listen replaced a regular file")
	}
}

func TestListenRefusesAnOpenDirectory(t *testing.T) {
	dir := socketDir(t)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(filepath.Join(dir, SocketName)); err == nil || !strings.Contains(err.Error(), "0700") {
		t.Errorf("Listen in a 0755 directory: %v; want a refusal naming the mode", err)
	}
}

func TestListenNamesAnOverlongPath(t *testing.T) {
	long := "/" + strings.Repeat("d", maxSocketPath) + "/" + SocketName
	if _, err := Listen(long); err == nil || !strings.Contains(err.Error(), "-control-socket") {
		t.Errorf("an overlong path: %v; want an error saying what to do", err)
	}
}

func TestDefaultSocketPathNeedsARuntimeDir(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")
	if _, err := DefaultSocketPath(); err == nil {
		t.Error("no XDG_RUNTIME_DIR, and a path was invented anyway")
	}
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	if p, err := DefaultSocketPath(); err != nil || p != "/run/user/1000/drivel/control.sock" {
		t.Errorf("DefaultSocketPath = %q, %v", p, err)
	}
}

func TestTheRoutes(t *testing.T) {
	h := serve(t)

	var v versionBody
	if c := h.req(t, "GET", "/v1/version", &v); c != 200 || v.API != 1 || v.PID != os.Getpid() {
		t.Errorf("version: %d %+v", c, v)
	}

	var ms mountsBody
	if c := h.req(t, "GET", "/v1/mounts", &ms); c != 200 || len(ms.Mounts) != 1 || ms.Mounts[0].Name != "work" ||
		ms.Mounts[0].Reachability != syncengine.ReachUnknown {
		t.Errorf("mounts: %d %+v", c, ms)
	}

	var d map[string]any
	if c := h.req(t, "GET", "/v1/mounts/work", &d); c != 200 {
		t.Errorf("mount detail: %d", c)
	}
	for _, k := range []string{"outbound", "inbound", "hydration", "queue", "reach", "sweep", "pause"} {
		if _, ok := d[k]; !ok {
			t.Errorf("mount detail has no %q section", k)
		}
	}

	var e errorBody
	if c := h.req(t, "GET", "/v1/mounts/nope", &e); c != 404 || e.Error.Code != "no_such_mount" {
		t.Errorf("unknown mount: %d %+v", c, e)
	}
	if c := h.req(t, "GET", "/v1/mounts/work/status?path="+url.QueryEscape("../x"), &e); c != 400 || e.Error.Code != "bad_path" {
		t.Errorf("escaping path: %d %+v", c, e)
	}
	if c := h.req(t, "GET", "/v1/mounts/work/status?path=missing", &e); c != 404 || e.Error.Code != "no_such_path" {
		t.Errorf("missing path: %d %+v", c, e)
	}

	if err := os.WriteFile(filepath.Join(h.m.r.DataDir, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	var p pathBody
	if c := h.req(t, "GET", "/v1/mounts/work/status?path=a.txt", &p); c != 200 || p.Status != "unknown" || p.Detail == "" {
		t.Errorf("status: %d %+v", c, p)
	}
	var ch childrenBody
	if c := h.req(t, "GET", "/v1/mounts/work/children?path=", &ch); c != 200 || len(ch.Entries) != 1 || ch.Truncated {
		t.Errorf("children: %d %+v", c, ch)
	}
	if c := h.req(t, "GET", "/v1/mounts/work/children?limit=0", &e); c != 400 {
		t.Errorf("limit=0: %d", c)
	}

	if c := h.req(t, "POST", "/v1/mounts/work/pause?for=48h", &e); c != 400 || e.Error.Code != "bad_duration" {
		t.Errorf("an over-long pause: %d %+v", c, e)
	}
	var pb pauseBody
	if c := h.req(t, "POST", "/v1/mounts/work/pause?for=30m", &pb); c != 200 || !pb.Paused || h.m.pauseD != 30*time.Minute {
		t.Errorf("pause: %d %+v (got %s)", c, pb, h.m.pauseD)
	}
	if c := h.req(t, "POST", "/v1/mounts/work/resume", &pb); c != 200 || pb.Paused {
		t.Errorf("resume: %d %+v", c, pb)
	}
	if c := h.req(t, "GET", "/v1/mounts/work/pause", &e); c != 405 && c != 404 {
		t.Errorf("GET on a POST route: %d", c)
	}
	if c := h.req(t, "GET", "/debug/pprof/cmdline", &e); c != 404 {
		t.Errorf("cmdline served over the socket: %d", c)
	}

	resp, err := h.client.Get("http://drivel/v1/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, want := range []string{`drivel_uploads_total{mount="work"} 0`, `drivel_reachability{mount="work",state="unknown"} 1`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("metrics lack %q", want)
		}
	}
}

// Shutdown answers first, then runs the signal path through the callback.
func TestShutdownRunsTheCallback(t *testing.T) {
	h := serve(t)
	var sb shutdownBody
	if c := h.req(t, "POST", "/v1/shutdown", &sb); c != 202 || !sb.ShuttingDown || sb.DrainTimeoutSeconds != 30 {
		t.Errorf("shutdown: %d %+v", c, sb)
	}
	select {
	case r := <-h.shutdown:
		if !strings.Contains(r, "control socket") {
			t.Errorf("the shutdown reason does not say where it came from: %q", r)
		}
	case <-time.After(time.Second):
		t.Fatal("the shutdown callback never ran")
	}
}

func TestTheEventStream(t *testing.T) {
	h := serve(t)
	resp, err := h.client.Get("http://drivel/v1/events?mount=work")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	next := func() streamLine {
		t.Helper()
		if !sc.Scan() {
			t.Fatalf("stream ended: %v", sc.Err())
		}
		var l streamLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			t.Fatalf("not JSON: %q", sc.Text())
		}
		return l
	}
	if l := next(); l.Type != "resync" {
		t.Fatalf("first line %+v; want a resync marker", l)
	}

	// A real engine, so the transition comes the way it does in production: an
	// event through the run loop, a push, and the monitor publishing the result.
	if err := os.WriteFile(filepath.Join(h.m.r.DataDir, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := syncengine.New(syncengine.Config{Store: nopStore{}, DataDir: h.m.r.DataDir, Monitor: h.m.mon,
		Debounce: time.Millisecond, Logger: log.New(io.Discard, "", 0)})
	events := make(chan fsevent.Event, 1)
	done := make(chan struct{})
	go func() { defer close(done); e.Run(context.Background(), events) }()
	events <- fsevent.Event{Op: fsevent.OpWrite, Path: "a.txt"}
	defer func() { close(events); <-done }()

	// Coalesced over the push delay: the settled status arrives, not every step.
	for {
		l := next()
		if l.Type != "status" || l.Path != "a.txt" || l.Mount != "work" {
			t.Fatalf("unexpected line %+v", l)
		}
		if l.Status == "synced" {
			return
		}
	}
}

// nopStore accepts everything.
type nopStore struct{}

func (nopStore) Put(_ context.Context, p string, r io.Reader) (provider.RemoteFile, error) {
	n, _ := io.Copy(io.Discard, r)
	return provider.RemoteFile{Path: p, Size: n, Version: "1"}, nil
}
func (nopStore) Mkdir(_ context.Context, p string) (provider.RemoteFile, error) {
	return provider.RemoteFile{Path: p, IsDir: true}, nil
}
func (nopStore) Move(_ context.Context, _, p string) (provider.RemoteFile, error) {
	return provider.RemoteFile{Path: p}, nil
}
func (nopStore) Remove(context.Context, string) error { return nil }
func (nopStore) Get(context.Context, string) (io.ReadCloser, error) {
	return nil, provider.ErrNotExist
}
func (nopStore) Stat(context.Context, string) (provider.RemoteFile, bool, error) {
	return provider.RemoteFile{}, false, nil
}
