// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package gdrive

import (
	"io"
	"log"
	"strings"
	"sync"
	"testing"

	"github.com/syshlted/drivel/provider"
)

// MC-30 from docs/dev/multiclient-test-plan.md, at the layer that owns it.
//
// Drive's data model permits what a POSIX directory cannot represent: two files
// with the same name in one folder. Three clients creating the same path at the
// same instant is an ordinary thing for a fleet to do — each one asks "does this
// path exist?", each one is truthfully told no, and each one creates. The result
// is three same-name siblings, of which the mount can show exactly one.
//
// These cases live against fakeDrive rather than in the in-process fleet
// (internal/app/multiclient_test.go) on purpose. Siblings are a property of
// Drive's data model; a path-keyed fake store cannot have them, so emulating
// them above the seam would test the emulation. Below the seam the fake is
// ID-keyed like the real thing, and the situation arises by itself.
//
// What is asserted here is behaviour, not a wish. Nothing in this file claims
// the outcome is *good* — the plan calls the mitigation a design decision, and
// these tests are what that decision would have to be made against.

// logBuf captures one client's output.
//
// Asserting on log text is usually a way to pin an implementation detail. Here
// the log is the behaviour: resolution succeeds either way, and the warning is
// the only place a user is ever told that two other files just became
// invisible at a path they can still read.
type logBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuf) saw(sub string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Contains(l.b.String(), sub)
}

// watch routes d's output into a buffer the test can interrogate.
func watch(d *Drive) *logBuf {
	buf := &logBuf{}
	d.lg = log.New(buf, "", 0)
	return buf
}

// read is the bytes a client can reach at p, or a failure.
func read(t *testing.T, d *Drive, p string) string {
	t.Helper()
	rc, err := d.Get(ctx, p)
	if err != nil {
		t.Fatalf("get %q: %v", p, err)
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("reading %q: %v", p, err)
	}
	return string(b)
}

// Three clients, one path, one instant: every lookup misses, so every client
// creates, and the folder ends up holding three files with one name.
//
// The barrier is what makes this deterministic. Without it the three Puts
// serialise, the second client finds the first client's file and updates it,
// and the fleet converges — which is the outcome everyone assumes and is not
// the one under test.
func TestSimultaneousCreatesOfOnePathMakeSameNameSiblings(t *testing.T) {
	const peers = 3
	_, fake := newFakeDrive(t)
	fake.nameGate = newNameBarrier(peers)

	clients := make([]*Drive, peers)
	for i := range clients {
		clients[i] = fake.client(t)
	}

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for i, d := range clients {
		wg.Add(1)
		go func(i int, d *Drive) {
			defer wg.Done()
			// Distinct content per peer, so "whose bytes survived" is answerable.
			if _, err := d.Put(ctx, "same.txt", strings.NewReader(peerBody(i))); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}(i, d)
	}
	wg.Wait()

	if fake.nameGate.stalled() {
		t.Fatal("the name queries never overlapped; the race MC-30 describes did not form, so nothing below this line means anything")
	}
	if len(errs) > 0 {
		t.Fatalf("Put: %v", errs)
	}

	// Every peer created. Nobody updated anybody: an update would mean one client
	// saw another's file, which is the interleaving the barrier exists to exclude.
	if _, _, creates, _ := fake.counts(); creates != peers {
		t.Errorf("creates = %d; want %d, one per peer", creates, peers)
	}
	if n := fake.updateCount(); n != 0 {
		t.Errorf("updates = %d; want 0 — a peer resolved another peer's file", n)
	}

	sibs := fake.namedChildren(fakeRootID, "same.txt")
	if len(sibs) != peers {
		t.Fatalf("the folder holds %d files named same.txt; want %d — Drive does not merge them", len(sibs), peers)
	}
	seen := map[string]bool{}
	for _, f := range sibs {
		if seen[f.Id] {
			t.Fatalf("duplicate ID %s among the siblings", f.Id)
		}
		seen[f.Id] = true
	}

	// The part that matters: a fresh client sees one file at that path, and two
	// of the three bodies are now unreachable through any path at all.
	fresh := fake.client(t)
	buf := watch(fresh)
	got := read(t, fresh, "same.txt")

	bodies := map[string]bool{}
	for i := range clients {
		bodies[peerBody(i)] = true
	}
	if !bodies[got] {
		t.Fatalf("same.txt reads %q, which no peer wrote", got)
	}
	if !buf.saw("remote files share the name") {
		t.Error("no client was told that other files are invisible at this path; the loss is then silent")
	}
	for i := range clients {
		if body := peerBody(i); body != got {
			if strings.Contains(got, body) {
				t.Errorf("peer %d's content leaked into the visible file", i)
			}
		}
	}
}

