// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/syshlted/drivel/internal/app"
	"github.com/syshlted/drivel/internal/config"
	"github.com/syshlted/drivel/internal/hydrate"
	"github.com/syshlted/drivel/internal/provider/gdrive/gdconf"
	"github.com/syshlted/drivel/internal/syncengine"
	"github.com/syshlted/drivel/plugin"
	"github.com/syshlted/drivel/provider"
)

// driveKind is the name the Drive backend is provided under — the suffix of the
// `drivel-provider-gdrive` executable the loader looks for. The mount flags are
// Drive-shaped by history (-drive-root, -credentials), so the flag path always
// selects this one; a config file may name any kind that is installed.
const driveKind = "gdrive"

// newRegistry builds the provider registry every entry point mounts through.
//
// A backend is a separate *process* either way (M9), and since M23 it may not be
// a separate *file*: the kinds in bundledBackends are launched by re-executing
// this binary, and everything else comes from a drivel-provider-<kind> on the
// plugin search path. The Loader decides between them, so the precedence is
// stated in one place — a bundled kind wins, and an installed file of that name
// is ignored with a log line naming it.
//
// Out of process is the part that was never about where the code lives. The
// Drive SDK, OAuth, the QUIC transport and the SSH stack all run in the child,
// so a failure in any of them is a failure of one process that the mount
// survives and the host relaunches, whichever way that child was started.
//
// And it is still written once. Both the flag/config path and the M16 mount
// helper call this, because a backend available from the command line and
// missing at boot would be the same drift the fstab option table already
// suffered once.
func newRegistry() (*provider.Registry, error) {
	reg := provider.NewRegistry()
	ld := plugin.NewLoader(nil, nil)
	ld.Bundled(bundledKinds()...)
	if err := ld.Register(reg); err != nil {
		return nil, err
	}
	return reg, nil
}

// mountShapingFlags describe *what* to mount, which is exactly what a config file
// is for. Mixing the two would need a precedence rule that nobody would remember,
// so passing one alongside -config is an error instead. -debug and -pprof are
// absent deliberately: they change how a mount reports and how the process can be
// inspected, not what any of it is.
var mountShapingFlags = []string{
	"mount", "data", "credentials", "token", "state", "index",
	"drive-root", "drive-sweep-mode", "drive-delete", "lazy", "xattr", "resync",
	"materialize", "max-deletes", "sweep-interval", "push-delay",
	"upload-workers", "hydrate-workers",
}

// mountSelectorFlags narrow a config file to some of the mounts it describes.
// They are the opposite category to mountShapingFlags and the distinction is
// what makes them safe: a selector chooses among mounts the file already
// describes rather than describing one itself, so there is no precedence to
// arbitrate and it composes with -config the way -debug and -pprof do. The
// corollary is that a selector *requires* a config file — without one there is
// nothing to choose from, and it is refused rather than quietly ignored.
var mountSelectorFlags = []string{"name", "account"}

// mountListingFlags print what a config file describes instead of mounting it.
// A third category, and the distinction from the other two is what decides their
// rules: a listing flag neither describes a mount (so, like a selector, it needs
// a config file to read and composes with -config) nor brings one up (so, unlike
// a selector, it ends the command — nothing is mounted, nothing is served, and
// the flags describing how this process would have reported have nothing to act
// on).
var mountListingFlags = []string{"list", "full"}

// mountProcessFlags describe how this *process* reports and can be inspected,
// rather than what it mounts. They are the ones that compose with -config, and
// the ones -list has nothing to do with.
var mountProcessFlags = []string{"debug", "pprof", "pprof-allow-remote"}

// mountCLI is everything `drivel mount` accepts: the mount description in
// specFlags, plus the flags that describe how this *process* reports rather than
// what it mounts, plus the two that print the config file instead of serving it.
type mountCLI struct {
	specFlags

	// Printing the config file rather than mounting it. Not on specFlags because
	// they describe no mount and reach no MountSpec: they decide whether this
	// command mounts at all. See mountListingFlags.
	list bool
	full bool

	pprofAddr   string
	pprofRemote bool
}

