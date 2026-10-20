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

	"github.com/syshlted/drivel/internal/fsevent"
	"github.com/syshlted/drivel/internal/state"
	"github.com/syshlted/drivel/internal/testenv"
	"github.com/syshlted/drivel/provider"
)

// A backing filesystem that folds names holds one file where a case-sensitive
// backend holds two. The tests that need one fold it by hand (foldAs), so they
// run on the case-sensitive filesystems CI has; TestOnARealFoldingFilesystem
// checks the kernel half where a folding directory is supplied.

// foldAs makes rel's components fold to the given stored names, the way a
// case-insensitive filesystem would: alias of "foo.txt" is "Foo.txt" when
// stored maps "foo.txt" to "Foo.txt".
func foldAs(stored map[string]string) func(string) (string, bool) {
	return func(rel string) (string, bool) {
		parts := strings.Split(rel, "/")
		for i := range parts {
			if s, ok := stored[strings.Join(parts[:i+1], "/")]; ok {
				return s, true
			}
		}
		return "", false
	}
}

func foldingDownloader(t *testing.T, store provider.Store, dir string, st *state.Store, stored map[string]string) (*Downloader, *capture) {
	t.Helper()
	logs := &capture{}
	d := NewDownloader(nil, store, dir, st, DefaultCadence).Logger(log.New(logs, "", 0)).Folds(Folding{Case: true})
	d.alias = foldAs(stored)
	return d, logs
}

// The corruption: a remote foo.txt written through a temporary and a rename
// onto foo.txt replaces the local Foo.txt's bytes on a folding filesystem. So it
// is not applied, and the line says which file it would have landed on.
func TestPullDoesNotWriteOverAFileStoredUnderAFoldedName(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "Foo.txt", "Foo's bytes")
	store := newFakeStore()
	store.content["foo.txt"] = []byte("foo's bytes")
	st := newState(t)
	d, logs := foldingDownloader(t, store, dir, st, map[string]string{"foo.txt": "Foo.txt"})
	rf := remote("foo.txt", "foo's bytes")

	if err := d.apply(context.Background(), provider.RemoteChange{Path: rf.Path, File: &rf}); err != nil {
		t.Fatal(err)
	}

	if got, _ := readBacking(t, dir, "Foo.txt"); got != "Foo's bytes" {
		t.Fatalf("Foo.txt = %q; a remote file of a folded name was written over it", got)
	}
	if listed(t, dir, "foo.txt") {
		t.Fatal("the folded name was applied")
	}
	if _, ok, _ := st.GetEcho("foo.txt"); ok {
		t.Fatal("a baseline was recorded for a path that was not applied")
	}
	if !strings.Contains(logs.String(), "skip    foo.txt (this backing filesystem folds letter case, and Foo.txt is already here") {
		t.Fatalf("the skip was not reported:\n%s", logs)
	}
}

// A folded directory hides everything under it: remote sub/x would land in the
// local Sub/x, merging two folders.
func TestPullDoesNotMergeIntoAFolderStoredUnderAFoldedName(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "Sub/mine.txt", "x")
	store := newFakeStore()
	store.content["sub/theirs.txt"] = []byte("y")
	d, _ := foldingDownloader(t, store, dir, newState(t), map[string]string{"sub": "Sub"})
	for _, rf := range []provider.RemoteFile{remoteDir("sub"), remote("sub/theirs.txt", "y")} {
		if err := d.apply(context.Background(), provider.RemoteChange{Path: rf.Path, File: &rf}); err != nil {
			t.Fatal(err)
		}
	}
	if listed(t, dir, "sub") {
		t.Fatal("the folded folder was created")
	}
	if listed(t, filepath.Join(dir, "Sub"), "theirs.txt") {
		t.Fatal("the folded folder's content was merged into the other folder")
	}
}

// linkAs gives the file at name a second name, alias: on a case-sensitive
// filesystem the nearest thing to a folding one, two names answering with one
// file — which is the shape the guards below exist for.
func linkAs(t *testing.T, dir, name, alias string) {
	t.Helper()
	if err := os.Link(filepath.Join(dir, name), filepath.Join(dir, alias)); err != nil {
		t.Skipf("no hard links here: %v", err)
	}
}

// A remote removal of the folded name is not a removal of the other file — and
// not, either, a reason to send that file back up under the removed name, which
// is what the removal's keep-what-changed rule would otherwise do with it.
func TestARemoteRemovalOfAFoldedNameLeavesTheOtherFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "Foo.txt", "Foo's bytes")
	linkAs(t, dir, "Foo.txt", "foo.txt")
	push := &recPusher{}
	d, _ := foldingDownloader(t, newFakeStore(), dir, newState(t), map[string]string{"foo.txt": "Foo.txt"})
	d.Reconcile(ReconcileOptions{Push: push})

	if err := d.apply(context.Background(), provider.RemoteChange{Path: "foo.txt", Removed: true}); err != nil {
		t.Fatal(err)
	}
	if got, ok := readBacking(t, dir, "Foo.txt"); !ok || got != "Foo's bytes" {
		t.Fatalf("Foo.txt = %q, %v; want it untouched", got, ok)
	}
	if ops := push.ops(); len(ops) != 0 {
		t.Fatalf("pushes = %v; want none — the other file must not be re-sent under the removed name", ops)
	}
}

