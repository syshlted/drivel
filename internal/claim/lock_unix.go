// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

//go:build unix

package claim

import (
	"os"

	"golang.org/x/sys/unix"
)

var errWouldBlock error = unix.EWOULDBLOCK

// lockExclusive takes a non-blocking exclusive flock on f. flock rather than
// fcntl locks because an fcntl lock is per process and is dropped when ANY
// descriptor on the file closes — so Enclosing's probe would release the
// mount's own claim.
func lockExclusive(f *os.File) error {
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err != unix.EINTR {
			return err
		}
	}
}

// heldExclusively reports whether another open file description holds an
// exclusive flock on f — which a shared probe cannot take while it stands —
// releasing the probe at once if it was granted.
func heldExclusively(f *os.File) bool {
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_SH|unix.LOCK_NB)
		switch err {
		case unix.EINTR:
			continue
		case nil:
			_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
			return false
		default:
			return err == unix.EWOULDBLOCK
		}
	}
}
