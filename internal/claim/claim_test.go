// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package claim

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// A held claim refuses a second holder and names the first; released, it can
// be taken again. The second Acquire comes from the same process on purpose:
// flock conflicts between open file descriptions, so two mounts in one process
// are refused exactly as two processes are.
func TestAClaimIsExclusiveUntilReleased(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no flock")
	}
	root := t.TempDir()
	a, err := Acquire(root, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(root, "beta"); !errors.Is(err, ErrHeld) {
		t.Fatalf("second Acquire: %v; want ErrHeld", err)
	} else if want := `"alpha"`; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not name the holder %s", err, want)
	}
	if err := a.Release(); err != nil {
		t.Fatal(err)
	}
	b, err := Acquire(root, "beta")
	if err != nil {
		t.Fatalf("Acquire after Release: %v", err)
	}
	defer b.Release() //nolint:errcheck // test cleanup
	if !Marked(root) {
		t.Error("Marked is false for a claimed root")
	}
}

// Enclosing finds the nearest marker above a path and tells a live claim from a
// stale one — the difference between a refusal and a warning.
func TestEnclosingTellsLiveFromStale(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no flock")
	}
	outer := t.TempDir()
	inner := filepath.Join(outer, "sub", "deeper")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, ok := Enclosing(inner); ok {
		t.Fatal("Enclosing found a marker in a tree that has none")
	}

	c, err := Acquire(outer, "outer-mount")
	if err != nil {
		t.Fatal(err)
	}
	got, ok := Enclosing(inner)
	if !ok || got.Dir != outer || got.Owner != "outer-mount" || !got.Live {
		t.Fatalf("Enclosing = %+v, %v; want a live claim by outer-mount at %s", got, ok, outer)
	}
	// The probe must not have taken the holder's lock away.
	if _, err := Acquire(outer, "x"); !errors.Is(err, ErrHeld) {
		t.Fatalf("after Enclosing probed it, the claim is no longer held: %v", err)
	}

	if err := c.Release(); err != nil {
		t.Fatal(err)
	}
	got, ok = Enclosing(inner)
	if !ok || got.Live {
		t.Errorf("after Release: %+v, %v; want the marker found and stale", got, ok)
	}
}

func TestParseMountinfo(t *testing.T) {
	cases := []struct {
		line, mp, fstype string
	}{
		{`36 35 98:0 /mnt1 /mnt/parent rw,noatime master:1 - ext3 /dev/root rw,errors=continue`, "/mnt/parent", "ext3"},
		{`700 29 0:63 / /home/j/My\040Drive rw,nosuid,nodev - fuse.drivel work rw,user_id=1000`, "/home/j/My Drive", "fuse.drivel"},
		{`700 29 0:63 / /x rw shared:5 master:2 - fuse.drivel drivel rw`, "/x", "fuse.drivel"},
	}
	for _, c := range cases {
		mp, fstype, ok := parseMountinfo(c.line)
		if !ok || mp != c.mp || fstype != c.fstype {
			t.Errorf("parseMountinfo(%q) = %q, %q, %v; want %q, %q", c.line, mp, fstype, ok, c.mp, c.fstype)
		}
	}
	if _, _, ok := parseMountinfo("garbage"); ok {
		t.Error("parseMountinfo accepted a line with no separator")
	}
	if got := unescapeOctal(`a\134b\011c\0`); got != "a\\b\tc\\0" {
		t.Errorf("unescapeOctal = %q", got)
	}
}
