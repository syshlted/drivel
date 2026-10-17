// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

//go:build !linux

package control

// maxSocketPath is sizeof(sockaddr_un.sun_path) on the BSDs and macOS, which
// includes the NUL. Elsewhere it is a conservative guess.
const maxSocketPath = 104
