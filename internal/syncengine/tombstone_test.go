// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package syncengine

import (
	"bytes"
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/syshlted/drivel/internal/fsevent"
	"github.com/syshlted/drivel/internal/state"
	"github.com/syshlted/drivel/provider"
)

// A client's own rm -rf, partially undone by its own change feed.
//
// Found live on 2026-10-16 (MC-23, scripts/multiclient/mc23-selfresurrect.sh),
// in 1 of 8 seed-and-delete cycles — the one cycle whose poll landed inside the
// window. The trace is three lines:
//
//	[sync] delete sub1/e20.txt
//	[pull] download sub1/e20.txt      (3s later)
//	[pull] delete sub1/e20.txt        (5s after that)
//
// Drive's trashed flag covers a trashed parent's whole subtree but propagates
// asynchronously, so the page still reported the child as present — and the
// delete push had already cleared that path's echo, so §4's loop breaker had
// nothing to match and the change read as an ordinary remote create. The file
// heals on the next page. The directory the download re-created does not: it is
// permanent at -sweep-interval 0, and with a sweep it is worse, because a
// local-only directory with no baseline is the local walk's "new locally" row,
// so the next sweep pushes it back and re-creates remotely a directory the user
// deleted.
//
// The fix keeps the baseline past the deletion instead of dropping it with it;
// see state.Tombstone and suppressDeleted.

// hashedStore is a fakeStore that reports a content digest for what it stored,
// as every shipped provider does (Drive an md5, SFTP an mtime:size version).
// That matters here because the engine records what the store tells it: a store
// reporting neither hash nor version produces a baseline with no identity, which
// can recognise nothing and is deliberately not entombed.
type hashedStore struct{ *fakeStore }

func (h hashedStore) Put(ctx context.Context, p string, r io.Reader) (provider.RemoteFile, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return provider.RemoteFile{}, err
	}
	rf, err := h.fakeStore.Put(ctx, p, bytes.NewReader(b))
	if err != nil {
		return rf, err
	}
	h.mu.Lock()
	h.content[p] = b
	h.mu.Unlock()
	rf.Hash, rf.Version = md5hex(string(b)), "1"
	return rf, nil
}

// deleteFixture is one mount's two halves over one state store: the engine that
// pushes the deletion and the pull loop that then receives the provider's stale
// view of it. Both are needed, because either half alone passes.
type deleteFixture struct {
	dir   string
	st    *state.Store
	store hashedStore
	e     *Engine
	d     *Downloader
	logs  *capture
}

func newDeleteFixture(t *testing.T) *deleteFixture {
	t.Helper()
	f := &deleteFixture{dir: t.TempDir(), st: newState(t), logs: &capture{}}
	f.store = hashedStore{newFakeStore()}
	lg := log.New(f.logs, "", 0)
	f.e = New(Config{Store: f.store, DataDir: f.dir, State: f.st, Logger: lg})
	f.d = NewDownloader(nil, f.store, f.dir, f.st, DefaultCadence).Logger(lg)
	return f
}

// seedAndDelete syncs sub/rel and then removes the subtree the way the mount
// reports it: the file first, then its directory.
func (f *deleteFixture) seedAndDelete(t *testing.T, dir, rel, content string) {
	t.Helper()
	ctx := context.Background()
	writeFile(t, f.dir, rel, content)
	f.e.handle(ctx, fsevent.Event{Op: fsevent.OpMkdir, Path: dir})
	f.e.handle(ctx, fsevent.Event{Op: fsevent.OpCreate, Path: rel})
	if err := os.RemoveAll(filepath.Join(f.dir, filepath.FromSlash(dir))); err != nil {
		t.Fatal(err)
	}
	f.e.handle(ctx, fsevent.Event{Op: fsevent.OpUnlink, Path: rel})
	f.e.handle(ctx, fsevent.Event{Op: fsevent.OpRmdir, Path: dir})
}

func (f *deleteFixture) exists(rel string) bool {
	_, err := os.Lstat(filepath.Join(f.dir, filepath.FromSlash(rel)))
	return err == nil
}

