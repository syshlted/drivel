package gdrive

import (
	"path/filepath"
	"testing"

	drive "google.golang.org/api/drive/v3"

	"github.com/zishmusic/drivel/internal/pathindex"
)

// withIndex attaches a persistent index backed by a temp file.
func withIndex(t *testing.T, d *Drive) *pathindex.Store {
	t.Helper()
	idx, err := pathindex.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	d.idx = idx
	return idx
}

// bind forces the lazy identity handshake so a test can seed the index with the
// same identity the provider will bind to.
func bindIndex(t *testing.T, d *Drive) *pathindex.Store {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	idx := d.indexLocked(ctx)
	if idx == nil {
		t.Fatal("index did not bind")
	}
	return idx
}

func resolve(t *testing.T, d *Drive, p string) (string, bool) {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.resolveLocked(ctx, p)
}

// The M7 headline: with nothing in memory and nothing on disk, a path that
// exists remotely still resolves — by asking Drive for it. Before M7 this
// returned "absent", and the caller then created a duplicate beside the real
// file.
func TestResolveFindsExistingRemoteFileWithColdIndex(t *testing.T) {
	d, _ := newFakeDrive(t,
		folder("id-dir", "dir", fakeRootID),
		file("id-f", "f.txt", "id-dir"),
	)

	id, ok := resolve(t, d, "dir/f.txt")
	if !ok || id != "id-f" {
		t.Fatalf("resolve = %q, %v; want id-f, true", id, ok)
	}
	// Both the file and the folder it walked through are now cached in memory.
	if d.idByPath["dir"] != "id-dir" || d.pathByID["id-f"] != "dir/f.txt" {
		t.Fatalf("resolution did not populate the in-memory index: %v / %v", d.idByPath, d.pathByID)
	}
}

func TestResolveReportsAbsentPath(t *testing.T) {
	d, _ := newFakeDrive(t, folder("id-dir", "dir", fakeRootID))

	if id, ok := resolve(t, d, "dir/nope.txt"); ok {
		t.Fatalf("absent path resolved to %q", id)
	}
	// A missing parent short-circuits: there is no child to look for.
	if id, ok := resolve(t, d, "no-such-dir/f.txt"); ok {
		t.Fatalf("path under a missing folder resolved to %q", id)
	}
}

// Stat is the gate M6's unchanged-content check sits behind, so a false "absent"
// there means re-uploading a file that is already on Drive byte for byte.
func TestStatFindsRemoteFileAfterRestart(t *testing.T) {
	d, _ := newFakeDrive(t,
		folder("id-dir", "dir", fakeRootID),
		file("id-f", "f.txt", "id-dir"),
	)

	rf, ok, err := d.Stat(ctx, "dir/f.txt")
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !ok {
		t.Fatal("Stat reported a file that exists remotely as absent")
	}
	if rf.Hash != "hash-id-f" || rf.Path != "dir/f.txt" {
		t.Fatalf("Stat returned %+v", rf)
	}
}

// Creating a folder that already exists remotely gives Drive two same-named
// siblings and splits the subtree in half — the parent-side version of the
// duplicate-file bug.
func TestEnsureDirReusesExistingRemoteFolder(t *testing.T) {
	d, fake := newFakeDrive(t, folder("id-dir", "dir", fakeRootID))

	d.mu.Lock()
	id, err := d.ensureDirLocked(ctx, "dir")
	d.mu.Unlock()
	if err != nil {
		t.Fatalf("ensureDir: %v", err)
	}
	if id != "id-dir" {
		t.Fatalf("ensureDir = %q; want the existing id-dir", id)
	}
	if _, _, creates, _ := fake.counts(); creates != 0 {
		t.Fatalf("ensureDir created %d folder(s) over an existing one", creates)
	}
}

func TestEnsureDirCreatesWhenGenuinelyMissing(t *testing.T) {
	d, fake := newFakeDrive(t)

	d.mu.Lock()
	id, err := d.ensureDirLocked(ctx, "fresh")
	d.mu.Unlock()
	if err != nil {
		t.Fatalf("ensureDir: %v", err)
	}
	// The ID is opaque — the fake mints a fresh one per create, as Drive does —
	// so the assertion is that the folder it named is the one now sitting under
	// the root, not that it has any particular spelling.
	kids := fake.namedChildren(fakeRootID, "fresh")
	if len(kids) != 1 || kids[0].Id != id {
		t.Fatalf("ensureDir = %q; the root holds %v under that name", id, kids)
	}
	if _, _, creates, _ := fake.counts(); creates != 1 {
		t.Fatalf("creates = %d; want 1", creates)
	}
}

