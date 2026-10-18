// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package gdrive

import (
	"errors"
	"io"
	"testing"

	"github.com/syshlted/drivel/provider"
)

// Copy is one files.copy, with the copy's missing parent folder made first and
// no content uploaded, and it refuses both an absent source and a taken name.
func TestCopyIsServerSide(t *testing.T) {
	d, fake := newFakeDrive(t)
	fake.seedFile("a.txt", fakeRootID, []byte("the loser"))
	fake.seedFile("taken.txt", fakeRootID, []byte("old"))

	rf, err := d.Copy(ctx, "a.txt", ".drivel-conflicts/a (conflict).txt")
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	if rf.Path != ".drivel-conflicts/a (conflict).txt" || rf.Hash == "" {
		t.Errorf("copy reported %+v; want the new path with its checksum", rf)
	}
	fake.mu.Lock()
	copies, updates := fake.copies, fake.updates
	fake.mu.Unlock()
	if copies != 1 || updates != 0 {
		t.Errorf("copies = %d, updates = %d; want one files.copy and no upload", copies, updates)
	}
	rc, err := fake.client(t).Get(ctx, ".drivel-conflicts/a (conflict).txt")
	if err != nil {
		t.Fatalf("get of the copy from a fresh client: %v", err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(got) != "the loser" {
		t.Errorf("copy holds %q; want the source's bytes", got)
	}

	if _, err := d.Copy(ctx, "a.txt", "taken.txt"); !errors.Is(err, provider.ErrExist) {
		t.Errorf("copy over a taken name = %v; want ErrExist", err)
	}
	if _, err := d.Copy(ctx, "never.txt", "x.txt"); !errors.Is(err, provider.ErrNotExist) {
		t.Errorf("copy of an absent path = %v; want ErrNotExist", err)
	}
}