// THE regression test. Without the tombstone the file is downloaded back and
// takes its directory with it.
func TestAStaleReportOfOurOwnDeleteIsNotApplied(t *testing.T) {
	f := newDeleteFixture(t)
	f.seedAndDelete(t, "sub1", "sub1/e20.txt", "twenty")

	// The page Drive served three seconds later: the child still reads
	// trashed = false, with exactly the content the deletion removed.
	err := f.d.apply(context.Background(), provider.RemoteChange{
		Path: "sub1/e20.txt", File: remoteFile("sub1/e20.txt", "twenty"),
	})
	if err != nil {
		t.Fatal(err)
	}

	if f.exists("sub1/e20.txt") {
		t.Error("the deleted file came back")
	}
	// The half that does not heal. Nothing reported this directory at all — the
	// download created it as the parent of the file it was writing.
	if f.exists("sub1") {
		t.Error("the deleted directory came back; the next sweep would push it to every other client")
	}
	if n := f.logs.count("[pull] download"); n != 0 {
		t.Errorf("downloaded %d time(s) after our own delete\n%s", n, strings.Join(f.logs.lines(), "\n"))
	}
	if n := f.logs.count("[pull] ignore  sub1/e20.txt"); n != 1 {
		t.Errorf("the suppression is not reported exactly once\n%s", strings.Join(f.logs.lines(), "\n"))
	}
}

// The narrowing that keeps this from swallowing real work: a report carrying
// content we never had at that path is somebody else's write, and is applied.
func TestARecreationWithDifferentContentIsApplied(t *testing.T) {
	f := newDeleteFixture(t)
	f.seedAndDelete(t, "sub1", "sub1/e20.txt", "twenty")

	f.store.content["sub1/e20.txt"] = []byte("somebody else's bytes")
	err := f.d.apply(context.Background(), provider.RemoteChange{
		Path: "sub1/e20.txt", File: remoteFile("sub1/e20.txt", "somebody else's bytes"),
	})
	if err != nil {
		t.Fatal(err)
	}

	got, ok := readBacking(t, f.dir, "sub1/e20.txt")
	if !ok || got != "somebody else's bytes" {
		t.Fatalf("content = %q ok=%v; a genuine remote create at a deleted path must land", got, ok)
	}
}

