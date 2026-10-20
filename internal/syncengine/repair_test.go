// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package syncengine

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/syshlted/drivel/internal/fsevent"
	"github.com/syshlted/drivel/internal/state"
	"github.com/syshlted/drivel/provider"
)

// M27 part 2: an abandoned push is forgotten, not merely abandoned.
//
// Giving up is right — retrying a permission error forever is a spin. What was
// wrong is that nothing marked the path, so the only thing that could ever put
// the file right was the next sweep, 24 hours away on Drive and disabled
// outright at -sweep-interval 0.

// owed reads the repair list back as a sorted slice.
func owed(t *testing.T, st *state.Store) []string {
	t.Helper()
	var got []string
	if err := st.EachRepair(func(p string, _ time.Time) error {
		got = append(got, p)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	return got
}

// Both ways of giving up record the path: the exhausted-retries branch and the
// non-retryable one, which drops the event after a single attempt.
func TestAbandonedPushIsRecordedForRepair(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
	}{
		{"retries exhausted", testErr{msg: "flaky", retry: true}},
		{"not retryable", testErr{msg: "forbidden", retry: false}},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, dir, "a.txt", "hello")
			st := newState(t)
			e := New(Config{Store: &retryStore{failN: 99, failErr: c.err}, DataDir: dir, State: st})
			fastRetry(e)

			e.executeWithRetry(context.Background(), fsevent.Event{Op: fsevent.OpWrite, Path: "a.txt"})

			if got := owed(t, st); len(got) != 1 || got[0] != "a.txt" {
				t.Fatalf("repair list = %v; want [a.txt]", got)
			}
		})
	}
}

// A push that eventually succeeds owes nothing.
func TestASuccessfulRetryRecordsNoRepair(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "hello")
	st := newState(t)
	e := New(Config{
		Store:   &retryStore{failN: 2, failErr: testErr{msg: "flaky", retry: true}},
		DataDir: dir, State: st,
	})
	fastRetry(e)

	e.executeWithRetry(context.Background(), fsevent.Event{Op: fsevent.OpWrite, Path: "a.txt"})

	if got := owed(t, st); len(got) != 0 {
		t.Fatalf("repair list = %v; want empty after a push that succeeded", got)
	}
}

// Which path a record names, per op. A rename is owed at its DESTINATION, where
// the bytes now are; a deletion is owed at the path it removed — not to re-send
// anything, but so a sweep falls due to re-make it (see the gone-locally row).
func TestRepairPathOf(t *testing.T) {
	cases := []struct {
		ev   fsevent.Event
		want string
	}{
		{fsevent.Event{Op: fsevent.OpWrite, Path: "a.txt"}, "a.txt"},
		{fsevent.Event{Op: fsevent.OpCreate, Path: "a.txt"}, "a.txt"},
		{fsevent.Event{Op: fsevent.OpMkdir, Path: "d"}, "d"},
		{fsevent.Event{Op: fsevent.OpRename, Path: "old.txt", NewPath: "new.txt"}, "new.txt"},
		{fsevent.Event{Op: fsevent.OpUnlink, Path: "a.txt"}, "a.txt"},
		{fsevent.Event{Op: fsevent.OpRmdir, Path: "d"}, "d"},
		{fsevent.Event{Op: fsevent.OpSetattr, Path: "a.txt"}, ""},
		{fsevent.Event{Op: fsevent.OpWrite, Path: ""}, ""},
	}
	for _, c := range cases {
		got, ok := repairPathOf(c.ev)
		if !ok {
			got = ""
		}
		if got != c.want {
			t.Errorf("repairPathOf(%s %s -> %s) = %q; want %q", c.ev.Op, c.ev.Path, c.ev.NewPath, got, c.want)
		}
	}
}

// An engine with no state store has nowhere to record the debt and must not
// panic reaching for one. Log-only mounts are the shape that has none.
func TestAbandonedPushWithNoStateStoreIsHarmless(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "hello")
	e := New(Config{Store: &retryStore{failN: 99, failErr: testErr{msg: "forbidden"}}, DataDir: dir})
	fastRetry(e)
	e.executeWithRetry(context.Background(), fsevent.Event{Op: fsevent.OpWrite, Path: "a.txt"})
}

// --- the repair pass --------------------------------------------------------

