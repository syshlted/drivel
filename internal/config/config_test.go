package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zishmusic/drivel/internal/hydrate"
	"github.com/zishmusic/drivel/internal/syncengine"
)

// write puts a config file in a fresh directory and points XDG at it, so the
// defaults a test observes are the ones the code derives rather than the
// developer's own home directory.
func write(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	path := filepath.Join(dir, AppName, "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func load(t *testing.T, body string) *Config {
	t.Helper()
	c, err := Load(write(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return c
}

// driveCfg mirrors the shape gdrive.Config decodes into, without importing it —
// internal/config must not depend on a provider, and a test that did would hide
// a leak rather than catch it.
type driveCfg struct {
	Credentials string `toml:"credentials"`
	Token       string `toml:"token"`
	RootID      string `toml:"root"`
	IndexPath   string `toml:"index"`
}

const twoAccounts = `
[account.personal]
provider    = "gdrive"
credentials = "./personal/credentials.json"
token       = "./personal/token.json"

[account.work]
provider    = "gdrive"
credentials = "./work/credentials.json"
token       = "./work/token.json"

[[mount]]
account = "personal"
path    = "./mnt/personal"
data    = "./data/personal"
lazy    = true
xattr   = true

[mount.provider]
root = "0BpersonalFolder"

[[mount]]
name        = "work-docs"
account     = "work"
path        = "./mnt/work"
data        = "./data/work"
max-deletes = 0

[mount.provider]
root = "0BworkFolder"
`

func TestSpecsResolvesTwoAccounts(t *testing.T) {
	c := load(t, twoAccounts)
	specs, err := c.Specs()
	if err != nil {
		t.Fatalf("Specs: %v", err)
	}
	if len(specs) != 2 {
		t.Fatalf("got %d specs; want 2", len(specs))
	}

	// The name defaults to the account, and is what the state dir is keyed on.
	if specs[0].Name != "personal" {
		t.Errorf("name = %q; want personal", specs[0].Name)
	}
	if specs[1].Name != "work-docs" {
		t.Errorf("explicit name lost: %q", specs[1].Name)
	}
	if !specs[0].Lazy {
		t.Error("lazy not carried through")
	}
	// Xattr passthrough is a security default, so both directions are asserted:
	// the mount that asked for it gets it, and the one that did not stays off.
	if !specs[0].Xattr {
		t.Error("xattr not carried through")
	}
	if specs[1].Xattr {
		t.Error("xattr is on for a mount that never mentioned it")
	}
	if specs[0].Provider != "gdrive" {
		t.Errorf("provider = %q", specs[0].Provider)
	}

	// Separate state DBs by default. Sharing one would let two accounts read each
	// other's echo records as their own baseline.
	if specs[0].StateDB == specs[1].StateDB {
		t.Fatalf("both mounts default to the same state DB: %s", specs[0].StateDB)
	}

	// The provider config is the account's settings with the mount's laid over.
	var a, b driveCfg
	if err := specs[0].ProviderConfig.Decode(&a); err != nil {
		t.Fatalf("decoding provider config: %v", err)
	}
	if err := specs[1].ProviderConfig.Decode(&b); err != nil {
		t.Fatalf("decoding provider config: %v", err)
	}
	if a.RootID != "0BpersonalFolder" || b.RootID != "0BworkFolder" {
		t.Errorf("roots not per-mount: %q / %q", a.RootID, b.RootID)
	}
	if a.Credentials == b.Credentials {
		t.Errorf("both mounts got the same credentials: %s", a.Credentials)
	}
	// "./" paths resolve against the config file's directory, not the process's.
	if !filepath.IsAbs(a.Credentials) {
		t.Errorf("credentials not expanded: %q", a.Credentials)
	}
	if !strings.HasSuffix(a.Credentials, filepath.Join(AppName, "personal", "credentials.json")) {
		t.Errorf("credentials resolved oddly: %q", a.Credentials)
	}
}

// A mount-level provider key must win over the account's, so one account can be
// mounted twice with different settings.
func TestProviderSettingsMountOverridesAccount(t *testing.T) {
	c := load(t, `
[account.a]
provider = "gdrive"
root     = "account-root"
token    = "./t.json"

[[mount]]
account = "a"
path    = "./mnt"
[mount.provider]
root = "mount-root"
`)
	specs, err := c.Specs()
	if err != nil {
		t.Fatalf("Specs: %v", err)
	}
	var got driveCfg
	if err := specs[0].ProviderConfig.Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.RootID != "mount-root" {
		t.Errorf("root = %q; want the mount's value", got.RootID)
	}
	if got.Token == "" {
		t.Error("account settings not inherited")
	}
}

func TestMaxDeletesZeroIsNotUnset(t *testing.T) {
	c := load(t, twoAccounts)
	specs, _ := c.Specs()
	if specs[0].MaxDeletes != syncengine.DefaultMaxDeletes {
		t.Errorf("unset max-deletes = %d; want the default %d", specs[0].MaxDeletes, syncengine.DefaultMaxDeletes)
	}
	// An explicit 0 means "no limit" and must survive; conflating it with unset
	// would silently re-cap, or silently uncap, the one guard between a broken
	// premise and a mass delete (M7b).
	if specs[1].MaxDeletes != 0 {
		t.Errorf("explicit max-deletes=0 became %d", specs[1].MaxDeletes)
	}
}

func TestSweepInterval(t *testing.T) {
	c := load(t, `
[[mount]]
path = "./a"
[[mount]]
path           = "./b"
sweep-interval = "90m"
[[mount]]
path           = "./c"
sweep-interval = "0s"
`)
	specs, err := c.Specs()
	if err != nil {
		t.Fatalf("Specs: %v", err)
	}
	if specs[0].SweepInterval != syncengine.DefaultSweepInterval {
		t.Errorf("unset = %v; want default", specs[0].SweepInterval)
	}
	if specs[1].SweepInterval != 90*time.Minute {
		t.Errorf("parsed = %v; want 90m", specs[1].SweepInterval)
	}
	if specs[2].SweepInterval != 0 {
		t.Errorf("explicit 0 became %v", specs[2].SweepInterval)
	}
}

// A key the schema does not define is a typo, and a typo that changes nothing is
// the same failure as a flag that stops being read.
func TestLoadRejectsUnknownKeys(t *testing.T) {
	_, err := Load(write(t, `
[[mount]]
path  = "./a"
lazzy = true
`))
	if err == nil {
		t.Fatal("unknown mount key accepted")
	}
	if !strings.Contains(err.Error(), "lazzy") {
		t.Errorf("error should name the key: %v", err)
	}
}

// An account's table is free-form below its `provider` key: those are the
// provider's settings, and this package never learns what they mean.
func TestLoadAllowsFreeFormAccountSettings(t *testing.T) {
	if _, err := Load(write(t, `
[account.a]
provider     = "gdrive"
credentials  = "./c.json"
anything-new = "tolerated"

[[mount]]
account = "a"
path    = "./mnt"
`)); err != nil {
		t.Fatalf("free-form account settings rejected: %v", err)
	}
}

// ...but a key the *provider* does not understand is caught when it decodes.
func TestProviderConfigRejectsUnknownKeys(t *testing.T) {
	c := load(t, `
[account.a]
provider = "gdrive"
nonsense = 1

[[mount]]
account = "a"
path    = "./mnt"
`)
	specs, err := c.Specs()
	if err != nil {
		t.Fatalf("Specs: %v", err)
	}
	var got driveCfg
	if err := specs[0].ProviderConfig.Decode(&got); err == nil {
		t.Fatal("provider accepted a key it does not define")
	} else if !strings.Contains(err.Error(), "nonsense") {
		t.Errorf("error should name the key: %v", err)
	}
}

func TestLoadErrors(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"no mounts", "[account.a]\nprovider = \"gdrive\"\n", "no [[mount]]"},
		{"mount without a path", "[[mount]]\nlazy = true\n", "no path"},
		{"account without a provider", "[account.a]\ncredentials = \"./c\"\n\n[[mount]]\npath = \"./m\"\n", "no provider"},
		{"account name with a separator", "[account.\"a/b\"]\nprovider = \"gdrive\"\n\n[[mount]]\npath = \"./m\"\n", "path separator"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Load(write(t, tc.body)); err == nil {
				t.Fatalf("%s accepted", tc.name)
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q should mention %q", err, tc.want)
			}
		})
	}
}

