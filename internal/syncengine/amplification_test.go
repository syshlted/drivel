// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package syncengine

import (
	"context"
	"fmt"
	"testing"

	"github.com/syshlted/drivel/internal/fsevent"
)

// Disk cost per synced file, as a commit count.
//
// MC-11 reported "write amplification ≈ 6000×" — 190 KB of file content against
// 1175 MB of process writes — and both halves of that ratio are misleading. The
// numerator was an artefact of the workload writing ~19-byte files where the case
// specifies 4 KiB, so the ratio moves with file size and settles no argument. The
// denominator is the real finding, and it decomposes exactly: a bbolt commit
// fdatasyncs twice and rewrites every page it touched, ~36 KB on a 10k-entry DB
// whatever the key is and flat as the DB grows, so disk cost is
// commits × a constant.
//
// The commit count is what a test can pin. It is deterministic, it does not
// depend on the filesystem underneath, and it is the number that moves when
// somebody adds a store write to a per-file path. §5.3 of the multi-client plan
// is the reason this is a Tier A test at all: the finding is reachable in-process
// at thousands of iterations a second, where MC-11 needed two and a half hours
// against real Drive to produce one instance per file.
//
// These budgets are an upper bound and a ratchet, not a target. Lowering one is
// an improvement; raising one is a decision that needs a reason in the commit
// message.
const (
	// One SetEcho per pushed file — the §4 baseline, which must be committed
	// before the change-feed reports our own upload back to us.
	commitsPerPush = 1
	// One ForgetMany, which drops the echo and the hydration bitmap together.
	commitsPerUnlink = 1
)

func TestPushCommitsPerFile(t *testing.T) {
	const files = 50

	dataDir := t.TempDir()
	st := newState(t)
	e := New(Config{Store: newFakeStore(), DataDir: dataDir, State: st})
	ctx := context.Background()

	base := st.Commits()
	for i := 0; i < files; i++ {
		rel := fmt.Sprintf("d%02d/f%d.txt", i%10, i)
		writeFile(t, dataDir, rel, "x")
		e.handle(ctx, fsevent.Event{Op: fsevent.OpCreate, Path: rel})
	}

	got := st.Commits() - base
	if want := uint64(files * commitsPerPush); got != want {
		t.Errorf("pushing %d files committed %d times, want %d (%.2f per file, budget %d)",
			files, got, want, float64(got)/files, commitsPerPush)
	}
}

func TestUnlinkCommitsPerFile(t *testing.T) {
	const files = 50

	dataDir := t.TempDir()
	st := newState(t)
	e := New(Config{Store: newFakeStore(), DataDir: dataDir, State: st})
	ctx := context.Background()

	for i := 0; i < files; i++ {
		rel := fmt.Sprintf("f%d.txt", i)
		writeFile(t, dataDir, rel, "x")
		e.handle(ctx, fsevent.Event{Op: fsevent.OpCreate, Path: rel})
	}

	base := st.Commits()
	for i := 0; i < files; i++ {
		e.handle(ctx, fsevent.Event{Op: fsevent.OpUnlink, Path: fmt.Sprintf("f%d.txt", i)})
	}

	got := st.Commits() - base
	if want := uint64(files * commitsPerUnlink); got != want {
		t.Errorf("unlinking %d files committed %d times, want %d (budget %d per file)",
			files, got, want, commitsPerUnlink)
	}
}

// A re-push of unchanged content still commits, and that is correct rather than
// an oversight: Echo.At is the time the baseline was taken, which M7b compares
// against the sweep's start to decide whether a local copy predates the sweep. So
// the echo genuinely differs on every push and cannot be deduplicated the way a
// path-index mapping can — pinning that here so nobody "fixes" it by holding the
// timestamp still, which would silently break the sweep's delete guard.
func TestRepushStillRecordsABaseline(t *testing.T) {
	dataDir := t.TempDir()
	st := newState(t)
	e := New(Config{Store: newFakeStore(), DataDir: dataDir, State: st})
	ctx := context.Background()

	writeFile(t, dataDir, "a.txt", "same")
	e.handle(ctx, fsevent.Event{Op: fsevent.OpCreate, Path: "a.txt"})

	first, ok, err := st.GetEcho("a.txt")
	if err != nil || !ok {
		t.Fatalf("no echo after first push (ok=%v, err=%v)", ok, err)
	}

	base := st.Commits()
	e.handle(ctx, fsevent.Event{Op: fsevent.OpWrite, Path: "a.txt"})
	if got := st.Commits() - base; got != commitsPerPush {
		t.Errorf("re-push committed %d times, want %d", got, commitsPerPush)
	}

	second, ok, err := st.GetEcho("a.txt")
	if err != nil || !ok {
		t.Fatalf("no echo after re-push (ok=%v, err=%v)", ok, err)
	}
	if !second.At.After(first.At) && !second.At.Equal(first.At) {
		t.Errorf("baseline went backwards: %v then %v", first.At, second.At)
	}
}
