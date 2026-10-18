// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package syncengine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/syshlted/drivel/internal/fsevent"
	"github.com/syshlted/drivel/internal/state"
	"github.com/syshlted/drivel/internal/trash"
	"github.com/syshlted/drivel/provider"
)

// The conflict policies (conflicts.go). Each case is one conflict met from one
// side; what varies is where the loser ends up.

// trashingStore is a patchStore with a trash, recording what it was handed.
type trashingStore struct {
	*patchStore
	trashed map[string][]byte
}

func newTrashingStore() *trashingStore {
	return &trashingStore{patchStore: newPatchStore(), trashed: map[string][]byte{}}
}

func (s *trashingStore) Trash(_ context.Context, p string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.content[p]
	if !ok {
		return fmt.Errorf("%q: %w", p, provider.ErrNotExist)
	}
	s.trashed[p] = b
	delete(s.content, p)
	return nil
}

// putFailStore refuses every Put to a path inside a conflicts folder, which is
// how a copy that cannot be published is driven.
type putFailStore struct{ *trashingStore }

func (s putFailStore) Put(ctx context.Context, p string, r io.Reader) (provider.RemoteFile, error) {
	if inConflictDir(p) {
		return provider.RemoteFile{}, errors.New("put refused")
	}
	return s.trashingStore.Put(ctx, p, r)
}

// remoteCopies lists the remote objects inside a conflicts folder.
func remoteCopies(s *patchStore) map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]string{}
	for p, b := range s.content {
		if inConflictDir(p) {
			out[p] = string(b)
		}
	}
	return out
}

// localCopies lists the files in dir's top-level conflicts folder.
func localCopies(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	ents, err := os.ReadDir(filepath.Join(dir, ConflictDirName))
	if os.IsNotExist(err) {
		return out
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		b, _ := os.ReadFile(filepath.Join(dir, ConflictDirName, e.Name()))
		out[e.Name()] = string(b)
	}
	return out
}

// trashed lists a local trash's files.
func trashed(t *testing.T, tr *trash.Trash) map[string]string {
	t.Helper()
	out := map[string]string{}
	ents, _ := os.ReadDir(filepath.Join(tr.Root(), "files"))
	for _, e := range ents {
		b, _ := os.ReadFile(filepath.Join(tr.Root(), "files", e.Name()))
		out[e.Name()] = string(b)
	}
	return out
}

func onlyValue(m map[string]string) string {
	for _, v := range m {
		return v
	}
	return ""
}

// pushOver is the push side of a conflict: a.txt synced at "base", the remote
// moved to "theirs", the local copy edited to "ours", and a push.
func pushOver(t *testing.T, store interface {
	provider.Store
	seed(string, []byte)
}, ps *patchStore, c *Conflicts) (dir string, st *state.Store, logs *capture, err error) {
	t.Helper()
	dir = t.TempDir()
	st = newState(t)
	syncedAt(t, st, ps, "a.txt", []byte("base"))
	ps.seed("a.txt", []byte("theirs"))
	writeFile(t, dir, "a.txt", "ours")
	logs = &capture{}
	e := New(Config{Store: store, DataDir: dir, State: st, Logger: log.New(logs, "", 0), Conflicts: c})
	return dir, st, logs, e.pushContent(context.Background(), "a.txt", nil)
}

// Where a conflicts folder lives decides where the remote's version goes, and
// the local edit wins the path in every case.
func TestTheConflictsFolderLivesWhereItIsConfigured(t *testing.T) {
	for _, place := range ConflictPlaces {
		t.Run(string(place), func(t *testing.T) {
			ps := newPatchStore()
			dir, st, _, err := pushOver(t, ps, ps, &Conflicts{Place: place})
			if err != nil {
				t.Fatal(err)
			}
			if got := string(ps.remoteBytes("a.txt")); got != "ours" {
				t.Errorf("remote a.txt = %q; the local edit should win", got)
			}
			local, remote := localCopies(t, dir), remoteCopies(ps)
			wantLocal := place != ConflictsRemote
			wantRemote := place != ConflictsLocal
			if (len(local) == 1) != wantLocal || (wantLocal && onlyValue(local) != "theirs") {
				t.Errorf("local copies = %v; want one holding theirs: %v", local, wantLocal)
			}
			if (len(remote) == 1) != wantRemote || (wantRemote && onlyValue(remote) != "theirs") {
				t.Errorf("remote copies = %v; want one holding theirs: %v", remote, wantRemote)
			}
			if place == ConflictsRemote {
				if _, err := os.Lstat(filepath.Join(dir, ConflictDirName)); err == nil {
					t.Error("an empty local conflicts folder was left behind")
				}
			}
			// On both sides the copy is synced content, so it has a baseline;
			// otherwise nothing records it.
			for p := range remote {
				if _, ok, _ := st.GetEcho(p); ok != (place == ConflictsBoth) {
					t.Errorf("baseline for %s present = %v; want %v", p, ok, place == ConflictsBoth)
				}
			}
		})
	}
}

