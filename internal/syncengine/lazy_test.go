// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package syncengine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zishmusic/drivel/internal/fsevent"
	"github.com/zishmusic/drivel/internal/state"
	"github.com/zishmusic/drivel/provider"
)

// fakeHydrator stands in for *hydrate.Hydrator on both seams: Placeholders (the
// uploader's guard) and Materializer (the downloader's placeholder writer). It
// records placeholder paths rather than touching xattrs, so these tests run on any
// filesystem.
type fakeHydrator struct {
	mu      sync.Mutex
	holes   map[string]bool
	stamped []string
}

func newFakeHydrator(paths ...string) *fakeHydrator {
	h := &fakeHydrator{holes: map[string]bool{}}
	for _, p := range paths {
		h.holes[p] = true
	}
	return h
}

func (h *fakeHydrator) IsPlaceholder(rel string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.holes[rel]
}

func (h *fakeHydrator) CreatePlaceholder(rel string, f provider.RemoteFile) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.holes[rel] = true
	h.stamped = append(h.stamped, rel)
	// Mirror the real hydrator closely enough for the downloader's stat-based
	// logic: an apparently-full-size file with no resident bytes.
	return nil
}

func (h *fakeHydrator) stampedPaths() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.stamped...)
}

var (
	_ Placeholders = (*fakeHydrator)(nil)
	_ Materializer = (*fakeHydrator)(nil)
)

// The M5 data-loss guard: an unhydrated placeholder holds zero bytes at its full
// apparent size, so pushing it would replace the real remote file with nothing.
// The uploader must skip it entirely.
func TestPushContentSkipsPlaceholder(t *testing.T) {
	dir := t.TempDir()
	// On disk this looks exactly like a truncated file — which is the whole reason
	// the marker is consulted instead of the size.
	writeFile(t, dir, "big.bin", strings.Repeat("\x00", 64))

	fs := newFakeStore()
	e := New(Config{Store: fs, DataDir: dir, Holes: newFakeHydrator("big.bin")})

	if err := e.push(context.Background(), fsevent.Event{Op: fsevent.OpWrite, Path: "big.bin"}); err != nil {
		t.Fatal(err)
	}
	if len(fs.calls) != 0 {
		t.Errorf("uploader called the store for a placeholder: %v; want no calls", fs.calls)
	}
}

// The guard must not suppress genuine local edits — including a legitimate
// truncation to zero bytes, which is indistinguishable from a placeholder by size
// alone.
func TestPushContentUploadsRealFilesIncludingEmptyOnes(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "edited.txt", "real local content")
	writeFile(t, dir, "truncated.txt", "")

	fs := newFakeStore()
	// Nothing is a placeholder here.
	e := New(Config{Store: fs, DataDir: dir, Holes: newFakeHydrator()})

	for _, p := range []string{"edited.txt", "truncated.txt"} {
		if err := e.push(context.Background(), fsevent.Event{Op: fsevent.OpWrite, Path: p}); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"Put(create,edited.txt,bytes=18)", "Put(create,truncated.txt,bytes=0)"}
	if len(fs.calls) != len(want) {
		t.Fatalf("calls = %v; want %v", fs.calls, want)
	}
	for i, w := range want {
		if fs.calls[i] != w {
			t.Errorf("call %d = %q; want %q", i, fs.calls[i], w)
		}
	}
}

// With no hydrator configured (eager mode, M1–M4), behaviour is unchanged.
func TestPushContentEagerModeUnaffected(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "content")
	fs := newFakeStore()
	e := New(Config{Store: fs, DataDir: dir}) // Holes nil

	if err := e.push(context.Background(), fsevent.Event{Op: fsevent.OpWrite, Path: "a.txt"}); err != nil {
		t.Fatal(err)
	}
	if len(fs.calls) != 1 || fs.calls[0] != "Put(create,a.txt,bytes=7)" {
		t.Errorf("calls = %v; want a single Put", fs.calls)
	}
}

// In lazy mode a new remote file becomes a placeholder — no content is fetched
// until something reads it.
func TestApplyCreatesPlaceholderInsteadOfDownloading(t *testing.T) {
	dir := t.TempDir()
	fs := newFakeStore()
	fs.content["a.txt"] = []byte("remote content")
	h := newFakeHydrator()
	st := newState(t)
	d := NewDownloader(nil, fs, dir, st, DefaultCadence).Lazy(h)

	ch := provider.RemoteChange{Path: "a.txt", File: remoteFile("a.txt", "remote content")}
	if err := d.apply(context.Background(), ch); err != nil {
		t.Fatal(err)
	}

	if got := h.stampedPaths(); len(got) != 1 || got[0] != "a.txt" {
		t.Errorf("stamped = %v; want [a.txt]", got)
	}
	for _, c := range fs.calls {
		if strings.HasPrefix(c, "Get(") {
			t.Errorf("lazy mode fetched content during apply: %v", fs.calls)
		}
	}
	// The echo is still recorded, so the change feed re-reporting this same content
	// is recognised and dropped exactly as in eager mode.
	if e, ok, err := st.GetEcho("a.txt"); err != nil || !ok || !e.Matches(md5hex("remote content"), "1") {
		t.Errorf("echo = %+v, ok=%t, err=%v; want the remote content recorded", e, ok, err)
	}
}

