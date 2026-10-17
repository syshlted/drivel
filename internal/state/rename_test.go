// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package state

import (
	"testing"
	"time"
)

// A move changes an object's path without changing the object, so the records
// keyed by that path follow it. These are the store-level properties; the
// behaviour they support — a directory rename costing one rename instead of a
// subtree of downloads and a refused delete pass — is in internal/syncengine's
// move_test.go.

func echoAt(t *testing.T, s *Store, p, hash string) {
	t.Helper()
	if err := s.SetEcho(p, Echo{Hash: hash, Version: "v", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
}

func hashAt(t *testing.T, s *Store, p string) string {
	t.Helper()
	e, ok, err := s.GetEcho(p)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		return ""
	}
	return e.Hash
}

// The subtree moves: the directory's own record and every descendant's, at every
// depth, and the old keys are gone rather than duplicated.
func TestRenameSubtreeMovesTheWholeSubtree(t *testing.T) {
	s := openStore(t)
	echoAt(t, s, "wide", "dir")
	echoAt(t, s, "wide/a.txt", "aaa")
	echoAt(t, s, "wide/sub/b.txt", "bbb")
	echoAt(t, s, "wide/sub/deep/c.txt", "ccc")
	// A sibling whose name merely starts with the moved one. "wide" is a prefix of
	// "wideload", so a scan keyed on the bare prefix rather than on "wide/" would
	// drag this along — and it must not, because nothing moved it.
	echoAt(t, s, "wideload.txt", "zzz")

	n, err := s.RenameSubtree("wide", "narrow")
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Errorf("moved %d record(s), want 4", n)
	}
	for old, want := range map[string]string{
		"narrow":                "dir",
		"narrow/a.txt":          "aaa",
		"narrow/sub/b.txt":      "bbb",
		"narrow/sub/deep/c.txt": "ccc",
		"wideload.txt":          "zzz",
	} {
		if got := hashAt(t, s, old); got != want {
			t.Errorf("%s = %q, want %q", old, got, want)
		}
	}
	for _, gone := range []string{"wide", "wide/a.txt", "wide/sub/b.txt", "wide/sub/deep/c.txt"} {
		if got := hashAt(t, s, gone); got != "" {
			t.Errorf("%s still holds %q; the old keys must not survive the move", gone, got)
		}
	}
}

// Whatever the subtree's size, one commit — which is the only reason this is
// affordable at the scale that motivates it (DESIGN.md §12: cost is commits times
// a constant, so 1101 paths one at a time would be ~39 MB of disk writes).
func TestRenameSubtreeIsOneCommit(t *testing.T) {
	s := openStore(t)
	for i := range 200 {
		echoAt(t, s, "d/f"+string(rune('a'+i%26))+string(rune('a'+i/26)), "h")
	}
	base := s.Commits()
	if _, err := s.RenameSubtree("d", "e"); err != nil {
		t.Fatal(err)
	}
	if got := s.Commits() - base; got != 1 {
		t.Errorf("renaming a 200-record subtree committed %d times, want 1", got)
	}
}

// Nothing to move costs no commit at all: an Update that writes nothing still
// advances bbolt's meta page, and the inbound side asks this question for every
// reported move on every client, most of which never held the subtree.
func TestRenameSubtreeOfNothingCommitsNothing(t *testing.T) {
	s := openStore(t)
	echoAt(t, s, "other.txt", "h")
	base := s.Commits()
	n, err := s.RenameSubtree("absent", "elsewhere")
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("moved %d record(s), want 0", n)
	}
	if got := s.Commits() - base; got != 0 {
		t.Errorf("a move with nothing to move committed %d times, want 0", got)
	}
}

