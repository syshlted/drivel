// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package syncengine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/syshlted/drivel/internal/state"
	"github.com/syshlted/drivel/provider"
)

// A remote deletion arriving through the feed deletes only what still matches
// its baseline. Until 2026-10-18 it was one RemoveAll, so a local edit not yet
// pushed went with the file, and every never-synced file went with a directory.

// removalFixture syncs keep.txt and edited.txt under dir/ at "base", edits
// edited.txt locally, and adds new.txt (never synced) beside them.
func removalFixture(t *testing.T) (dir string, st *state.Store, push *recPusher, d *Downloader) {
	t.Helper()
	dir = t.TempDir()
	st = newState(t)
	for _, p := range []string{"dir/keep.txt", "dir/edited.txt", "solo.txt", "solo-edited.txt"} {
		writeFile(t, dir, p, "base")
		_ = st.SetEcho(p, state.Echo{Hash: md5hex("base"), Version: "1", At: time.Now()})
	}
	_ = st.SetEcho("dir", state.Echo{Version: "d", At: time.Now()})
	writeFile(t, dir, "dir/edited.txt", "edited here")
	writeFile(t, dir, "solo-edited.txt", "edited here")
	writeFile(t, dir, "dir/new.txt", "never synced")
	push = &recPusher{}
	d = NewDownloader(nil, newPatchStore(), dir, st, DefaultCadence).Reconcile(ReconcileOptions{Push: push})
	return dir, st, push, d
}

func TestARemoteDeletionKeepsAnUnpushedEdit(t *testing.T) {
	dir, st, push, d := removalFixture(t)
	ctx := context.Background()
	for _, p := range []string{"solo.txt", "solo-edited.txt"} {
		if err := d.apply(ctx, provider.RemoteChange{Path: p, Removed: true}); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := readBacking(t, dir, "solo.txt"); ok {
		t.Error("an unchanged file survived its remote deletion")
	}
	if got, ok := readBacking(t, dir, "solo-edited.txt"); !ok || got != "edited here" {
		t.Errorf("an unpushed edit was deleted with its file: %q, %v", got, ok)
	}
	if _, ok, _ := st.GetEcho("solo-edited.txt"); ok {
		t.Error("the kept file's stale baseline was left in place")
	}
	if got, want := push.ops(), []string{"write solo-edited.txt"}; !equal(got, want) {
		t.Errorf("pushes = %v; want %v (the kept edit goes back up)", got, want)
	}
}

func TestARemoteDirectoryDeletionKeepsWhatWasNotSynced(t *testing.T) {
	dir, _, push, d := removalFixture(t)
	writeFile(t, dir, "dir/"+ConflictDirName+"/edited (conflict 2026-01-02 03-04-05).txt", "a copy")
	d.Conflicts(&Conflicts{Place: ConflictsLocal})
	if err := d.apply(context.Background(), provider.RemoteChange{Path: "dir", Removed: true}); err != nil {
		t.Fatal(err)
	}
	if _, ok := readBacking(t, dir, "dir/keep.txt"); ok {
		t.Error("an unchanged file survived its directory's remote deletion")
	}
	for _, p := range []string{"dir/edited.txt", "dir/new.txt", "dir/" + ConflictDirName + "/edited (conflict 2026-01-02 03-04-05).txt"} {
		if _, ok := readBacking(t, dir, p); !ok {
			t.Errorf("%s was deleted with its directory", p)
		}
	}
	if got, want := push.ops(), []string{"write dir/edited.txt", "write dir/new.txt"}; !equal(got, want) {
		t.Errorf("pushes = %v; want %v", got, want)
	}
}

// The ordinary case is untouched: a directory whose every file matches its
// baseline goes, directory and all.
func TestARemoteDirectoryDeletionRemovesASyncedTree(t *testing.T) {
	dir := t.TempDir()
	st := newState(t)
	for _, p := range []string{"d/a.txt", "d/sub/b.txt"} {
		writeFile(t, dir, p, "base")
		_ = st.SetEcho(p, state.Echo{Hash: md5hex("base"), Version: "1", At: time.Now()})
	}
	d := NewDownloader(nil, newPatchStore(), dir, st, DefaultCadence)
	if err := d.apply(context.Background(), provider.RemoteChange{Path: "d", Removed: true}); err != nil {
		t.Fatal(err)
	}
	if _, ok := readBacking(t, dir, "d/sub/b.txt"); ok {
		t.Error("a synced file survived")
	}
	if _, err := os.Lstat(filepath.Join(dir, "d")); err == nil {
		t.Error("the directory survived")
	}
}

// A kept file changed since this process opened changed through the mount, so
// the engine already holds its push, and the deletion's keep does not send it
// a second time. Before 2026-10-18 it did, and a live SFTP run logged both.
func TestAKeptFileTheEngineHoldsIsNotPushedTwice(t *testing.T) {
	dir, _, push, d := removalFixture(t)
	writeFile(t, dir, "solo-edited.txt", "edited through the mount")
	setMtime(t, dir, "solo-edited.txt", d.openedAt.Add(time.Second))
	if err := d.apply(context.Background(), provider.RemoteChange{Path: "solo-edited.txt", Removed: true}); err != nil {
		t.Fatal(err)
	}
	if got, ok := readBacking(t, dir, "solo-edited.txt"); !ok || got != "edited through the mount" {
		t.Errorf("the edit was not kept: %q, %v", got, ok)
	}
	if got := push.ops(); len(got) != 0 {
		t.Errorf("pushes = %v; want none (the engine has it queued)", got)
	}
}
