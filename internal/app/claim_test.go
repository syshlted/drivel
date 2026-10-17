// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package app

import (
	"bytes"
	"log"
	"path/filepath"
	"strings"
	"testing"
)

// MC-47's shapes, refused at open. Two Opens in one test process stand in for
// two drivel processes: Validate is never asked (it would refuse these itself,
// in-process), so what refuses them here is the claim, which is the only guard
// that works across processes. flock conflicts between open file descriptions,
// so it behaves the same whether the other holder is this process or another.

func TestOpenRefusesABackingStoreInsideALiveOne(t *testing.T) {
	a := baseSpec(t)
	ma, err := Open(t.Context(), a, nil)
	if err != nil {
		t.Fatalf("Open A: %v", err)
	}
	defer ma.Close() //nolint:errcheck // test cleanup

	for name, mut := range map[string]func(*MountSpec){
		"backing store": func(s *MountSpec) { s.DataDir = filepath.Join(a.DataDir, "bdata") },
		"mountpoint":    func(s *MountSpec) { s.Mountpoint = filepath.Join(a.DataDir, "bmnt") },
		"state DB":      func(s *MountSpec) { s.StateDB = filepath.Join(a.DataDir, "b-state.db") },
		"same backing":  func(s *MountSpec) { s.DataDir = a.DataDir },
	} {
		t.Run(name, func(t *testing.T) {
			b := baseSpec(t)
			b.Name = "b"
			mut(&b)
			mb, err := Open(t.Context(), b, nil)
			if err == nil {
				mb.Close() //nolint:errcheck // the failure is already reported
				t.Fatalf("Open B with its %s inside A's live backing store succeeded", name)
			}
			if !strings.Contains(err.Error(), `"test"`) {
				t.Errorf("error %q does not name the mount it collides with", err)
			}
		})
	}
}

// The same layout after A has stopped is an old directory, not a live mount: a
// note, not a refusal.
func TestOpenOnlyNotesAStaleBackingStore(t *testing.T) {
	a := baseSpec(t)
	ma, err := Open(t.Context(), a, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := ma.Close(); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	b := baseSpec(t)
	b.Name = "b"
	b.DataDir = filepath.Join(a.DataDir, "bdata")
	b.Logger = log.New(&logs, "", 0)
	mb, err := Open(t.Context(), b, nil)
	if err != nil {
		t.Fatalf("Open B under a stopped mount's backing store: %v", err)
	}
	defer mb.Close() //nolint:errcheck // test cleanup
	if !strings.Contains(logs.String(), "not running now") {
		t.Errorf("no note about the stale backing store:\n%s", logs.String())
	}
}