// A copy that cannot be uploaded is still kept — locally — and the push of the
// winner goes ahead: the loser is preserved, which is the promise.
func TestACopyThatCannotBeUploadedStaysLocal(t *testing.T) {
	for _, c := range []*Conflicts{{Place: ConflictsBoth}, {Place: ConflictsRemote}, {Action: ConflictsRemoteTrash}} {
		t.Run(c.String(), func(t *testing.T) {
			ts := newTrashingStore()
			dir, _, logs, err := pushOver(t, putFailStore{ts}, ts.patchStore, c)
			if err != nil {
				t.Fatal(err)
			}
			if got := localCopies(t, dir); onlyValue(got) != "theirs" {
				t.Errorf("local copies = %v; want the remote's version kept here", got)
			}
			if logs.count("put refused") != 1 {
				t.Errorf("the failed upload was not reported:\n%s", strings.Join(logs.lines(), "\n"))
			}
		})
	}
}

// remote-trash: the loser is uploaded into the folder and trashed at once, so
// the folder holds nothing and the trash holds it.
func TestRemoteTrashTrashesTheLoser(t *testing.T) {
	ts := newTrashingStore()
	dir, _, _, err := pushOver(t, ts, ts.patchStore, &Conflicts{Action: ConflictsRemoteTrash})
	if err != nil {
		t.Fatal(err)
	}
	if got := remoteCopies(ts.patchStore); len(got) != 0 {
		t.Errorf("remote copies = %v; want none left untrashed", got)
	}
	if len(ts.trashed) != 1 || onlyValue(ts.trashed2()) != "theirs" {
		t.Errorf("trashed = %v; want the remote's version", ts.trashed2())
	}
	if got := localCopies(t, dir); len(got) != 0 {
		t.Errorf("local copies = %v; want none", got)
	}
}

func (s *trashingStore) trashed2() map[string]string {
	out := map[string]string{}
	for p, b := range s.trashed {
		out[p] = string(b)
	}
	return out
}

// remote-trash with nothing to trash it in (Open refuses that; a store whose
// capabilities changed could still reach it) keeps the remote's version here,
// never nowhere.
func TestRemoteTrashWithoutATrashKeepsTheCopyHere(t *testing.T) {
	ps := newPatchStore()
	dir, _, _, err := pushOver(t, ps, ps, &Conflicts{Action: ConflictsRemoteTrash})
	if err != nil {
		t.Fatal(err)
	}
	if got := localCopies(t, dir); onlyValue(got) != "theirs" {
		t.Errorf("local copies = %v; want the remote's version kept here", got)
	}
}

// local-trash: the loser is in the home trash with a .trashinfo naming where
// it came from, under the mountpoint.
func TestLocalTrashKeepsTheLoserInTheTrash(t *testing.T) {
	tr, err := trash.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ps := newPatchStore()
	dir, _, _, err := pushOver(t, ps, ps, &Conflicts{Action: ConflictsLocalTrash, Trash: tr, MountPath: "/mnt/x"})
	if err != nil {
		t.Fatal(err)
	}
	if got := trashed(t, tr); len(got) != 1 || onlyValue(got) != "theirs" {
		t.Errorf("trash = %v; want the remote's version", got)
	}
	info, _ := os.ReadFile(filepath.Join(tr.Root(), "info", "a.txt.trashinfo"))
	if !strings.Contains(string(info), "Path=/mnt/x/a.txt\n") {
		t.Errorf(".trashinfo does not name the mountpoint path:\n%s", info)
	}
	if got := localCopies(t, dir); len(got) != 0 {
		t.Errorf("local copies = %v; want none", got)
	}
}

