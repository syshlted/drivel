package config

import (
	"bytes"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/zishmusic/drivel/internal/app"
	"github.com/zishmusic/drivel/internal/hydrate"
	"github.com/zishmusic/drivel/internal/syncengine"
)

// Config is a parsed config file, not yet resolved into mounts.
type Config struct {
	// dir is the config file's own directory. Relative paths in the file resolve
	// against it, so a config directory is self-contained and can be moved or
	// checked into dotfiles without meaning something different.
	dir string

	accounts map[string]account
	mounts   []mountEntry
}

type account struct {
	provider string
	// settings is the rest of the account's table: the provider's own
	// configuration, kept opaque. This package never learns what a key means.
	settings map[string]any
}

type mountEntry struct {
	name           string
	accountName    string
	path           string
	data           string
	state          string
	lazy           bool
	xattr          bool
	debug          bool
	resync         bool
	materialize    bool
	maxDeletes     *int
	sweepInterval  *string
	uploadWorkers  *int
	hydrateWorkers *int
	provider       map[string]any
}

// The TOML shapes. They are separate from the resolved types above so that
// "what the file may say" and "what a mount ends up being" can differ — defaults,
// path expansion and the account/mount merge all happen in between.
type fileTOML struct {
	Accounts map[string]accountTOML `toml:"account"`
	Mounts   []mountTOML            `toml:"mount"`
}

type accountTOML struct {
	Provider string `toml:"provider"`
}

type mountTOML struct {
	Name          string  `toml:"name"`
	Account       string  `toml:"account"`
	Path          string  `toml:"path"`
	Data          string  `toml:"data"`
	State         string  `toml:"state"`
	Lazy          bool    `toml:"lazy"`
	Xattr         bool    `toml:"xattr"`
	Debug         bool    `toml:"debug"`
	Resync        bool    `toml:"resync"`
	Materialize   bool    `toml:"materialize"`
	MaxDeletes    *int    `toml:"max-deletes"`
	SweepInterval *string `toml:"sweep-interval"`
	// Pointers so that an explicit 0 is distinguishable from "not mentioned" and
	// can be refused. Neither number has a meaning at 0 — an upload pool of that
	// size never pushes and a fetch pool of it never hydrates — and in a file
	// where `max-deletes = 0` means "no limit", silently reading it as "use the
	// default" would be the M8 rule 6 failure: a setting that looks applied and
	// is not.
	UploadWorkers  *int           `toml:"upload-workers"`
	HydrateWorkers *int           `toml:"hydrate-workers"`
	Provider       map[string]any `toml:"provider"`
}

// Load parses the config file at path.
func Load(path string) (*Config, error) {
	var typed fileTOML
	md, err := toml.DecodeFile(path, &typed)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if err := rejectTypos(path, md); err != nil {
		return nil, err
	}

	// A second pass as plain tables, to pick up the free-form provider settings
	// an account carries alongside its `provider` key. Decoding twice is cheaper
	// than a custom UnmarshalTOML, and keeps the typed shape readable.
	var raw struct {
		Accounts map[string]map[string]any `toml:"account"`
	}
	if _, err := toml.DecodeFile(path, &raw); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	c := &Config{
		dir:      filepath.Dir(path),
		accounts: make(map[string]account, len(typed.Accounts)),
	}
	for name, a := range typed.Accounts {
		if err := ValidName(name); err != nil {
			return nil, fmt.Errorf("%s: account %q: %w", path, name, err)
		}
		if a.Provider == "" {
			return nil, fmt.Errorf("%s: account %q has no provider", path, name)
		}
		settings := make(map[string]any)
		for k, v := range raw.Accounts[name] {
			if k == "provider" {
				continue // the kind, not a setting
			}
			settings[k] = v
		}
		c.accounts[name] = account{provider: a.Provider, settings: settings}
	}
	for i, m := range typed.Mounts {
		c.mounts = append(c.mounts, mountEntry{
			name:           m.Name,
			accountName:    m.Account,
			path:           m.Path,
			data:           m.Data,
			state:          m.State,
			lazy:           m.Lazy,
			xattr:          m.Xattr,
			debug:          m.Debug,
			resync:         m.Resync,
			materialize:    m.Materialize,
			maxDeletes:     m.MaxDeletes,
			sweepInterval:  m.SweepInterval,
			uploadWorkers:  m.UploadWorkers,
			hydrateWorkers: m.HydrateWorkers,
			provider:       m.Provider,
		})
		if m.Path == "" {
			return nil, fmt.Errorf("%s: mount #%d has no path", path, i+1)
		}
	}
	if len(c.mounts) == 0 {
		return nil, fmt.Errorf("%s: no [[mount]] entries", path)
	}
	return c, nil
}

