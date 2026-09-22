// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zishmusic/drivel/internal/app"
	"github.com/zishmusic/drivel/internal/hydrate"
	"github.com/zishmusic/drivel/internal/provider/gdrive"
	"github.com/zishmusic/drivel/internal/syncengine"
)

// resolveHelper parses a helper command line the way mount(8) presents one.
func resolveHelper(t *testing.T, argv ...string) (*helperArgs, *helperOptions) {
	t.Helper()
	h, err := parseHelperArgs(argv)
	if err != nil {
		t.Fatalf("parseHelperArgs(%q): %v", argv, err)
	}
	c, err := h.resolve()
	if err != nil {
		t.Fatalf("resolve(%q): %v", argv, err)
	}
	return h, c
}

// specFor runs a command line all the way to the mount it describes.
func specFor(t *testing.T, argv ...string) app.MountSpec {
	t.Helper()
	h, c := resolveHelper(t, argv...)
	s, err := helperSpec(h, c)
	if err != nil {
		t.Fatalf("helperSpec(%q): %v", argv, err)
	}
	return s
}

// helperErr returns the first error the pipeline produces, and fails if there is
// none — a test that asserts on a refusal must not pass because nothing refused.
func helperErr(t *testing.T, argv ...string) string {
	t.Helper()
	h, err := parseHelperArgs(argv)
	if err != nil {
		return err.Error()
	}
	c, err := h.resolve()
	if err != nil {
		return err.Error()
	}
	if _, err := helperSpec(h, c); err != nil {
		return err.Error()
	}
	t.Fatalf("%q: no error", argv)
	return ""
}

// Every -o option has to reach the spec. An option that silently stops being
// read is invisible to every other test in the tree — the same reason
// TestFlagsReachTheSpec exists for the flag path.
func TestHelperOptionsReachTheSpec(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	s := specFor(t, "work", "/m", "-o", strings.Join([]string{
		"data=/d", "credentials=/creds.json", "token=/tok.json",
		"state=/state.db", "index=/index.db", "drive-root=0Bfolder",
		"drive-delete=permanent", "drive-sweep-mode=flat",
		"lazy", "xattr", "debug", "resync", "materialize",
		"max-deletes=7", "sweep-interval=90m",
		"upload-workers=11", "hydrate-workers=13",
	}, ","))

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
	// Distinct numbers, for the reason the flag path's test gives: one value
	// reaching both fields would satisfy a test that used the same one twice.
	if s.UploadWorkers != 11 || s.HydrateWorkers != 13 {
		t.Errorf("workers = up %d / hydrate %d; want 11 / 13", s.UploadWorkers, s.HydrateWorkers)
	}
	if s.Provider != driveKind {
		t.Errorf("Provider = %q; want %q", s.Provider, driveKind)
	}

	var got gdrive.Config
	if err := s.ProviderConfig.Decode(&got); err != nil {
		t.Fatalf("decoding provider config: %v", err)
	}
	want := gdrive.Config{
		Credentials: "/creds.json", Token: "/tok.json", RootID: "0Bfolder",
		IndexPath: "/index.db", Delete: gdrive.DeletePermanent, SweepMode: gdrive.SweepFlat,
	}
	if got != want {
		t.Errorf("provider config = %+v; want %+v", got, want)
	}
}

// An fstab mount defaults to the same pool sizes the flag path does, rather than
// to zero and whatever the layer below happens to make of it.
func TestHelperWorkerDefaults(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	s := specFor(t, "work", "/m", "-o", "data=/d")
	if s.UploadWorkers != syncengine.DefaultWorkers || s.HydrateWorkers != hydrate.DefaultWorkers {
		t.Errorf("workers = up %d / hydrate %d; want %d / %d",
			s.UploadWorkers, s.HydrateWorkers, syncengine.DefaultWorkers, hydrate.DefaultWorkers)
	}
}