// overwrite keeps nothing.
func TestOverwriteKeepsNothing(t *testing.T) {
	ps := newPatchStore()
	dir, _, logs, err := pushOver(t, ps, ps, &Conflicts{Action: ConflictsOverwrite})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(ps.remoteBytes("a.txt")); got != "ours" {
		t.Errorf("remote = %q; want ours", got)
	}
	if len(localCopies(t, dir)) != 0 || len(remoteCopies(ps)) != 0 {
		t.Error("a copy was kept under conflicts = overwrite")
	}
	if logs.count("conflicts = overwrite") != 1 {
		t.Errorf("the overwrite was not reported:\n%s", strings.Join(logs.lines(), "\n"))
	}
}

// The original question: a small first push over a remote file this mount has
// no record of. Through Creator the upload itself finds it; without Creator a
// Stat does. Either way the remote's file is kept. Under overwrite neither is
// asked, which is the setting's cost and its saving.
func TestASmallFirstPushOverAnUnknownRemoteKeepsIt(t *testing.T) {
	for _, tc := range []struct {
		name      string
		creator   bool
		action    ConflictAction
		wantCopy  bool
		wantStats int
	}{
		{"creator", true, ConflictsDir, true, 1},
		{"stat", false, ConflictsDir, true, 1},
		{"overwrite", true, ConflictsOverwrite, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			ps := newPatchStore()
			ps.seed("a.txt", []byte("theirs"))
			writeFile(t, dir, "a.txt", "ours")
			var store provider.Store = ps
			if tc.creator {
				store = creatingStore{ps}
			}
			e := New(Config{Store: store, DataDir: dir, State: newState(t), Conflicts: &Conflicts{Action: tc.action, Place: ConflictsLocal}})
			if err := e.pushContent(context.Background(), "a.txt", nil); err != nil {
				t.Fatal(err)
			}
			if got := string(ps.remoteBytes("a.txt")); got != "ours" {
				t.Errorf("remote = %q; the local file should win the path", got)
			}
			got := localCopies(t, dir)
			if tc.wantCopy && onlyValue(got) != "theirs" {
				t.Errorf("copies = %v; want the remote file kept", got)
			}
			if !tc.wantCopy && len(got) != 0 {
				t.Errorf("copies = %v; want none", got)
			}
			if n := ps.statCount(); n != tc.wantStats {
				t.Errorf("Stat calls = %d; want %d", n, tc.wantStats)
			}
		})
	}
}

// A first push of identical bytes over a remote this mount has no record of —
// two clients that made the same file, or a baseline lost — keeps no copy: the
// digest says they are the same file.
func TestAFirstPushOfTheSameBytesKeepsNoCopy(t *testing.T) {
	dir := t.TempDir()
	ps := newPatchStore()
	ps.seed("a.txt", []byte("same"))
	writeFile(t, dir, "a.txt", "same")
	e := New(Config{Store: creatingStore{ps}, DataDir: dir, State: newState(t)})
	if err := e.pushContent(context.Background(), "a.txt", nil); err != nil {
		t.Fatal(err)
	}
	if got := localCopies(t, dir); len(got) != 0 {
		t.Errorf("copies = %v; want none for identical bytes", got)
	}
}

// pullConflict is the pull side, remote newer: a.txt synced at "base", edited
// here to "ours" an hour ago, and a remote change to "theirs" arriving now.
func pullConflict(t *testing.T, store provider.Store, ps *patchStore, c *Conflicts) (dir string, d *Downloader, err error) {
	t.Helper()
	return pullConflictWith(t, store, ps, c, func(string) {})
}

