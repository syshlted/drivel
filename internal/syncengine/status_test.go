// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package syncengine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/syshlted/drivel/internal/fsevent"
	"github.com/syshlted/drivel/internal/state"
)

// M14: every status comes from local state, and anything that cannot be vouched
// for is unknown with a reason — never synced by default.

type statusFixture struct {
	dir string
	st  *state.Store
	mon *Monitor
	r   StatusReader
}

func newStatusFixture(t *testing.T) *statusFixture {
	t.Helper()
	f := &statusFixture{dir: t.TempDir(), st: newState(t), mon: NewMonitor("m")}
	f.r = StatusReader{Monitor: f.mon, State: f.st, DataDir: f.dir}
	return f
}

// synced writes rel and records a baseline that fingerprints it, as a push would.
func (f *statusFixture) synced(t *testing.T, rel, content string) {
	t.Helper()
	writeFile(t, f.dir, rel, content)
	fi, err := os.Stat(filepath.Join(f.dir, rel))
	if err != nil {
		t.Fatal(err)
	}
	size, mtime := state.FingerprintOf(fi)
	if err := f.st.SetEcho(rel, state.Echo{Hash: md5hex(content), Version: "1", At: time.Now(), LocalSize: size, LocalMTime: mtime}); err != nil {
		t.Fatal(err)
	}
}

func (f *statusFixture) status(t *testing.T, rel string) PathStatus {
	t.Helper()
	ps, err := f.r.Path(rel)
	if err != nil {
		t.Fatalf("Path(%q): %v", rel, err)
	}
	return ps
}

func TestStatusOfASyncedFile(t *testing.T) {
	f := newStatusFixture(t)
	f.synced(t, "a.txt", "hello")
	if ps := f.status(t, "a.txt"); ps.Status != StatusSynced || ps.Kind != "file" || ps.Size != 5 || ps.SyncedAt.IsZero() {
		t.Errorf("a synced file reads %+v", ps)
	}
}

// The status a status API is most tempted to lie about.
func TestAPathWithNoRecordIsUnknownNotSynced(t *testing.T) {
	f := newStatusFixture(t)
	writeFile(t, f.dir, "new.txt", "x")
	ps := f.status(t, "new.txt")
	if ps.Status != StatusUnknown || ps.Detail == "" {
		t.Errorf("a never-synced file reads %+v; want unknown, with a reason", ps)
	}
}

// A local copy that no longer matches its baseline, with nothing queued for it,
// was changed outside the mount (or lost to a crash). Synced would be a lie.
func TestADivergedFileWithNothingQueuedIsUnknown(t *testing.T) {
	f := newStatusFixture(t)
	f.synced(t, "a.txt", "hello")
	writeFile(t, f.dir, "a.txt", "hello, changed")
	if ps := f.status(t, "a.txt"); ps.Status != StatusUnknown || !strings.Contains(ps.Detail, "differs") {
		t.Errorf("a diverged file reads %+v", ps)
	}
}

