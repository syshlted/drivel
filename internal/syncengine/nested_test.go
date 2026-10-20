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
	"github.com/syshlted/drivel/internal/fsevent"
	"github.com/syshlted/drivel/internal/state"
	"github.com/syshlted/drivel/provider"
)

// Another drivel's tree nested in this mount's backing store is that mount's to
// sync (MC-47). The sweep's walk always pruned it; every other pass went on, so
// a write through this mount into the nested tree reached this mount's remote, a
// pull wrote this remote's files into the other mount's tree, and a remote
// deletion deleted the other mount's files.

// nest marks rel as another drivel's backing root.
func nest(t *testing.T, dir, rel string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, claim.Name), []byte("drivel backing store for mount other\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestEngineDoesNotPushIntoANestedTree(t *testing.T) {
	dir := t.TempDir()
	nest(t, dir, "other")
	writeFile(t, dir, "other/theirs.txt", "the other mount's file")
	writeFile(t, dir, "mine.txt", "ours")
	store := newFakeStore()
	logs := &capture{}
	e := New(Config{Store: store, DataDir: dir, Logger: log.New(logs, "", 0)})
	ctx := context.Background()

	e.handle(ctx, fsevent.Event{Op: fsevent.OpWrite, Path: "other/theirs.txt"})
	e.handle(ctx, fsevent.Event{Op: fsevent.OpUnlink, Path: "other/gone.txt"})
	e.handle(ctx, fsevent.Event{Op: fsevent.OpWrite, Path: "mine.txt"})

	assertCalls(t, store.calls, []string{"Put(create,mine.txt,bytes=4)"})
	if !strings.Contains(logs.String(), "inside another drivel mount's backing store at other/") {
		t.Fatalf("the skip did not say why:\n%s", logs)
	}
}

// A rename across the edge is the one event that means something: moved in, the
// file left this mount's tree (a removal here); moved out, it entered it.
func TestEngineTreatsARenameAcrossANestedTreeAsLeavingOrEntering(t *testing.T) {
	dir := t.TempDir()
	nest(t, dir, "other")
	writeFile(t, dir, "other/in.txt", "moved in")
	writeFile(t, dir, "out.txt", "moved out")
	store := newFakeStore()
	store.seen["in.txt"] = true
	e := New(Config{Store: store, DataDir: dir})
	ctx := context.Background()

	e.handle(ctx, fsevent.Event{Op: fsevent.OpRename, Path: "in.txt", NewPath: "other/in.txt"})
	e.handle(ctx, fsevent.Event{Op: fsevent.OpRename, Path: "other/out.txt", NewPath: "out.txt"})

	assertCalls(t, store.calls, []string{"Remove(in.txt)", "Put(create,out.txt,bytes=9)"})
}

func TestPullDoesNotWriteIntoANestedTree(t *testing.T) {
	dir := t.TempDir()
	nest(t, dir, "other")
	store := newFakeStore()
	store.content["other/x.txt"] = []byte("from this mount's remote")
	logs := &capture{}
	d := NewDownloader(nil, store, dir, newState(t), DefaultCadence).Logger(log.New(logs, "", 0))
	rf := remote("other/x.txt", "from this mount's remote")

	if err := d.apply(context.Background(), provider.RemoteChange{Path: rf.Path, File: &rf}); err != nil {
		t.Fatal(err)
	}

	if _, ok := readBacking(t, dir, "other/x.txt"); ok {
		t.Fatal("a remote file was written into another drivel's backing store")
	}
	if !strings.Contains(logs.String(), "skip    other/x.txt (inside another drivel mount's backing store at other/") {
		t.Fatalf("the skip did not say why:\n%s", logs)
	}
}

