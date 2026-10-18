// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package fsevent

import "sync"

// Writing is the set of paths a mount has a NEW file open on: created through
// it and not yet closed. A new file's one event leaves when it is closed, so
// until then nothing has told the sync engine about it — and the sweep's walk,
// which finds local files with no baseline and pushes them, would otherwise
// upload it part-written, then again at close. On a feedless backend the sweep
// is the poll, every few seconds, so that was every slow copy uploaded twice.
//
// It is keyed by the path at Create. A file renamed while still open is
// released under its old name and is then the walk's again; that costs an early
// push, never a missed one.
//
// A nil *Writing is an empty set, so a mount and a sweep can each be built
// without the other.
type Writing struct {
	mu sync.Mutex
	n  map[string]int
}

// Open records one more open handle on a new file at p.
func (w *Writing) Open(p string) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.n == nil {
		w.n = map[string]int{}
	}
	w.n[p]++
}

// Close records that one of those handles was released.
func (w *Writing) Close(p string) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.n[p] <= 1 {
		delete(w.n, p)
		return
	}
	w.n[p]--
}

// Has reports whether a new file at p is still open.
func (w *Writing) Has(p string) bool {
	if w == nil {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.n[p] > 0
}
