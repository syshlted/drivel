package completion

import (
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
