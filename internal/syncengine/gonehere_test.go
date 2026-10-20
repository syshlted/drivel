// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package syncengine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/syshlted/drivel/internal/fsevent"
	"github.com/syshlted/drivel/internal/state"
	"github.com/syshlted/drivel/provider"
)

// The "deleted locally" row of the three-way table: baseline present, local
// absent, remote present. DESIGN.md's table promised "delete remotely" for it
// from M7b, and nothing did it — the pull's apply returned on the echo match
// before it looked at the local side — so a deletion made while drivel was down,
// or one whose push was abandoned, left the remote copy standing for good.

// listedStore is an enumStore whose Stat and Remove agree with its listing, so
// the delete pass's re-check before a removal sees what the sweep saw unless a
// test changes it in between.
type listedStore struct {
	*enumStore
	mu      sync.Mutex
	remote  map[string]provider.RemoteFile
	removes []string
	failRm  map[string]error               // Remove(path) fails with this, once
	onPage  map[int]func()                 // run before handing out page N
	statAs  map[string]provider.RemoteFile // what Stat reports, overriding the listing
}

func newListedStore(pages ...[]provider.RemoteFile) *listedStore {
	s := &listedStore{
		enumStore: newEnumStore(nil, pages...),
		remote:    map[string]provider.RemoteFile{},
		failRm:    map[string]error{},
		onPage:    map[int]func(){},
		statAs:    map[string]provider.RemoteFile{},
	}
	for _, pg := range pages {
		for _, f := range pg {
			s.remote[f.Path] = f
		}
	}
	return s
}

func (s *listedStore) Enumerate(ctx context.Context, cursor string) ([]provider.RemoteFile, string, error) {
	i := 0
	if cursor != "" {
		fmt.Sscanf(cursor, "page-%d", &i)
	}
	if fn := s.onPage[i]; fn != nil {
		fn()
	}
	return s.enumStore.Enumerate(ctx, cursor)
}

func (s *listedStore) Stat(_ context.Context, p string) (provider.RemoteFile, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rf, ok := s.statAs[p]; ok {
		return rf, true, nil
	}
	rf, ok := s.remote[p]
	return rf, ok, nil
}

func (s *listedStore) Remove(_ context.Context, p string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.failRm[p]; err != nil {
		delete(s.failRm, p)
		return err
	}
	s.removes = append(s.removes, p)
	for q := range s.remote {
		if q == p || strings.HasPrefix(q, p+"/") {
			delete(s.remote, q)
		}
	}
	return nil
}

func (s *listedStore) removed() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.removes...)
}

// goneFixture is a sweep over a listedStore that pushes through a real engine,
// so a removal is entombed and a failed one is owed exactly as in a mount.
func goneFixture(t *testing.T, opts ReconcileOptions, pages ...[]provider.RemoteFile) (*sweepFixture, *listedStore) {
	t.Helper()
	opts.Force = true
	f := newSweepFixture(t, opts)
	ls := newListedStore(pages...)
	f.useEnumerator(ls)
	f.dl.rec.Push = New(Config{Store: ls, DataDir: f.dir, State: f.st})
	return f, ls
}

