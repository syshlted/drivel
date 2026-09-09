// Package completion renders shell completion scripts from a description of a
// command-line program.
//
// It exists so that drivel's completions are generated from drivel's own
// flag.FlagSets rather than maintained as two shell scripts alongside them. A
// hand-written completion is a copy of the flag list that nothing checks: the
// flags move, the copy does not, and the first anyone hears of it is a flag that
// will not tab-complete. Here the names, defaults and boolean-ness come from the
// real flag set; only the one-line summary and the "what does this value look
// like" hint are written by hand, and the generator refuses a flag that has
// neither (see cmd/drivel, build tag `completions`).
//
// The package is pure: it takes a description and returns bytes. Nothing here
// knows what drivel is, and nothing here touches a file — which is what lets the
// renderers be tested by reading their output rather than by running a shell.
package completion

import (
	"fmt"
	"sort"
	"strings"
)

// Kind says what a flag's *value* looks like, which is the whole of what a
// completion script can usefully offer for it.
type Kind int

const (
	// Opaque values cannot be suggested: a Drive folder ID, a port, a duration.
	// Distinct from "unset" on purpose — a flag whose value nothing can suggest is
	// a decision, and the generator makes you make it.
	Opaque Kind = iota
	// File completes filenames.
	File
	// Dir completes directories only.
	Dir
	// Enum completes a fixed set of words, which is also what makes it worth
	// refusing an unknown value at startup.
	Enum
	// Flag takes no value at all.
	None
)

// Flag is one flag, as a completion script needs to see it.
type Flag struct {
	Name    string   // without the leading dash
	Summary string   // one line, for the menu; NOT the flag's full usage text
	Kind    Kind     // what its value looks like
	Values  []string // for Enum
	// Arg names the value in zsh's menu ("mode", "folder id"). Empty falls back
	// to the flag's own name, which reads badly enough to be worth filling in.
	Arg string
}

// Command is one subcommand and its flags.
type Command struct {
	Name    string
	Summary string
	Flags   []Flag
	// Default marks the command a bare flag implies, so `drivel -mount ...` is
	// completed as `drivel mount -mount ...`.
	Default bool
}

// App is a whole program.
type App struct {
	Name     string
	Commands []Command
	// Extra are commands with no flags of their own, offered in the command slot
	// (drivel's `help`). Keeping them out of Commands is what stops the renderers
	// having to special-case a flagless command everywhere they iterate.
	Extra []Command
}

// Validate reports the mistakes that would otherwise become a completion script
// that misbehaves quietly.
//
// The cross-command check is the one worth explaining: bash completes a value by
// looking at the previous word alone, with no idea which subcommand it is in, so
// two commands that give one flag name different value kinds cannot both be
// right. drivel has no such pair today, and this is what keeps it that way — the
// alternative is a completion that offers directories for a flag that takes a
// port, in whichever command lost the race.
func (a App) Validate() error {
	if a.Name == "" {
		return fmt.Errorf("app has no name")
	}
	kinds := map[string]Flag{}
	for _, c := range a.Commands {
		if c.Name == "" {
			return fmt.Errorf("%s: a command has no name", a.Name)
		}
		seen := map[string]bool{}
		for _, f := range c.Flags {
			switch {
			case f.Name == "":
				return fmt.Errorf("%s %s: a flag has no name", a.Name, c.Name)
			case f.Summary == "":
				return fmt.Errorf("%s %s: flag -%s has no summary", a.Name, c.Name, f.Name)
			case seen[f.Name]:
				return fmt.Errorf("%s %s: flag -%s is defined twice", a.Name, c.Name, f.Name)
			case f.Kind == Enum && len(f.Values) == 0:
				return fmt.Errorf("%s %s: flag -%s is an enum with no values", a.Name, c.Name, f.Name)
			case f.Kind != Enum && len(f.Values) > 0:
				return fmt.Errorf("%s %s: flag -%s has values but is not an enum", a.Name, c.Name, f.Name)
			}
			seen[f.Name] = true
			if prev, ok := kinds[f.Name]; ok && (prev.Kind != f.Kind || !sameWords(prev.Values, f.Values)) {
				return fmt.Errorf("%s: -%s means different things in two commands (%v vs %v); "+
					"bash completes a value from the previous word alone and cannot tell them apart",
					a.Name, f.Name, prev.Kind, f.Kind)
			}
			kinds[f.Name] = f
		}
	}
	return nil
}

func sameWords(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (a App) defaultCommand() (Command, bool) {
	for _, c := range a.Commands {
		if c.Default {
			return c, true
		}
	}
	return Command{}, false
}

// commandNames is every name offered in the command slot, in the order given.
func (a App) commandNames() []string {
	var out []string
	for _, c := range a.Commands {
		out = append(out, c.Name)
	}
	for _, c := range a.Extra {
		out = append(out, c.Name)
	}
	return out
}

// flagsByKind collects every flag of one kind across all commands, deduplicated
// and sorted, for the grouped `case` arms both renderers emit. Validate has
// already established that a name means one thing everywhere.
func (a App) flagsByKind(k Kind) []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range a.Commands {
		for _, f := range c.Flags {
			if f.Kind == k && !seen[f.Name] {
				seen[f.Name] = true
				out = append(out, "-"+f.Name)
			}
		}
	}
	sort.Strings(out)
	return out
}

// enumFlags is every Enum flag across all commands, in name order, deduplicated.
func (a App) enumFlags() []Flag {
	seen := map[string]bool{}
	var out []Flag
	for _, c := range a.Commands {
		for _, f := range c.Flags {
			if f.Kind == Enum && !seen[f.Name] {
				seen[f.Name] = true
				out = append(out, f)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// flagNames is one command's flags as "-name", in declaration order, with the
// help pair appended — every flag set answers -h and -help without declaring
// them, so a completion that omitted them would be missing the two flags people
// type most often when they are lost.
func flagNames(c Command) []string {
	out := make([]string, 0, len(c.Flags)+2)
	for _, f := range c.Flags {
		out = append(out, "-"+f.Name)
	}
	return append(out, "-h", "-help")
}

func joinWords(w []string) string { return strings.Join(w, " ") }
