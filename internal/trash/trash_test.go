// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package trash

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDirFollowsTheEnvironment(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "/x/data")
	if d, err := Dir(); err != nil || d != "/x/data/Trash" {
		t.Errorf("Dir = %q, %v", d, err)
	}
	t.Setenv("XDG_DATA_HOME", "relative")
	if _, err := Dir(); err == nil {
		t.Error("a relative XDG_DATA_HOME was accepted")
	}
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("HOME", "/home/u")
	if d, err := Dir(); err != nil || d != "/home/u/.local/share/Trash" {
		t.Errorf("Dir = %q, %v", d, err)
	}
	t.Setenv("HOME", "")
	if _, err := Dir(); err == nil {
		t.Error("no HOME and no XDG_DATA_HOME produced a trash")
	}
}

// A moved file lands in files/ under its own name, the .trashinfo names where
// it came from (escaped) and when, and a second file of the same name gets a
// different one rather than replacing the first.
func TestMoveRecordsTheOriginAndNeverReplaces(t *testing.T) {
	tr, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	when := time.Date(2026, 10, 18, 9, 30, 5, 0, time.Local)
	var got []string
	for _, body := range []string{"first", "second"} {
		p := filepath.Join(src, "a b.txt")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		dst, err := tr.Move(p, "/mnt/drive/dir/a b.txt", when)
		if err != nil {
			t.Fatalf("Move: %v", err)
		}
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s is still there after Move", p)
		}
		b, _ := os.ReadFile(dst)
		if string(b) != body {
			t.Errorf("%s holds %q; want %q", dst, b, body)
		}
		got = append(got, filepath.Base(dst))
	}
	if got[0] != "a b.txt" || got[1] != "a b.2.txt" {
		t.Errorf("trash names = %v; want [a b.txt a b.2.txt]", got)
	}
	info, err := os.ReadFile(filepath.Join(tr.Root(), "info", "a b.txt.trashinfo"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"[Trash Info]\n", "Path=/mnt/drive/dir/a%20b.txt\n", "DeletionDate=2026-10-18T09:30:05\n"} {
		if !strings.Contains(string(info), want) {
			t.Errorf(".trashinfo lacks %q:\n%s", want, info)
		}
	}
}

// A name already taken in files/ with no .trashinfo is not ours to replace.
func TestReserveStepsOverAnOrphanedFile(t *testing.T) {
	tr, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(tr.Root(), "files", "x.txt")
	if err := os.WriteFile(orphan, []byte("theirs"), 0o644); err != nil {
		t.Fatal(err)
	}
	e, err := tr.Reserve("/m/x.txt", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(e.Path) != "x.2.txt" {
		t.Errorf("reserved %s; want x.2.txt", e.Path)
	}
	if b, _ := os.ReadFile(orphan); string(b) != "theirs" {
		t.Error("the orphaned file was touched")
	}
	if _, err := os.Lstat(filepath.Join(tr.Root(), "info", "x.txt.trashinfo")); err == nil {
		t.Error("a .trashinfo was left for the name that was skipped")
	}
}
