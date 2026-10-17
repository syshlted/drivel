// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package gdrive

import (
	"testing"

	"github.com/syshlted/drivel/provider"
)

// The change feed's shape, above the seam.
//
// Drive's changes.list is keyed by object identity: one entry per fileID that
// changed, carrying that object's metadata *now*. provider.RemoteChange is
// keyed by path and carries no identity, so the translation between them is
// where a rename either survives or is lost. That translation is
// toRemoteChange, and until these tests there was nothing exercising it — the
// fake answered /changes with an empty page.

// changed is the paths a batch of changes touches, split into what appeared and
// what went away, which is all a caller above the seam can act on.
func changed(t *testing.T, chs []provider.RemoteChange) (present, removed []string) {
	t.Helper()
	for _, ch := range chs {
		if ch.Removed || ch.File == nil {
			removed = append(removed, ch.Path)
			continue
		}
		present = append(present, ch.Path)
	}
	return present, removed
}

func contains(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}

// A rename is the one remote mutation the seam cannot express in a single
// change: the object still exists, so nothing is "removed", but the path it used
// to occupy is now empty and every client watching the feed has a file sitting
// there. Drive will never say so — its change is about the object, and the
// object is fine — so the provider has to say it, from the mapping it already
// keeps.
//
// Without the removal, a rename on one client duplicates the file on every
// other: the new path is downloaded and the old one is left behind until an
// enumeration sweep infers the delete, which is -sweep-interval away (24h by
// default). See TestFleetRenameLeavesNoStalePathOnOtherPeers in internal/app for
// the same contract asserted from above.
func TestChangesReportsTheOldPathOfARenamedFile(t *testing.T) {
	d, fake := newFakeDrive(t, file("f1", "x.txt", fakeRootID))

	// Resolve it once, as a running mount does the moment it touches the path.
	// This is what makes the old path known; nothing else in the change can.
	if _, ok, err := d.Stat(ctx, "x.txt"); err != nil || !ok {
		t.Fatalf("Stat(x.txt) = ok %v, err %v", ok, err)
	}

	cursor, err := d.StartCursor(ctx)
	if err != nil {
		t.Fatalf("StartCursor: %v", err)
	}
	fake.rename("f1", "y.txt", "")

	chs, _, err := d.Changes(ctx, cursor)
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	present, removed := changed(t, chs)

	if !contains(present, "y.txt") {
		t.Errorf("the feed did not report the new path y.txt; got present %v", present)
	}
	if !contains(removed, "x.txt") {
		t.Errorf("the feed did not report that x.txt is gone; got removed %v. "+
			"Every other client keeps the pre-rename file until a sweep infers the delete", removed)
	}
}

// The same for a move between folders: the old path is in a different directory,
// which is the case where leaving the stale copy behind is most visible.
func TestChangesReportsTheOldPathOfAMovedFile(t *testing.T) {
	d, fake := newFakeDrive(t,
		folder("dir-a", "a", fakeRootID),
		folder("dir-b", "b", fakeRootID),
		file("f1", "doc.txt", "dir-a"),
	)
	if _, ok, err := d.Stat(ctx, "a/doc.txt"); err != nil || !ok {
		t.Fatalf("Stat(a/doc.txt) = ok %v, err %v", ok, err)
	}

	cursor, err := d.StartCursor(ctx)
	if err != nil {
		t.Fatalf("StartCursor: %v", err)
	}
	fake.rename("f1", "doc.txt", "dir-b")

	chs, _, err := d.Changes(ctx, cursor)
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	present, removed := changed(t, chs)

	if !contains(present, "b/doc.txt") {
		t.Errorf("the feed did not report the destination b/doc.txt; got present %v", present)
	}
	if !contains(removed, "a/doc.txt") {
		t.Errorf("the feed did not report that a/doc.txt is gone; got removed %v", removed)
	}
}

// An edit in place must NOT report a removal. The old path and the new path are
// the same one, and a removal for a path that is about to be re-reported is a
// window in which every client deletes a file it is about to download again —
// and, worse, drops the echo record that makes the re-download recognisable.
func TestChangesDoesNotReportARemovalForAnEditInPlace(t *testing.T) {
	d, fake := newFakeDrive(t, file("f1", "x.txt", fakeRootID))
	if _, ok, err := d.Stat(ctx, "x.txt"); err != nil || !ok {
		t.Fatalf("Stat(x.txt) = ok %v, err %v", ok, err)
	}

	cursor, err := d.StartCursor(ctx)
	if err != nil {
		t.Fatalf("StartCursor: %v", err)
	}
	fake.touch("f1")

	chs, _, err := d.Changes(ctx, cursor)
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	present, removed := changed(t, chs)

	if !contains(present, "x.txt") {
		t.Errorf("the feed did not report the edited file; got present %v", present)
	}
	if len(removed) != 0 {
		t.Errorf("an edit in place reported removals %v; the file never left its path", removed)
	}
}

