// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package plugin

import (
	"slices"
	"strings"
	"testing"
)

// The environment a plugin gets is built, not inherited. This is the test that
// fails if a future change makes it inherit again — which would be invisible
// otherwise, since everything would carry on working.
func TestPluginEnvIsBuiltNotInherited(t *testing.T) {
	// Things a plugin needs.
	t.Setenv("PATH", "/usr/bin")
	t.Setenv("HOME", "/home/someone")
	t.Setenv("SSH_AUTH_SOCK", "/run/agent.sock")
	t.Setenv("LC_TIME", "en_GB.UTF-8")

	// Things it must not get. Each one is a way for a plugin to act as somebody
	// the user did not configure it to be.
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "/root/sa.json")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "shhh")
	t.Setenv("XDG_CONFIG_HOME", "/root/.config")
	t.Setenv("XDG_STATE_HOME", "/root/.local/state")
	t.Setenv(PathEnv, "/root/plugins")
	t.Setenv("DRIVEL_LIVE_RIG", "1")

	got := pluginEnv()
	has := func(prefix string) bool {
		return slices.ContainsFunc(got, func(kv string) bool { return strings.HasPrefix(kv, prefix) })
	}

	for _, want := range []string{"PATH=", "HOME=", "SSH_AUTH_SOCK=", "LC_TIME="} {
		if !has(want) {
			t.Errorf("%s was dropped, and a plugin needs it: %v", want, got)
		}
	}
	for _, unwanted := range []string{
		"GOOGLE_APPLICATION_CREDENTIALS=",
		"AWS_SECRET_ACCESS_KEY=",
		"XDG_CONFIG_HOME=",
		"XDG_STATE_HOME=",
		PathEnv + "=",
		"DRIVEL_LIVE_RIG=",
	} {
		if has(unwanted) {
			t.Errorf("%s reached the plugin's environment: %v", unwanted, got)
		}
	}
}

// The documented list and the enforced list have to be the same list, or the
// documentation is a second answer that drifts.
func TestEnvAllowedMatchesWhatIsPassed(t *testing.T) {
	doc := EnvAllowed()
	if !slices.IsSorted(doc) {
		t.Errorf("EnvAllowed() is not sorted: %v", doc)
	}
	if !slices.Contains(doc, "LC_*") {
		t.Error("EnvAllowed() omits the LC_* prefix rule, which pluginEnv honours")
	}
	for name := range envAllowed {
		if !slices.Contains(doc, name) {
			t.Errorf("%s is passed through but not listed by EnvAllowed()", name)
		}
	}
}
