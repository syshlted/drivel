// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package syncengine

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/syshlted/drivel/internal/state"
	"github.com/syshlted/drivel/provider"
)

// flakyStore serves content like fakeStore, reports it truthfully from Stat
// (hash and all, which is what a retry applies), and fails the next failGets
// downloads — or every one, at -1.
type flakyStore struct {
	*fakeStore
	failGets int
	gets     int
}

func newFlakyStore() *flakyStore { return &flakyStore{fakeStore: newFakeStore()} }

var errFlaky = errors.New("connection reset by peer")

func (f *flakyStore) Get(ctx context.Context, p string) (io.ReadCloser, error) {
	f.mu.Lock()
	f.gets++
	fail := f.failGets != 0
	if f.failGets > 0 {
		f.failGets--
	}
	f.mu.Unlock()
	if fail {
		return nil, errFlaky
	}
	return f.fakeStore.Get(ctx, p)
}

func (f *flakyStore) Stat(_ context.Context, p string) (provider.RemoteFile, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.content[p]
	if !ok {
		return provider.RemoteFile{}, false, nil
	}
	return *remoteFile(p, string(b)), true, nil
}

func refetchOwed(t *testing.T, st *state.Store, p string) bool {
	t.Helper()
	recs, err := st.Lookup([]string{p})
	if err != nil {
		t.Fatal(err)
	}
	return recs[p].OwesFetch
}

// fireRefetch runs the retry pass as if its deadline had passed.
func fireRefetch(d *Downloader) {
	d.refetchDue = time.Now().Add(-time.Second)
	d.dueRefetch(context.Background())
}

