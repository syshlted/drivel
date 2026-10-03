// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

//go:build nobundle

package main

import "github.com/syshlted/drivel/provider"

// bundledBackends is empty under the `nobundle` tag: this build links no backend
// at all, and every provider comes from a drivel-provider-<kind> executable on
// the plugin search path, which is M9 unchanged.
//
// It exists for packaging rather than for development. A distribution that ships
// one package per backend wants the host and the backends as separate artifacts
// — so that a Drive SDK advisory is reported against the Drive backend, and so
// that a user who only mounts SFTP need not install the Drive SDK's code at all.
//
// Keeping it buildable is a gate (`make build-nobundle`, run by CI), because a
// build nobody performs is a build that stops working: the host stays honest
// about having no compiled-in provider only as long as something checks.
var bundledBackends map[string]provider.Factory
