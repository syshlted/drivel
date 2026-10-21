// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package gdrive

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/api/drive/v3"

	"github.com/syshlted/drivel/provider"
)

// What Upload's after-the-fact check (checkBefore, displacedBy, GetVersion)
// assumes of real Drive, and no fake can settle: that a rewrite nobody else
// touched reports nothing displaced however its history is dated — Drive lists
// revisions by modifiedTime, which uploads set — that an edit landing during
// an upload is found whether it is dated before ours or after, and that it can
// be read back without marking it kept forever, which Drive will not undo.
// First run on 2026-10-21, when the version step and the listing order it
// logs are what overturned the version-counter shortcut. Off unless
// DRIVEL_LIVE_RIG is set; it writes, so it refuses "root", and it deletes
// everything it made.
func TestLiveUpload(t *testing.T) {
	if os.Getenv("DRIVEL_LIVE_RIG") == "" {
		t.Skip("set DRIVEL_LIVE_RIG=1 with -creds/-token/-root env to run")
	}
	root := os.Getenv("DRIVEL_LIVE_ROOT")
	if root == "" || root == "root" {
		t.Fatal("DRIVEL_LIVE_ROOT must name a dedicated folder ID, never \"root\"")
	}
	lctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cfg := Config{
		Credentials: os.Getenv("DRIVEL_CREDS"),
		Token:       os.Getenv("DRIVEL_TOKEN"),
		RootID:      root,
		// Every rewrite here is small, and every one is meant to be checked.
		OverwriteCheck: OverwriteAlways,
	}
	d, err := Open(lctx, cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	peer, err := Open(lctx, cfg)
	if err != nil {
		t.Fatalf("open peer: %v", err)
	}
	t.Cleanup(func() { _ = peer.Close() })

	const name = "drivel-live-upload-probe.bin"
	body := func(tag string, n int) []byte {
		b := bytes.Repeat([]byte{'.'}, n)
		copy(b, tag)
		return b
	}
	digest := func(b []byte) string { s := md5.Sum(b); return hex.EncodeToString(s[:]) }
	version := func(rf provider.RemoteFile) int64 {
		v, err := strconv.ParseInt(rf.Version, 10, 64)
		if err != nil {
			t.Fatalf("version %q of %s is not a number: %v", rf.Version, rf.Path, err)
		}
		return v
	}

	if _, err := d.Put(lctx, name, bytes.NewReader(body("v0", 4<<10))); err != nil {
		t.Fatalf("put: %v", err)
	}
	id, ok := resolve(t, d, name)
	if !ok {
		t.Fatalf("just-written %s does not resolve", name)
	}
	t.Cleanup(func() {
		// A delete, not a trash: it takes every revision with it, kept ones too.
		ctx, c := context.WithTimeout(context.Background(), 30*time.Second)
		defer c()
		if err := d.svc.Files.Delete(id).Context(ctx).Do(); err != nil {
			t.Logf("cleanup: %s (%s) left behind: %v", name, id, err)
		}
	})

	// 1. Rewrites nobody else touched, dated out of order on purpose: a checked
	// rewrite must report nothing whatever the order the history is listed in.
	// The version step is logged because it is the evidence the counter cannot
	// tell anyone whether somebody else wrote.
	rewrites := []struct {
		what     string
		size     int
		modified bool
	}{
		{"4 KiB with a modified time", 4 << 10, true},
		{"4 KiB with a modified time, again", 4 << 10, true},
		{"4 KiB without a modified time", 4 << 10, false},
		{"40 MiB (three chunks) with a modified time", 40 << 20, true},
		{"4 KiB with a modified time, after the large one", 4 << 10, true},
	}
	for i, rw := range rewrites {
		st, ok, err := d.Stat(lctx, name)
		if err != nil || !ok {
			t.Fatalf("stat before rewrite %d: ok=%v err=%v", i, ok, err)
		}
		opts := provider.UploadOptions{Replaces: &st}
		if rw.modified {
			opts.Modified = time.Now().Add(-time.Duration(i+1) * time.Hour).Truncate(time.Millisecond)
		}
		up, err := d.Upload(lctx, name, bytes.NewReader(body("rewrite "+strconv.Itoa(i), rw.size)), opts)
		if err != nil {
			t.Fatalf("rewrite %d (%s): %v", i, rw.what, err)
		}
		t.Logf("rewrite %d, %s: version %s -> %s (step %d)", i, rw.what, st.Version, up.File.Version, version(up.File)-version(st))
		if len(up.Displaced) != 0 {
			t.Errorf("rewrite %d reported displacing %+v; nothing else wrote the file", i, up.Displaced)
		}
		if rw.modified && !up.File.Modified.Equal(opts.Modified) {
			t.Errorf("rewrite %d: modified time %v, sent %v", i, up.File.Modified, opts.Modified)
		}
	}
	// Anything Drive does to a file after the update has answered — indexing,
	// thumbnails — would move the version again with no content change.
	before, _, _ := d.Stat(lctx, name)
	time.Sleep(10 * time.Second)
	after, _, _ := d.Stat(lctx, name)
	t.Logf("10s after the last rewrite: version %s -> %s", before.Version, after.Version)

	// 2. Edits landing during our upload are found, one dated by the server's
	// clock and one carrying a writer's old local time, which Drive lists ahead
	// of what we expected to replace. Our content stalls partway (gated) until
	// the peer's write is done, so it lands after the check's first listing and
	// before our update commits. An edit landing before that listing — between
	// the engine's Stat and the upload's start, about a round trip — is the
	// window the check leaves open, and is not what this asks.
	for _, tc := range []struct {
		what  string
		dated time.Time
	}{
		{"an edit dated now", time.Time{}},
		{"an edit dated last year", time.Now().AddDate(-1, 0, 0).Truncate(time.Millisecond)},
	} {
		stale, _, err := d.Stat(lctx, name)
		if err != nil {
			t.Fatal(err)
		}
		theirs := body("the peer's edit: "+tc.what, 6000)
		ours := newGated(body("ours, over "+tc.what, 5000), 1000)
		type result struct {
			up  provider.Uploaded
			err error
		}
		done := make(chan result, 1)
		go func() {
			up, err := d.Upload(lctx, name, ours, provider.UploadOptions{Replaces: &stale, Modified: time.Now()})
			done <- result{up, err}
		}()
		select {
		case <-ours.started:
		case r := <-done:
			t.Fatalf("our upload over %s ended before it read its content: %v", tc.what, r.err)
		case <-lctx.Done():
			t.Fatal(lctx.Err())
		}
		if _, err := peer.Upload(lctx, name, bytes.NewReader(theirs), provider.UploadOptions{Modified: tc.dated}); err != nil {
			t.Fatalf("peer upload (%s): %v", tc.what, err)
		}
		close(ours.gate)
		r := <-done
		up, err := r.up, r.err
		if err != nil {
			t.Fatalf("upload over %s: %v", tc.what, err)
		}
		if len(up.Displaced) != 1 {
			t.Fatalf("an upload over %s reported %d displaced: %+v", tc.what, len(up.Displaced), up.Displaced)
		}
		got := up.Displaced[0]
		t.Logf("%s: displaced %s, %d bytes, dated %v", tc.what, got.Version, got.Size, got.Modified)
		if got.Hash != digest(theirs) || got.Size != int64(len(theirs)) {
			t.Errorf("%s: displaced revision is %s/%d bytes; the peer wrote %s/%d", tc.what, got.Hash, got.Size, digest(theirs), len(theirs))
		}

		// 3. Read back as the engine does: no mark, and nothing left kept.
		rc, err := d.GetVersion(lctx, name, got.Version)
		if err != nil {
			t.Fatalf("%s: get version: %v", tc.what, err)
		}
		b, err := io.ReadAll(rc)
		if err != nil {
			t.Fatalf("%s: reading the displaced revision: %v", tc.what, err)
		}
		_ = rc.Close()
		if !bytes.Equal(b, theirs) {
			t.Errorf("%s: displaced revision holds %d bytes %.20q..., want the peer's %d", tc.what, len(b), b, len(theirs))
		}
		revID, _ := strings.CutPrefix(got.Version, revisionVersion)
		rev, err := d.svc.Revisions.Get(id, revID).Fields("id,keepForever").Context(lctx).Do()
		if err != nil {
			t.Fatalf("%s: revision after reading it: %v", tc.what, err)
		}
		if rev.KeepForever {
			t.Errorf("%s: revision %s is kept forever after GetVersion", tc.what, revID)
		}
	}

	// 4. Why GetVersion marks nothing: Drive does not let a mark be taken back.
	// Logged rather than asserted — Drive relenting would cost drivel nothing.
	if revs, err := d.revisions(lctx, id); err == nil && len(revs) > 1 {
		probe := revs[0].Id
		kept := func(on bool) error {
			_, err := d.svc.Revisions.Update(id, probe, &drive.Revision{KeepForever: on, ForceSendFields: []string{"KeepForever"}}).
				Fields("id").Context(lctx).Do()
			return err
		}
		if err := kept(true); err != nil {
			t.Logf("keep-forever probe: marking %s: %v", probe, err)
		} else if err := kept(false); err != nil {
			t.Logf("keep-forever probe: a mark cannot be taken back: %v", err)
		} else {
			t.Logf("keep-forever probe: a mark CAN now be taken back; GetVersion's note on why it marks nothing is out of date")
		}
	}

	// The history as Drive lists it, keep-forever included (the probe above
	// leaves one kept; deleting the file takes it).
	list, err := d.svc.Revisions.List(id).Fields("revisions(id,md5Checksum,size,modifiedTime,keepForever)").Context(lctx).Do()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range list.Revisions {
		t.Logf("revision %s  %s  %9d bytes  md5 %.8s  kept=%v", r.Id, r.ModifiedTime, r.Size, r.Md5Checksum, r.KeepForever)
	}
}

// gated is content that stops after its first n bytes until gate is closed,
// saying so on started: an upload held in flight while something else happens.
type gated struct {
	head, tail    []byte
	started, gate chan struct{}
	once          sync.Once
}

func newGated(b []byte, n int) *gated {
	return &gated{head: b[:n], tail: b[n:], started: make(chan struct{}), gate: make(chan struct{})}
}

func (g *gated) Read(p []byte) (int, error) {
	if len(g.head) > 0 {
		n := copy(p, g.head)
		g.head = g.head[n:]
		return n, nil
	}
	g.once.Do(func() { close(g.started); <-g.gate })
	if len(g.tail) == 0 {
		return 0, io.EOF
	}
	n := copy(p, g.tail)
	g.tail = g.tail[n:]
	return n, nil
}
