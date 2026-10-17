// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

//go:build !unix

package claim

import (
	"errors"
	"os"
)

var errWouldBlock = errors.New("would block")

// lockExclusive always succeeds where there is no flock: every claim then reads
// as stale, which degrades to warnings and the walk's pruning, never a refusal.
func lockExclusive(*os.File) error { return nil }