// pullConflictWith is pullConflict with prepare run on the backing directory
// first. It returns once the copy's remote half has finished too.
func pullConflictWith(t *testing.T, store provider.Store, ps *patchStore, c *Conflicts, prepare func(dir string)) (dir string, d *Downloader, err error) {
	t.Helper()
	dir = t.TempDir()
	prepare(dir)
	st := newState(t)
	ps.seed("a.txt", []byte("theirs"))
	writeFile(t, dir, "a.txt", "ours")
	setMtime(t, dir, "a.txt", time.Now().Add(-time.Hour))
	_ = st.SetEcho("a.txt", state.Echo{Hash: md5hex("base"), Version: "1", At: time.Now()})
	d = NewDownloader(nil, store, dir, st, DefaultCadence).Conflicts(c)
	remote := &provider.RemoteFile{Path: "a.txt", Hash: md5hex("theirs"), Version: "2", Modified: time.Now()}
	err = d.apply(context.Background(), provider.RemoteChange{Path: "a.txt", File: remote})
	d.sending.Wait()
	return dir, d, err
}

// The pull's remote-newer branch keeps the LOCAL edit, which is the case where
// the loser's bytes exist only here; each policy must put them somewhere real
// before the remote's version replaces them.
func TestALocalEditThatLosesIsKeptByEachPolicy(t *testing.T) {
	tr, err := trash.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		c    *Conflicts
		kept func(t *testing.T, dir string, ts *trashingStore) string
	}{
		{"dir-local", &Conflicts{Place: ConflictsLocal}, func(t *testing.T, dir string, _ *trashingStore) string {
			return onlyValue(localCopies(t, dir))
		}},
		{"dir-remote", &Conflicts{Place: ConflictsRemote}, func(_ *testing.T, _ string, ts *trashingStore) string {
			return onlyValue(remoteCopies(ts.patchStore))
		}},
		{"local-trash", &Conflicts{Action: ConflictsLocalTrash, Trash: tr}, func(t *testing.T, _ string, _ *trashingStore) string {
			return onlyValue(trashed(t, tr))
		}},
		{"remote-trash", &Conflicts{Action: ConflictsRemoteTrash}, func(_ *testing.T, _ string, ts *trashingStore) string {
			return onlyValue(ts.trashed2())
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := newTrashingStore()
			dir, _, err := pullConflict(t, ts, ts.patchStore, tc.c)
			if err != nil {
				t.Fatal(err)
			}
			if got, _ := readBacking(t, dir, "a.txt"); got != "theirs" {
				t.Errorf("a.txt = %q; the newer remote should win", got)
			}
			if got := tc.kept(t, dir, ts); got != "ours" {
				t.Errorf("kept %q; want the local edit", got)
			}
		})
	}
}

// A local edit that cannot be kept is not replaced: the apply fails and the
// edit is where it was. A file where the conflicts folder should be is what
// makes it unkeepable here.
func TestALocalEditThatCannotBeKeptIsNotReplaced(t *testing.T) {
	ts := newTrashingStore()
	dir, _, err := pullConflictWith(t, ts, ts.patchStore, &Conflicts{Action: ConflictsRemoteTrash}, func(dir string) {
		writeFile(t, dir, ConflictDirName, "in the way")
	})
	if err == nil {
		t.Fatal("the apply succeeded with nowhere to keep the local edit")
	}
	if got, _ := readBacking(t, dir, "a.txt"); got != "ours" {
		t.Errorf("a.txt = %q; the local edit was replaced unkept", got)
	}
}

// One conflict is met twice — by the pull ("local newer") and then by the push
// of that local edit — and kept once, even where no local file is left to
// recognise it by.
func TestOneConflictIsKeptOnceAcrossThePullAndThePush(t *testing.T) {
	ts := newTrashingStore()
	dir := t.TempDir()
	st := newState(t)
	ts.seed("a.txt", []byte("theirs"))
	writeFile(t, dir, "a.txt", "ours")
	_ = st.SetEcho("a.txt", state.Echo{Hash: md5hex("base"), Version: "1", At: time.Now()})
	c := &Conflicts{Action: ConflictsRemoteTrash}

	d := NewDownloader(nil, ts, dir, st, DefaultCadence).Conflicts(c)
	remote, _, _ := ts.Stat(context.Background(), "a.txt")
	remote.Modified = time.Now().Add(-time.Hour)
	if err := d.apply(context.Background(), provider.RemoteChange{Path: "a.txt", File: &remote}); err != nil {
		t.Fatal(err)
	}
	d.sending.Wait()
	e := New(Config{Store: ts, DataDir: dir, State: st, Conflicts: c})
	if err := e.pushContent(context.Background(), "a.txt", nil); err != nil {
		t.Fatal(err)
	}
	if len(ts.trashed) != 1 {
		t.Errorf("trashed %v; want the remote version once", ts.trashed2())
	}
	if got := string(ts.remoteBytes("a.txt")); got != "ours" {
		t.Errorf("remote = %q; want ours", got)
	}
}

