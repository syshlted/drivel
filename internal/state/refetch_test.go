// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package state

import (
	"testing"
	"time"
)

func refetchOwed(t *testing.T, s *Store, path string) bool {
	t.Helper()
	recs, err := s.Lookup([]string{path})
	if err != nil {
		t.Fatal(err)
	}
	return recs[path].OwesFetch
}

// What the inbound path leaves behind when an apply fails: the path, the first
// time, and nothing else.
func TestRefetchRecordRoundTrip(t *testing.T) {
	s := openTestStore(t)
	if owed, err := s.RefetchesOwed(); err != nil || owed {
		t.Fatalf("RefetchesOwed on a fresh store = %v, %v; want false", owed, err)
	}

	first := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	if err := s.OweRefetch("a.txt", first); err != nil {
		t.Fatal(err)
	}
	if err := s.OweRefetch("a.txt", first.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if owed, err := s.RefetchesOwed(); err != nil || !owed {
		t.Fatalf("RefetchesOwed after a record = %v, %v; want true", owed, err)
	}
	n := 0
	if err := s.EachRefetch(func(p string, at time.Time) error {
		n++
		if p != "a.txt" || !at.Equal(first) {
			t.Errorf("EachRefetch yielded %q at %v; want a.txt at the FIRST failure, %v", p, at, first)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("EachRefetch yielded %d records; want 1", n)
	}
	recs, err := s.Lookup([]string{"a.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if r := recs["a.txt"]; !r.OwesFetch || !r.FetchOwedAt.Equal(first) {
		t.Fatalf("Lookup = %+v; want OwesFetch at %v", r, first)
	}

	if err := s.ClearRefetch("a.txt"); err != nil {
		t.Fatal(err)
	}
	if owed, _ := s.RefetchesOwed(); owed {
		t.Fatal("record survived ClearRefetch")
	}
}

// A backing filesystem that refuses every write fails every change in every
// page; recording the same path again, or clearing one that is not there, must
// not turn that into a commit per attempt (DESIGN.md §12).
func TestRefetchRecordsCompareBeforeWriting(t *testing.T) {
	s := openTestStore(t)
	if err := s.OweRefetch("a.txt", time.Now()); err != nil {
		t.Fatal(err)
	}
	before := s.Commits()
	for range 10 {
		if err := s.OweRefetch("a.txt", time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := s.ClearRefetch("absent.txt"); err != nil {
			t.Fatal(err)
		}
	}
	if got := s.Commits() - before; got != 0 {
		t.Fatalf("re-recording and clearing nothing cost %d commit(s); want 0", got)
	}
}

// The three writes that make a retry pointless each discharge the debt inside
// the transaction they already commit, so a retry that succeeds costs no extra
// commit and a path that went away does not keep a record nobody can settle.
func TestRefetchIsDischargedByTheWritesThatSettleIt(t *testing.T) {
	cases := map[string]func(s *Store) error{
		"SetEcho":       func(s *Store) error { return s.SetEcho("a.txt", Echo{Hash: "h", At: time.Now()}) },
		"ForgetMany":    func(s *Store) error { return s.ForgetMany([]string{"a.txt"}) },
		"ForgetDeleted": func(s *Store) error { return s.ForgetDeleted("a.txt", Tombstone{Deleted: time.Now()}) },
	}
	for name, settle := range cases {
		t.Run(name, func(t *testing.T) {
			s := openTestStore(t)
			if err := s.OweRefetch("a.txt", time.Now()); err != nil {
				t.Fatal(err)
			}
			before := s.Commits()
			if err := settle(s); err != nil {
				t.Fatal(err)
			}
			if got := s.Commits() - before; got != 1 {
				t.Errorf("%s cost %d commits; want its own 1", name, got)
			}
			if refetchOwed(t, s, "a.txt") {
				t.Errorf("%s left the refetch record standing", name)
			}
		})
	}
}

// A directory move carries the debt to where the object now is, like repair.
func TestRenameSubtreeCarriesARefetch(t *testing.T) {
	s := openTestStore(t)
	if err := s.OweRefetch("old/a.txt", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RenameSubtree("old", "new"); err != nil {
		t.Fatal(err)
	}
	if refetchOwed(t, s, "old/a.txt") || !refetchOwed(t, s, "new/a.txt") {
		t.Fatal("the refetch record did not move with its subtree")
	}
}
