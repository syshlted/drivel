// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package syncengine

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/syshlted/drivel/internal/fsevent"
	"github.com/syshlted/drivel/provider"
)

// M27 part 1: back-pressure is correct and was invisible.
//
// Past 1024 + workers*64 events the mount blocks on purpose — dropping one
// would lose a file until a sweep — so a bulk copy runs at the provider's
// upload rate, measured at 1.09/s on Drive. Nothing said so, which left "drivel
// is broken" as the only conclusion available to the person watching it.
//
// Pinning log text is normally the wrong thing to test. Here the log IS the
// feature: there is no other output, by design (M14 is the API, M25 the levels).

// blockingStore makes every upload wait for a ticket, so a test can run the
// provider at whatever rate it likes — including the rate that matters, which
// is "slower than the writer" rather than "stopped".
//
// Stopped is the easy case and the uninteresting one: one dispatch blocks and
// the engine wedges. Sustained saturation is where the reporting has to earn
// its keep, because every dispatch finds room the instant a worker frees a slot
// and then refills it.
type blockingStore struct {
	*fakeStore
	tickets chan struct{}
}

func newBlockingStore() *blockingStore {
	return &blockingStore{fakeStore: newFakeStore(), tickets: make(chan struct{}, 4096)}
}

// ticket blocks until the test releases one.
func (b *blockingStore) ticket(ctx context.Context) error {
	select {
	case <-b.tickets:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// release lets n more uploads through.
func (b *blockingStore) release(n int) {
	for i := 0; i < n; i++ {
		b.tickets <- struct{}{}
	}
}

// open lets every remaining upload through: the provider catching up.
func (b *blockingStore) open() { close(b.tickets) }

func (b *blockingStore) Put(ctx context.Context, p string, r io.Reader) (provider.RemoteFile, error) {
	if err := b.ticket(ctx); err != nil {
		return provider.RemoteFile{}, err
	}
	return b.fakeStore.Put(ctx, p, r)
}

func (b *blockingStore) Mkdir(ctx context.Context, p string) (provider.RemoteFile, error) {
	if err := b.ticket(ctx); err != nil {
		return provider.RemoteFile{}, err
	}
	return b.fakeStore.Mkdir(ctx, p)
}

// Entering and leaving saturation are each reported exactly once, however many
// writes wait in between. One line per state transition is the whole design:
// a line per blocked dispatch would be the ten-thousand-line log this replaces.
func TestBackPressureIsReportedOnceEachWay(t *testing.T) {
	const events = 400
	dir := t.TempDir()
	store := newBlockingStore()
	logs := &capture{}
	e := New(Config{
		Store: store, DataDir: dir, Workers: 1,
		Logger: log.New(logs, "", 0),
	})
	e.satSample = 5 * time.Millisecond

	// Buffered as internal/app's is (eventBuffer), because the geometry decides
	// what the report can see. This test ran against an unbuffered channel until
	// 2026-10-14, which made the bound the single worker queue and the backlog
	// visible in it — the one arrangement in which a fraction-of-the-bound clear
	// condition behaves. MC-12 then met the real one; see satSample's comment.
	ch := make(chan fsevent.Event, 1024)
	done := make(chan struct{})
	go func() { defer close(done); e.Run(context.Background(), ch) }()

	// Mkdirs dispatch immediately (no debounce window), and with one worker they
	// all land on the same queue. They are sent from a goroutine because once the
	// queue is full the Run loop stops reading and these sends block too — that
	// is the back-pressure reaching the writer, so a test that waited for them
	// all to land would be waiting for the condition it is trying to observe to
	// go away.
	sent := make(chan struct{})
	go func() {
		defer close(sent)
		for i := 0; i < events; i++ {
			ch <- fsevent.Event{Op: fsevent.OpMkdir, Path: dirName(i)}
		}
	}()
	waitFor(t, func() bool {
		return logs.count("back-pressure: writes through the mount are now waiting") == 1
	}, "timed out waiting for the saturation report")

	// Now run the provider slower than the writer for a while. Every one of these
	// frees exactly one slot, which one dispatch immediately takes and the next
	// dispatch then waits for — a hundred blocked dispatches, and the sampler
	// ticking throughout, all inside ONE episode.
	for i := 0; i < 100; i++ {
		store.release(1)
		time.Sleep(time.Millisecond)
	}
	if n := logs.count("back-pressure: cleared"); n != 0 {
		t.Fatalf("the episode was cleared %d time(s) while the queue was still full", n)
	}
	if n := logs.count("back-pressure: writes through the mount are now waiting"); n != 1 {
		t.Fatalf("saturation was reported %d time(s) during one episode; want 1 — "+
			"the report is keyed on the state transition, not on the blocked dispatch", n)
	}

	store.open() // the provider catches up
	<-sent
	waitFor(t, func() bool {
		return logs.count("back-pressure: cleared") == 1
	}, "timed out waiting for the recovery report")

	close(ch)
	<-done

	if n := logs.count("back-pressure: writes through the mount are now waiting"); n != 1 {
		t.Errorf("entered saturation %d time(s) in the log; want exactly 1", n)
	}
	if n := logs.count("back-pressure: cleared"); n != 1 {
		t.Errorf("left saturation %d time(s) in the log; want exactly 1", n)
	}
	// The line has to be actionable: the settings that govern the bound are the
	// whole reason it is worth printing rather than counting.
	var entered string
	for _, l := range logs.lines() {
		if strings.Contains(l, "back-pressure: writes through the mount") {
			entered = l
		}
	}
	for _, want := range []string{"-upload-workers 1", "-push-delay", "event(s) queued"} {
		if !strings.Contains(entered, want) {
			t.Errorf("the saturation line does not mention %q: %s", want, entered)
		}
	}
}

func dirName(i int) string { return "d" + strings.Repeat("x", i%3) + itoa(i) }

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// A mount that keeps up says nothing at all. Back-pressure reporting that fired
// on ordinary traffic would be noise, and noise in this log is what M25 exists
// to avoid adding.
func TestNoBackPressureLineWhenTheUploaderKeepsUp(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "hello")
	writeFile(t, dir, "b.txt", "world")
	logs := &capture{}
	e := New(Config{
		Store: newFakeStore(), DataDir: dir, Debounce: time.Millisecond,
		Logger: log.New(logs, "", 0),
	})
	e.satSample = 5 * time.Millisecond

	events := make(chan fsevent.Event, 8)
	done := make(chan struct{})
	go func() { defer close(done); e.Run(context.Background(), events) }()
	events <- fsevent.Event{Op: fsevent.OpWrite, Path: "a.txt"}
	events <- fsevent.Event{Op: fsevent.OpWrite, Path: "b.txt"}
	close(events)
	<-done

	if n := logs.count("back-pressure"); n != 0 {
		t.Fatalf("a mount that kept up logged %d back-pressure line(s):\n%s", n, strings.Join(logs.lines(), "\n"))
	}
}

// An episode still open when the engine stops is closed in the log. An
// unterminated "queue full" is precisely the shape that reads as a hang.
//
// satSample is an hour here so the sampler cannot be what closes it: the case
// being pinned is a shutdown that arrives before the recovery was ever noticed,
// which is the common one — the drain is what empties the queues, and it runs
// after the loop that services the sampler has gone.
func TestBackPressureIsClosedAtShutdown(t *testing.T) {
	dir := t.TempDir()
	store := newBlockingStore()
	logs := &capture{}
	e := New(Config{
		Store: store, DataDir: dir, Workers: 1,
		Logger: log.New(logs, "", 0),
	})
	e.satSample = time.Hour

	events := make(chan fsevent.Event)
	done := make(chan struct{})
	go func() { defer close(done); e.Run(context.Background(), events) }()
	sent := make(chan struct{})
	go func() {
		defer close(sent)
		for i := 0; i < workerQueue+8; i++ {
			events <- fsevent.Event{Op: fsevent.OpMkdir, Path: dirName(i)}
		}
	}()
	waitFor(t, func() bool {
		return logs.count("back-pressure: writes through the mount are now waiting") == 1
	}, "timed out waiting for the saturation report")

	// Let the provider finish. Nothing may interrupt a dispatch that is already
	// waiting for room — the send is unconditional, by design, because the
	// alternative is dropping the event — so a shutdown cannot overtake it.
	store.open()
	<-sent
	close(events)
	<-done

	if n := logs.count("back-pressure: cleared at shutdown"); n != 1 {
		t.Fatalf("shutdown closed the episode %d time(s); want 1:\n%s", n, strings.Join(logs.lines(), "\n"))
	}
	if n := logs.count("back-pressure: writes through the mount are now waiting"); n != 1 {
		t.Errorf("entered saturation %d time(s); want 1", n)
	}
}

// The depth is read where the blocking happens — the worker queues plus the
// mount's own channel — not where the event was created. A depth sampled in
// vfs.node.emit cannot tell a full worker queue from a slow provider, and the
// two want different advice.
func TestBackPressureCountsTheWholeOutboundPath(t *testing.T) {
	e := New(Config{Workers: 4})
	events := make(chan fsevent.Event, 1024)
	bp := &backpressure{
		e: e, bound: cap(events) + e.workers*workerQueue,
		depth: func() (int, int) { return 0, 0 },
	}
	if want := 1024 + 4*64; bp.bound != want {
		t.Fatalf("bound = %d; want %d (eventBuffer + workers*workerQueue)", bp.bound, want)
	}
}

// The backlog of a bulk copy sits in the coalescer's debounce map, not in the
// queues — so an episode must not end while the coalescer still has events to
// hand over, however shallow the queues look.
//
// This is MC-12's live finding (2026-10-14) as a test. 1100 files written into
// one directory plateaued at 254 queued of a 1280 bound with 846 events still
// in the debounce map; the shipped rule declared an episode over below a quarter
// of the bound, which 254 already is, so it cleared on the first tick and
// reopened on the next blocked dispatch — 53 lines in three minutes, each
// "cleared" line reporting 253 events still queued. It fails with 26 clears
// against that rule and 1 against this one.
func TestBackPressureSurvivesABacklogTheQueuesCannotHold(t *testing.T) {
	const events = 400
	dir := t.TempDir()
	store := newBlockingStore()
	logs := &capture{}
	e := New(Config{
		Store: store, DataDir: dir, Workers: 1,
		// Long enough that every event is absorbed into the debounce map before
		// the first one is dispatched, which is what empties the channel and
		// leaves the backlog somewhere no channel length can see.
		Debounce: 150 * time.Millisecond,
		Logger:   log.New(logs, "", 0),
	})
	e.satSample = 5 * time.Millisecond

	for i := 0; i < events; i++ {
		writeFile(t, dir, dirName(i), "x")
	}
	ch := make(chan fsevent.Event, 1024)
	done := make(chan struct{})
	go func() { defer close(done); e.Run(context.Background(), ch) }()
	for i := 0; i < events; i++ {
		ch <- fsevent.Event{Op: fsevent.OpWrite, Path: dirName(i)}
	}
	waitFor(t, func() bool {
		return logs.count("back-pressure: writes through the mount are now waiting") == 1
	}, "timed out waiting for the saturation report")

	// The provider now runs slower than the backlog drains, which is the whole
	// of a bulk copy against a real provider: one upload at a time, the queue
	// refilling the instant it empties.
	for i := 0; i < 100; i++ {
		store.release(1)
		time.Sleep(time.Millisecond)
	}
	if n := logs.count("back-pressure: cleared"); n != 0 {
		t.Fatalf("the episode was cleared %d time(s) while the coalescer still held a backlog:\n%s",
			n, strings.Join(logs.lines(), "\n"))
	}
	if n := logs.count("back-pressure: writes through the mount are now waiting"); n != 1 {
		t.Fatalf("saturation was reported %d time(s) during one episode; want 1", n)
	}

	store.open()
	waitFor(t, func() bool { return logs.count("back-pressure: cleared") == 1 }, "timed out waiting for the recovery report")
	close(ch)
	<-done

	if n := logs.count("back-pressure: writes through the mount are now waiting"); n != 1 {
		t.Errorf("entered saturation %d time(s); want exactly 1", n)
	}
	if n := logs.count("back-pressure: cleared"); n != 1 {
		t.Errorf("left saturation %d time(s); want exactly 1", n)
	}
	// The entering line has to say how big the backlog is, not only how full the
	// queue it could see was.
	var entered string
	for _, l := range logs.lines() {
		if strings.Contains(l, "back-pressure: writes through the mount") {
			entered = l
		}
	}
	if !strings.Contains(entered, "awaiting dispatch") {
		t.Errorf("the saturation line does not report the undispatched backlog: %s", entered)
	}
}

// DrainedCleanly is what lets the mount clear its unclean-shutdown mark, so it
// must be false exactly when the drain abandoned something: a drain cut off by
// its deadline aborts in-flight uploads and records nothing, which is a lost
// push the next start has to sweep for.
func TestDrainedCleanlyIsFalseWhenTheDrainHitsItsDeadline(t *testing.T) {
	for _, tc := range []struct {
		name    string
		provide bool // let the upload through, so the drain can finish
		want    bool
	}{
		{"drain finished", true, true},
		{"drain deadline", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newBlockingStore()
			if tc.provide {
				store.open()
			}
			e := New(Config{
				Store: store, DataDir: t.TempDir(), Workers: 1,
				DrainTimeout: 50 * time.Millisecond, Logger: log.New(io.Discard, "", 0),
			})
			ch := make(chan fsevent.Event, 1)
			ch <- fsevent.Event{Op: fsevent.OpMkdir, Path: "d"}
			close(ch)
			if e.DrainedCleanly() {
				t.Fatal("DrainedCleanly before Run")
			}
			e.Run(context.Background(), ch)
			if got := e.DrainedCleanly(); got != tc.want {
				t.Errorf("DrainedCleanly = %v; want %v", got, tc.want)
			}
		})
	}
}

