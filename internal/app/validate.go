// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package app

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// Validate checks a set of mounts against each other before any of them is
// opened. It runs on the config path and the flag path alike — the single-mount
// case is just the N-mount case with N of one, and the checks that do not need a
// second mount (a state DB inside its own backing tree) were always worth making.
//
// Every check here is about a way one mount can corrupt another's data, or its
// own. Failing at startup with a specific message is the whole point: each of
// these otherwise surfaces as a deadlock, an opaque five-second bbolt timeout, or
// a file quietly synced to the wrong account.
func Validate(specs []MountSpec) error {
	if len(specs) == 0 {
		return errors.New("no mounts configured")
	}

	resolved := make([]resolvedPaths, len(specs))
	for i, s := range specs {
		r, err := resolve(s)
		if err != nil {
			return err
		}
		resolved[i] = r
	}

	var errs []error
	seenName := map[string]int{}
	seenState := map[string]int{}
	for i, r := range resolved {
		if prev, dup := seenName[r.name]; dup {
			// Names key the default state directory and prefix every log line.
			errs = append(errs, fmt.Errorf("mounts #%d and #%d are both named %q", prev+1, i+1, r.name))
		}
		seenName[r.name] = i

		if r.state != "" {
			if prev, dup := seenState[r.state]; dup {
				// Two engines sharing one state DB read each other's echo records as
				// their own. Echoes are §4's loop suppression AND M7b's delete
				// baseline, so this can infer deletions across accounts. bbolt would
				// eventually refuse with a five-second lock timeout; say so now.
				errs = append(errs, fmt.Errorf("mounts %q and %q share the state DB %s (echo records are per-account; sharing them can infer deletes across accounts)",
					resolved[prev].name, r.name, r.state))
			}
			seenState[r.state] = i
		}

		if r.inPlaceRequested && r.dataGiven {
			errs = append(errs, fmt.Errorf("mount %q sets its backing dir to its own mountpoint; omit it to select in-place mode", r.name))
		}

		// A state DB inside a backing tree is synced to the cloud like any other
		// file — and its own writes generate the events that cause more writes.
		// Checked against every mount including this one: a state DB inside its
		// OWN backing tree is the version of this that needed no second mount.
		for _, other := range resolved {
			if r.state != "" && within(r.state, other.backing) {
				errs = append(errs, fmt.Errorf("mount %q keeps its state DB inside mount %q's backing tree (%s); it would sync itself to the cloud",
					r.name, other.name, other.backing))
			}
		}
	}

	for i, a := range resolved {
		for j, b := range resolved {
			if i == j {
				continue
			}
			if a.mountpoint == b.mountpoint {
				// Symmetric, so report it once rather than with the names swapped.
				if i < j {
					errs = append(errs, fmt.Errorf("mounts %q and %q share the mountpoint %s", a.name, b.name, a.mountpoint))
				}
				continue
			}
			// Reading a's backing through b's FUSE handler recurses into our own
			// filesystem — the in-place cardinal rule (DESIGN.md §2.7), which only
			// ever applied to one mount's own mountpoint before there were several.
			if within(a.backing, b.mountpoint) {
				errs = append(errs, fmt.Errorf("mount %q's backing dir %s is inside mount %q's mountpoint %s; backing I/O would recurse through the other mount",
					a.name, a.backing, b.name, b.mountpoint))
			}
			// The other direction leaks rather than deadlocks: everything written to
			// a shows up in b's backing tree and is pushed to b's remote.
			if within(a.mountpoint, b.backing) {
				errs = append(errs, fmt.Errorf("mount %q is mounted inside mount %q's backing tree %s; its files would sync to both",
					a.name, b.name, b.backing))
			}
		}
	}
	return dedupe(errs)
}

type resolvedPaths struct {
	name       string
	mountpoint string
	// backing is what the mount actually reads and writes: the data dir, or the
	// mountpoint itself in in-place mode.
	backing          string
	state            string
	dataGiven        bool
	inPlaceRequested bool
}

func resolve(s MountSpec) (resolvedPaths, error) {
	r := resolvedPaths{name: s.label(), dataGiven: s.DataDir != ""}
	if r.name == "" {
		return r, errors.New("a mount has neither a name nor a mountpoint")
	}
	var err error
	if r.mountpoint, err = filepath.Abs(s.Mountpoint); err != nil {
		return r, fmt.Errorf("%s: resolving mountpoint: %w", r.name, err)
	}
	r.backing = r.mountpoint
	if s.DataDir != "" {
		if r.backing, err = filepath.Abs(s.DataDir); err != nil {
			return r, fmt.Errorf("%s: resolving backing dir: %w", r.name, err)
		}
	}
	r.inPlaceRequested = r.backing == r.mountpoint
	if s.StateDB != "" {
		if r.state, err = filepath.Abs(s.StateDB); err != nil {
			return r, fmt.Errorf("%s: resolving state DB: %w", r.name, err)
		}
	}
	return r, nil
}

// within reports whether child is parent or lies beneath it. Both must already
// be absolute and clean.
func within(child, parent string) bool {
	if child == parent {
		return true
	}
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// dedupe collapses the mirrored pair errors — every cross-mount check runs in
// both directions, so a single misconfiguration would otherwise be reported
// twice with the names swapped.
func dedupe(errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	seen := map[string]bool{}
	out := errs[:0]
	for _, e := range errs {
		if seen[e.Error()] {
			continue
		}
		seen[e.Error()] = true
		out = append(out, e)
	}
	return errors.Join(out...)
}
