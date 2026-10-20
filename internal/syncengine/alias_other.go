// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

//go:build !linux

package syncengine

import "os"

// storedName answers from the directory's listing. macOS, the platform where
// a folding backing store is the default, has F_GETPATH for this; it is not
// used because nobody can run it here (M10), and the listing is right
// everywhere, only slower.
func storedName(dir, name, _ string, fi os.FileInfo) (string, error) {
	return storedNameByListing(dir, name, fi)
}
