// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package state

import (
	"sort"
	"testing"
	"time"
)

// What the outbound path leaves behind when it gives up (M27): the path, the
// time, and nothing else.
func TestRepairRecordRoundTrip(t *testing.T) {
	s := openTestStore(t)

	if owed, err := s.RepairsOwed(); err != nil || owed {
		t.Fatalf("RepairsOwed on a fresh store = %v, %v; want false", owed, err)
	}

	at := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	for _, p := range []string{"b/two.txt", "a/one.txt"} {
		if err := s.OweRepair(p, at); err != nil {
			t.Fatal(err)
		}
	}

	if owed, err := s.RepairsOwed(); err != nil || !owed {
		t.Fatalf("RepairsOwed after two records = %v, %v; want true", owed, err)
	}

	var got []string
	if err := s.EachRepair(func(p string, when time.Time) error {
		got = append(got, p)
		if !when.Equal(at) {
			t.Errorf("%s recorded at %v; want %v", p, when, at)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	if len(got) != 2 || got[0] != "a/one.txt" || got[1] != "b/two.txt" {
		t.Fatalf("EachRepair yielded %v; want both paths", got)
	}

	if err := s.ClearRepair("a/one.txt"); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearRepair("b/two.txt"); err != nil {
		t.Fatal(err)
	}
	if owed, err := s.RepairsOwed(); err != nil || owed {
		t.Fatalf("RepairsOwed after clearing both = %v, %v; want false", owed, err)
	}
}

// Re-recording a path already owed must cost no commit.
//
// This is the disk-cost ratchet, and it is the budget that matters most here:
// the shape that abandons the most pushes is a non-retryable error, which gives
// up after a single attempt with no backoff, so without the check a permission
// failure over one directory would fsync twice per failed file per retry round.
// Nothing about correctness would ever notice — the value stored is right
// either way.
func TestOweRepairCommitsOncePerPath(t *testing.T) {
	s := openTestStore(t)

	base := s.Commits()
	if err := s.OweRepair("a.txt", time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := s.Commits() - base; got != 1 {
		t.Fatalf("the first record of a path cost %d commit(s); want 1", got)
	}

	after := s.Commits()
	for i := 0; i < 20; i++ {
		if err := s.OweRepair("a.txt", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if got := s.Commits() - after; got != 0 {
		t.Errorf("20 repeats of an owed path cost %d commit(s); want 0", got)
	}

	// A second path is genuinely new information and is worth its commit.
	after = s.Commits()
	if err := s.OweRepair("b.txt", time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := s.Commits() - after; got != 1 {
		t.Errorf("a new owed path cost %d commit(s); want 1", got)
	}
}

// The first time a path was abandoned is when the local copy started being
// ahead of the remote, so a later abandonment must not overwrite it.
func TestOweRepairKeepsTheFirstTime(t *testing.T) {
	s := openTestStore(t)

	first := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second)
	if err := s.OweRepair("a.txt", first); err != nil {
		t.Fatal(err)
	}
	if err := s.OweRepair("a.txt", time.Now()); err != nil {
		t.Fatal(err)
	}

	var got time.Time
	if err := s.EachRepair(func(_ string, when time.Time) error {
		got = when
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !got.Equal(first) {
		t.Errorf("recorded time = %v; want the first one, %v", got, first)
	}
}

// An empty path is not a record. It would be unreachable from the engine, which
// only ever offers a path an event named, but a key of "" is indistinguishable
// from the bucket being non-empty and would make every sweep a repair sweep.
func TestOweRepairIgnoresTheEmptyPath(t *testing.T) {
	s := openTestStore(t)
	base := s.Commits()
	if err := s.OweRepair("", time.Now()); err != nil {
		t.Fatal(err)
	}
	if owed, _ := s.RepairsOwed(); owed {
		t.Error("an empty path was recorded as owed")
	}
	if got := s.Commits() - base; got != 0 {
		t.Errorf("an empty path cost %d commit(s); want 0", got)
	}
}

// The shutdown deadline records everything it shed at once: one commit however
// many paths, and a path already owed keeps the stamp of its first abandonment.
func TestOweRepairsIsOneCommitAndKeepsTheFirstStamp(t *testing.T) {
	s := openTestStore(t)
	first := time.Date(2026, 10, 17, 8, 0, 0, 0, time.UTC)
	if err := s.OweRepair("a.txt", first); err != nil {
		t.Fatal(err)
	}
	before := s.Commits()
	if err := s.OweRepairs([]string{"a.txt", "b.txt", "", "c/d.txt"}, first.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := s.Commits() - before; got != 1 {
		t.Errorf("OweRepairs cost %d commits; want 1", got)
	}
	got := map[string]time.Time{}
	if err := s.EachRepair(func(p string, at time.Time) error { got[p] = at; return nil }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || !got["a.txt"].Equal(first) {
		t.Errorf("repairs = %v; want a.txt (first stamp kept), b.txt, c/d.txt", got)
	}
}