// mountFlagSet defines the mount command's flags, binding them into c.
//
// It is a function rather than a block inside runMount so that exactly one
// description of these flags exists: the completion generator (build tag
// `completions`) walks this same set, so a flag cannot be added to drivel and
// missing from the shell completions. Binding directly into the struct also
// removes the pointer-and-copy block that used to sit between the definitions
// and specFlags, where a flag could be defined and then never carried.
func mountFlagSet(c *mountCLI) *flag.FlagSet {
	fset := flag.NewFlagSet("drivel mount", flag.ExitOnError)
	fset.StringVar(&c.configPath, "config", "", "TOML config file describing one or more mounts; defaults to $XDG_CONFIG_HOME/drivel/config.toml when no mount flags are given")
	fset.StringVar(&c.mountpoint, "mount", "", "path to mount the filesystem (required unless -config is used)")
	fset.StringVar(&c.selectName, "name", "", "serve only the mount with this name from the config file, instead of every mount in it")
	fset.StringVar(&c.selectAccount, "account", "", "serve only the mounts belonging to this account, instead of every mount in the config file")
	fset.BoolVar(&c.list, "list", false, "print the mounts the config file describes — their name, account, provider and mountpoint, resolved as mounting them would resolve them — and exit without mounting any of them")
	fset.BoolVar(&c.full, "full", false, "with -list, widen each row with the backing directory, the state DB, whether content is resident or faulted in, the sweep interval and the delete cap")
	fset.StringVar(&c.dataDir, "data", "", "backing directory (source of truth). If omitted, in-place mode uses the mount dir as its own backing (Linux only)")
	fset.StringVar(&c.credentials, "credentials", "", "OAuth client secret JSON; enables Drive sync (else log-only)")
	fset.StringVar(&c.token, "token", "token.json", "path to the cached OAuth token (from 'drivel login')")
	fset.StringVar(&c.stateDB, "state", "drivel-state.db", "path to the sync-state DB (cursor + echo records); kept outside the backing tree")
	fset.StringVar(&c.indexDB, "index", "drivel-index.db", "path to the provider's path↔ID index (a cache; safe to delete); \"\" disables it")
	fset.StringVar(&c.driveRoot, "drive-root", "root", "Drive folder ID mapped to the mount root")
	fset.StringVar(&c.driveSweepMode, "drive-sweep-mode", "", "how the enumeration sweep walks Drive: \"scoped\" descends from -drive-root, \"flat\" lists the whole account, \"auto\" (default) descends unless -drive-root names the whole Drive")
	fset.StringVar(&c.driveDelete, "drive-delete", "", "what removing a file does remotely: \"trash\" (default) moves it to the Drive trash, where it can be restored for 30 days but still counts against your quota; \"permanent\" unlinks it outright, with no undo")
	fset.BoolVar(&c.lazy, "lazy", false, "lazy hydration (M5): materialise remote files as placeholders and fetch content on first read (requires -credentials)")
	fset.BoolVar(&c.xattr, "xattr", false, "serve extended attributes through the mountpoint by passing them to the backing store; off by default because it exposes drivel's own placeholder marker to anything that can write to the mount")
	fset.BoolVar(&c.resync, "resync", false, "enumerate the whole remote tree and reconcile it against the backing dir at startup, even if a baseline already exists")
	fset.BoolVar(&c.materialize, "materialize", false, "during a reconcile in eager mode, download remote files that have no local copy (implied by -lazy, where it costs only a placeholder)")
	fset.IntVar(&c.maxDeletes, "max-deletes", syncengine.DefaultMaxDeletes, "cap on deletions one reconcile may infer, in either direction; 0 for no limit")
	fset.DurationVar(&c.sweepInterval, "sweep-interval", syncengine.DefaultSweepInterval, "re-enumerate and reconcile the remote tree this often, timed from the last completed sweep; 0 disables it")
	fset.DurationVar(&c.pushDelay, "push-delay", syncengine.DefaultPushDelay, "how long a file must go unchanged before it is uploaded; a longer window coalesces repeated saves into one request and drains the outbound queue on a calmer cycle, at the cost of how soon a change reaches the remote. It cannot hold a file indefinitely, and it has no effect on one that is never closed")
	fset.IntVar(&c.uploadWorkers, "upload-workers", syncengine.DefaultWorkers, "how many files this mount uploads at once; same-path writes stay ordered whatever this is. Each in-flight upload can hold a provider-sized chunk buffer (16 MiB on Drive), and past the point the provider throttles, more workers cost quota rather than throughput")
	fset.IntVar(&c.hydrateWorkers, "hydrate-workers", hydrate.DefaultWorkers, "how many placeholders this mount fetches at once under -lazy; bounds a recursive read over a lazy tree, which would otherwise fault every file simultaneously")
	fset.BoolVar(&c.debug, "debug", false, "enable FUSE debug logging")
	fset.StringVar(&c.pprofAddr, "pprof", "", "serve net/http/pprof on this address for goroutine and heap profiling; a bare port means loopback, and a non-loopback address needs -pprof-allow-remote; empty disables it")
	fset.BoolVar(&c.pprofRemote, "pprof-allow-remote", false, "permit -pprof to bind an address other than loopback, publishing this process's heap — and so the paths and some contents of synced files — to whoever can reach it")
	return fset
}

