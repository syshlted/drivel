// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

//go:build darwin

package hydrate

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// errNoAttr is the errno the xattr calls return for "no such attribute". macOS
// spells it ENOATTR. It also defines ENODATA, for XSI STREAMS, and that is not
// this — checking for the Linux spelling here would classify every missing marker
// as an I/O error.
const errNoAttr = unix.ENOATTR

// xattrNative reports whether probe's filesystem stores extended attributes
// itself, as APFS and HFS+ do, rather than emulating them in an AppleDouble
// sidecar as macOS does on volumes with no native support (exFAT, FAT, and some
// SMB and NFS mounts).
//
// The distinction matters more than it looks, which is why the probe asks a
// second question after a successful write. An emulated attribute reads back
// perfectly, so setxattr succeeding proves nothing — but M5's marker then lives
// in a "._name" file *inside the backing tree*, where drivel syncs it like any
// other file, another client materialises it as literal garbage, and separating
// it from its parent turns a placeholder into an apparently-empty file. That last
// one is M5 invariant 2 exactly: the uploader pushes the placeholder's zeros over
// the remote content. Reporting emulation as "no xattrs" earns the mount the same
// warning a filesystem without them gets, which is the honest answer for a store
// where the marker cannot be trusted to stay attached.
//
// It looks for the sidecar rather than asking getattrlist for the volume's
// capabilities because the claim that matters is not what the volume advertises
// but whether this write produced a file. A stale sidecar left by an earlier run
// costs one spurious warning and is removed here, so the next probe is clean.
func xattrNative(probe string) bool {
	side := appleDouble(probe)
	if _, err := os.Lstat(side); err != nil {
		return true
	}
	os.Remove(side)
	return false
}

// appleDouble is the sidecar path macOS pairs with p when it has to store
// extended attributes or a resource fork out of band.
func appleDouble(p string) string {
	return filepath.Join(filepath.Dir(p), "._"+filepath.Base(p))
}
