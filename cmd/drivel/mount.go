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
	"github.com/zishmusic/drivel/internal/provider"
	"github.com/zishmusic/drivel/internal/provider/gdrive"
	"github.com/zishmusic/drivel/internal/syncengine"
)

// driveKind is the name the Drive backend is registered under. The mount flags
// are Drive-shaped by history (-drive-root, -credentials), so the flag path
// always selects this one; a config file may name any registered kind.
const driveKind = "gdrive"

// mountShapingFlags describe *what* to mount, which is exactly what a config file
// is for. Mixing the two would need a precedence rule that nobody would remember,
// so passing one alongside -config is an error instead. -debug and -pprof are
// absent deliberately: they change how a mount reports and how the process can be
// inspected, not what any of it is.
var mountShapingFlags = []string{
	"mount", "data", "credentials", "token", "state", "index",
	"drive-root", "drive-sweep-mode", "lazy", "xattr", "resync", "materialize", "max-deletes",
	"sweep-interval", "upload-workers", "hydrate-workers",
}

func runMount(args []string) error {
	fset := flag.NewFlagSet("mount", flag.ExitOnError)
	configPath := fset.String("config", "", "TOML config file describing one or more mounts; defaults to $XDG_CONFIG_HOME/drivel/config.toml when no mount flags are given")
	mountpoint := fset.String("mount", "", "path to mount the filesystem (required unless -config is used)")
	dataDir := fset.String("data", "", "backing directory (source of truth). If omitted, in-place mode uses the mount dir as its own backing (Linux only)")
	credentials := fset.String("credentials", "", "OAuth client secret JSON; enables Drive sync (else log-only)")
	token := fset.String("token", "token.json", "path to the cached OAuth token (from 'drivel login')")
	stateDB := fset.String("state", "drivel-state.db", "path to the sync-state DB (cursor + echo records); kept outside the backing tree")
	indexDB := fset.String("index", "drivel-index.db", "path to the provider's path↔ID index (a cache; safe to delete); \"\" disables it")
	driveRoot := fset.String("drive-root", "root", "Drive folder ID mapped to the mount root")
	driveSweepMode := fset.String("drive-sweep-mode", "", "how the enumeration sweep walks Drive: \"scoped\" descends from -drive-root, \"flat\" lists the whole account, \"auto\" (default) descends unless -drive-root names the whole Drive")
	lazy := fset.Bool("lazy", false, "lazy hydration (M5): materialise remote files as placeholders and fetch content on first read (requires -credentials)")
	xattr := fset.Bool("xattr", false, "serve extended attributes through the mountpoint by passing them to the backing store; off by default because it exposes drivel's own placeholder marker to anything that can write to the mount")
	resync := fset.Bool("resync", false, "enumerate the whole remote tree and reconcile it against the backing dir at startup, even if a baseline already exists")
	materialize := fset.Bool("materialize", false, "during a reconcile in eager mode, download remote files that have no local copy (implied by -lazy, where it costs only a placeholder)")
	maxDeletes := fset.Int("max-deletes", syncengine.DefaultMaxDeletes, "cap on deletions one reconcile may infer, in either direction; 0 for no limit")
	sweepInterval := fset.Duration("sweep-interval", syncengine.DefaultSweepInterval, "re-enumerate and reconcile the remote tree this often, timed from the last completed sweep; 0 disables it")
	uploadWorkers := fset.Int("upload-workers", syncengine.DefaultWorkers, "how many files this mount uploads at once; same-path writes stay ordered whatever this is. Each in-flight upload can hold a provider-sized chunk buffer (16 MiB on Drive), and past the point the provider throttles, more workers cost quota rather than throughput")
	hydrateWorkers := fset.Int("hydrate-workers", hydrate.DefaultWorkers, "how many placeholders this mount fetches at once under -lazy; bounds a recursive read over a lazy tree, which would otherwise fault every file simultaneously")
	debug := fset.Bool("debug", false, "enable FUSE debug logging")
	pprofAddr := fset.String("pprof", "", "serve net/http/pprof on this address for goroutine and heap profiling; a bare port means loopback, and a non-loopback address needs -pprof-allow-remote; empty disables it")
	pprofRemote := fset.Bool("pprof-allow-remote", false, "permit -pprof to bind an address other than loopback, publishing this process's heap — and so the paths and some contents of synced files — to whoever can reach it")
	_ = fset.Parse(args)

	given := map[string]bool{}
	fset.Visit(func(f *flag.Flag) { given[f.Name] = true })

	if err := validatePprofFlags(*pprofAddr, given["pprof-allow-remote"]); err != nil {
		return err
	}

	specs, err := mountSpecs(fset, given, specFlags{
		configPath: *configPath, mountpoint: *mountpoint, dataDir: *dataDir,
		credentials: *credentials, token: *token, stateDB: *stateDB, indexDB: *indexDB,
		driveRoot: *driveRoot, driveSweepMode: *driveSweepMode,
		lazy: *lazy, xattr: *xattr, resync: *resync, materialize: *materialize,
		maxDeletes: *maxDeletes, sweepInterval: *sweepInterval, debug: *debug,
		uploadWorkers: *uploadWorkers, hydrateWorkers: *hydrateWorkers,
	})
	if err != nil {
		return err
	}

	reg := provider.NewRegistry()
	if err := reg.Register(driveKind, gdrive.Factory); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Before the mounts, so a bad address fails while nothing is mounted yet, and
	// so a mount that hangs during startup is itself profilable.
	if *pprofAddr != "" {
		_, stopPprof, err := startPprof(ctx, *pprofAddr, *pprofRemote)
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
		spec.ProviderConfig = provider.StaticDecoder(gdrive.Config{
			Credentials: f.credentials,
			Token:       f.token,
			RootID:      f.driveRoot,
			SweepMode:   gdrive.SweepMode(f.driveSweepMode),
			IndexPath:   f.indexDB,
		})
	}
	return []app.MountSpec{spec}, nil
}
