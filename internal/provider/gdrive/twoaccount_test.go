package gdrive

import (
	"path/filepath"
	"sync"
	"testing"

	"github.com/zishmusic/drivel/internal/pathindex"
)

// M8's seam proof, below the seam: two independently-configured Drive stores in
// one process. If anything Drive-shaped were process-global — a cached root ID, a
// shared path map, an index that does not know whose it is — two accounts holding
// the same paths under different file IDs is what surfaces it, and the failure
// would be one account's data written over the other's.
//
// It needs no credentials and no network: newFakeDrive builds a real *Drive
// against an httptest server, so the second registration M8 calls for costs
// nothing to ship because it never ships.
func twoAccounts(t *testing.T) (a, b *Drive) {
	t.Helper()
	// Same paths on both sides, different IDs and different owners — the shape
	// that makes cross-talk visible rather than merely possible.
	a, fa := newFakeDrive(t,
		folder("docs-a", "docs", fakeRootID),
		file("file-a", "report.txt", "docs-a"),
	)
	b, fb := newFakeDrive(t,
		folder("docs-b", "docs", fakeRootID),
		file("file-b", "report.txt", "docs-b"),
	)
	fa.permID, fb.permID = "perm-alpha", "perm-beta"

	dir := t.TempDir()
	for _, x := range []struct {
		d    *Drive
		name string
	}{{a, "alpha"}, {b, "beta"}} {
		idx, err := pathindex.Open(filepath.Join(dir, x.name+".db"))
		if err != nil {
			t.Fatalf("opening %s index: %v", x.name, err)
		}
		t.Cleanup(func() { _ = idx.Close() })
		x.d.idx = idx
	}
	return a, b
}

func TestTwoDrivesResolveTheSamePathToTheirOwnFile(t *testing.T) {
	a, b := twoAccounts(t)

	ra, ok, err := a.Stat(ctx, "docs/report.txt")
	if err != nil || !ok {
		t.Fatalf("alpha Stat: %v, ok=%v", err, ok)
	}
	rb, ok, err := b.Stat(ctx, "docs/report.txt")
	if err != nil || !ok {
		t.Fatalf("beta Stat: %v, ok=%v", err, ok)
	}
	// The fake derives each file's checksum from its ID, so equal hashes here
	// would mean one account resolved to the other's object.
	if ra.Hash == rb.Hash {
		t.Fatalf("both accounts resolved to the same object (%s); the stores are not independent", ra.Hash)
	}
	if ra.Hash != "hash-file-a" {
		t.Errorf("alpha resolved to %s; want file-a", ra.Hash)
	}
	if rb.Hash != "hash-file-b" {
		t.Errorf("beta resolved to %s; want file-b", rb.Hash)
	}

	// The in-memory maps must hold each account's own IDs, with nothing of the
	// other's anywhere in them.
	if got := a.idByPath["docs/report.txt"]; got != "file-a" {
		t.Errorf("alpha's map holds %q for docs/report.txt", got)
	}
	if got := b.idByPath["docs/report.txt"]; got != "file-b" {
		t.Errorf("beta's map holds %q for docs/report.txt", got)
	}
	for id := range a.pathByID {
		if id == "file-b" || id == "docs-b" {
			t.Errorf("beta's ID %q leaked into alpha's index", id)
		}
	}
}

// The persistent index is bound to (account, root), and the two accounts must
// produce different identities — this is what stops a reused index file from
// reading one account's paths as another's IDs (M7).
func TestTwoDrivesBindDistinctIdentities(t *testing.T) {
	a, b := twoAccounts(t)

	ia, err := a.identityLocked(ctx)
	if err != nil {
		t.Fatalf("alpha identity: %v", err)
	}
	ib, err := b.identityLocked(ctx)
	if err != nil {
		t.Fatalf("beta identity: %v", err)
	}
	if ia == ib {
		t.Fatalf("both accounts bind the same identity %q", ia)
	}
}

// Two mounts in one process run their engines concurrently, so the stores are
// used concurrently. -race is the assertion.
func TestTwoDrivesConcurrentUse(t *testing.T) {
	a, b := twoAccounts(t)

	var wg sync.WaitGroup
	for _, d := range []*Drive{a, b} {
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, _, err := d.Stat(ctx, "docs/report.txt"); err != nil {
					t.Errorf("Stat: %v", err)
				}
			}()
		}
	}
	wg.Wait()

	// Still each other's opposite after all that.
	if a.idByPath["docs/report.txt"] == b.idByPath["docs/report.txt"] {
		t.Fatal("concurrent use collapsed the two accounts onto one file ID")
	}
}

// A write through one account must not appear in the other's index.
func TestTwoDrivesDoNotSeeEachOthersMutations(t *testing.T) {
	a, b := twoAccounts(t)

	if _, err := a.Mkdir(ctx, "shared-name"); err != nil {
		t.Fatalf("alpha Mkdir: %v", err)
	}
	if _, ok := b.idByPath["shared-name"]; ok {
		t.Error("alpha's mkdir appeared in beta's index")
	}
	if _, ok := a.idByPath["shared-name"]; !ok {
		t.Error("alpha's mkdir did not appear in its own index")
	}
}