// A persisted mapping that still holds is used as-is: one verification request,
// no name query.
func TestPersistedEntryIsUsedAfterVerification(t *testing.T) {
	d, fake := newFakeDrive(t,
		folder("id-dir", "dir", fakeRootID),
		file("id-f", "f.txt", "id-dir"),
	)
	withIndex(t, d)
	idx := bindIndex(t, d)
	if err := idx.Set("dir", "id-dir"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := idx.Set("dir/f.txt", "id-f"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	fake.reset()

	id, ok := resolve(t, d, "dir/f.txt")
	if !ok || id != "id-f" {
		t.Fatalf("resolve = %q, %v; want id-f, true", id, ok)
	}
	if _, lists, _, _ := fake.counts(); lists != 0 {
		t.Errorf("a valid persisted entry still cost %d name quer(ies)", lists)
	}
}

// The hazard persistence introduces that the in-memory index never had: while
// drivel was down, that ID was renamed. It still resolves — to a file the user
// never meant us to write. Handing it to Files.Update would overwrite that file
// with no conflict copy, so the entry must be rejected and dropped.
func TestPersistedEntryRejectedWhenRemoteMovedOn(t *testing.T) {
	renamed := file("id-f", "renamed.txt", fakeRootID)
	d, _ := newFakeDrive(t, renamed)
	withIndex(t, d)
	idx := bindIndex(t, d)
	if err := idx.Set("f.txt", "id-f"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if id, ok := resolve(t, d, "f.txt"); ok {
		t.Fatalf("stale entry was believed: resolve = %q", id)
	}
	if _, ok, err := idx.Lookup("f.txt"); err != nil || ok {
		t.Errorf("stale entry survived in the index (ok=%v, err=%v)", ok, err)
	}
	// The real object is still findable under the name it now has.
	if id, ok := resolve(t, d, "renamed.txt"); !ok || id != "id-f" {
		t.Fatalf("resolve of the new name = %q, %v; want id-f, true", id, ok)
	}
}

// Same check, the other way it fails: the object is still named f.txt but now
// lives in another folder.
func TestPersistedEntryRejectedWhenParentChanged(t *testing.T) {
	moved := file("id-f", "f.txt", "id-other")
	d, _ := newFakeDrive(t,
		folder("id-dir", "dir", fakeRootID),
		folder("id-other", "other", fakeRootID),
		moved,
	)
	withIndex(t, d)
	idx := bindIndex(t, d)
	if err := idx.Set("dir/f.txt", "id-f"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if id, ok := resolve(t, d, "dir/f.txt"); ok {
		t.Fatalf("entry with a changed parent was believed: resolve = %q", id)
	}
	if id, ok := resolve(t, d, "other/f.txt"); !ok || id != "id-f" {
		t.Fatalf("resolve at the new location = %q, %v; want id-f, true", id, ok)
	}
}

// A deleted ID is a 404 on verification: drop it and fall back to the name query.
func TestPersistedEntryRejectedWhenRemoteDeleted(t *testing.T) {
	d, _ := newFakeDrive(t)
	withIndex(t, d)
	idx := bindIndex(t, d)
	if err := idx.Set("gone.txt", "id-gone"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if id, ok := resolve(t, d, "gone.txt"); ok {
		t.Fatalf("entry for a deleted object resolved to %q", id)
	}
	if _, ok, _ := idx.Lookup("gone.txt"); ok {
		t.Error("entry for a deleted object survived in the index")
	}
}

// A directory that is not where we left it invalidates everything recorded
// beneath it, not just its own entry.
func TestStaleDirectoryDropsItsSubtree(t *testing.T) {
	d, _ := newFakeDrive(t, folder("id-dir", "renamed-dir", fakeRootID))
	withIndex(t, d)
	idx := bindIndex(t, d)
	for p, id := range map[string]string{"dir": "id-dir", "dir/a.txt": "id-a", "dir/sub/b.txt": "id-b"} {
		if err := idx.Set(p, id); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	if _, ok := resolve(t, d, "dir"); ok {
		t.Fatal("renamed directory still resolved at its old path")
	}
	for _, p := range []string{"dir", "dir/a.txt", "dir/sub/b.txt"} {
		if _, ok, _ := idx.Lookup(p); ok {
			t.Errorf("%q survived invalidation of its parent directory", p)
		}
	}
}

// Resolution writes through, so what this session learns is there for the next
// one.
func TestResolutionWritesThroughToTheIndex(t *testing.T) {
	d, _ := newFakeDrive(t,
		folder("id-dir", "dir", fakeRootID),
		file("id-f", "f.txt", "id-dir"),
	)
	withIndex(t, d)
	idx := bindIndex(t, d)

	if _, ok := resolve(t, d, "dir/f.txt"); !ok {
		t.Fatal("resolve failed")
	}
	for p, want := range map[string]string{"dir": "id-dir", "dir/f.txt": "id-f"} {
		got, ok, err := idx.Lookup(p)
		if err != nil || !ok || got != want {
			t.Errorf("index[%q] = %q, %v (err %v); want %q", p, got, ok, err, want)
		}
	}
}

// The change feed's direction. Without the persisted reverse map, naming an ID
// costs one request per level of nesting; with it, one verification.
func TestPathForIDUsesPersistedReverseIndex(t *testing.T) {
	deep := []*drive.File{
		folder("id-a", "a", fakeRootID),
		folder("id-b", "b", "id-a"),
		folder("id-c", "c", "id-b"),
	}

	d, fake := newFakeDrive(t, deep...)
	d.mu.Lock()
	p, ok := d.pathForIDLocked(ctx, "id-c")
	d.mu.Unlock()
	if !ok || p != "a/b/c" {
		t.Fatalf("walk resolved %q, %v; want a/b/c", p, ok)
	}
	walkGets, _, _, _ := fake.counts()

	withIdx, fake2 := newFakeDrive(t, deep...)
	withIndex(t, withIdx)
	idx := bindIndex(t, withIdx)
	for path, id := range map[string]string{"a": "id-a", "a/b": "id-b", "a/b/c": "id-c"} {
		if err := idx.Set(path, id); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	fake2.reset()

	withIdx.mu.Lock()
	p, ok = withIdx.pathForIDLocked(ctx, "id-c")
	withIdx.mu.Unlock()
	if !ok || p != "a/b/c" {
		t.Fatalf("indexed resolve = %q, %v; want a/b/c", p, ok)
	}
	indexedGets, _, _, _ := fake2.counts()

	if indexedGets >= walkGets {
		t.Errorf("persisted reverse index saved nothing: %d requests vs %d for the parent walk", indexedGets, walkGets)
	}
}

// Drive allows two files with the same name in one folder; a POSIX directory
// does not. One of them has to stand for the path, and the rule is "most
// recently modified".
func TestDuplicateNamesResolveToNewest(t *testing.T) {
	older := file("id-old", "dup.txt", fakeRootID)
	older.ModifiedTime = "2026-01-01T00:00:00Z"
	newer := file("id-new", "dup.txt", fakeRootID)
	newer.ModifiedTime = "2026-08-01T00:00:00Z"

	d, _ := newFakeDrive(t, older, newer)

	id, ok := resolve(t, d, "dup.txt")
	if !ok || id != "id-new" {
		t.Fatalf("resolve = %q, %v; want id-new (the most recently modified)", id, ok)
	}
}

func TestTrashedFileDoesNotResolve(t *testing.T) {
	trashed := file("id-t", "t.txt", fakeRootID)
	trashed.Trashed = true
	d, _ := newFakeDrive(t, trashed)

	if id, ok := resolve(t, d, "t.txt"); ok {
		t.Fatalf("trashed file resolved to %q", id)
	}
}

// Names with a quote or a backslash must survive being embedded in a Drive query
// string, or resolution silently fails (or, worse, matches something else).
func TestLookupEscapesQueryLiterals(t *testing.T) {
	odd := file("id-odd", `it's a \ file.txt`, fakeRootID)
	d, _ := newFakeDrive(t, odd)

	if id, ok := resolve(t, d, `it's a \ file.txt`); !ok || id != "id-odd" {
		t.Fatalf("resolve = %q, %v; want id-odd, true", id, ok)
	}
}

func TestEscapeQuery(t *testing.T) {
	for in, want := range map[string]string{
		`plain.txt`:  `plain.txt`,
		`it's`:       `it\'s`,
		`back\slash`: `back\\slash`,
		`both'\`:     `both\'\\`,
	} {
		if got := escapeQuery(in); got != want {
			t.Errorf("escapeQuery(%q) = %q; want %q", in, got, want)
		}
	}
}

// Removals arrive as a bare ID with no record attached, so they are the one case
// with nothing left to verify against — the stored path is all there is.
func TestKnownPathForIDAnswersDeletionsFromTheIndex(t *testing.T) {
	d, fake := newFakeDrive(t)
	withIndex(t, d)
	idx := bindIndex(t, d)
	if err := idx.Set("dir/gone.txt", "id-gone"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	fake.reset()

	d.mu.Lock()
	p, ok := d.knownPathForIDLocked(ctx, "id-gone")
	d.mu.Unlock()
	if !ok || p != "dir/gone.txt" {
		t.Fatalf("knownPathForID = %q, %v; want dir/gone.txt, true", p, ok)
	}
	if gets, lists, _, _ := fake.counts(); gets != 0 || lists != 0 {
		t.Errorf("naming a deleted file cost %d get(s) and %d list(s); want none", gets, lists)
	}

	d.mu.Lock()
	_, ok = d.knownPathForIDLocked(ctx, "id-never-seen")
	d.mu.Unlock()
	if ok {
		t.Error("an ID we have never recorded was claimed to be inside our subtree")
	}
}

// Binding is what stops one account's index from being read as another's.
func TestIndexBindsToAccountAndRoot(t *testing.T) {
	d, _ := newFakeDrive(t, file("id-f", "f.txt", fakeRootID))
	idx := withIndex(t, d)
	bindIndex(t, d)
	if err := idx.Set("f.txt", "id-f"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Same DB, same account, a different -drive-root: the mappings are meaningless
	// there and must not be reused.
	other, _ := newFakeDrive(t, file("id-f", "f.txt", fakeRootID))
	other.root = "some-other-folder"
	other.idx = idx
	other.mu.Lock()
	reboundIdx := other.indexLocked(ctx)
	other.mu.Unlock()
	if reboundIdx == nil {
		t.Fatal("index did not bind for the second root")
	}
	if _, ok, _ := idx.Lookup("f.txt"); ok {
		t.Error("mappings for one root survived being reused under another")
	}
}

// The index is a cache, so anything that goes wrong with it degrades to the
// memory-only behaviour of M2-M6 rather than failing an operation.
func TestResolutionWorksWithNoIndexAtAll(t *testing.T) {
	d, _ := newFakeDrive(t, file("id-f", "f.txt", fakeRootID))
	d.idx = nil

	if id, ok := resolve(t, d, "f.txt"); !ok || id != "id-f" {
		t.Fatalf("resolve without an index = %q, %v; want id-f, true", id, ok)
	}
}

// A file directly in the mount root has the *concrete* root ID as its parent,
// never the "root" alias that -drive-root defaults to. Resolving that parent has
// to recognise it as our root and stop, or the walk climbs to My Drive, finds a
// folder with no parents of its own, and reports the object as living outside
// the mounted subtree — which silently dropped every inbound change to a
// top-level file.
func TestPathForIDRecognisesTheRootUnderItsRealID(t *testing.T) {
	d, _ := newFakeDrive(t, file("id-top", "top.txt", fakeRootID))

	d.mu.Lock()
	p, ok := d.pathForIDLocked(ctx, fakeRootID)
	d.mu.Unlock()
	if !ok || p != "" {
		t.Fatalf("pathForID(concrete root) = %q, %v; want \"\", true", p, ok)
	}

	d.mu.Lock()
	p, ok = d.pathForIDLocked(ctx, "id-top")
	d.mu.Unlock()
	if !ok || p != "top.txt" {
		t.Fatalf("pathForID(top-level file) = %q, %v; want top.txt, true", p, ok)
	}
}
