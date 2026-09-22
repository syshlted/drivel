// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

// Command drivel-provider-sftp is drivel's SFTP backend, running as an
// out-of-process plugin (M9).
//
// It is not run directly; see the note in drivel-provider-gdrive. The kind it
// provides — `sftp` — is this executable's filename.
//
// Copyright (C) SystemHalted and Jeremy Melanson. Licensed under the Mozilla
// Public License, version 2.0.
package main

import (
	"github.com/zishmusic/drivel/internal/provider/sftp"
	"github.com/zishmusic/drivel/plugin"
)

func main() {
	plugin.Serve(sftp.Factory)
}
