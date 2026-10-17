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

	"github.com/syshlted/drivel/internal/fsevent"
	"github.com/syshlted/drivel/internal/state"
	"github.com/syshlted/drivel/provider"
)

// A directory rename, from both sides.
//
// MC-27, live against real Drive at 1100 files: the renaming client pays one
// request, and every other client ends up holding the subtree twice and cannot
// repair itself. Drive's feed reports the folder under its new name and nothing
// at all for its children, so a receiving client used to see an empty new
// directory appear and keep the whole old one — 1100 orphans with baselines,
// which the next sweep offers as 1101 deletions, which -max-deletes refuses.
// Two correct guards composing into a stuck state.
//
// The fix is to say that the folder MOVED, and to carry the records across with
// it on both sides: provider.RemoteChange.MovedFrom, Downloader.moveLocally and
// state.RenameSubtree. The old behaviour is still what any client gets when the
// provider cannot say, or when the local tree is not in a state where a move can
// be performed, so every decline below is a fall back to it and not to a new one.

// dirEntry is a listing entry for a directory. Directories carry a constant
// version rather than an empty one, as a real provider must (sftp rule 13).
func dirEntry(path string) provider.RemoteFile {
	return provider.RemoteFile{Path: path, IsDir: true, Version: "dir"}
}

// syncedUnder records a baseline for a path the way a completed sync does, at a
// time before any sweep in these tests starts.
func syncedUnder(t *testing.T, st *state.Store, rel, content string, dir string) {
	t.Helper()
	e := state.Echo{Hash: md5hex(content), Version: "1", At: time.Now().Add(-time.Hour)}
	if dir != "" {
		e = state.Echo{Version: "dir", At: time.Now().Add(-time.Hour)}
	}
	if err := st.SetEcho(rel, e); err != nil {
		t.Fatal(err)
	}
}

// movedFixture is a client that holds wide/ synced and is about to be told the
// folder is called narrow/ now. The listing is the post-move remote tree, so the
// sweep that runs afterwards is the one a real client would run next.
//
// -max-deletes is 1 deliberately: two stale baselines is already over it, which
// is MC-27's composition in miniature. A cap big enough to absorb them would
// make this test pass against the unfixed code, slowly.
func movedFixture(t *testing.T) *sweepFixture {
	t.Helper()
	f := newSweepFixture(t, ReconcileOptions{Fetch: true, MaxDeletes: 1},
		[]provider.RemoteFile{
			dirEntry("narrow"),
			remote("narrow/a.txt", "alpha"),
			remote("narrow/b.txt", "beta"),
		},
	)
	f.logs = &capture{}
	f.dl = f.dl.Logger(log.New(f.logs, "", 0))

	writeFile(t, f.dir, "wide/a.txt", "alpha")
	writeFile(t, f.dir, "wide/b.txt", "beta")
	f.store.content["narrow/a.txt"] = []byte("alpha")
	f.store.content["narrow/b.txt"] = []byte("beta")
	syncedUnder(t, f.st, "wide", "", "dir")
	syncedUnder(t, f.st, "wide/a.txt", "alpha", "")
	syncedUnder(t, f.st, "wide/b.txt", "beta", "")
	return f
}

