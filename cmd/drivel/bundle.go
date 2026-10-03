// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

// Bundled backends (M23): this binary carries its providers' code and launches
// one by re-executing itself as `drivel plugin-serve <kind>`.
//
// Which kinds are carried is the only part that varies by build, and it lives in
// bundle_on.go and bundle_off.go — the `nobundle` build tag chooses between
// them. Everything here is the same either way, so the two tagged files stay
// small enough to read at a glance.
//
// It is a supply-chain change and not an isolation one. A backend still runs in
// its own process, with the environment plugin/env.go builds for it, as the same
// user with that user's whole filesystem — nothing about what it may do once it
// is running is different. What changes is what has to be trusted to get there:
// no search path to write a file onto, no shadowed kind, and no window between
// checking a file's mode and executing it, because on Linux the image being
// re-executed is /proc/self/exe, which is the running inode rather than a name
// for it.
package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/syshlted/drivel/plugin"
)

// bundledKinds lists what this build carries, sorted, for registration and for
// the error a kind nobody bundled produces.
func bundledKinds() []string {
	out := make([]string, 0, len(bundledBackends))
	for kind := range bundledBackends {
		out = append(out, kind)
	}
	sort.Strings(out)
	return out
}

// runPluginServe is `drivel plugin-serve KIND`: this program being the backend
// it was asked for. It does not return — plugin.Serve speaks go-plugin's
// protocol on stdout and stderr until the host closes the connection.
//
// The kind arrives as an argument from the host, which is M9 rule 4 with the
// filename replaced: it is still not anything the backend says about itself, and
// the failure that rule was written against — two files claiming one kind,
// resolved by directory order — cannot arise here at all.
//
// Run by hand it is inert: go-plugin refuses to serve unless the handshake
// cookie is in the environment, so it prints its "not meant to be executed
// directly" notice and exits non-zero. The subcommand is still kept out of
// usage(), `drivel help` and the completions, deliberately; see
// plugin.ServeCommand.
func runPluginServe(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: drivel %s KIND", plugin.ServeCommand)
	}
	kind := args[0]
	factory, ok := bundledBackends[kind]
	if !ok {
		if len(bundledBackends) == 0 {
			return fmt.Errorf("this drivel was built with no bundled backends, so it cannot serve %q itself; install %s%s",
				kind, plugin.BinaryPrefix, kind)
		}
		return fmt.Errorf("no bundled %q backend in this drivel (bundled: %s)",
			kind, strings.Join(bundledKinds(), ", "))
	}
	plugin.Serve(factory)
	return nil
}
