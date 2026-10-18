// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package sftp

import (
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/syshlted/drivel/provider"
)

// Create refuses an occupied path, leaves it untouched and leaves no temporary,
// both with the hard-link extension and with the check-and-rename fallback.
func TestCreateRefusesAnOccupiedPath(t *testing.T) {
	for _, link := range []bool{true, false} {
		name := "check-and-rename"
		if link {
			name = "hardlink"
		}
		t.Run(name, func(t *testing.T) {
			ts := startServer(t)
			s := ts.open(t)
			c, err := s.session(t.Context())
			if err != nil {
				t.Fatalf("session: %v", err)
			}
			if link && !c.hardlink {
				t.Fatal("the test server does not advertise hardlink@openssh.com")
			}
			c.hardlink = link

			if err := os.WriteFile(ts.path("taken.txt"), []byte("theirs"), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err = s.Create(t.Context(), "taken.txt", strings.NewReader("ours"))
			if !errors.Is(err, provider.ErrExist) {
				t.Fatalf("Create over an occupied path = %v; want ErrExist", err)
			}
			if b, _ := os.ReadFile(ts.path("taken.txt")); string(b) != "theirs" {
				t.Errorf("a refused Create changed the file: %q", b)
			}

			rf, err := s.Create(t.Context(), "sub/new.txt", strings.NewReader("ours"))
			if err != nil {
				t.Fatalf("Create of a free path: %v", err)
			}
			if b, _ := os.ReadFile(ts.path("sub/new.txt")); string(b) != "ours" || rf.Size != 4 {
				t.Errorf("created %q (size %d)", b, rf.Size)
			}
			for _, dir := range []string{ts.root, ts.path("sub")} {
				entries, _ := os.ReadDir(dir)
				for _, e := range entries {
					if isTemp(e.Name()) {
						t.Errorf("upload temporary %s left behind", e.Name())
					}
				}
			}
		})
	}
}

// The race the hard link exists for: the destination appears after the vacancy
// check, while the content is uploading. A rename would replace it; the link
// must refuse.
func TestCreateLosesARaceWithoutReplacing(t *testing.T) {
	ts := startServer(t)
	s := ts.open(t)
	r := &raceReader{body: "ours", before: func() {
		_ = os.WriteFile(ts.path("race.txt"), []byte("theirs"), 0o644)
	}}
	_, err := s.Create(t.Context(), "race.txt", r)
	if !errors.Is(err, provider.ErrExist) {
		t.Fatalf("Create that lost the race = %v; want ErrExist", err)
	}
	if b, _ := os.ReadFile(ts.path("race.txt")); string(b) != "theirs" {
		t.Errorf("the winner's file was replaced: %q", b)
	}
}

// raceReader runs before on its first read, which is after Create checked the
// path and before it links.
type raceReader struct {
	body   string
	before func()
	done   bool
}

func (r *raceReader) Read(p []byte) (int, error) {
	if !r.done {
		r.done = true
		r.before()
	}
	if r.body == "" {
		return 0, io.EOF
	}
	n := copy(p, r.body)
	r.body = r.body[n:]
	return n, nil
}