// The same, across a restart between the two meetings: a fresh Conflicts value
// over the same state store still knows the version was kept, and the push's
// SetEcho then discharges the record.
func TestOneConflictIsKeptOnceAcrossARestart(t *testing.T) {
	ts := newTrashingStore()
	dir := t.TempDir()
	st := newState(t)
	ts.seed("a.txt", []byte("theirs"))
	writeFile(t, dir, "a.txt", "ours")
	_ = st.SetEcho("a.txt", state.Echo{Hash: md5hex("base"), Version: "1", At: time.Now()})

	d := NewDownloader(nil, ts, dir, st, DefaultCadence).Conflicts(&Conflicts{Action: ConflictsRemoteTrash})
	remote, _, _ := ts.Stat(context.Background(), "a.txt")
	remote.Modified = time.Now().Add(-time.Hour)
	if err := d.apply(context.Background(), provider.RemoteChange{Path: "a.txt", File: &remote}); err != nil {
		t.Fatal(err)
	}
	d.sending.Wait()
	e := New(Config{Store: ts, DataDir: dir, State: st, Conflicts: &Conflicts{Action: ConflictsRemoteTrash}})
	if err := e.pushContent(context.Background(), "a.txt", nil); err != nil {
		t.Fatal(err)
	}
	if len(ts.trashed) != 1 {
		t.Errorf("trashed %v; want the remote version once", ts.trashed2())
	}
	if _, ok, _ := st.Kept("a.txt"); ok {
		t.Error("the kept record outlived the push that settled it")
	}
}

// What the folder's policy means to every other pass.
func TestAConflictsFolderSyncsOnlyOnBothSides(t *testing.T) {
	both, local := &Conflicts{}, &Conflicts{Place: ConflictsLocal}
	for _, p := range []string{".drivel-conflicts/a.txt", "sub/.drivel-conflicts/b (conflict 2026-01-02 03-04-05).txt", ".drivel-conflicts"} {
		if !both.Syncs(p) {
			t.Errorf("%s is not synced with the folder on both sides", p)
		}
		if local.Syncs(p) {
			t.Errorf("%s is synced with the folder kept local", p)
		}
		if (&Conflicts{Action: ConflictsLocalTrash}).Syncs(p) {
			t.Errorf("%s is synced under local-trash, which makes no folder", p)
		}
	}
	if !local.Syncs("sub/x.txt") || !local.Syncs("a.drivel-conflicts") {
		t.Error("an ordinary path is not synced")
	}
}

