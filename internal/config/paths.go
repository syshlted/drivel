// Package config reads drivel's TOML configuration file and resolves it into the
// mount specifications internal/app runs.
//
// It exists because the flag surface stops scaling at one mount (DESIGN.md §9,
// M8): a second account needs a second everything — credentials, token, state DB,
// index, backing dir — and putting that on a command line is unreadable long
// before it is wrong.
//
// The package knows nothing about any provider. An account names a provider
// *kind* and carries that provider's settings as an opaque table, which is
// forwarded undecoded to the registry. Adding a provider does not touch this
// package.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// AppName is the per-user directory drivel uses under the XDG base directories.
const AppName = "drivel"

// DefaultPath is where the config file lives when none is named:
// $XDG_CONFIG_HOME/drivel/config.toml, falling back to ~/.config/drivel.
//
// Its absence is not an error anywhere — a bare `drivel mount -mount ./mnt` never
// reads it. The config file is what you graduate to when one mount stops being
// enough, not a prerequisite for the first one.
func DefaultPath() (string, error) {
	dir, err := configHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.toml"), nil
}

// AccountDir is where `drivel login -account NAME` writes that account's
// credentials and token: $XDG_CONFIG_HOME/drivel/NAME.
//
// Per account rather than one global token.json, because two accounts sharing a
// token file is not a degraded experience — it is one account silently
// overwriting the other's credentials at the next login.
func AccountDir(name string) (string, error) {
	if err := ValidName(name); err != nil {
		return "", err
	}
	dir, err := configHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

// StateDir is where a mount's control-plane databases live:
// $XDG_STATE_HOME/drivel/NAME, falling back to ~/.local/state/drivel/NAME.
//
// State rather than config because these are derived and safe to delete (the
// index outright, the state DB at the price of a re-sync), and state rather than
// cache because deleting them at the wrong moment is not free: losing the state
// DB loses the echo baseline, and in lazy mode without xattrs it loses the
// placeholder marks (M5).
func StateDir(name string) (string, error) {
	if err := ValidName(name); err != nil {
		return "", err
	}
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locating the home directory: %w", err)
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, AppName, name), nil
}

func configHome() (string, error) {
	if base := os.Getenv("XDG_CONFIG_HOME"); base != "" {
		return filepath.Join(base, AppName), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locating the home directory: %w", err)
	}
	return filepath.Join(home, ".config", AppName), nil
}

// ValidName rejects account and mount names that cannot safely become a path
// component. Both are used to derive directories, so a name containing a
// separator or "." / ".." would place a state DB somewhere the user never named.
func ValidName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("name is empty")
	case name == "." || name == "..":
		return fmt.Errorf("name %q is not usable as a directory", name)
	case strings.ContainsAny(name, `/\`):
		return fmt.Errorf("name %q contains a path separator", name)
	case strings.HasPrefix(name, "-"):
		// Names appear in log prefixes and command lines; a leading dash reads as
		// a flag everywhere it is echoed.
		return fmt.Errorf("name %q starts with a dash", name)
	}
	return nil
}

// expand resolves a path written in the config file: "~" for the home directory,
// and anything still relative against base (the config file's own directory).
//
// Relative-to-the-config-file rather than relative-to-the-process is what makes a
// config directory self-contained: it can be copied to another machine, or
// checked into a dotfiles repo, and still mean the same thing regardless of where
// drivel is started from.
func expand(base, p string) (string, error) {
	if p == "" {
		return "", nil
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("expanding %q: %w", p, err)
		}
		return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/")), nil
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p), nil
	}
	return filepath.Join(base, p), nil
}
