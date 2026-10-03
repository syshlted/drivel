// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package app

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

// module is this repository's import path, so a dependency can be told from a
// third party's by prefix.
const module = "github.com/syshlted/drivel/"

// aboveTheSeam are the packages that must know a provider only through the
// provider package's interfaces. Each one is here because something would break
// if it learned a concrete backend: the mount backend and the sync core would
// stop being portable (§2.9's cross-compile proof is read off exactly this), and
// app is the composition root whose entire job is to name a provider *kind*.
var aboveTheSeam = []string{
	module + "internal/app",
	module + "internal/config",
	module + "internal/fsevent",
	module + "internal/hydrate",
	module + "internal/mount",
	module + "internal/pathindex",
	module + "internal/state",
	module + "internal/syncengine",
	module + "internal/vfs",
	module + "plugin",
	module + "provider",
	module + "ranges",
}

// Nothing above the seam links a concrete provider.
//
// This used to be readable off the binary: until M23 `drivel` linked no backend
// at all, so "the Drive SDK is not in here" was a fact about the artifact and
// this property came with it for free. A bundled build links both backends into
// the host, which changes nothing in the source and takes that inference away —
// so the rule is asserted directly, which is the better statement of it anyway.
// It is the one test the milestone owed.
//
// It is a claim about the *build* graph. A test may import a backend, and M8's
// seam proof does exactly that (multiclient_test.go registers gdrive twice to
// run two Drive stores at once); `go list` without -test is what keeps the two
// questions apart.
func TestNothingAboveTheSeamLinksAProvider(t *testing.T) {
	t.Parallel()

	deps := packageDeps(t, aboveTheSeam)
	for _, pkg := range aboveTheSeam {
		for _, dep := range deps[pkg] {
			if strings.HasPrefix(dep, module+"internal/provider/") {
				t.Errorf("%s depends on %s; above the seam a provider is reached through the provider package, never by name",
					pkg, dep)
			}
		}
	}
}

// ...and the query that says so has to be able to find one, or the test above
// passes for the wrong reason. The host command is where a provider *is* linked
// in the default build (M23), so it is the positive control.
func TestTheHostCommandLinksTheBundledBackends(t *testing.T) {
	t.Parallel()

	cmdPkg := module + "cmd/drivel"
	var found []string
	for _, dep := range packageDeps(t, []string{cmdPkg})[cmdPkg] {
		if strings.HasPrefix(dep, module+"internal/provider/") {
			found = append(found, dep)
		}
	}
	if len(found) == 0 {
		t.Errorf("%s links no provider, so the import-graph check above proves nothing; "+
			"if the default build stopped bundling backends, this test is what to update", cmdPkg)
	}
}

// packageDeps returns each named package's transitive build dependencies.
func packageDeps(t *testing.T, pkgs []string) map[string][]string {
	t.Helper()

	args := append([]string{"list", "-f", `{{.ImportPath}} {{join .Deps " "}}`}, pkgs...)
	cmd := exec.Command("go", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, stderr.String())
	}

	deps := make(map[string][]string, len(pkgs))
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		deps[fields[0]] = fields[1:]
	}
	for _, pkg := range pkgs {
		if _, ok := deps[pkg]; !ok {
			t.Fatalf("go list said nothing about %s; has it been renamed?", pkg)
		}
	}
	return deps
}
