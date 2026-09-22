// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

//go:build darwin

package vfs

import (
	"testing"

	"golang.org/x/sys/unix"
)

// mountFlags asks the kernel what a mounted filesystem actually enforces. See the
// linux file for the contract; darwin reports both flags, spelled MNT_* rather
// than ST_* and living in a narrower Flags word.
func mountFlags(t *testing.T, path string) (nosuid, nodev, nodevKnown bool) {
	t.Helper()
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		t.Fatalf("statfs %s: %v", path, err)
	}
	return st.Flags&unix.MNT_NOSUID != 0, st.Flags&unix.MNT_NODEV != 0, true
}
