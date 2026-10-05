// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/syshlted/drivel/internal/config"
	"github.com/syshlted/drivel/internal/hydrate"
	"github.com/syshlted/drivel/internal/provider/gdrive"
	"github.com/syshlted/drivel/internal/syncengine"
)

// quietFlagSet stands in for the real one; mountSpecs only ever calls Usage.
func quietFlagSet() *flag.FlagSet {
	fset := flag.NewFlagSet("mount", flag.ContinueOnError)
	// io.Discard, not os.NewFile(0, ...): that wrapped *stdin* (fd 0, not
	// /dev/null) in a new *os.File whose finalizer closes the fd. Each call
	// made another owner of fd 0, so after the first finalizer freed it the next
	// socket in the test binary took fd 0 and a later finalizer closed that
	// instead — a pprof listener failing accept(2) with EBADF, at random.
	fset.SetOutput(io.Discard)
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
		pushDelay:     syncengine.DefaultPushDelay,

		uploadWorkers:  syncengine.DefaultWorkers,
		hydrateWorkers: hydrate.DefaultWorkers,
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
	f.driveDelete = "permanent"
	f.lazy = true
	f.xattr = true
	f.debug = true
	f.resync = true
	f.materialize = true
	f.maxDeletes = 7
	f.sweepInterval = 90 * time.Minute
	f.pushDelay = 12 * time.Second
	f.uploadWorkers = 11
	f.hydrateWorkers = 13

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
	if s.PushDelay != 12*time.Second {
		t.Errorf("PushDelay = %v; want 12s", s.PushDelay)
	}
	// Distinct values on purpose: one number reaching both fields would satisfy a
	// test that used the same one, and the whole point is that the two directions
	// are tuned separately.
	if s.UploadWorkers != 11 || s.HydrateWorkers != 13 {
		t.Errorf("workers = up %d / hydrate %d; want 11 / 13", s.UploadWorkers, s.HydrateWorkers)
	}
	if s.Provider != driveKind {
		t.Errorf("Provider = %q; want %q", s.Provider, driveKind)
	}

	// The Drive-shaped flags have to arrive as Drive's own config.
	var got gdrive.Config
	if err := s.ProviderConfig.Decode(&got); err != nil {
		t.Fatalf("decoding provider config: %v", err)
	}
	want := gdrive.Config{
		Credentials: "/creds.json", Token: "/tok.json", RootID: "0Bfolder",
		IndexPath: "/index.db", Delete: gdrive.DeletePermanent,
	}
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

// Zero is refused rather than quietly meaning "the default". A pool of that size
// transfers nothing, and -max-deletes 0 means "no limit" in the same flag set, so
// a user has every reason to read 0 as a request rather than as an omission.
func TestWorkerPoolOfZeroIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*specFlags)
		want string
	}{
		{"upload zero", func(f *specFlags) { f.uploadWorkers = 0 }, "-upload-workers"},
		{"upload negative", func(f *specFlags) { f.uploadWorkers = -1 }, "-upload-workers"},
		{"hydrate zero", func(f *specFlags) { f.hydrateWorkers = 0 }, "-hydrate-workers"},
		{"hydrate negative", func(f *specFlags) { f.hydrateWorkers = -2 }, "-hydrate-workers"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := defaults()
			f.mountpoint = "/m"
			tc.set(&f)
			_, err := mountSpecs(quietFlagSet(), map[string]bool{"mount": true}, f)
			if err == nil {
				t.Fatalf("%s was accepted", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %s", err, tc.want)
			}
		})
	}
}

// A push delay of zero is refused for the same reason a pool of zero is: it is
// not a window, New would substitute the default behind the user's back, and
// -max-deletes 0 in the same flag set means "no limit" rather than "unset".
func TestPushDelayOfZeroIsRefused(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Second} {
		f := defaults()
		f.mountpoint, f.pushDelay = "/m", d
		_, err := mountSpecs(quietFlagSet(), map[string]bool{"mount": true}, f)
		if err == nil {
			t.Fatalf("-push-delay %s was accepted", d)
		}
		if !strings.Contains(err.Error(), "-push-delay") {
			t.Errorf("error %q does not name -push-delay", err)
		}
	}
}

// Both knobs describe a mount, so both belong in the config file — which makes
// combining them with -config an error, like every other shaping flag. Left out
// of the list, -config -upload-workers 8 would silently ignore the flag.
func TestWorkerFlagsAreShapingFlags(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("[[mount]]\npath = \"/m\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"upload-workers", "hydrate-workers"} {
		f := defaults()
		f.configPath = cfgPath
		_, err := mountSpecs(quietFlagSet(), map[string]bool{"config": true, name: true}, f)
		if err == nil {
			t.Errorf("-%s alongside -config was accepted", name)
			continue
		}
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error %q does not name -%s", err, name)
		}
	}
}

