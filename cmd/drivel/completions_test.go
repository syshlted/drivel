// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"strings"
	"testing"
)

// completionApp is where every completion mistake is caught, so this is the test
// that used to be `make completions-check`.
//
// The move is not cosmetic. That target compared two committed files against what
// the flags would produce, which could only ever fail if someone forgot to run
// `make completions` — a check on a build artefact. There is no artefact now, so
// the thing worth checking is the program: that every flag has a hint, that no
// hint names a flag that has gone, that the hints agree with each flag's
// boolean-ness, and that one flag name does not mean two things across
// subcommands. completionApp enforces all four, and this is what runs it in CI.
func TestEveryFlagHasAUsableCompletionHint(t *testing.T) {
	if _, err := completionApp(); err != nil {
		t.Fatalf("completionApp: %v\n\n"+
			"A flag was probably added or renamed without updating completionHints "+
			"in cmd/drivel/completions.go.", err)
	}
}

// Both shells render, and the output is not empty. The renderers have their own
// tests in internal/completion — bash sources its output and zsh parses it — so
// what this adds is that drivel's *own* description survives them.
func TestCompletionRendersForEveryShell(t *testing.T) {
	app, err := completionApp()
	if err != nil {
		t.Fatalf("completionApp: %v", err)
	}
	for shell, render := range renderers {
		t.Run(shell, func(t *testing.T) {
			out, err := render(app)
			if err != nil {
				t.Fatalf("rendering %s: %v", shell, err)
			}
			if len(out) == 0 {
				t.Fatalf("%s completion is empty", shell)
			}
			// A completion that does not mention the program is a completion for
			// something else, which is the failure an empty-output check misses.
			if !strings.Contains(string(out), "drivel") {
				t.Errorf("%s completion never mentions drivel", shell)
			}
		})
	}
}

// The command advertises exactly the shells it can render, so `drivel completion
// <TAB>` and the error message cannot offer a shell that then fails.
func TestShellsMatchTheRenderers(t *testing.T) {
	got := shells()
	if len(got) != len(renderers) {
		t.Fatalf("shells() = %v, but there are %d renderers", got, len(renderers))
	}
	for _, s := range got {
		if _, ok := renderers[s]; !ok {
			t.Errorf("shells() offers %q, which has no renderer", s)
		}
	}
}

// A shell drivel cannot render is refused by name rather than producing an empty
// script that would silently complete nothing.
func TestCompletionRefusesAnUnknownShell(t *testing.T) {
	err := runCompletion([]string{"fish"})
	if err == nil {
		t.Fatal("runCompletion(fish) succeeded; want a refusal")
	}
	if !strings.Contains(err.Error(), "fish") {
		t.Errorf("error %q does not name the shell that was asked for", err)
	}
}
