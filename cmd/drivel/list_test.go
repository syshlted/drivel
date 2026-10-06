// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/syshlted/drivel/internal/app"
	"github.com/syshlted/drivel/internal/config"
)

// mixedConfig writes the shape -list exists for: two providers and a log-only
// mount in one file, where the provider a mount talks to is written once in an
// [account.*] table that the [[mount]] refers to by name and never repeats.
func mixedConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	body := `
[account.personal]
provider = "gdrive"
credentials = "/c.json"
token = "/t.json"

[account.nas]
provider = "sftp"
host = "nas.example.lan"
user = "jeremy"

[[mount]]
name = "docs"
account = "personal"
path = "./docs"
data = "./docs-data"
state = "./docs-state.db"
lazy = true

[[mount]]
name = "backups"
account = "nas"
path = "./nas"
data = "./nas-data"
state = "./nas-state.db"
sweep-interval = "5m"
max-deletes = 0

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

// The headline case, and the question the feature was asked for: which provider
// does each mount actually talk to. A config file answers it only by following a
// mount's account key into a table somewhere else, and never for several mounts
// at once.
func TestListReportsEachMountsProvider(t *testing.T) {
	path := mixedConfig(t)
	f := defaults()
	f.configPath = path

	ls, got, err := mountListing(map[string]bool{"config": true, "list": true}, f)
	if err != nil {
		t.Fatalf("mountListing: %v", err)
	}
	if got != path {
		t.Errorf("listed %s; want %s", got, path)
	}
	want := []struct{ name, account, kind string }{
		{"docs", "personal", "gdrive"},
		{"backups", "nas", "sftp"},
		{"scratch", "", ""}, // log-only: no account, so no provider
	}
	if len(ls) != len(want) {
		t.Fatalf("listed %d mounts; want %d", len(ls), len(want))
	}
	for i, w := range want {
		switch {
		case ls[i].Spec.Name != w.name:
			t.Errorf("mount %d is %q; want %q", i, ls[i].Spec.Name, w.name)
		case ls[i].Account != w.account:
			t.Errorf("%s draws on account %q; want %q", w.name, ls[i].Account, w.account)
		case ls[i].Spec.Provider != w.kind:
			t.Errorf("%s uses provider %q; want %q", w.name, ls[i].Spec.Provider, w.kind)
		}
	}
}

// A listing is resolved rather than echoed back: the defaults nobody typed and
// the paths the file wrote relative are what the mount will use, so they are
// what a reader has to be shown.
func TestListShowsResolvedValues(t *testing.T) {
	path := mixedConfig(t)
	dir := filepath.Dir(path)
	f := defaults()
	f.configPath = path
	f.selectName = "docs"

	ls, _, err := mountListing(map[string]bool{"config": true, "list": true, "name": true}, f)
	if err != nil {
		t.Fatalf("mountListing: %v", err)
	}
	if len(ls) != 1 {
		t.Fatalf("listed %d mounts; want 1", len(ls))
	}
	s := ls[0].Spec
	if want := filepath.Join(dir, "docs"); s.Mountpoint != want {
		t.Errorf("mountpoint %s; want %s (relative to the config file's own directory)", s.Mountpoint, want)
	}
	if s.MaxDeletes == 0 {
		t.Error("max-deletes resolved to 0; the file does not set it, so the listing must show the default")
	}
	if !s.Lazy {
		t.Error("lazy did not reach the listing")
	}
}

// -list and SpecsFor must not be two readings of one file. They share
// config.List for exactly that reason, and this is what says so.
func TestListingAgreesWithWhatWouldBeMounted(t *testing.T) {
	path := mixedConfig(t)
	f := defaults()
	f.configPath = path

	ls, _, err := mountListing(map[string]bool{"config": true, "list": true}, f)
	if err != nil {
		t.Fatalf("mountListing: %v", err)
	}
	specs, err := mountSpecs(quietFlagSet(), map[string]bool{"config": true}, f)
	if err != nil {
		t.Fatalf("mountSpecs: %v", err)
	}
	if len(ls) != len(specs) {
		t.Fatalf("listed %d mounts but would mount %d", len(ls), len(specs))
	}
	for i := range specs {
		if ls[i].Spec.Name != specs[i].Name || ls[i].Spec.Mountpoint != specs[i].Mountpoint ||
			ls[i].Spec.Provider != specs[i].Provider || ls[i].Spec.StateDB != specs[i].StateDB {
			t.Errorf("mount %d: listing says %+v, mounting would use %+v", i, ls[i].Spec, specs[i])
		}
	}
}

// The selectors narrow a listing exactly as they narrow a mount, which is what
// makes `-list -account nas` the way to check what `-account nas` would serve.
func TestListComposesWithTheSelectors(t *testing.T) {
	path := mixedConfig(t)
	for _, tc := range []struct {
		flag, value string
		want        string
	}{
		{"name", "backups", "backups"},
		{"account", "personal", "docs"},
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
			ls, _, err := mountListing(map[string]bool{"config": true, "list": true, tc.flag: true}, f)
			if err != nil {
				t.Fatalf("mountListing: %v", err)
			}
			if len(ls) != 1 || ls[0].Spec.Name != tc.want {
				t.Errorf("got %d mounts (first %q); want just %q", len(ls), ls[0].Spec.Name, tc.want)
			}
		})
	}
}

// A selector that matches nothing is an error here for the same reason it is one
// when mounting: printing an empty table would answer "which mounts are there?"
// with "none", which is false.
func TestListWithASelectorThatMatchesNothingIsRefused(t *testing.T) {
	f := defaults()
	f.configPath = mixedConfig(t)
	f.selectName = "nope"

	_, _, err := mountListing(map[string]bool{"config": true, "list": true, "name": true}, f)
	if err == nil {
		t.Fatal("-list -name nope printed something")
	}
	for _, want := range []string{"nope", "docs", "backups", "scratch"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// -list reads a config file, so like a selector it needs one, and like a
// selector it must not quietly print nothing when there is none.
func TestListWithoutAConfigIsRefused(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	t.Run("no config at all", func(t *testing.T) {
		_, _, err := mountListing(map[string]bool{"list": true}, defaults())
		if err == nil {
			t.Fatal("-list with no config file was accepted")
		}
		if !strings.Contains(err.Error(), "-list") {
			t.Errorf("error %q does not name -list", err)
		}
	})

	t.Run("with -mount", func(t *testing.T) {
		f := defaults()
		f.mountpoint = "/m"
		_, _, err := mountListing(map[string]bool{"list": true, "mount": true}, f)
		if err == nil {
			t.Fatal("-list -mount was accepted")
		}
		if !strings.Contains(err.Error(), "-mount") {
			t.Errorf("error %q does not name -mount", err)
		}
	})
}

// A flag describing a mount cannot be combined with -list, for the reason it
// cannot be combined with -config: the listing could not show that mount, and
// nothing would have said so.
func TestListRefusesShapingFlags(t *testing.T) {
	path := mixedConfig(t)
	for _, name := range mountShapingFlags {
		if name == "mount" {
			continue // covered above, where it fails earlier for having no config
		}
		f := defaults()
		f.configPath = path
		_, _, err := mountListing(map[string]bool{"config": true, "list": true, name: true}, f)
		if err == nil {
			t.Errorf("-%s alongside -list was accepted", name)
			continue
		}
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error %q does not name -%s", err, name)
		}
	}
}

// Both halves of checkListingFlags. Each refuses a flag that would otherwise be
// discarded in silence, which is the failure an unknown config key is an error
// for (M8 rule 6).
func TestListingFlagsThatWouldBeInertAreRefused(t *testing.T) {
	t.Run("-full without -list", func(t *testing.T) {
		err := checkListingFlags(map[string]bool{"full": true})
		if err == nil {
			t.Fatal("-full on its own was accepted")
		}
		if !strings.Contains(err.Error(), "-list") {
			t.Errorf("error %q does not say what -full needs", err)
		}
	})
	for _, name := range mountProcessFlags {
		t.Run("-"+name+" with -list", func(t *testing.T) {
			err := checkListingFlags(map[string]bool{"list": true, name: true})
			if err == nil {
				t.Fatalf("-%s alongside -list was accepted", name)
			}
			if !strings.Contains(err.Error(), name) {
				t.Errorf("error %q does not name -%s", err, name)
			}
		})
	}
	if err := checkListingFlags(map[string]bool{"list": true, "full": true, "config": true}); err != nil {
		t.Errorf("-list -full -config was refused: %v", err)
	}
}

// The three flag categories must stay disjoint: each decides a different rule
// about combining with -config, so a flag in two of them has two answers.
func TestFlagCategoriesAreDisjoint(t *testing.T) {
	categories := map[string][]string{
		"shaping":  mountShapingFlags,
		"selector": mountSelectorFlags,
		"listing":  mountListingFlags,
		"process":  mountProcessFlags,
	}
	seen := map[string]string{}
	for category, flags := range categories {
		for _, name := range flags {
			if other, dup := seen[name]; dup {
				t.Errorf("-%s is in both %s and %s", name, other, category)
			}
			seen[name] = category
		}
	}

	// And every mount flag belongs to one of them, so a flag added without a
	// category cannot quietly inherit whatever the fall-through does.
	fset := mountFlagSet(&mountCLI{})
	fset.VisitAll(func(fl *flag.Flag) {
		if fl.Name == "config" {
			return // the file itself, which the categories are defined against
		}
		if _, ok := seen[fl.Name]; !ok {
			t.Errorf("-%s is in no flag category", fl.Name)
		}
	})
}

// The table. Short is what you need to pick a -name; -full adds the five that
// are decisions rather than tuning.
func TestPrintMountListColumns(t *testing.T) {
	ls := []config.Listing{{
		Account: "personal",
		Spec: app.MountSpec{
			Name: "docs", Mountpoint: "/home/u/drive", DataDir: "/home/u/.local/share/drivel/docs",
			StateDB: "/home/u/.local/state/drivel/docs/state.db", Provider: "gdrive",
			Lazy: true, SweepInterval: 5 * time.Minute, MaxDeletes: 100,
		},
	}}

	var short strings.Builder
	if err := printMountList(&short, ls, false); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"NAME", "ACCOUNT", "PROVIDER", "MOUNTPOINT", "docs", "personal", "gdrive", "/home/u/drive"} {
		if !strings.Contains(short.String(), want) {
			t.Errorf("short listing lacks %q:\n%s", want, short.String())
		}
	}
	for _, absent := range []string{"STATE", "MODE", "SWEEP", "MAX-DELETES", "state.db"} {
		if strings.Contains(short.String(), absent) {
			t.Errorf("short listing contains %q, which belongs to -full:\n%s", absent, short.String())
		}
	}

	var full strings.Builder
	if err := printMountList(&full, ls, true); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"DATA", "STATE", "MODE", "SWEEP", "MAX-DELETES", "state.db", "lazy", "5m0s", "100"} {
		if !strings.Contains(full.String(), want) {
			t.Errorf("full listing lacks %q:\n%s", want, full.String())
		}
	}
	if lines := strings.Count(strings.TrimSpace(full.String()), "\n"); lines != 1 {
		t.Errorf("full listing is %d lines after the header; want 1", lines)
	}
}

// What a cell says when the value is absent rather than small. Each of these
// would otherwise read as a setting the mount does not have.
func TestPrintMountListSpellsOutTheAbsentValues(t *testing.T) {
	// A log-only mount: no account, no provider, no state DB — and the two
	// reconcile settings it still carries never apply, so they must not be shown
	// as though they did.
	logOnly := config.Listing{Spec: app.MountSpec{
		Name: "scratch", Mountpoint: "/home/u/scratch", DataDir: "/home/u/scratch-data",
		SweepInterval: 24 * time.Hour, MaxDeletes: 100,
	}}
	row := mountRow(logOnly, true)
	want := []string{"scratch", "-", "-", "/home/u/scratch", "/home/u/scratch-data", "-", "log-only", "-", "-"}
	if len(row) != len(want) {
		t.Fatalf("row has %d cells; want %d", len(row), len(want))
	}
	for i := range want {
		if row[i] != want[i] {
			t.Errorf("%s: cell %d is %q; want %q", fullColumns[i], i, row[i], want[i])
		}
	}

	// In-place mode is a mode, not a missing backing directory.
	inPlace := config.Listing{Account: "a", Spec: app.MountSpec{
		Name: "inplace", Mountpoint: "/home/u/d", Provider: "gdrive",
		StateDB: "/s.db", SweepInterval: 0, MaxDeletes: 0,
	}}
	row = mountRow(inPlace, true)
	for i, want := range map[int]string{4: "(in place)", 6: "eager", 7: "off", 8: "unlimited"} {
		if row[i] != want {
			t.Errorf("%s: cell %d is %q; want %q", fullColumns[i], i, row[i], want)
		}
	}
}
