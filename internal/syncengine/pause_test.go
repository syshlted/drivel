// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package syncengine

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/syshlted/drivel/internal/fsevent"
	"github.com/syshlted/drivel/provider"
)

// M14: pause stops dispatch, never consumption.

// puts counts the Put calls a fakeStore has seen.
func (f *fakeStore) puts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, "Put(") {
			n++
		}
	}
	return n
}

// mkdirs counts the Mkdir calls.
func (f *fakeStore) mkdirs() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, "Mkdir(") {
			n++
		}
	}
	return n
}

// runEngine starts e on a channel buffered as internal/app's is, and returns a
// function that closes it and waits for the drain.
func runEngine(t *testing.T, e *Engine) (chan fsevent.Event, func()) {
	t.Helper()
	ch := make(chan fsevent.Event, 1024)
	done := make(chan struct{})
	go func() { defer close(done); e.Run(context.Background(), ch) }()
	return ch, func() {
		close(ch)
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Fatal("the engine did not drain")
		}
	}
}

// The test the milestone names: a write storm against a paused mount must not
// block the writer. Three thousand events is more than the event buffer and the
// worker queue together (1024 + 64), so a pause that stopped consuming, or that
// let dispatch run into a gated worker, wedges the sending loop below — which
// is vfs.node.emit blocking a FUSE handler, which is `cp` hanging.
func TestPauseHoldsUnderAWriteStormWithoutBlockingTheMount(t *testing.T) {
	const files, dirs = 3000, 2000
	dir := t.TempDir()
	store := newFakeStore()
	mon := NewMonitor("m")
	e := New(Config{Store: store, DataDir: dir, Workers: 1, Debounce: time.Millisecond, Monitor: mon})
	ch, stop := runEngine(t, e)

	ctx := t.Context()
	if _, err := e.Pause(ctx, time.Hour); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	for i := 0; i < files; i++ {
		writeFile(t, dir, fmt.Sprintf("f%04d.txt", i), "x")
	}

	sent := make(chan struct{})
	go func() {
		defer close(sent)
		for i := 0; i < files; i++ {
			ch <- fsevent.Event{Op: fsevent.OpCreate, Path: fmt.Sprintf("f%04d.txt", i)}
			ch <- fsevent.Event{Op: fsevent.OpWrite, Path: fmt.Sprintf("f%04d.txt", i)}
		}
		// Structural ops dispatch at once rather than waiting out the push delay,
		// so these are what reach dispatch during the pause — more of them than
		// the event buffer and a worker queue can hold, so they must be held
		// rather than sent to a worker that is waiting at the gate.
		for i := 0; i < dirs; i++ {
			ch <- fsevent.Event{Op: fsevent.OpMkdir, Path: dirName(i)}
		}
	}()
	select {
	case <-sent:
	case <-time.After(10 * time.Second):
		t.Fatal("the writer blocked while paused: a pause must stop dispatch, never consumption")
	}

	// Give the timers a chance to fire; nothing may go out.
	time.Sleep(50 * time.Millisecond)
	if n, d := store.puts(), store.mkdirs(); n != 0 || d != 0 {
		t.Fatalf("%d upload(s) and %d mkdir(s) went out while paused", n, d)
	}
	waitFor(t, func() bool { return mon.Snapshot().Coalescing == files }, "the coalescer did not hold every path")
	if s := mon.Snapshot(); !s.Pause.Paused {
		t.Fatalf("the snapshot does not report the pause: %+v", s.Pause)
	}

	if err := e.Resume(ctx); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	waitFor(t, func() bool { return store.puts() == files && store.mkdirs() == dirs },
		"the held operations were not sent after resume")
	stop()
	if got := store.puts(); got != files {
		t.Errorf("%d uploads after resume; want %d — each path once, coalesced", got, files)
	}
}