// A sibling rename where one name is a prefix of the other, end to end. This is
// the shape that LOOKS like it needs the delete-before-write ordering and does
// not: "wideout" is not inside "wide", so the two key sets are disjoint. What it
// genuinely exercises is that a scan for "wide" keys on "wide/" — the assertion
// for which is the untouched sibling in TestRenameSubtreeMovesTheWholeSubtree,
// and the guard for the one case where the ordering would matter is in
// TestRenameSubtreeRefusesWhatItCannotDo.
func TestRenameSubtreeASiblingPrefixRename(t *testing.T) {
	s := openStore(t)
	echoAt(t, s, "wide", "dir")
	echoAt(t, s, "wide/a.txt", "aaa")

	if _, err := s.RenameSubtree("wide", "wideout"); err != nil {
		t.Fatal(err)
	}
	if got := hashAt(t, s, "wideout/a.txt"); got != "aaa" {
		t.Errorf("wideout/a.txt = %q, want %q", got, "aaa")
	}
	if got := hashAt(t, s, "wide/a.txt"); got != "" {
		t.Errorf("wide/a.txt still holds %q after the move", got)
	}
}

// Hydration and repair travel with the object; a tombstone does not. A tombstone
// records the deletion of whatever used to be at a path, and the object arriving
// there is a different one — carrying it across would arm a suppression against
// content nobody deleted.
func TestRenameSubtreeMovesRecordsButNotTombstones(t *testing.T) {
	s := openStore(t)
	echoAt(t, s, "d/f.txt", "aaa")
	if err := s.SetHydration("d/f.txt", []byte{9, 9}); err != nil {
		t.Fatal(err)
	}
	if err := s.OweRepair("d/f.txt", time.Now()); err != nil {
		t.Fatal(err)
	}
	// A tombstone at a path inside the subtree, from an earlier deletion there.
	if err := s.ForgetDeleted("d/old.txt", Tombstone{
		Echo:    Echo{Hash: "ddd", Version: "v"},
		Deleted: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := s.RenameSubtree("d", "e"); err != nil {
		t.Fatal(err)
	}

	if v, ok, err := s.Hydration("e/f.txt"); err != nil || !ok || len(v) != 2 {
		t.Errorf("hydration at e/f.txt = %v, %v, %v; want the moved bitmap", v, ok, err)
	}
	if _, ok, _ := s.Hydration("d/f.txt"); ok {
		t.Error("hydration record left behind at the old path")
	}
	owed := map[string]bool{}
	if err := s.EachRepair(func(p string, _ time.Time) error { owed[p] = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if !owed["e/f.txt"] || owed["d/f.txt"] {
		t.Errorf("repair debt is at %v; want it moved to e/f.txt", owed)
	}
	if _, ok, _ := s.Tombstone("d/old.txt"); !ok {
		t.Error("the tombstone moved or was dropped; it must stay where the deletion happened")
	}
	if _, ok, _ := s.Tombstone("e/old.txt"); ok {
		t.Error("a tombstone was carried across the move, arming a suppression nobody asked for")
	}
}

// Four requests this store will not perform, each returning (0, nil) so a caller
// treats it as "no move happened" rather than as a failure. The nested pair is
// the one with teeth: it is the only shape where the key ranges nest, and
// os.Rename refuses it anyway.
func TestRenameSubtreeRefusesWhatItCannotDo(t *testing.T) {
	cases := []struct{ name, from, to string }{
		{"empty source", "", "x"},
		{"empty destination", "x", ""},
		{"same path", "x", "x"},
		{"destination inside the source", "a", "a/b"},
		{"source inside the destination", "a/b", "a"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := openStore(t)
			echoAt(t, s, "a", "dir")
			echoAt(t, s, "a/b", "bbb")
			echoAt(t, s, "x", "xxx")
			base := s.Commits()
			n, err := s.RenameSubtree(c.from, c.to)
			if err != nil {
				t.Fatalf("RenameSubtree(%q, %q) = %v", c.from, c.to, err)
			}
			if n != 0 {
				t.Errorf("moved %d record(s), want 0", n)
			}
			if got := s.Commits() - base; got != 0 {
				t.Errorf("committed %d times, want 0", got)
			}
			if got := hashAt(t, s, "a/b"); got != "bbb" {
				t.Errorf("a/b = %q; the refused call must not have touched anything", got)
			}
		})
	}
}
