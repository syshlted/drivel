// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package plugin

import (
	"context"
	"fmt"
	"os"
	"runtime"

	"github.com/syshlted/drivel/provider"
)

// ServeCommand is the subcommand a host passes to itself to serve a backend it
// carries in its own binary (M23): `drivel plugin-serve <kind>`.
//
// The kind is an argument rather than part of the subcommand name, so dispatch
// is one entry rather than a list to keep in step with the set of bundled
// backends. It is still the *host's* word for which backend to serve and never
// anything the backend says about itself, so M9's rule holds with the filename
// it used to be read from replaced by an argument the host passed.
//
// It is deliberately absent from usage(), `drivel help` and the shell
// completions. Nobody has a reason to run it, and running it by hand is already
// inert — go-plugin refuses to serve without MagicCookieKey in the environment —
// so keeping it out is about the size of the visible surface, not a control.
const ServeCommand = "plugin-serve"

// bundledFactory returns the Factory that launches a kind this binary carries,
// by re-executing the host.
//
// Everything after the exec is M9 unchanged: the same handshake, the same unix
// socket, the same generated protocol, the same capability negotiation, the same
// relaunch-on-crash. That is the requirement and not a happy accident — a
// behaviour reachable on one launch path and not the other is a difference
// nothing tests, which is also why the installed path must stay exercised (see
// plugin/testdata/drivel-provider-fake).
func bundledFactory(kind string) provider.Factory {
	return func(ctx context.Context, p provider.Params) (provider.Store, error) {
		spec, err := bundledSpec(kind)
		if err != nil {
			// Retryable, like every other launch failure in this package: the
			// engine's retry loop is what waits one out, and a host that cannot name
			// its own image is a condition that can change under it rather than a
			// configuration error worth failing the mount on.
			return nil, transportError("plugin %s: %w", kind, err)
		}
		return open(ctx, spec, p)
	}
}

// bundledSpec describes the launch: this program, told to serve one kind.
func bundledSpec(kind string) (launchSpec, error) {
	exe, err := hostImage()
	if err != nil {
		return launchSpec{}, err
	}
	return launchSpec{
		kind:    kind,
		path:    exe,
		args:    []string{ServeCommand, kind},
		bundled: true,
	}, nil
}

// hostImage resolves the executable to re-run.
//
// On Linux /proc/self/exe is preferred over the path os.Executable reports,
// because it *is* the running inode rather than a name for it: there is nothing
// to swap between the decision to launch and the exec, which closes by
// construction the check-then-exec window safeToRun can only ever narrow for an
// installed plugin — and is why a bundled backend is not put through that check
// at all. It also survives the binary being replaced underneath a long-lived
// mount, where the resolved path would name the new file or a deleted one.
//
// Everywhere else — and on a Linux without /proc mounted — the path of the
// running program is the best answer available, and it is enough: anyone able to
// replace it can replace drivel itself, which is a trust the user has already
// extended by running it.
func hostImage() (string, error) {
	const procSelfExe = "/proc/self/exe"
	if runtime.GOOS == "linux" {
		if _, err := os.Stat(procSelfExe); err == nil {
			return procSelfExe, nil
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolving this program's own path: %w", err)
	}
	return exe, nil
}
