// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package config

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/BurntSushi/toml"
)

// ErrAccountExists reports that the config file already defines an account by
// that name. It is a sentinel because the right response is not to fail but to
// show the user what to change by hand.
var ErrAccountExists = errors.New("account already defined")

// Setting is one key in an account's table. A slice rather than a map so the
// generated block comes out in a deliberate order every time.
type Setting struct{ Key, Value string }

// HasAccount reports whether path defines [account.name]. A missing file is not
// an error — it simply defines nothing.
//
// It decodes only the account names, so it works on a config file that Load
// would reject (one with no mounts yet, which is exactly what the first `drivel
// login` leaves behind).
func HasAccount(path, name string) (bool, error) {
	var doc struct {
		Accounts map[string]toml.Primitive `toml:"account"`
	}
	if _, err := toml.DecodeFile(path, &doc); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("reading %s: %w", path, err)
	}
	_, ok := doc.Accounts[name]
	return ok, nil
}

// AccountBlock renders the TOML for one account.
//
// The name is always quoted. ValidName permits a dot, and an unquoted
// [account.a.b] would silently define an account "b" nested under "a" —
// a table that nothing would ever match.
func AccountBlock(name string, settings []Setting) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[account.%s]\n", quoteKey(name))
	renderSettings(&b, settings)
	return b.String()
}

// AppendAccount adds an account to the config file, creating the file and its
// directory if needed. It returns ErrAccountExists rather than touching an
// account that is already defined — see appendBlock for why nothing here ever
// rewrites.
func AppendAccount(path, name string, settings []Setting) error {
	if err := ValidName(name); err != nil {
		return err
	}
	exists, err := HasAccount(path, name)
	if err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("%w: [account.%s] in %s", ErrAccountExists, name, path)
	}
	return appendBlock(path, AccountBlock(name, settings))
}
