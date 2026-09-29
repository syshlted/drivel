// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package completion

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// sample is a two-command app with one flag of every kind, which is enough to
// exercise every branch both renderers have.
func sample() App {
	return App{
		Name: "prog",
		Commands: []Command{
			{Name: "go", Summary: "Do the thing", Default: true, Flags: []Flag{
				{Name: "at", Summary: "where to work", Kind: Dir, Arg: "dir"},
				{Name: "from", Summary: "input file", Kind: File, Arg: "file"},
				{Name: "mode", Summary: "how to do it", Kind: Enum, Values: []string{"fast", "slow"}, Arg: "mode"},
				{Name: "count", Summary: "how many", Kind: Opaque, Arg: "n"},
				{Name: "loud", Summary: "say more", Kind: None},
			}},
			{Name: "setup", Summary: "Get ready", Flags: []Flag{
				{Name: "from", Summary: "input file", Kind: File, Arg: "file"},
			}},
		},
		Extra: []Command{{Name: "help", Summary: "Show usage"}},
	}
}

// The mistakes Validate exists to catch. Each of these produces a completion
// script that is wrong in a way nobody would notice from reading it.
func TestValidateRejects(t *testing.T) {
	cases := []struct {
		name string
		app  App
		want string
	}{
		{"no summary", App{Name: "p", Commands: []Command{{Name: "c", Flags: []Flag{{Name: "f"}}}}}, "no summary"},
		{"enum with no values", App{Name: "p", Commands: []Command{{Name: "c", Flags: []Flag{
			{Name: "f", Summary: "s", Kind: Enum}}}}}, "no values"},
		{"values without enum", App{Name: "p", Commands: []Command{{Name: "c", Flags: []Flag{
			{Name: "f", Summary: "s", Kind: Opaque, Values: []string{"a"}}}}}}, "not an enum"},
		{"duplicate flag", App{Name: "p", Commands: []Command{{Name: "c", Flags: []Flag{
			{Name: "f", Summary: "s"}, {Name: "f", Summary: "s"}}}}}, "defined twice"},
		// The one that is not obvious: bash decides a value from the previous word
		// alone, so one name cannot mean two things across commands.
		{"kind conflict across commands", App{Name: "p", Commands: []Command{
			{Name: "a", Flags: []Flag{{Name: "f", Summary: "s", Kind: File}}},
			{Name: "b", Flags: []Flag{{Name: "f", Summary: "s", Kind: Dir}}},
		}}, "different things in two commands"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.app.Validate()
			if err == nil {
				t.Fatalf("Validate accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
	if err := sample().Validate(); err != nil {
		t.Errorf("Validate rejected the sample app: %v", err)
	}
}

// A generated script is checked by running it, not by reading it. bash sources
// the file and answers a completion the same way it would for a real user; a
// rendering mistake shows up as the wrong answer or a syntax error, both of
// which an eyeball comparison misses.
func TestBashCompletes(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not installed")
	}
	script, err := Bash(sample())
	if err != nil {
		t.Fatalf("Bash: %v", err)
	}
	run := func(t *testing.T, words string, cword int) string {
		t.Helper()
		// _init_completion belongs to the bash-completion package and is not here,
		// so this exercises the fallback path the generated script carries for
		// exactly that case.
		prog := string(script) + "\nCOMP_WORDS=(" + words + ")\nCOMP_CWORD=" +
			itoa(cword) + "\n_prog\necho \"${COMPREPLY[*]}\"\n"
		out, err := exec.Command(bash, "-c", prog).CombinedOutput() //nolint:gosec // G204: a script this test just rendered
		if err != nil {
			t.Fatalf("running the generated script: %v\n%s", err, out)
		}
		return strings.TrimSpace(string(out))
	}

	if got := run(t, `prog go -mode ""`, 3); got != "fast slow" {
		t.Errorf("completing -mode gave %q; want the enum values", got)
	}
	if got := run(t, `prog go -mode sl`, 3); got != "slow" {
		t.Errorf("completing -mode sl gave %q; want slow", got)
	}
	if got := run(t, `prog ""`, 1); got != "go setup help" {
		t.Errorf("the command slot gave %q; want every command", got)
	}
	// A leading flag means the default command, so its flags are what belongs in
	// the command slot — not the union of every command's.
	if got := run(t, `prog -lo`, 1); got != "-loud" {
		t.Errorf("a leading flag gave %q; want the default command's flag", got)
	}
	if got := run(t, `prog setup -f`, 2); got != "-from" {
		t.Errorf("completing a subcommand's flags gave %q; want -from", got)
	}
	// An opaque value has nothing to offer, and must not fall through to the flag
	// list — offering -loud as the value of -count is worse than silence. The
	// half-typed value has to start with a dash for this to mean anything: with an
	// empty one the flag branch declines too, and the test would pass whether the
	// opaque arm returned or not.
	if got := run(t, `prog go -count -`, 3); got != "" {
		t.Errorf("completing an opaque value gave %q; want nothing", got)
	}
}

// zsh will not load a file whose quoting is wrong, so the description escaping
// is checked by parsing the result with zsh itself where it is available, and by
// reading the escapes where it is not.
func TestZshEscapesDescriptions(t *testing.T) {
	app := sample()
	app.Commands[0].Flags[0].Summary = `a [tricky] one: it's got "everything"`
	out, err := Zsh(app)
	if err != nil {
		t.Fatalf("Zsh: %v", err)
	}
	got := string(out)
	for _, want := range []string{`\[tricky\]`, `\:`, `'\''`} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered zsh does not escape %s:\n%s", want, got)
		}
	}
	zsh, err := exec.LookPath("zsh")
	if err != nil {
		t.Skip("zsh not installed; escaping checked by inspection only")
	}
	f := t.TempDir() + "/_prog"
	if err := writeFile(f, out); err != nil {
		t.Fatal(err)
	}
	if b, err := exec.Command(zsh, "-n", f).CombinedOutput(); err != nil { //nolint:gosec // G204: a file this test just wrote
		t.Errorf("zsh cannot parse the generated file: %v\n%s", err, b)
	}
}

// zshHarness drives a real zsh through a pseudo-terminal. A completion function
// only runs inside a completion widget, a widget only runs under ZLE, and ZLE
// needs a tty — so unlike bash, zsh cannot be handed COMP_WORDS and asked for an
// answer. zsh/zpty supplies the terminal; the _dump widget reports the line
// buffer through a file, because reading it off the screen would mean parsing
// terminal escapes.
//
// No sleep between the TAB and the ^X: ZLE handles keys in order, so completion
// has finished by the time the dump widget runs.
//
// Arguments: the completion file, the install form (fpath or eval), a scratch
// file, then one input per completion to try. Prints the registered function
// name, then the resulting line buffer for each input.
const zshHarness = `
zmodload zsh/zpty || exit 3
file=$1 mode=$2 out=$3; shift 3
dir=${file:h}; name=${${file:t}#_}
: > $out
zpty Z "zsh -f" || exit 3
w() { zpty -w Z "$1" }
await() { local i; for i in {1..200}; do [[ $(wc -l < $out) -ge $1 ]] && return 0; sleep 0.1; done; return 1 }
w 'PS1="%% "'
w 'setopt nonomatch; unsetopt beep; zstyle ":completion:*" menu no'
w "_dump() { print -r -- \"\$BUFFER\" >> $out; BUFFER=''; CURSOR=0 }"
w 'zle -N _dump; bindkey "^X" _dump'
[[ $mode == fpath ]] && w "fpath=($dir \$fpath)"
w 'autoload -Uz compinit && compinit -u -d /dev/null'
[[ $mode == eval ]] && w "eval \"\$(cat $file)\""
w "print -r -- \"registered=\${_comps[$name]}\" >> $out"
await 1 || { print -r -- "the harness shell never answered"; zpty -d Z; exit 4 }
n=1
for in; do
	zpty -w -n Z "$in"$'\t\C-x'
	(( n++ ))
	await $n || { print -r -- "TIMEOUT completing: $in"; break }
done
zpty -d Z
cat $out
`

// The zsh half of TestBashCompletes: the generated script is checked by
// completing with it, not by reading it. Both install forms are exercised
// because the last line of the file has to do a different thing in each, and
// getting that wrong is invisible to a syntax check — eval'd, the old unguarded
// call to _prog ran _arguments outside a completion widget, which failed with
// "can only be called from completion function" and left nothing registered.
func TestZshCompletes(t *testing.T) {
	zsh, err := exec.LookPath("zsh")
	if err != nil {
		t.Skip("zsh not installed")
	}
	script, err := Zsh(sample())
	if err != nil {
		t.Fatalf("Zsh: %v", err)
	}
	dir := t.TempDir()
	file := dir + "/_prog"
	if err := writeFile(file, script); err != nil {
		t.Fatal(err)
	}
	harness := dir + "/harness.zsh"
	if err := writeFile(harness, []byte(zshHarness)); err != nil {
		t.Fatal(err)
	}

	cases := []struct{ in, want string }{
		// The command slot, and the leading flag that means the default command.
		{"prog se", "prog setup"},
		{"prog -lo", "prog -loud"},
		// A subcommand's own flags, and an enum's values.
		{"prog setup -f", "prog setup -from="},
		{"prog go -mode=sl", "prog go -mode=slow"},
	}
	for _, mode := range []string{"fpath", "eval"} {
		t.Run(mode, func(t *testing.T) {
			args := []string{harness, file, mode, dir + "/out." + mode}
			for _, c := range cases {
				args = append(args, c.in)
			}
			out, err := exec.Command(zsh, append([]string{"-f"}, args...)...).CombinedOutput() //nolint:gosec // G204: a harness this test just wrote
			if err != nil {
				var ee *exec.ExitError
				if errors.As(err, &ee) && ee.ExitCode() == 3 {
					t.Skip("zsh/zpty unavailable")
				}
				t.Fatalf("running the harness: %v\n%s", err, out)
			}
			lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
			if len(lines) != len(cases)+1 {
				t.Fatalf("harness produced %d lines, want %d:\n%s", len(lines), len(cases)+1, out)
			}
			// Half the bug: with nothing registered, every completion below would
			// fall back to filenames and quietly "pass" for the wrong reason.
			if lines[0] != "registered=_prog" {
				t.Fatalf("the %s form left %q registered; want _prog\n%s", mode, lines[0], out)
			}
			for i, c := range cases {
				if got := strings.TrimSpace(lines[i+1]); got != c.want {
					t.Errorf("completing %q gave %q; want %q", c.in, got, c.want)
				}
			}
		})
	}
}

// The reported failure, isolated from the completion machinery: the eval form
// must define and register the function and print nothing at all. It is a
// separate test because it needs no terminal, so it still guards this if zpty
// is unavailable wherever the suite runs.
func TestZshEvalFormIsSilent(t *testing.T) {
	zsh, err := exec.LookPath("zsh")
	if err != nil {
		t.Skip("zsh not installed")
	}
	script, err := Zsh(sample())
	if err != nil {
		t.Fatalf("Zsh: %v", err)
	}
	file := t.TempDir() + "/_prog"
	if err := writeFile(file, script); err != nil {
		t.Fatal(err)
	}
	prog := `autoload -Uz compinit && compinit -u -d /dev/null
eval "$(cat ` + file + `)" || exit 1
[[ $_comps[prog] == _prog ]] || { print -r -- "not registered: [$_comps[prog]]"; exit 1 }
`
	out, err := exec.Command(zsh, "-f", "-c", prog).CombinedOutput() //nolint:gosec // G204: a script this test just rendered
	if err != nil {
		t.Fatalf("eval'ing the generated script: %v\n%s", err, out)
	}
	if len(out) != 0 {
		t.Errorf("eval'ing the generated script printed %q; want nothing", out)
	}
}
