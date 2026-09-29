// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"slices"
	"testing"
)

// TestParseCommand pins the dispatch main() performs on argv. The help rows are
// the reason it exists: every one of them used to select the mount command,
// which answered with "Usage of mount:" and mount's flags.
func TestParseCommand(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		args []string
		cmd  string
		rest []string
	}{
		{"no arguments mount", nil, "", nil},
		{"subcommand is taken", []string{"login", "-account", "work"}, "login", []string{"-account", "work"}},
		{"leading flag means mount", []string{"-mount", "./mnt"}, "", []string{"-mount", "./mnt"}},
		{"help word", []string{"help"}, "help", []string{}},
		{"short help flag", []string{"-h"}, "help", []string{}},
		{"single dash help flag", []string{"-help"}, "help", []string{}},
		{"double dash help flag", []string{"--help"}, "help", []string{}},
		// The one that must not regress in the other direction: a help flag after
		// a subcommand belongs to that subcommand's flag set, which is what prints
		// its options. Only a bare help flag is a question about the program.
		{"help after a subcommand stays with it", []string{"mount", "-h"}, "mount", []string{"-h"}},
		{"help after login stays with it", []string{"login", "--help"}, "login", []string{"--help"}},
		// Not a help flag: -hydrate-workers shares its first two characters.
		{"a flag that merely starts with -h", []string{"-hydrate-workers", "2"}, "", []string{"-hydrate-workers", "2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cmd, rest := parseCommand(tc.args)
			if cmd != tc.cmd {
				t.Errorf("parseCommand(%q) command = %q, want %q", tc.args, cmd, tc.cmd)
			}
			if !slices.Equal(rest, tc.rest) {
				t.Errorf("parseCommand(%q) rest = %q, want %q", tc.args, rest, tc.rest)
			}
		})
	}
}

// TestParseCommandHelpReachesTheHelpBranch is the half the table cannot state:
// that the command parseCommand returns for a help flag is one main() actually
// has a case for. The old code returned "" here and the "-h"/"--help" arms of
// that switch were unreachable, which is precisely how the bug survived.
func TestParseCommandHelpReachesTheHelpBranch(t *testing.T) {
	t.Parallel()

	for _, arg := range []string{"-h", "-help", "--help", "help"} {
		cmd, _ := parseCommand([]string{arg})
		if cmd != "help" {
			t.Errorf("parseCommand([%q]) = %q, want the help command", arg, cmd)
		}
	}
}