// The engine sends nothing from inside a folder it keeps to itself, sends a
// copy moved out of it, and removes from the remote a file moved into it.
func TestTheEngineRespectsAPrivateConflictsFolder(t *testing.T) {
	dir := t.TempDir()
	ps := newPatchStore()
	ps.seed("gone.txt", []byte("x"))
	writeFile(t, dir, ".drivel-conflicts/a.txt", "copy")
	writeFile(t, dir, "restored.txt", "copy")
	e := New(Config{Store: ps, DataDir: dir, State: newState(t), Conflicts: &Conflicts{Place: ConflictsLocal}})
	ctx := context.Background()

	if err := e.push(ctx, fsevent.Event{Op: fsevent.OpWrite, Path: ".drivel-conflicts/a.txt"}); err != nil {
		t.Fatal(err)
	}
	if len(remoteCopies(ps)) != 0 {
		t.Error("a write inside a private conflicts folder was uploaded")
	}
	if err := e.push(ctx, fsevent.Event{Op: fsevent.OpRename, Path: ".drivel-conflicts/b.txt", NewPath: "restored.txt"}); err != nil {
		t.Fatal(err)
	}
	if got := string(ps.remoteBytes("restored.txt")); got != "copy" {
		t.Errorf("a copy restored out of the folder was not uploaded: %q", got)
	}
	if err := e.push(ctx, fsevent.Event{Op: fsevent.OpRename, Path: "gone.txt", NewPath: ".drivel-conflicts/gone.txt"}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := ps.Stat(ctx, "gone.txt"); ok {
		t.Error("a file moved into a private conflicts folder is still on the remote")
	}
}

// The pull neither applies nor removes anything in a folder it keeps out of
// its sync, and does both with the folder on both sides.
func TestThePullRespectsAPrivateConflictsFolder(t *testing.T) {
	for _, tc := range []struct {
		c     *Conflicts
		apply bool
	}{{&Conflicts{Place: ConflictsLocal}, false}, {&Conflicts{}, true}} {
		dir := t.TempDir()
		ps := newPatchStore()
		ps.seed(".drivel-conflicts/a.txt", []byte("theirs"))
		writeFile(t, dir, ".drivel-conflicts/mine.txt", "mine")
		st := newState(t)
		_ = st.SetEcho(".drivel-conflicts/mine.txt", state.Echo{Hash: md5hex("mine"), Version: "1", At: time.Now()})
		d := NewDownloader(nil, ps, dir, st, DefaultCadence).Conflicts(tc.c)
		ctx := context.Background()
		rf, _, _ := ps.Stat(ctx, ".drivel-conflicts/a.txt")
		if err := d.apply(ctx, provider.RemoteChange{Path: rf.Path, File: &rf}); err != nil {
			t.Fatal(err)
		}
		if err := d.apply(ctx, provider.RemoteChange{Path: ".drivel-conflicts/mine.txt", Removed: true}); err != nil {
			t.Fatal(err)
		}
		_, got := readBacking(t, dir, ".drivel-conflicts/a.txt")
		_, mine := readBacking(t, dir, ".drivel-conflicts/mine.txt")
		if got != tc.apply || mine == tc.apply {
			t.Errorf("%s: applied=%v removed=%v; want both %v", tc.c, got, !mine, tc.apply)
		}
	}
}

// The sweep's walk uploads a copy left in a conflicts folder that syncs — one
// whose upload failed when it was made — and leaves a private folder alone,
// along with the sibling copies drivel made before there was a folder.
func TestTheWalkSendsAConflictsFolderOnlyWhenItSyncs(t *testing.T) {
	for _, tc := range []struct {
		c    *Conflicts
		want []string
	}{
		{&Conflicts{}, []string{"write .drivel-conflicts/a (conflict 2026-01-02 03-04-05).txt"}},
		{&Conflicts{Place: ConflictsLocal}, nil},
	} {
		f := newSweepFixture(t, ReconcileOptions{Fetch: true})
		f.dl.Conflicts(tc.c)
		writeFile(t, f.dir, ".drivel-conflicts/a (conflict 2026-01-02 03-04-05).txt", "kept")
		writeFile(t, f.dir, "old (conflict 2026-01-02 03-04-05).txt", "a sibling copy")
		f.run(t)
		if got := f.push.ops(); !equal(got, tc.want) {
			t.Errorf("%s: pushes = %v; want %v", tc.c, got, tc.want)
		}
	}
}

// blockingPutStore holds every Put inside a conflicts folder until release is
// closed, and says when one has started.
type blockingPutStore struct {
	*trashingStore
	started chan string
	release chan struct{}
}

func (s blockingPutStore) Put(ctx context.Context, p string, r io.Reader) (provider.RemoteFile, error) {
	if inConflictDir(p) {
		s.started <- p
		<-s.release
	}
	return s.trashingStore.Put(ctx, p, r)
}

// heldGate is a Holder that is paused until opened.
type heldGate struct{ open chan struct{} }

func (g heldGate) WaitUnpaused(ctx context.Context) bool {
	select {
	case <-g.open:
		return true
	case <-ctx.Done():
		return false
	}
}

// pendingPull is pullConflict's setup with the apply left to the caller.
func pendingPull(t *testing.T, store provider.Store, ps *patchStore, c *Conflicts) (dir string, d *Downloader, apply func() error) {
	t.Helper()
	dir = t.TempDir()
	st := newState(t)
	ps.seed("a.txt", []byte("theirs"))
	writeFile(t, dir, "a.txt", "ours")
	setMtime(t, dir, "a.txt", time.Now().Add(-time.Hour))
	_ = st.SetEcho("a.txt", state.Echo{Hash: md5hex("base"), Version: "1", At: time.Now()})
	d = NewDownloader(nil, store, dir, st, DefaultCadence).Conflicts(c)
	remote := &provider.RemoteFile{Path: "a.txt", Hash: md5hex("theirs"), Version: "2", Modified: time.Now()}
	return dir, d, func() error {
		return d.apply(context.Background(), provider.RemoteChange{Path: "a.txt", File: remote})
	}
}

// The pull loop does not wait for a conflict copy's upload: the apply returns
// with the loser safe locally while the upload is still running. Before
// 2026-10-18 the upload ran inside the apply, so one large loser held up every
// inbound change behind it.
func TestThePullDoesNotWaitForAConflictCopysUpload(t *testing.T) {
	ts := newTrashingStore()
	bs := blockingPutStore{ts, make(chan string, 1), make(chan struct{})}
	dir, d, apply := pendingPull(t, bs, ts.patchStore, &Conflicts{})
	defer d.sending.Wait()
	defer close(bs.release)

	applied := make(chan error, 1)
	go func() { applied <- apply() }()
	select {
	case <-bs.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the copy's upload never started")
	}
	select {
	case err := <-applied:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the apply waited for the conflict copy's upload")
	}
	if got, _ := readBacking(t, dir, "a.txt"); got != "theirs" {
		t.Errorf("a.txt = %q; the newer remote should already be in place", got)
	}
	if got := onlyValue(localCopies(t, dir)); got != "ours" {
		t.Errorf("local copy = %q; the loser should be safe here before its upload", got)
	}
}

// A pause holds a conflict copy's upload, as it holds every other transfer,
// and the upload goes once the pause ends.
func TestAPauseHoldsAConflictCopysUpload(t *testing.T) {
	ts := newTrashingStore()
	g := heldGate{make(chan struct{})}
	_, d, apply := pendingPull(t, ts, ts.patchStore, &Conflicts{})
	d.PausedBy(g)
	if err := apply(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if got := remoteCopies(ts.patchStore); len(got) != 0 {
		t.Errorf("remote copies = %v while paused; want none yet", got)
	}
	close(g.open)
	d.sending.Wait()
	if got := onlyValue(remoteCopies(ts.patchStore)); got != "ours" {
		t.Errorf("remote copy = %q after the pause; want the local edit", got)
	}
}

// A directory crosses the edge of a private conflicts folder as one rename
// event. Moved out, everything in it is uploaded; moved in, it is removed from
// the remote and its children's records go with it, so no sweep later reads
// them as files deleted here.
func TestADirectoryCrossesAPrivateConflictsFolder(t *testing.T) {
	dir := t.TempDir()
	ps := newPatchStore()
	st := newState(t)
	e := New(Config{Store: ps, DataDir: dir, State: st, Conflicts: &Conflicts{Place: ConflictsLocal}})
	ctx := context.Background()

	writeFile(t, dir, "back/a.txt", "a")
	writeFile(t, dir, "back/sub/b.txt", "b")
	writeFile(t, dir, "back/"+ConflictDirName+"/c.txt", "private")
	if err := e.push(ctx, fsevent.Event{Op: fsevent.OpRename, Path: ConflictDirName + "/back", NewPath: "back"}); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]string{"back/a.txt": "a", "back/sub/b.txt": "b"} {
		if got := string(ps.remoteBytes(p)); got != want {
			t.Errorf("%s on the remote = %q; want %q", p, got, want)
		}
	}
	if got := remoteCopies(ps); len(got) != 0 {
		t.Errorf("remote copies = %v; the nested private folder was uploaded", got)
	}

	// Now move it back in: the local tree is under the folder, as after rename(2).
	if err := os.MkdirAll(filepath.Join(dir, ConflictDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "back"), filepath.Join(dir, ConflictDirName, "back")); err != nil {
		t.Fatal(err)
	}
	if err := e.push(ctx, fsevent.Event{Op: fsevent.OpRename, Path: "back", NewPath: ConflictDirName + "/back"}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := ps.Stat(ctx, "back/a.txt"); ok {
		t.Error("back/a.txt is still on the remote")
	}
	for _, p := range []string{"back/a.txt", "back/sub/b.txt", "back/sub"} {
		if _, ok, _ := st.GetEcho(p); ok {
			t.Errorf("the record for %s survived its directory's move into the folder", p)
		}
	}
}
