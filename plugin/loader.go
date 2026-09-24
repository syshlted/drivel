// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package plugin

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/syshlted/drivel/provider"
)

// PathEnv names the environment variable that replaces the default search path.
//
// It REPLACES rather than prepends, deliberately. A variable that merely went
// first would make "which plugin am I running?" depend on something invisible in
// the config file, in a program where two plugins for one kind is already the
// ambiguity the filename rule exists to avoid. Replacing is a decision the
// operator can see the whole of.
//
// It is also on M16's scrub list: an fstab mount drops privileges and must not
// carry root's idea of where executables live into the user's mount.
const PathEnv = "DRIVEL_PLUGIN_PATH"

// Loader finds provider plugins on disk and turns them into provider.Factory
// values the registry can open.
//
// It does not launch anything. Discovery is a directory scan, so a process that
// mounts nothing pays nothing, and a config naming a kind that is not installed
// fails with provider.ErrUnknownKind listing what is — which is the same error,
// with the same wording, that a typo in a kind name produced before M9.
type Loader struct {
	path []string
	lg   *log.Logger

	// found maps kind to the executable that implements it, and refused maps kind
	// to why drivel will not run the executable it found. A refused kind is still
	// *discovered*: it registers a Factory that fails with the reason, because
	// "unknown kind gdrive (available: sftp)" would send someone hunting for a missing
	// install when the file is right there and its permissions are wrong.
	found   map[string]string
	refused map[string]error
	// shadowed records kinds that appeared in more than one directory, for the
	// one log line that says which won.
	shadowed map[string][]string
}

// NewLoader returns a Loader searching path, or the default search path when it
// is empty. A nil logger uses the log package's default.
func NewLoader(path []string, lg *log.Logger) *Loader {
	if len(path) == 0 {
		path = DefaultPath()
	}
	return &Loader{path: path, lg: lg}
}

// DefaultPath is where drivel looks for plugins, in order, when PathEnv is
// unset. The first directory holding a given kind wins.
//
// The order is "closest to this binary first". A drivel built from a checkout
// finds the plugins built beside it without any configuration, which is what
// makes `make build && ./bin/drivel …` work; a packaged drivel in /usr/bin finds
// the plugins its package installed next to it; and a plugin a user installed
// for themselves is found before a system-wide one of the same kind, which is
// the direction every other per-user override in the tree runs.
func DefaultPath() []string {
	if env := os.Getenv(PathEnv); env != "" {
		var out []string
		for _, dir := range filepath.SplitList(env) {
			if dir != "" {
				out = append(out, dir)
			}
		}
		return out
	}

	var out []string
	if exe, err := os.Executable(); err == nil {
		// Resolved, so that a symlinked drivel finds the plugins next to the real
		// binary rather than next to the link. /sbin/mount.fuse.drivel is exactly
		// that symlink (M16), and a boot mount is the case that can least afford to
		// come up without a backend.
		if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
			exe = resolved
		}
		out = append(out, filepath.Dir(exe))
	}
	if dir := os.Getenv("XDG_DATA_HOME"); dir != "" {
		out = append(out, filepath.Join(dir, "drivel", "plugins"))
	} else if home, err := os.UserHomeDir(); err == nil {
		out = append(out, filepath.Join(home, ".local", "share", "drivel", "plugins"))
	}
	return append(out,
		"/usr/local/lib/drivel/plugins",
		"/usr/lib/drivel/plugins",
	)
}

// Path reports the directories this Loader searches.
func (l *Loader) Path() []string { return append([]string(nil), l.path...) }

// Discover scans the search path. It is idempotent and cheap, and callers may
// invoke it directly to report what is installed; Kinds and Register call it.
//
// A directory that does not exist is not an error — most of the default path
// never does — and neither is one that cannot be read, which would otherwise
// make a single unreadable system directory stop a user's own plugins from
// loading.
func (l *Loader) Discover() {
	if l.found != nil {
		return
	}
	l.found = map[string]string{}
	l.refused = map[string]error{}
	l.shadowed = map[string][]string{}

	for _, dir := range l.path {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				l.logf("plugin search: %s: %v", dir, err)
			}
			continue
		}
		for _, e := range entries {
			kind, ok := kindOf(e.Name())
			if !ok {
				continue
			}
			full := filepath.Join(dir, e.Name())
			if _, taken := l.found[kind]; taken {
				l.shadowed[kind] = append(l.shadowed[kind], full)
				continue
			}
			if _, taken := l.refused[kind]; taken {
				l.shadowed[kind] = append(l.shadowed[kind], full)
				continue
			}
			if err := safeToRun(full); err != nil {
				l.refused[kind] = err
				continue
			}
			l.found[kind] = full
		}
	}

	for kind, others := range l.shadowed {
		winner := l.found[kind]
		if winner == "" {
			winner = "(refused)"
		}
		l.logf("plugin %s: using %s; also found %s", kind, winner, strings.Join(others, ", "))
	}
}