// rejectTypos fails on a key the schema does not define.
//
// Silently ignoring `lazzy = true` is the same failure mode as a flag that stops
// being read: the setting looks applied and is not, and nothing anywhere reports
// it. Free-form regions are exempt, because "unknown key" is their whole point —
// an account's provider settings, and a mount's [mount.provider] table.
func rejectTypos(path string, md toml.MetaData) error {
	var bad []string
	for _, key := range md.Undecoded() {
		k := []string(key)
		// account.NAME.SETTING — the provider's own config.
		if len(k) == 3 && k[0] == "account" {
			continue
		}
		bad = append(bad, key.String())
	}
	if len(bad) == 0 {
		return nil
	}
	sort.Strings(bad)
	return fmt.Errorf("%s: unknown %s: %s", path, plural(len(bad), "key", "keys"), strings.Join(bad, ", "))
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// Specs resolves the file into mount specifications, applying defaults and
// expanding paths. It does not validate them against each other; that is
// app.Validate, which runs on the flag path too.
func (c *Config) Specs() ([]app.MountSpec, error) {
	out := make([]app.MountSpec, 0, len(c.mounts))
	for i, m := range c.mounts {
		spec, err := c.spec(m)
		if err != nil {
			return nil, fmt.Errorf("mount #%d (%s): %w", i+1, m.describe(), err)
		}
		out = append(out, spec)
	}
	return out, nil
}

func (m mountEntry) describe() string {
	if m.name != "" {
		return m.name
	}
	return m.path
}

func (c *Config) spec(m mountEntry) (app.MountSpec, error) {
	var spec app.MountSpec

	name := m.name
	if name == "" {
		// The account name is the better default than the mountpoint's basename:
		// it is what the state directory is keyed on, and it is stable if the
		// mountpoint moves.
		name = m.accountName
	}
	if name == "" {
		name = filepath.Base(m.path)
	}
	if err := ValidName(name); err != nil {
		return spec, err
	}
	spec.Name = name

	path, err := expand(c.dir, m.path)
	if err != nil {
		return spec, err
	}
	spec.Mountpoint = path
	if spec.DataDir, err = expand(c.dir, m.data); err != nil {
		return spec, err
	}

	spec.Lazy = m.lazy
	spec.Xattr = m.xattr
	spec.Debug = m.debug
	spec.Resync = m.resync
	spec.Materialize = m.materialize

	spec.MaxDeletes = syncengine.DefaultMaxDeletes
	if m.maxDeletes != nil {
		// A pointer so that an explicit 0 ("no limit") is distinguishable from
		// "not mentioned"; conflating them would silently uncap the one guard
		// standing between a broken premise and a mass delete (M7b).
		spec.MaxDeletes = *m.maxDeletes
	}
	spec.SweepInterval = syncengine.DefaultSweepInterval
	if m.sweepInterval != nil {
		d, err := time.ParseDuration(*m.sweepInterval)
		if err != nil {
			return spec, fmt.Errorf("sweep-interval: %w", err)
		}
		spec.SweepInterval = d
	}

	spec.UploadWorkers = syncengine.DefaultWorkers
	if m.uploadWorkers != nil {
		if *m.uploadWorkers < 1 {
			return spec, fmt.Errorf("upload-workers: %d is not a pool size (1 or more; omit the key for the default of %d)",
				*m.uploadWorkers, syncengine.DefaultWorkers)
		}
		spec.UploadWorkers = *m.uploadWorkers
	}
	spec.HydrateWorkers = hydrate.DefaultWorkers
	if m.hydrateWorkers != nil {
		if *m.hydrateWorkers < 1 {
			return spec, fmt.Errorf("hydrate-workers: %d is not a pool size (1 or more; omit the key for the default of %d)",
				*m.hydrateWorkers, hydrate.DefaultWorkers)
		}
		spec.HydrateWorkers = *m.hydrateWorkers
	}

	// No account means a log-only mount: no provider, no state DB, no network.
	if m.accountName == "" {
		if m.lazy {
			return spec, fmt.Errorf("lazy needs an account (there is nothing to hydrate from)")
		}
		return spec, nil
	}
	acct, ok := c.accounts[m.accountName]
	if !ok {
		return spec, fmt.Errorf("no [account.%s] is defined%s", m.accountName, c.knownAccounts())
	}
	spec.Provider = acct.provider

	// The provider's configuration is the account's settings with the mount's own
	// [mount.provider] table laid over it: credentials belong to the account,
	// while what to mount from it — a Drive folder, a bucket prefix — belongs to
	// the mount.
	settings := make(map[string]any, len(acct.settings)+len(m.provider))
	for k, v := range acct.settings {
		settings[k] = v
	}
	for k, v := range m.provider {
		settings[k] = v
	}
	if err := c.expandSettings(settings); err != nil {
		return spec, err
	}
	spec.ProviderConfig = tomlDecoder(settings)

	state := m.state
	if state == "" {
		dir, err := StateDir(name)
		if err != nil {
			return spec, err
		}
		state = filepath.Join(dir, "state.db")
	} else if state, err = expand(c.dir, state); err != nil {
		return spec, err
	}
	spec.StateDB = state
	return spec, nil
}

func (c *Config) knownAccounts() string {
	if len(c.accounts) == 0 {
		return ""
	}
	names := make([]string, 0, len(c.accounts))
	for n := range c.accounts {
		names = append(names, n)
	}
	sort.Strings(names)
	return " (defined: " + strings.Join(names, ", ") + ")"
}

// expandSettings resolves path-like values in a provider table.
//
// This package cannot know which of a provider's keys name files — that is the
// provider's business, and asking would put a list of Drive's key names in here.
// So the rule is syntactic and documented instead: a value is treated as a path
// only when it is written like one, with a leading "~/", "./" or "../". Anything
// else is passed through untouched, which is what a Drive folder ID needs.
// `drivel login` writes absolute paths, so the common case never depends on it.
func (c *Config) expandSettings(settings map[string]any) error {
	for k, v := range settings {
		s, ok := v.(string)
		if !ok {
			continue
		}
		if !strings.HasPrefix(s, "~") && !strings.HasPrefix(s, "./") && !strings.HasPrefix(s, "../") {
			continue
		}
		e, err := expand(c.dir, s)
		if err != nil {
			return fmt.Errorf("%s: %w", k, err)
		}
		settings[k] = e
	}
	return nil
}

// tomlDecoder turns a settings table back into the provider.Factory decode
// contract. Re-encoding and decoding is what lets a provider keep one plain
// struct with toml tags — the alternative is a reflective map-to-struct walk
// here, which would be this package's own half-implementation of the decoder it
// already depends on.
func tomlDecoder(settings map[string]any) func(any) error {
	return func(dst any) error {
		var buf bytes.Buffer
		if err := toml.NewEncoder(&buf).Encode(settings); err != nil {
			return fmt.Errorf("re-encoding provider settings: %w", err)
		}
		md, err := toml.Decode(buf.String(), dst)
		if err != nil {
			return fmt.Errorf("provider settings: %w", err)
		}
		if u := md.Undecoded(); len(u) > 0 {
			keys := make([]string, 0, len(u))
			for _, k := range u {
				keys = append(keys, k.String())
			}
			sort.Strings(keys)
			return fmt.Errorf("provider settings: unknown %s: %s",
				plural(len(keys), "key", "keys"), strings.Join(keys, ", "))
		}
		return nil
	}
}
