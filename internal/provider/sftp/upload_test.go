// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package sftp

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/syshlted/drivel/provider"
)

// The object takes the time it was given — whole seconds, SFTP's resolution —
// and it is the one in the RemoteFile, so the engine's baseline and the next
// listing agree.
func TestUploadCarriesTheModificationTime(t *testing.T) {
	ts := startServer(t)
	s := ts.open(t)
	when := time.Date(2003, 4, 5, 6, 7, 8, 0, time.UTC)
	up, err := s.Upload(t.Context(), "dated.txt", strings.NewReader("x"), provider.UploadOptions{Modified: when})
	if err != nil {
		t.Fatal(err)
	}
	if !up.File.Modified.Equal(when) {
		t.Errorf("RemoteFile.Modified = %v; want %v", up.File.Modified, when)
	}
	fi, err := os.Stat(ts.path("dated.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !fi.ModTime().Equal(when) {
		t.Errorf("the server's file is dated %v; want %v", fi.ModTime(), when)
	}
	if listed, ok, _ := s.Stat(t.Context(), "dated.txt"); !ok || listed.Version != up.File.Version {
		t.Errorf("Stat's version %q differs from Upload's %q", listed.Version, up.File.Version)
	}
}

// The case Replaces exists for: somebody else's write lands while ours is
// uploading. The rename would replace it unseen; the check before the rename
// refuses, and their file stays.
func TestUploadRefusesToReplaceAWriteMadeDuringIt(t *testing.T) {
	ts := startServer(t)
	s := ts.open(t)
	seen, err := s.Put(t.Context(), "doc.txt", strings.NewReader("as we last saw it"))
	if err != nil {
		t.Fatal(err)
	}
	r := &raceReader{body: "ours", before: func() {
		// A different length, so the version moves even inside one second.
		_ = os.WriteFile(ts.path("doc.txt"), []byte("theirs, written meanwhile"), 0o644)
	}}
	_, err = s.Upload(t.Context(), "doc.txt", r, provider.UploadOptions{Replaces: &seen})
	if !errors.Is(err, provider.ErrChanged) {
		t.Fatalf("Upload over a file changed during it = %v; want ErrChanged", err)
	}
	if b, _ := os.ReadFile(ts.path("doc.txt")); string(b) != "theirs, written meanwhile" {
		t.Errorf("their write was replaced: %q", b)
	}
	if left := tempsIn(t, ts.path("")); len(left) != 0 {
		t.Errorf("a refused upload left %v behind", left)
	}

	// Removed meanwhile counts as changed too: re-creating it is the engine's
	// call, made with the remote in view.
	_ = os.Remove(ts.path("doc.txt"))
	if _, err := s.Upload(t.Context(), "doc.txt", strings.NewReader("ours"), provider.UploadOptions{Replaces: &seen}); !errors.Is(err, provider.ErrChanged) {
		t.Fatalf("Upload over a file removed meanwhile = %v; want ErrChanged", err)
	}
}

// And when nothing moved, it replaces as Put would.
func TestUploadReplacesWhatItExpected(t *testing.T) {
	ts := startServer(t)
	s := ts.open(t)
	seen, err := s.Put(t.Context(), "doc.txt", strings.NewReader("old"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Upload(t.Context(), "doc.txt", strings.NewReader("new"), provider.UploadOptions{Replaces: &seen}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(ts.path("doc.txt")); string(b) != "new" {
		t.Errorf("doc.txt = %q; want new", b)
	}
}

func tempsIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if isTemp(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out
}
