// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

//go:build !linux

package main

import (
	"fmt"
	"runtime"
)

// runMountHelper refuses off Linux.
//
// Not a gap so much as a different mechanism: the mount(8) helper protocol, the
// /sbin/mount.<type> lookup, the privilege drop and the re-exec handshake are all
// how *Linux* brings a filesystem up unattended. macOS and FreeBSD each have
// their own answer (a launchd agent, an rc.d script), and neither is reached by
// pretending to be a mount helper. The option parser above is deliberately
// portable anyway, so its tests run everywhere.
func runMountHelper([]string) error {
	return fmt.Errorf("mounting drivel from fstab is Linux-only (this is %s); run `drivel mount` instead", runtime.GOOS)
}
