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
func checkPrivateDir(dir string) error {
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
	if int(st.Uid) != os.Getuid() {
		return fmt.Errorf("control socket directory %s is owned by uid %d, not by this user", dir, st.Uid)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("control socket directory %s has mode %04o; it must be 0700 so only this user can reach the socket", dir, fi.Mode().Perm())
	}
	return nil
}