// A remote deletion of a directory holding a nested tree removes what is this
// mount's and leaves the nested tree whole — and so its parents.
func TestPullRemovalLeavesANestedTreeWhole(t *testing.T) {
	dir := t.TempDir()
	st := newState(t)
	writeFile(t, dir, "top/mine.txt", "base")
	_ = st.SetEcho("top/mine.txt", state.Echo{Hash: md5hex("base"), Version: "1", At: time.Now()})
	nest(t, dir, "top/other")
	writeFile(t, dir, "top/other/theirs.txt", "the other mount's")
	if err := os.MkdirAll(filepath.Join(dir, "top", "other", "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	push := &recPusher{}
	d := NewDownloader(nil, newFakeStore(), dir, st, DefaultCadence).Reconcile(ReconcileOptions{Push: push})

	if err := d.apply(context.Background(), provider.RemoteChange{Path: "top", Removed: true}); err != nil {
		t.Fatal(err)
	}

	if _, ok := readBacking(t, dir, "top/mine.txt"); ok {
		t.Error("this mount's own synced file survived the deletion of its directory")
	}
	if got, ok := readBacking(t, dir, "top/other/theirs.txt"); !ok || got != "the other mount's" {
		t.Errorf("the nested tree's file = %q, %v; want it untouched", got, ok)
	}
	if _, err := os.Stat(filepath.Join(dir, "top", "other", "empty")); err != nil {
		t.Errorf("an empty directory of the nested tree was removed: %v", err)
	}
	if ops := push.ops(); len(ops) != 0 {
		t.Errorf("pushes = %v; want none — nothing of the nested tree is this mount's to send", ops)
	}
}

// The sweep: a listed object under the nested tree is neither materialised nor
// a deletion candidate in either direction, and a baseline from before the
// nesting is not one either.
func TestSweepLeavesANestedTreeAlone(t *testing.T) {
	f := newSweepFixture(t, ReconcileOptions{Force: true, Fetch: true},
		[]provider.RemoteFile{remote("other/listed.txt", "remote"), remote("mine.txt", "remote too"),
			remote("other/absent-here.txt", "synced before the nesting")})
	f.store.content["other/listed.txt"] = []byte("remote")
	f.store.content["mine.txt"] = []byte("remote too")
	nest(t, f.dir, "other")
	writeFile(t, f.dir, "other/old.txt", "synced before the nesting")
	seedBaseline(t, f.st, "other/old.txt", "synced before the nesting")
	seedBaseline(t, f.st, "other/absent-here.txt", "synced before the nesting")

	f.run(t)

	if f.exists("other/listed.txt") {
		t.Error("the sweep materialised a remote file into another drivel's tree")
	}
	if !f.exists("other/old.txt") {
		t.Error("the sweep deleted a file in another drivel's tree")
	}
	if !f.exists("mine.txt") {
		t.Error("the control: this mount's own remote file was not materialised")
	}
	if _, ok, _ := f.st.GetEcho("other/absent-here.txt"); !ok {
		t.Error("a path in another drivel's tree was taken for a deletion made here")
	}
	if ops := f.push.ops(); len(ops) != 0 {
		t.Errorf("pushes = %v; want none", ops)
	}
}

// A mountpoint is checked from the mount table, before any marker, so an lstat
// never has to go through another drivel's FUSE mount to find out.
func TestForeignTreesSeesANestedMountpoint(t *testing.T) {
	dir := t.TempDir()
	reads := 0
	tr := newForeignTrees(dir)
	tr.mountsOf = func(string) map[string]bool {
		reads++
		return map[string]bool{"mnt/other": true}
	}
	if dir, what, ok := tr.owner("mnt/other/file.txt"); !ok || dir != "mnt/other" || what != "mountpoint" {
		t.Fatalf("owner = %q, %q, %v; want the nested mountpoint", dir, what, ok)
	}
	if _, _, ok := tr.owner("mnt/mine.txt"); ok {
		t.Fatal("a sibling of the mountpoint was reported as inside it")
	}
	if reads != 1 {
		t.Fatalf("mount table read %d times for two lookups; want once per refresh", reads)
	}
	now := time.Now().Add(mountsRefresh)
	tr.now = func() time.Time { return now }
	tr.owner("mnt/x")
	if reads != 2 {
		t.Fatalf("mount table read %d times after the refresh interval; want 2", reads)
	}
}

func TestStatusReportsANestedTreeAsLocalOnly(t *testing.T) {
	dir := t.TempDir()
	nest(t, dir, "other")
	writeFile(t, dir, "other/theirs.txt", "x")
	r := StatusReader{Monitor: NewMonitor("m"), State: newState(t), DataDir: dir}
	ps, err := r.Path("other/theirs.txt")
	if err != nil {
		t.Fatal(err)
	}
	if ps.Status != StatusLocalOnly || !strings.Contains(ps.Detail, "another drivel mount's backing store") {
		t.Fatalf("status = %s (%s); want local-only, naming the nested tree", ps.Status, ps.Detail)
	}
}