// MC-46's finding, end to end: a download that fails while the cursor advances
// past it is fetched again by the pull loop itself, with no sweep anywhere —
// this downloader cannot enumerate at all, which is the -sweep-interval 0 shape.
// Before the refetch bucket the file stayed missing until a restart.
func TestAFailedDownloadIsRetriedWithoutASweep(t *testing.T) {
	dir := t.TempDir()
	fs := newFlakyStore()
	fs.content["a.txt"] = []byte("alpha")
	fs.failGets = 1
	st := newState(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	src := &fakeSource{
		start:   "cur0",
		batches: [][]provider.RemoteChange{{{Path: "a.txt", File: remoteFile("a.txt", "alpha")}}},
		cursors: []string{"cur1"},
	}
	d := NewDownloader(src, fs, dir, st, Cadence{Fast: time.Millisecond, Slow: 5 * time.Millisecond})
	d.refetchBase = 10 * time.Millisecond

	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	for {
		if _, ok := readBacking(t, dir, "a.txt"); ok {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("the failed download was never retried")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	<-done

	if got, _ := readBacking(t, dir, "a.txt"); got != "alpha" {
		t.Fatalf("a.txt = %q; want alpha", got)
	}
	if cur, _, _ := st.Cursor(); cur != "cur1" {
		t.Fatalf("cursor = %q; want cur1 — the failure must not hold the feed", cur)
	}
	if refetchOwed(t, st, "a.txt") {
		t.Fatal("the refetch record survived a successful retry")
	}
	if e, ok, _ := st.GetEcho("a.txt"); !ok || e.Hash != md5hex("alpha") {
		t.Fatalf("no baseline after the retry: %+v ok=%v", e, ok)
	}
}

// The retry applies what the remote holds when it runs, not the change that
// failed: replaying a stale description would put an old version on disk.
func TestARetryFetchesWhatTheRemoteHoldsNow(t *testing.T) {
	dir := t.TempDir()
	fs := newFlakyStore()
	fs.content["a.txt"] = []byte("v1")
	fs.failGets = 1
	st := newState(t)
	d := NewDownloader(nil, fs, dir, st, DefaultCadence)

	ch := provider.RemoteChange{Path: "a.txt", File: remoteFile("a.txt", "v1")}
	if err := d.apply(context.Background(), ch); err == nil {
		t.Fatal("the injected failure did not fail the apply")
	}
	d.oweRefetch(ch)
	fs.content["a.txt"] = []byte("v2")

	before := st.Commits()
	fireRefetch(d)
	if got, _ := readBacking(t, dir, "a.txt"); got != "v2" {
		t.Fatalf("a.txt = %q; want v2, what the remote holds now", got)
	}
	// The download's own SetEcho discharges the record: no commit of its own.
	if got := st.Commits() - before; got != 1 {
		t.Fatalf("a successful retry cost %d commits; want 1", got)
	}
	if refetchOwed(t, st, "a.txt") || !d.refetchDue.IsZero() || d.refetchFails != 0 {
		t.Fatalf("debt standing after success: owed=%v due=%v fails=%d",
			refetchOwed(t, st, "a.txt"), d.refetchDue, d.refetchFails)
	}
}

// A path the remote no longer has is dropped and nothing local is touched — a
// retry never deletes, because what is there now may be unpushed work.
func TestARetryOfAVanishedPathTouchesNothing(t *testing.T) {
	dir := t.TempDir()
	fs := newFlakyStore()
	st := newState(t)
	d := NewDownloader(nil, fs, dir, st, DefaultCadence)
	writeFile(t, dir, "a.txt", "local work")

	d.oweRefetch(provider.RemoteChange{Path: "a.txt", File: remoteFile("a.txt", "gone")})
	fireRefetch(d)

	if got, ok := readBacking(t, dir, "a.txt"); !ok || got != "local work" {
		t.Fatalf("a.txt = %q ok=%v; the retry must leave the local file alone", got, ok)
	}
	if refetchOwed(t, st, "a.txt") {
		t.Fatal("a vanished path kept its record")
	}
}

// A failed removal is not recorded: re-applying it hours later would delete
// whatever is at the path then. See oweRefetch.
func TestAFailedRemovalIsNotRecorded(t *testing.T) {
	st := newState(t)
	d := NewDownloader(nil, newFlakyStore(), t.TempDir(), st, DefaultCadence)
	d.oweRefetch(provider.RemoteChange{Path: "a.txt", Removed: true})
	if refetchOwed(t, st, "a.txt") || !d.refetchDue.IsZero() {
		t.Fatal("a removal was recorded for retry")
	}
}

// A retry that fails again keeps the record and backs off, so a full disk costs
// an attempt an hour rather than one a minute forever.
func TestARefetchThatFailsAgainBacksOff(t *testing.T) {
	fs := newFlakyStore()
	fs.content["a.txt"] = []byte("alpha")
	fs.failGets = -1
	st := newState(t)
	d := NewDownloader(nil, fs, t.TempDir(), st, DefaultCadence)
	d.refetchBase = time.Hour

	d.oweRefetch(provider.RemoteChange{Path: "a.txt", File: remoteFile("a.txt", "alpha")})
	if w := time.Until(d.refetchDue); w < 59*time.Minute || w > time.Hour {
		t.Fatalf("first retry due in %s; want the base delay", w)
	}
	for want := 1; want <= maxRefetchBackoff+2; want++ {
		fireRefetch(d)
		if !refetchOwed(t, st, "a.txt") {
			t.Fatal("a retry that failed dropped its record")
		}
		if d.refetchFails != want {
			t.Fatalf("refetchFails = %d; want %d", d.refetchFails, want)
		}
	}
	ceiling := time.Hour << maxRefetchBackoff
	if w := time.Until(d.refetchDue); w > ceiling || w < ceiling-time.Minute {
		t.Fatalf("retry due in %s after repeated failures; want the ceiling %s", w, ceiling)
	}
}

// A record left by an earlier run is retried at once: that debt has had a
// restart's worth of time to clear.
func TestARefetchLeftByAnEarlierRunIsRetriedAtStart(t *testing.T) {
	st := newState(t)
	if err := st.OweRefetch("a.txt", time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	d := NewDownloader(nil, newFlakyStore(), t.TempDir(), st, DefaultCadence)
	d.resumeRefetches()
	if d.refetchDue.IsZero() || time.Until(d.refetchDue) > 0 {
		t.Fatalf("refetchDue = %v; want now", d.refetchDue)
	}
}

// A failure the initial sweep recorded is this run's own and waits the usual
// delay; resumeRefetches, which runs after that sweep, must not pull it forward.
func TestAFailureFromThisRunsInitialSweepIsNotRetriedAtOnce(t *testing.T) {
	d := NewDownloader(nil, newFlakyStore(), t.TempDir(), newState(t), DefaultCadence)
	d.oweRefetch(provider.RemoteChange{Path: "a.txt", File: remoteFile("a.txt", "x")})
	due := d.refetchDue
	d.resumeRefetches()
	if !d.refetchDue.Equal(due) {
		t.Fatalf("resumeRefetches moved the retry from %v to %v", due, d.refetchDue)
	}
}

// A sweep's failed apply is recorded too: the next sweep might be a day away.
func TestASweepsFailedApplyIsRecorded(t *testing.T) {
	fs := newFlakyStore()
	fs.content["a.txt"] = []byte("alpha")
	fs.failGets = 1
	st := newState(t)
	d := NewDownloader(nil, fs, t.TempDir(), st, DefaultCadence).Reconcile(ReconcileOptions{Fetch: true})

	var stats sweepStats
	noRepush := map[string]struct{}{}
	files := []provider.RemoteFile{*remoteFile("a.txt", "alpha")}
	if err := d.applyPage(context.Background(), state.Sweep{Gen: "g1"}, files, &stats, noRepush); err != nil {
		t.Fatal(err)
	}
	if !refetchOwed(t, st, "a.txt") {
		t.Fatal("the sweep's failed download was not recorded")
	}
	if _, claimed := noRepush["a.txt"]; !claimed {
		t.Fatal("the failed path was not withheld from the lost-push row")
	}
}
