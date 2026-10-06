// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package config

import (
	"fmt"

	"github.com/syshlted/drivel/internal/app"
)

// Listing is one mount of a config file, resolved exactly as mounting it would
// resolve it, plus the config-level facts an app.MountSpec deliberately drops.
//
// Resolved rather than as-written is the whole point of the type. A reader who
// wanted the file's own words has the file; what they cannot see there is the
// name a mount will report itself under, the state DB that gets derived from it,
// the defaults nobody typed, and which provider settings an account contributed
// — all of which decide what the mount does and none of which appear in the TOML.
// Taking them from the same resolution the mount uses is what keeps the listing
// from becoming a second, drifting account of what a config file means.
type Listing struct {
	// Spec is what app.New would be handed for this mount.
	Spec app.MountSpec

	// Account is the [account.NAME] this mount draws credentials from, empty for
	// a log-only mount. It is on the listing rather than on the spec because
	// nothing below cmd/drivel has any use for it: app names a provider *kind*
	// and knows nothing of accounts, and putting it on MountSpec to display it
	// would be a field carried everywhere and read in one place.
	Account string
}

// List resolves the mounts sel selects, as Listings.
//
// It is the one loop: SpecsFor and Specs are both this, with the config-level
// half dropped. Resolving twice in two places is how the listing would come to
// disagree with the mount it claims to describe.
//
// What it is not is a validator. The cross-mount guards in app.Validate — shared
// state DBs, overlapping trees, duplicate names — are deliberately not run here:
// a listing is the thing you reach for *because* a config file is misbehaving,
// and one that refused to print until the file was already correct would be
// useless at the only moment it is wanted. It still fails on a mount it cannot
// resolve at all, because there is nothing to print for one.
func (c *Config) List(sel Selector) ([]Listing, error) {
	// Refused here as well as at the flag layer, which words it for the flags the
	// user typed. An intersection would be the only other reading and it answers
	// a question nobody asks: a name already identifies one mount, so adding the
	// account it belongs to can only confirm or contradict it.
	if sel.Name != "" && sel.Account != "" {
		return nil, fmt.Errorf("a selector names a mount or an account, not both")
	}

	out := make([]Listing, 0, len(c.mounts))
	for i, m := range c.mounts {
		// Filtered before it is resolved, so a malformed mount nobody asked for
		// cannot keep the one they did ask for from coming up. The name matched on
		// therefore has to be the name the spec will carry, which is why both go
		// through mountEntry.specName rather than computing the fallbacks twice.
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
		out = append(out, Listing{Spec: spec, Account: m.accountName})
	}
	if len(out) == 0 && !sel.empty() {
		return nil, c.noMatch(sel)
	}
	return out, nil
}
