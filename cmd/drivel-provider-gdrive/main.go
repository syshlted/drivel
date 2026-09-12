// Command drivel-provider-gdrive is drivel's Google Drive backend, running as an
// out-of-process plugin (M9).
//
// It is not run directly. drivel launches it when a mount names the `gdrive`
// provider, hands it that mount's configuration over a private socket, and
// forwards everything it logs to that mount's log. Started from a shell it
// prints a handshake line and exits, which is go-plugin's protocol and not a
// useful thing to look at.
//
// The kind it provides — `gdrive` — is this executable's filename, not anything
// it says about itself. See plugin.BinaryPrefix.
//
// Copyright (C) SystemHalted and Jeremy Melanson. Licensed under the GNU Affero
// General Public License version 3.
package main

import (
	"github.com/zishmusic/drivel/internal/provider/gdrive"
	"github.com/zishmusic/drivel/plugin"
)

func main() {
	plugin.Serve(gdrive.Factory)
}
