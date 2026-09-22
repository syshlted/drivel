// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

// Package fsevent defines the backend-neutral change events emitted by a mount
// frontend (go-fuse today; cgofuse or an NFS-loopback backend later) and consumed
// by the sync engine. Keeping it separate from any concrete backend lets the
// sync core stay portable and lets multiple mount backends share one event type.
package fsevent

import "github.com/zishmusic/drivel/ranges"

// Op identifies the kind of filesystem mutation an Event describes.
type Op string

const (
	OpCreate  Op = "create"
	OpWrite   Op = "write"
	OpMkdir   Op = "mkdir"
	OpRmdir   Op = "rmdir"
	OpUnlink  Op = "unlink"
	OpRename  Op = "rename"
	OpSetattr Op = "setattr"
)

// Event describes a single mutation observed at the mount, addressed by paths
// relative to the filesystem root (no leading slash). NewPath is set only for
// OpRename.
type Event struct {
	Op      Op
	Path    string
	NewPath string

	// Dirty bounds the byte extents an OpWrite/OpCreate touched, so the uploader
	// can ship only those where the provider supports range writes (M6).
	//
	// nil means "unknown" and is the fail-safe default: the whole file gets
	// pushed. Every code path that cannot account for every changed byte —
	// a truncate or any other size change, an event synthesised without a file
	// handle behind it, a backend that does not track extents — leaves it nil and
	// is correct by construction. Only a handle that saw all of a file's writes
	// may set it. Getting this backwards would upload a stale block over a good
	// one, so "unsure" must always mean nil.
	Dirty *ranges.Set
}