func runMount(args []string) error {
	var c mountCLI
	fset := mountFlagSet(&c)
	_ = fset.Parse(args)

	given := map[string]bool{}
	fset.Visit(func(f *flag.Flag) { given[f.Name] = true })

	if err := checkListingFlags(given); err != nil {
		return err
	}
	// Before the registry, the signal context and any mounting: -list reads a
	// file and prints it, and everything below this line is setup for serving.
	if c.list {
		return runMountList(given, c.specFlags, c.full)
	}

	if err := validatePprofFlags(c.pprofAddr, given["pprof-allow-remote"]); err != nil {
		return err
	}

	specs, err := mountSpecs(fset, given, c.specFlags)
	if err != nil {
		return err
	}

	reg, err := newRegistry()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Before the mounts, so a bad address fails while nothing is mounted yet, and
	// so a mount that hangs during startup is itself profilable.
	if c.pprofAddr != "" {
		_, stopPprof, err := startPprof(ctx, c.pprofAddr, c.pprofRemote)
		if err != nil {
			return err
		}
		defer stopPprof()
	}

	a, err := app.New(ctx, specs, reg)
	if err != nil {
		return err
	}
	// Close only after Run returns: the backing dirfds are what the mounts read
	// through, and the engines drain inside Run.
	defer a.Close() //nolint:errcheck // the process is exiting; Run's error is the one that matters
	if a.Len() > 1 {
		log.Printf("serving %d mounts", a.Len())
	}
	return a.Run(ctx)
}

// specFlags is the parsed flag set, gathered so mountSpecs stays testable.
type specFlags struct {
	configPath string

	// The selectors. Not a mount description, but consumed where one is decided:
	// they pick which of the config file's mounts become specs. See
	// mountSelectorFlags.
	selectName    string
	selectAccount string

	mountpoint     string
	dataDir        string
	credentials    string
	token          string
	stateDB        string
	indexDB        string
	driveRoot      string
	driveSweepMode string
	driveDelete    string

	lazy          bool
	xattr         bool
	resync        bool
	materialize   bool
	maxDeletes    int
	sweepInterval time.Duration
	pushDelay     time.Duration
	debug         bool

	uploadWorkers  int
	hydrateWorkers int
}

// checkSelectorFlags refuses the two ways a selector can be self-defeating. An
// empty value is refused for the reason an explicit 0 is refused for the pool
// sizes: `-name ""` would otherwise read as "no selector" and serve every mount
// in the file, which is the opposite of what typing it asks for.
func checkSelectorFlags(given map[string]bool, f specFlags) error {
	for _, s := range []struct{ flag, value string }{
		{"name", f.selectName},
		{"account", f.selectAccount},
	} {
		if given[s.flag] && s.value == "" {
			return fmt.Errorf("-%s needs a value (omit the flag to serve every mount in the config file)", s.flag)
		}
	}
	if given["name"] && given["account"] {
		// Also refused in config.SpecsFor; worded here for the flags that were
		// typed. An intersection is the only other reading and it answers nothing:
		// a name already identifies one mount, so naming its account too can only
		// agree or contradict.
		return errors.New("-name and -account cannot be combined; -name already identifies a single mount")
	}
	return nil
}

