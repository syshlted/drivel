// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package app

import (
	"context"
	"time"

	"github.com/syshlted/drivel/internal/syncengine"
)

// What the control API (M14, internal/control) may ask of a mount. Every method
// here reads local state or asks the run loop through its control channel;
// none calls a provider, and none is read back by anything that decides what to
// sync.

// Name is the mount's name as the control API addresses it.
func (m *Mount) Name() string { return m.spec.controlName() }

// Mountpoint is where the filesystem is mounted.
func (m *Mount) Mountpoint() string { return m.spec.Mountpoint }

// Provider is the provider kind, or "" for a log-only mount.
func (m *Mount) Provider() string { return m.spec.Provider }

// Lazy reports whether the mount hydrates on read.
func (m *Mount) Lazy() bool { return m.spec.Lazy }

// Stats reads the mount's counters.
func (m *Mount) Stats() syncengine.Stats { return m.mon.Snapshot() }

// Monitor is the mount's observation surface, for subscribing to transitions.
func (m *Mount) Monitor() *syncengine.Monitor { return m.mon }

// statusReader answers path queries from local state alone.
func (m *Mount) statusReader() syncengine.StatusReader {
	return syncengine.StatusReader{
		Monitor: m.mon,
		State:   m.state,
		DataDir: m.backing.Path,
		Holes:   holesOf(m.hyd),

		Conflicts: m.conflicts,
	}
}

// Status reports one path's sync status.
func (m *Mount) Status(rel string) (syncengine.PathStatus, error) {
	return m.statusReader().Path(rel)
}

// Children reports the status of a directory's direct children, paged.
func (m *Mount) Children(rel, after string, limit int) ([]syncengine.PathStatus, bool, error) {
	return m.statusReader().Children(rel, after, limit)
}

// Pause holds this mount's uploads, pull loop and sweep for d.
func (m *Mount) Pause(ctx context.Context, d time.Duration) (time.Time, error) {
	return m.engine.Pause(ctx, d)
}

// Resume ends a pause.
func (m *Mount) Resume(ctx context.Context) error { return m.engine.Resume(ctx) }

// Mounts returns the open mounts, in the order they were given.
func (a *App) Mounts() []*Mount { return a.mounts }
