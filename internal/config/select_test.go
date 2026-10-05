// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package config

import (
	"strings"
	"testing"
)

// fleet is three mounts over two accounts, which is the shape that makes -name
// and -account differ: "personal" owns two of them, so selecting by account is
// not the same question as selecting by name.
const fleet = `
[account.personal]
provider = "gdrive"
credentials = "/c.json"
token = "/t.json"

[account.work]
provider = "gdrive"
credentials = "/c.json"
token = "/t.json"

[[mount]]
name = "docs"
account = "personal"
path = "./docs"
data = "./docs-data"

[[mount]]
name = "photos"
account = "personal"
path = "./photos"
data = "./photos-data"

[[mount]]
name = "work"
account = "work"
path = "./work"
data = "./work-data"
`

func TestSelectorByName(t *testing.T) {
	c := load(t, fleet)
	specs, err := c.SpecsFor(Selector{Name: "photos"})
	if err != nil {
		t.Fatalf("SpecsFor: %v", err)
	}
	if len(specs) != 1 || specs[0].Name != "photos" {
		t.Fatalf("got %d specs %v; want just photos", len(specs), specs)
	}
}

// An account selects every mount that uses it, which is the whole reason it is a
// separate flag from -name.
func TestSelectorByAccount(t *testing.T) {
	c := load(t, fleet)
	specs, err := c.SpecsFor(Selector{Account: "personal"})
	if err != nil {
		t.Fatalf("SpecsFor: %v", err)
	}
	var got []string
	for _, s := range specs {
		got = append(got, s.Name)
	}
	if len(got) != 2 || got[0] != "docs" || got[1] != "photos" {
		t.Fatalf("got %v; want [docs photos] in file order", got)
	}
}

// The zero selector is what `drivel mount` with no selector passes, and it must
// stay indistinguishable from Specs.
func TestEmptySelectorServesEverything(t *testing.T) {
	c := load(t, fleet)
	all, err := c.Specs()
	if err != nil {
		t.Fatalf("Specs: %v", err)
	}
	specs, err := c.SpecsFor(Selector{})
	if err != nil {
		t.Fatalf("SpecsFor: %v", err)
	}
	if len(specs) != len(all) {
		t.Fatalf("got %d specs; want the %d Specs returns", len(specs), len(all))
	}
}

// A mistyped selector must be an error naming what the file does define. Serving
// nothing would exit successfully having mounted nothing at all.
func TestSelectorThatMatchesNothingIsAnError(t *testing.T) {
	c := load(t, fleet)
	for _, tc := range []struct {
		label string
		sel   Selector
		want  []string
	}{
		{"name", Selector{Name: "photoss"}, []string{`"photoss"`, "docs, photos, work"}},
		{"account", Selector{Account: "personall"}, []string{`"personall"`, "personal, work"}},
	} {
		t.Run(tc.label, func(t *testing.T) {
			_, err := c.SpecsFor(tc.sel)
			if err == nil {
				t.Fatal("a selector matching nothing was accepted")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error should mention %s: %v", want, err)
				}
			}
		})
	}
}

// An account with no mount of its own is a different mistake from a mistyped
// one — a missing [[mount]] rather than a typo — so it gets its own message.
func TestSelectorNamesAnUnusedAccount(t *testing.T) {
	c := load(t, `
[account.spare]
provider = "gdrive"
credentials = "/c.json"
token = "/t.json"

[[mount]]
name = "one"
path = "./one"
data = "./one-data"
`)
	_, err := c.SpecsFor(Selector{Account: "spare"})
	if err == nil {
		t.Fatal("an account with no mount was accepted")
	}
	if !strings.Contains(err.Error(), "no [[mount]] uses it") {
		t.Errorf("error should say the account has no mount: %v", err)
	}
}

func TestSelectorRefusesBothFields(t *testing.T) {
	c := load(t, fleet)
	if _, err := c.SpecsFor(Selector{Name: "photos", Account: "personal"}); err == nil {
		t.Fatal("a selector naming both a mount and an account was accepted")
	}
}

// -name has to match the name the logs print, so it matches on the same
// fallbacks Specs applies: the account name, then the mountpoint's basename.
func TestSelectorMatchesDerivedNames(t *testing.T) {
	c := load(t, `
[account.personal]
provider = "gdrive"
credentials = "/c.json"
token = "/t.json"

[[mount]]
account = "personal"
path = "./drive"
data = "./drive-data"

[[mount]]
path = "./scratch"
data = "./scratch-data"
`)
	for _, want := range []string{"personal", "scratch"} {
		specs, err := c.SpecsFor(Selector{Name: want})
		if err != nil {
			t.Fatalf("SpecsFor(%s): %v", want, err)
		}
		if len(specs) != 1 || specs[0].Name != want {
			t.Fatalf("-name %s selected %v", want, specs)
		}
	}
}

// Filtering happens before resolution, so a broken entry nobody selected cannot
// keep the one they did select from coming up. The unselected entry is genuinely
// broken: Specs must reject the same file.
func TestSelectorSkipsAnUnselectedBrokenEntry(t *testing.T) {
	c := load(t, `
[[mount]]
name = "good"
path = "./good"
data = "./good-data"

[[mount]]
name = "bad"
account = "missing"
path = "./bad"
data = "./bad-data"
`)
	if _, err := c.Specs(); err == nil {
		t.Fatal("Specs accepted a mount naming an undefined account")
	}
	specs, err := c.SpecsFor(Selector{Name: "good"})
	if err != nil {
		t.Fatalf("a broken sibling entry blocked an unrelated selection: %v", err)
	}
	if len(specs) != 1 || specs[0].Name != "good" {
		t.Fatalf("got %v; want just good", specs)
	}
}

// ...but selecting the broken entry itself still reports it.
func TestSelectorReportsASelectedBrokenEntry(t *testing.T) {
	c := load(t, `
[[mount]]
name = "bad"
account = "missing"
path = "./bad"
data = "./bad-data"
`)
	_, err := c.SpecsFor(Selector{Name: "bad"})
	if err == nil {
		t.Fatal("a selected entry naming an undefined account was accepted")
	}
	if !strings.Contains(err.Error(), "missing") {
		t.Errorf("error should name the undefined account: %v", err)
	}
}