// THE regression test for M27 part 2, and the one that decided its shape.
//
// A file that was synced, then rewritten locally, whose push was then abandoned:
// the remote still holds the content the baseline describes, so the sweep's
// remote pass matches the echo and correctly does nothing, and pushLocalOnly
// skips every path that has a baseline at all. Measured before the fix: the
// sweep re-pushed nothing, so a bare "a sweep is owed" flag would have scheduled
// a sweep that repaired nothing.
func TestSweepRepushesAnAbandonedWriteToAnAlreadySyncedFile(t *testing.T) {
	f := newSweepFixture(t, ReconcileOptions{Fetch: true},
		[]provider.RemoteFile{remote("a.txt", "old")})
	f.store.content["a.txt"] = []byte("old")

	// We last synced "old" at a.txt, which is still what the remote holds.
	if err := f.st.SetEcho("a.txt", state.Echo{
		Hash: md5hex("old"), Version: "1", At: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	// The local file holds content the engine gave up on sending.
	writeFile(t, f.dir, "a.txt", "new and unsent")
	if err := f.st.OweRepair("a.txt", time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}

	f.run(t)

	if got, want := f.push.ops(), []string{"write a.txt"}; !equal(got, want) {
		t.Fatalf("pushes = %v; want %v — the abandoned write was never re-sent", got, want)
	}
	if got := owed(t, f.st); len(got) != 0 {
		t.Errorf("repair list = %v; want empty once the path has been re-pushed", got)
	}
}

// The record is dropped BEFORE the push, so a re-push that is abandoned again
// leaves a fresh record behind instead of having its own erased.
func TestARepairThatFailsAgainStaysOwed(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "still unsent")
	st := newState(t)
	engine := New(Config{
		Store:   &retryStore{failN: 99, failErr: testErr{msg: "forbidden", retry: false}},
		DataDir: dir, State: st,
	})
	fastRetry(engine)

	if err := st.OweRepair("a.txt", time.Now()); err != nil {
		t.Fatal(err)
	}
	dl := NewDownloader(nil, newFakeStore(), dir, st, DefaultCadence)
	var stats sweepStats
	dl.rec = ReconcileOptions{Push: engine}
	dl.pushOwedRepairs(context.Background(), &stats, nil)

	if stats.repaired != 1 {
		t.Fatalf("repaired = %d; want 1 attempt", stats.repaired)
	}
	if got := owed(t, st); len(got) != 1 || got[0] != "a.txt" {
		t.Fatalf("repair list = %v; want [a.txt] — a repair that failed again must stay owed", got)
	}
}

// A record whose path no longer exists locally is dropped without a push. There
// is nothing to send, and asking the engine would fail the push and re-record
// the path forever.
func TestARepairForAVanishedPathIsDropped(t *testing.T) {
	dir := t.TempDir()
	st := newState(t)
	if err := st.OweRepair("gone.txt", time.Now()); err != nil {
		t.Fatal(err)
	}
	push := &recPusher{}
	dl := NewDownloader(nil, newFakeStore(), dir, st, DefaultCadence)
	dl.rec = ReconcileOptions{Push: push}

	var stats sweepStats
	dl.pushOwedRepairs(context.Background(), &stats, nil)

	if ops := push.ops(); len(ops) != 0 {
		t.Errorf("pushes = %v; want none for a path that is gone locally", ops)
	}
	if got := owed(t, st); len(got) != 0 {
		t.Errorf("repair list = %v; want the stale record dropped", got)
	}
	if stats.repaired != 0 {
		t.Errorf("repaired = %d; want 0", stats.repaired)
	}
}

// A directory is re-sent as a mkdir, not as a zero-byte write.
func TestARepairForADirectoryPushesMkdir(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	st := newState(t)
	if err := st.OweRepair("sub", time.Now()); err != nil {
		t.Fatal(err)
	}
	push := &recPusher{}
	dl := NewDownloader(nil, newFakeStore(), dir, st, DefaultCadence)
	dl.rec = ReconcileOptions{Push: push}

	var stats sweepStats
	dl.pushOwedRepairs(context.Background(), &stats, nil)

	if got, want := push.ops(), []string{"mkdir sub"}; !equal(got, want) {
		t.Fatalf("pushes = %v; want %v", got, want)
	}
}

// A placeholder is remote-born, so re-pushing one would upload its zeros over
// the real remote file — the M5 catastrophe, reachable here by a new route.
func TestARepairNeverPushesAPlaceholder(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "big.bin", "")
	st := newState(t)
	if err := st.OweRepair("big.bin", time.Now()); err != nil {
		t.Fatal(err)
	}
	push := &recPusher{}
	dl := NewDownloader(nil, newFakeStore(), dir, st, DefaultCadence).Lazy(holeyMaterializer{})
	dl.rec = ReconcileOptions{Push: push}

	var stats sweepStats
	dl.pushOwedRepairs(context.Background(), &stats, nil)

	if ops := push.ops(); len(ops) != 0 {
		t.Fatalf("pushes = %v; a placeholder must never be re-pushed", ops)
	}
	if got := owed(t, st); len(got) != 0 {
		t.Errorf("repair list = %v; want the record dropped rather than retried forever", got)
	}
}

