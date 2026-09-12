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

	"github.com/zishmusic/drivel/internal/app"
	"github.com/zishmusic/drivel/internal/config"
	"github.com/zishmusic/drivel/internal/hydrate"
	"github.com/zishmusic/drivel/internal/provider/gdrive/gdconf"
	"github.com/zishmusic/drivel/internal/syncengine"
	"github.com/zishmusic/drivel/plugin"
	"github.com/zishmusic/drivel/provider"
)

// driveKind is the name the Drive backend is provided under — the suffix of the
// `drivel-provider-gdrive` executable the loader looks for. The mount flags are
// Drive-shaped by history (-drive-root, -credentials), so the flag path always
// selects this one; a config file may name any kind that is installed.
const driveKind = "gdrive"

// newRegistry builds the provider registry every entry point mounts through.
//
// Since M9 there is nothing compiled in for it to hold: a backend is a separate
// executable that drivel launches, so this is a scan of the plugin search path
// and a Factory per kind found there. Two things follow from that and are worth
// stating where the wiring is.
//
// The drivel binary no longer links any backend. That is the point of the
// milestone and not a side effect — the Drive SDK, OAuth, the QUIC transport and
// the SSH stack are all in the plugin that needs them, so a failure in any of
// them is a failure of one process that the mount survives and the host
// relaunches.
//
// And it is still written once. Both the flag/config path and the M16 mount
// helper call this, because a backend available from the command line and
// missing at boot would be the same drift the fstab option table already
// suffered once.
func newRegistry() (*provider.Registry, error) {
	reg := provider.NewRegistry()
	if err := plugin.NewLoader(nil, nil).Register(reg); err != nil {
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
	"materialize", "max-deletes", "sweep-interval", "upload-workers", "hydrate-workers",
}

// mountCLI is everything `drivel mount` accepts: the mount description in
// specFlags, plus the two process-level flags that describe how this *process*
// reports rather than what it mounts.
type mountCLI struct {
	specFlags

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
	fset := flag.NewFlagSet("mount", flag.ExitOnError)
	fset.StringVar(&c.configPath, "config", "", "TOML config file describing one or more mounts; defaults to $XDG_CONFIG_HOME/drivel/config.toml when no mount flags are given")
	fset.StringVar(&c.mountpoint, "mount", "", "path to mount the filesystem (required unless -config is used)")
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
	configPath     string
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
	debug         bool

	uploadWorkers  int
	hydrateWorkers int
}

// mountSpecs decides between the config file and the flags, and returns what to
// mount either way. Both paths end in the same []app.MountSpec, so everything
// downstream — validation, opening, the shutdown ordering — has one
// implementation rather than a single-mount one that drifts from the N-mount one.
func mountSpecs(fset *flag.FlagSet, given map[string]bool, f specFlags) ([]app.MountSpec, error) {
	path := f.configPath
	if !given["config"] && !given["mount"] {
		// No -mount and no -config: fall back to the config file if the user has
		// one. Absent, we fall through to the -mount required error below.
		if p, err := config.DefaultPath(); err == nil {
			if _, statErr := os.Stat(p); statErr == nil {
				path = p
			}
		}
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
		specs, err := cfg.Specs()
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
