// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

//go:build !unix

package syncengine

import "testing"

func mkfifo(t *testing.T, _ string) {
	t.Skip("no named pipes on this platform")
}
