// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package syncengine

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/syshlted/drivel/internal/state"
	"github.com/syshlted/drivel/provider"
)

// sparseBody is size bytes of zeros with data at the given offsets.
func sparseBody(size int, data map[int]string) []byte {
	b := make([]byte, size)
	for off, s := range data {
		copy(b[off:], s)
	}
	return b
}

func fetchBody(t *testing.T, body []byte, dst string) {
	t.Helper()
	store := newFakeStore()
	store.content["f"] = body
	n, err := fetchTo(context.Background(), store, "f", dst, time.Time{}, func(error) {})
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len(body)) {
		t.Fatalf("fetchTo reported %d bytes; want %d", n, len(body))
	}
}

// Whatever was left unwritten, the file reads back byte for byte, at its full
// length: a hole in the middle, at the start, at the end, a short last block,
// and nothing at all.
func TestASparseDownloadReadsBackExactly(t *testing.T) {
	cases := map[string][]byte{
		"empty":            {},
		"one byte":         []byte("x"),
		"all zeros":        make([]byte, 3*sparseBlock+17),
		"data, hole, data": sparseBody(64*sparseBlock, map[int]string{0: "head", 40 * sparseBlock: "middle"}),
		"leading hole":     sparseBody(8*sparseBlock, map[int]string{7*sparseBlock + 5: "tail"}),
		"trailing hole":    sparseBody(70*sparseBlock+123, map[int]string{9: "start"}),
		"dense":            bytes.Repeat([]byte("abcdefg"), 50000),
		"zero inside a block": sparseBody(2*sparseBlock, map[int]string{
			1: "x", sparseBlock - 1: "y", sparseBlock + 1: "z",
		}),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			dst := filepath.Join(t.TempDir(), "out")
			fetchBody(t, body, dst)
			got, err := os.ReadFile(dst)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, body) {
				t.Fatalf("read back %d bytes differing from the %d sent", len(got), len(body))
			}
		})
	}
}

// A file a download replaces keeps its permission bits — a script edited
// elsewhere stays executable — but never setuid, setgid or sticky, which content
// from a remote must not carry. A new file is 0600.
func TestADownloadKeepsTheModeOfTheFileItReplaces(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name string
		had  os.FileMode // 0: no file there before
		want os.FileMode
	}{
		{"new", 0, 0o600},
		{"executable", 0o755, 0o755},
		{"read-only", 0o444, 0o444},
		{"setuid stripped", 0o755 | os.ModeSetuid | os.ModeSetgid | os.ModeSticky, 0o755},
	}
	for _, c := range cases {
		dst := filepath.Join(dir, c.name)
		if c.had != 0 {
			if err := os.WriteFile(dst, []byte("old"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(dst, c.had); err != nil {
				t.Fatal(err)
			}
		}
		fetchBody(t, []byte("new"), dst)
		fi, err := os.Lstat(dst)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode() & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky); got != c.want {
			t.Errorf("%s: mode = %v; want %v", c.name, got, c.want)
		}
	}
}

// A remote edit that wins a conflict replaces the local file by moving the loser
// away and downloading anew, so it is a new file to fetchTo; it still takes the
// mode the path had.
func TestAConflictWonRemotelyKeepsTheMode(t *testing.T) {
	dir := t.TempDir()
	fs := newFakeStore()
	st := newState(t)
	fs.content["run.sh"] = []byte("#!/bin/sh\necho remote\n")
	writeFile(t, dir, "run.sh", "#!/bin/sh\necho local\n")
	if err := os.Chmod(filepath.Join(dir, "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	setMtime(t, dir, "run.sh", time.Now().Add(-time.Hour))
	_ = st.SetEcho("run.sh", state.Echo{Hash: md5hex("baseline"), Version: "1", At: time.Now()})
	d := NewDownloader(nil, fs, dir, st, DefaultCadence)
	rf := &provider.RemoteFile{Path: "run.sh", Hash: md5hex("#!/bin/sh\necho remote\n"), Version: "2", Modified: time.Now()}

	if err := d.apply(context.Background(), provider.RemoteChange{Path: "run.sh", File: rf}); err != nil {
		t.Fatal(err)
	}
	if got, _ := readBacking(t, dir, "run.sh"); got != "#!/bin/sh\necho remote\n" {
		t.Fatalf("run.sh = %q; the remote edit should have won", got)
	}
	fi, err := os.Lstat(filepath.Join(dir, "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Fatalf("mode after the remote edit won = %v; want 0755", fi.Mode().Perm())
	}
}
