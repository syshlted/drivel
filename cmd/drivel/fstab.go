// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package main

// The mount(8) helper contract, so a drivel mount can be described in /etc/fstab
// and brought up non-interactively at boot (M15).
//
// mount(8) execs /sbin/mount.<type> with a fixed argument shape that is nothing
// like drivel's flags:
//
//	mount.fuse.drivel SPEC DIR [-sfnv] [-o opt,opt,...]
//
// Everything here translates that into the same app.MountSpec the flag and config
// paths produce, so validation, opening and the shutdown ordering keep one
// implementation rather than gaining a second one that drifts from it.
//
// Two things make this *non-interactive* rather than merely scriptable: the
// helper must exit once the filesystem is live, because mount(8) does not return
// until it does (see daemonize), and it must be able to stop being root, because
// the token and the backing tree belong to a user (see run-as).

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zishmusic/drivel/internal/app"
	"github.com/zishmusic/drivel/internal/config"
	"github.com/zishmusic/drivel/internal/hydrate"
	"github.com/zishmusic/drivel/internal/provider/gdrive/gdconf"
	"github.com/zishmusic/drivel/internal/syncengine"
	"github.com/zishmusic/drivel/provider"
)

// helperArgs is a parsed mount(8) helper command line.
type helperArgs struct {
	spec    string // fstab field 1; names the mount
	dir     string // fstab field 2; the mountpoint
	sloppy  bool   // -s: warn about an unknown option instead of failing
	fake    bool   // -f: do everything except mount
	verbose bool   // -v
	opts    []parsedOption
}

// parsedOption is one comma-separated -o entry. hasValue distinguishes `lazy`
// from `lazy=`, which are not the same request.
type parsedOption struct {
	key      string
	value    string
	hasValue bool
}

func (o parsedOption) String() string {
	if o.hasValue {
		return o.key + "=" + o.value
	}
	return o.key
}

// isMountHelper reports whether this process was invoked through one of the
// /sbin/mount.* symlinks rather than as `drivel`.
func isMountHelper(argv0 string) bool {
	return strings.HasPrefix(filepath.Base(argv0), "mount.")
}

// parseHelperArgs implements the mount(8) helper argument contract.
//
// Each flag is honoured rather than merely tolerated: -f is what `mount -f`
// means, -s is the documented escape hatch for an option this version does not
// know, and -n is a no-op only because the helper genuinely never writes mtab —
// fusermount3 does.
func parseHelperArgs(args []string) (*helperArgs, error) {
	h := &helperArgs{}
	var positional []string

	// Consumed from the front rather than indexed, so that "take the next
	// argument" is a slice operation with its own length check right above it.
	for len(args) > 0 {
		a := args[0]
		args = args[1:]
		switch {
		case a == "-o" || a == "--options":
			if len(args) == 0 {
				return nil, errors.New("-o needs an option list")
			}
			h.opts = append(h.opts, splitMountOptions(args[0])...)
			args = args[1:]
		case strings.HasPrefix(a, "-o") && len(a) > 2:
			// `-oopt,opt`, which mount(8) does not emit but a hand-run helper might.
			h.opts = append(h.opts, splitMountOptions(a[2:])...)
		case a == "-t" || a == "--types":
			// The type is already implied by the program name. Accepted so that a
			// mount(8) which does forward it is not a boot failure.
			if len(args) == 0 {
				return nil, errors.New("-t needs a type")
			}
			args = args[1:]
		case a == "--":
			positional = append(positional, args...)
			args = nil
		case strings.HasPrefix(a, "-") && len(a) > 1:
			// A short cluster: -s, -f, -n, -v in any combination.
			for _, c := range a[1:] {
				switch c {
				case 's':
					h.sloppy = true
				case 'f':
					h.fake = true
				case 'n':
					// We never write mtab; fusermount3 does. Accepted and dropped.
				case 'v':
					h.verbose = true
				default:
					return nil, fmt.Errorf("unknown flag -%c", c)
				}
			}
		default:
			positional = append(positional, a)
		}
	}

	const usage = "usage: mount.fuse.drivel SPEC DIR [-sfnv] [-o options]"
	switch len(positional) {
	case 0:
		return nil, errors.New("no device and no mountpoint (" + usage + ")")
	case 1:
		return nil, fmt.Errorf("no mountpoint for %q (%s)", positional[0], usage)
	case 2:
		h.spec, h.dir = positional[0], positional[1]
	default:
		return nil, fmt.Errorf("too many arguments: %s", strings.Join(positional, " "))
	}
	if !filepath.IsAbs(h.dir) {
		// At boot the working directory is /, so a relative mountpoint does not
		// name what whoever wrote the fstab line meant.
		return nil, fmt.Errorf("mountpoint %q must be an absolute path", h.dir)
	}
	h.dir = filepath.Clean(h.dir)
	return h, nil
}

