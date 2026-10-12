// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package pathindex

import (
	"os"
	"path/filepath"
	"testing"
)

func newStore(t *testing.T, identity string) *Store {
	t.Helper()
	s := openAt(t, filepath.Join(t.TempDir(), "index.db"))
	if _, err := s.Bind(identity); err != nil {
		t.Fatalf("bind: %v", err)
	}
	return s
}

func openAt(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func mustSet(t *testing.T, s *Store, path, id string) {
	t.Helper()
	if err := s.Set(path, id); err != nil {
		t.Fatalf("set %q: %v", path, err)
	}
}

func lookup(t *testing.T, s *Store, path string) (string, bool) {
	t.Helper()
	id, ok, err := s.Lookup(path)
	if err != nil {
		t.Fatalf("lookup %q: %v", path, err)
	}
	return id, ok
}

func pathFor(t *testing.T, s *Store, id string) (string, bool) {
	t.Helper()
	p, ok, err := s.PathFor(id)
	if err != nil {
		t.Fatalf("pathFor %q: %v", id, err)
	}
	return p, ok
}

func TestRoundTripBothDirections(t *testing.T) {
	s := newStore(t, "acct-1")
	mustSet(t, s, "a/b.txt", "id-b")

	if id, ok := lookup(t, s, "a/b.txt"); !ok || id != "id-b" {
		t.Fatalf("Lookup = %q, %v; want id-b, true", id, ok)
	}
	if p, ok := pathFor(t, s, "id-b"); !ok || p != "a/b.txt" {
		t.Fatalf("PathFor = %q, %v; want a/b.txt, true", p, ok)
	}
	if _, ok := lookup(t, s, "a/missing.txt"); ok {
		t.Error("unstored path reported as present")
	}
}

// The mount root is the empty path, which bbolt cannot store as a key — hence the
// encoding. If this breaks, the root silently stops being indexable.
func TestRootPathIsStorable(t *testing.T) {
	s := newStore(t, "acct-1")
	mustSet(t, s, "", "root-id")
	if id, ok := lookup(t, s, ""); !ok || id != "root-id" {
		t.Fatalf("root Lookup = %q, %v; want root-id, true", id, ok)
	}
	if p, ok := pathFor(t, s, "root-id"); !ok || p != "" {
		t.Fatalf("root PathFor = %q, %v; want \"\", true", p, ok)
	}
}

// A rewrite in either direction must not leave the other direction pointing at
// what used to be true: a stale reverse entry would resolve a change-feed ID to a
// path the object no longer occupies.
func TestSetEvictsStaleHalves(t *testing.T) {
	t.Run("path re-pointed to a new id", func(t *testing.T) {
		s := newStore(t, "acct-1")
		mustSet(t, s, "f.txt", "old-id")
		mustSet(t, s, "f.txt", "new-id")

		if p, ok := pathFor(t, s, "old-id"); ok {
			t.Errorf("replaced id still resolves to %q", p)
		}
		if p, _ := pathFor(t, s, "new-id"); p != "f.txt" {
			t.Errorf("PathFor(new-id) = %q; want f.txt", p)
		}
	})

	t.Run("id moved to a new path", func(t *testing.T) {
		s := newStore(t, "acct-1")
		mustSet(t, s, "old.txt", "id-1")
		mustSet(t, s, "new.txt", "id-1")

		if id, ok := lookup(t, s, "old.txt"); ok {
			t.Errorf("vacated path still resolves to %q", id)
		}
		if id, _ := lookup(t, s, "new.txt"); id != "id-1" {
			t.Errorf("Lookup(new.txt) = %q; want id-1", id)
		}
	})
}

func TestForgetDropsSubtree(t *testing.T) {
	s := newStore(t, "acct-1")
	mustSet(t, s, "a", "id-a")
	mustSet(t, s, "a/b.txt", "id-b")
	mustSet(t, s, "a/sub/c.txt", "id-c")
	mustSet(t, s, "ab", "id-ab") // prefix-string neighbour, not a descendant
	mustSet(t, s, "other.txt", "id-o")

	if err := s.Forget("a"); err != nil {
		t.Fatalf("forget: %v", err)
	}

	for _, gone := range []string{"a", "a/b.txt", "a/sub/c.txt"} {
		if _, ok := lookup(t, s, gone); ok {
			t.Errorf("%q survived Forget of its subtree", gone)
		}
	}
	for _, id := range []string{"id-a", "id-b", "id-c"} {
		if p, ok := pathFor(t, s, id); ok {
			t.Errorf("reverse entry %s -> %q survived Forget", id, p)
		}
	}
	// "ab" is a string-prefix match for "a" but not a child of it.
	if _, ok := lookup(t, s, "ab"); !ok {
		t.Error(`Forget("a") wrongly dropped the sibling "ab"`)
	}
	if _, ok := lookup(t, s, "other.txt"); !ok {
		t.Error("Forget dropped an unrelated path")
	}
}

func TestRenameMovesSubtree(t *testing.T) {
	s := newStore(t, "acct-1")
	mustSet(t, s, "dir", "id-dir")
	mustSet(t, s, "dir/f.txt", "id-f")
	mustSet(t, s, "dir/sub/g.txt", "id-g")
	mustSet(t, s, "dirt", "id-dirt") // prefix neighbour again

	if err := s.Rename("dir", "moved"); err != nil {
		t.Fatalf("rename: %v", err)
	}

	want := map[string]string{"moved": "id-dir", "moved/f.txt": "id-f", "moved/sub/g.txt": "id-g"}
	for p, id := range want {
		if got, ok := lookup(t, s, p); !ok || got != id {
			t.Errorf("after rename Lookup(%q) = %q, %v; want %q", p, got, ok, id)
		}
	}
	for _, gone := range []string{"dir", "dir/f.txt", "dir/sub/g.txt"} {
		if _, ok := lookup(t, s, gone); ok {
			t.Errorf("old key %q survived Rename", gone)
		}
	}
	if p, _ := pathFor(t, s, "id-g"); p != "moved/sub/g.txt" {
		t.Errorf("reverse index not rewritten: %q", p)
	}
	if id, ok := lookup(t, s, "dirt"); !ok || id != "id-dirt" {
		t.Error(`Rename("dir") disturbed the neighbour "dirt"`)
	}
}

// Renaming into an overlapping key range ("a" -> "ab") puts the new keys inside
// the range the old ones occupied. Deleting after writing would remove what was
// just written.
func TestRenameIntoOverlappingRange(t *testing.T) {
	s := newStore(t, "acct-1")
	mustSet(t, s, "a", "id-a")
	mustSet(t, s, "a/f.txt", "id-f")

	if err := s.Rename("a", "ab"); err != nil {
		t.Fatalf("rename: %v", err)
	}

	if id, ok := lookup(t, s, "ab"); !ok || id != "id-a" {
		t.Fatalf("Lookup(ab) = %q, %v; want id-a", id, ok)
	}
	if id, ok := lookup(t, s, "ab/f.txt"); !ok || id != "id-f" {
		t.Fatalf("Lookup(ab/f.txt) = %q, %v; want id-f", id, ok)
	}
	if _, ok := lookup(t, s, "a"); ok {
		t.Error(`old key "a" survived`)
	}
}

func TestPersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")

	s := openAt(t, path)
	if _, err := s.Bind("acct-1"); err != nil {
		t.Fatalf("bind: %v", err)
	}
	mustSet(t, s, "keep/me.txt", "id-keep")
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	again := openAt(t, path)
	reused, err := again.Bind("acct-1")
	if err != nil {
		t.Fatalf("rebind: %v", err)
	}
	if !reused {
		t.Fatal("Bind with the same identity reported the store as fresh")
	}
	if id, ok := lookup(t, again, "keep/me.txt"); !ok || id != "id-keep" {
		t.Fatalf("after reopen Lookup = %q, %v; want id-keep", id, ok)
	}
	if n, err := again.Len(); err != nil || n != 1 {
		t.Fatalf("Len = %d, %v; want 1", n, err)
	}
}

