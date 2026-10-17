// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

//go:build unix

package control

import (
	"fmt"
	"os"
	"syscall"
)

// checkPrivateDir refuses a socket directory that someone else could reach or
// replace: a symlink, a directory owned by another user, or one with any group
// or other permission bits. MkdirAll does not fix an existing directory's mode,
// so a directory created earlier by something careless is caught here.
func checkPrivateDir(dir string) error { return checkPrivateDirOf(dir, os.Getuid()) }

// checkPrivateDirOf is checkPrivateDir for a directory that must belong to uid,
// which is not this process's uid while root prepares one for a run-as account.
func checkPrivateDirOf(dir string, uid int) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("control socket directory: %w", err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("control socket directory %s is not a directory (a symlink is refused)", dir)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("control socket directory %s: cannot read its owner", dir)
	}
	if int(st.Uid) != uid {
		return fmt.Errorf("control socket directory %s is owned by uid %d, not by uid %d", dir, st.Uid, uid)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("control socket directory %s has mode %04o; it must be 0700 so only this user can reach the socket", dir, fi.Mode().Perm())
	}
	return nil
}

// checkSharedDir refuses a parent of per-user socket directories that anyone but
// owner could write to, since whoever can write it can replace a user's
// directory with their own.
func checkSharedDir(dir string, owner int) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("control socket directory: %w", err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("control socket directory %s is not a directory (a symlink is refused)", dir)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("control socket directory %s: cannot read its owner", dir)
	}
	if int(st.Uid) != owner {
		return fmt.Errorf("control socket directory %s is owned by uid %d, not by uid %d", dir, st.Uid, owner)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("control socket directory %s has mode %04o; nobody but its owner may write to it", dir, fi.Mode().Perm())
	}
	return nil
}
