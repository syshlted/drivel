// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package state

import (
	"path/filepath"
	"testing"
	"time"
)

// A tombstone is the baseline a deletion removed, kept where no existing reader
// can see it. These are the store-level properties the suppression rests on; the
// behaviour they support is in internal/syncengine's tombstone_test.go.

func openStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// ForgetDeleted is Forget plus the record, and it is one commit — the same as
// the delete push cost before tombstones existed (DESIGN.md §12).
func TestForgetDeletedReplacesTheBaselineInOneCommit(t *testing.T) {
	s := openStore(t)
	e := Echo{Hash: "abc", Version: "7", At: time.Now()}
	if err := s.SetEcho("a.txt", e); err != nil {
		t.Fatal(err)
	}
	if err := s.SetHydration("a.txt", []byte{1, 2, 3}); err != nil {
		t.Fatal(err)
	}

	base := s.Commits()
	if err := s.ForgetDeleted("a.txt", Tombstone{Echo: e, Deleted: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if got := s.Commits() - base; got != 1 {
		t.Errorf("ForgetDeleted committed %d times; want 1", got)
	}

	if _, ok, _ := s.GetEcho("a.txt"); ok {
		t.Error("the echo survived the deletion that entombed it")
	}
	if _, ok, _ := s.Hydration("a.txt"); ok {
		t.Error("the hydration bitmap survived the deletion")
	}
	tomb, ok, err := s.Tombstone("a.txt")
	if err != nil || !ok {
		t.Fatalf("no tombstone stored (ok=%v, err=%v)", ok, err)
	}
	if !tomb.Echo.Matches("abc", "7") {
		t.Errorf("tombstone does not carry the deleted baseline: %+v", tomb)
	}
}

// A tombstone must be invisible to every reader that existed before it, because
// those readers are M7b's delete baseline, M6's gate 2 and M27's lost-push row.
// HasEchoes is the sharpest of them: it answers "has this mount ever synced
// anything", and a tombstone is not a sync.
func TestATombstoneIsNotABaseline(t *testing.T) {
	s := openStore(t)
	e := Echo{Hash: "abc", Version: "7", At: time.Now()}
	if err := s.ForgetDeleted("a.txt", Tombstone{Echo: e, Deleted: time.Now()}); err != nil {
		t.Fatal(err)
	}

	if has, err := s.HasEchoes(); err != nil || has {
		t.Errorf("HasEchoes = %v (err %v); a tombstone must not read as a baseline", has, err)
	}
	if err := s.MarkSeen("gen1", []string{"b.txt"}); err != nil {
		t.Fatal(err)
	}
	seen := 0
	if err := s.EachUnseenEcho("gen1", func(string, Echo) error { seen++; return nil }); err != nil {
		t.Fatal(err)
	}
	if seen != 0 {
		t.Errorf("the delete pass saw %d tombstone(s) as deletable baselines; want 0", seen)
	}
}

// Syncing a path again clears its tombstone: a live echo and a tombstone are
// contradictory claims about one path, and the echo is always the newer.
func TestSetEchoClearsTheTombstone(t *testing.T) {
	s := openStore(t)
	if err := s.ForgetDeleted("a.txt", Tombstone{Echo: Echo{Hash: "old"}, Deleted: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetEcho("a.txt", Echo{Hash: "new", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Tombstone("a.txt"); ok {
		t.Error("the tombstone survived a later sync of that path")
	}
}

// And so does the inbound path agreeing that the object is gone — which is the
// commonest way a tombstone is retired, at no extra cost: ForgetMany is already
// one commit per feed page.
func TestForgetManyClearsTombstones(t *testing.T) {
	s := openStore(t)
	if err := s.ForgetDeleted("a.txt", Tombstone{Echo: Echo{Hash: "old"}, Deleted: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.ForgetMany([]string{"a.txt"}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Tombstone("a.txt"); ok {
		t.Error("the tombstone survived a confirmed removal of that path")
	}
}

// Pruning drops what is expired, keeps what is not, and — the §12 half — does
// not commit when there is nothing to drop.
func TestPruneTombstones(t *testing.T) {
	s := openStore(t)
	now := time.Now()
	if err := s.ForgetDeleted("old.txt", Tombstone{Echo: Echo{Hash: "a"}, Deleted: now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := s.ForgetDeleted("fresh.txt", Tombstone{Echo: Echo{Hash: "b"}, Deleted: now}); err != nil {
		t.Fatal(err)
	}

	n, err := s.PruneTombstones(now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("pruned %d; want 1", n)
	}
	if _, ok, _ := s.Tombstone("old.txt"); ok {
		t.Error("an expired tombstone survived the prune")
	}
	if _, ok, _ := s.Tombstone("fresh.txt"); !ok {
		t.Error("a live tombstone was pruned")
	}

	base := s.Commits()
	if n, err := s.PruneTombstones(now.Add(-time.Minute)); err != nil || n != 0 {
		t.Fatalf("second prune removed %d (err %v); want 0", n, err)
	}
	if got := s.Commits() - base; got != 0 {
		t.Errorf("a prune with nothing to do committed %d time(s); bbolt charges ~20 KB for that", got)
	}
}
