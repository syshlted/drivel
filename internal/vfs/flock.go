// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package vfs

import "sync"

// flocks is the mount's record of the exclusive flocks it has forwarded to
// backing files, and of the content it announced while one was held.
//
// Forwarding is what lets the engine see a lock taken through the mount
// (claim.HeldByAnother), and the engine declines to upload a file locked that
// way, because it may be caught mid-transaction. Declining is only safe if
// something announces the file again once the lock is gone. For a holder that
// writes — a database — its own close does. For one that does not, nothing
// did: `flock f -c 'echo x >> f'` holds the lock on a descriptor that never
// writes, the append's close is declined, and the holder's close announced
// nothing, so the append was never sent. And a database created under its own
// lock announces itself at its first close, while the lock still stands. So
// a content event emitted while an exclusive lock is held marks the path owed,
// and when the last exclusive holder lets go — an unlock, a downgrade to
// shared, or the handle's release — the path is announced once more.
//
// Keyed by the path each handle was opened at, like the events it emits.
type flocks struct {
	mu   sync.Mutex
	held map[string]*flockState
}

type flockState struct {
	holders int  // handles holding an exclusive flock on the path
	owed    bool // a content event was announced while one was held
}

func newFlocks() *flocks { return &flocks{held: map[string]*flockState{}} }

// took records an exclusive lock newly held by one handle.
func (l *flocks) took(path string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.held[path]
	if s == nil {
		s = &flockState{}
		l.held[path] = s
	}
	s.holders++
}

// dropped records one handle letting its exclusive lock go, and reports whether
// the path is owed an announcement now that none holds one.
func (l *flocks) dropped(path string) (owed bool) {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.held[path]
	if s == nil {
		return false
	}
	if s.holders--; s.holders > 0 {
		return false
	}
	delete(l.held, path)
	return s.owed
}

// announced records that a content event for path is going out, marking it owed
// if an exclusive lock on it is held right now.
func (l *flocks) announced(path string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if s := l.held[path]; s != nil {
		s.owed = true
	}
}