// The whole point of binding: a DB reused against another account (or another
// -drive-root) must not resolve this account's paths to that one's IDs.
func TestBindWipesOnIdentityChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")

	s := openAt(t, path)
	if _, err := s.Bind("acct-1"); err != nil {
		t.Fatalf("bind: %v", err)
	}
	mustSet(t, s, "secret.txt", "id-from-acct-1")
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	again := openAt(t, path)
	reused, err := again.Bind("acct-2")
	if err != nil {
		t.Fatalf("rebind: %v", err)
	}
	if reused {
		t.Fatal("Bind reported reuse across a changed identity")
	}
	if id, ok := lookup(t, again, "secret.txt"); ok {
		t.Fatalf("previous account's mapping survived rebinding: %q", id)
	}
	if p, ok := pathFor(t, again, "id-from-acct-1"); ok {
		t.Fatalf("previous account's reverse mapping survived: %q", p)
	}
}

// An unbound store is treated as no store at all: reads miss and writes are
// dropped, so nothing on disk can be believed before we know whose it is.
func TestUnboundStoreReadsEmptyAndDropsWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")

	s := openAt(t, path)
	if _, err := s.Bind("acct-1"); err != nil {
		t.Fatalf("bind: %v", err)
	}
	mustSet(t, s, "a.txt", "id-a")
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	unbound := openAt(t, path)
	if id, ok := lookup(t, unbound, "a.txt"); ok {
		t.Errorf("unbound store answered a lookup with %q", id)
	}
	if p, ok := pathFor(t, unbound, "id-a"); ok {
		t.Errorf("unbound store answered a reverse lookup with %q", p)
	}
	if n, err := unbound.Len(); err != nil || n != 0 {
		t.Errorf("unbound Len = %d, %v; want 0", n, err)
	}
	// Writes are no-ops rather than errors, so a caller that binds late is not
	// forced to special-case every call.
	mustSet(t, unbound, "b.txt", "id-b")
	if err := unbound.Forget("a.txt"); err != nil {
		t.Errorf("unbound Forget: %v", err)
	}
	if err := unbound.Rename("a.txt", "c.txt"); err != nil {
		t.Errorf("unbound Rename: %v", err)
	}

	if _, err := unbound.Bind("acct-1"); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if id, ok := lookup(t, unbound, "a.txt"); !ok || id != "id-a" {
		t.Fatalf("after binding, Lookup = %q, %v; want id-a — the unbound writes should not have disturbed the store", id, ok)
	}
	if _, ok := lookup(t, unbound, "b.txt"); ok {
		t.Error("a write made before Bind was persisted")
	}
}