// Zero is refused rather than read as "the default", exactly as the flag path
// refuses it: max-deletes=0 means "no limit" two options away in the same line.
func TestHelperRefusesAZeroPool(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	for _, opt := range []string{"upload-workers=0", "hydrate-workers=-1"} {
		h, c := resolveHelper(t, "work", "/m", "-o", "data=/d,"+opt)
		if _, err := helperSpec(h, c); err == nil {
			t.Errorf("%s was accepted", opt)
		} else if !strings.Contains(err.Error(), "pool size") {
			t.Errorf("error %q does not explain that %s is not a pool size", err, opt)
		}
	}
}

// The flag path's push-delay refusal, worded for the option an admin typed.
func TestHelperRefusesAZeroPushDelay(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	for _, opt := range []string{"push-delay=0", "push-delay=-5s"} {
		h, c := resolveHelper(t, "work", "/m", "-o", "data=/d,"+opt)
		if _, err := helperSpec(h, c); err == nil {
			t.Errorf("%s was accepted", opt)
		} else if !strings.Contains(err.Error(), "push-delay") {
			t.Errorf("error %q does not name push-delay", err)
		}
	}
}

// Every flag that describes a mount must have an fstab option, or an admin who
// can write the flag on a command line cannot write the same mount in fstab.
//
// This is the gate the missing options got past: -drive-sweep-mode,
// -upload-workers and -hydrate-workers were all mount-shaping flags with no
// option here, so a line naming one failed as "unknown option" — and nothing
// said so until someone tried it. Recognition is all that is asserted: the value
// is deliberately nonsense, so a type error still counts as "the option exists"
// and the test needs no table of plausible values to drift out of date.
func TestEveryShapingFlagHasAnFstabOption(t *testing.T) {
	for _, name := range mountShapingFlags {
		if name == "mount" {
			continue // the mountpoint is a positional argument to the helper
		}
		h, err := parseHelperArgs([]string{"work", "/m", "-o", name + "=x"})
		if err != nil {
			t.Fatalf("parsing -o %s=x: %v", name, err)
		}
		_, err = h.resolve()
		if err != nil && strings.Contains(err.Error(), "unknown option") {
			t.Errorf("-%s has no fstab option: %v", name, err)
		}
	}
}

// The safety knob has to be reachable from an fstab line. A mount that comes up
// at boot is precisely the one nobody is watching, and config= is not an answer
// for a line that is otherwise complete.
func TestHelperCarriesTheDeleteMode(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	s := specFor(t, "work", "/m", "-o", "data=/d,credentials=/c.json,token=/t.json,drive-delete=permanent")
	var got gdrive.Config
	if err := s.ProviderConfig.Decode(&got); err != nil {
		t.Fatalf("decoding provider config: %v", err)
	}
	if got.Delete != gdrive.DeletePermanent {
		t.Errorf("Delete = %q; want %q", got.Delete, gdrive.DeletePermanent)
	}
	// An unreadable value is not this layer's to judge: the option table passes it
	// through and gdrive.Open refuses it, which TestOpenRejectsUnknownDeleteMode
	// covers. What has to hold here is only that the value arrives.
}

// `user=NAME` is mount(8)'s record of who mounted, and util-linux really does
// send it down to the helper — this line's options were copied from a live
// `mount -o user` run. Reading it as drivel's privilege drop would act on an
// option the admin never aimed at us, which is why that one is spelled run-as.
func TestFstabUserOptionIsNotRunAs(t *testing.T) {
	_, c := resolveHelper(t, "work", "/m", "-o", "rw,user=someone-else,data=/d")
	if c.runAs != "" {
		t.Errorf("run-as = %q; user= must not select the account to run as", c.runAs)
	}

	_, c = resolveHelper(t, "work", "/m", "-o", "data=/d,run-as=someone-else")
	if c.runAs != "someone-else" {
		t.Errorf("run-as = %q; want someone-else", c.runAs)
	}
}