func seedDirBaseline(t *testing.T, st *state.Store, rel string) {
	t.Helper()
	if err := st.SetEcho(rel, state.Echo{Version: "dir", At: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
}

func remoteDir(rel string) provider.RemoteFile {
	return provider.RemoteFile{Path: rel, IsDir: true, Version: "dir"}
}

// The row itself: synced, then deleted here while drivel was not running. The
// remote copy goes, through the engine, and the baseline becomes a tombstone.
func TestSweepDeletesRemotelyWhatWasDeletedHere(t *testing.T) {
	f, ls := goneFixture(t, ReconcileOptions{Fetch: true},
		[]provider.RemoteFile{remote("gone.txt", "synced"), remote("kept.txt", "also synced")})
	seedBaseline(t, f.st, "gone.txt", "synced")
	seedBaseline(t, f.st, "kept.txt", "also synced")
	writeFile(t, f.dir, "kept.txt", "also synced")

	f.run(t)

	if got := ls.removed(); !equal(got, []string{"gone.txt"}) {
		t.Fatalf("remote removals = %v; want [gone.txt]", got)
	}
	if _, ok, _ := f.st.GetEcho("gone.txt"); ok {
		t.Fatal("the baseline of a path deleted on both sides survived")
	}
	if _, ok, _ := f.st.Tombstone("gone.txt"); !ok {
		t.Fatal("no tombstone: a stale report of the deleted file would be downloaded back")
	}
	if f.exists("gone.txt") {
		t.Fatal("the sweep re-created a file that was deleted here")
	}
	if !strings.Contains(f.logs.String(), "delete  gone.txt remotely") {
		t.Fatalf("the deletion was not logged:\n%s", f.logs)
	}
}

// An edit beats a deletion, in this direction as in the other: a remote that no
// longer matches the baseline is somebody's later work, so it is not deleted —
// it is applied here as the change it is.
func TestSweepKeepsARemoteEditOverALocalDeletion(t *testing.T) {
	f, ls := goneFixture(t, ReconcileOptions{Fetch: true},
		[]provider.RemoteFile{remote("doc.txt", "edited on the other machine")})
	ls.content["doc.txt"] = []byte("edited on the other machine")
	seedBaseline(t, f.st, "doc.txt", "as last synced")

	f.run(t)

	if got := ls.removed(); len(got) != 0 {
		t.Fatalf("remote removals = %v; want none — the remote changed after our baseline", got)
	}
	if !f.exists("doc.txt") {
		t.Fatal("the remote edit was not applied here")
	}
}

// The listing can be minutes old by the time the delete pass runs. The remote is
// asked again immediately before the removal, and an edit made in between keeps
// the file.
func TestSweepRechecksTheRemoteBeforeDeletingIt(t *testing.T) {
	f, ls := goneFixture(t, ReconcileOptions{Fetch: true},
		[]provider.RemoteFile{remote("doc.txt", "synced")})
	seedBaseline(t, f.st, "doc.txt", "synced")
	ls.statAs["doc.txt"] = remote("doc.txt", "edited after the listing")

	f.run(t)

	if got := ls.removed(); len(got) != 0 {
		t.Fatalf("remote removals = %v; want none — the remote moved after it was listed", got)
	}
	if !strings.Contains(f.logs.String(), "keep    doc.txt remotely") {
		t.Fatalf("the kept file was not reported:\n%s", f.logs)
	}
}

// Nor is a path deleted that came back locally between the listing and the
// delete pass: it is the live path's business by then.
func TestSweepDoesNotDeleteAPathRecreatedDuringTheSweep(t *testing.T) {
	f, ls := goneFixture(t, ReconcileOptions{Fetch: true},
		[]provider.RemoteFile{remote("doc.txt", "synced")},
		[]provider.RemoteFile{remote("other.txt", "x")})
	seedBaseline(t, f.st, "doc.txt", "synced")
	ls.content["other.txt"] = []byte("x")
	ls.onPage[1] = func() { writeFile(t, f.dir, "doc.txt", "made again") }

	f.run(t)

	if got := ls.removed(); len(got) != 0 {
		t.Fatalf("remote removals = %v; want none — the file is back locally", got)
	}
}

// The age rule applies here as it does to the other row: a baseline written
// after the sweep started is not evidence about anything the listing saw.
func TestSweepIgnoresAGoneBaselineWrittenDuringTheSweep(t *testing.T) {
	f, ls := goneFixture(t, ReconcileOptions{Fetch: true},
		[]provider.RemoteFile{remote("doc.txt", "synced")})
	if err := f.st.SetEcho("doc.txt", state.Echo{Hash: md5hex("synced"), Version: "1", At: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}

	f.run(t)

	if got := ls.removed(); len(got) != 0 {
		t.Fatalf("remote removals = %v; want none", got)
	}
}

// A directory goes once everything the sweep saw inside it has gone first —
// children before parent, and then the parent, so nothing is left behind.
func TestSweepDeletesADirectoryDeletedHere(t *testing.T) {
	f, ls := goneFixture(t, ReconcileOptions{Fetch: true},
		[]provider.RemoteFile{remoteDir("dir"), remote("dir/a.txt", "a"), remote("dir/sub/b.txt", "b"), remoteDir("dir/sub")})
	seedDirBaseline(t, f.st, "dir")
	seedDirBaseline(t, f.st, "dir/sub")
	seedBaseline(t, f.st, "dir/a.txt", "a")
	seedBaseline(t, f.st, "dir/sub/b.txt", "b")

	f.run(t)

	if got, want := ls.removed(), []string{"dir/sub/b.txt", "dir/a.txt", "dir/sub", "dir"}; !equal(got, want) {
		t.Fatalf("remote removals = %v; want %v (deepest first, the directory last)", got, want)
	}
}

// But a removal of a directory is recursive, so one holding anything this sweep
// saw and is not deleting — here a file somebody added remotely, which this
// eager mount has not fetched — stays, and only what was synced and deleted goes.
func TestSweepKeepsARemoteDirectoryHoldingWhatItNeverSynced(t *testing.T) {
	f, ls := goneFixture(t, ReconcileOptions{},
		[]provider.RemoteFile{remoteDir("dir"), remote("dir/a.txt", "a"), remote("dir/new.txt", "added remotely")})
	seedDirBaseline(t, f.st, "dir")
	seedBaseline(t, f.st, "dir/a.txt", "a")

	f.run(t)

	if got := ls.removed(); !equal(got, []string{"dir/a.txt"}) {
		t.Fatalf("remote removals = %v; want only [dir/a.txt]", got)
	}
	if _, ok := ls.remote["dir/new.txt"]; !ok {
		t.Fatal("a remote file this mount never saw was removed with its directory")
	}
	if !strings.Contains(f.logs.String(), "keep    dir remotely") {
		t.Fatalf("the kept directory was not reported:\n%s", f.logs)
	}
}

// Both rows draw on one -max-deletes count, and the refusal says which side each
// came from: a broken premise produces either one, and an empty backing
// directory against a full remote produces this one.
func TestGoneHereCountsTowardMaxDeletes(t *testing.T) {
	f, ls := goneFixture(t, ReconcileOptions{MaxDeletes: 3},
		[]provider.RemoteFile{remote("a.txt", "a"), remote("b.txt", "b")})
	seedBaseline(t, f.st, "a.txt", "a") // listed, gone here
	seedBaseline(t, f.st, "b.txt", "b") // listed, gone here
	for _, p := range []string{"c.txt", "d.txt"} {
		writeFile(t, f.dir, p, "body") // unlisted, present here
		seedBaseline(t, f.st, p, "body")
	}

	f.run(t)

	if got := ls.removed(); len(got) != 0 {
		t.Fatalf("remote removals = %v; want none — four deletions exceed a cap of three", got)
	}
	if !f.exists("c.txt") || !f.exists("d.txt") {
		t.Fatal("a local file was deleted by a refused pass")
	}
	if !strings.Contains(f.logs.String(), "4 previously-synced path(s) are gone from one side (2 from the remote, 2 from here)") {
		t.Fatalf("the refusal did not count both rows:\n%s", f.logs)
	}
}

// A path more than one remote object claims is not deleted: Remove would take
// the one standing for it and the next would come back in its place (MC-30).
func TestSweepDoesNotDeleteAPathSharedBySiblings(t *testing.T) {
	older := remote("doc.txt", "synced")
	older.Modified = time.Now().Add(-2 * time.Hour)
	newer := remote("doc.txt", "synced")
	newer.Modified = time.Now().Add(-time.Hour)
	f, ls := goneFixture(t, ReconcileOptions{}, []provider.RemoteFile{older, newer})
	seedBaseline(t, f.st, "doc.txt", "synced")

	f.run(t)

	if got := ls.removed(); len(got) != 0 {
		t.Fatalf("remote removals = %v; want none for a shared path", got)
	}
}

// The repair this row is for: a deletion whose push the engine gave up on. The
// path is owed, which makes a repair sweep fall due, and that sweep re-makes the
// deletion — where before nothing ever did, and the file stayed on the remote
// and on every other client.
func TestAnAbandonedDeletionIsRemadeByTheNextSweep(t *testing.T) {
	f, ls := goneFixture(t, ReconcileOptions{},
		[]provider.RemoteFile{remote("doc.txt", "synced")})
	seedBaseline(t, f.st, "doc.txt", "synced")
	ls.failRm["doc.txt"] = errors.New("permission denied")
	engine := f.dl.rec.Push.(*Engine)

	engine.handle(context.Background(), fsevent.Event{Op: fsevent.OpUnlink, Path: "doc.txt"})

	if owed := owedPaths(t, f.st); !equal(owed, []string{"doc.txt"}) {
		t.Fatalf("repair list after the failed deletion = %v; want [doc.txt]", owed)
	}
	if _, ok, _ := f.st.GetEcho("doc.txt"); !ok {
		t.Fatal("the failed deletion dropped the baseline the sweep needs to re-make it")
	}

	f.run(t)

	if got := ls.removed(); !equal(got, []string{"doc.txt"}) {
		t.Fatalf("remote removals after the sweep = %v; want [doc.txt]", got)
	}
	if owed := owedPaths(t, f.st); len(owed) != 0 {
		t.Fatalf("repair list after the sweep = %v; want empty", owed)
	}
	if pushes := strings.Count(f.logs.String(), "repush"); pushes != 0 {
		t.Fatalf("the repair pass re-sent something for a deletion:\n%s", f.logs)
	}
}

func owedPaths(t *testing.T, st *state.Store) []string {
	t.Helper()
	var out []string
	if err := st.EachRepair(func(p string, _ time.Time) error {
		out = append(out, p)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// String is everything captured so far, for a containment check.
func (c *capture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}
