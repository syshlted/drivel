//go:build completions

// Completion generation, compiled only under the `completions` build tag.
//
// It lives behind a tag because it is build-time machinery, not a feature of the
// program: a released drivel has no reason to carry a shell-script renderer, and
// `internal/completion` is imported from this file alone, so an ordinary build
// contains neither it nor the table below. Run it through the Makefile:
//
//	make completions        # rewrite completions/ from these definitions
//	make completions-check  # fail if the checked-in files have drifted
//
// or directly:
//
//	go run -tags completions ./cmd/drivel gen-completions -o completions
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zishmusic/drivel/internal/completion"
	"github.com/zishmusic/drivel/internal/provider/gdrive"
)

// hint is the half of a flag's completion that its definition cannot supply: the
// `flag` package knows a flag's name, usage and default, but nothing about
// whether its value is a path, one of a fixed set of words, or opaque.
//
// The summary is deliberately not the flag's usage string. Usage text is a
// sentence or a paragraph — it has to be, it is what -h prints — and a
// completion menu has one line. Keeping them apart is also what lets the menu
// text stay stable while the usage grows a caveat.
type hint struct {
	summary string
	kind    completion.Kind
	values  []string
	// arg names the value in zsh's menu. Optional; the flag's own name is the
	// fallback, and "mode" reads better than "drive-delete".
	arg string
}

// completionHints is keyed "command/flag". Every flag must appear: a missing
// entry fails the generator rather than producing a completion that silently
// omits the newest flag, which is the failure the generated files exist to
// prevent. An entry for a flag that no longer exists fails it too — that one is
// how a rename would otherwise leave a stale line behind.
// Fields in order: summary, kind, enum values, zsh argument label.
var completionHints = map[string]hint{
	// --- mount ---------------------------------------------------------------
	"mount/config":             {"TOML file describing one or more mounts", completion.File, nil, "config file"},
	"mount/mount":              {"path to mount the filesystem (required unless -config)", completion.Dir, nil, "mountpoint"},
	"mount/data":               {"backing directory (source of truth); omit for in-place mode", completion.Dir, nil, "backing dir"},
	"mount/credentials":        {"OAuth client secret JSON; enables Drive sync", completion.File, nil, "credentials file"},
	"mount/token":              {"cached OAuth token from drivel login", completion.File, nil, "token file"},
	"mount/state":              {"sync-state DB (cursor + echo records)", completion.File, nil, "state db"},
	"mount/index":              {"provider path<->ID index; a cache, safe to delete", completion.File, nil, "index db"},
	"mount/drive-root":         {"Drive folder ID mapped to the mount root", completion.Opaque, nil, "folder id"},
	"mount/drive-sweep-mode":   {"how the enumeration sweep walks Drive", completion.Enum, sweepModes, "mode"},
	"mount/drive-delete":       {"what removing a file does remotely: trash it, or delete it outright", completion.Enum, deleteModes, "mode"},
	"mount/lazy":               {"materialise remote files as placeholders; fetch content on first read", completion.None, nil, ""},
	"mount/xattr":              {"serve extended attributes through the mountpoint (off by default)", completion.None, nil, ""},
	"mount/resync":             {"enumerate the remote tree and reconcile it at startup", completion.None, nil, ""},
	"mount/materialize":        {"eager mode: download remote files that have no local copy", completion.None, nil, ""},
	"mount/max-deletes":        {"cap on deletions one reconcile may infer; 0 for no limit", completion.Opaque, nil, "count"},
	"mount/sweep-interval":     {"re-enumerate this often, from the last completed sweep; 0 disables", completion.Opaque, nil, "duration"},
	"mount/upload-workers":     {"how many files this mount uploads at once", completion.Opaque, nil, "count"},
	"mount/hydrate-workers":    {"how many placeholders this mount fetches at once under -lazy", completion.Opaque, nil, "count"},
	"mount/debug":              {"enable FUSE debug logging", completion.None, nil, ""},
	"mount/pprof":              {"serve net/http/pprof on this address, e.g. localhost:6060", completion.Opaque, nil, "address"},
	"mount/pprof-allow-remote": {"let -pprof bind a non-loopback address, publishing this process's heap", completion.None, nil, ""},

	// --- login ---------------------------------------------------------------
	"login/account":       {"store this login as a named account under $XDG_CONFIG_HOME/drivel", completion.Opaque, nil, "account name"},
	"login/config":        {"config file to add the account to", completion.File, nil, "config file"},
	"login/credentials":   {"path to read/write the OAuth client secret JSON", completion.File, nil, "credentials file"},
	"login/token":         {"path to write the OAuth token", completion.File, nil, "token file"},
	"login/client-id":     {"OAuth client ID", completion.Opaque, nil, "client id"},
	"login/client-secret": {"OAuth client secret", completion.Opaque, nil, "client secret"},
	"login/project-id":    {"GCP project ID (optional)", completion.Opaque, nil, "project id"},
	"login/scope":         {"Drive scope", completion.Enum, loginScopeNames, "scope"},
	"login/port":          {"loopback port for the OAuth redirect (0 = auto)", completion.Opaque, nil, "port"},
	"login/open":          {"attempt to open the auth URL with the OS browser handler", completion.None, nil, ""},
}

