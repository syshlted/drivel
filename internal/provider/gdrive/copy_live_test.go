// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package gdrive

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/syshlted/drivel/provider"
)

// What the conflict policy's server-side copy assumes of real Drive, and no fake
// can settle: that files.copy into a folder that does not exist yet produces a
// second object with the source's bytes and digest, that the source is left
// alone, that an occupied destination is refused rather than duplicated, and
// that trashing the copy (remote-trash's second step) leaves the original
// visible. Off unless DRIVEL_LIVE_RIG is set; it writes, so it refuses "root",
// and it deletes everything it made.
func TestLiveCopy(t *testing.T) {
	if os.Getenv("DRIVEL_LIVE_RIG") == "" {
		t.Skip("set DRIVEL_LIVE_RIG=1 with -creds/-token/-root env to run")
	}
	root := os.Getenv("DRIVEL_LIVE_ROOT")
	if root == "" || root == "root" {
		t.Fatal("DRIVEL_LIVE_ROOT must name a dedicated folder ID, never \"root\"")
	}
	lctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	cfg := Config{
		Credentials: os.Getenv("DRIVEL_CREDS"),
		Token:       os.Getenv("DRIVEL_TOKEN"),
		RootID:      root,
	}
	d, err := Open(lctx, cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	const (
		name = "drivel-live-copy-probe.txt"
		dir  = "drivel-live-copy-dir"
		dst  = dir + "/.drivel-conflicts/drivel-live-copy-probe (conflict).txt"
		body = "the loser's bytes"
	)
	del := func(what, id string) {
		t.Cleanup(func() {
			ctx, c := context.WithTimeout(context.Background(), 30*time.Second)
			defer c()
			if err := d.svc.Files.Delete(id).Context(ctx).Do(); err != nil {
				t.Logf("cleanup: %s (%s) left behind: %v", what, id, err)
			}
		})
	}

	src, err := d.Put(lctx, name, strings.NewReader(body))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	srcID, ok := resolve(t, d, name)
	if !ok {
		t.Fatalf("just-written %s does not resolve", name)
	}
	del(name, srcID)

	cp, err := d.Copy(lctx, name, dst)
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	if dirID, ok := resolve(t, d, dir); ok {
		del(dir, dirID)
	}
	dstID, ok := resolve(t, d, dst)
	if !ok {
		t.Fatalf("copy returned but %s does not resolve", dst)
	}
	if dstID == srcID {
		t.Fatalf("copy resolves to the source object %s", srcID)
	}
	if cp.Hash == "" || cp.Hash != src.Hash {
		t.Errorf("copy digest %q, source %q: the copy is not the same bytes", cp.Hash, src.Hash)
	}
	if cp.Size != int64(len(body)) {
		t.Errorf("copy size %d, want %d", cp.Size, len(body))
	}
	rc, err := d.Get(lctx, dst)
	if err != nil {
		t.Fatalf("get copy: %v", err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(got) != body {
		t.Errorf("copy holds %q, want %q", got, body)
	}

	// An occupied destination is refused, never a same-name sibling (MC-30).
	if _, err := d.Copy(lctx, name, dst); !errors.Is(err, provider.ErrExist) {
		t.Errorf("copy onto an occupied path: %v, want ErrExist", err)
	}

	// remote-trash's second step: the copy goes to the trash, the original stays.
	if err := d.Remove(lctx, dst); err != nil {
		t.Fatalf("remove copy: %v", err)
	}
	f, err := d.svc.Files.Get(dstID).Fields("trashed").Context(lctx).Do()
	if err != nil {
		t.Fatalf("the trashed copy is gone outright: %v", err)
	}
	if !f.Trashed {
		t.Errorf("the copy survived its removal untrashed")
	}
	peer, err := Open(lctx, cfg)
	if err != nil {
		t.Fatalf("open peer: %v", err)
	}
	t.Cleanup(func() { _ = peer.Close() })
	if rf, ok, err := peer.Stat(lctx, name); err != nil || !ok {
		t.Fatalf("the source no longer resolves for a fresh client: ok=%v err=%v", ok, err)
	} else if rf.Hash != src.Hash {
		t.Errorf("the source changed: %q, was %q", rf.Hash, src.Hash)
	}
}
