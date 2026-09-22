// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	width := 0
	for _, s := range settings {
		if len(s.Key) > width {
			width = len(s.Key)
		}
	}
	for _, s := range settings {
		fmt.Fprintf(&b, "%-*s = %s\n", width, s.Key, quoteValue(s.Value))
	}
	return b.String()
}

// AppendAccount adds an account to the config file, creating the file and its
// directory if needed. It returns ErrAccountExists rather than touching an
// account that is already defined.
//
// Appending, never rewriting: a config file is hand-edited and commented, and
// re-serializing it through a TOML encoder would silently drop every comment in
// it — which is most of the reason the format was chosen. Appending a table
// header is safe wherever the file currently ends, because a header closes
// whatever table preceded it.
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
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	lead, err := separator(path)
	if err != nil {
		return err
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("opening %s: %w", path, err)
	}
	if _, err := f.WriteString(lead + AccountBlock(name, settings)); err != nil {
		_ = f.Close()
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("syncing %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", path, err)
	}
	return nil
}

// separator is what to write before a new table so it neither runs into the
// previous line nor opens the file with a blank one.
func separator(path string) (string, error) {
	b, err := os.ReadFile(path)
	switch {
	case err != nil && !errors.Is(err, os.ErrNotExist):
		// Checked before the length, or an unreadable file would be
		// indistinguishable from an empty one and the error would be lost.
		return "", fmt.Errorf("reading %s: %w", path, err)
	case len(b) == 0:
		return "", nil
	case b[len(b)-1] != '\n':
		// The last line has no newline of its own; supply it, then the blank line.
		return "\n\n", nil
	default:
		return "\n", nil
	}
}

// quoteKey renders a bare key where TOML allows one and a quoted key otherwise.
func quoteKey(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

func quoteValue(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}
