// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package config

import (
	"fmt"
	"sort"
	"strings"

	"github.com/syshlted/drivel/internal/app"
)

// Selector narrows a config file to a subset of the mounts it describes. The
// zero value selects all of them, which is what `drivel mount` has always done.
//
// It filters and never describes: nothing here changes what a mount *is*, which
// is why a selector composes with a config file where a shaping flag cannot
// (see cmd/drivel's mountShapingFlags), and why a selector without a config file
// is an error rather than a mount description of its own.
type Selector struct {
	// Name matches the one mount that Specs would call this. Names are unique —
	// app.Validate refuses duplicates — so it selects one mount or none.
	Name string

	// Account matches every mount whose account = NAME, so it selects any number
	// of them. A log-only mount names no account and is never selected by it.
	Account string
}

func (s Selector) empty() bool { return s.Name == "" && s.Account == "" }

// SpecsFor is Specs narrowed to sel.
//
// It is List with the config-level half dropped, because a selection that
// resolved separately from the listing of the same selection would be two
// answers to one question — and the listing is read precisely to find out what
// the mount will do.
func (c *Config) SpecsFor(sel Selector) ([]app.MountSpec, error) {
	ls, err := c.List(sel)
	if err != nil {
		return nil, err
	}
	out := make([]app.MountSpec, len(ls))
	for i, l := range ls {
		out[i] = l.Spec
	}
	return out, nil
}

// noMatch explains an empty selection in terms of the mistake behind it. An
// account that exists with no mount of its own is a different error from one
// that was mistyped, and the fix for each is a different edit.
func (c *Config) noMatch(sel Selector) error {
	if sel.Name != "" {
		return fmt.Errorf("no mount is named %q%s", sel.Name, listed(c.mountNames()))
	}
	if _, ok := c.accounts[sel.Account]; ok {
		return fmt.Errorf("account %q is defined but no [[mount]] uses it", sel.Account)
	}
	return fmt.Errorf("no mount uses account %q%s", sel.Account, listed(c.mountAccounts()))
}

// mountNames is every name Specs would hand out, in sorted order.
func (c *Config) mountNames() []string {
	names := make([]string, 0, len(c.mounts))
	for _, m := range c.mounts {
		names = append(names, m.specName())
	}
	sort.Strings(names)
	return names
}

// mountAccounts is every account some mount uses — which is narrower than every
// account the file defines, and is the set -account can actually select from.
func (c *Config) mountAccounts() []string {
	seen := map[string]bool{}
	names := make([]string, 0, len(c.mounts))
	for _, m := range c.mounts {
		if m.accountName == "" || seen[m.accountName] {
			continue
		}
		seen[m.accountName] = true
		names = append(names, m.accountName)
	}
	sort.Strings(names)
	return names
}

func listed(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return " (defined: " + strings.Join(names, ", ") + ")"
}