// The sweep's own delete row, for a baseline of the folded name the remote no
// longer lists: what answers to it here is the other file, which must not be
// judged against this baseline — it would read as diverged and go back up under
// the removed name.
func TestSweepDropsTheBaselineOfAFoldedNameGoneRemotely(t *testing.T) {
	f := newSweepFixture(t, ReconcileOptions{Force: true, Fetch: true})
	writeFile(t, f.dir, "Foo.txt", "Foo's bytes")
	linkAs(t, f.dir, "Foo.txt", "foo.txt")
	seedBaseline(t, f.st, "foo.txt", "foo's bytes")
	f.dl.Folds(Folding{Case: true})
	f.dl.alias = foldAs(map[string]string{"foo.txt": "Foo.txt"})

	f.run(t)

	if got, ok := readBacking(t, f.dir, "Foo.txt"); !ok || got != "Foo's bytes" {
		t.Fatalf("Foo.txt = %q, %v; want it untouched", got, ok)
	}
	for _, op := range f.push.ops() {
		if strings.HasSuffix(op, " foo.txt") {
			t.Fatalf("pushes = %v; want nothing sent under foo.txt", f.push.ops())
		}
	}
	if _, ok, _ := f.st.GetEcho("foo.txt"); ok {
		t.Fatal("the folded name's baseline survived; it describes nothing here")
	}
}

// Opened through the mount under another spelling, the bytes are the stored
// file's, so they go up under its name rather than as a second remote file.
func TestEnginePushesAFoldedNameUnderItsStoredName(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "Foo.txt", "edited as FOO.TXT")
	store := newFakeStore()
	store.seen["Foo.txt"] = true
	e := New(Config{Store: store, DataDir: dir, Folding: Folding{Case: true}})
	e.alias = foldAs(map[string]string{"FOO.TXT": "Foo.txt"})

	e.handle(context.Background(), fsevent.Event{Op: fsevent.OpWrite, Path: "FOO.TXT"})

	assertCalls(t, store.calls, []string{"Put(replace,Foo.txt,bytes=17)"})
}

func TestStoredNameByListingFindsTheSameFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "Foo.txt", "x")
	fi, err := os.Lstat(filepath.Join(dir, "Foo.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, asked := range []string{"foo.txt", "Foo.txt"} {
		if got, err := storedNameByListing(dir, asked, fi); err != nil || got != "Foo.txt" {
			t.Errorf("storedNameByListing(%q) = %q, %v; want Foo.txt", asked, got, err)
		}
	}
}

func TestProbeFoldingLeavesNothingBehind(t *testing.T) {
	dir := t.TempDir()
	if _, err := ProbeFolding(dir); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("the probe left %v behind", entries)
	}
}

// The kernel half, on a directory that really folds. Point DRIVEL_CASEFOLD_DIR at
// one to run it — on Linux a tmpfs mounted with -o casefold and a directory in it
// given chattr +F; on macOS any directory on the default APFS volume.
func TestOnARealFoldingFilesystem(t *testing.T) {
	root := testenv.FoldingDir(t)
	dir, err := os.MkdirTemp(root, "drivel-fold-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	f, err := ProbeFolding(dir)
	if err != nil || !f.Case {
		t.Fatalf("ProbeFolding = %+v, %v; want letter case folded", f, err)
	}
	writeFile(t, dir, "Foo.txt", "Foo's bytes")
	writeFile(t, dir, "Sub/x.txt", "x")
	for rel, want := range map[string]string{"foo.txt": "Foo.txt", "FOO.TXT": "Foo.txt", "sub/y.txt": "Sub", "sub/x.txt": "Sub"} {
		if got, ok := aliasOf(dir, rel); !ok || got != want {
			t.Errorf("aliasOf(%q) = %q, %v; want %q", rel, got, ok, want)
		}
	}
	if got, ok := aliasOf(dir, "Foo.txt"); ok {
		t.Errorf("aliasOf(Foo.txt) = %q; the stored name is not an alias of itself", got)
	}

	store := newFakeStore()
	store.content["foo.txt"] = []byte("foo's bytes")
	d := NewDownloader(nil, store, dir, newState(t), DefaultCadence).Folds(f)
	rf := remote("foo.txt", "foo's bytes")
	if err := d.apply(context.Background(), provider.RemoteChange{Path: rf.Path, File: &rf}); err != nil {
		t.Fatal(err)
	}
	if got, _ := readBacking(t, dir, "Foo.txt"); got != "Foo's bytes" {
		t.Fatalf("Foo.txt = %q; the folded remote name was written over it", got)
	}

	// A remote folder renamed only in case moves the local one, really changing
	// the stored case — where the occupied-destination check would decline it,
	// and one rename(2) onto itself would succeed and do nothing.
	sub := remoteDir("sub")
	if err := d.apply(context.Background(), provider.RemoteChange{Path: "sub", MovedFrom: "Sub", File: &sub}); err != nil {
		t.Fatal(err)
	}
	if got, err := storedName(dir, "sub", filepath.Join(dir, "sub"), mustLstat(t, filepath.Join(dir, "sub"))); err != nil || got != "sub" {
		t.Fatalf("after a case-only move the stored name is %q, %v; want sub", got, err)
	}
	if _, ok := readBacking(t, dir, "sub/x.txt"); !ok {
		t.Fatal("the folder's content did not move with it")
	}
}

// listed reports whether dir's listing holds exactly name. A lookup would not
// do: on a folding filesystem it finds the other spelling.
func listed(t *testing.T, dir, name string) bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == name {
			return true
		}
	}
	return false
}

func mustLstat(t *testing.T, p string) os.FileInfo {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi
}
