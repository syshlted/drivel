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

	"github.com/syshlted/drivel/plugin"
)

// The hidden subcommand stays hidden, and the test is what makes that
// deliberate rather than accidental: it is not a command anyone has a reason to
// run, and a menu entry offering it would invite exactly the direct invocation
// go-plugin then refuses.
//
// Both renderers are checked because they are separate string builders, and
// usage() because it is the other list a reader takes the commands from.
func TestPluginServeIsNotAdvertised(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "usage")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	usage(f)
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	help, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(help), plugin.ServeCommand) {
		t.Errorf("usage() offers %q:\n%s", plugin.ServeCommand, help)
	}

	app, err := completionApp()
	if err != nil {
		t.Fatalf("completionApp: %v", err)
	}
	for shell, render := range renderers {
		out, rerr := render(app)
		if rerr != nil {
			t.Fatalf("%s: %v", shell, rerr)
		}
		if strings.Contains(string(out), plugin.ServeCommand) {
			t.Errorf("the %s completion offers %q", shell, plugin.ServeCommand)
		}
	}
}

// What `drivel plugin-serve` does with arguments that cannot name a backend.
// Serving a real one is covered against the built binary (bundle_on_test.go),
// because plugin.Serve does not return.
func TestPluginServeRefusesWhatItCannotServe(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no kind", nil, "usage"},
		{"two kinds", []string{"gdrive", "sftp"}, "usage"},
		// A kind this build does not carry is not a crash and not a silent
		// fallthrough to mount: it says what this binary can serve, because the
		// answer differs between builds.
		{"a kind nobody bundled", []string{"nosuchkind"}, "nosuchkind"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := runPluginServe(tc.args)
			if err == nil {
				t.Fatalf("runPluginServe(%q) = nil, want an error", tc.args)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("runPluginServe(%q) = %q, want it to mention %q", tc.args, err, tc.want)
			}
		})
	}
}