func TestSpecsErrors(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{
			"unknown account",
			"[account.real]\nprovider = \"gdrive\"\n\n[[mount]]\naccount = \"typo\"\npath = \"./m\"\n",
			"no [account.typo]",
		},
		{
			// M5: a placeholder is a promise to fetch bytes later.
			"lazy without an account",
			"[[mount]]\npath = \"./m\"\nlazy = true\n",
			"nothing to hydrate from",
		},
		{
			"unparseable sweep interval",
			"[[mount]]\npath = \"./m\"\nsweep-interval = \"soon\"\n",
			"sweep-interval",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Load(write(t, tc.body))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if _, err := c.Specs(); err == nil {
				t.Fatalf("%s accepted", tc.name)
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q should mention %q", err, tc.want)
			}
		})
	}
}

// A mount with no account is log-only: no provider, and therefore no state DB
// to open or network to reach.
func TestLogOnlyMountNeedsNoAccount(t *testing.T) {
	c := load(t, "[[mount]]\nname = \"local\"\npath = \"./m\"\n")
	specs, err := c.Specs()
	if err != nil {
		t.Fatalf("Specs: %v", err)
	}
	if specs[0].Provider != "" {
		t.Errorf("provider = %q; want none", specs[0].Provider)
	}
	if specs[0].StateDB != "" {
		t.Errorf("state DB = %q; want none", specs[0].StateDB)
	}
}