// Kinds lists every provider kind found on the search path, sorted, including
// kinds whose executable was refused.
func (l *Loader) Kinds() []string {
	l.Discover()
	out := make([]string, 0, len(l.found)+len(l.refused))
	for k := range l.found {
		out = append(out, k)
	}
	for k := range l.refused {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Register adds a Factory for every discovered kind to reg.
//
// This is the whole of the integration above the seam: the registry, the
// composition root and the sync engine are unchanged, because a plugin-backed
// provider is a provider.Factory like any other. Registering by hand rather than
// by init() is M8 rule 3, and it matters more now than it did then — a factory
// registered as a side effect of a *directory listing* would be process-global
// mutable state with an external input.
func (l *Loader) Register(reg *provider.Registry) error {
	l.Discover()
	// So that a config naming a backend nobody installed says where to install it,
	// rather than only that the name is unknown. See provider.Registry.Hint.
	reg.Hint(l.describeMissing)
	for _, kind := range l.Kinds() {
		f, err := l.Factory(kind)
		if err != nil {
			return err
		}
		if err := reg.Register(kind, f); err != nil {
			return err
		}
	}
	return nil
}

// describeMissing says where this Loader looked for a kind it does not have.
func (l *Loader) describeMissing(kind string) string {
	if _, ok := kindOf(BinaryPrefix + kind); !ok {
		// Not a name a plugin file could carry, so naming directories would be
		// misleading — nothing that could be installed would have helped.
		return fmt.Sprintf("%q is not a usable provider name", kind)
	}
	return fmt.Sprintf("no %s%s in %s", BinaryPrefix, kind, strings.Join(l.path, ", "))
}

// Factory returns the Factory that launches the named kind. The error for a kind
// that is not installed wraps provider.ErrUnknownKind, so a caller can tell it
// apart from a backend that exists and failed.
func (l *Loader) Factory(kind string) (provider.Factory, error) {
	l.Discover()
	if reason, refused := l.refused[kind]; refused {
		// A Factory rather than an error, so the refusal is reported when someone
		// tries to *use* the backend, naming the file and the problem, instead of
		// at startup for a kind nothing was going to mount.
		return func(context.Context, provider.Params) (provider.Store, error) {
			return nil, reason
		}, nil
	}
	path, ok := l.found[kind]
	if !ok {
		return nil, fmt.Errorf("%w: %q (%s)", provider.ErrUnknownKind, kind, l.describeMissing(kind))
	}
	return func(ctx context.Context, p provider.Params) (provider.Store, error) {
		return open(ctx, kind, path, p)
	}, nil
}

// open launches one backend and returns the host-side store.
func open(ctx context.Context, kind, path string, p provider.Params) (provider.Store, error) {
	proc := &process{kind: kind, path: path, cfg: p.Config, lg: p.Log}
	proc.mu.Lock()
	err := proc.launch(ctx)
	caps := proc.caps
	proc.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if p.Log != nil {
		p.Log.Print(describeLaunch(kind, path, caps))
	} else {
		log.Print(describeLaunch(kind, path, caps))
	}
	return &pluginStore{
		remoteStore: &remoteStore{
			sess: proc,
			caps: caps,
			lg:   func(f string, a ...any) { proc.logf(kind+": "+f, a...) },
		},
		proc: proc,
	}, nil
}

// kindOf extracts the provider kind from an executable's name, and reports
// whether the name is a plugin's at all.
//
// The kind comes from the filename and from nothing the plugin says about
// itself. A plugin that announced its own kind could contradict its filename,
// and then two files could claim one kind — with a resolution order that would
// have to be either "first found" (so the answer depends on a directory listing)
// or "last wins" (so installing anything can silently replace a backend).
func kindOf(name string) (string, bool) {
	if runtime.GOOS == "windows" {
		name = strings.TrimSuffix(name, ".exe")
	}
	kind, ok := strings.CutPrefix(name, BinaryPrefix)
	if !ok || kind == "" {
		return "", false
	}
	// A kind is a name in a config file, so it stays to what one can safely hold:
	// no dots, no separators, nothing that could make `provider = "…"` mean a
	// path. This also drops the debris a build leaves in a directory —
	// `drivel-provider-gdrive.old`, an editor's backup — rather than launching it.
	for _, r := range kind {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return "", false
		}
	}
	return kind, true
}

// safeToRun refuses an executable that someone other than its owner could have
// written.
//
// This is not a substitute for trusting the plugin — see the package doc, a
// plugin runs as the user and drivel loading it is drivel trusting it. It is the
// narrower check that the thing being trusted is the thing the administrator
// installed: a group- or world-writable binary, or one sitting in a directory
// anybody can write to, can be swapped between the moment it was installed and
// the moment drivel launches it, and drivel would then hand a stranger's code
// the user's credentials. The M16 boot mount is where this matters most, because
// there is nobody watching it happen.
//
// The check is skipped for a directory that is world-writable but sticky (/tmp
// and its like), because sticky is precisely the bit that says "you may create
// here but not replace what is not yours".
func safeToRun(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("plugin %s: %w", path, err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("plugin %s: not a regular file", path)
	}
	if fi.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("plugin %s: not executable", path)
	}
	if w := fi.Mode().Perm() & 0o022; w != 0 {
		return fmt.Errorf("plugin %s: refusing to run: mode %#o is writable by %s",
			path, fi.Mode().Perm(), writableBy(w))
	}
	dir, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("plugin %s: %w", path, err)
	}
	if w := dir.Mode().Perm() & 0o022; w != 0 && dir.Mode()&os.ModeSticky == 0 {
		return fmt.Errorf("plugin %s: refusing to run: its directory %s is writable by %s",
			path, filepath.Dir(path), writableBy(w))
	}
	return nil
}

// writableBy names who a permission bit pair lets write, for the refusal
// message. Naming it is the difference between a message someone can act on and
// one they have to decode.
func writableBy(bits fs.FileMode) string {
	switch {
	case bits&0o022 == 0o022:
		return "its group and by everyone"
	case bits&0o020 != 0:
		return "its group"
	default:
		return "everyone"
	}
}

func (l *Loader) logf(format string, args ...any) {
	if l.lg == nil {
		log.Printf(format, args...)
		return
	}
	l.lg.Printf(format, args...)
}