// holeyMaterializer is allPlaceholders (rangewrite_test.go) as a Materializer:
// the downloader needs both halves of the interface, the uploader only one.
type holeyMaterializer struct{ allPlaceholders }

func (holeyMaterializer) CreatePlaceholder(string, provider.RemoteFile) error { return nil }

// --- scheduling -------------------------------------------------------------

// An abandoned push puts a sweep on the schedule even where there was none:
// -sweep-interval 0 means "do not poll", not "never repair".
func TestARepairSchedulesASweepWithTheIntervalDisabled(t *testing.T) {
	f := newSweepFixture(t, ReconcileOptions{Fetch: true, Interval: 0})
	f.dl.repairBase = 10 * time.Millisecond

	// Startup: nothing owed yet, so no deadline at all.
	f.run(t)
	if !f.dl.nextSweep.IsZero() {
		t.Fatalf("nextSweep = %v; want none with -sweep-interval 0", f.dl.nextSweep)
	}
	before := f.store.swept.Load()

	if err := f.st.OweRepair("a.txt", time.Now()); err != nil {
		t.Fatal(err)
	}
	// The first look arms the deadline; it must not sweep on the spot.
	if _, swept := f.dl.dueSweep(context.Background()); swept {
		t.Fatal("a sweep ran the moment a push was abandoned; it must wait out repairDelay")
	}
	if f.dl.nextSweep.IsZero() {
		t.Fatal("no repair sweep was scheduled")
	}

	time.Sleep(20 * time.Millisecond)
	if _, swept := f.dl.dueSweep(context.Background()); !swept {
		t.Fatal("the repair sweep did not run once its deadline passed")
	}
	if got := f.store.swept.Load(); got <= before {
		t.Errorf("sweeps = %d; want another one", got-before)
	}
	// And the schedule goes back to "never": one abandoned push must not leave a
	// mount with -sweep-interval 0 sweeping forever.
	if !f.dl.nextSweep.IsZero() {
		t.Errorf("nextSweep = %v; want none again once the repair sweep has run", f.dl.nextSweep)
	}
}

// A debt recorded by a previous run is swept at startup: repairDelay exists to
// collapse a live outage into one sweep, and that outage is already over.
func TestADebtFromAPreviousRunSweepsAtStartup(t *testing.T) {
	f := newSweepFixture(t, ReconcileOptions{Fetch: true, Interval: time.Hour})
	// A completed sweep, so neither the first-run nor the overdue trigger fires.
	if err := f.st.SetCursor("already-tailing"); err != nil {
		t.Fatal(err)
	}
	if err := f.st.FinishSweep(time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := f.st.OweRepair("a.txt", time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}

	f.run(t)

	if got := f.store.swept.Load(); got == 0 {
		t.Fatal("a debt carried across a restart did not trigger a sweep")
	}
}

// With nothing owed, nothing is scheduled and nothing is logged about repairs.
func TestNoRepairOwedSchedulesNothing(t *testing.T) {
	f := newSweepFixture(t, ReconcileOptions{Fetch: true, Interval: 0})
	f.run(t)
	if _, swept := f.dl.dueSweep(context.Background()); swept {
		t.Fatal("a sweep ran with nothing owed and no interval")
	}
	if !f.dl.nextSweep.IsZero() || !f.dl.repairDue.IsZero() {
		t.Errorf("deadlines armed with nothing owed: next=%v repair=%v", f.dl.nextSweep, f.dl.repairDue)
	}
}

// A repair sweep that leaves the debt standing backs off, so a provider that is
// failing but still enumerable is not swept every five minutes forever.
func TestRepeatedRepairFailuresBackOff(t *testing.T) {
	d := &Downloader{repairBase: repairDelay}
	for _, c := range []struct {
		fails int
		want  time.Duration
	}{
		{0, 5 * time.Minute},
		{1, 10 * time.Minute},
		{4, 80 * time.Minute},
		{9, 80 * time.Minute}, // capped
	} {
		d.repairFails = c.fails
		if got := d.repairWait(); got != c.want {
			t.Errorf("repairWait after %d failure(s) = %s; want %s", c.fails, got, c.want)
		}
	}
}
