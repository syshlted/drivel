// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

//go:build linux || darwin

package vfs

import "golang.org/x/sys/unix"

// The client half of the xattr tests: these call the mountpoint the way an
// ordinary program would, so they are spelled in the platform's own interface
// rather than in drivel's. Linux and macOS take one string in which "user." is a
// namespace prefix; FreeBSD takes the namespace as a separate argument. The tests
// therefore pass a *bare* name and let this shim place it, which is what lets one
// test body assert the same guard on all three kernels — see
// xattrclient_freebsd_test.go for the other half, and hydrate/xattr_unix.go for
// the same split below the seam.

// userNS spells name as an attribute of the unprivileged namespace.
func userNS(name string) string { return "user." + name }

func clientSetxattr(path, name string, v []byte) error {
	return unix.Setxattr(path, userNS(name), v, 0)
}

func clientGetxattr(path, name string) ([]byte, error) {
	buf := make([]byte, 256)
	n, err := unix.Getxattr(path, userNS(name), buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

func clientRemovexattr(path, name string) error {
	return unix.Removexattr(path, userNS(name))
}

func clientListxattr(path string) (int, error) {
	return unix.Listxattr(path, make([]byte, 256))
}
