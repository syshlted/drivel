// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"errors"
	"log"

	"github.com/syshlted/drivel/internal/app"
	"github.com/syshlted/drivel/internal/control"
)

// errControlShutdown is the cause a control-API shutdown cancels the mount
// context with, so the end of the run can say what ended it.
var errControlShutdown = errors.New("shutdown requested over the control socket")

// startControl serves the control API (M14) for every mount in a, until the
// returned function is called. shutdown is how POST /v1/shutdown stops the
// process: by cancelling the context the signals cancel, never by exiting.
func startControl(path string, a *app.App, shutdown func(error)) (func(), error) {
	if path == "" {
		p, err := control.DefaultSocketPath()
		if err != nil {
			return nil, err
		}
		path = p
	}
	ln, err := control.Listen(path)
	if err != nil {
		return nil, err
	}
	mounts := make([]control.Mount, 0, a.Len())
	for _, m := range a.Mounts() {
		mounts = append(mounts, m)
	}
	srv := control.New(mounts, func(string) { shutdown(errControlShutdown) }, log.Default())
	go func() {
		if err := srv.Serve(ln); err != nil {
			log.Printf("control socket: %v", err)
		}
	}()
	// gosec G706: on the fstab path this comes from the line's options or the
	// mount's name, both root's to write; on the flag path, the caller's own flag.
	log.Printf("control socket at %s", path) //nolint:gosec // G706: see above
	return func() { _ = srv.Close() }, nil
}

// validateControlFlags refuses -control-socket without -control, for the reason
// -pprof-allow-remote is refused without -pprof: a flag accepted and then
// ignored is a setting that looks applied and is not (M8 rule 6).
func validateControlFlags(enabled, socketGiven bool) error {
	if socketGiven && !enabled {
		return errors.New("-control-socket has no effect without -control")
	}
	return nil
}
