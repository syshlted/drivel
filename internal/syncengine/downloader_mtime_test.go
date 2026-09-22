// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package syncengine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zishmusic/drivel/provider"
)

// A downloaded file carries the remote's modifiedTime, not the time it landed.
//
// This is load-bearing twice over. resolveConflict compares the remote's
// Modified against the local mtime, so a file stamped "now" on arrival would
// look strictly newer than the remote that produced it, and the next sweep would
// push it straight back — the ping-pong MC-32 hunts for, arriving through the
// download path instead of a real conflict. And it is what makes two *pulling*
// peers agree with each other about a file's timestamp at all.
//
// What it does NOT do — and the campaign's convergence check depends on knowing
// this — is make the whole fleet agree. The peer that originated the write keeps
// its own local write time; Drive stamps the upload with a time of its own, and
// nothing writes that back down over the originator's copy. So for every file,
// one client's mtime differs from everyone else's, permanently and by design.
// docs/dev/multiclient-test-plan.md §2.6 defines convergence on content for exactly
// this reason.
func TestDownloadCarriesTheRemoteModifiedTime(t *testing.T) {
	dir := t.TempDir()
	fs := newFakeStore()
	st := newState(t)
	fs.content["doc.txt"] = []byte("remote-body")

	// Truncated to the second: a filesystem is not obliged to store more, and the
	// assertion is about which instant was recorded, not about its precision.
	remoteTime := time.Now().Add(-90 * time.Minute).Truncate(time.Second)

	d := NewDownloader(nil, fs, dir, st, DefaultCadence)
	remote := &provider.RemoteFile{
		Path: "doc.txt", Hash: md5hex("remote-body"), Version: "1", Modified: remoteTime,
	}
	if err := d.apply(context.Background(), provider.RemoteChange{Path: "doc.txt", File: remote}); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(filepath.Join(dir, "doc.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.ModTime().Truncate(time.Second); !got.Equal(remoteTime) {
		t.Fatalf("downloaded doc.txt has mtime %v; want the remote's %v — a file stamped on arrival looks newer than the remote it came from, and gets pushed back",
			got, remoteTime)
	}
}
