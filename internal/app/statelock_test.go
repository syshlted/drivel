// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package app

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/syshlted/drivel/internal/state"
	"github.com/syshlted/drivel/provider"
)

// TestOpenOnALockedStateDBSaysWhyAndSaysItOnce is the Tier A half of MC-47 (a):
// two processes aimed at one state DB.
//
// app.Validate refuses that inside one process and the fleet harness exercises
// the guard on every start, so the shape only exists between processes — which is
// why running it needed two real drivels and why what reaches the operator is the
// whole finding. The guard itself was already right: bbolt's flock times out in
// 5s, nothing is written, and state.unreadable deliberately excludes ErrTimeout so
// the live mount's file is never moved aside (TestOpenDoesNotReplaceALockedFile
// pins that half). What was wrong was the sentence. The real run produced
//
//	mount b: open state db: open state db /path/state.db: timeout
//
// which says the operation twice, and names neither the cause nor the fix. "Timeout"
// on its own reads as a network error, and this is the one failure in the program
// that cannot be: the file is local, and the thing holding it is another drivel.
//
// This asserts the message rather than the error value on purpose. There is no
// sentinel to match — bbolt's ErrTimeout is the only signal and it is already
// wrapped — so the message IS the interface here, and the two mistakes worth
// catching (losing the explanation, and re-adding the wrap that duplicated it)
// are both invisible to a test that only checks that Open failed.
func TestOpenOnALockedStateDBSaysWhyAndSaysItOnce(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")

	// The first mount's store, held open for the duration: this is the live mount
	// whose state the second one must not disturb.
	live, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("opening the first state db: %v", err)
	}
	defer live.Close()

	reg := provider.NewRegistry()
	if err := reg.Register("mem", func(context.Context, provider.Params) (provider.Store, error) {
		return newMemStore(), nil
	}); err != nil {
		t.Fatalf("registering the test provider: %v", err)
	}

	// A spec that reaches the state DB and stops there. Open resolves the backing
	// store and builds the provider before it opens the state DB, and mounts only
	// after — so this needs no /dev/fuse, which is what lets it run everywhere.
	spec := MountSpec{
		Name:       "second",
		Mountpoint: filepath.Join(dir, "mnt"),
		DataDir:    filepath.Join(dir, "data"),
		StateDB:    dbPath,
		Provider:   "mem",
	}
	m, err := Open(context.Background(), spec, reg)
	if err == nil {
		_ = m.Close()
		t.Fatal("a second mount opened a state DB another process holds; it must not")
	}
	msg := err.Error()

	// The cause and the fix. Without the first an operator debugs the network;
	// without the second they are told there is a problem and not what to do.
	for _, want := range []string{"another process", dbPath, "own"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the error does not mention %q, so it does not say why or what to do:\n\t%s", want, msg)
		}
	}
	// And it says it once. This is what the removed wrap in Open cost: the inner
	// error already names the operation and the file, and App adds the mount name
	// on top, so a wrap here is the third copy of the same words.
	if n := strings.Count(msg, "state db"); n != 1 {
		t.Errorf("%q appears %d times; the operation should be named once:\n\t%s", "state db", n, msg)
	}

	// The live mount is untouched: still open, still holding its own state.
	if err := live.SetCursor("still-mine"); err != nil {
		t.Errorf("the live store stopped working after the second mount was refused: %v", err)
	}
}
