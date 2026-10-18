// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package syncengine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/syshlted/drivel/internal/state"
	"github.com/syshlted/drivel/provider"
)

// §6 on a backend with no content digest (SFTP, M17–M21). The pull used to skip
// the conflict check whenever the remote reported no hash, so a local edit not
// yet pushed was overwritten by the next remote change, with no copy anywhere.

// hashlessChange is a remote change as SFTP reports one: a version, a size,
// a modification time, and no digest.
func hashlessChange(p, body string, modified time.Time) provider.RemoteChange {
	return provider.RemoteChange{Path: p, File: &provider.RemoteFile{
		Path: p, Size: int64(len(body)), Version: "v2", Modified: modified,
	}}
}

// syncedHashless records a baseline for rel as the pull would after applying it:
// the remote's version and the local file's fingerprint.
func syncedHashless(t *testing.T, st *state.Store, dir, rel string) {
	t.Helper()
	fi, err := os.Stat(filepath.Join(dir, rel))
	if err != nil {
		t.Fatal(err)
	}
	size, mtime := state.FingerprintOf(fi)
	if err := st.SetEcho(rel, state.Echo{Version: "v1", At: time.Now(), LocalSize: size, LocalMTime: mtime}); err != nil {
		t.Fatal(err)
	}
}

func copiesOf(t *testing.T, dir, stem string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), stem+" (conflict ") {
			out = append(out, e.Name())
		}
	}
	return out
}

// THE case: synced, edited here, changed remotely before the edit was pushed.
// The local edit is newer, so it keeps the path and the remote's version is
// kept beside it. Drop the hashless branch and the edit is overwritten.
func TestAHashlessRemoteChangeDoesNotOverwriteALocalEdit(t *testing.T) {
	dir := t.TempDir()
	st := newState(t)
	fs := newFakeStore()
	writeFile(t, dir, "a.txt", "base")
	syncedHashless(t, st, dir, "a.txt")
	writeFile(t, dir, "a.txt", "edited here")
	fs.content["a.txt"] = []byte("edited there")
	d := NewDownloader(nil, fs, dir, st, DefaultCadence)

	if err := d.apply(context.Background(), hashlessChange("a.txt", "edited there", time.Now().Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}
	if got, _ := readBacking(t, dir, "a.txt"); got != "edited here" {
		t.Fatalf("local = %q; the unpushed edit was overwritten", got)
	}
	copies := copiesOf(t, dir, "a")
	if len(copies) != 1 {
		t.Fatalf("conflict copies = %v; want the remote's version kept", copies)
	}
	if got, _ := readBacking(t, dir, copies[0]); got != "edited there" {
		t.Errorf("conflict copy = %q; want the remote's bytes", got)
	}
}

// Synced and untouched here: the remote change applies as it always did.
func TestAHashlessRemoteChangeAppliesOverAnUntouchedFile(t *testing.T) {
	dir := t.TempDir()
	st := newState(t)
	fs := newFakeStore()
	writeFile(t, dir, "a.txt", "base")
	syncedHashless(t, st, dir, "a.txt")
	fs.content["a.txt"] = []byte("edited there")
	d := NewDownloader(nil, fs, dir, st, DefaultCadence)

	if err := d.apply(context.Background(), hashlessChange("a.txt", "edited there", time.Now())); err != nil {
		t.Fatal(err)
	}
	if got, _ := readBacking(t, dir, "a.txt"); got != "edited there" {
		t.Fatalf("local = %q; want the remote change applied", got)
	}
	if c := copiesOf(t, dir, "a"); len(c) != 0 {
		t.Errorf("conflict copies = %v; nothing was edited here", c)
	}
}

// No baseline, the same bytes on both sides — a first sync of a tree that
// already mirrors the server. Recorded, not rewritten, and no copy.
func TestAHashlessFirstSyncOfIdenticalBytesRecordsThem(t *testing.T) {
	dir := t.TempDir()
	st := newState(t)
	fs := newFakeStore()
	writeFile(t, dir, "a.txt", "same")
	fs.content["a.txt"] = []byte("same")
	d := NewDownloader(nil, fs, dir, st, DefaultCadence)

	if err := d.apply(context.Background(), hashlessChange("a.txt", "same", time.Now())); err != nil {
		t.Fatal(err)
	}
	if c := copiesOf(t, dir, "a"); len(c) != 0 {
		t.Errorf("conflict copies = %v; the two sides already agreed", c)
	}
	if _, ok, _ := st.GetEcho("a.txt"); !ok {
		t.Error("no baseline recorded for a file found to match")
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 1 {
		t.Errorf("backing dir holds %d entries; the comparison's temporary was left behind", len(ents))
	}
}

// No baseline and different bytes: neither side is known to be synced, so
// neither is thrown away.
func TestAHashlessFirstSyncOfDifferentBytesKeepsBoth(t *testing.T) {
	dir := t.TempDir()
	st := newState(t)
	fs := newFakeStore()
	writeFile(t, dir, "a.txt", "mine")
	fs.content["a.txt"] = []byte("them")
	d := NewDownloader(nil, fs, dir, st, DefaultCadence)

	if err := d.apply(context.Background(), hashlessChange("a.txt", "them", time.Now().Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}
	if got, _ := readBacking(t, dir, "a.txt"); got != "mine" {
		t.Fatalf("local = %q; want it kept", got)
	}
	if c := copiesOf(t, dir, "a"); len(c) != 1 {
		t.Errorf("conflict copies = %v; want the remote's version kept", c)
	}
}
