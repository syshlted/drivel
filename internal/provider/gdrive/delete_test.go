package gdrive

import (
	"strings"
	"testing"
)

// The default removal puts the object in the trash rather than deleting it, and
// from above the seam the two are indistinguishable: the path stops resolving
// either way.
//
// That last half is what makes trashing safe to default to. Every query the
// resolver issues carries "trashed = false", so a trashed object is as absent as
// a deleted one to lookups, to the sweep and to the change feed — the only
// difference is that the bytes are still somewhere a user can reach them.
func TestRemoveTrashesByDefault(t *testing.T) {
	doomed := file("id-a", "a.txt", fakeRootID)
	d, fake := newFakeDrive(t, doomed)

	if err := d.Remove(ctx, "a.txt"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if !fake.has(doomed.Id) {
		t.Fatal("the default removal deleted the object; want it trashed")
	}
	if !fake.trashed(doomed.Id) {
		t.Fatal("the object survived the removal untrashed")
	}
	if _, _, _, deletes := fake.counts(); deletes != 0 {
		t.Errorf("Files.Delete called %d times; want 0", deletes)
	}

	// A fresh client has none of the first one's cached mappings, so this asks
	// Drive itself whether the path is still there.
	if _, ok := resolve(t, fake.client(t), "a.txt"); ok {
		t.Error("a trashed path still resolves; the removal is not visible as one")
	}
}

// -drive-delete permanent is the old behaviour, unlinking the object outright.
func TestRemovePermanentlyDeletesWhenAsked(t *testing.T) {
	doomed := file("id-a", "a.txt", fakeRootID)
	d, fake := newFakeDrive(t, doomed)
	d.deleteMode = DeletePermanent

	if err := d.Remove(ctx, "a.txt"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if fake.has(doomed.Id) {
		t.Fatal("permanent delete left the object behind")
	}
	if _, _, _, deletes := fake.counts(); deletes != 1 {
		t.Errorf("Files.Delete called %d times; want 1", deletes)
	}
}

// Trashing a folder carries its subtree, which is the recursion Store.Remove
// documents. This asserts the settled state, which is all anything above the seam
// can observe; real Drive gets there asynchronously, and TestLiveRemoveTrashes is
// what pins that. Drive's `trashed` means "explicitly, or from a trashed parent", so
// a child of a trashed folder is invisible to every listing the provider makes —
// and a fake that trashed only the named object would let a subtree removal look
// complete while the children were still being enumerated.
func TestRemovingADirectoryTrashesItsSubtree(t *testing.T) {
	dir := folder("id-d", "sub", fakeRootID)
	child := file("id-c", "c.txt", "id-d")
	d, fake := newFakeDrive(t, dir, child)

	if err := d.Remove(ctx, "sub"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if !fake.trashed(dir.Id) {
		t.Error("the directory was not trashed")
	}
	if !fake.trashed(child.Id) {
		t.Error("the directory was trashed but its child was left visible")
	}
}

// What one client trashes, every other client must delete.
//
// This is the half of the change that reaches the fleet, and it is load-bearing
// in a way it was not before: a permanent delete arrives in changes.list as
// Removed, which nothing could misread, while a trashed file arrives as an
// ordinary change carrying a file that happens to say trashed. toRemoteChange
// has always read that as a removal — but until removals were trashings, nothing
// depended on it. If it regressed, deletions would simply stop propagating and
// every peer would keep its copy until a sweep inferred the delete, up to
// -sweep-interval later.
func TestATrashedFileReachesTheFeedAsARemoval(t *testing.T) {
	d, fake := newFakeDrive(t, file("f1", "x.txt", fakeRootID))

	// The peer is a second machine watching the same folder, with its own maps —
	// it has to resolve x.txt before it can name the path a removal refers to,
	// which is exactly what a running mount has done by the time this matters.
	peer := fake.client(t)
	if _, ok, err := peer.Stat(ctx, "x.txt"); err != nil || !ok {
		t.Fatalf("peer Stat(x.txt) = ok %v, err %v", ok, err)
	}
	cursor, err := peer.StartCursor(ctx)
	if err != nil {
		t.Fatalf("StartCursor: %v", err)
	}

	if err := d.Remove(ctx, "x.txt"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	// The change Drive raises for the trashing. The fake records changes only when
	// a test asks it to — the same convention its renames use — so this stands in
	// for the entry the API would have appended itself.
	fake.touch("f1")

	chs, _, err := peer.Changes(ctx, cursor)
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	present, removed := changed(t, chs)
	if !contains(removed, "x.txt") {
		t.Errorf("the feed did not report x.txt as removed; got removed %v, present %v. "+
			"A trashed file every peer still holds is a deletion that never propagated", removed, present)
	}
}

// An unreadable delete mode is refused at open, for the reason M8 rule 6 gives:
// a misspelling that fell back to the default would silently give the operator
// the opposite of what they asked for, in the one direction that loses files.
func TestOpenRejectsUnknownDeleteMode(t *testing.T) {
	_, err := Open(ctx, Config{Credentials: "unused.json", Delete: "bin"})
	if err == nil {
		t.Fatal("open accepted delete mode \"bin\"")
	}
	if !strings.Contains(err.Error(), "delete") {
		t.Fatalf("error %q does not name the option that was wrong", err)
	}
}
