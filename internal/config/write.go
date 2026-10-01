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
)

// appendBlock adds a rendered TOML block to the end of the config file,
// creating the file and its directory if needed.
//
// Appending, never rewriting: a config file is hand-edited and commented, and
// re-serializing it through a TOML encoder would silently drop every comment in
// it — which is most of the reason the format was chosen. Appending a table
// header is safe wherever the file currently ends, because a header closes
// whatever table preceded it. That holds for `[[mount]]` as much as for
// `[account.x]`, including when the previous block ended in a `[mount.provider]`
// sub-table.
//
// The consequence is the boundary on everything built over this: the app can
// only ever *grow* this file. Editing or removing an entry needs a rewrite, and
// a rewrite costs the user their comments, so the answer there is to print the
// change and let them make it.
func appendBlock(path, block string) error {
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
	if _, err := f.WriteString(lead + block); err != nil {
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

// renderSettings writes `key = "value"` lines with the `=` aligned, which is how
// both block renderers lay out a table body.
func renderSettings(b *strings.Builder, settings []Setting) {
	width := 0
	for _, s := range settings {
		if len(s.Key) > width {
			width = len(s.Key)
		}
	}
	for _, s := range settings {
		fmt.Fprintf(b, "%-*s = %s\n", width, s.Key, quoteValue(s.Value))
	}
}

// quoteKey renders a bare key where TOML allows one and a quoted key otherwise.
func quoteKey(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

func quoteValue(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}