// Past its cap a pause ends on its own and says so; it neither drops what it
// holds nor starts blocking the mount.
func TestAPauseOverItsCapResumesAndSaysSo(t *testing.T) {
	dir := t.TempDir()
	store := newFakeStore()
	logs := &capture{}
	mon := NewMonitor("m")
	e := New(Config{Store: store, DataDir: dir, Debounce: time.Millisecond, Monitor: mon,
		PauseCap: 50, Logger: log.New(logs, "", 0)})
	ch, stop := runEngine(t, e)
	defer stop()

	if _, err := e.Pause(t.Context(), time.Hour); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		p := fmt.Sprintf("f%03d.txt", i)
		writeFile(t, dir, p, "x")
		ch <- fsevent.Event{Op: fsevent.OpWrite, Path: p}
	}
	waitFor(t, func() bool { return store.puts() == 100 }, "an overflowing pause did not resume and send everything")
	if logs.count("RESUMED") != 1 || logs.count("past its cap of 50") != 1 {
		t.Errorf("the overflow was not reported loudly, once:\n%s", strings.Join(logs.lines(), "\n"))
	}
	if r := mon.Snapshot().Pause; r.Paused || !strings.Contains(r.LastAutoResume, "cap") {
		t.Errorf("the snapshot does not say why the pause ended: %+v", r)
	}
}

// A pause expires: the two real uses (a metered link, a reorganisation) must
// heal themselves when the user forgets.
func TestAPauseExpires(t *testing.T) {
	dir := t.TempDir()
	store := newFakeStore()
	logs := &capture{}
	e := New(Config{Store: store, DataDir: dir, Debounce: time.Millisecond, Logger: log.New(logs, "", 0)})
	ch, stop := runEngine(t, e)
	defer stop()

	if _, err := e.Pause(t.Context(), 100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "a.txt", "x")
	ch <- fsevent.Event{Op: fsevent.OpWrite, Path: "a.txt"}
	waitFor(t, func() bool { return store.puts() == 1 }, "an expired pause never resumed")
	if logs.count("pause expired") != 1 {
		t.Errorf("the expiry was not logged:\n%s", strings.Join(logs.lines(), "\n"))
	}
}

// Shutting down while paused drains what was held: a pause is not persisted,
// and discarding the held set is the one option the design rules out.
func TestShutdownWhilePausedDrainsWhatWasHeld(t *testing.T) {
	dir := t.TempDir()
	store := newFakeStore()
	e := New(Config{Store: store, DataDir: dir, Debounce: time.Hour})
	ch, stop := runEngine(t, e)
	if _, err := e.Pause(t.Context(), time.Hour); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "a.txt", "x")
	ch <- fsevent.Event{Op: fsevent.OpWrite, Path: "a.txt"}
	ch <- fsevent.Event{Op: fsevent.OpMkdir, Path: "d"}
	stop()
	assertCalls(t, store.calls, []string{"Mkdir(d)", "Put(create,a.txt,bytes=1)"})
}

// Held operations keep per-path order: content flushed by a structural op goes
// out before it, exactly as it would unpaused.
func TestAPauseKeepsPerPathOrder(t *testing.T) {
	dir := t.TempDir()
	store := newFakeStore()
	e := New(Config{Store: store, DataDir: dir, Debounce: time.Hour})
	ch, stop := runEngine(t, e)
	if _, err := e.Pause(t.Context(), time.Hour); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "b.txt", "x")
	ch <- fsevent.Event{Op: fsevent.OpWrite, Path: "a.txt"}
	ch <- fsevent.Event{Op: fsevent.OpRename, Path: "a.txt", NewPath: "b.txt"}
	if err := e.Resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	stop()
	// a.txt no longer exists locally, so its content push is a no-op; the rename
	// then finds no remote source and falls back to a whole-file push of b.txt.
	// What matters is that the rename did not overtake the write.
	assertCalls(t, store.calls, []string{"Put(create,b.txt,bytes=1)"})
}

// A pause asked for while dispatch is blocked on a full worker queue is
// answered from inside the blocked send, so pausing a saturated mount does not
// wait for the next upload to finish.
func TestAPauseIsAnsweredWhileDispatchIsBlocked(t *testing.T) {
	dir := t.TempDir()
	store := newBlockingStore()
	mon := NewMonitor("m")
	e := New(Config{Store: store, DataDir: dir, Workers: 1, Monitor: mon})
	ch, stop := runEngine(t, e)
	sent := make(chan struct{})
	defer func() { store.open(); <-sent; stop() }()

	go func() {
		defer close(sent)
		for i := 0; i < 400; i++ {
			ch <- fsevent.Event{Op: fsevent.OpMkdir, Path: dirName(i)}
		}
	}()
	waitFor(t, func() bool { return mon.Snapshot().Saturated }, "the outbound path never saturated")

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if _, err := e.Pause(ctx, time.Hour); err != nil {
		t.Fatalf("Pause on a saturated engine: %v", err)
	}
}

