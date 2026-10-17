// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

//go:build !unix

package control

import "errors"

// checkPrivateDir cannot establish ownership off unix, so it refuses: a control
// socket whose directory cannot be shown to be private is not served.
func checkPrivateDir(string) error {
	return errors.New("the control socket is not supported on this platform")
}
