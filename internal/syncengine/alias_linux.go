// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

//go:build linux

package syncengine

import (
	"os"
	"path/filepath"
	"strconv"

	"golang.org/x/sys/unix"
)

// storedName asks the kernel what name the entry p resolves to is stored
// under: an O_PATH descriptor's /proc link names the dentry the lookup found,
// and a case-insensitive lookup finds the entry by its own name — measured on a
// casefold tmpfs, where foo.txt and FOO.TXT both read back as Foo.txt. Four
// syscalls whatever the directory's size. If the link cannot be read, the
// listing answers instead.
func storedName(dir, name, p string, fi os.FileInfo) (string, error) {
	fd, err := unix.Open(p, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", err
	}
	defer unix.Close(fd)
	target, err := os.Readlink("/proc/self/fd/" + strconv.Itoa(fd))
	if err != nil {
		return storedNameByListing(dir, name, fi)
	}
	return filepath.Base(target), nil
}
