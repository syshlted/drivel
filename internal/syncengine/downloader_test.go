// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package syncengine

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/syshlted/drivel/internal/state"
	"github.com/syshlted/drivel/provider"
)

// fakeSource is a scripted provider.ChangeSource: it hands out a fixed sequence
// of change batches, then reports empty. onDrain fires once, after the last batch
// is consumed, so a Run test can cancel itself deterministically.
type fakeSource struct {
	mu      sync.Mutex
	start   string
	batches [][]provider.RemoteChange
	cursors []string
	calls   int
	onDrain func()
}

func (s *fakeSource) StartCursor(context.Context) (string, error) { return s.start, nil }

func (s *fakeSource) Changes(_ context.Context, cursor string) ([]provider.RemoteChange, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.calls >= len(s.batches) {
		if s.onDrain != nil {
			f := s.onDrain
			s.onDrain = nil
			f()
		}
		return nil, cursor, nil
	}
	b, next := s.batches[s.calls], s.cursors[s.calls]
	s.calls++
	return b, next, nil
}

func md5hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func newState(t *testing.T) *state.Store {
	t.Helper()
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func remoteFile(path, content string) *provider.RemoteFile {
	return &provider.RemoteFile{Path: path, Size: int64(len(content)), Hash: md5hex(content), Version: "1"}
}

func readBacking(t *testing.T, dir, rel string) (string, bool) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if os.IsNotExist(err) {
		return "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(b), true
}

// A genuine remote file is downloaded into the backing dir and its echo recorded.
func TestApplyDownloadsRemoteFile(t *testing.T) {
	dir := t.TempDir()
	fs := newFakeStore()
	fs.content["notes.txt"] = []byte("remote body")
	d := NewDownloader(nil, fs, dir, newState(t), DefaultCadence)

	err := d.apply(context.Background(), provider.RemoteChange{Path: "notes.txt", File: remoteFile("notes.txt", "remote body")})
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := readBacking(t, dir, "notes.txt"); !ok || got != "remote body" {
		t.Fatalf("backing content = %q, ok=%v; want %q", got, ok, "remote body")
	}
	if e, ok, _ := d.state.GetEcho("notes.txt"); !ok || e.Hash != md5hex("remote body") {
		t.Fatalf("echo not recorded after download: %+v ok=%v", e, ok)
	}
}

// Our own upload coming back on the feed (echo record already present) is dropped:
// no Get, no write.
func TestApplyDropsEcho(t *testing.T) {
	dir := t.TempDir()
	fs := newFakeStore()
	st := newState(t)
	// Simulate the uploader having recorded this content as ours.
	if err := st.SetEcho("mine.txt", state.Echo{Hash: md5hex("v1"), Version: "1", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	d := NewDownloader(nil, fs, dir, st, DefaultCadence)

	if err := d.apply(context.Background(), provider.RemoteChange{Path: "mine.txt", File: remoteFile("mine.txt", "v1")}); err != nil {
		t.Fatal(err)
	}
	if _, ok := readBacking(t, dir, "mine.txt"); ok {
		t.Fatal("echo change was applied; expected it to be dropped")
	}
	if len(fs.calls) != 0 {
		t.Fatalf("store was called on an echo: %v", fs.calls)
	}
}

// If the backing file already holds identical bytes (hash match) but no echo
// record exists, the download is skipped (loop breaker §4.4) and the echo recorded.
func TestApplySkipsWhenLocalContentMatches(t *testing.T) {
	dir := t.TempDir()
	fs := newFakeStore()
	writeFile(t, dir, "same.txt", "identical")
	d := NewDownloader(nil, fs, dir, newState(t), DefaultCadence)

	if err := d.apply(context.Background(), provider.RemoteChange{Path: "same.txt", File: remoteFile("same.txt", "identical")}); err != nil {
		t.Fatal(err)
	}
	if len(fs.calls) != 0 {
		t.Fatalf("downloaded despite matching local content: %v", fs.calls)
	}
	if e, ok, _ := d.state.GetEcho("same.txt"); !ok || e.Hash != md5hex("identical") {
		t.Fatalf("echo not recorded for matching local file: %+v ok=%v", e, ok)
	}
}

// A removal deletes the backing file and clears its echo record.
func TestApplyRemoval(t *testing.T) {
	dir := t.TempDir()
	fs := newFakeStore()
	st := newState(t)
	writeFile(t, dir, "gone.txt", "bye")
	_ = st.SetEcho("gone.txt", state.Echo{Hash: md5hex("bye"), At: time.Now()})
	d := NewDownloader(nil, fs, dir, st, DefaultCadence)

	if err := d.apply(context.Background(), provider.RemoteChange{Path: "gone.txt", Removed: true}); err != nil {
		t.Fatal(err)
	}
	if _, ok := readBacking(t, dir, "gone.txt"); ok {
		t.Fatal("file still present after removal")
	}
	if _, ok, _ := d.state.GetEcho("gone.txt"); ok {
		t.Fatal("echo record survived removal")
	}
}

// A directory change creates the directory.
func TestApplyMkdir(t *testing.T) {
	dir := t.TempDir()
	d := NewDownloader(nil, newFakeStore(), dir, newState(t), DefaultCadence)

	rf := &provider.RemoteFile{Path: "sub/deep", IsDir: true, Version: "1"}
	if err := d.apply(context.Background(), provider.RemoteChange{Path: "sub/deep", File: rf}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "sub", "deep"))
	if err != nil || !info.IsDir() {
		t.Fatalf("directory not created: err=%v", err)
	}
}

// Run resumes from a fresh start cursor, applies a batch, and persists the
// advanced cursor.
func TestRunAppliesBatchAndPersistsCursor(t *testing.T) {
	dir := t.TempDir()
	fs := newFakeStore()
	fs.content["a.txt"] = []byte("alpha")
	st := newState(t)

	ctx, cancel := context.WithCancel(context.Background())
	src := &fakeSource{
		start:   "cur0",
		batches: [][]provider.RemoteChange{{{Path: "a.txt", File: remoteFile("a.txt", "alpha")}}},
		cursors: []string{"cur1"},
		onDrain: cancel, // stop the loop once the scripted batch is consumed
	}
	d := NewDownloader(src, fs, dir, st, Cadence{Fast: time.Millisecond, Slow: 5 * time.Millisecond})
	d.Run(ctx)

	if got, ok := readBacking(t, dir, "a.txt"); !ok || got != "alpha" {
		t.Fatalf("batch not applied: got %q ok=%v", got, ok)
	}
	if cur, ok, _ := st.Cursor(); !ok || cur != "cur1" {
		t.Fatalf("cursor = %q ok=%v; want cur1", cur, ok)
	}
}

// On first run (no persisted cursor) the downloader adopts the source's start
// token so it only sees changes from "now" forward.
func TestResumeCursorFirstRunUsesStartToken(t *testing.T) {
	st := newState(t)
	src := &fakeSource{start: "start-tok"}
	d := NewDownloader(src, newFakeStore(), t.TempDir(), st, DefaultCadence)

	cur, err := d.resumeCursor(context.Background())
	if err != nil || cur != "start-tok" {
		t.Fatalf("resumeCursor = %q, %v; want start-tok", cur, err)
	}
	if persisted, ok, _ := st.Cursor(); !ok || persisted != "start-tok" {
		t.Fatalf("start token not persisted: %q ok=%v", persisted, ok)
	}
}

// A page's removals are dropped in one commit at the end of the page, so a path
// removed and then re-created within that same page must not have the fresh echo
// deleted by the deferred flush. Getting this wrong makes the next report of the
// file unrecognisable as one we already hold.
func TestApplyToCancelsPendingForgetOnRecreate(t *testing.T) {
	dir := t.TempDir()
	fs := newFakeStore()
	fs.content["doc.txt"] = []byte("second")
	st := newState(t)
	writeFile(t, dir, "doc.txt", "first")
	_ = st.SetEcho("doc.txt", state.Echo{Hash: md5hex("first"), At: time.Now()})
	d := NewDownloader(nil, fs, dir, st, DefaultCadence)

	forget := map[string]struct{}{}
	ctx := context.Background()
	if err := d.applyTo(ctx, provider.RemoteChange{Path: "doc.txt", Removed: true}, forget); err != nil {
		t.Fatal(err)
	}
	if _, pending := forget["doc.txt"]; !pending {
		t.Fatal("removal did not queue the record for dropping")
	}
	if err := d.applyTo(ctx, provider.RemoteChange{Path: "doc.txt", File: remoteFile("doc.txt", "second")}, forget); err != nil {
		t.Fatal(err)
	}
	if _, pending := forget["doc.txt"]; pending {
		t.Fatal("re-creation left the path queued for dropping; the flush would erase its new echo")
	}
	if err := d.flushForget(forget); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.GetEcho("doc.txt"); !ok {
		t.Fatal("echo for the re-created file was erased by the flush")
	}
	if got, _ := readBacking(t, dir, "doc.txt"); got != "second" {
		t.Fatalf("backing file = %q; want %q", got, "second")
	}
}

// The batched path has to leave exactly the state the per-change path did.
func TestFlushForgetClearsBothBuckets(t *testing.T) {
	st := newState(t)
	for _, p := range []string{"a.txt", "b.txt"} {
		_ = st.SetEcho(p, state.Echo{Hash: md5hex(p), At: time.Now()})
		_ = st.SetHydration(p, []byte{1, 2, 3})
	}
	d := NewDownloader(nil, newFakeStore(), t.TempDir(), st, DefaultCadence)

	if err := d.flushForget(map[string]struct{}{"a.txt": {}, "b.txt": {}}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"a.txt", "b.txt"} {
		if _, ok, _ := st.GetEcho(p); ok {
			t.Errorf("echo for %s survived the flush", p)
		}
		if _, ok, _ := st.Hydration(p); ok {
			t.Errorf("hydration bitmap for %s survived the flush", p)
		}
	}
}