// selectorConfig writes a config file with three mounts over two accounts, the
// shape where -name and -account ask different questions.
func selectorConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	body := `
[account.personal]
provider = "gdrive"
credentials = "/c.json"
token = "/t.json"

[[mount]]
name = "docs"
account = "personal"
path = "./docs"
data = "./docs-data"

[[mount]]
name = "photos"
account = "personal"
path = "./photos"
data = "./photos-data"

[[mount]]
name = "scratch"
path = "./scratch"
data = "./scratch-data"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The headline case: one mount out of a config file that describes several.
func TestSelectorsNarrowTheConfig(t *testing.T) {
	path := selectorConfig(t)
	for _, tc := range []struct {
		flag  string
		value string
		want  []string
	}{
		{"name", "photos", []string{"photos"}},
		{"name", "scratch", []string{"scratch"}},
		{"account", "personal", []string{"docs", "photos"}},
	} {
		t.Run(tc.flag+"="+tc.value, func(t *testing.T) {
			f := defaults()
			f.configPath = path
			switch tc.flag {
			case "name":
				f.selectName = tc.value
			case "account":
				f.selectAccount = tc.value
			}
			specs, err := mountSpecs(quietFlagSet(), map[string]bool{"config": true, tc.flag: true}, f)
			if err != nil {
				t.Fatalf("mountSpecs: %v", err)
			}
			var got []string
			for _, s := range specs {
				got = append(got, s.Name)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("got %v; want %v", got, tc.want)
			}
		})
	}
}

// A selector is not a shaping flag: it chooses among mounts the file already
// describes, so unlike every flag in mountShapingFlags it composes with -config.
func TestSelectorsAreNotShapingFlags(t *testing.T) {
	for _, name := range mountSelectorFlags {
		for _, shaping := range mountShapingFlags {
			if name == shaping {
				t.Errorf("-%s is in both mountSelectorFlags and mountShapingFlags", name)
			}
		}
	}
}

// With no config file there is nothing to select from, and the flags describing
// a mount must not quietly serve that mount while ignoring the -name.
func TestSelectorWithoutAConfigIsRefused(t *testing.T) {
	// An empty XDG_CONFIG_HOME, so the default path exists as a path and holds no
	// file: that is the case where the error can name where it looked.
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	t.Run("no config at all", func(t *testing.T) {
		f := defaults()
		f.selectName = "photos"
		_, err := mountSpecs(quietFlagSet(), map[string]bool{"name": true}, f)
		if err == nil {
			t.Fatal("-name with no config file was accepted")
		}
		if !strings.Contains(err.Error(), "-name") || !strings.Contains(err.Error(), "config.toml") {
			t.Errorf("error should name the flag and the path it looked at: %v", err)
		}
	})

	t.Run("alongside -mount", func(t *testing.T) {
		f := defaults()
		f.mountpoint = "/m"
		f.selectName = "photos"
		_, err := mountSpecs(quietFlagSet(), map[string]bool{"name": true, "mount": true}, f)
		if err == nil {
			t.Fatal("-name alongside -mount was accepted")
		}
		if !strings.Contains(err.Error(), "-mount") {
			t.Errorf("error should name -mount: %v", err)
		}
	})
}

// -name already identifies one mount, so naming its account too can only agree
// or contradict. Refused rather than resolved.
func TestSelectorsCannotBeCombined(t *testing.T) {
	f := defaults()
	f.configPath = selectorConfig(t)
	f.selectName, f.selectAccount = "photos", "personal"
	_, err := mountSpecs(quietFlagSet(), map[string]bool{"config": true, "name": true, "account": true}, f)
	if err == nil {
		t.Fatal("-name and -account together were accepted")
	}
}

// An explicit empty value is refused for the reason an explicit 0 is refused for
// the pool sizes: it would otherwise read as "no selector" and serve every mount
// in the file, which is the opposite of what typing the flag asks for.
func TestSelectorWithAnEmptyValueIsRefused(t *testing.T) {
	path := selectorConfig(t)
	for _, name := range mountSelectorFlags {
		t.Run(name, func(t *testing.T) {
			f := defaults()
			f.configPath = path
			_, err := mountSpecs(quietFlagSet(), map[string]bool{"config": true, name: true}, f)
			if err == nil {
				t.Fatalf("-%s with an empty value was accepted", name)
			}
			if !strings.Contains(err.Error(), "-"+name) {
				t.Errorf("error should name the flag: %v", err)
			}
		})
	}
}

// A selector works off the default config path too, which is what makes
// `drivel mount -name photos` the whole invocation.
func TestSelectorUsesTheDefaultConfig(t *testing.T) {
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
	f := defaults()
	f.selectName = "two"
	specs, err := mountSpecs(quietFlagSet(), map[string]bool{"name": true}, f)
	if err != nil {
		t.Fatalf("mountSpecs: %v", err)
	}
	if len(specs) != 1 || specs[0].Name != "two" {
		t.Fatalf("got %v; want just two", specs)
	}
}

// A mistyped selector must fail at startup naming what the config does define,
// not serve nothing and exit successfully.
func TestSelectorThatMatchesNothingFailsAtStartup(t *testing.T) {
	f := defaults()
	f.configPath = selectorConfig(t)
	f.selectName = "photoss"
	_, err := mountSpecs(quietFlagSet(), map[string]bool{"config": true, "name": true}, f)
	if err == nil {
		t.Fatal("a -name matching no mount was accepted")
	}
	if !strings.Contains(err.Error(), "photos") {
		t.Errorf("error should list the names the config defines: %v", err)
	}
}
