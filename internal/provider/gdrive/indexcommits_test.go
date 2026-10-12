// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package gdrive

import (
	"fmt"
	"strings"
	"testing"
)

// What the index actually costs per operation, counted in bbolt commits.
//
// Every path this provider resolves ends in rememberLocked, which stores the
// mapping whether or not the index already held it. That is most of the index's
// disk cost and all of it is avoidable: a commit fdatasyncs twice and rewrites
// every page it touched (~36 KB on a 10k-entry DB) even when the value stored is
// byte-identical to the one already there, so the two resolutions a single push
// performs — Stat for M6's gates, then Put — used to cost two full commits for
// one mapping, and an index *hit* cost a commit to re-store what it had just
// read.
//
// These assertions are on the count, not the content: the content was always
// correct, which is exactly why the waste was invisible.

// A push resolves the path twice (Stat, then Put) and must commit once.
func TestStatThenPutCommitsTheMappingOnce(t *testing.T) {
	d, _ := newFakeDrive(t, folder("id-dir", "dir", fakeRootID))
	idx := withIndex(t, d)
	bindIndex(t, d)

	if _, err := d.Put(ctx, "dir/f.txt", strings.NewReader("hello")); err != nil {
		t.Fatalf("put: %v", err)
	}
	afterCreate := idx.Commits()

	// The steady state: Stat for the M6 gates, then Put the new content. Neither
	// changes the path→ID mapping, so neither may commit.
	if _, ok, err := d.Stat(ctx, "dir/f.txt"); err != nil || !ok {
		t.Fatalf("stat: ok=%v err=%v", ok, err)
	}
	if _, err := d.Put(ctx, "dir/f.txt", strings.NewReader("goodbye")); err != nil {
		t.Fatalf("re-put: %v", err)
	}

	if got := idx.Commits() - afterCreate; got != 0 {
		t.Errorf("stat-then-replace committed %d time(s) for an unchanged mapping; want 0", got)
	}
}

// An index hit verifies the stored entry and then re-stores it. Re-storing what
// was just read is the one write that can never be useful.
func TestIndexHitDoesNotRewriteWhatItRead(t *testing.T) {
	d, _ := newFakeDrive(t,
		folder("id-dir", "dir", fakeRootID),
		file("id-f", "f.txt", "id-dir"),
	)
	idx := withIndex(t, d)
	bindIndex(t, d)

	// Seed the index the way a previous session would have left it, then drop the
	// in-memory half so resolution has to come off disk.
	if err := idx.Set("dir", "id-dir"); err != nil {
		t.Fatalf("seed dir: %v", err)
	}
	if err := idx.Set("dir/f.txt", "id-f"); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	d.mu.Lock()
	d.idByPath = map[string]string{"": "root"}
	d.pathByID = map[string]string{"root": ""}
	d.kids = map[string]map[string]struct{}{}
	d.mu.Unlock()

	before := idx.Commits()
	id, ok := resolve(t, d, "dir/f.txt")
	if !ok || id != "id-f" {
		t.Fatalf("resolve off a warm index = %q, %v; want id-f, true", id, ok)
	}
	if got := idx.Commits() - before; got != 0 {
		t.Errorf("an index hit committed %d time(s) re-storing what it read; want 0", got)
	}
}

// A path that does not exist remotely costs exactly one index commit when it is
// created, and the guard must not take that one away — a mapping for a new path
// genuinely is new. This is the budget MC-11's create-only burst pays, and the
// reason the guard does nothing for that case; see DESIGN.md §12.2.
func TestCreatingANewPathCostsOneCommit(t *testing.T) {
	d, _ := newFakeDrive(t, folder("id-dir", "dir", fakeRootID))
	idx := withIndex(t, d)
	bindIndex(t, d)

	// Resolve the parent once up front, so the per-file figure below is the file's
	// own cost and not the one-off cost of learning its directory.
	if _, ok, err := d.Stat(ctx, "dir/warm.txt"); err != nil || ok {
		t.Fatalf("priming stat: ok=%v err=%v", ok, err)
	}

	for i := 0; i < 3; i++ {
		p := fmt.Sprintf("dir/f%d.txt", i)
		before := idx.Commits()
		if _, ok, err := d.Stat(ctx, p); err != nil {
			t.Fatalf("stat %s: %v", p, err)
		} else if ok {
			t.Fatalf("%s should not exist yet", p)
		}
		if _, err := d.Put(ctx, p, strings.NewReader("hello")); err != nil {
			t.Fatalf("put %s: %v", p, err)
		}
		if got := idx.Commits() - before; got != 1 {
			t.Errorf("creating %s cost %d index commit(s); want exactly 1", p, got)
		}
	}
}

// The guard must not stop the index learning something new, which is the failure
// mode that would make it silently useless.
func TestColdResolveStillRecordsWhatItLearned(t *testing.T) {
	d, _ := newFakeDrive(t,
		folder("id-dir", "dir", fakeRootID),
		file("id-f", "f.txt", "id-dir"),
	)
	idx := withIndex(t, d)
	bindIndex(t, d)

	before := idx.Commits()
	if id, ok := resolve(t, d, "dir/f.txt"); !ok || id != "id-f" {
		t.Fatalf("resolve = %q, %v; want id-f, true", id, ok)
	}
	if got := idx.Commits() - before; got == 0 {
		t.Fatal("a cold resolve stored nothing; the index would never warm up")
	}
	if got, ok, err := idx.Lookup("dir/f.txt"); err != nil || !ok || got != "id-f" {
		t.Errorf("index lookup = %q, %v, %v; want id-f, true, nil", got, ok, err)
	}
}