// selectorGiven names the selector flag that was typed, for an error message.
// checkSelectorFlags has already refused more than one.
func selectorGiven(given map[string]bool) string {
	for _, f := range mountSelectorFlags {
		if given[f] {
			return f
		}
	}
	return ""
}

// mountConfigPath decides which config file this invocation reads, if any, and
// also reports where it looked when it found none — which is what lets a flag
// that needs one name the path rather than only report its absence.
//
// Extracted because -list answers the same question as -config and the
// selectors, and a second copy of "how drivel finds your config file" is a
// second answer to it.
func mountConfigPath(given map[string]bool, f specFlags) (path, defaultPath string) {
	path = f.configPath
	if !given["config"] && !given["mount"] {
		// No -mount and no -config: fall back to the config file if the user has
		// one. Absent, the caller falls through to its own error.
		if p, err := config.DefaultPath(); err == nil {
			defaultPath = p
			if _, statErr := os.Stat(p); statErr == nil {
				path = p
			}
		}
	}
	return path, defaultPath
}

// errNoConfigFile explains why a flag that reads a config file has none to read,
// in terms of the flag that was typed. `because` is the half that differs: what
// that particular flag wanted the file for.
func errNoConfigFile(flagName, because string, given map[string]bool, defaultPath string) error {
	switch {
	case given["mount"]:
		return fmt.Errorf("-%s cannot be combined with -mount; %s, so pass -config FILE instead", flagName, because)
	case defaultPath == "":
		return fmt.Errorf("-%s needs a config file; pass -config FILE", flagName)
	default:
		return fmt.Errorf("-%s needs a config file; there is none at %s (pass -config FILE)", flagName, defaultPath)
	}
}

// checkListingFlags refuses the two ways -list and -full can be inert.
//
// Both are M8 rule 6 — a setting that looks applied and is not. -full on its own
// has no table to widen, and a flag describing how this process reports has
// nothing to report on when the command prints a file and exits. Neither is
// harmful; both would be silently discarded, which is the failure an unknown
// config key is an error for.
func checkListingFlags(given map[string]bool) error {
	if !given["list"] {
		if given["full"] {
			return errors.New("-full needs -list; on its own there is no listing for it to widen")
		}
		return nil
	}
	for _, name := range mountProcessFlags {
		if given[name] {
			return fmt.Errorf("-%s cannot be combined with -list; -list prints the config file and exits without mounting anything, so -%s would have nothing to act on", name, name)
		}
	}
	return nil
}

// runMountList prints the mounts a config file describes and returns, having
// mounted nothing.
func runMountList(given map[string]bool, f specFlags, full bool) error {
	ls, path, err := mountListing(given, f)
	if err != nil {
		return err
	}
	// The table goes to stdout so it pipes; which file it came from goes to
	// stderr, because without -config that file was discovered rather than named
	// and the reader has no other way to tell which one they are looking at.
	noun := "mounts"
	if len(ls) == 1 {
		noun = "mount"
	}
	fmt.Fprintf(os.Stderr, "%d %s in %s\n", len(ls), noun, path)
	return printMountList(os.Stdout, ls, full)
}

// mountListing resolves what -list prints, and reports the file it read.
//
// It goes through config.List, which is the same resolution SpecsFor performs,
// so the listing describes the mount that would actually come up rather than a
// separate reading of the file. The shaping flags are refused for the reason
// they are refused alongside -config: this command describes no mount, and one
// described on the command line would be a mount the listing cannot show and
// nothing would have said so.
func mountListing(given map[string]bool, f specFlags) ([]config.Listing, string, error) {
	if err := checkSelectorFlags(given, f); err != nil {
		return nil, "", err
	}
	path, defaultPath := mountConfigPath(given, f)
	if path == "" {
		return nil, "", errNoConfigFile("list", "-list prints the mounts a config file describes", given, defaultPath)
	}
	for _, name := range mountShapingFlags {
		if given[name] {
			return nil, "", fmt.Errorf("-%s cannot be combined with -list; -list prints what %s describes and describes nothing itself", name, path)
		}
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, "", err
	}
	ls, err := cfg.List(config.Selector{Name: f.selectName, Account: f.selectAccount})
	if err != nil {
		return nil, "", fmt.Errorf("%s: %w", path, err)
	}
	return ls, path, nil
}