// The test the milestone names: a status read is served while the run loop is
// blocked on a full worker queue — the moment someone most wants to ask.
func TestStatusIsServedWhileTheRunLoopIsBlocked(t *testing.T) {
	dir := t.TempDir()
	store := newBlockingStore()
	mon := NewMonitor("m")
	e := New(Config{Store: store, DataDir: dir, Workers: 1, Monitor: mon})
	ch, stop := runEngine(t, e)
	sent := make(chan struct{})
	defer func() { store.open(); <-sent; stop() }()

	go func() {
		defer close(sent)
		for i := 0; i < 400; i++ {
			ch <- fsevent.Event{Op: fsevent.OpMkdir, Path: dirName(i)}
		}
	}()
	waitFor(t, func() bool { return mon.Snapshot().Saturated }, "the outbound path never saturated")

	r := StatusReader{Monitor: mon, State: newState(t), DataDir: dir}
	answered := make(chan PathStatus, 1)
	go func() {
		ps, err := r.Path(dirName(1)) // in a worker queue: d0 is in flight
		if err != nil {
			t.Errorf("Path: %v", err)
		}
		answered <- ps
	}()
	select {
	case ps := <-answered:
		if ps.Status != StatusPending {
			t.Errorf("a path still waiting to be dispatched reads %q; want pending", ps.Status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a status read waited on the blocked run loop")
	}
	if s := mon.Snapshot(); s.Queued == 0 || s.InFlight != 1 {
		t.Errorf("the snapshot under saturation reads queued=%d in_flight=%d", s.Queued, s.InFlight)
	}
}

// A mount with nothing to send has nothing to pause, and an engine that is not
// running says so instead of hanging the caller.
func TestPauseRefusesWhatItCannotDo(t *testing.T) {
	if _, err := New(Config{DataDir: t.TempDir()}).Pause(t.Context(), time.Minute); !errors.Is(err, ErrLogOnly) {
		t.Errorf("pausing a log-only engine: %v; want ErrLogOnly", err)
	}
	e := New(Config{Store: newFakeStore(), DataDir: t.TempDir()})
	_, stop := runEngine(t, e)
	stop()
	if _, err := e.Pause(t.Context(), time.Minute); !errors.Is(err, ErrNotRunning) {
		t.Errorf("pausing a stopped engine: %v; want ErrNotRunning", err)
	}
	if _, err := New(Config{Store: newFakeStore()}).Pause(t.Context(), 48*time.Hour); err == nil {
		t.Error("a pause past MaxPauseFor was accepted")
	}
}

// holder is a Holder a test opens and shuts by hand.
type holder struct{ g gate }

func (h *holder) WaitUnpaused(ctx context.Context) bool { return h.g.wait(ctx) }

// The pull loop waits with the push side: a pause promises that nothing is
// fetched either.
func TestAPauseHoldsThePullLoop(t *testing.T) {
	dir := t.TempDir()
	st := newState(t)
	store := newFakeStore()
	store.content["r.txt"] = []byte("remote")
	src := &fakeSource{
		start:   "c0",
		batches: [][]provider.RemoteChange{{{Path: "r.txt", File: remoteFile("r.txt", "remote")}}},
		cursors: []string{"c1"},
	}
	if err := st.SetCursor("c0"); err != nil {
		t.Fatal(err)
	}
	h := &holder{}
	h.g.shut()
	d := NewDownloader(src, store, dir, st, Cadence{Fast: time.Millisecond, Slow: 2 * time.Millisecond}).PausedBy(h)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); d.Run(ctx) }()

	time.Sleep(50 * time.Millisecond)
	if _, ok := readBacking(t, dir, "r.txt"); ok {
		t.Fatal("the pull loop fetched while paused")
	}
	h.g.release()
	waitFor(t, func() bool { _, ok := readBacking(t, dir, "r.txt"); return ok }, "the pull loop did not resume")
	cancel()
	<-done
}
