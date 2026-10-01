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

// ErrMountExists reports that the config file already describes a mount at that
// mountpoint. A sentinel for the same reason ErrAccountExists is one: the useful
// response is to show the user the block and let them reconcile it, not to fail.
var ErrMountExists = errors.New("mount already defined")

// MountEntry is a mount to write into the config file. Empty fields are omitted,
// so a caller supplies only what it actually knows.
//
// Provider carries that provider's own settings, rendered as a [mount.provider]
// sub-table and otherwise opaque — this package never learns what a Drive folder
// ID is (M8 rule 4), on the way out as much as on the way in.
type MountEntry struct {
	Name     string
	Account  string
	Path     string
	Data     string
	Provider []Setting
}

// MountBlock renders the TOML for one mount.
//
// [mount.provider] follows the [[mount]] header and so attaches to it, and the
// next [[mount]] header closes it again — which is what keeps appending safe
// however many mounts the file already has.
func MountBlock(m MountEntry) string {
	var settings []Setting
	for _, s := range []Setting{
		{Key: "name", Value: m.Name},
		{Key: "account", Value: m.Account},
		{Key: "path", Value: m.Path},
		{Key: "data", Value: m.Data},
	} {
		if s.Value != "" {
			settings = append(settings, s)
		}
	}

	var b strings.Builder
	b.WriteString("[[mount]]\n")
	renderSettings(&b, settings)
	if len(m.Provider) > 0 {
		b.WriteString("\n[mount.provider]\n")
		renderSettings(&b, m.Provider)
	}
	return b.String()
}

// HasMount reports whether path already describes a mount at mountpoint. A
// missing file is not an error — it simply describes nothing.
//
// Mountpoints are compared after expansion, because "~/drive" and an absolute
// path to the same directory are the same mount to everything downstream:
// app.Validate refuses two mounts sharing a mountpoint, so an appender that
// compared the raw strings would happily write a config file that cannot boot.
func HasMount(path, mountpoint string) (bool, error) {
	var doc struct {
		Mounts []struct {
			Path string `toml:"path"`
		} `toml:"mount"`
	}
	if _, err := toml.DecodeFile(path, &doc); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("reading %s: %w", path, err)
	}

	dir := filepath.Dir(path)
	want, err := expand(dir, mountpoint)
	if err != nil {
		return false, err
	}
	for _, m := range doc.Mounts {
		if m.Path == "" {
			continue
		}
		got, err := expand(dir, m.Path)
		if err != nil {
			// A path this layer cannot expand is one it cannot compare. Skipping it
			// risks a duplicate the user can see and fix; failing here would block
			// an append over an unrelated malformed entry.
			continue
		}
		if got == want {
			return true, nil
		}
	}
	return false, nil
}

// AppendMount adds a mount to the config file, creating the file and its
// directory if needed. It returns ErrMountExists rather than adding a second
// mount over a mountpoint the file already claims.
//
// It does not validate the mount beyond that: what makes a *set* of mounts legal
// is app.Validate's job, it needs every mount at once, and this package cannot
// see the ones a later hand edit will add.
func AppendMount(path string, m MountEntry) error {
	if m.Path == "" {
		return errors.New("a mount needs a path (where the filesystem appears)")
	}
	if m.Name != "" {
		if err := ValidName(m.Name); err != nil {
			return err
		}
	}
	exists, err := HasMount(path, m.Path)
	if err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("%w: a [[mount]] at %s in %s", ErrMountExists, m.Path, path)
	}
	return appendBlock(path, MountBlock(m))
}