// SetMany writes a batch in one transaction and still evicts the stale halves a
// rewrite leaves behind — the enumeration sweep (M7b) relies on both: on the
// batching for speed, and on the eviction for the two directions staying exact
// inverses.
func TestSetManyBatchesAndEvictsStaleHalves(t *testing.T) {
	s := newStore(t, "acct-1")

	if err := s.SetMany([]string{"a.txt", "dir", "dir/b.txt"}, []string{"id-a", "id-dir", "id-b"}); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{"a.txt": "id-a", "dir": "id-dir", "dir/b.txt": "id-b"} {
		if got, ok, err := s.Lookup(path); err != nil || !ok || got != want {
			t.Fatalf("Lookup(%q) = %q, %v, %v; want %q", path, got, ok, err, want)
		}
		if back, ok, err := s.PathFor(want); err != nil || !ok || back != path {
			t.Fatalf("PathFor(%q) = %q, %v, %v; want %q", want, back, ok, err, path)
		}
	}

	// The object at a.txt was replaced remotely: the old ID must not survive in
	// the reverse direction, or the change feed would resolve it to a live path.
	if err := s.SetMany([]string{"a.txt"}, []string{"id-a2"}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.PathFor("id-a"); ok {
		t.Fatal("the displaced ID still resolves to a path")
	}
	if got, ok, _ := s.Lookup("a.txt"); !ok || got != "id-a2" {
		t.Fatalf("Lookup after replace = %q, %v; want id-a2", got, ok)
	}

	if err := s.SetMany([]string{"x"}, []string{"id-x", "id-y"}); err == nil {
		t.Fatal("mismatched batch lengths accepted")
	}
}

// Re-storing a mapping the store already holds must not commit. A bbolt commit
// costs two fdatasyncs and rewrites every page it touched whatever the value is,
// so storing a byte-identical value is as expensive as a real write — and the
// provider does exactly that on every index hit and on every Stat-before-Put.
//
// The count, not the content, is the assertion: the content was already correct
// before this guard existed, which is why the bug was invisible.
func TestSetSkipsTheCommitWhenNothingChanges(t *testing.T) {
	s := newStore(t, "acct|root")

	mustSet(t, s, "a/b.txt", "id-1")
	after := s.Commits()

	for i := 0; i < 5; i++ {
		mustSet(t, s, "a/b.txt", "id-1")
	}
	if got := s.Commits(); got != after {
		t.Errorf("re-storing an identical mapping committed %d time(s); want 0", got-after)
	}

	// A changed value must still commit, or the guard has broken the store.
	mustSet(t, s, "a/b.txt", "id-2")
	if got := s.Commits(); got != after+1 {
		t.Errorf("storing a new id committed %d times; want 1", got-after)
	}
	if id, ok := lookup(t, s, "a/b.txt"); !ok || id != "id-2" {
		t.Errorf("lookup = %q, %v; want id-2, true", id, ok)
	}
}

// A half-match is not a match. setLocked evicts the stale halves a rewrite leaves
// behind, so skipping the write when only one direction agrees would leave the
// two buckets as non-inverses — the state that later resolves an ID to a path it
// no longer occupies.
func TestSetCommitsWhenOnlyOneDirectionAgrees(t *testing.T) {
	s := newStore(t, "acct|root")

	// old/p -> id-1 both ways. Now claim id-1 for a different path: byID agrees
	// with neither, so the write must happen and must vacate old/p.
	mustSet(t, s, "old/p.txt", "id-1")
	before := s.Commits()
	mustSet(t, s, "new/p.txt", "id-1")
	if got := s.Commits(); got != before+1 {
		t.Fatalf("re-pointing an id committed %d times; want 1", got-before)
	}
	if _, ok := lookup(t, s, "old/p.txt"); ok {
		t.Error("old path still resolves; the stale half was not evicted")
	}
	if p, ok := pathFor(t, s, "id-1"); !ok || p != "new/p.txt" {
		t.Errorf("reverse lookup = %q, %v; want new/p.txt, true", p, ok)
	}
}

// SetMany is the sweep's batch, and a periodic re-enumeration of an unchanged
// tree re-offers every mapping it already stored. That must cost nothing.
func TestSetManySkipsTheCommitWhenNothingChanges(t *testing.T) {
	s := newStore(t, "acct|root")
	paths := []string{"d/1.txt", "d/2.txt", "d/3.txt"}
	ids := []string{"id-1", "id-2", "id-3"}

	if err := s.SetMany(paths, ids); err != nil {
		t.Fatalf("setmany: %v", err)
	}
	after := s.Commits()

	if err := s.SetMany(paths, ids); err != nil {
		t.Fatalf("setmany again: %v", err)
	}
	if got := s.Commits(); got != after {
		t.Errorf("re-offering an unchanged page committed %d time(s); want 0", got-after)
	}

	// One changed entry in the page must still commit the page.
	ids[1] = "id-2b"
	if err := s.SetMany(paths, ids); err != nil {
		t.Fatalf("setmany changed: %v", err)
	}
	if got := s.Commits(); got != after+1 {
		t.Errorf("a page with one change committed %d times; want 1", got-after)
	}
	if id, ok := lookup(t, s, "d/2.txt"); !ok || id != "id-2b" {
		t.Errorf("lookup = %q, %v; want id-2b, true", id, ok)
	}
}

// An index bbolt cannot read is replaced rather than left disabled. The caller's
// fallback — run from memory — is correct but permanent, so one bad write would
// otherwise cost every later session a cold index.
func TestOpenReplacesAnUnreadableFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	if err := os.WriteFile(path, []byte("not a bolt database"), 0o600); err != nil {
		t.Fatal(err)
	}

	s := openAt(t, path)
	if s.Recovered() != path+CorruptSuffix {
		t.Errorf("Recovered() = %q; want %q", s.Recovered(), path+CorruptSuffix)
	}
	if _, err := os.Stat(path + CorruptSuffix); err != nil {
		t.Errorf("the corrupt file was not kept aside: %v", err)
	}

	// The replacement is a working index.
	if _, err := s.Bind("acct|root"); err != nil {
		t.Fatalf("bind after replacement: %v", err)
	}
	mustSet(t, s, "a.txt", "id-1")
	if id, ok := lookup(t, s, "a.txt"); !ok || id != "id-1" {
		t.Errorf("lookup = %q, %v; want id-1, true", id, ok)
	}
}

