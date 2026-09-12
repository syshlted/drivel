// Command drivel-provider-sftp is drivel's SFTP backend, running as an
// out-of-process plugin (M9).
//
// It is not run directly; see the note in drivel-provider-gdrive. The kind it
// provides — `sftp` — is this executable's filename.
//
// Copyright (C) SystemHalted and Jeremy Melanson. Licensed under the GNU Affero
// General Public License version 3.
package main

import (
	"github.com/zishmusic/drivel/internal/provider/sftp"
	"github.com/zishmusic/drivel/plugin"
)

func main() {
	plugin.Serve(sftp.Factory)
}