// peerBody is peer i's content: distinguishable, and the same length for every
// peer so that a size comparison can never stand in for a content comparison.
func peerBody(i int) string {
	return strings.Repeat(string(rune('a'+i)), 32)
}

// Once the siblings exist, deleting "the file" removes one of them — and the
// next-newest surfaces at that path with different content.
//
// This is the sharp end of MC-30 and it is worth being explicit about: to every
// client in the fleet, an ordinary `rm` of a path is followed by that path
// reappearing with someone else's bytes in it. Nothing is corrupt and no rule
// has been broken; a path simply names three objects and removal uncovers the
// next one. A fleet-wide mitigation would have to make this stop.
//
// Removing to the trash does not change the shape of this, and adds one turn to
// it: restoring the trashed sibling from the web UI puts two objects back at one
// path, and which of them is visible depends on which was modified last.
func TestRemovingTheVisibleSiblingUncoversTheNextOne(t *testing.T) {
	_, fake := newFakeDrive(t)
	oldest := fake.seedFile("same.txt", fakeRootID, []byte("oldest"))
	middle := fake.seedFile("same.txt", fakeRootID, []byte("middle"))
	newest := fake.seedFile("same.txt", fakeRootID, []byte("newest"))

	d := fake.client(t)
	watch(d)
	if got := read(t, d, "same.txt"); got != "newest" {
		t.Fatalf("same.txt reads %q; want the most recently modified sibling", got)
	}

	if err := d.Remove(ctx, "same.txt"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if !fake.trashed(newest.Id) {
		t.Error("the visible sibling survived the removal")
	}
	for _, id := range []string{oldest.Id, middle.Id} {
		if !fake.has(id) || fake.trashed(id) {
			t.Errorf("removing one path removed sibling %s as well", id)
		}
	}

	// The path is not gone. It now holds the middle sibling's bytes, and every
	// other client in the fleet will download them as a change.
	got := read(t, d, "same.txt")
	if got != "middle" {
		t.Fatalf("after removing same.txt it reads %q; want the next-newest sibling, %q", got, "middle")
	}
}

// A sweep sees every sibling, and reports them all at one path.
//
// Enumerate is where the fleet's other half meets this: reconcile receives the
// same path three times with three different hashes, so the local file ends up
// holding whichever the sweep applied last — not necessarily the one the mount
// reads through Get, which picks by modifiedTime. The two disagree by
// construction, and no amount of care in the sweep can fix it while a path
// names three objects.
func TestEnumerateReportsEverySiblingAtTheSamePath(t *testing.T) {
	_, fake := newFakeDrive(t)
	fake.seedFile("same.txt", fakeRootID, []byte("first"))
	fake.seedFile("same.txt", fakeRootID, []byte("second"))
	fake.seedFile("other.txt", fakeRootID, []byte("unrelated"))

	d := fake.client(t)
	var all []provider.RemoteFile
	cursor := ""
	for {
		page, next, err := d.Enumerate(ctx, cursor)
		if err != nil {
			t.Fatalf("enumerate: %v", err)
		}
		all = append(all, page...)
		if next == "" {
			break
		}
		cursor = next
	}

	byPath := map[string][]provider.RemoteFile{}
	for _, f := range all {
		byPath[f.Path] = append(byPath[f.Path], f)
	}
	if n := len(byPath["same.txt"]); n != 2 {
		t.Fatalf("the sweep reported same.txt %d time(s); want 2 — a flat listing has no notion of a path, so both siblings arrive", n)
	}
	if n := len(byPath["other.txt"]); n != 1 {
		t.Errorf("other.txt reported %d time(s); want 1", n)
	}
	// Different hashes at one path is the whole problem in one line: the seam
	// above cannot tell these apart from one file that changed twice.
	if a, b := byPath["same.txt"][0], byPath["same.txt"][1]; a.Hash == b.Hash {
		t.Errorf("both siblings reported hash %s; the fake is folding them together", a.Hash)
	}
}
