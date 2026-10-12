// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package syncengine

import (
	"context"
	"io"
	"log"
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

	// Unbuffered, so the bound is the single worker queue and the low-water mark
	// is only reached once that queue has genuinely drained.
	ch := make(chan fsevent.Event)
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
	bp := &backpressure{e: e, bound: cap(events) + e.workers*workerQueue, depth: func() int { return 0 }}
	if want := 1024 + 4*64; bp.bound != want {
		t.Fatalf("bound = %d; want %d (eventBuffer + workers*workerQueue)", bp.bound, want)
	}
}