// A baseline with no fingerprint cannot vouch for the local bytes without a
// hash, and a status read never hashes.
func TestAnUnfingerprintedBaselineIsUnknown(t *testing.T) {
	f := newStatusFixture(t)
	writeFile(t, f.dir, "old.txt", "x")
	if err := f.st.SetEcho("old.txt", state.Echo{Hash: md5hex("x"), At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if ps := f.status(t, "old.txt"); ps.Status != StatusUnknown {
		t.Errorf("an unfingerprinted baseline reads %+v", ps)
	}
}

func TestWhatTheOutboundPathHoldsOutranksTheRecord(t *testing.T) {
	f := newStatusFixture(t)
	f.synced(t, "a.txt", "hello")
	ev := fsevent.Event{Op: fsevent.OpWrite, Path: "a.txt"}

	f.mon.coalescing("a.txt", true)
	if s := f.status(t, "a.txt").Status; s != StatusPending {
		t.Errorf("debouncing reads %q", s)
	}
	f.mon.coalescing("a.txt", false)
	f.mon.enqueued(ev)
	f.mon.began(ev)
	// A second write lands while the first upload runs: the path must not read
	// synced the moment the first finishes.
	f.mon.coalescing("a.txt", true)
	if s := f.status(t, "a.txt").Status; s != StatusUploading {
		t.Errorf("uploading reads %q", s)
	}
	f.mon.finished(ev, nil, false)
	if s := f.status(t, "a.txt").Status; s != StatusPending {
		t.Errorf("after the first upload, with a second write pending, reads %q; want pending", s)
	}
	f.mon.coalescing("a.txt", false)
	f.mon.enqueued(ev)
	f.mon.began(ev)
	f.mon.retrying(ev, 1, time.Now().Add(time.Second), errors.New("503"))
	if ps := f.status(t, "a.txt"); ps.Status != StatusRetrying || ps.Attempt != 2 || ps.Error != "503" {
		t.Errorf("retrying reads %+v", ps)
	}
	f.mon.attemptAgain(ev)
	f.mon.finished(ev, errors.New("permission denied"), false)
	if ps := f.status(t, "a.txt"); ps.Status != StatusError || ps.Error != "permission denied" {
		t.Errorf("abandoned reads %+v", ps)
	}
	f.mon.enqueued(ev)
	f.mon.began(ev)
	f.mon.finished(ev, nil, false)
	if s := f.status(t, "a.txt").Status; s != StatusSynced {
		t.Errorf("a later success does not clear the error: %q", s)
	}
	if n := len(f.mon.paths); n != 0 {
		t.Errorf("the monitor kept %d entr(ies) for a path with nothing left to say", n)
	}
}

// An abandonment recorded by an earlier process is still an error.
func TestARepairRecordReadsAsError(t *testing.T) {
	f := newStatusFixture(t)
	f.synced(t, "a.txt", "hello")
	if err := f.st.OweRepair("a.txt", time.Now()); err != nil {
		t.Fatal(err)
	}
	if s := f.status(t, "a.txt").Status; s != StatusError {
		t.Errorf("a recorded repair reads %q", s)
	}
}

type fakeHoles map[string]bool

func (h fakeHoles) IsPlaceholder(rel string) bool { return h[rel] }

func TestPlaceholdersConflictsAndLocalOnly(t *testing.T) {
	f := newStatusFixture(t)
	f.r.Holes = fakeHoles{"lazy.bin": true}
	f.synced(t, "lazy.bin", "")
	f.synced(t, "doc.txt", "mine")
	copyName := conflictName("doc.txt", time.Date(2026, 10, 1, 9, 30, 0, 0, time.UTC))
	writeFile(t, f.dir, copyName, "theirs")
	writeFile(t, f.dir, ".drivel-tmp123", "")
	mkfifo(t, filepath.Join(f.dir, "pipe"))

	if s := f.status(t, "lazy.bin").Status; s != StatusPlaceholder {
		t.Errorf("a placeholder reads %q", s)
	}
	if ps := f.status(t, "doc.txt"); ps.Status != StatusConflict || len(ps.Conflicts) != 1 || ps.Conflicts[0] != copyName {
		t.Errorf("a file with a conflict copy beside it reads %+v", ps)
	}
	for _, p := range []string{copyName, ".drivel-tmp123", "pipe"} {
		if s := f.status(t, p).Status; s != StatusLocalOnly {
			t.Errorf("%s reads %q; want local-only", p, s)
		}
	}

	// The listing reaches the same answers from the one directory read.
	es, truncated, err := f.r.Children("", "", 0)
	if err != nil || truncated {
		t.Fatalf("Children: %v (truncated %v)", err, truncated)
	}
	got := map[string]string{}
	for _, e := range es {
		got[e.Path] = e.Status
	}
	want := map[string]string{"lazy.bin": StatusPlaceholder, "doc.txt": StatusConflict, copyName: StatusLocalOnly,
		".drivel-tmp123": StatusLocalOnly, "pipe": StatusLocalOnly}
	for p, s := range want {
		if got[p] != s {
			t.Errorf("listing: %s reads %q; want %q", p, got[p], s)
		}
	}
}

func TestChildrenPagesAndTruncates(t *testing.T) {
	f := newStatusFixture(t)
	for i := 0; i < 25; i++ {
		f.synced(t, fmt.Sprintf("d/f%02d", i), "x")
	}
	var all []string
	after := ""
	for pages := 0; ; pages++ {
		es, truncated, err := f.r.Children("d", after, 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range es {
			all = append(all, e.Path)
			if e.Status != StatusSynced {
				t.Errorf("%s reads %q", e.Path, e.Status)
			}
		}
		if !truncated {
			break
		}
		if pages > 5 {
			t.Fatal("paging does not terminate")
		}
		after = filepath.Base(es[len(es)-1].Path)
	}
	if len(all) != 25 || all[0] != "d/f00" || all[24] != "d/f24" {
		t.Errorf("paged listing: %d entries, %v", len(all), all)
	}
}

func TestStatusRefusesPathsOutsideTheMount(t *testing.T) {
	f := newStatusFixture(t)
	for _, p := range []string{"../etc", "a/../../b", "a//b", "a/./b"} {
		if _, err := f.r.Path(p); !errors.Is(err, ErrBadPath) {
			t.Errorf("Path(%q): %v; want ErrBadPath", p, err)
		}
	}
	if _, err := f.r.Path("nope"); !errors.Is(err, ErrNoSuchPath) {
		t.Errorf("an absent path: %v; want ErrNoSuchPath", err)
	}
	if ps := f.status(t, "/"); ps.Kind != "dir" {
		t.Errorf("the root reads %+v", ps)
	}
}

func TestALogOnlyMountVouchesForNothing(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "x")
	r := StatusReader{Monitor: NewMonitor("m"), DataDir: dir}
	if ps, err := r.Path("a.txt"); err != nil || ps.Status != StatusUnknown {
		t.Errorf("a log-only mount reads %+v, %v", ps, err)
	}
}

// --- the subscription's producer rules ---------------------------------------

func TestTransitionsAreCoalescedPerPath(t *testing.T) {
	mon := NewMonitor("m")
	sub := NewSubscription(0)
	mon.Subscribe(sub)
	ev := fsevent.Event{Op: fsevent.OpWrite, Path: "a.txt"}
	mon.coalescing("a.txt", true)
	mon.coalescing("a.txt", false)
	mon.enqueued(ev)
	mon.began(ev)
	mon.finished(ev, nil, false)
	mon.applied("b.txt", StatusSynced)

	select {
	case <-sub.Ready():
	default:
		t.Fatal("no signal")
	}
	ts, _, dropped := sub.Drain()
	if dropped || len(ts) != 2 || ts[0].Path != "a.txt" || ts[0].Status != StatusSynced || ts[1].Path != "b.txt" {
		t.Errorf("drained %+v (dropped %v); want one settled transition per path, in first-arrival order", ts, dropped)
	}
}

// A subscriber that falls behind is dropped, never waited for.
func TestASlowSubscriberIsDroppedNotWaitedFor(t *testing.T) {
	mon := NewMonitor("m")
	sub := NewSubscription(10)
	mon.Subscribe(sub)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 10000; i++ {
			mon.applied(fmt.Sprintf("f%d", i), StatusSynced)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("publishing blocked on a subscriber nobody was reading")
	}
	if _, _, dropped := sub.Drain(); !dropped {
		t.Error("a subscriber 10000 paths behind was not dropped")
	}
}
