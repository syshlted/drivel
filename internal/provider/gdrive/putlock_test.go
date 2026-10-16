// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package gdrive

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// What Put may hold while it transfers, and what it must not.
//
// Put used to take d.mu for its whole body, upload included. d.mu is what every
// other operation needs for a moment — Get and GetRange to resolve a path,
// Changes to resolve each entry in a page, Enumerate to register a page's
// folders — so a client that was pushing could not pull, and could not push any
// other path either. MC-36 and MC-50 measured the first half: during the 23.5
// minutes its own 1000 uploads took, each client fetched about 20 of its peers'
// 2000 files, then took the remaining ~1980 in the 15 minutes after its last
// upload landed. The second half is quieter and worse for being invisible:
// -upload-workers is documented as buying latency-hiding, and it bought nothing,
// because four workers all queued on one mutex.
//
// It is now a per-path lock (see lockPath) held across the transfer, with d.mu
// taken only for the index work at either end. The two tests below are the two
// halves of that: uploads of different paths no longer exclude each other or
// anything else, and uploads of ONE path still do.

// gatedReader blocks on its first Read until released, so a test can hold an
// upload open at exactly the point where the transfer has begun and the index
// work at the near end is finished.
type gatedReader struct {
	started   chan struct{}
	gate      chan struct{}
	once      sync.Once
	openOnce  sync.Once
	remaining string
}

func newGatedReader(content string) *gatedReader {
	return &gatedReader{
		started:   make(chan struct{}),
		gate:      make(chan struct{}),
		remaining: content,
	}
}

func (g *gatedReader) Read(p []byte) (int, error) {
	g.openOnce.Do(func() {
		close(g.started)
		<-g.gate
	})
	if g.remaining == "" {
		return 0, io.EOF
	}
	n := copy(p, g.remaining)
	g.remaining = g.remaining[n:]
	return n, nil
}

// release lets the held upload finish. Safe to call twice, because the tests
// below arm it as a cleanup as well as calling it in the body: a failure between
// the two would otherwise leave a goroutine blocked on an unclosed channel.
func (g *gatedReader) release() { g.once.Do(func() { close(g.gate) }) }

// An upload in flight must not stop this mount doing anything else.
func TestAnUploadDoesNotBlockTheRestOfTheMount(t *testing.T) {
	d, f := newFakeDrive(t)
	f.seedFile("other.txt", fakeRootID, []byte("other"))
	ctx := context.Background()

	held := newGatedReader("a slow upload")
	t.Cleanup(held.release)
	slow := make(chan error, 1)
	go func() {
		_, err := d.Put(ctx, "slow.txt", held)
		slow <- err
	}()
	// Past the resolve, inside the transfer: the only place the old code held
	// d.mu for as long as the network took.
	<-held.started

	// Inbound work. One lock acquisition to resolve a path, and it used to wait
	// for the upload above.
	stat := make(chan error, 1)
	go func() {
		_, _, err := d.Stat(ctx, "other.txt")
		stat <- err
	}()
	// Outbound work on another path, which is what -upload-workers is for.
	other := make(chan error, 1)
	go func() {
		_, err := d.Put(ctx, "quick.txt", strings.NewReader("quick"))
		other <- err
	}()

	for _, c := range []struct {
		what string
		ch   chan error
	}{
		{"Stat (the inbound path's resolve)", stat},
		{"a Put of another path", other},
	} {
		select {
		case err := <-c.ch:
			if err != nil {
				t.Fatalf("%s: %v", c.what, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s blocked behind an upload that is still transferring", c.what)
		}
	}

	held.release()
	if err := <-slow; err != nil {
		t.Fatalf("the held upload failed: %v", err)
	}
}

// And the exclusion the whole-body lock did buy, which per-path keeps: two
// pushes of ONE path must not both create.
//
// Deterministic in the direction that matters. The second upload's reader is
// gated too, so "did it start transferring while the first was in flight?" is a
// channel and not a race: with no per-path lock it resolves (the first create
// has not landed, so the name query finds nothing), creates, and the path ends
// up with two objects — MC-30 from inside a single process, where it is
// inexcusable rather than merely bad.
func TestConcurrentPutsOfOnePathDoNotBothCreate(t *testing.T) {
	d, f := newFakeDrive(t)
	ctx := context.Background()

	first := newGatedReader("first")
	t.Cleanup(first.release)
	firstDone := make(chan error, 1)
	go func() {
		_, err := d.Put(ctx, "same.txt", first)
		firstDone <- err
	}()
	<-first.started

	second := newGatedReader("second")
	t.Cleanup(second.release)
	secondDone := make(chan error, 1)
	go func() {
		_, err := d.Put(ctx, "same.txt", second)
		secondDone <- err
	}()

	select {
	case <-second.started:
		t.Fatal("a second push of one path began transferring while the first was still in flight: " +
			"both will have resolved to nothing and both will create, leaving same-name siblings")
	case <-time.After(250 * time.Millisecond):
		// Nothing happened, which is the assertion.
	}

	first.release()
	if err := <-firstDone; err != nil {
		t.Fatalf("first push: %v", err)
	}
	<-second.started
	second.release()
	if err := <-secondDone; err != nil {
		t.Fatalf("second push: %v", err)
	}

	if kids := f.namedChildren(fakeRootID, "same.txt"); len(kids) != 1 {
		t.Fatalf("the path holds %d objects; want 1", len(kids))
	}
	if _, _, creates, _ := f.counts(); creates != 1 {
		t.Errorf("creates = %d; want 1 (the second push must have found the first)", creates)
	}
	if n := f.updateCount(); n != 1 {
		t.Errorf("updates = %d; want 1", n)
	}
}
