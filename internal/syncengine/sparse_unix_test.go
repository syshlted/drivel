// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

//go:build unix

package syncengine

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// allocated is how many bytes of p's content the filesystem stores.
func allocated(t *testing.T, p string) int64 {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Sys().(*syscall.Stat_t).Blocks * 512
}

// The point of it: an 8 MiB file holding 8 KiB of data occupies about 8 KiB,
// not 8 MiB. Skipped where the filesystem under TMPDIR has no holes, which a
// truncated empty file shows.
func TestASparseDownloadStaysSparse(t *testing.T) {
	dir := t.TempDir()
	probe := filepath.Join(dir, "probe")
	if err := os.WriteFile(probe, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(probe, 1<<20); err != nil {
		t.Fatal(err)
	}
	if allocated(t, probe) >= 1<<20 {
		t.Skip("the filesystem under TMPDIR does not keep holes")
	}
	body := sparseBody(8<<20, map[int]string{0: "head", 4 << 20: "middle"})
	dst := filepath.Join(dir, "image")
	fetchBody(t, body, dst)
	if got := allocated(t, dst); got > 1<<20 {
		t.Fatalf("an 8 MiB file holding two blocks of data occupies %d bytes; the zeros were written", got)
	}
}
