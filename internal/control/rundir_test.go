// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

//go:build unix

package control

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// prepareRunDir is what root does for an fstab mount before it gives up root:
// make the account a private directory under a base only root can write. Run
// here with this process's own uid standing in for root's, which is the only
// difference from the real call.
func TestPrepareRunDir(t *testing.T) {
	me, gid := os.Getuid(), os.Getgid()

	t.Run("creates a private directory for the account", func(t *testing.T) {
		base := filepath.Join(t.TempDir(), "drivel")
		dir, err := prepareRunDir(base, me, me, gid)
		if err != nil {
			t.Fatal(err)
		}
		if dir != filepath.Join(base, strconv.Itoa(me)) {
			t.Errorf("dir = %s", dir)
		}
		fi, err := os.Stat(dir)
		if err != nil || fi.Mode().Perm() != 0o700 {
			t.Fatalf("dir: %v, %v; want 0700", fi, err)
		}
		// Twice is fine: a second fstab line for the same account shares it.
		if _, err := prepareRunDir(base, me, me, gid); err != nil {
			t.Errorf("second call: %v", err)
		}
		// And Listen, as the account, accepts it.
		ln, err := Listen(filepath.Join(dir, "work.sock"))
		if err != nil {
			t.Fatalf("Listen in the prepared directory: %v", err)
		}
		_ = ln.Close()
	})

	t.Run("refuses a base others can write", func(t *testing.T) {
		base := filepath.Join(t.TempDir(), "drivel")
		if err := os.Mkdir(base, 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(base, 0o777); err != nil { // past the umask
			t.Fatal(err)
		}
		if _, err := prepareRunDir(base, me, me, gid); err == nil || !strings.Contains(err.Error(), "nobody but its owner") {
			t.Errorf("err = %v; want a refusal of the writable base", err)
		}
	})

	t.Run("refuses a base owned by someone else", func(t *testing.T) {
		base := filepath.Join(t.TempDir(), "drivel")
		if _, err := prepareRunDir(base, me+1, me, gid); err == nil {
			t.Error("a base not owned by the required owner was accepted")
		}
	})

	t.Run("refuses a symlinked base", func(t *testing.T) {
		tmp := t.TempDir()
		target := filepath.Join(tmp, "real")
		if err := os.Mkdir(target, 0o755); err != nil {
			t.Fatal(err)
		}
		base := filepath.Join(tmp, "drivel")
		if err := os.Symlink(target, base); err != nil {
			t.Fatal(err)
		}
		if _, err := prepareRunDir(base, me, me, gid); err == nil {
			t.Error("a symlinked base was accepted")
		}
	})

	t.Run("checks an existing account directory rather than fixing it", func(t *testing.T) {
		base := filepath.Join(t.TempDir(), "drivel")
		if err := os.MkdirAll(filepath.Join(base, strconv.Itoa(me)), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := prepareRunDir(base, me, me, gid); err == nil || !strings.Contains(err.Error(), "0700") {
			t.Errorf("err = %v; want the 0755 directory refused", err)
		}
		fi, _ := os.Stat(filepath.Join(base, strconv.Itoa(me)))
		if fi.Mode().Perm() != 0o755 {
			t.Errorf("the existing directory was chmod'ed to %04o", fi.Mode().Perm())
		}
	})
}
