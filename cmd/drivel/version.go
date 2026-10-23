// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
)

// runVersion prints what this binary is: its version, the commit it was built
// from, the toolchain and platform, and the backends it carries.
//
// Nothing here is stamped by the build. Go records the module version in the
// binary itself — the tag for `go install …@v0.1.0`, and since Go 1.24 the tag
// (or a pseudo-version, with "+dirty" for a modified tree) for a `go build` in a
// git checkout — so there is no -ldflags variable to forget, and a release and a
// local build answer the same way. The release workflow compares the first line
// against the tag it is building, which is what makes that claim checked rather
// than assumed.
func runVersion(args []string) error {
	fs := flag.NewFlagSet("version", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "Usage: drivel version\n\n"+
			"Prints the version, the commit it was built from, the Go toolchain and\n"+
			"platform, and the backends built in. Include it in a bug report.\n")
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("version takes no arguments")
	}
	info, _ := debug.ReadBuildInfo()
	fmt.Print(versionText(info, runtime.Version(), runtime.GOOS+"/"+runtime.GOARCH, bundledKinds()))
	return nil
}

// versionText is runVersion's output, from its inputs, so every shape of build
// is testable without building one.
//
// The first line is "drivel VERSION" and nothing else, because it is the line a
// script reads. A binary with no build information says "unknown" rather than
// leaving the line out, and one built outside a checkout says Go's own
// "(devel)": both are true, and both tell a reader that this is not a release.
func versionText(info *debug.BuildInfo, goVersion, platform string, backends []string) string {
	var b strings.Builder
	version := "unknown"
	if info != nil && info.Main.Version != "" {
		version = info.Main.Version
	}
	fmt.Fprintf(&b, "drivel %s\n", version)

	if info != nil {
		var rev, when string
		modified := false
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.time":
				when = s.Value
			case "vcs.modified":
				modified = s.Value == "true"
			}
		}
		// `go install …@version` builds from a module zip, which carries no VCS
		// information, so a release installed that way has no commit line. The
		// version is the identity there.
		if rev != "" {
			line := "commit " + rev
			if when != "" {
				line += " (" + when + ")"
			}
			if modified {
				line += ", with uncommitted changes"
			}
			fmt.Fprintln(&b, line)
		}
	}

	fmt.Fprintf(&b, "%s %s\n", goVersion, platform)
	if len(backends) == 0 {
		fmt.Fprintln(&b, "backends: none built in (TAGS=nobundle); installed drivel-provider-* are used")
	} else {
		fmt.Fprintf(&b, "backends: %s\n", strings.Join(backends, ", "))
	}
	return b.String()
}