func (f *sweepFixture) read(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.dir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// THE regression test, and it asserts the whole composition rather than the
// rename alone: no bytes move, the subtree is not doubled, and the sweep that
// follows neither downloads anything nor proposes a single deletion. Against the
// unfixed code every one of those four fails at once.
func TestAFolderMoveRenamesTheSubtreeInsteadOfDoublingIt(t *testing.T) {
	f := movedFixture(t)

	err := f.dl.apply(context.Background(), provider.RemoteChange{
		Path: "narrow", File: ptr(dirEntry("narrow")), MovedFrom: "wide",
	})
	if err != nil {
		t.Fatal(err)
	}

	if f.exists("wide") {
		t.Error("the old directory is still here; its files are now orphans present nowhere remotely")
	}
	if got := f.read(t, "narrow/a.txt"); got != "alpha" {
		t.Errorf("narrow/a.txt = %q, want %q", got, "alpha")
	}
	if got := f.read(t, "narrow/b.txt"); got != "beta" {
		t.Errorf("narrow/b.txt = %q, want %q", got, "beta")
	}
	if n := f.logs.count("[pull] download"); n != 0 {
		t.Errorf("downloaded %d file(s) for a move\n%s", n, strings.Join(f.logs.lines(), "\n"))
	}
	if n := f.logs.count("[pull] move    wide -> narrow"); n != 1 {
		t.Errorf("the move is not reported exactly once\n%s", strings.Join(f.logs.lines(), "\n"))
	}

	// The half MC-27 found: the sweep is what used to turn this into a permanent
	// doubling, so it has to run inside the test rather than be reasoned about.
	f.run(t)
	if n := f.logs.count("[pull] download"); n != 0 {
		t.Errorf("the sweep re-downloaded %d file(s)\n%s", n, strings.Join(f.logs.lines(), "\n"))
	}
	if n := f.logs.count("REFUSING to delete"); n != 0 {
		t.Errorf("the sweep refused its delete pass; MC-27's stuck state survives\n%s", strings.Join(f.logs.lines(), "\n"))
	}
	if n := f.logs.count("[sweep] delete"); n != 0 {
		t.Errorf("the sweep deleted %d path(s); nothing was deleted anywhere\n%s", n, strings.Join(f.logs.lines(), "\n"))
	}
	if ops := f.push.ops(); len(ops) != 0 {
		t.Errorf("the sweep pushed %v; the move is not local work", ops)
	}
}

// An occupied destination declines, and the content that is already there is
// untouched. os.Rename would replace a file silently, and what it replaced might
// be local work nobody has pushed — §6's question, not a move's.
func TestAMoveOntoAnOccupiedPathIsDeclined(t *testing.T) {
	f := movedFixture(t)
	writeFile(t, f.dir, "narrow/mine.txt", "local work")

	err := f.dl.apply(context.Background(), provider.RemoteChange{
		Path: "narrow", File: ptr(dirEntry("narrow")), MovedFrom: "wide",
	})
	if err != nil {
		t.Fatal(err)
	}

	if got := f.read(t, "narrow/mine.txt"); got != "local work" {
		t.Errorf("narrow/mine.txt = %q; the move overwrote local content", got)
	}
	if !f.exists("wide/a.txt") {
		t.Error("the source subtree was taken apart by a move that could not complete")
	}
	if n := f.logs.count("the destination already exists locally"); n != 1 {
		t.Errorf("the decline is not reported\n%s", strings.Join(f.logs.lines(), "\n"))
	}
}

// A MovedFrom that climbs out of the backing tree is refused and reported, and
// nothing is renamed. The subtree is the blast radius here, which is why this is
// checked above the seam at all (M9 rule 8 — a plugin is trusted code, and this
// is still not a reason to act on an answer that cannot be right).
func TestAMoveOutOfTheBackingTreeIsRefused(t *testing.T) {
	for _, bad := range []string{"../escape", "/etc", "..", "a/../../b", "a//b"} {
		t.Run(bad, func(t *testing.T) {
			f := movedFixture(t)
			err := f.dl.apply(context.Background(), provider.RemoteChange{
				Path: "narrow", File: ptr(dirEntry("narrow")), MovedFrom: bad,
			})
			if err != nil {
				t.Fatal(err)
			}
			if !f.exists("wide/a.txt") {
				t.Error("the source subtree moved on the strength of an unusable path")
			}
			if n := f.logs.count("not a path inside the mount"); n != 1 {
				t.Errorf("the refusal is not reported\n%s", strings.Join(f.logs.lines(), "\n"))
			}
		})
	}
}

// Nothing at the old path is the ordinary case on a client that never held the
// subtree, and on a re-report of a move already applied. It declines silently and
// the apply below does what it always did.
func TestAMoveWithNothingToMoveFallsThrough(t *testing.T) {
	f := movedFixture(t)
	err := f.dl.apply(context.Background(), provider.RemoteChange{
		Path: "narrow", File: ptr(dirEntry("narrow")), MovedFrom: "neverhadit",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !f.exists("narrow") {
		t.Error("the change was not applied after the move declined")
	}
	if n := f.logs.count("[pull] move"); n != 0 {
		t.Errorf("a move was reported for a path this client does not have\n%s", strings.Join(f.logs.lines(), "\n"))
	}
}

// Either path inside the other is not a move a filesystem performs, and it is
// the one shape where the record key ranges nest. Refused on both sides.
//
// The assertion is that nothing was ATTEMPTED, not merely that nothing happened:
// os.Rename answers EINVAL for the first shape and the occupied-destination
// check catches the second, so a test that only looked at the tree would pass
// whether or not this case were recognised at all. What distinguishes the two is
// the log — an attempt that failed says so.
func TestANestedMoveIsRefused(t *testing.T) {
	for _, c := range []struct{ from, to string }{{"wide", "wide/inner"}, {"wide/a.txt", "wide"}} {
		t.Run(c.from+"->"+c.to, func(t *testing.T) {
			f := movedFixture(t)
			err := f.dl.apply(context.Background(), provider.RemoteChange{
				Path: c.to, File: ptr(dirEntry(c.to)), MovedFrom: c.from,
			})
			if err != nil {
				t.Fatal(err)
			}
			if !f.exists("wide/a.txt") {
				t.Errorf("a nested move was performed: %s -> %s", c.from, c.to)
			}
			for _, attempted := range []string{"[pull] move", "already exists locally"} {
				if n := f.logs.count(attempted); n != 0 {
					t.Errorf("a nested move reached the filesystem (%q)\n%s",
						attempted, strings.Join(f.logs.lines(), "\n"))
				}
			}
		})
	}
}

// The sending side. The renaming client has no inbound change to tell it where
// the subtree went, so if its own push does not carry the baselines across, its
// own next sweep finds one unseen baseline per file and proposes a deletion for
// each — the same refusal, on the client that performed the rename.
func TestAnOutboundDirectoryRenameCarriesTheBaselinesWithIt(t *testing.T) {
	dir := t.TempDir()
	st := newState(t)
	store := newFakeStore()
	logs := &capture{}
	e := New(Config{Store: store, DataDir: dir, State: st, Logger: log.New(logs, "", 0)})

	writeFile(t, dir, "wide/a.txt", "alpha")
	writeFile(t, dir, "wide/b.txt", "beta")
	ctx := context.Background()
	e.handle(ctx, fsevent.Event{Op: fsevent.OpMkdir, Path: "wide"})
	e.handle(ctx, fsevent.Event{Op: fsevent.OpCreate, Path: "wide/a.txt"})
	e.handle(ctx, fsevent.Event{Op: fsevent.OpCreate, Path: "wide/b.txt"})

	if err := os.Rename(filepath.Join(dir, "wide"), filepath.Join(dir, "narrow")); err != nil {
		t.Fatal(err)
	}
	e.handle(ctx, fsevent.Event{Op: fsevent.OpRename, Path: "wide", NewPath: "narrow"})

	for _, rel := range []string{"narrow/a.txt", "narrow/b.txt"} {
		if _, ok, err := st.GetEcho(rel); err != nil || !ok {
			t.Errorf("no baseline at %s after the rename; the next sweep reads it as a deletion", rel)
		}
	}
	for _, rel := range []string{"wide/a.txt", "wide/b.txt"} {
		if _, ok, _ := st.GetEcho(rel); ok {
			t.Errorf("a baseline survives at %s, which no longer exists on either side", rel)
		}
	}
}

// ptr is the address of a listing entry, for the File field of a change.
func ptr(f provider.RemoteFile) *provider.RemoteFile { return &f }
