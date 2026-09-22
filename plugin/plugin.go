// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

// Package plugin loads drivel storage backends that run in their own process
// (DESIGN.md §9, M9), and is also the SDK a backend is written against.
//
// # Why out of process
//
// The seam a backend implements — provider.Store plus the optional interfaces
// beside it — has existed since M2 and was proven against two independently
// configured stores in M8. M9 adds no interface. What it adds is a way to load
// an implementation that this repository did not compile, which Go's own
// `plugin` package cannot honestly do: it is Linux-only, it requires the plugin
// and the host to have been built by the same toolchain from the same
// dependency versions, and nothing can ever be unloaded. An out-of-process
// plugin has none of those constraints, and it buys two properties that an
// in-process one could not have at any price:
//
//   - **Failure isolation.** A backend that panics, deadlocks or leaks takes its
//     own process down. The mount stays up, the engine's retry with backoff sees
//     a retryable error, and the host relaunches the backend underneath it. The
//     filesystem a user is looking at does not go away because a cloud SDK hit a
//     nil map.
//   - **A blast radius that can be talked about.** A backend is a separate
//     executable with its own address space, so "what can this code reach?" has
//     an answer that is not "everything drivel can reach".
//
// What it costs is one process per mount and a local RPC hop on every byte of
// every transfer. The second is the one to be honest about: a file's content now
// crosses a unix socket on the way to and from the backend. Against a network
// backend that is noise — the socket moves data two to three orders of magnitude
// faster than Drive or an SFTP server will — but against a future local backend
// (M11) it is a real cost, and that backend will want measuring rather than
// assuming.
//
// # What this package is not
//
// It is not a sandbox. A plugin runs as the same user as drivel, with that
// user's filesystem access, and drivel launching it is drivel trusting it —
// exactly as much as if the code had been compiled in. What the host does do is
// refuse to launch code that someone other than its owner could have written
// (see Loader), and hand the plugin nothing it was not configured with: the
// environment is rebuilt rather than inherited, so an ambient credential in
// drivel's environment does not become the plugin's. See the security notes on
// Loader for the full list and for what remains the user's responsibility.
//
// # Writing a plugin
//
// A backend's main is three lines:
//
//	func main() {
//		plugin.Serve("mykind", myprovider.Factory)
//	}
//
// Everything else — the handshake, the gRPC plumbing, capability negotiation,
// routing the provider's log output back to the mount that launched it — is this
// package's business. Build the result as `drivel-provider-mykind` and drop it
// in one of the directories Loader searches.
package plugin

import (
	goplugin "github.com/hashicorp/go-plugin"
)

// ProtocolVersion is the version of the wire protocol in proto/drivel/plugin/v1.
// A host refuses to launch a plugin that does not speak exactly this version,
// with a message naming both numbers.
//
// It is a single number rather than a range because the failure it prevents is
// silent: a plugin that is *almost* compatible answers most calls correctly and
// loses a capability, a sentinel error or an mtime somewhere in the middle,
// which surfaces as data not syncing rather than as an error. Adding a field to
// the protocol does not bump it — protobuf already makes that compatible in both
// directions, and that is the mechanism to use for anything additive. Removing
// or changing the meaning of one does.
const ProtocolVersion = 1

// MagicCookieKey and MagicCookieValue are go-plugin's handshake. They are not a
// security measure and go-plugin's own documentation says so; their job is to
// make the failure mode of running an unrelated executable a clean "this is not
// a drivel plugin" rather than a hang waiting for a handshake line that is never
// coming.
const (
	MagicCookieKey   = "DRIVEL_PLUGIN"
	MagicCookieValue = "drivel-provider-v1"
)

// handshake is the agreed greeting between a host and a plugin.
var handshake = goplugin.HandshakeConfig{
	ProtocolVersion:  ProtocolVersion,
	MagicCookieKey:   MagicCookieKey,
	MagicCookieValue: MagicCookieValue,
}

// pluginName is the key a provider is dispensed under. There is exactly one
// service per plugin, so the name is a constant rather than a parameter: a
// plugin serves one backend kind, and which kind that is was decided by the
// filename the host found it under.
const pluginName = "provider"

// BinaryPrefix is what a provider plugin's executable is called: this prefix
// followed by the kind it implements, so `drivel-provider-gdrive` provides
// `gdrive`. The kind is taken from the filename and nothing else — a plugin does
// not get to announce what it is, because the alternative is two files claiming
// the same kind and a resolution order nobody would remember.
const BinaryPrefix = "drivel-provider-"