// A lock held by another process is not corruption, and replacing that file would
// pull the index out from under a live mount.
func TestOpenDoesNotReplaceALockedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	first := openAt(t, path)
	if _, err := first.Bind("acct|root"); err != nil {
		t.Fatal(err)
	}
	mustSet(t, first, "live.txt", "id-live")

	second, err := Open(path)
	if err == nil {
		_ = second.Close()
		t.Fatal("a second Open on a locked index succeeded; it must not")
	}
	if _, serr := os.Stat(path + CorruptSuffix); serr == nil {
		t.Fatal("a locked index was moved aside; that discards a live mount's cache")
	}
	if id, ok := lookup(t, first, "live.txt"); !ok || id != "id-live" {
		t.Errorf("the live index lost its entry: %q, %v", id, ok)
	}
}

// Re-binding an index to the identity it already carries must not commit. It is
// once per mount rather than once per file, so the saving is small — but a write
// transaction that changes nothing still costs two fdatasyncs, and the rule is the
// same one Set follows.
func TestBindSkipsTheCommitWhenTheIdentityMatches(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	s := openAt(t, path)

	if reused, err := s.Bind("acct|root"); err != nil || reused {
		t.Fatalf("first bind: reused=%v err=%v; want false, nil", reused, err)
	}
	mustSet(t, s, "a.txt", "id-1")
	after := s.Commits()

	// Same identity again: nothing to write.
	if reused, err := s.Bind("acct|root"); err != nil || !reused {
		t.Fatalf("re-bind: reused=%v err=%v; want true, nil", reused, err)
	}
	if got := s.Commits(); got != after {
		t.Errorf("re-binding the same identity committed %d time(s); want 0", got-after)
	}
	// And the fast path must leave the store usable and its contents intact.
	if id, ok := lookup(t, s, "a.txt"); !ok || id != "id-1" {
		t.Errorf("lookup after re-bind = %q, %v; want id-1, true", id, ok)
	}

	// A different identity still wipes and commits.
	if reused, err := s.Bind("other|root"); err != nil || reused {
		t.Fatalf("rebind to a new identity: reused=%v err=%v; want false, nil", reused, err)
	}
	if _, ok := lookup(t, s, "a.txt"); ok {
		t.Error("an identity change did not wipe the store")
	}
}
