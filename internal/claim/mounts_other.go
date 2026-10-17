// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

//go:build !linux

package claim

// MountsUnder has no portable source of mount types; elsewhere a foreign drivel
// mount inside a backing tree is caught only if its own backing store carries a
// marker the walk can see.
func MountsUnder(string) map[string]bool { return nil }