// The options mount(8), systemd and the kernel handle above the helper. Each one
// must be accepted and dropped: failing on them would make an ordinary fstab line
// unmountable, and forwarding them would hand the mount backend an option it does
// not know.
func TestHelperIgnoresGenericMountOptions(t *testing.T) {
	generic := []string{
		"defaults", "auto", "noauto", "_netdev", "nofail", "user", "users",
		"nouser", "owner", "group", "rw", "atime", "relatime", "norelatime",
		"strictatime", "nostrictatime", "comment=whatever",
		"x-systemd.automount", "x-systemd.idle-timeout=1min", "x-gvfs-show",
	}
	s := specFor(t, "work", "/m", "-o", strings.Join(append(generic, "data=/d"), ","))
	if len(s.BackendOptions) != 0 {
		t.Errorf("BackendOptions = %q; generic options must not reach the backend", s.BackendOptions)
	}
	if s.AllowOther {
		t.Error("AllowOther set by a generic option")
	}
}

// These name a real mount semantic drivel does not implement. Accepting one
// would be worse than refusing it: `ro` in the fstab line would say the mount is
// read-only while it stayed writable.
func TestHelperRejectsUnsupportedSemantics(t *testing.T) {
	for _, opt := range []string{"ro", "remount", "bind", "move"} {
		got := helperErr(t, "work", "/m", "-o", opt)
		if !strings.Contains(got, "not supported") {
			t.Errorf("-o %s: %q; want a refusal", opt, got)
		}
	}
}

// An unknown option is an error, for the reason the config loader rejects an
// unknown key: `lazzy` doing nothing looks exactly like a setting that applied.
// -s is the documented escape hatch, and mount(8) is the one that sends it.
func TestHelperUnknownOption(t *testing.T) {
	got := helperErr(t, "work", "/m", "-o", "lazzy")
	if !strings.Contains(got, "unknown option lazzy") {
		t.Errorf("err = %q; want an unknown-option error", got)
	}

	if _, err := (&helperArgs{sloppy: true, opts: []parsedOption{{key: "lazzy"}}}).resolve(); err != nil {
		t.Errorf("-s should downgrade an unknown option to a warning, got %v", err)
	}
}

// config= describes what to mount, and so does every shaping option. Combining
// them would need a precedence rule nobody would remember — the same reason
// -config refuses the shaping flags.
func TestHelperConfigRefusesShapingOptions(t *testing.T) {
	got := helperErr(t, "work", "/m", "-o", "config=/etc/drivel/config.toml,data=/d,lazy")
	if !strings.Contains(got, "cannot be combined with config=") {
		t.Errorf("err = %q; want the config conflict", got)
	}
	// The message must name the options that were actually typed.
	if !strings.Contains(got, "data") || !strings.Contains(got, "lazy") {
		t.Errorf("err = %q; want it to name data and lazy", got)
	}
}

// The shaping set is derived from the flag path's list rather than restated, so
// a flag added there cannot silently become combinable with config= here.
func TestHelperShapingOptionsTrackTheFlags(t *testing.T) {
	shaping := helperShapingOptions()
	for _, f := range mountShapingFlags {
		if f == "mount" {
			continue // the helper takes the mountpoint as a positional argument
		}
		if !shaping[f] {
			t.Errorf("flag -%s describes what to mount but -o %s is not a shaping option", f, f)
		}
	}
	if shaping["mount"] {
		t.Error("mount= should not be a shaping option; the mountpoint is positional")
	}
	if !shaping["account"] {
		t.Error("account= selects credentials and must be a shaping option")
	}
}

// Mount flags the kernel and fusermount3 both understand go to the backend
// verbatim. Every one of them makes the mount more restricted, so a dropped
// option would leave it less restricted than the fstab line asked for.
func TestHelperBackendOptionsPassThrough(t *testing.T) {
	s := specFor(t, "work", "/m", "-o", "data=/d,nosuid,nodev,noexec,default_permissions,allow_other")
	if !s.AllowOther {
		t.Error("allow_other did not reach the spec")
	}
	want := []string{"nosuid", "nodev", "noexec", "default_permissions"}
	if strings.Join(s.BackendOptions, ",") != strings.Join(want, ",") {
		t.Errorf("BackendOptions = %q; want %q", s.BackendOptions, want)
	}
}

