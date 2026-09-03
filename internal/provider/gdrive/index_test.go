package gdrive

import (
	"context"
	"testing"

	drive "google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
)

// newIndex builds a Drive with just the in-memory path↔ID index populated (no
// svc), which is all the index helpers touch.
var ctx = context.Background()

func newIndex() *Drive {
	return &Drive{
		root:     "ROOT",
		idByPath: map[string]string{"": "ROOT"},
		pathByID: map[string]string{"ROOT": ""},
	}
}

// assertIndexConsistent checks idByPath and pathByID are exact inverses.
func assertIndexConsistent(t *testing.T, d *Drive) {
	t.Helper()
	if len(d.idByPath) != len(d.pathByID) {
		t.Fatalf("index sizes differ: idByPath=%v pathByID=%v", d.idByPath, d.pathByID)
	}
	for p, id := range d.idByPath {
		if got := d.pathByID[id]; got != p {
			t.Fatalf("inconsistent: idByPath[%q]=%q but pathByID[%q]=%q", p, id, id, got)
		}
	}
}

func TestRememberIndexesBothDirections(t *testing.T) {
	d := newIndex()
	rf := d.rememberLocked(ctx, "a/b.txt", &drive.File{
		Id:           "id-b",
		Name:         "b.txt",
		Md5Checksum:  "abc123",
		Version:      7,
		Size:         42,
		ModifiedTime: "2026-07-17T10:00:00Z",
	})

	if d.idByPath["a/b.txt"] != "id-b" || d.pathByID["id-b"] != "a/b.txt" {
		t.Fatalf("remember did not index both ways: %v / %v", d.idByPath, d.pathByID)
	}
	if rf.Path != "a/b.txt" || rf.Hash != "abc123" || rf.Version != "7" || rf.Size != 42 {
		t.Fatalf("RemoteFile view wrong: %+v", rf)
	}
	if rf.IsDir {
		t.Fatal("plain file reported as dir")
	}
	if rf.Modified.IsZero() {
		t.Fatal("modified time not parsed")
	}
	assertIndexConsistent(t, d)
}

func TestForgetLockedDropsSubtree(t *testing.T) {
	d := newIndex()
	d.rememberLocked(ctx, "a", &drive.File{Id: "id-a", MimeType: folderMIME})
	d.rememberLocked(ctx, "a/b.txt", &drive.File{Id: "id-b"})
	d.rememberLocked(ctx, "a/sub/c.txt", &drive.File{Id: "id-c"})
	d.rememberLocked(ctx, "other.txt", &drive.File{Id: "id-o"})

	d.forgetLocked(ctx, "a")

	for _, gone := range []string{"a", "a/b.txt", "a/sub/c.txt"} {
		if _, ok := d.idByPath[gone]; ok {
			t.Errorf("%q survived forget of its subtree", gone)
		}
	}
	if _, ok := d.idByPath["other.txt"]; !ok {
		t.Error("unrelated sibling other.txt was dropped")
	}
	// A path that is only a prefix-string (not a real ancestor) must not be caught.
	assertIndexConsistent(t, d)
}

// forgetLocked keys on the "p/" boundary, so "ab" must not be dropped when "a" is
// forgotten (string-prefix false positive guard).
func TestForgetLockedRespectsPathBoundary(t *testing.T) {
	d := newIndex()
	d.rememberLocked(ctx, "a", &drive.File{Id: "id-a", MimeType: folderMIME})
	d.rememberLocked(ctx, "ab", &drive.File{Id: "id-ab"})

	d.forgetLocked(ctx, "a")

	if _, ok := d.idByPath["ab"]; !ok {
		t.Fatal("forgetLocked(\"a\") wrongly dropped sibling \"ab\"")
	}
	assertIndexConsistent(t, d)
}

func TestReindexLockedMovesSubtree(t *testing.T) {
	d := newIndex()
	d.rememberLocked(ctx, "dir", &drive.File{Id: "id-dir", MimeType: folderMIME})
	d.rememberLocked(ctx, "dir/f.txt", &drive.File{Id: "id-f"})
	d.rememberLocked(ctx, "dir/sub/g.txt", &drive.File{Id: "id-g"})

	d.reindexLocked(ctx, "dir", "moved")

	want := map[string]string{"moved": "id-dir", "moved/f.txt": "id-f", "moved/sub/g.txt": "id-g"}
	for p, id := range want {
		if d.idByPath[p] != id {
			t.Errorf("after reindex idByPath[%q]=%q; want %q", p, d.idByPath[p], id)
		}
	}
	for _, gone := range []string{"dir", "dir/f.txt", "dir/sub/g.txt"} {
		if _, ok := d.idByPath[gone]; ok {
			t.Errorf("old key %q survived reindex", gone)
		}
	}
	if d.pathByID["id-g"] != "moved/sub/g.txt" {
		t.Errorf("reverse index not rewritten: %q", d.pathByID["id-g"])
	}
	assertIndexConsistent(t, d)
}

func TestToRemoteFileFolder(t *testing.T) {
	rf := toRemoteFile("d", &drive.File{Id: "x", MimeType: folderMIME, Version: 1})
	if !rf.IsDir {
		t.Fatal("folder MIME not reported as dir")
	}
	// A missing/blank ModifiedTime leaves a zero time rather than erroring.
	if !rf.Modified.IsZero() {
		t.Fatal("blank modifiedTime should yield zero time")
	}
}

func TestIsNotFound(t *testing.T) {
	if !isNotFound(&googleapi.Error{Code: 404}) {
		t.Error("404 should be not-found")
	}
	if isNotFound(&googleapi.Error{Code: 500}) {
		t.Error("500 is not not-found")
	}
	if isNotFound(nil) {
		t.Error("nil is not not-found")
	}
}
