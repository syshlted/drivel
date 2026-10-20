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
// afterwards, in the revision history, and reported with a version GetVersion
// reads — which it can only do once the revision is kept, and it un-keeps it
// after.
func TestUploadReportsAnEditItReplaced(t *testing.T) {
	d, fake := newFakeDrive(t)
	if _, err := d.Put(ctx, "doc.txt", strings.NewReader("as last synced")); err != nil {
		t.Fatal(err)
	}
	seen, _, err := d.Stat(ctx, "doc.txt")
	if err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.interloper = func(f *fakeDrive, file *drive.File) {
		f.interloper = nil
		f.writeBodyLocked(file, []byte("their edit, made meanwhile"))
	}
	fake.mu.Unlock()

	up, err := d.Upload(ctx, "doc.txt", strings.NewReader("my edit"), provider.UploadOptions{Replaces: &seen})
	if err != nil {
		t.Fatal(err)
	}
	if up.Displaced == nil {
		t.Fatal("an edit replaced during the upload was not reported")
	}
	if up.Displaced.Hash != md5Of("their edit, made meanwhile") {
		t.Fatalf("displaced = %+v; want their edit", up.Displaced)
	}
	rc, err := d.GetVersion(ctx, "doc.txt", up.Displaced.Version)
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
	id := d.idByPath["doc.txt"]
	for _, rev := range fake.revs[id] {
		if rev.KeepForever {
			t.Errorf("revision %s is still kept forever after it was read", rev.Id)
		}
	}
}

// The common case costs nothing: nobody wrote in between, the version moved by
// exactly one, and no revision listing is made.
func TestAnUncontestedUploadListsNoRevisions(t *testing.T) {
	d, fake := newFakeDrive(t)
	if _, err := d.Put(ctx, "doc.txt", strings.NewReader("old")); err != nil {
		t.Fatal(err)
	}
	seen, _, _ := d.Stat(ctx, "doc.txt")
	up, err := d.Upload(ctx, "doc.txt", strings.NewReader("new"), provider.UploadOptions{Replaces: &seen})
	if err != nil {
		t.Fatal(err)
	}
	if up.Displaced != nil {
		t.Fatalf("displaced = %+v; nothing was written in between", up.Displaced)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.revList != 0 {
		t.Fatalf("%d revision listing(s) for an uncontested upload; want none", fake.revList)
	}
}

// A change in between that was not content — a rename, a share — moves the
// version too. The revision before ours still holds what was expected, so
// nothing is reported displaced.
func TestAMetadataChangeInBetweenDisplacesNothing(t *testing.T) {
	d, fake := newFakeDrive(t)
	if _, err := d.Put(ctx, "doc.txt", strings.NewReader("old")); err != nil {
		t.Fatal(err)
	}
	seen, _, _ := d.Stat(ctx, "doc.txt")
	fake.mu.Lock()
	fake.interloper = func(f *fakeDrive, file *drive.File) {
		f.interloper = nil
		file.Version++ // metadata only
	}
	fake.mu.Unlock()
	up, err := d.Upload(ctx, "doc.txt", strings.NewReader("new"), provider.UploadOptions{Replaces: &seen})
	if err != nil {
		t.Fatal(err)
	}
	if up.Displaced != nil {
		t.Fatalf("displaced = %+v after a metadata-only change", up.Displaced)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.revList != 1 {
		t.Fatalf("%d revision listing(s); the moved version should have cost exactly one", fake.revList)
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
