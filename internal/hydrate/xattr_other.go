// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

//go:build !linux && !darwin && !freebsd

package hydrate

import "syscall"

// xattrSupported is false on platforms with no implementation here: the
// placeholder marker degrades to the state-store cache alone. See
// Hydrator.IsPlaceholder for what that costs.
const xattrSupported = false

// xattrName is unreachable while xattrSupported is false, but XattrName is
// exported and appears in log lines, so it keeps the spelling most readers will
// recognise rather than an empty string.
const xattrName = "user.drivel.placeholder"

func getxattr(string, string) ([]byte, error) { return nil, syscall.ENOTSUP }
func setxattr(string, string, []byte) error   { return syscall.ENOTSUP }
func removexattr(string, string) error        { return syscall.ENOTSUP }

func isNoAttr(err error) bool    { return false }
func isNoSupport(err error) bool { return err == syscall.ENOTSUP }

// xattrNative is unreachable for the same reason; it exists so the probe in
// XattrsUsable compiles everywhere.
func xattrNative(string) bool { return false }