// At boot the working directory is /, so a relative path in an fstab line does
// not name what whoever wrote it meant. Refused rather than resolved.
func TestHelperPathsMustBeAbsolute(t *testing.T) {
	for _, opt := range []string{"data=d", "config=c.toml", "state=./s.db", "credentials=../c.json"} {
		got := helperErr(t, "work", "/m", "-o", opt)
		if !strings.Contains(got, "must be an absolute path") {
			t.Errorf("-o %s: %q; want the absolute-path refusal", opt, got)
		}
	}
	if got := helperErr(t, "work", "relative/mnt"); !strings.Contains(got, "must be an absolute path") {
		t.Errorf("relative mountpoint: %q; want a refusal", got)
	}
}

// The state DB must not default to a bare relative filename the way the flag
// path's does: an fstab mount starts with / as its working directory, where
// "drivel-state.db" would mean /drivel-state.db.
func TestHelperStateDefaultsUnderXDG(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	s := specFor(t, "work", "/m", "-o", "data=/d,account=work")

	want := filepath.Join(state, "drivel", "work", "state.db")
	if s.StateDB != want {
		t.Errorf("StateDB = %q; want %q", s.StateDB, want)
	}
	var got gdrive.Config
	if err := s.ProviderConfig.Decode(&got); err != nil {
		t.Fatalf("decoding provider config: %v", err)
	}
	if got.IndexPath != filepath.Join(state, "drivel", "work", "index.db") {
		t.Errorf("IndexPath = %q; want it beside the state DB", got.IndexPath)
	}
}

// account=NAME has to find the same two files `drivel login -account NAME` wrote,
// or an fstab mount authenticates as nobody.
func TestHelperAccountResolvesLoginPaths(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	s := specFor(t, "work", "/m", "-o", "data=/d,account=work")
	var got gdrive.Config
	if err := s.ProviderConfig.Decode(&got); err != nil {
		t.Fatalf("decoding provider config: %v", err)
	}
	if want := filepath.Join(cfg, "drivel", "work", "credentials.json"); got.Credentials != want {
		t.Errorf("Credentials = %q; want %q", got.Credentials, want)
	}
	if want := filepath.Join(cfg, "drivel", "work", "token.json"); got.Token != want {
		t.Errorf("Token = %q; want %q", got.Token, want)
	}
}

// credentials= on its own has to imply a token path. Left empty it reaches the
// provider as "no cached OAuth token at " — an error naming nothing at all —
// and `drivel login` writes the two files side by side anyway.
func TestHelperTokenDefaultsBesideTheCredentials(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	s := specFor(t, "work", "/m", "-o", "data=/d,credentials=/etc/drivel/creds.json")
	var got gdrive.Config
	if err := s.ProviderConfig.Decode(&got); err != nil {
		t.Fatalf("decoding provider config: %v", err)
	}
	if got.Token != "/etc/drivel/token.json" {
		t.Errorf("Token = %q; want it beside the credentials", got.Token)
	}
}

// No credentials and no account means log-only (M1 behaviour), which is what
// makes an fstab line testable without touching the network.
func TestHelperWithoutCredentialsIsLogOnly(t *testing.T) {
	s := specFor(t, "work", "/m", "-o", "data=/d")
	if s.Provider != "" {
		t.Errorf("Provider = %q; want none", s.Provider)
	}
	if s.ProviderConfig != nil {
		t.Error("a log-only mount got a provider config")
	}
}

// A placeholder is a promise the bytes can be fetched later; with no provider
// there is nothing to redeem it against. The flag path makes the same check.
func TestHelperLazyNeedsCredentials(t *testing.T) {
	got := helperErr(t, "work", "/m", "-o", "data=/d,lazy")
	if !strings.Contains(got, "lazy needs credentials=") {
		t.Errorf("err = %q; want the lazy guard", got)
	}
}

