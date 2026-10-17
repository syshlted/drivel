// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package syncengine

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/syshlted/drivel/internal/claim"
	"github.com/syshlted/drivel/internal/state"
	"github.com/syshlted/drivel/provider"
)

// M27 part 4: a push LOST is not a push abandoned.
//
// Parts 2 and 3 record a path when the engine gives up, which is code that runs.
// kill -9 runs none, and neither does a power cut, an OOM kill or a shutdown
// drain that timed out — so the repair bucket is empty and every other row of
// the reconcile table correctly declines: the remote still holds the content the
// baseline describes, and the walk's "new locally" row skips anything that has a
// baseline at all. MC-40 (b) measured that as 44 sweeps over 7 minutes changing
// nothing, which made a crash strictly worse than an outage.
//
// The row that closes it is pushLocalOnly/repushDiverged, and the evidence it
// uses was already being stored: state.Echo's local size and mtime, written for
// M18's hashless providers and read by the delete rows. See repushDiverged for
// the four things that narrow it; one test each below.

// synced puts content on both sides and records the baseline a successful push
// leaves behind, fingerprint included. The echo predates the sweep, as any real
// one does.
func synced(t *testing.T, f *sweepFixture, rel, content string) {
	t.Helper()
	writeFile(t, f.dir, rel, content)
	f.store.content[rel] = []byte(content)
	f.store.seen[rel] = true
	fi, err := os.Stat(filepath.Join(f.dir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.SetEcho(rel, state.Echo{
		Hash: md5hex(content), Version: "1", At: time.Now().Add(-time.Hour),
		LocalSize: fi.Size(), LocalMTime: fi.ModTime(),
	}); err != nil {
		t.Fatal(err)
	}
}

// lostWrite rewrites rel the way a crash leaves it: new bytes on disk, a baseline
// still describing the old ones, nothing recorded anywhere — and an mtime from
// before this mount opened, which is the whole point. A write this process
// received would be one the engine is still answerable for.
func lostWrite(t *testing.T, f *sweepFixture, rel, content string) {
	t.Helper()
	writeFile(t, f.dir, rel, content)
	stamp := f.dl.openedAt.Add(-time.Minute)
	if err := os.Chtimes(filepath.Join(f.dir, filepath.FromSlash(rel)), stamp, stamp); err != nil {
		t.Fatal(err)
	}
}

// liveWrite rewrites rel the way the running mount sees it: an mtime after this
// mount opened, which is what makes the write the engine's to finish. Stamped
// rather than left to the clock, because a test that writes microseconds after
// openedAt is asking the backing filesystem's timestamp granularity to decide
// which side of the boundary it falls on.
func liveWrite(t *testing.T, f *sweepFixture, rel, content string) {
	t.Helper()
	writeFile(t, f.dir, rel, content)
	stamp := f.dl.openedAt.Add(time.Minute)
	if err := os.Chtimes(filepath.Join(f.dir, filepath.FromSlash(rel)), stamp, stamp); err != nil {
		t.Fatal(err)
	}
}

// captureLogs attaches a log capture to a fixture built by newSweepFixture,
// keeping the downloader it already has (and therefore its openedAt).
func captureLogs(f *sweepFixture) {
	f.logs = &capture{}
	f.dl = f.dl.Logger(log.New(f.logs, "", 0))
}

// THE regression test. Remove the row and this fails with pushes = [].
func TestSweepRepushesAWriteLostBeforeThisMountOpened(t *testing.T) {
	f := newSweepFixture(t, ReconcileOptions{Fetch: true},
		[]provider.RemoteFile{remote("a.txt", "old")})
	captureLogs(f)
	synced(t, f, "a.txt", "old")
	lostWrite(t, f, "a.txt", "new and never sent")

	f.run(t)

	if got, want := f.push.ops(), []string{"write a.txt"}; !equal(got, want) {
		t.Fatalf("pushes = %v; want %v — the lost write was never re-sent", got, want)
	}
	if n := f.logs.count("[sweep] repush  a.txt"); n != 1 {
		t.Errorf("repush lines = %d; want 1\n%s", n, strings.Join(f.logs.lines(), "\n"))
	}
	if n := f.logs.count("1 re-pushed after diverging from its baseline"); n != 1 {
		t.Errorf("the sweep summary does not report the re-push\n%s", strings.Join(f.logs.lines(), "\n"))
	}
}

// The ordinary case, and the one that decides whether this row is affordable: a
// local copy that still matches its baseline is not pushed. Otherwise every
// sweep would re-upload the whole tree.
func TestSweepDoesNotRepushAFileThatMatchesItsBaseline(t *testing.T) {
	f := newSweepFixture(t, ReconcileOptions{Fetch: true},
		[]provider.RemoteFile{remote("a.txt", "old"), remote("sub/b.txt", "beta")})
	synced(t, f, "a.txt", "old")
	synced(t, f, "sub/b.txt", "beta")

	f.run(t)

	if got := f.push.ops(); len(got) != 0 {
		t.Fatalf("pushes = %v; want none — nothing diverged", got)
	}
}

// The boundary. A file modified since this mount opened belongs to the running
// engine: it is in the debounce map, a worker queue, in flight, or in the repair
// bucket. Pushing it here would race the engine's own push, and on a backend
// whose sweep interval is its poll interval it would do so every few seconds for
// as long as a large copy takes.
func TestSweepLeavesAWriteThisMountReceivedToTheEngine(t *testing.T) {
	f := newSweepFixture(t, ReconcileOptions{Fetch: true},
		[]provider.RemoteFile{remote("a.txt", "old")})
	synced(t, f, "a.txt", "old")
	// Modified after this mount opened: a copy still in progress, or a close the
	// engine has not finished pushing yet.
	liveWrite(t, f, "a.txt", "being written right now")

	f.run(t)

	if got := f.push.ops(); len(got) != 0 {
		t.Fatalf("pushes = %v; want none — the engine owns a write this process received", got)
	}
}

// A baseline with no local fingerprint is no evidence of divergence, and must not
// be read as evidence of agreement either: it is counted and skipped. Guessing
// "diverged" here re-uploads every path in a state DB written before M18 added
// the field.
func TestSweepWillNotRepushAnUnfingerprintedBaseline(t *testing.T) {
	f := newSweepFixture(t, ReconcileOptions{Fetch: true},
		[]provider.RemoteFile{remote("a.txt", "old")})
	captureLogs(f)
	synced(t, f, "a.txt", "old")
	// As written before M18, or after a stat that failed.
	if err := f.st.SetEcho("a.txt", state.Echo{
		Hash: md5hex("old"), Version: "1", At: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	lostWrite(t, f, "a.txt", "new and never sent")

	f.run(t)

	if got := f.push.ops(); len(got) != 0 {
		t.Fatalf("pushes = %v; want none — the baseline cannot say what the local copy was", got)
	}
	if n := f.logs.count("1 baseline(s) with no fingerprint to check"); n != 1 {
		t.Errorf("the sweep summary does not report the unjudgeable baseline\n%s",
			strings.Join(f.logs.lines(), "\n"))
	}
}

// A path the sweep could not settle is not a path to push over. Here the remote
// diverged too and the §6 conflict copy's download failed, so what the remote
// holds is unknown — and a whole-file Put over it would publish one side of a
// conflict while keeping no copy of the other.
func TestSweepDoesNotRepushAPathItCouldNotSettle(t *testing.T) {
	f := newSweepFixture(t, ReconcileOptions{Fetch: true},
		[]provider.RemoteFile{remote("a.txt", "theirs")})
	captureLogs(f)
	synced(t, f, "a.txt", "old")
	lostWrite(t, f, "a.txt", "mine and never sent")
	// The remote's content is unreachable, so the conflict copy cannot be fetched.
	delete(f.store.content, "a.txt")

	f.run(t)

	if got := f.push.ops(); len(got) != 0 {
		t.Fatalf("pushes = %v; want none while the remote side is unknown", got)
	}
	if n := f.logs.count("conflict"); n != 0 {
		t.Errorf("a conflict copy was reported despite the failed download\n%s",
			strings.Join(f.logs.lines(), "\n"))
	}
}

// One path, two rows that both want to re-send it: the owed repair wins and the
// walk stands down. Without that the path is pushed twice in one sweep — twice
// the attempts, twice the retry storm if the provider is the reason it failed.
func TestAnOwedRepairAndTheWalkDoNotBothPushOnePath(t *testing.T) {
	f := newSweepFixture(t, ReconcileOptions{Fetch: true},
		[]provider.RemoteFile{remote("a.txt", "old")})
	synced(t, f, "a.txt", "old")
	lostWrite(t, f, "a.txt", "new and never sent")
	if err := f.st.OweRepair("a.txt", time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}

	f.run(t)

	if got, want := f.push.ops(), []string{"write a.txt"}; !equal(got, want) {
		t.Fatalf("pushes = %v; want %v — exactly one re-send", got, want)
	}
}

// M5 stays in front of everything. A placeholder's size and mtime are the
// remote's, so a hydration that changed either would look like divergence — and
// pushing one uploads zeros over the file it stands for.
func TestSweepNeverRepushesAPlaceholder(t *testing.T) {
	f := newSweepFixture(t, ReconcileOptions{Fetch: true},
		[]provider.RemoteFile{remote("a.txt", "old")})
	synced(t, f, "a.txt", "old")
	lostWrite(t, f, "a.txt", "")
	f.dl = f.dl.Lazy(holeyMaterializer{})

	f.run(t)

	if got := f.push.ops(); len(got) != 0 {
		t.Fatalf("pushes = %v; a placeholder must never be pushed", got)
	}
}

// A baseline whose path the sweep did not list belongs to the delete pass, which
// has strictly more to go on. Here the remote lost the object and the local copy
// diverged, which is deleteGone's "keep and push it back" row — one push, from
// that pass, with the baseline dropped. The walk must not reach it first and
// leave the baseline describing content that exists nowhere.
func TestAnUnlistedPathIsTheDeletePassesAndNotTheWalks(t *testing.T) {
	f := newSweepFixture(t, ReconcileOptions{Fetch: true}, []provider.RemoteFile{})
	captureLogs(f)
	synced(t, f, "a.txt", "old")
	lostWrite(t, f, "a.txt", "new and never sent")

	f.run(t)

	if got, want := f.push.ops(), []string{"write a.txt"}; !equal(got, want) {
		t.Fatalf("pushes = %v; want %v", got, want)
	}
	if n := f.logs.count("[sweep] keep    a.txt"); n != 1 {
		t.Errorf("the delete pass did not claim the path\n%s", strings.Join(f.logs.lines(), "\n"))
	}
	if n := f.logs.count("[sweep] repush"); n != 0 {
		t.Errorf("the walk pushed a path the delete pass owns\n%s", strings.Join(f.logs.lines(), "\n"))
	}
	if _, ok, _ := f.st.GetEcho("a.txt"); ok {
		t.Error("the baseline survived a path that is gone remotely")
	}
}

// The comparison both rows share, stated against its inputs rather than against
// a fixture. "Not recorded" is neither a match nor a mismatch.
func TestFingerprintDiverged(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		fi   os.FileInfo
		echo state.Echo
		want bool
	}{
		{"same size and mtime", fakeInfo{size: 10, mtime: now}, state.Echo{LocalSize: 10, LocalMTime: now}, false},
		{"size moved", fakeInfo{size: 11, mtime: now}, state.Echo{LocalSize: 10, LocalMTime: now}, true},
		{"mtime moved", fakeInfo{size: 10, mtime: now.Add(time.Second)}, state.Echo{LocalSize: 10, LocalMTime: now}, true},
		{"no fingerprint", fakeInfo{size: 11, mtime: now}, state.Echo{}, false},
		{"empty file, recorded", fakeInfo{size: 0, mtime: now}, state.Echo{LocalSize: 0, LocalMTime: now}, false},
		{"directory", fakeInfo{size: 10, mtime: now, dir: true}, state.Echo{LocalSize: 1, LocalMTime: now}, false},
		{"no file", nil, state.Echo{LocalSize: 10, LocalMTime: now}, false},
	}
	for _, c := range cases {
		var fi os.FileInfo
		if c.fi != nil {
			fi = c.fi
		}
		if got := fingerprintDiverged(fi, c.echo); got != c.want {
			t.Errorf("%s: fingerprintDiverged = %v; want %v", c.name, got, c.want)
		}
	}
}

// fakeInfo is the two fields the comparison reads, plus the one it refuses on.
type fakeInfo struct {
	size  int64
	mtime time.Time
	dir   bool
}

func (f fakeInfo) Name() string       { return "f" }
func (f fakeInfo) Size() int64        { return f.size }
func (f fakeInfo) Mode() os.FileMode  { return 0o644 }
func (f fakeInfo) ModTime() time.Time { return f.mtime }
func (f fakeInfo) IsDir() bool        { return f.dir }
func (f fakeInfo) Sys() any           { return nil }

// enumPatchStore is the patchStore — a real ContentHasher, so M6's gates run —
// with an enumeration of whatever it currently holds. One page and no cursor:
// what is under test below is two sweeps in a row, not paging.
type enumPatchStore struct {
	*patchStore
	paths []string
}

func (s *enumPatchStore) Enumerate(context.Context, string) ([]provider.RemoteFile, string, error) {
	out := make([]provider.RemoteFile, 0, len(s.paths))
	for _, p := range s.paths {
		out = append(out, s.file(p))
	}
	return out, "", nil
}

// Both halves together, which is the only place the loop is visible.
//
// A metadata-only change to an already-synced file is divergence as far as the
// fingerprint can tell — and it must be, because the fingerprint is all a
// hashless provider has. The row therefore proposes the push, M6's gate 3 finds
// the remote already holding those bytes and sends nothing, and the baseline it
// records is what stops the next sweep proposing the same push again. Without
// the record this converges never: one wasted hash of the whole file per mount,
// forever, and a whole upload for anything under hashSkipMinSize.
func TestARepushedMetadataChangeSettlesAfterOneSweep(t *testing.T) {
	f := newSweepFixture(t, ReconcileOptions{Fetch: true, Force: true})
	store := &enumPatchStore{patchStore: newPatchStore(), paths: []string{"a.bin"}}
	f.opts.Push = New(Config{Store: store, DataDir: f.dir, State: f.st})
	f.useEnumerator(store)

	body := bigBody('a')
	local := filepath.Join(f.dir, "a.bin")
	if err := os.WriteFile(local, body, 0o644); err != nil {
		t.Fatal(err)
	}
	store.seed("a.bin", body)
	// The baseline a successful push left behind, fingerprinted at a different
	// mtime than the file now carries: the bytes agree, the metadata does not,
	// and all of it predates this mount.
	stamp := f.dl.openedAt.Add(-2 * time.Hour)
	if err := os.Chtimes(local, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if err := f.st.SetEcho("a.bin", state.Echo{
		Hash: store.file("a.bin").Hash, At: stamp,
		LocalSize: int64(len(body)), LocalMTime: stamp.Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	f.run(t)
	if n := f.logs.count("[sweep] repush  a.bin"); n != 1 {
		t.Fatalf("repush lines after the first sweep = %d; want 1\n%s", n, strings.Join(f.logs.lines(), "\n"))
	}
	if got := store.ops(); len(got) != 0 {
		t.Errorf("ops = %v; want none — the remote already held these bytes", got)
	}

	f.run(t)
	if n := f.logs.count("[sweep] repush  a.bin"); n != 1 {
		t.Errorf("the second sweep proposed the push again (%d repush lines in total): "+
			"the skipped push did not settle the baseline\n%s", n, strings.Join(f.logs.lines(), "\n"))
	}
	if got := store.ops(); len(got) != 0 {
		t.Errorf("ops = %v; want none across both sweeps", got)
	}
}

// MC-47 (b), the direction a refusal at open cannot catch: this mount started
// AFTER a mount whose backing store is nested inside its tree. The walk must
// leave that subtree alone — pushing it is what multiplied one file into a dozen
// at bdata/bdata/bdata/… — while still pushing this mount's own new files.
func TestTheWalkSkipsAnotherMountsBackingStore(t *testing.T) {
	f := newSweepFixture(t, ReconcileOptions{Fetch: true}, nil)
	captureLogs(f)
	writeFile(t, f.dir, "mine.txt", "ours")
	writeFile(t, f.dir, "bdata/theirs.txt", "belongs to the nested mount")
	writeFile(t, f.dir, "bdata/"+claim.Name, "drivel backing store for mount b\n")

	f.run(t)

	if got, want := f.push.ops(), []string{"write mine.txt"}; !equal(got, want) {
		t.Fatalf("pushes = %v; want only %v — the nested backing store was synced", got, want)
	}
	if f.logs.count("[sweep] skip    bdata/ (another drivel mount's backing store") != 1 {
		t.Errorf("the skip was not reported\n%s", strings.Join(f.logs.lines(), "\n"))
	}
}
