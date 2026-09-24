// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package syncengine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/syshlted/drivel/provider"
)

// A provider with no change feed is a supported shape, not a degraded one. Every
// filesystem backend in the M17–M21 group is in it — SFTP, WebDAV, a plain
// directory — and for them the M7b sweep is not a safety net beneath a feed, it
// IS the inbound path. These tests cover the branch that makes that true.
//
// Before it existed the downloader was constructed only for a store implementing
// provider.ChangeSource, so a store that could enumerate but not tail got no
// downloader at all: no sweep, no reconcile, no inbound sync of any kind.

// newFeedlessFixture is newSweepFixture with no change feed behind it.
func newFeedlessFixture(t *testing.T, opts ReconcileOptions, pages ...[]provider.RemoteFile) *sweepFixture {
	t.Helper()
	f := &sweepFixture{dir: t.TempDir(), st: newState(t), push: &recPusher{}}
	f.store = newEnumStore(&f.trace, pages...)
	if opts.Push == nil {
		opts.Push = f.push
	}
	f.dl = NewDownloader(nil, f.store, f.dir, f.st, DefaultCadence).Reconcile(opts)
	return f
}

// The whole point: a remote file reaches the backing dir with no feed involved.
func TestSweepIsTheInboundPathWithoutAFeed(t *testing.T) {
	f := newFeedlessFixture(t, ReconcileOptions{Fetch: true, Interval: time.Hour},
		[]provider.RemoteFile{remote("hello.txt", "body")})
	f.store.content["hello.txt"] = []byte("body")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); f.dl.Run(ctx) }()

	waitFor(t, func() bool {
		b, err := os.ReadFile(filepath.Join(f.dir, "hello.txt"))
		return err == nil && string(b) == "body"
	}, "the sweep never materialised the remote file")

	cancel()
	<-done
}

// The cursor must NOT be persisted for a feedless provider. state.Cursor would
// then report ok=true for an empty token, which startFeed reads as "we are
// already tailing this remote" — and would skip the startup sweep, i.e. skip the
// entire inbound path, on every mount after the first.
func TestFeedlessSweepDoesNotPersistACursor(t *testing.T) {
	f := newFeedlessFixture(t, ReconcileOptions{Fetch: true, Interval: time.Hour},
		[]provider.RemoteFile{remote("a.txt", "x")})

	if _, err := f.dl.startFeed(t.Context()); err != nil {
		t.Fatalf("startFeed: %v", err)
	}
	if _, ok, err := f.st.Cursor(); err != nil {
		t.Fatalf("reading the cursor: %v", err)
	} else if ok {
		t.Fatal("a feedless sweep persisted a cursor; the next mount would skip its sweep")
	}

	// ...so a second start sweeps again, which is the only way it ever learns what
	// changed remotely while the mount was down.
	before := f.store.swept.Load()
	if _, err := f.dl.startFeed(t.Context()); err != nil {
		t.Fatalf("second startFeed: %v", err)
	}
	if got := f.store.swept.Load(); got <= before {
		t.Errorf("the second start ran %d sweeps, want another one", got-before)
	}
}

// A store that can neither tail nor enumerate has no inbound path at all. Run
// must return rather than spin: outbound sync still works, and that is the
// engine's job, not the downloader's.
func TestFeedlessDownloaderWithNothingToEnumerateReturns(t *testing.T) {
	d := NewDownloader(nil, newFakeStore(), t.TempDir(), newState(t), DefaultCadence)

	done := make(chan struct{})
	go func() { defer close(done); d.Run(t.Context()) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return for a store with no feed and no enumeration")
	}
}

// -sweep-interval 0 disables the only inbound path there is. That is a legitimate
// thing to ask for, and Run must still return rather than busy-loop on a deadline
// that never arrives.
func TestFeedlessDownloaderStopsWhenTheIntervalIsZero(t *testing.T) {
	f := newFeedlessFixture(t, ReconcileOptions{Fetch: true, Interval: 0},
		[]provider.RemoteFile{remote("a.txt", "x")})

	done := make(chan struct{})
	go func() { defer close(done); f.dl.Run(t.Context()) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return with -sweep-interval 0")
	}
	// The initial sweep still ran: the interval governs repeats, not the first one.
	if f.store.swept.Load() == 0 {
		t.Error("no initial sweep ran")
	}
}