// The device column is what findmnt(8) shows and what umount(8) can match on, so
// it has to be the name the admin wrote rather than a constant.
func TestHelperFsNameDefaultsToTheSpecField(t *testing.T) {
	if s := specFor(t, "work", "/m", "-o", "data=/d"); s.FsName != "work" {
		t.Errorf("FsName = %q; want the fstab device field", s.FsName)
	}
	if s := specFor(t, "work", "/m", "-o", "data=/d,fsname=other"); s.FsName != "other" {
		t.Errorf("FsName = %q; want fsname= to win", s.FsName)
	}
}

// The mount's name decides its log prefix and which directory its state DB lives
// in, so "drivel" — which is what a great many fstab lines put in the device
// column — must not become every mount's name.
func TestHelperMountName(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cases := []struct {
		name string
		spec string
		opts string
		want string
	}{
		{"name= wins", "work", "data=/d,name=chosen,account=acct", "chosen"},
		{"then the account", "work", "data=/d,account=acct", "acct"},
		{"then the device field", "work", "data=/d", "work"},
		{"filler falls back to the mountpoint", "drivel", "data=/d", "mnt"},
		{"none falls back too", "none", "data=/d", "mnt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := specFor(t, tc.spec, "/srv/mnt", "-o", tc.opts).Name; got != tc.want {
				t.Errorf("Name = %q; want %q", got, tc.want)
			}
		})
	}
}

func TestParseHelperArgs(t *testing.T) {
	t.Run("flag cluster", func(t *testing.T) {
		h, err := parseHelperArgs([]string{"work", "/m", "-sfnv"})
		if err != nil {
			t.Fatalf("parseHelperArgs: %v", err)
		}
		if !h.sloppy || !h.fake || !h.verbose {
			t.Errorf("cluster not applied: %+v", h)
		}
	})

	t.Run("repeated -o merges", func(t *testing.T) {
		h, err := parseHelperArgs([]string{"work", "/m", "-o", "data=/d", "-o", "lazy,xattr"})
		if err != nil {
			t.Fatalf("parseHelperArgs: %v", err)
		}
		if len(h.opts) != 3 {
			t.Errorf("got %d options; want 3 (%+v)", len(h.opts), h.opts)
		}
	})

	t.Run("mount(8) order", func(t *testing.T) {
		// The real invocation: flags before the positional arguments.
		h, err := parseHelperArgs([]string{"-o", "rw,data=/d", "work", "/m"})
		if err != nil {
			t.Fatalf("parseHelperArgs: %v", err)
		}
		if h.spec != "work" || h.dir != "/m" {
			t.Errorf("spec/dir = %q/%q; want work//m", h.spec, h.dir)
		}
	})

	t.Run("-t is accepted and dropped", func(t *testing.T) {
		h, err := parseHelperArgs([]string{"-t", "fuse.drivel", "work", "/m"})
		if err != nil {
			t.Fatalf("parseHelperArgs: %v", err)
		}
		if h.spec != "work" {
			t.Errorf("spec = %q; -t consumed the wrong argument", h.spec)
		}
	})

	for _, tc := range []struct {
		name string
		argv []string
		want string
	}{
		{"no arguments", []string{}, "no device and no mountpoint"},
		{"no mountpoint", []string{"work"}, "no mountpoint"},
		{"too many", []string{"work", "/m", "/extra"}, "too many arguments"},
		{"dangling -o", []string{"work", "/m", "-o"}, "-o needs an option list"},
		{"unknown flag", []string{"work", "/m", "-z"}, "unknown flag -z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseHelperArgs(tc.argv)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v; want %q", err, tc.want)
			}
		})
	}
}

