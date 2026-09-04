package state

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestEchoMatches(t *testing.T) {
	cases := []struct {
		name      string
		rec       Echo
		hash, ver string
		want      bool
	}{
		{"same hash", Echo{Hash: "abc", Version: "1"}, "abc", "9", true},
		{"different hash", Echo{Hash: "abc", Version: "1"}, "xyz", "1", false},
		{"no hashes, same version", Echo{Version: "5"}, "", "5", true},
		{"no hashes, different version", Echo{Version: "5"}, "", "6", false},
		{"record has hash, incoming none", Echo{Hash: "abc"}, "", "1", false},
		{"both empty", Echo{}, "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.rec.Matches(c.hash, c.ver); got != c.want {
				t.Errorf("Matches(%q,%q) = %v, want %v", c.hash, c.ver, got, c.want)
			}
		})
	}
}

func TestCursorAndEchoRoundTrip(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if _, ok, _ := st.Cursor(); ok {
		t.Fatal("fresh store reported a cursor")
	}
	if err := st.SetCursor("tok-1"); err != nil {
		t.Fatal(err)
	}
	if cur, ok, _ := st.Cursor(); !ok || cur != "tok-1" {
		t.Fatalf("cursor = %q ok=%v; want tok-1", cur, ok)
	}

	want := Echo{Hash: "h", Version: "2", At: time.Unix(1700000000, 0)}
	if err := st.SetEcho("p/q.txt", want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := st.GetEcho("p/q.txt")
	if err != nil || !ok || got.Hash != want.Hash || got.Version != want.Version {
		t.Fatalf("GetEcho = %+v ok=%v err=%v; want %+v", got, ok, err, want)
	}
	if err := st.DeleteEcho("p/q.txt"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.GetEcho("p/q.txt"); ok {
		t.Fatal("echo survived delete")
	}
}

// --- enumeration sweep (M7b) ------------------------------------------------

// openTestStore is a state store in a temp dir, closed with the test.
func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// The sweep record round-trips, and clearing it takes the seen-set with it: the
// marks of a finished sweep must not be readable as a later one's.
func TestSweepRecordRoundTripAndClear(t *testing.T) {
	s := openTestStore(t)

	if _, ok, err := s.Sweep(); err != nil || ok {
		t.Fatalf("Sweep on a fresh store = ok %v, err %v; want no sweep", ok, err)
	}
	sw := Sweep{Gen: "g1", Token: "tok0", Cursor: "page-2", Started: time.Now().Truncate(time.Second)}
	if err := s.SetSweep(sw); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Sweep()
	if err != nil || !ok {
		t.Fatalf("Sweep = ok %v, err %v", ok, err)
	}
	if got.Gen != sw.Gen || got.Token != sw.Token || got.Cursor != sw.Cursor || !got.Started.Equal(sw.Started) {
		t.Fatalf("Sweep = %+v; want %+v", got, sw)
	}

	if err := s.MarkSeen("g1", []string{"a.txt"}); err != nil {
		t.Fatal(err)
	}
	done := time.Now().Truncate(time.Second)
	if err := s.FinishSweep(done); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Sweep(); ok {
		t.Fatal("sweep record survived FinishSweep")
	}
	if seen, _ := s.SeenPath("g1", "a.txt"); seen {
		t.Fatal("seen mark survived FinishSweep")
	}
	// The completion stamp is what the periodic schedule reads; a sweep that ends
	// without one makes the next mount think it is overdue.
	at, ok, err := s.SweepDone()
	if err != nil || !ok {
		t.Fatalf("SweepDone = ok %v, err %v", ok, err)
	}
	if !at.Equal(done) {
		t.Fatalf("SweepDone = %s; want %s", at, done)
	}
}

// A store that has never finished a sweep reports none, which the schedule reads
// as overdue rather than as "just swept".
func TestSweepDoneIsAbsentUntilASweepFinishes(t *testing.T) {
	s := openTestStore(t)
	if _, ok, err := s.SweepDone(); err != nil || ok {
		t.Fatalf("SweepDone on a fresh store = ok %v, err %v; want false, nil", ok, err)
	}
}

// Marks are namespaced by generation, so a sweep that was abandoned mid-way
// cannot lend its marks to the next one — which would make paths look present
// remotely that this sweep never saw.
func TestSeenMarksAreScopedToTheirGeneration(t *testing.T) {
	s := openTestStore(t)
	if err := s.MarkSeen("g1", []string{"a.txt", "dir/b.txt"}); err != nil {
		t.Fatal(err)
	}
	if seen, _ := s.SeenPath("g1", "dir/b.txt"); !seen {
		t.Fatal("mark not recorded for its own generation")
	}
	if seen, _ := s.SeenPath("g2", "dir/b.txt"); seen {
		t.Fatal("generation g2 can see g1's marks")
	}
	if seen, _ := s.SeenPath("", "a.txt"); seen {
		t.Fatal("an empty generation matched a mark")
	}
}

// EachUnseenEcho is the delete-candidate query: only paths that HAVE a baseline
// and were NOT seen. A path with no baseline is new, not deleted — the rule that
// makes the first-ever run delete nothing.
func TestEachUnseenEchoVisitsOnlyBaselinedAndUnseen(t *testing.T) {
	s := openTestStore(t)
	for _, p := range []string{"kept.txt", "gone.txt"} {
		if err := s.SetEcho(p, Echo{Hash: "h-" + p, At: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	// "kept.txt" was seen this sweep; "new.txt" exists remotely but has no baseline.
	if err := s.MarkSeen("g1", []string{"kept.txt", "new.txt"}); err != nil {
		t.Fatal(err)
	}

	got := map[string]Echo{}
	if err := s.EachUnseenEcho("g1", func(p string, e Echo) error {
		got[p] = e
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("EachUnseenEcho visited %v; want only gone.txt", got)
	}
	if e, ok := got["gone.txt"]; !ok || e.Hash != "h-gone.txt" {
		t.Fatalf("EachUnseenEcho visited %v; want gone.txt with its echo", got)
	}
}

// The walk stops on the callback's error and hands it back unwrapped: that is how
// the reconciler enforces -max-deletes without reading a bucket it is about to
// refuse to act on.
func TestEachUnseenEchoStopsOnCallbackError(t *testing.T) {
	s := openTestStore(t)
	for _, p := range []string{"a.txt", "b.txt", "c.txt"} {
		if err := s.SetEcho(p, Echo{Hash: "h", At: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	stop := errors.New("enough")
	visited := 0
	err := s.EachUnseenEcho("g1", func(string, Echo) error {
		visited++
		return stop
	})
	if !errors.Is(err, stop) {
		t.Fatalf("EachUnseenEcho err = %v; want %v", err, stop)
	}
	if visited != 1 {
		t.Fatalf("visited %d entries; want 1 (the walk did not stop)", visited)
	}
}

// HasEchoes distinguishes "never synced anything" from "synced and lost it",
// which is the difference between a first run and a reconcile with a baseline.
func TestHasEchoes(t *testing.T) {
	s := openTestStore(t)
	if has, err := s.HasEchoes(); err != nil || has {
		t.Fatalf("HasEchoes on a fresh store = %v, %v; want false", has, err)
	}
	if err := s.SetEcho("a.txt", Echo{Hash: "h"}); err != nil {
		t.Fatal(err)
	}
	if has, err := s.HasEchoes(); err != nil || !has {
		t.Fatalf("HasEchoes = %v, %v; want true", has, err)
	}
}

// ForgetMany clears both per-path buckets in one transaction, and is a no-op for
// paths that hold no records.
func TestForgetManyClearsEchoAndHydration(t *testing.T) {
	s := openTestStore(t)
	for _, p := range []string{"a.txt", "b.txt", "keep.txt"} {
		if err := s.SetEcho(p, Echo{Hash: "h-" + p, At: time.Now()}); err != nil {
			t.Fatal(err)
		}
		if err := s.SetHydration(p, []byte{0xff}); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.ForgetMany([]string{"a.txt", "b.txt", "never-stored.txt"}); err != nil {
		t.Fatal(err)
	}

	for _, p := range []string{"a.txt", "b.txt"} {
		if _, ok, err := s.GetEcho(p); err != nil || ok {
			t.Errorf("echo for %s survived: ok=%v err=%v", p, ok, err)
		}
		if _, ok, err := s.Hydration(p); err != nil || ok {
			t.Errorf("hydration for %s survived: ok=%v err=%v", p, ok, err)
		}
	}
	if _, ok, _ := s.GetEcho("keep.txt"); !ok {
		t.Error("ForgetMany dropped a path it was not given")
	}
	if _, ok, _ := s.Hydration("keep.txt"); !ok {
		t.Error("ForgetMany dropped a hydration record it was not given")
	}
	if err := s.ForgetMany(nil); err != nil {
		t.Errorf("empty ForgetMany: %v", err)
	}
}

// Forget is the single-path spelling of the same thing.
func TestForgetClearsOnePath(t *testing.T) {
	s := openTestStore(t)
	if err := s.SetEcho("x", Echo{Hash: "h", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetHydration("x", []byte{1}); err != nil {
		t.Fatal(err)
	}
	if err := s.Forget("x"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.GetEcho("x"); ok {
		t.Error("echo survived Forget")
	}
	if _, ok, _ := s.Hydration("x"); ok {
		t.Error("hydration survived Forget")
	}
}