// A remote change to a path that is still an unhydrated placeholder must re-stamp
// it, NOT manufacture a conflict copy. Hashing a placeholder would digest a hole
// and read as a divergent local edit.
func TestRemoteChangeOnPlaceholderDoesNotConflict(t *testing.T) {
	dir := t.TempDir()
	fs := newFakeStore()
	h := newFakeHydrator()
	st := newState(t)
	d := NewDownloader(nil, fs, dir, st, DefaultCadence).Lazy(h)

	// First sighting: becomes a placeholder.
	first := provider.RemoteChange{Path: "a.txt", File: remoteFile("a.txt", "v1")}
	if err := d.apply(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	// The remote changes again before anyone reads the file.
	second := provider.RemoteChange{Path: "a.txt", File: remoteFile("a.txt", "v2-longer")}
	if err := d.apply(context.Background(), second); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "conflict") {
			t.Fatalf("a placeholder produced a bogus conflict copy: %s", e.Name())
		}
	}
	if got := h.stampedPaths(); len(got) != 2 {
		t.Errorf("stamped = %v; want the placeholder re-stamped twice", got)
	}
}

// Conflict copies must always hold real bytes: their paths exist only locally, so
// a placeholder there could never be hydrated. This is the one place lazy mode
// still downloads in full.
func TestConflictCopyIsAlwaysDownloadedInFull(t *testing.T) {
	dir := t.TempDir()
	fs := newFakeStore()
	fs.content["a.txt"] = []byte("remote version")
	h := newFakeHydrator() // nothing is a placeholder: the local file is hydrated
	st := newState(t)
	d := NewDownloader(nil, fs, dir, st, DefaultCadence).Lazy(h)

	// A hydrated local file with a divergent edit, newer than the remote.
	writeFile(t, dir, "a.txt", "local edit")
	local := time.Now()
	if err := os.Chtimes(filepath.Join(dir, "a.txt"), local, local); err != nil {
		t.Fatal(err)
	}
	if err := st.SetEcho("a.txt", state.Echo{Hash: md5hex("base"), Version: "1"}); err != nil {
		t.Fatal(err)
	}

	rf := remoteFile("a.txt", "remote version")
	rf.Modified = local.Add(-time.Hour) // remote is older => local wins the real path
	if err := d.apply(context.Background(), provider.RemoteChange{Path: "a.txt", File: rf}); err != nil {
		t.Fatal(err)
	}

	// The local edit keeps the real path...
	if got, ok := readBacking(t, dir, "a.txt"); !ok || got != "local edit" {
		t.Errorf("a.txt = %q (exists=%t); want the local edit preserved", got, ok)
	}
	// ...and the remote version lands beside it with real content, not a hole.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var copyName string
	for _, e := range entries {
		if strings.Contains(e.Name(), "conflict") {
			copyName = e.Name()
		}
	}
	if copyName == "" {
		t.Fatal("no conflict copy was created")
	}
	b, err := os.ReadFile(filepath.Join(dir, copyName))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "remote version" {
		t.Errorf("conflict copy = %q; want the full remote content (it can never be hydrated later)", b)
	}
	if len(h.stampedPaths()) != 0 {
		t.Errorf("a conflict copy was stamped as a placeholder: %v", h.stampedPaths())
	}
}

// Deleting a remote file clears its hydration record along with its echo, so a
// later file at the same path doesn't inherit stale range state.
func TestDeleteClearsHydrationRecord(t *testing.T) {
	dir := t.TempDir()
	st := newState(t)
	writeFile(t, dir, "a.txt", "content")
	if err := st.SetHydration("a.txt", []byte(`{"block":4194304,"size":7}`)); err != nil {
		t.Fatal(err)
	}

	d := NewDownloader(nil, newFakeStore(), dir, st, DefaultCadence).Lazy(newFakeHydrator())
	if err := d.apply(context.Background(), provider.RemoteChange{Path: "a.txt", Removed: true}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := st.Hydration("a.txt"); err != nil || ok {
		t.Errorf("hydration record survived a delete (ok=%t, err=%v)", ok, err)
	}
}
