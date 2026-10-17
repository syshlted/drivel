// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

//go:build darwin || freebsd

package control

import (
	"net"

	"golang.org/x/sys/unix"
)

// peerOf reads the connecting process's credentials with LOCAL_PEERCRED. The
// BSD structure carries no pid, so a shutdown is logged with the uid alone.
func peerOf(c net.Conn) (peer, bool) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return peer{}, false
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return peer{}, false
	}
	var cred *unix.Xucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	}); err != nil || credErr != nil {
		return peer{}, false
	}
	return peer{uid: int(cred.Uid), pid: -1}, true
}
