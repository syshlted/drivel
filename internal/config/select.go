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
// Entries are filtered *before* they are resolved, so a malformed mount nobody
// asked for cannot keep the one they did ask for from coming up. The name a
// selector matches on therefore has to be the name Specs would give the same
// entry, which is why both go through mountEntry.specName rather than computing
// the fallbacks twice.
//
// A selector that matches nothing is an error naming what the file does define.
// Serving no mounts because a name was mistyped would exit successfully having
// done nothing at all, which is the failure an unknown config key is an error
// for.
func (c *Config) SpecsFor(sel Selector) ([]app.MountSpec, error) {
	if sel.empty() {
		return c.Specs()
	}
	// Refused here as well as at the flag layer, which words it for the flags the
	// user typed. An intersection would be the only other reading and it answers
	// a question nobody asks: a name already identifies one mount, so adding the
	// account it belongs to can only confirm or contradict it.
	if sel.Name != "" && sel.Account != "" {
		return nil, fmt.Errorf("a selector names a mount or an account, not both")
	}

	out := make([]app.MountSpec, 0, len(c.mounts))
	for i, m := range c.mounts {
		switch {
		case sel.Name != "" && m.specName() != sel.Name:
			continue
		case sel.Account != "" && m.accountName != sel.Account:
			continue
		}
		spec, err := c.spec(m)
		if err != nil {
			return nil, fmt.Errorf("mount #%d (%s): %w", i+1, m.describe(), err)
		}
		out = append(out, spec)
	}
	if len(out) == 0 {
		return nil, c.noMatch(sel)
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