// splitMountOptions splits an -o list on commas, honouring the backslash escape
// that fusermount and libmount use for a comma inside a value.
func splitMountOptions(s string) []parsedOption {
	var out []parsedOption
	var cur strings.Builder
	escaped := false
	flush := func() {
		field := cur.String()
		cur.Reset()
		if field == "" {
			return
		}
		if k, v, found := strings.Cut(field, "="); found {
			out = append(out, parsedOption{key: k, value: v, hasValue: true})
			return
		}
		out = append(out, parsedOption{key: field})
	}
	for _, r := range s {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case r == '\\':
			escaped = true
		case r == ',':
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}

// helperOptions is the resolved -o set: what to mount, how to run it, and what to
// hand the mount backend.
type helperOptions struct {
	configPath string
	name       string

	account       string
	data          string
	credentials   string
	token         string
	state         string
	index         string
	indexSet      bool // index= was given; "" then means "disable it", not "default"
	driveRoot     string
	driveDelete   string
	driveSweep    string
	lazy          bool
	xattr         bool
	resync        bool
	materialize   bool
	maxDeletes    *int
	sweepInterval *time.Duration
	pushDelay     *time.Duration
	// Pool sizes, as pointers for the reason max-deletes is one: an explicit 0 is
	// refused rather than read as "the default", and merging it with "unset" is
	// what would make that impossible to tell apart.
	uploadWorkers  *int
	hydrateWorkers *int

	runAs        string
	foreground   bool
	logfile      string
	debug        bool
	fsname       string
	mountTimeout time.Duration

	allowOther     bool
	backendOptions []string

	// shaping records which shaping options were given, so the config= conflict
	// can name the one the admin actually typed.
	shaping []string
}

// ignoredHelperOptions are handled by mount(8), by systemd or by the kernel
// before the helper ever sees them, or describe a policy this layer has no part
// in. They are accepted and dropped.
//
// `user`, `users`, `owner` and `group` are in here deliberately: they are
// mount(8)'s "who may mount this", and util-linux passes `user=NAME` down to the
// helper to record who did. Reading that as drivel's own privilege-drop request
// would act on an option the admin never aimed at us — which is why that one is
// spelled run-as. See DESIGN.md §9, M15.
var ignoredHelperOptions = map[string]bool{
	"defaults": true, "auto": true, "noauto": true, "_netdev": true,
	"nofail": true, "user": true, "users": true, "nouser": true, "owner": true,
	"group": true, "rw": true, "atime": true, "relatime": true,
	"norelatime": true, "strictatime": true, "nostrictatime": true,
	"comment": true,
}

// backendPassthroughOptions go to the mount backend verbatim. They are mount
// flags the kernel and fusermount3 both understand, and every one of them makes
// the mount *more* restricted — so silently dropping one would hand back a
// privilege the fstab line refused.
var backendPassthroughOptions = map[string]bool{
	"nosuid": true, "nodev": true, "noexec": true, "noatime": true,
	"sync": true, "dirsync": true, "default_permissions": true,
	"allow_root": true, "auto_unmount": true,
}

// rejectedHelperOptions name a real mount semantic drivel does not implement.
// Accepting one would be worse than refusing it: the line would say the mount is
// read-only, and it would not be.
var rejectedHelperOptions = map[string]string{
	"ro":      "drivel has no read-only mode",
	"remount": "drivel cannot remount in place; unmount and mount again",
	"bind":    "a bind mount does not go through a filesystem helper",
	"move":    "drivel cannot move a mount",
}

// helperShapingOptions are the options that describe *what* to mount, and so
// cannot be combined with config=.
//
// Derived from mountShapingFlags rather than restated, so the flag path and the
// helper cannot drift: "mount" drops out because the helper takes the mountpoint
// as a positional argument, and "account" joins because it selects credentials,
// which is exactly what a config file also does.
func helperShapingOptions() map[string]bool {
	s := map[string]bool{"account": true}
	for _, f := range mountShapingFlags {
		if f == "mount" {
			continue
		}
		s[f] = true
	}
	return s
}

// resolve turns the raw -o list into helperOptions.
func (h *helperArgs) resolve() (*helperOptions, error) {
	c := &helperOptions{}
	shaping := helperShapingOptions()

	for _, o := range h.opts {
		if shaping[o.key] {
			c.shaping = append(c.shaping, o.key)
		}
		var err error
		switch o.key {
		// --- mode -------------------------------------------------------------
		case "config":
			c.configPath, err = pathValue(o)
		case "name":
			c.name, err = stringValue(o)

		// --- shaping ----------------------------------------------------------
		case "account":
			c.account, err = stringValue(o)
		case "data":
			c.data, err = pathValue(o)
		case "credentials":
			c.credentials, err = pathValue(o)
		case "token":
			c.token, err = pathValue(o)
		case "state":
			c.state, err = pathValue(o)
		case "index":
			// An empty value is meaningful here: it disables the index. So only a
			// non-empty one has to look like a path.
			c.indexSet = true
			if c.index, err = stringValue(o); err == nil && c.index != "" {
				c.index, err = pathValue(o)
			}
		case "drive-root":
			c.driveRoot, err = stringValue(o)
		case "drive-delete":
			c.driveDelete, err = stringValue(o)
		case "drive-sweep-mode":
			c.driveSweep, err = stringValue(o)
		case "lazy":
			c.lazy, err = boolValue(o)
		case "xattr":
			c.xattr, err = boolValue(o)
		case "resync":
			c.resync, err = boolValue(o)
		case "materialize":
			c.materialize, err = boolValue(o)
		case "max-deletes":
			var n int
			if n, err = intValue(o); err == nil {
				c.maxDeletes = &n
			}
		case "sweep-interval":
			var d time.Duration
			if d, err = durationValue(o); err == nil {
				c.sweepInterval = &d
			}
		case "push-delay":
			var d time.Duration
			if d, err = durationValue(o); err == nil {
				c.pushDelay = &d
			}
		case "upload-workers":
			var n int
			if n, err = intValue(o); err == nil {
				c.uploadWorkers = &n
			}
		case "hydrate-workers":
			var n int
			if n, err = intValue(o); err == nil {
				c.hydrateWorkers = &n
			}

		// --- process ----------------------------------------------------------
		case "run-as":
			c.runAs, err = stringValue(o)
		case "foreground":
			c.foreground, err = boolValue(o)
		case "logfile":
			c.logfile, err = pathValue(o)
		case "debug":
			c.debug, err = boolValue(o)
		case "fsname":
			c.fsname, err = stringValue(o)
		case "mount-timeout":
			c.mountTimeout, err = durationValue(o)

		// --- backend ----------------------------------------------------------
		case "allow_other":
			c.allowOther, err = boolValue(o)

		default:
			switch {
			case backendPassthroughOptions[o.key]:
				c.backendOptions = append(c.backendOptions, o.String())
			case ignoredHelperOptions[o.key], strings.HasPrefix(o.key, "x-"):
				// mount(8)'s business, or systemd's. Not ours.
			case rejectedHelperOptions[o.key] != "":
				err = fmt.Errorf("option %s is not supported: %s", o.key, rejectedHelperOptions[o.key])
			case h.sloppy:
				// gosec G706 flags this as log injection. The option text comes from
				// /etc/fstab or from a mount(8) command line, both of which are
				// already root's to write — there is no lower-privileged source for
				// it to be injected from.
				log.Printf("warning: ignoring unknown option %s (-s)", o) //nolint:gosec // G706: see above
			default:
				err = fmt.Errorf("unknown option %s (pass -s to ignore unknown options)", o)
			}
		}
		if err != nil {
			return nil, err
		}
	}

	if c.configPath != "" && len(c.shaping) > 0 {
		return nil, fmt.Errorf("%s cannot be combined with config=%s; describe the mount in the config file",
			strings.Join(dedupe(c.shaping), ", "), c.configPath)
	}
	if c.lazy && c.credentials == "" && c.account == "" {
		// The same check the flag path makes, worded for the option that was typed.
		// (Config mode cannot reach here: lazy is a shaping option, and the config
		// loader makes the equivalent check itself.)
		return nil, errors.New("lazy needs credentials= or account= (there is nothing to hydrate from)")
	}
	return c, nil
}

func dedupe(s []string) []string {
	out := make([]string, 0, len(s))
	seen := make(map[string]bool, len(s))
	for _, v := range s {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

func stringValue(o parsedOption) (string, error) {
	if !o.hasValue {
		return "", fmt.Errorf("option %s needs a value (%s=...)", o.key, o.key)
	}
	return o.value, nil
}

// pathValue accepts an absolute path, or one under the mounting account's home.
// A relative path is refused rather than resolved: at boot the working directory
// is /, so it would silently name something else entirely.
func pathValue(o parsedOption) (string, error) {
	v, err := stringValue(o)
	if err != nil {
		return "", err
	}
	if filepath.IsAbs(v) || v == "~" || strings.HasPrefix(v, "~/") {
		return v, nil
	}
	return "", fmt.Errorf("option %s: %q must be an absolute path (or ~/... for the mounting account's home)", o.key, v)
}

func boolValue(o parsedOption) (bool, error) {
	if !o.hasValue {
		return true, nil
	}
	switch strings.ToLower(o.value) {
	case "true", "yes", "1", "on":
		return true, nil
	case "false", "no", "0", "off":
		return false, nil
	}
	return false, fmt.Errorf("option %s: %q is not a boolean", o.key, o.value)
}

func intValue(o parsedOption) (int, error) {
	v, err := stringValue(o)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("option %s: %q is not a number", o.key, v)
	}
	return n, nil
}

func durationValue(o parsedOption) (time.Duration, error) {
	v, err := stringValue(o)
	if err != nil {
		return 0, err
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("option %s: %w", o.key, err)
	}
	return d, nil
}

// expandHelperPath resolves a "~" prefix against the running account's home. It
// runs after any privilege drop, so "~" is the mounting account's home rather
// than root's.
func expandHelperPath(p string) (string, error) {
	if p == "" || !strings.HasPrefix(p, "~") {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("expanding %q: %w", p, err)
	}
	return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/")), nil
}

// helperSpec builds the single mount this fstab line describes.
//
// One line means one mount: the fstab dir is the mountpoint, and mount(8) and
// umount(8) both address it by that path. A config file naming several mounts is
// therefore filtered down to the one this line is for, not served whole.
func helperSpec(h *helperArgs, c *helperOptions) (app.MountSpec, error) {
	var (
		spec app.MountSpec
		err  error
	)
	if c.configPath != "" {
		spec, err = helperConfigSpec(h, c)
	} else {
		spec, err = helperOptionSpec(h, c)
	}
	if err != nil {
		return spec, err
	}

	// Process and backend options compose with both modes, the way -debug and
	// -pprof compose with -config on the flag path.
	spec.Mountpoint = h.dir
	spec.Debug = spec.Debug || c.debug
	spec.AllowOther = c.allowOther
	spec.BackendOptions = c.backendOptions
	spec.FsName = c.fsname
	if spec.FsName == "" {
		// The fstab spec field, so findmnt(8) shows the source the admin wrote and
		// umount(8) can match on it.
		spec.FsName = h.spec
	}
	return spec, nil
}

// helperConfigSpec selects this line's mount from a config file.
func helperConfigSpec(h *helperArgs, c *helperOptions) (app.MountSpec, error) {
	var zero app.MountSpec

	path, err := expandHelperPath(c.configPath)
	if err != nil {
		return zero, err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return zero, err
	}
	specs, err := cfg.Specs()
	if err != nil {
		return zero, fmt.Errorf("%s: %w", path, err)
	}

	want := c.name
	if want == "" {
		want = h.spec
	}
	// One entry and nothing to disambiguate: the fstab spec field is conventional
	// filler ("drivel", "none") as often as it is a name, and there is only one
	// thing this line could mean.
	if len(specs) == 1 && c.name == "" {
		return checkHelperMountpoint(specs[0], h, path)
	}
	for _, s := range specs {
		if s.Name == want {
			return checkHelperMountpoint(s, h, path)
		}
	}
	names := make([]string, 0, len(specs))
	for _, s := range specs {
		names = append(names, s.Name)
	}
	sort.Strings(names)
	return zero, fmt.Errorf("%s defines no mount named %q (defined: %s); name it in the fstab device field or with name=",
		path, want, strings.Join(names, ", "))
}

// checkHelperMountpoint refuses a config entry that is mounted somewhere else.
//
// Two places now say where this filesystem goes, and if they disagree the fstab
// line is the one the system will act on afterwards: umount(8), `mount -a` and
// systemd's generated unit all key on the fstab dir. Mounting the config's path
// instead would leave a mount nothing can address.
func checkHelperMountpoint(s app.MountSpec, h *helperArgs, path string) (app.MountSpec, error) {
	if filepath.Clean(s.Mountpoint) != h.dir {
		return app.MountSpec{}, fmt.Errorf("mount %q in %s is configured for %s, but the fstab line mounts %s",
			s.Name, path, s.Mountpoint, h.dir)
	}
	return s, nil
}

// helperOptionSpec builds a mount from the -o options alone, which mirror the
// `drivel mount` flags one for one.
func helperOptionSpec(h *helperArgs, c *helperOptions) (app.MountSpec, error) {
	var zero app.MountSpec

	name := helperMountName(h, c)
	if err := config.ValidName(name); err != nil {
		return zero, fmt.Errorf("mount name: %w (set one with name=)", err)
	}

	data, err := expandHelperPath(c.data)
	if err != nil {
		return zero, err
	}
	spec := app.MountSpec{
		Name:          name,
		Mountpoint:    h.dir,
		DataDir:       data,
		Lazy:          c.lazy,
		Xattr:         c.xattr,
		Resync:        c.resync,
		Materialize:   c.materialize,
		MaxDeletes:    syncengine.DefaultMaxDeletes,
		SweepInterval: syncengine.DefaultSweepInterval,
		PushDelay:     syncengine.DefaultPushDelay,

		UploadWorkers:  syncengine.DefaultWorkers,
		HydrateWorkers: hydrate.DefaultWorkers,
	}
	if c.maxDeletes != nil {
		spec.MaxDeletes = *c.maxDeletes
	}
	if c.sweepInterval != nil {
		spec.SweepInterval = *c.sweepInterval
	}
	if c.pushDelay != nil {
		if *c.pushDelay <= 0 {
			return zero, fmt.Errorf("push-delay=%s is not a coalescing window (omit the option for the default of %s)",
				*c.pushDelay, syncengine.DefaultPushDelay)
		}
		spec.PushDelay = *c.pushDelay
	}
	// The same refusal the flag path makes, worded for the option that was typed:
	// zero is not a pool size, and max-deletes=0 means "no limit" two options
	// away, so reading it as "the default" here is M8 rule 6 in a new place.
	if c.uploadWorkers != nil {
		if *c.uploadWorkers < 1 {
			return zero, fmt.Errorf("upload-workers=%d is not a pool size (1 or more; omit the option for the default of %d)",
				*c.uploadWorkers, syncengine.DefaultWorkers)
		}
		spec.UploadWorkers = *c.uploadWorkers
	}
	if c.hydrateWorkers != nil {
		if *c.hydrateWorkers < 1 {
			return zero, fmt.Errorf("hydrate-workers=%d is not a pool size (1 or more; omit the option for the default of %d)",
				*c.hydrateWorkers, hydrate.DefaultWorkers)
		}
		spec.HydrateWorkers = *c.hydrateWorkers
	}

	// Where this account's files live, if the line named one. Reusing
	// config.AccountDir is what keeps `drivel login -account work` and
	// `-o account=work` pointing at the same two files.
	var accountDir string
	if c.account != "" {
		if accountDir, err = config.AccountDir(c.account); err != nil {
			return zero, fmt.Errorf("account=%s: %w", c.account, err)
		}
	}

	credentials, err := helperCredentialPath(c.credentials, accountDir, "credentials.json")
	if err != nil {
		return zero, err
	}
	// Credentials are what turn cloud sync on; without them the mount runs
	// log-only (M1 behaviour) and never reaches a provider.
	if credentials == "" {
		return spec, nil
	}
	token, err := helperCredentialPath(c.token, accountDir, "token.json")
	if err != nil {
		return zero, err
	}
	if token == "" {
		// credentials= without account= or token=. `drivel login` writes the two
		// side by side, so that is where to look — better than an empty path, which
		// reaches the provider as "no cached OAuth token at ".
		token = filepath.Join(filepath.Dir(credentials), "token.json")
	}

	// Unlike the flag path, these default under $XDG_STATE_HOME rather than to a
	// bare relative filename: an fstab mount starts with / as its working
	// directory, where "drivel-state.db" would mean /drivel-state.db.
	stateDir, err := config.StateDir(name)
	if err != nil {
		return zero, err
	}
	state, err := expandHelperPath(c.state)
	if err != nil {
		return zero, err
	}
	if state == "" {
		state = filepath.Join(stateDir, "state.db")
	}
	spec.StateDB = state

	index := filepath.Join(stateDir, "index.db")
	if c.indexSet {
		if index, err = expandHelperPath(c.index); err != nil {
			return zero, err
		}
	}

	spec.Provider = driveKind
	spec.ProviderConfig = provider.MustEncodeConfig(gdconf.Config{
		Credentials: credentials,
		Token:       token,
		RootID:      c.driveRoot,
		Delete:      gdconf.DeleteMode(c.driveDelete),
		SweepMode:   gdconf.SweepMode(c.driveSweep),
		IndexPath:   index,
	})
	return spec, nil
}

// helperMountName picks the name this mount is known by — in logs, and as the
// directory its state DB lives in.
func helperMountName(h *helperArgs, c *helperOptions) string {
	if c.name != "" {
		return c.name
	}
	if c.account != "" {
		return c.account
	}
	// The fstab device field, when it can be a name. It often is ("work"), and
	// often is conventional filler ("drivel", "none") that would name every mount
	// on the machine the same thing — so fall back to the mountpoint.
	if h.spec != "" && h.spec != "drivel" && h.spec != "none" && config.ValidName(h.spec) == nil {
		return h.spec
	}
	return filepath.Base(h.dir)
}

// helperCredentialPath resolves an explicit path, else the account's copy.
func helperCredentialPath(explicit, accountDir, name string) (string, error) {
	if explicit != "" {
		return expandHelperPath(explicit)
	}
	if accountDir == "" {
		return "", nil
	}
	return filepath.Join(accountDir, name), nil
}
