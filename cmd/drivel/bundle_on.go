// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

//go:build !nobundle

package main

import (
	"github.com/syshlted/drivel/internal/provider/gdrive"
	"github.com/syshlted/drivel/internal/provider/sftp"
	"github.com/syshlted/drivel/provider"
)

// bundledBackends are the provider kinds this binary carries the code for, and
// the default build carries every backend that ships in the tree. A fresh
// checkout is therefore usable the moment it compiles, and a release is one file
// rather than a binary plus a directory to populate.
//
// The key is the kind a config file names — the same word that used to be the
// suffix of a `drivel-provider-*` filename. It is also what `drivel plugin-serve`
// is handed, so this map is read twice, by the host registering what it can open
// and by the child deciding which factory to serve.
//
// This is the whole of what the `nobundle` tag turns off (see bundle_off.go).
// Everything else about the launch is identical on both paths, so a build with
// no bundled backends is a host that finds every provider on the search path,
// exactly as M9 shipped it — which is the shape a distribution packaging each
// backend separately wants, and the one that keeps a Drive SDK advisory reported
// against drivel-provider-gdrive rather than against drivel.
var bundledBackends = map[string]provider.Factory{
	driveKind: gdrive.Factory,
	sftpKind:  sftp.Factory,
}

// sftpKind is the SFTP backend's name, beside driveKind in mount.go. Neither is
// declared by the provider: a kind is what the host calls a backend.
const sftpKind = "sftp"