func TestSplitMountOptions(t *testing.T) {
	cases := []struct {
		in   string
		want []parsedOption
	}{
		{"", nil},
		{"lazy", []parsedOption{{key: "lazy"}}},
		{"a,b", []parsedOption{{key: "a"}, {key: "b"}}},
		{"k=v", []parsedOption{{key: "k", value: "v", hasValue: true}}},
		// An empty value is not the same request as no value: index= disables the
		// index, while index alone is missing its argument.
		{"k=", []parsedOption{{key: "k", value: "", hasValue: true}}},
		// libmount escapes a comma inside a value; a path may legitimately hold one.
		{`data=/a\,b,lazy`, []parsedOption{{key: "data", value: "/a,b", hasValue: true}, {key: "lazy"}}},
		{"a,,b", []parsedOption{{key: "a"}, {key: "b"}}},
	}
	for _, tc := range cases {
		got := splitMountOptions(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("splitMountOptions(%q) = %+v; want %+v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("splitMountOptions(%q)[%d] = %+v; want %+v", tc.in, i, got[i], tc.want[i])
			}
		}
	}
}

func TestHelperConfigMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	body := "" +
		"[[mount]]\nname = \"work\"\npath = \"" + dir + "/work\"\ndata = \"" + dir + "/workdata\"\n\n" +
		"[[mount]]\nname = \"other\"\npath = \"" + dir + "/other\"\ndata = \"" + dir + "/otherdata\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("the device field selects the entry", func(t *testing.T) {
		s := specFor(t, "work", dir+"/work", "-o", "config="+path)
		if s.Name != "work" || s.DataDir != dir+"/workdata" {
			t.Errorf("selected %+v; want the work entry", s)
		}
	})

	t.Run("name= selects the entry", func(t *testing.T) {
		s := specFor(t, "drivel", dir+"/other", "-o", "config="+path+",name=other")
		if s.Name != "other" {
			t.Errorf("Name = %q; want other", s.Name)
		}
	})

	t.Run("process options still compose", func(t *testing.T) {
		s := specFor(t, "work", dir+"/work", "-o", "config="+path+",debug,allow_other,nosuid")
		if !s.Debug || !s.AllowOther || len(s.BackendOptions) != 1 {
			t.Errorf("process options did not compose with config=: %+v", s)
		}
	})

	t.Run("an unknown name lists the known ones", func(t *testing.T) {
		got := helperErr(t, "nosuch", dir+"/work", "-o", "config="+path)
		if !strings.Contains(got, "defines no mount named") || !strings.Contains(got, "other, work") {
			t.Errorf("err = %q; want it to list the defined names", got)
		}
	})

	// Two places now say where this filesystem goes. If they disagree the fstab
	// line is the one everything afterwards keys on — umount(8), `mount -a`, and
	// systemd's generated unit — so mounting the config's path instead would leave
	// a mount nothing can address.
	t.Run("a mountpoint mismatch is refused", func(t *testing.T) {
		got := helperErr(t, "other", dir+"/work", "-o", "config="+path)
		if !strings.Contains(got, "is configured for") {
			t.Errorf("err = %q; want the mountpoint mismatch", got)
		}
	})

	t.Run("one entry needs no name", func(t *testing.T) {
		single := filepath.Join(dir, "single.toml")
		body := "[[mount]]\npath = \"" + dir + "/only\"\ndata = \"" + dir + "/onlydata\"\n"
		if err := os.WriteFile(single, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		// "drivel" in the device column is conventional filler, and there is only
		// one thing this line could mean.
		if s := specFor(t, "drivel", dir+"/only", "-o", "config="+single); s.DataDir != dir+"/onlydata" {
			t.Errorf("selected %+v; want the only entry", s)
		}
	})
}

func TestIsMountHelper(t *testing.T) {
	cases := map[string]bool{
		"/sbin/mount.fuse.drivel": true,
		"/sbin/mount.drivel":      true,
		"mount.drivel":            true,
		"/usr/bin/drivel":         false,
		"drivel":                  false,
		"/usr/bin/umount.drivel":  false,
	}
	for argv0, want := range cases {
		if got := isMountHelper(argv0); got != want {
			t.Errorf("isMountHelper(%q) = %v; want %v", argv0, got, want)
		}
	}
}
