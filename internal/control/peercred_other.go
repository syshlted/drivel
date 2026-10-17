// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

//go:build !linux && !darwin && !freebsd

package control

import "net"

// peerOf has no implementation here; the socket directory's mode is then the
// only check, which is what it is on every platform anyway — peer credentials
// back it up, they do not replace it.
func peerOf(net.Conn) (peer, bool) { return peer{}, false }