// And the outer bound: a provider that has not caught up within the window is
// not what this describes, so the report is treated as news.
func TestAStaleReportAfterTheWindowIsApplied(t *testing.T) {
	f := newDeleteFixture(t)
	f.seedAndDelete(t, "sub1", "sub1/e20.txt", "twenty")

	tomb, ok, err := f.st.Tombstone("sub1/e20.txt")
	if err != nil || !ok {
		t.Fatalf("no tombstone after the delete push (ok=%v, err=%v)", ok, err)
	}
	tomb.Deleted = time.Now().Add(-2 * tombstoneWindow)
	if err := f.st.ForgetDeleted("sub1/e20.txt", tomb); err != nil {
		t.Fatal(err)
	}

	err = f.d.apply(context.Background(), provider.RemoteChange{
		Path: "sub1/e20.txt", File: remoteFile("sub1/e20.txt", "twenty"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !f.exists("sub1/e20.txt") {
		t.Error("a report long after the deletion was suppressed; the window is the whole bound on that")
	}
}

// A baseline with no identity is not entombed: it could recognise nothing, and
// writing it would spend a record to say nothing. A directory on a provider that
// reports no version for one is the real case.
func TestADeleteWithNoIdentityLeavesNoTombstone(t *testing.T) {
	dir := t.TempDir()
	st := newState(t)
	// The plain fakeStore reports neither hash nor version.
	e := New(Config{Store: newFakeStore(), DataDir: dir, State: st})
	ctx := context.Background()

	writeFile(t, dir, "a.txt", "x")
	e.handle(ctx, fsevent.Event{Op: fsevent.OpCreate, Path: "a.txt"})
	e.handle(ctx, fsevent.Event{Op: fsevent.OpUnlink, Path: "a.txt"})

	if _, ok, _ := st.Tombstone("a.txt"); ok {
		t.Error("stored a tombstone that can never match a report")
	}
	if _, ok, _ := st.GetEcho("a.txt"); ok {
		t.Error("the delete left its baseline behind")
	}
}

// The provider agreeing that the object is gone retires the record. Keeping it
// would cost space for a report that can no longer arrive.
func TestAConfirmedRemovalClearsTheTombstone(t *testing.T) {
	f := newDeleteFixture(t)
	f.seedAndDelete(t, "sub1", "sub1/e20.txt", "twenty")

	err := f.d.apply(context.Background(), provider.RemoteChange{Path: "sub1/e20.txt", Removed: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := f.st.Tombstone("sub1/e20.txt"); ok {
		t.Error("the tombstone survived the removal it was waiting for")
	}
}

// Re-creating the path locally clears it too, so the push that follows is not
// suppressed when it comes back round the feed.
func TestRecreatingThePathLocallyClearsTheTombstone(t *testing.T) {
	f := newDeleteFixture(t)
	f.seedAndDelete(t, "sub1", "sub1/e20.txt", "twenty")

	ctx := context.Background()
	writeFile(t, f.dir, "sub1/e20.txt", "twenty")
	f.e.handle(ctx, fsevent.Event{Op: fsevent.OpCreate, Path: "sub1/e20.txt"})

	if _, ok, _ := f.st.Tombstone("sub1/e20.txt"); ok {
		t.Fatal("the tombstone survived a re-creation of that path")
	}
	// And the echo it was replaced by does the ordinary §4 job.
	err := f.d.apply(ctx, provider.RemoteChange{
		Path: "sub1/e20.txt", File: remoteFile("sub1/e20.txt", "twenty"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := readBacking(t, f.dir, "sub1/e20.txt"); !ok || got != "twenty" {
		t.Fatalf("content = %q ok=%v; the re-created file must survive its own echo", got, ok)
	}
}

// The sweep asks the same question, because a listing can be taken inside the
// same window — and it reports what it declined to do, rather than counting it
// as a file it reconciled.
func TestTheSweepIgnoresItsOwnUnpropagatedDelete(t *testing.T) {
	f := newSweepFixture(t, ReconcileOptions{Fetch: true},
		[]provider.RemoteFile{remote("sub/a.txt", "alpha")})
	f.store.content["sub/a.txt"] = []byte("alpha")
	captureLogs(f)
	tomb := state.Tombstone{Echo: state.Echo{Hash: md5hex("alpha"), Version: "1"}, Deleted: time.Now()}
	if err := f.st.ForgetDeleted("sub/a.txt", tomb); err != nil {
		t.Fatal(err)
	}

	f.run(t)

	if f.exists("sub/a.txt") {
		t.Error("the sweep materialised an object this mount had just deleted")
	}
	if n := f.logs.count("1 listed object(s) ignored as our own un-propagated delete"); n != 1 {
		t.Errorf("the sweep summary does not report the suppression\n%s", strings.Join(f.logs.lines(), "\n"))
	}
}

// And the sweep is where the records are retired, so a mount that deletes a lot
// does not carry one per deleted path for ever.
func TestTheSweepPrunesExpiredTombstones(t *testing.T) {
	f := newSweepFixture(t, ReconcileOptions{Fetch: true},
		[]provider.RemoteFile{remote("kept.txt", "k")})
	f.store.content["kept.txt"] = []byte("k")
	captureLogs(f)
	old := state.Tombstone{Echo: state.Echo{Hash: md5hex("gone")}, Deleted: time.Now().Add(-2 * tombstoneWindow)}
	if err := f.st.ForgetDeleted("expired.txt", old); err != nil {
		t.Fatal(err)
	}
	fresh := state.Tombstone{Echo: state.Echo{Hash: md5hex("recent")}, Deleted: time.Now()}
	if err := f.st.ForgetDeleted("recent.txt", fresh); err != nil {
		t.Fatal(err)
	}

	f.run(t)

	if _, ok, _ := f.st.Tombstone("expired.txt"); ok {
		t.Error("an expired tombstone survived the sweep")
	}
	if _, ok, _ := f.st.Tombstone("recent.txt"); !ok {
		t.Error("a live tombstone was pruned by the sweep")
	}
	if n := f.logs.count("1 expired deletion record(s) dropped"); n != 1 {
		t.Errorf("the sweep summary does not report the prune\n%s", strings.Join(f.logs.lines(), "\n"))
	}
}