// A file this mount has never resolved has no old path to report. Reporting one
// anyway — guessed from a stale hint, say — would delete whatever the other
// client happens to have there, so the absence of a removal here is the safe
// answer and not merely an omission.
func TestChangesReportsNoRemovalForAnUnknownFile(t *testing.T) {
	d, fake := newFakeDrive(t, file("f1", "x.txt", fakeRootID))

	cursor, err := d.StartCursor(ctx)
	if err != nil {
		t.Fatalf("StartCursor: %v", err)
	}
	fake.touch("f1")

	chs, _, err := d.Changes(ctx, cursor)
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	present, removed := changed(t, chs)

	if !contains(present, "x.txt") {
		t.Errorf("the feed did not report the file; got present %v", present)
	}
	if len(removed) != 0 {
		t.Errorf("a first sighting reported removals %v", removed)
	}
}

// A folder move is reported AS a move, and still never as a removal.
//
// Both halves are load-bearing and they are two different facts. The removal is
// wrong for the reason it always was: above the seam it is a recursive local
// delete, and Drive reports no changes for the children of a moved folder, so
// nothing would bring them back. Saying nothing about the old path was the only
// other option until the seam could carry a move, and MC-27 measured what it
// cost at 1100 files — the receiving client downloads the subtree a second time
// and then cannot delete the first copy, because the deletions exceed
// -max-deletes and the pass refuses itself.
func TestChangesReportsAFolderMoveAsAMove(t *testing.T) {
	d, fake := newFakeDrive(t,
		folder("dir-a", "a", fakeRootID),
		file("f1", "doc.txt", "dir-a"),
	)
	if _, ok, err := d.Stat(ctx, "a/doc.txt"); err != nil || !ok {
		t.Fatalf("Stat(a/doc.txt) = ok %v, err %v", ok, err)
	}

	cursor, err := d.StartCursor(ctx)
	if err != nil {
		t.Fatalf("StartCursor: %v", err)
	}
	fake.rename("dir-a", "renamed", "")

	chs, _, err := d.Changes(ctx, cursor)
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	_, removed := changed(t, chs)

	if contains(removed, "a") {
		t.Error("a folder move reported its old path as removed; applying that deletes the whole local subtree, " +
			"and Drive reports no changes for the children that would restore it")
	}
	moved := ""
	for _, ch := range chs {
		if ch.Path == "renamed" {
			moved = ch.MovedFrom
		}
	}
	if moved != "a" {
		t.Errorf("the change at renamed carries MovedFrom %q, want %q — without it the host has nothing "+
			"connecting the new name to the subtree it already holds", moved, "a")
	}
}

// A file move keeps the pair it has always had, and must not ALSO claim to be a
// move. Both forms converge for a file, the pair is the one an older host
// understands, and a host that acted on both would be told to rename a path it
// had just been told to delete.
func TestChangesReportsAFileMoveAsAPairAndNotAsAMove(t *testing.T) {
	d, fake := newFakeDrive(t,
		folder("dir-a", "a", fakeRootID),
		folder("dir-b", "b", fakeRootID),
		file("f1", "doc.txt", "dir-a"),
	)
	if _, ok, err := d.Stat(ctx, "a/doc.txt"); err != nil || !ok {
		t.Fatalf("Stat(a/doc.txt) = ok %v, err %v", ok, err)
	}
	cursor, err := d.StartCursor(ctx)
	if err != nil {
		t.Fatalf("StartCursor: %v", err)
	}
	fake.rename("f1", "doc.txt", "dir-b")

	chs, _, err := d.Changes(ctx, cursor)
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	_, removed := changed(t, chs)
	if !contains(removed, "a/doc.txt") {
		t.Errorf("the pair is gone: removed %v, want a/doc.txt", removed)
	}
	for _, ch := range chs {
		if ch.MovedFrom != "" {
			t.Errorf("the change at %s carries MovedFrom %q; a file move is the pair, not a move",
				ch.Path, ch.MovedFrom)
		}
	}
}

// The index follows the subtree, not just the folder. Drive reports nothing for
// the children of a moved folder, so a descendant left keyed under the path it
// has left resolves to a live ID whose parent no longer matches — which
// verifyLocked correctly rejects, at the cost of a name query per file.
func TestAFolderMoveReindexesTheSubtree(t *testing.T) {
	d, fake := newFakeDrive(t,
		folder("dir-a", "a", fakeRootID),
		file("f1", "doc.txt", "dir-a"),
	)
	if _, ok, err := d.Stat(ctx, "a/doc.txt"); err != nil || !ok {
		t.Fatalf("Stat(a/doc.txt) = ok %v, err %v", ok, err)
	}
	cursor, err := d.StartCursor(ctx)
	if err != nil {
		t.Fatalf("StartCursor: %v", err)
	}
	fake.rename("dir-a", "renamed", "")
	if _, _, err := d.Changes(ctx, cursor); err != nil {
		t.Fatalf("Changes: %v", err)
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if got := d.idByPath["renamed/doc.txt"]; got != "f1" {
		t.Errorf("renamed/doc.txt resolves to %q, want f1", got)
	}
	if got, ok := d.idByPath["a/doc.txt"]; ok {
		t.Errorf("a/doc.txt still resolves to %q after the folder moved", got)
	}
	if got := d.pathByID["f1"]; got != "renamed/doc.txt" {
		t.Errorf("f1 reverse-maps to %q, want renamed/doc.txt", got)
	}
}