// mountSpecs decides between the config file and the flags, and returns what to
// mount either way. Both paths end in the same []app.MountSpec, so everything
// downstream — validation, opening, the shutdown ordering — has one
// implementation rather than a single-mount one that drifts from the N-mount one.
func mountSpecs(fset *flag.FlagSet, given map[string]bool, f specFlags) ([]app.MountSpec, error) {
	if err := checkSelectorFlags(given, f); err != nil {
		return nil, err
	}
	sel := config.Selector{Name: f.selectName, Account: f.selectAccount}

	path, defaultPath := mountConfigPath(given, f)

	// A selector with nothing to select from. Refused rather than ignored: the
	// flags describing a mount would otherwise serve that mount and say nothing
	// about the -name they disregarded, which is a successful mount of something
	// the user did not ask for.
	if s := selectorGiven(given); s != "" && path == "" {
		return nil, errNoConfigFile(s, "a selector chooses among the mounts a config file describes", given, defaultPath)
	}

	if path != "" {
		for _, name := range mountShapingFlags {
			if given[name] {
				return nil, fmt.Errorf("-%s cannot be combined with -config (%s); describe the mount in the config file", name, path)
			}
		}
		cfg, err := config.Load(path)
		if err != nil {
			return nil, err
		}
		specs, err := cfg.SpecsFor(sel)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if f.debug {
			for i := range specs {
				specs[i].Debug = true
			}
		}
		return specs, nil
	}

	if f.mountpoint == "" {
		fset.Usage()
		return nil, errors.New("-mount is required (or -config, or a config file at the default path)")
	}
	// Zero is refused rather than read as "use the default": a pool of that size
	// transfers nothing, and -max-deletes 0 means "no limit" three flags away, so
	// a user has every reason to expect 0 to mean something here too.
	if f.uploadWorkers < 1 {
		return nil, fmt.Errorf("-upload-workers %d is not a pool size (1 or more; omit the flag for the default of %d)", f.uploadWorkers, syncengine.DefaultWorkers)
	}
	if f.hydrateWorkers < 1 {
		return nil, fmt.Errorf("-hydrate-workers %d is not a pool size (1 or more; omit the flag for the default of %d)", f.hydrateWorkers, hydrate.DefaultWorkers)
	}
	// Zero is refused for the reason the pool sizes are: it is not a window, the
	// engine would silently substitute the default, and a user who has just read
	// that -max-deletes 0 means "no limit" has every reason to expect 0 to mean
	// something here as well.
	if f.pushDelay <= 0 {
		return nil, fmt.Errorf("-push-delay %s is not a coalescing window (omit the flag for the default of %s)", f.pushDelay, syncengine.DefaultPushDelay)
	}
	if f.lazy && f.credentials == "" {
		// A placeholder is a promise that the bytes can be fetched later; without a
		// provider there is nothing to redeem it against. Checked here as well as in
		// app.Open so the message names the flag the user actually typed.
		return nil, errors.New("-lazy requires -credentials (there is nothing to hydrate from)")
	}

	spec := app.MountSpec{
		Mountpoint:    f.mountpoint,
		DataDir:       f.dataDir,
		StateDB:       f.stateDB,
		Lazy:          f.lazy,
		Xattr:         f.xattr,
		Debug:         f.debug,
		Resync:        f.resync,
		Materialize:   f.materialize,
		MaxDeletes:    f.maxDeletes,
		SweepInterval: f.sweepInterval,
		PushDelay:     f.pushDelay,

		UploadWorkers:  f.uploadWorkers,
		HydrateWorkers: f.hydrateWorkers,
	}
	// Credentials are what turn cloud sync on; without them the mount runs
	// log-only (M1 behaviour) and never reaches a provider.
	if f.credentials != "" {
		spec.Provider = driveKind
		spec.ProviderConfig = provider.MustEncodeConfig(gdconf.Config{
			Credentials: f.credentials,
			Token:       f.token,
			RootID:      f.driveRoot,
			SweepMode:   gdconf.SweepMode(f.driveSweepMode),
			Delete:      gdconf.DeleteMode(f.driveDelete),
			IndexPath:   f.indexDB,
		})
	}
	return []app.MountSpec{spec}, nil
}