// The enum values are the provider's own constants rather than copies of them,
// so a mode that is added, renamed or withdrawn cannot leave the completions
// offering a value the program refuses at startup.
var (
	sweepModes  = []string{string(gdrive.SweepAuto), string(gdrive.SweepFlat), string(gdrive.SweepScoped)}
	deleteModes = []string{string(gdrive.DeleteTrash), string(gdrive.DeletePermanent)}
)

// completionApp describes drivel to the renderers, taking the flags from the
// real flag sets. Nothing here lists a flag by name.
func completionApp() (completion.App, error) {
	mount, err := describe("mount", "Mount a directory and sync it with Google Drive (default)",
		mountFlagSet(&mountCLI{}))
	if err != nil {
		return completion.App{}, err
	}
	mount.Default = true
	login, err := describe("login", "Interactive Google OAuth setup (writes credentials.json + token.json)",
		loginFlagSet(&loginCLI{}))
	if err != nil {
		return completion.App{}, err
	}
	app := completion.App{
		Name:     "drivel",
		Commands: []completion.Command{login, mount},
		Extra:    []completion.Command{{Name: "help", Summary: "Show top-level usage"}},
	}
	if err := checkHintsAreSpent(app); err != nil {
		return completion.App{}, err
	}
	return app, app.Validate()
}

// describe turns one flag set into a Command, in declaration order.
//
// flag.VisitAll walks in lexical order rather than declaration order, which is
// fine here: a completion menu is a set, and sorted is the more useful order for
// a human reading it.
func describe(name, summary string, fset *flag.FlagSet) (completion.Command, error) {
	c := completion.Command{Name: name, Summary: summary}
	var missing []string
	fset.VisitAll(func(f *flag.Flag) {
		h, ok := completionHints[name+"/"+f.Name]
		if !ok {
			missing = append(missing, "-"+f.Name)
			return
		}
		// A bool flag takes no value; anything else does. Cross-checked rather
		// than trusted, because a table that says otherwise produces a completion
		// offering filenames to a flag that never takes one.
		isBool := false
		if bf, ok := f.Value.(interface{ IsBoolFlag() bool }); ok {
			isBool = bf.IsBoolFlag()
		}
		if isBool != (h.kind == completion.None) {
			missing = append(missing, fmt.Sprintf("-%s (bool=%v but hint says kind %v)", f.Name, isBool, h.kind))
			return
		}
		c.Flags = append(c.Flags, completion.Flag{
			Name: f.Name, Summary: h.summary, Kind: h.kind, Values: h.values, Arg: h.arg,
		})
	})
	if len(missing) > 0 {
		return completion.Command{}, fmt.Errorf(
			"%s: no usable completion hint for %s — add one to completionHints in cmd/drivel/completions.go",
			name, strings.Join(missing, ", "))
	}
	return c, nil
}

// checkHintsAreSpent is the other direction of the same guard: an entry naming a
// flag that no longer exists. Without it a rename leaves a line in the table
// that describes nothing, and the next person to read it believes it.
func checkHintsAreSpent(app completion.App) error {
	used := map[string]bool{}
	for _, c := range app.Commands {
		for _, f := range c.Flags {
			used[c.Name+"/"+f.Name] = true
		}
	}
	var stale []string
	for k := range completionHints {
		if !used[k] {
			stale = append(stale, k)
		}
	}
	if len(stale) > 0 {
		return fmt.Errorf("completionHints has entries for flags that do not exist: %s",
			strings.Join(stale, ", "))
	}
	return nil
}

// generated is what the completions directory should contain.
func generated() (map[string][]byte, error) {
	app, err := completionApp()
	if err != nil {
		return nil, err
	}
	bash, err := completion.Bash(app)
	if err != nil {
		return nil, err
	}
	zsh, err := completion.Zsh(app)
	if err != nil {
		return nil, err
	}
	return map[string][]byte{"drivel.bash": bash, "_drivel": zsh}, nil
}

// runGenCompletions writes (or checks) the generated files.
func runGenCompletions(args []string) error {
	fs := flag.NewFlagSet("gen-completions", flag.ExitOnError)
	out := fs.String("o", "completions", "directory to write the completion files to")
	check := fs.Bool("check", false, "do not write; fail if the files on disk differ from what would be written")
	_ = fs.Parse(args)

	files, err := generated()
	if err != nil {
		return err
	}
	for _, name := range []string{"drivel.bash", "_drivel"} {
		path := filepath.Join(*out, name)
		if *check {
			have, err := os.ReadFile(path) //nolint:gosec // G304: a path this program was told to check
			if err != nil {
				return fmt.Errorf("%s: %w (run 'make completions')", path, err)
			}
			if string(have) != string(files[name]) {
				return fmt.Errorf("%s is out of date; run 'make completions' and commit the result", path)
			}
			continue
		}
		if err := os.WriteFile(path, files[name], 0o644); err != nil { //nolint:gosec // G306: a shell script, world-readable by design
			return err
		}
		fmt.Fprintf(os.Stderr, "wrote %s\n", path)
	}
	return nil
}

// runExtraCommand is the tagged half of the pair in completions_off.go.
func runExtraCommand(cmd string, args []string) (handled bool, err error) {
	if cmd == "gen-completions" {
		return true, runGenCompletions(args)
	}
	return false, nil
}