// Only values written like paths are expanded. A Drive folder ID is a string
// too, and rewriting it against the config directory would be silent corruption.
func TestProviderSettingPathExpansion(t *testing.T) {
	c := load(t, `
[account.a]
provider = "gdrive"
token    = "./tok.json"
root     = "0B_not_a_path"

[[mount]]
account = "a"
path    = "./m"
[mount.provider]
index = "~/idx.db"
`)
	specs, _ := c.Specs()
	var got driveCfg
	if err := specs[0].ProviderConfig.Decode(&got); err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(got.Token) {
		t.Errorf("./ path not expanded: %q", got.Token)
	}
	if got.RootID != "0B_not_a_path" {
		t.Errorf("a non-path value was rewritten: %q", got.RootID)
	}
	home, err := os.UserHomeDir()
	if err == nil && !strings.HasPrefix(got.IndexPath, home) {
		t.Errorf("~ not expanded: %q", got.IndexPath)
	}
}

func TestValidName(t *testing.T) {
	for _, bad := range []string{"", ".", "..", "a/b", `a\b`, "-x"} {
		if err := ValidName(bad); err == nil {
			t.Errorf("ValidName(%q) accepted", bad)
		}
	}
	for _, good := range []string{"personal", "work-docs", "a.b", "x_1"} {
		if err := ValidName(good); err != nil {
			t.Errorf("ValidName(%q) = %v", good, err)
		}
	}
}

func TestDefaultPathsFollowXDG(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/xdg/config")
	t.Setenv("XDG_STATE_HOME", "/xdg/state")

	p, err := DefaultPath()
	if err != nil || p != filepath.Join("/xdg/config", AppName, "config.toml") {
		t.Errorf("DefaultPath() = %q, %v", p, err)
	}
	a, err := AccountDir("personal")
	if err != nil || a != filepath.Join("/xdg/config", AppName, "personal") {
		t.Errorf("AccountDir() = %q, %v", a, err)
	}
	s, err := StateDir("personal")
	if err != nil || s != filepath.Join("/xdg/state", AppName, "personal") {
		t.Errorf("StateDir() = %q, %v", s, err)
	}
	// A name that escapes its directory must never reach a path join.
	if _, err := StateDir("../elsewhere"); err == nil {
		t.Error("StateDir accepted a traversing name")
	}
}

// Both pool sizes reach the spec, default independently, and are refused at zero.
// Separate keys because the directions are tuned separately: one number carried
// into both fields would pass a test that used the same value for each.
func TestWorkerPools(t *testing.T) {
	c := load(t, `
[[mount]]
path = "./defaults"
[[mount]]
path            = "./tuned"
upload-workers  = 11
hydrate-workers = 13
`)
	specs, err := c.Specs()
	if err != nil {
		t.Fatalf("Specs: %v", err)
	}
	if specs[0].UploadWorkers != syncengine.DefaultWorkers {
		t.Errorf("unset upload-workers = %d; want the default %d", specs[0].UploadWorkers, syncengine.DefaultWorkers)
	}
	if specs[0].HydrateWorkers != hydrate.DefaultWorkers {
		t.Errorf("unset hydrate-workers = %d; want the default %d", specs[0].HydrateWorkers, hydrate.DefaultWorkers)
	}
	if specs[1].UploadWorkers != 11 || specs[1].HydrateWorkers != 13 {
		t.Errorf("workers = up %d / hydrate %d; want 11 / 13", specs[1].UploadWorkers, specs[1].HydrateWorkers)
	}
}

// Zero is a request, not an omission — `max-deletes = 0` means "no limit" three
// lines up in the same file, so reading `upload-workers = 0` as "use the default"
// is the M8 rule 6 failure: a setting that looks applied and is not.
func TestWorkerPoolOfZeroIsRefused(t *testing.T) {
	for _, key := range []string{"upload-workers", "hydrate-workers"} {
		for _, val := range []string{"0", "-1"} {
			c := load(t, "[[mount]]\npath = \"./a\"\n"+key+" = "+val+"\n")
			_, err := c.Specs()
			if err == nil {
				t.Errorf("%s = %s was accepted", key, val)
				continue
			}
			if !strings.Contains(err.Error(), key) {
				t.Errorf("error %q does not name %s", err, key)
			}
		}
	}
}
