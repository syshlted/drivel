// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package config

import (
	"strings"
	"testing"
)

// twoProviders is the file a listing exists to make readable: the provider a
// mount talks to is named once, in an account table the mount refers to by key.
const twoProviders = `
[account.personal]
provider = "gdrive"
credentials = "/c.json"
token = "/t.json"
root = "root"

[account.nas]
provider = "sftp"
host = "nas.example.lan"
user = "jeremy"

[[mount]]
name = "docs"
account = "personal"
path = "./docs"
data = "./docs-data"

[mount.provider]
root = "FOLDERID"

[[mount]]
name = "backups"
account = "nas"
path = "./nas"
data = "./nas-data"

[[mount]]
name = "scratch"
path = "./scratch"
data = "./scratch-data"
`

// The account is on the Listing and not on the Spec, so this is the one place it
// can be checked at all — and it is what tells a reader which credentials a
// mount is about to use.
func TestListCarriesTheAccount(t *testing.T) {
	ls, err := load(t, twoProviders).List(Selector{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []struct{ name, account, kind string }{
		{"docs", "personal", "gdrive"},
		{"backups", "nas", "sftp"},
		{"scratch", "", ""},
	}
	if len(ls) != len(want) {
		t.Fatalf("listed %d mounts; want %d", len(ls), len(want))
	}
	for i, w := range want {
		if ls[i].Spec.Name != w.name || ls[i].Account != w.account || ls[i].Spec.Provider != w.kind {
			t.Errorf("mount %d: %q/%q/%q; want %q/%q/%q",
				i, ls[i].Spec.Name, ls[i].Account, ls[i].Spec.Provider, w.name, w.account, w.kind)
		}
	}
}

// A listing is the resolution a mount gets, which includes the account/mount
// provider merge — so a reader of `root` in a [mount.provider] table can see
// that it is the one that won.
func TestListResolvesTheProviderSettings(t *testing.T) {
	ls, err := load(t, twoProviders).List(Selector{Name: "docs"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	cfg := string(ls[0].Spec.ProviderConfig)
	if !strings.Contains(cfg, `root = "FOLDERID"`) {
		t.Errorf("the mount's own root did not override the account's: %q", cfg)
	}
	if !strings.Contains(cfg, `credentials = "/c.json"`) {
		t.Errorf("the account's credentials did not survive the merge: %q", cfg)
	}
}

// Specs, SpecsFor and List must not be three readings of one file. The first two
// are the third with a field dropped, and this is what says so.
func TestListAndSpecsAgree(t *testing.T) {
	c := load(t, twoProviders)
	ls, err := c.List(Selector{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	specs, err := c.Specs()
	if err != nil {
		t.Fatalf("Specs: %v", err)
	}
	if len(ls) != len(specs) {
		t.Fatalf("List returned %d mounts, Specs %d", len(ls), len(specs))
	}
	for i := range specs {
		if ls[i].Spec.Name != specs[i].Name || ls[i].Spec.Mountpoint != specs[i].Mountpoint ||
			ls[i].Spec.StateDB != specs[i].StateDB || ls[i].Spec.Provider != specs[i].Provider ||
			string(ls[i].Spec.ProviderConfig) != string(specs[i].ProviderConfig) {
			t.Errorf("mount %d: List says %+v; Specs says %+v", i, ls[i].Spec, specs[i])
		}
	}
}

// A listing is not a validator: it is what someone reaches for when a config
// file is misbehaving, so the cross-mount guards must not be applied here. Two
// mounts sharing a mountpoint is one app.Validate refuses; List prints it.
func TestListDoesNotApplyTheCrossMountGuards(t *testing.T) {
	const clashing = `
[[mount]]
name = "a"
path = "./same"
data = "./a-data"

[[mount]]
name = "b"
path = "./same"
data = "./b-data"
`
	ls, err := load(t, clashing).List(Selector{})
	if err != nil {
		t.Fatalf("List refused a config that app.Validate would reject: %v", err)
	}
	if len(ls) != 2 {
		t.Fatalf("listed %d mounts; want both", len(ls))
	}
}
