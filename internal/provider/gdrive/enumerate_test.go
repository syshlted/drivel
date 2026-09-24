// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package gdrive

import (
	"testing"

	drive "google.golang.org/api/drive/v3"

	"github.com/syshlted/drivel/provider"
)

// sweep drives a full enumeration and returns every object it emitted, keyed by
// path, plus the number of pages it took.
func sweep(t *testing.T, d *Drive) (map[string]provider.RemoteFile, int) {
	t.Helper()
	out := map[string]provider.RemoteFile{}
	cursor := ""
	pages := 0
	for {
		files, next, err := d.Enumerate(ctx, cursor)
		if err != nil {
			t.Fatalf("enumerate: %v", err)
		}
		pages++
		for _, f := range files {
			if _, dup := out[f.Path]; dup {
				t.Fatalf("path %q emitted twice", f.Path)
			}
			out[f.Path] = f
		}
		if next == "" {
			return out, pages
		}
		cursor = next
		if pages > 20 {
			t.Fatal("enumeration did not terminate")
		}
	}
}

// A flat listing has no parent-before-child guarantee. A child that arrives
// before its parent — even a page earlier — still gets the right path, and the
// grandchild behind it comes along when the chain completes.
func TestEnumerateResolvesChildrenListedBeforeParents(t *testing.T) {
	d, fake := newFakeDrive(t,
		folder("id-dir", "dir", fakeRootID),
		folder("id-sub", "sub", "id-dir"),
		file("id-deep", "deep.txt", "id-sub"),
		file("id-top", "top.txt", fakeRootID),
	)
	// Strictly child-first: the deepest object is listed first and the folder that
	// gives it a path arrives two pages later.
	fake.listOrder = []string{"id-deep", "id-sub", "id-top", "id-dir"}

	got, pages := sweep(t, d)
	if pages != 2 {
		t.Fatalf("pages = %d; want 2 (4 objects at %d per page)", pages, fakeEnumPage)
	}
	for _, want := range []string{"top.txt", "dir", "dir/sub", "dir/sub/deep.txt"} {
		if _, ok := got[want]; !ok {
			t.Fatalf("path %q missing from the sweep; got %v", want, keys(got))
		}
	}
	if len(got) != 4 {
		t.Fatalf("emitted %d objects; want 4: %v", len(got), keys(got))
	}
	if !got["dir"].IsDir || got["dir/sub/deep.txt"].IsDir {
		t.Fatalf("directory flag wrong: %+v / %+v", got["dir"], got["dir/sub/deep.txt"])
	}
}

// A listing covers the whole account, so most of what it returns for a subfolder
// mount is outside it. Anything whose parent chain never reaches our root is
// dropped rather than guessed at — the same rule the change feed uses.
func TestEnumerateDropsObjectsOutsideTheMountRoot(t *testing.T) {
	d, fake := newFakeDrive(t,
		folder("id-mine", "mine", fakeRootID),
		file("id-in", "in.txt", "id-mine"),
		folder("id-other", "other", fakeRootID),
		file("id-out", "out.txt", "id-other"),
		file("id-loose", "loose.txt", "id-vanished"), // parent not in the listing at all
	)
	// Mount the "mine" folder rather than My Drive.
	//
	// Pinned to the flat sweep on purpose: a concrete root selects the M7c descent
	// under auto, which never lists what is outside the root and so would pass this
	// test without ever exercising the rule it is named for.
	d.sweepMode = SweepFlat
	d.root = "id-mine"
	d.rootID = ""
	d.idByPath = map[string]string{"": "id-mine"}
	d.pathByID = map[string]string{"id-mine": ""}
	fake.listOrder = []string{"id-in", "id-out", "id-loose", "id-mine", "id-other"}

	got, _ := sweep(t, d)
	if _, ok := got["in.txt"]; !ok {
		t.Fatalf("file under the mount root was dropped: %v", keys(got))
	}
	if len(got) != 1 {
		t.Fatalf("emitted %v; want only in.txt — everything else is outside the mount", keys(got))
	}
}

// Google-native docs are reported, not hidden: the sweep is what tells the
// reconciler the remote still has them (so it never reads their absence as a
// delete), and ExportOnly is what tells it not to try to materialise them.
func TestEnumerateFlagsGoogleNativeDocs(t *testing.T) {
	doc := file("id-doc", "notes", fakeRootID)
	doc.MimeType = "application/vnd.google-apps.document"
	doc.Md5Checksum = ""
	doc.Size = 0
	d, _ := newFakeDrive(t, doc, file("id-bin", "bin.txt", fakeRootID))

	got, _ := sweep(t, d)
	if !got["notes"].ExportOnly {
		t.Fatalf("google doc not flagged ExportOnly: %+v", got["notes"])
	}
	if got["bin.txt"].ExportOnly {
		t.Fatalf("ordinary file flagged ExportOnly: %+v", got["bin.txt"])
	}
}

