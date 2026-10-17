// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package syncengine

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/syshlted/drivel/internal/claim"
	"github.com/syshlted/drivel/internal/fsevent"
	"github.com/syshlted/drivel/internal/state"
	"github.com/syshlted/drivel/provider"
)

// A claim marker is never pushed, at the root or below it. Below it is the case
// that spreads: a second drivel whose backing tree is inside this mount writes
// its marker through FUSE, and once uploaded every other client's walk would
// prune that directory from every sweep.
func TestAClaimMarkerIsNeverPushed(t *testing.T) {
	dataDir := t.TempDir()
	fs := newFakeStore()
	e := New(Config{Store: fs, DataDir: dataDir})
	ctx := context.Background()

	writeFile(t, dataDir, claim.Name, "pid 1\n")
	writeFile(t, dataDir, "nested/"+claim.Name, "pid 2\n")
	writeFile(t, dataDir, "user.txt", "x")
	e.handle(ctx, fsevent.Event{Op: fsevent.OpCreate, Path: claim.Name})
	e.handle(ctx, fsevent.Event{Op: fsevent.OpWrite, Path: "nested/" + claim.Name})
	e.handle(ctx, fsevent.Event{Op: fsevent.OpRename, Path: "user.txt", NewPath: claim.Name})
	e.handle(ctx, fsevent.Event{Op: fsevent.OpUnlink, Path: claim.Name})

	assertCalls(t, fs.calls, nil)
}

// A remote object named like a marker neither overwrites this mount's live one
// nor, when the remote deletes it, removes it.
func TestARemoteObjectNamedLikeAClaimMarkerIsNotApplied(t *testing.T) {
	dir := t.TempDir()
	fs := newFakeStore()
	fs.content[claim.Name] = []byte("from elsewhere")
	d := NewDownloader(nil, fs, dir, newState(t), DefaultCadence)
	writeFile(t, dir, claim.Name, "pid 1\n")
	ctx := context.Background()

	if err := d.apply(ctx, provider.RemoteChange{Path: claim.Name, File: remoteFile(claim.Name, "from elsewhere")}); err != nil {
		t.Fatal(err)
	}
	if got, _ := readBacking(t, dir, claim.Name); got != "pid 1\n" {
		t.Fatalf("marker = %q after a remote create; the live marker was overwritten", got)
	}
	if err := d.apply(ctx, provider.RemoteChange{Path: claim.Name, Removed: true}); err != nil {
		t.Fatal(err)
	}
	if _, ok := readBacking(t, dir, claim.Name); !ok {
		t.Fatal("a remote deletion removed the live marker")
	}
	if _, ok, _ := d.state.GetEcho(claim.Name); ok {
		t.Error("an echo was recorded for a marker")
	}
}

// Another drivel's state DB inside this backing tree, opened before this mount
// started — the one shape of MC-47 no claim check sees, since the database is an
// ordinary file of any name. bbolt holds an exclusive flock on it while it is
// open, and an upload of it would publish a file caught mid-transaction, on
// every commit. Closed, it is an ordinary file again and is pushed.
func TestALiveDatabaseInTheBackingTreeIsNotPushed(t *testing.T) {
	dataDir := t.TempDir()
	other, err := state.Open(filepath.Join(dataDir, "theirs.db"))
	if err != nil {
		t.Fatal(err)
	}
	fs := newFakeStore()
	e := New(Config{Store: fs, DataDir: dataDir})
	ctx := context.Background()

	e.handle(ctx, fsevent.Event{Op: fsevent.OpWrite, Path: "theirs.db"})
	assertCalls(t, fs.calls, nil)

	if err := other.Close(); err != nil {
		t.Fatal(err)
	}
	e.handle(ctx, fsevent.Event{Op: fsevent.OpWrite, Path: "theirs.db"})
	if len(fs.calls) != 1 {
		t.Fatalf("calls = %v; a database nobody holds open is an ordinary file and should be pushed", fs.calls)
	}
}
