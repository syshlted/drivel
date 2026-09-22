// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

//go:build freebsd

package vfs

// compulsoryOptions returns the mount options drivel always requests. See the
// linux file for the reasoning.
//
// FreeBSD is nosuid only, and the missing half is not a gap: MNT_NODEV was
// removed from the kernel, because only devfs may hold device nodes — a device
// node on any other filesystem is inert whatever the mount says. So the property
// item 1 of M15 wants holds here by construction rather than by request, and
// "nodev" would be at best a no-op string handed to mount_fusefs, which parses
// its options against a fixed table and exits non-zero on one it does not know.
// A mount that fails to come up is a worse outcome than the flag it was asking
// for, and this is not hypothetical: adding "nodev" here on FreeBSD 15.1 gives
//
//	mount_fusefs: -o dev: option not supported
//
// and no mount at all. (It reports the option with the "no" prefix stripped, so
// grepping for the string this file does not contain will not find it.)
func compulsoryOptions() []string { return []string{"nosuid"} }