// The interval is the poll interval here, so a second sweep has to arrive without
// anything else prompting it.
func TestFeedlessDownloaderSweepsPeriodically(t *testing.T) {
	f := newFeedlessFixture(t, ReconcileOptions{Fetch: true, Interval: time.Millisecond},
		[]provider.RemoteFile{remote("a.txt", "x")})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); f.dl.Run(ctx) }()

	waitFor(t, func() bool { return f.store.swept.Load() >= 2 }, "no repeat sweep arrived")
	cancel()
	<-done
}

// A provider with no content checksum must still be able to conclude that a local
// file is unmodified, or a remote deletion is never applied: the file is kept,
// pushed straight back up, and the deletion is undone on every sweep forever.
// Before state.Echo carried a local fingerprint that is exactly what happened,
// against a live OpenSSH server.
func TestHashlessRemoteDeleteIsAppliedLocally(t *testing.T) {
	f := newFeedlessFixture(t, ReconcileOptions{Fetch: true, Interval: time.Hour},
		[]provider.RemoteFile{{Path: "doomed.txt", Size: 4, Version: "1"}}) // no Hash: an SFTP-shaped listing
	f.store.content["doomed.txt"] = []byte("body")

	// Sweep once: the file arrives locally and a baseline is recorded.
	if _, err := f.dl.startFeed(t.Context()); err != nil {
		t.Fatalf("startFeed: %v", err)
	}
	dst := filepath.Join(f.dir, "doomed.txt")
	if _, err := os.Stat(dst); err != nil {
		t.Fatalf("the file was never materialised: %v", err)
	}
	echo, ok, err := f.st.GetEcho("doomed.txt")
	if err != nil || !ok {
		t.Fatalf("no baseline recorded: ok=%v err=%v", ok, err)
	}
	if echo.Hash != "" {
		t.Fatalf("this fixture is meant to be hashless, but the baseline has %q", echo.Hash)
	}
	if echo.LocalMTime.IsZero() {
		t.Fatal("no local fingerprint recorded; a remote delete could never be applied")
	}

	// Now the file is gone remotely. A second sweep must delete it locally.
	f.store.pages = [][]provider.RemoteFile{{}}
	if _, err := f.dl.startFeed(t.Context()); err != nil {
		t.Fatalf("second startFeed: %v", err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Errorf("a remote deletion was not applied locally: %v", err)
	}
}

// ...and the guard it must not break: a local copy that really was edited is kept
// and pushed back, never deleted. Getting this wrong loses what someone wrote.
func TestHashlessLocallyModifiedFileSurvivesARemoteDelete(t *testing.T) {
	f := newFeedlessFixture(t, ReconcileOptions{Fetch: true, Interval: time.Hour},
		[]provider.RemoteFile{{Path: "edited.txt", Size: 4, Version: "1"}})
	f.store.content["edited.txt"] = []byte("body")

	if _, err := f.dl.startFeed(t.Context()); err != nil {
		t.Fatalf("startFeed: %v", err)
	}
	dst := filepath.Join(f.dir, "edited.txt")
	if err := os.WriteFile(dst, []byte("edited locally, after the baseline"), 0o600); err != nil {
		t.Fatalf("editing: %v", err)
	}

	f.store.pages = [][]provider.RemoteFile{{}}
	if _, err := f.dl.startFeed(t.Context()); err != nil {
		t.Fatalf("second startFeed: %v", err)
	}
	b, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("a locally-edited file was deleted by a remote deletion: %v", err)
	}
	if string(b) != "edited locally, after the baseline" {
		t.Errorf("content = %q", b)
	}
}

// waitFor polls cond until it holds or the test's patience runs out.
func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal(msg)
}