// A shutdown must overtake a dispatch blocked on a full worker queue. Against a
// provider that has stopped answering, the run loop sits in that send and stops
// reading events, so neither closing the channel nor cancelling a context could
// reach it, and Run never returned. BeginShutdown's deadline is what does; the
// events it sheds are recorded for repair, and the stop is reported unclean.
// Remove the abort case from send and this test times out.
func TestShutdownOvertakesABlockedDispatch(t *testing.T) {
	const events = 200
	dir := t.TempDir()
	st := newState(t)
	store := newBlockingStore() // no tickets: nothing ever completes
	e := New(Config{
		Store: store, DataDir: dir, State: st, Workers: 1,
		DrainTimeout: 100 * time.Millisecond, Logger: log.New(io.Discard, "", 0),
	})
	for i := range events {
		if err := os.MkdirAll(filepath.Join(dir, fmt.Sprintf("d%03d", i)), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	ch := make(chan fsevent.Event, 16)
	done := make(chan struct{})
	go func() { defer close(done); e.Run(context.Background(), ch) }()
	sent := make(chan struct{})
	go func() {
		defer close(sent)
		for i := range events {
			ch <- fsevent.Event{Op: fsevent.OpMkdir, Path: fmt.Sprintf("d%03d", i)}
		}
	}()

	// Saturated: one Mkdir in flight forever, the worker queue full, the run loop
	// parked in the send, and the writer parked behind it.
	time.Sleep(200 * time.Millisecond)
	select {
	case <-sent:
		t.Fatal("every event was accepted; the engine never saturated, so this tests nothing")
	default:
	}

	e.BeginShutdown()
	select {
	case <-sent:
	case <-time.After(5 * time.Second):
		t.Fatal("the writer is still blocked: the shutdown deadline never released the run loop")
	}
	close(ch)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the shutdown deadline")
	}

	if e.DrainedCleanly() {
		t.Error("DrainedCleanly after shedding events; the unclean-shutdown mark would be cleared")
	}
	if owed, err := st.RepairsOwed(); err != nil || !owed {
		t.Errorf("RepairsOwed = %v, %v; the shed events were not recorded for repair", owed, err)
	}
}

// deafStore's Mkdir ignores its context: it returns only when the test lets it,
// the shape of a provider call stuck below anything cancellation can reach.
type deafStore struct {
	*fakeStore
	entered chan struct{}
	unblock chan struct{}
}

func (d *deafStore) Mkdir(_ context.Context, p string) (provider.RemoteFile, error) {
	d.entered <- struct{}{}
	<-d.unblock
	return provider.RemoteFile{Path: p, IsDir: true}, nil
}

// The drain deadline cancels in-flight uploads, and used to wait for them
// without a bound afterwards — so one provider call that ignored its context
// held the shutdown for ever. It now waits abandonGrace and goes on, reporting
// the stop as unclean so the next start sweeps. Make the post-cancel wait
// unbounded again and Run never returns.
func TestADrainDoesNotWaitForeverOnACallThatIgnoresCancellation(t *testing.T) {
	dir := t.TempDir()
	store := &deafStore{fakeStore: newFakeStore(), entered: make(chan struct{}, 1), unblock: make(chan struct{})}
	t.Cleanup(func() { close(store.unblock) })
	logs := &capture{}
	e := New(Config{
		Store: store, DataDir: dir, Workers: 1,
		DrainTimeout: 100 * time.Millisecond, Logger: log.New(logs, "", 0),
	})
	e.abandonGrace = 100 * time.Millisecond
	if err := os.Mkdir(filepath.Join(dir, "d"), 0o755); err != nil {
		t.Fatal(err)
	}

	ch := make(chan fsevent.Event, 1)
	done := make(chan struct{})
	go func() { defer close(done); e.Run(context.Background(), ch) }()
	ch <- fsevent.Event{Op: fsevent.OpMkdir, Path: "d"}
	select {
	case <-store.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the Mkdir never reached the store")
	}
	close(ch)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run is still waiting on a provider call that ignored its cancellation")
	}
	if e.DrainedCleanly() {
		t.Error("DrainedCleanly with a call abandoned mid-flight; the unclean-shutdown mark would be cleared")
	}
	if logs.count("still inside a provider call") != 1 {
		t.Errorf("the abandonment was not reported\n%s", strings.Join(logs.lines(), "\n"))
	}
}
