package main

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zishmusic/drivel/internal/config"
	"github.com/zishmusic/drivel/internal/provider/gdrive"
	"github.com/zishmusic/drivel/internal/syncengine"
)

// quietFlagSet stands in for the real one; mountSpecs only ever calls Usage.
func quietFlagSet() *flag.FlagSet {
	fset := flag.NewFlagSet("mount", flag.ContinueOnError)
	fset.SetOutput(os.NewFile(0, os.DevNull))
	fset.Usage = func() {}
	return fset
}

// defaults mirror the flag defaults, so a test says only what it varies — and a
// default that changes without this being updated shows up as a failing test
// rather than as nothing at all.
func defaults() specFlags {
	return specFlags{
		token:         "token.json",
		stateDB:       "drivel-state.db",
		indexDB:       "drivel-index.db",
		driveRoot:     "root",
		maxDeletes:    syncengine.DefaultMaxDeletes,
		sweepInterval: syncengine.DefaultSweepInterval,
	}
}

// Every mount flag has to reach the spec. A flag that silently stops being read
// is invisible to every other test in the tree.
func TestFlagsReachTheSpec(t *testing.T) {
	f := defaults()
	f.mountpoint = "/m"
	f.dataDir = "/d"
	f.credentials = "/creds.json"
	f.token = "/tok.json"
	f.stateDB = "/state.db"
	f.indexDB = "/index.db"
	f.driveRoot = "0Bfolder"
	f.lazy = true
	f.xattr = true
	f.debug = true
	f.resync = true
	f.materialize = true
	f.maxDeletes = 7
	f.sweepInterval = 90 * time.Minute

	specs, err := mountSpecs(quietFlagSet(), map[string]bool{"mount": true}, f)
	if err != nil {
		t.Fatalf("mountSpecs: %v", err)
	}
	if len(specs) != 1 {
		t.Fatalf("got %d specs; want 1", len(specs))
	}
	s := specs[0]
	if s.Mountpoint != "/m" || s.DataDir != "/d" || s.StateDB != "/state.db" {
		t.Errorf("paths not carried: %+v", s)
	}
	if !s.Lazy || !s.Xattr || !s.Debug || !s.Resync || !s.Materialize {
		t.Errorf("booleans not carried: %+v", s)
	}
	if s.MaxDeletes != 7 {
		t.Errorf("MaxDeletes = %d; want 7", s.MaxDeletes)
	}
	if s.SweepInterval != 90*time.Minute {
		t.Errorf("SweepInterval = %v; want 90m", s.SweepInterval)
	}
	if s.Provider != driveKind {
		t.Errorf("Provider = %q; want %q", s.Provider, driveKind)
	}

	// The Drive-shaped flags have to arrive as Drive's own config.
	var got gdrive.Config
	if err := s.ProviderConfig(&got); err != nil {
		t.Fatalf("decoding provider config: %v", err)
	}
	want := gdrive.Config{Credentials: "/creds.json", Token: "/tok.json", RootID: "0Bfolder", IndexPath: "/index.db"}
	if got != want {
		t.Errorf("provider config = %+v; want %+v", got, want)
	}
}

// Xattr passthrough is off unless asked for. Stated as its own test because it is
// a security default rather than a preference: the passthrough exposes M5's
// placeholder marker at the mountpoint, where anything that can write to the mount
// can strip it.
func TestXattrPassthroughIsOffByDefault(t *testing.T) {
	f := defaults()
	f.mountpoint = "/m"
	specs, err := mountSpecs(quietFlagSet(), map[string]bool{"mount": true}, f)
	if err != nil {
		t.Fatalf("mountSpecs: %v", err)
	}
	if specs[0].Xattr {
		t.Error("Xattr is on without -xattr")
	}
}

// No credentials means log-only (M1 behaviour): no provider, nothing to reach.
func TestNoCredentialsIsLogOnly(t *testing.T) {
	f := defaults()
	f.mountpoint = "/m"
	specs, err := mountSpecs(quietFlagSet(), map[string]bool{"mount": true}, f)
	if err != nil {
		t.Fatalf("mountSpecs: %v", err)
	}
	if specs[0].Provider != "" {
		t.Errorf("Provider = %q; want none", specs[0].Provider)
	}
	if specs[0].ProviderConfig != nil {
		t.Error("a log-only mount got a provider config")
	}
}

func TestMountSpecsErrors(t *testing.T) {
	t.Run("no mount and no config", func(t *testing.T) {
		// Point XDG at an empty dir so the default config file cannot be found.
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		_, err := mountSpecs(quietFlagSet(), map[string]bool{}, defaults())
		if err == nil || !strings.Contains(err.Error(), "-mount is required") {
			t.Fatalf("err = %v; want -mount required", err)
		}
	})

	t.Run("lazy without credentials", func(t *testing.T) {
		f := defaults()
		f.mountpoint, f.lazy = "/m", true
		_, err := mountSpecs(quietFlagSet(), map[string]bool{"mount": true}, f)
		if err == nil || !strings.Contains(err.Error(), "-lazy requires -credentials") {
			t.Fatalf("err = %v; want the -lazy guard", err)
		}
	})
}

// Mixing -config with a flag that describes what to mount would need a
// precedence rule nobody would remember, so it is refused by name.
func TestConfigRejectsMountShapingFlags(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("[[mount]]\npath = \"./m\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range mountShapingFlags {
		t.Run(name, func(t *testing.T) {
			f := defaults()
			f.configPath = path
			_, err := mountSpecs(quietFlagSet(), map[string]bool{"config": true, name: true}, f)
			if err == nil {
				t.Fatalf("-%s alongside -config was accepted", name)
			}
			if !strings.Contains(err.Error(), "-"+name) {
				t.Errorf("error should name the flag: %v", err)
			}
		})
	}
	// -debug describes how a mount reports, not what it is, so it composes.
	f := defaults()
	f.configPath, f.debug = path, true
	specs, err := mountSpecs(quietFlagSet(), map[string]bool{"config": true, "debug": true}, f)
	if err != nil {
		t.Fatalf("-debug alongside -config was rejected: %v", err)
	}
	if !specs[0].Debug {
		t.Error("-debug did not reach the spec from the config path")
	}
}

// With neither -mount nor -config, a config file at the default location is
// what `drivel mount` on its own should pick up.
func TestDefaultConfigIsFoundWhenNoFlagsGiven(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	path := filepath.Join(dir, config.AppName, "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "[[mount]]\nname = \"one\"\npath = \"./a\"\n\n[[mount]]\nname = \"two\"\npath = \"./b\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	specs, err := mountSpecs(quietFlagSet(), map[string]bool{}, defaults())
	if err != nil {
		t.Fatalf("mountSpecs: %v", err)
	}
	if len(specs) != 2 {
		t.Fatalf("got %d specs; want the 2 in the default config", len(specs))
	}
}

// An explicit -mount must not be overridden by a config file that happens to
// exist at the default path.
func TestExplicitMountBeatsDefaultConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	path := filepath.Join(dir, config.AppName, "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("[[mount]]\npath = \"./from-config\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := defaults()
	f.mountpoint = "/from-flags"
	specs, err := mountSpecs(quietFlagSet(), map[string]bool{"mount": true}, f)
	if err != nil {
		t.Fatalf("mountSpecs: %v", err)
	}
	if len(specs) != 1 || specs[0].Mountpoint != "/from-flags" {
		t.Errorf("default config overrode an explicit -mount: %+v", specs)
	}
}
