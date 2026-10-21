// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package gdrive

import (
	"crypto/md5"
	"encoding/hex"
	"io"
	"strings"
	"testing"
	"time"

	"google.golang.org/api/drive/v3"

	"github.com/syshlted/drivel/internal/provider/gdrive/gdconf"
	"github.com/syshlted/drivel/provider"
)

func md5Of(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// The local modification time rides in the request that carries the content,
// for a create and for an update alike, and comes back as the file's.
func TestUploadCarriesTheModificationTime(t *testing.T) {
	d, fake := newFakeDrive(t)
	when := time.Date(2004, 5, 6, 7, 8, 9, 123000000, time.UTC)
	up, err := d.Upload(ctx, "dated.txt", strings.NewReader("x"), provider.UploadOptions{Modified: when})
	if err != nil {
		t.Fatal(err)
	}
	if !up.File.Modified.Equal(when) {
		t.Fatalf("created file's Modified = %v; want %v", up.File.Modified, when)
	}
	later := when.Add(time.Hour)
	up, err = d.Upload(ctx, "dated.txt", strings.NewReader("y"), provider.UploadOptions{Modified: later})
	if err != nil {
		t.Fatal(err)
	}
	if !up.File.Modified.Equal(later) {
		t.Fatalf("updated file's Modified = %v; want %v", up.File.Modified, later)
	}
	if n := fake.updateCount(); n != 1 {
		t.Fatalf("%d update(s) for one rewrite; the time must not cost a second request", n)
	}
}

// Drive has no conditional update, so a write landing during ours is found
// afterwards, as a revision that appeared while the upload ran, and reported
// with a version GetVersion reads — without marking anything kept forever,
// which Drive will not let it take back.
func TestUploadReportsAnEditItReplaced(t *testing.T) {
	d, fake := newFakeDrive(t)
	d.overwriteMode = OverwriteAlways
	seen := synced(t, d, "doc.txt", "as last synced")
	interlope(fake, "their edit, made meanwhile", "")

	up, err := d.Upload(ctx, "doc.txt", strings.NewReader("my edit"), provider.UploadOptions{Replaces: &seen})
	if err != nil {
		t.Fatal(err)
	}
	if len(up.Displaced) != 1 || up.Displaced[0].Hash != md5Of("their edit, made meanwhile") {
		t.Fatalf("displaced = %+v; want their edit", up.Displaced)
	}
	rc, err := d.GetVersion(ctx, "doc.txt", up.Displaced[0].Version)
	if err != nil {
		t.Fatalf("GetVersion: %v", err)
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("reading the displaced version: %v", err)
	}
	if err := rc.Close(); err != nil {
		t.Fatal(err)
	}
	if string(got) != "their edit, made meanwhile" {
		t.Fatalf("GetVersion = %q; want their edit", got)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.revUpd != 0 {
		t.Fatalf("%d revision update(s) to read a displaced version; want none", fake.revUpd)
	}
}

// The edit is found whatever it is dated. A writer carrying an old local
// modification time — an edit made offline yesterday, a `cp -p` — makes a
// revision Drive lists ahead of the one we replaced, so "the revision before
// ours" was what we expected to replace and the edit went unreported.
func TestAnEditDatedBeforeOursIsFound(t *testing.T) {
	d, fake := newFakeDrive(t)
	d.overwriteMode = OverwriteAlways
	seen := synced(t, d, "doc.txt", "as last synced")
	interlope(fake, "an edit made offline last year", "2025-01-01T00:00:00Z")

	up, err := d.Upload(ctx, "doc.txt", strings.NewReader("my edit"), provider.UploadOptions{Replaces: &seen, Modified: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if len(up.Displaced) != 1 || up.Displaced[0].Hash != md5Of("an edit made offline last year") {
		t.Fatalf("displaced = %+v; want the back-dated edit", up.Displaced)
	}
}

// And nothing is found where nothing happened, however the history is dated.
// A file whose last rewrite carried an old time (restored from a backup, say)
// has that revision listed first, and the revision listed before the next
// rewrite's is then some older one — which was reported as somebody's edit,
// and became a conflict copy of a version nobody had just written. The live
// rig did exactly that.
func TestARewriteOverABackDatedOneDisplacesNothing(t *testing.T) {
	d, fake := newFakeDrive(t)
	d.overwriteMode = OverwriteAlways
	synced(t, d, "doc.txt", "first")
	restored := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := d.Upload(ctx, "doc.txt", strings.NewReader("restored"), provider.UploadOptions{Modified: restored}); err != nil {
		t.Fatal(err)
	}
	seen, _, _ := d.Stat(ctx, "doc.txt")

	up, err := d.Upload(ctx, "doc.txt", strings.NewReader("edited"), provider.UploadOptions{Replaces: &seen})
	if err != nil {
		t.Fatal(err)
	}
	if len(up.Displaced) != 0 {
		t.Fatalf("displaced = %+v; nobody else wrote the file", up.Displaced)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.revList != 2 {
		t.Fatalf("%d revision listing(s) for a checked rewrite; want 2", fake.revList)
	}
}

// An edit during the upload that wrote what we expected to replace, or what we
// were writing, loses nobody's work, so it is no conflict.
func TestAnEditOfTheSameBytesIsNoConflict(t *testing.T) {
	for _, same := range []string{"as last synced", "my edit"} {
		d, fake := newFakeDrive(t)
		d.overwriteMode = OverwriteAlways
		seen := synced(t, d, "doc.txt", "as last synced")
		interlope(fake, same, "")
		up, err := d.Upload(ctx, "doc.txt", strings.NewReader("my edit"), provider.UploadOptions{Replaces: &seen})
		if err != nil {
			t.Fatal(err)
		}
		if len(up.Displaced) != 0 {
			t.Errorf("an edit writing %q during the upload was reported displaced: %+v", same, up.Displaced)
		}
	}
}

// A change in between that was not content — a rename, a share, or Drive's own
// bookkeeping, which moves the version with nobody writing — makes no revision,
// so nothing is reported.
func TestAMetadataChangeInBetweenDisplacesNothing(t *testing.T) {
	d, fake := newFakeDrive(t)
	d.overwriteMode = OverwriteAlways
	seen := synced(t, d, "doc.txt", "old")
	fake.mu.Lock()
	fake.interloper = func(f *fakeDrive, file *drive.File) {
		f.interloper = nil
		file.Version += 3 // metadata only
	}
	fake.mu.Unlock()
	up, err := d.Upload(ctx, "doc.txt", strings.NewReader("new"), provider.UploadOptions{Replaces: &seen})
	if err != nil {
		t.Fatal(err)
	}
	if len(up.Displaced) != 0 {
		t.Fatalf("displaced = %+v after a metadata-only change", up.Displaced)
	}
}

// overwrite-check decides which rewrites pay the two listings. Each case has an
// edit landing during the upload, so whether it was found is whether the
// rewrite was checked.
func TestOverwriteCheckFollowsTheMode(t *testing.T) {
	small, large := "a small rewrite", strings.Repeat("x", gdconf.OverwriteCheckMinSize)
	for _, tc := range []struct {
		mode    OverwriteMode
		body    string
		checked bool
	}{
		{"", small, false},
		{"", large, true},
		{OverwriteLarge, small, false},
		{OverwriteLarge, large[:len(large)-1], false},
		{OverwriteLarge, large, true},
		{OverwriteAlways, small, true},
		{OverwriteOff, small, false},
		{OverwriteOff, large, false},
	} {
		d, fake := newFakeDrive(t)
		d.overwriteMode = tc.mode
		seen := synced(t, d, "doc.txt", "as last synced")
		interlope(fake, "their edit", "")
		up, err := d.Upload(ctx, "doc.txt", strings.NewReader(tc.body), provider.UploadOptions{Replaces: &seen})
		if err != nil {
			t.Fatal(err)
		}
		id := d.idByPath["doc.txt"]
		fake.mu.Lock()
		got, lists := fake.content[id], fake.revList
		fake.mu.Unlock()
		if string(got) != tc.body {
			t.Fatalf("mode %q, %d bytes: the remote holds %d bytes, not the upload", tc.mode, len(tc.body), len(got))
		}
		if want := map[bool]int{true: 2, false: 0}[tc.checked]; lists != want {
			t.Errorf("mode %q, %d bytes: %d revision listing(s); want %d", tc.mode, len(tc.body), lists, want)
		}
		if found := len(up.Displaced) == 1; found != tc.checked {
			t.Errorf("mode %q, %d bytes: the edit made during the upload found = %v; want %v", tc.mode, len(tc.body), found, tc.checked)
		}
	}
}

// A first upload and a Put expect nothing, so neither is checked.
func TestOnlyARewriteWithAnExpectationIsChecked(t *testing.T) {
	d, fake := newFakeDrive(t)
	d.overwriteMode = OverwriteAlways
	if _, err := d.Upload(ctx, "doc.txt", strings.NewReader("new"), provider.UploadOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Put(ctx, "doc.txt", strings.NewReader("newer")); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.revList != 0 {
		t.Fatalf("%d revision listing(s) for uploads that expected nothing; want none", fake.revList)
	}
}

// An unreadable overwrite-check is refused at open (M8 rule 6): a misspelled
// "off" would go on paying for checks, and a misspelled "always" would leave
// unchecked the small files it was set for.
func TestOpenRejectsAnUnknownOverwriteCheck(t *testing.T) {
	_, err := Open(ctx, Config{Credentials: "unused.json", OverwriteCheck: "sometimes"})
	if err == nil {
		t.Fatal("open accepted overwrite-check \"sometimes\"")
	}
	if !strings.Contains(err.Error(), "overwrite-check") {
		t.Fatalf("error %q does not name the option that was wrong", err)
	}
}

func TestGetVersionRefusesAVersionItNeverReported(t *testing.T) {
	d, _ := newFakeDrive(t)
	if _, err := d.Put(ctx, "doc.txt", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := d.GetVersion(ctx, "doc.txt", "17"); err == nil {
		t.Fatal("GetVersion accepted a file version counter as a revision")
	}
}

// synced writes body to p and returns what a Stat then reports, as the engine
// holds it before a rewrite.
func synced(t *testing.T, d *Drive, p, body string) provider.RemoteFile {
	t.Helper()
	if _, err := d.Put(ctx, p, strings.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	seen, _, err := d.Stat(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	return seen
}

// interlope lands another client's write of body inside the next update with a
// body, dated as given ("" for the server's clock).
func interlope(fake *fakeDrive, body, dated string) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.interloper = func(f *fakeDrive, file *drive.File) {
		f.interloper = nil
		f.writeBodyLocked(file, []byte(body))
		f.datedLocked(file, dated)
	}
}
