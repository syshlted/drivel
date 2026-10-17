// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package control

import (
	"net"

	"golang.org/x/sys/unix"
)

// peerOf reads the connecting process's credentials with SO_PEERCRED.
func peerOf(c net.Conn) (peer, bool) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return peer{}, false
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return peer{}, false
	}
	var cred *unix.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil || credErr != nil {
		return peer{}, false
	}
	return peer{uid: int(cred.Uid), pid: int(cred.Pid)}, true
}
