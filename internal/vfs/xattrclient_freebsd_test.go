// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

//go:build freebsd

package vfs

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

// The FreeBSD client half of the xattr tests. See xattrclient_unix_test.go for
// why the tests pass a bare name: here the namespace is an argument rather than a
// prefix, so "user.test" would be an attribute literally called "user.test" in the
// user namespace — a different attribute, and a test that quietly asserted the
// wrong one.
//
// The uintptr wrappers carry //go:uintptrescapes for the reason
// hydrate/xattr_freebsd.go spells out: x/sys types the buffer as a uintptr, which
// neither keeps the array alive nor survives a stack copy, and the calls allocate
// before reaching the kernel. A test that got this wrong would fail rarely and
// look like a kernel bug.

//go:uintptrescapes
func extattrGetTest(path, attr string, data uintptr, n int) (int, error) {
	return unix.ExtattrGetFile(path, unix.EXTATTR_NAMESPACE_USER, attr, data, n)
}

//go:uintptrescapes
func extattrSetTest(path, attr string, data uintptr, n int) (int, error) {
	return unix.ExtattrSetFile(path, unix.EXTATTR_NAMESPACE_USER, attr, data, n)
}

//go:uintptrescapes
func extattrListTest(path string, data uintptr, n int) (int, error) {
	return unix.ExtattrListFile(path, unix.EXTATTR_NAMESPACE_USER, data, n)
}

func clientSetxattr(path, name string, v []byte) error {
	if len(v) == 0 {
		_, err := extattrSetTest(path, name, 0, 0)
		return err
	}
	_, err := extattrSetTest(path, name, uintptr(unsafe.Pointer(&v[0])), len(v))
	return err
}

func clientGetxattr(path, name string) ([]byte, error) {
	buf := make([]byte, 256)
	n, err := extattrGetTest(path, name, uintptr(unsafe.Pointer(&buf[0])), len(buf))
	if err != nil {
		return nil, err
	}
	if n > len(buf) {
		n = len(buf)
	}
	return buf[:n], nil
}

func clientRemovexattr(path, name string) error {
	return unix.ExtattrDeleteFile(path, unix.EXTATTR_NAMESPACE_USER, name)
}

func clientListxattr(path string) (int, error) {
	buf := make([]byte, 256)
	return extattrListTest(path, uintptr(unsafe.Pointer(&buf[0])), len(buf))
}
