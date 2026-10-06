// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/syshlted/drivel/internal/config"
)

// The columns, split where -full splits them.
//
// The short set is the one a reader needs to act: NAME is what -name matches,
// ACCOUNT is what -account matches, and PROVIDER is what the mount will actually
// talk to — which is the question a config file holding both a Drive mount and an
// SFTP mount raises and never answers in its own text, since the kind is written
// once in an [account.*] table some other block refers to by name.
//
// -full adds the five that are decisions rather than tuning: where the files
// really live, where the sync state lives (derived from the name, so it appears
// nowhere in the file), whether content is resident or faulted in, how often the
// tree is re-enumerated — which on a feedless provider like SFTP is the poll
// interval and not a safety net — and the cap on how many deletions one reconcile
// may infer. The tuning numbers are deliberately left out: past about nine columns
// a table stops being readable, and unlike these five they change how fast a mount
// works rather than what it does.
var (
	shortColumns = []string{"NAME", "ACCOUNT", "PROVIDER", "MOUNTPOINT"}
	fullColumns  = append(append([]string{}, shortColumns...),
		"DATA", "STATE", "MODE", "SWEEP", "MAX-DELETES")
)

// printMountList renders ls as a table.
//
// Tab-separated through a tabwriter rather than fixed widths, so a long
// mountpoint widens its column instead of being truncated: a path a reader
// cannot copy whole is worse than a wide line, and the lines are what they came
// for.
func printMountList(w io.Writer, ls []config.Listing, full bool) error {
	columns := shortColumns
	if full {
		columns = fullColumns
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(columns, "\t"))
	for _, l := range ls {
		fmt.Fprintln(tw, strings.Join(mountRow(l, full), "\t"))
	}
	return tw.Flush()
}

// mountRow is one mount's cells, in the order of the headers above.
func mountRow(l config.Listing, full bool) []string {
	s := l.Spec
	row := []string{s.Name, orDash(l.Account), orDash(s.Provider), s.Mountpoint}
	if !full {
		return row
	}
	// A log-only mount reaches no provider, so it never enumerates and never
	// infers a deletion: the two reconcile settings it still carries are inert.
	// Printing their values would be M8 rule 6 arriving as a display — a setting
	// shown as applied that is not — so they read as absent, the way its empty
	// state DB already does.
	sweep, deletes := sweepCell(s.SweepInterval), deletesCell(s.MaxDeletes)
	if s.Provider == "" {
		sweep, deletes = "-", "-"
	}
	return append(row,
		// Empty DataDir is in-place mode (the mount is its own backing store),
		// which is a mode rather than a missing value and must not read as one.
		or(s.DataDir, "(in place)"),
		orDash(s.StateDB),
		mountMode(s.Provider, s.Lazy),
		sweep,
		deletes,
	)
}

// mountMode names what the mount does with content. Log-only comes first because
// it outranks the other two: a mount with no provider fetches nothing to be lazy
// about, and someone wondering why nothing syncs is owed that word here rather
// than having to notice an empty ACCOUNT column.
func mountMode(providerKind string, lazy bool) string {
	switch {
	case providerKind == "":
		return "log-only"
	case lazy:
		return "lazy"
	default:
		return "eager"
	}
}

// sweepCell spells out the disabling value. "0s" is what the duration prints as
// and reads like a setting of zero rather than the absence of a schedule.
func sweepCell(d time.Duration) string {
	if d == 0 {
		return "off"
	}
	return d.String()
}

// deletesCell likewise: 0 is "no limit" in this one setting, which is the
// opposite of what a bare 0 looks like next to a column of counts.
func deletesCell(n int) string {
	if n == 0 {
		return "unlimited"
	}
	return strconv.Itoa(n)
}

func or(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func orDash(s string) string { return or(s, "-") }
