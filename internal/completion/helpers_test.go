// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package completion

import (
	"os"
	"strconv"
)

func itoa(n int) string { return strconv.Itoa(n) }

func writeFile(path string, b []byte) error { return os.WriteFile(path, b, 0o600) }