// The sweep doubles as index warm-up: what it resolved is in the persistent index
// afterwards, so the next session resolves those paths without asking Drive.
func TestEnumerateWarmsPersistentIndex(t *testing.T) {
	d, _ := newFakeDrive(t,
		folder("id-dir", "dir", fakeRootID),
		file("id-f", "f.txt", "id-dir"),
	)
	withIndex(t, d)
	idx := bindIndex(t, d)

	sweep(t, d)

	for path, want := range map[string]string{"dir": "id-dir", "dir/f.txt": "id-f"} {
		got, ok, err := idx.Lookup(path)
		if err != nil || !ok || got != want {
			t.Fatalf("index[%q] = %q, ok=%v, err=%v; want %q", path, got, ok, err, want)
		}
	}
}

// Resuming a sweep needs the pages a previous process consumed, and the only
// record of those is the persistent index. With no index there is no way to place
// a child whose parent was listed before the restart, and a sweep that silently
// omits a subtree is worse than a slow one — the reconcile above would read the
// gap as "deleted remotely". So it restarts instead.
func TestEnumerateRestartsResumeWithoutAnIndex(t *testing.T) {
	d, fake := newFakeDrive(t,
		folder("id-dir", "dir", fakeRootID),
		file("id-a", "a.txt", "id-dir"),
		file("id-b", "b.txt", "id-dir"),
		file("id-c", "c.txt", "id-dir"),
	)
	fake.listOrder = []string{"id-dir", "id-a", "id-b", "id-c"}

	// A fresh process handed the cursor a previous one persisted mid-sweep.
	files, next, err := d.Enumerate(ctx, "page-2")
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	if next != "page-2" {
		t.Fatalf("next = %q; want page-2 — the sweep should have restarted from the first page", next)
	}
	if len(files) != 2 || files[0].Path != "dir" || files[1].Path != "dir/a.txt" {
		t.Fatalf("restarted sweep returned %v; want the first page (dir, dir/a.txt)", paths(files))
	}
}

// With an index the resume is real: the folder from the skipped page is
// recovered from it (and verified, per M7) rather than restarting the sweep.
func TestEnumerateResumesUsingTheIndex(t *testing.T) {
	d, fake := newFakeDrive(t,
		folder("id-dir", "dir", fakeRootID),
		file("id-a", "a.txt", "id-dir"),
		file("id-b", "b.txt", "id-dir"),
		file("id-c", "c.txt", "id-dir"),
	)
	fake.listOrder = []string{"id-dir", "id-a", "id-b", "id-c"}
	withIndex(t, d)
	idx := bindIndex(t, d)
	// What the interrupted run had already learned.
	if err := idx.Set("dir", "id-dir"); err != nil {
		t.Fatal(err)
	}
	// Nothing in memory: this stands in for a fresh process.
	d.idByPath = map[string]string{"": "root"}
	d.pathByID = map[string]string{"root": ""}

	files, next, err := d.Enumerate(ctx, "page-2")
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	if next != "" {
		t.Fatalf("next = %q; want \"\" — page-2 is the last page", next)
	}
	if got := paths(files); len(got) != 2 || got[0] != "dir/b.txt" || got[1] != "dir/c.txt" {
		t.Fatalf("resumed sweep returned %v; want dir/b.txt, dir/c.txt", got)
	}
}

// A sweep is idempotent, so a dead listing token costs a restart rather than
// leaving enumeration permanently stuck on it.
func TestEnumerateRestartsOnExpiredPageToken(t *testing.T) {
	d, fake := newFakeDrive(t, file("id-a", "a.txt", fakeRootID))
	fake.listOrder = []string{"id-a"}
	withIndex(t, d)
	bindIndex(t, d)
	fake.expirePageTokens = true

	files, next, err := d.Enumerate(ctx, "page-1")
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	if next != "" || len(files) != 1 || files[0].Path != "a.txt" {
		t.Fatalf("enumerate = %v, next=%q; want the whole listing from a restarted sweep", paths(files), next)
	}
}

// isExportOnly is what separates "no content" from "empty content"; a folder is
// neither.
func TestIsExportOnly(t *testing.T) {
	cases := map[string]bool{
		"application/vnd.google-apps.document":    true,
		"application/vnd.google-apps.spreadsheet": true,
		"application/vnd.google-apps.shortcut":    true,
		folderMIME:                                false,
		"text/plain":                              false,
		"":                                        false,
	}
	for mime, want := range cases {
		if got := isExportOnly(mime); got != want {
			t.Errorf("isExportOnly(%q) = %v; want %v", mime, got, want)
		}
	}
}

func keys(m map[string]provider.RemoteFile) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func paths(files []provider.RemoteFile) []string {
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.Path
	}
	return out
}

var _ = drive.File{}
