// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
)

// nopStore is a Store that does nothing; the registry only ever hands it back.
type nopStore struct{ tag string }

func (nopStore) Put(context.Context, string, io.Reader) (RemoteFile, error) {
	return RemoteFile{}, nil
}
func (nopStore) Mkdir(context.Context, string) (RemoteFile, error) { return RemoteFile{}, nil }
func (nopStore) Move(context.Context, string, string) (RemoteFile, error) {
	return RemoteFile{}, nil
}
func (nopStore) Remove(context.Context, string) error { return nil }
func (nopStore) Get(context.Context, string) (io.ReadCloser, error) {
	return nil, errors.New("nop")
}
func (nopStore) Stat(context.Context, string) (RemoteFile, bool, error) {
	return RemoteFile{}, false, nil
}

// tagged is what factoryFor decodes into: one key, so a test can prove which
// config reached which factory.
type tagged struct {
	Tag string `toml:"tag"`
}

// factoryFor returns a Factory that decodes its settings and tags the store with
// the result.
func factoryFor(tag string) Factory {
	return func(_ context.Context, p Params) (Store, error) {
		var into tagged
		if err := p.Config.Decode(&into); err != nil {
			return nil, err
		}
		return nopStore{tag: tag + ":" + into.Tag}, nil
	}
}

// configWith builds the settings a factoryFor store expects.
func configWith(tag string) Config { return MustEncodeConfig(tagged{Tag: tag}) }

func TestRegistryOpenPassesConfigThrough(t *testing.T) {
	r := NewRegistry()
	if err := r.Register("alpha", factoryFor("alpha")); err != nil {
		t.Fatalf("Register: %v", err)
	}
	s, err := r.Open(context.Background(), "alpha", Params{Config: configWith("cfg")})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got := s.(nopStore).tag; got != "alpha:cfg" {
		t.Errorf("tag = %q; want %q — the factory did not receive its config", got, "alpha:cfg")
	}
}

// The M8 seam proof in miniature: one factory, two names, two independent
// stores, each with its own config. This is what lets a test mount two
// separately-configured Drive stores in one process without shipping a
// pseudo-provider in the binary.
func TestRegistrySameFactoryUnderTwoNames(t *testing.T) {
	r := NewRegistry()
	f := factoryFor("drive")
	if err := r.Register("drive-a", f); err != nil {
		t.Fatalf("Register a: %v", err)
	}
	if err := r.Register("drive-b", f); err != nil {
		t.Fatalf("Register b: %v", err)
	}
	a, err := r.Open(context.Background(), "drive-a", Params{Config: configWith("account-a")})
	if err != nil {
		t.Fatalf("Open a: %v", err)
	}
	b, err := r.Open(context.Background(), "drive-b", Params{Config: configWith("account-b")})
	if err != nil {
		t.Fatalf("Open b: %v", err)
	}
	if a.(nopStore).tag == b.(nopStore).tag {
		t.Fatalf("both stores got the same config %q; they must be independent", a.(nopStore).tag)
	}
}

func TestRegistryRejectsDuplicate(t *testing.T) {
	r := NewRegistry()
	if err := r.Register("dup", factoryFor("first")); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	err := r.Register("dup", factoryFor("second"))
	if err == nil {
		t.Fatal("duplicate Register succeeded; one factory would be silently unreachable")
	}
	// The first registration must survive: a rejected duplicate that still
	// overwrote would be worse than allowing it.
	s, err := r.Open(context.Background(), "dup", Params{Config: configWith("x")})
	if err != nil {
		t.Fatalf("Open after rejected duplicate: %v", err)
	}
	if !strings.HasPrefix(s.(nopStore).tag, "first:") {
		t.Errorf("tag = %q; the rejected duplicate replaced the original", s.(nopStore).tag)
	}
}

func TestRegistryRejectsBadRegistration(t *testing.T) {
	r := NewRegistry()
	if err := r.Register("", factoryFor("x")); err == nil {
		t.Error("registering an empty kind succeeded")
	}
	if err := r.Register("nilf", nil); err == nil {
		t.Error("registering a nil factory succeeded")
	}
}

func TestRegistryUnknownKind(t *testing.T) {
	r := NewRegistry()
	_, err := r.Open(context.Background(), "nope", Params{})
	if !errors.Is(err, ErrUnknownKind) {
		t.Fatalf("err = %v; want it to wrap ErrUnknownKind", err)
	}
	if !strings.Contains(err.Error(), "no providers available") {
		t.Errorf("empty-registry error should say so: %v", err)
	}

	_ = r.Register("gdrive", factoryFor("gdrive"))
	_ = r.Register("alpha", factoryFor("alpha"))
	_, err = r.Open(context.Background(), "gdrve", Params{})
	if !errors.Is(err, ErrUnknownKind) {
		t.Fatalf("err = %v; want ErrUnknownKind", err)
	}
	// A typo in a hand-written config is the overwhelmingly likely cause, so the
	// error has to name what was available.
	if !strings.Contains(err.Error(), "alpha, gdrive") {
		t.Errorf("error should list available kinds sorted: %v", err)
	}
}

// Since a backend became something installed rather than compiled in, "unknown
// kind" is accurate and unactionable on its own: the user has to be told where
// to put one. The registry must not learn what a plugin is, so the layer that
// does contributes the sentence.
func TestRegistryUnknownKindCarriesTheHint(t *testing.T) {
	r := NewRegistry()
	r.Hint(func(kind string) string { return "no drivel-provider-" + kind + " in /somewhere" })
	_ = r.Register("alpha", factoryFor("alpha"))

	_, err := r.Open(context.Background(), "beta", Params{})
	if !errors.Is(err, ErrUnknownKind) {
		t.Fatalf("err = %v; want ErrUnknownKind", err)
	}
	if !strings.Contains(err.Error(), "no drivel-provider-beta in /somewhere") {
		t.Errorf("the hint did not reach the error: %v", err)
	}

	// An empty hint must add nothing rather than a dangling separator.
	r.Hint(func(string) string { return "" })
	_, err = r.Open(context.Background(), "beta", Params{})
	if strings.HasSuffix(err.Error(), ";") || strings.Contains(err.Error(), "; ") {
		t.Errorf("an empty hint left a separator behind: %v", err)
	}
}

// A Factory never has to nil-check what it was handed: Open substitutes a no-op
// decode and the default logger, so the zero Params is usable.
func TestRegistryZeroParamsIsSafe(t *testing.T) {
	r := NewRegistry()
	called := false
	_ = r.Register("k", func(_ context.Context, p Params) (Store, error) {
		called = true
		var into tagged
		if err := p.Config.Decode(&into); err != nil {
			return nil, err
		}
		if into.Tag != "" {
			return nil, fmt.Errorf("decoding an empty config wrote %q", into.Tag)
		}
		if p.Log == nil {
			return nil, errors.New("Log is nil; every provider logs unconditionally")
		}
		return nopStore{}, nil
	})
	if _, err := r.Open(context.Background(), "k", Params{}); err != nil {
		t.Fatalf("Open with nil decode: %v", err)
	}
	if !called {
		t.Error("factory was never called")
	}
}

func TestRegistryRejectsNilStore(t *testing.T) {
	r := NewRegistry()
	_ = r.Register("liar", func(context.Context, Params) (Store, error) {
		// A nil Store with a nil error — the shape being tested.
		return nil, nil
	})
	_, err := r.Open(context.Background(), "liar", Params{})
	if err == nil {
		t.Fatal("a nil Store with no error was accepted; it would panic on first push")
	}
}

func TestRegistryFactoryErrorNamesKind(t *testing.T) {
	r := NewRegistry()
	sentinel := errors.New("boom")
	_ = r.Register("kaboom", func(context.Context, Params) (Store, error) {
		return nil, sentinel
	})
	_, err := r.Open(context.Background(), "kaboom", Params{})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v; want it to wrap the factory's error", err)
	}
	if !strings.Contains(err.Error(), "kaboom") {
		t.Errorf("error should name the provider kind: %v", err)
	}
}

func TestRegistryKindsSorted(t *testing.T) {
	r := NewRegistry()
	for _, k := range []string{"zeta", "alpha", "mu"} {
		_ = r.Register(k, factoryFor(k))
	}
	got := r.Kinds()
	want := []string{"alpha", "mu", "zeta"}
	if len(got) != len(want) {
		t.Fatalf("Kinds() = %v; want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Kinds() = %v; want %v", got, want)
		}
	}
}

// Register and Open race in principle even though startup serialises them in
// practice; -race would find a missing lock here.
func TestRegistryConcurrent(t *testing.T) {
	r := NewRegistry()
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(2)
		go func() { defer wg.Done(); _ = r.Register(fmt.Sprintf("k%d", i), factoryFor("x")) }()
		go func() { defer wg.Done(); _, _ = r.Open(context.Background(), "k0", Params{}); _ = r.Kinds() }()
	}
	wg.Wait()
	if len(r.Kinds()) != 8 {
		t.Errorf("Kinds() = %d; want 8", len(r.Kinds()))
	}
}

func TestEncodeConfigRoundTrips(t *testing.T) {
	type driveish struct {
		Root string `toml:"root"`
	}

	var got driveish
	if err := MustEncodeConfig(driveish{Root: "abc"}).Decode(&got); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.Root != "abc" {
		t.Errorf("Root = %q; want abc", got.Root)
	}
}

// The flag path and the config-file path build the same settings two different
// ways, and the tags are what makes them agree. A struct encoded here has to
// decode the same as the equivalent hand-written table.
func TestEncodeConfigMatchesHandWrittenTOML(t *testing.T) {
	type driveish struct {
		Root  string `toml:"root"`
		Sweep string `toml:"sweep-mode"`
	}

	var fromStruct, fromText driveish
	if err := MustEncodeConfig(driveish{Root: "abc", Sweep: "scoped"}).Decode(&fromStruct); err != nil {
		t.Fatalf("Decode(struct): %v", err)
	}
	if err := Config("root = \"abc\"\nsweep-mode = \"scoped\"\n").Decode(&fromText); err != nil {
		t.Fatalf("Decode(text): %v", err)
	}
	if fromStruct != fromText {
		t.Errorf("struct path gave %+v, config-file path gave %+v", fromStruct, fromText)
	}
}

// M8 rule 6: a key the provider does not define is an error, not a shrug. The
// message has to name every offender, since someone fixing a config file would
// rather see all of them than find them one run at a time.
func TestConfigDecodeRejectsUnknownKeys(t *testing.T) {
	type driveish struct {
		Root string `toml:"root"`
	}
	var got driveish
	err := Config("root = \"abc\"\nlazzy = true\nnonsense = 1\n").Decode(&got)
	if err == nil {
		t.Fatal("unknown keys accepted")
	}
	for _, want := range []string{"lazzy", "nonsense"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name %q: %v", want, err)
		}
	}
}

// The zero Config is what a mount with no settings for its kind produces, and a
// Factory must be able to call Decode on it without checking first.
func TestZeroConfigDecodesToZeroValue(t *testing.T) {
	type driveish struct {
		Root string `toml:"root"`
	}
	var got driveish
	if err := (Config(nil)).Decode(&got); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.Root != "" {
		t.Errorf("Root = %q; want empty", got.Root)
	}
}

// Config is a []byte, so an ordinary %v would print a provider's whole
// configuration into a log file. It must print the shape and not the values.
func TestConfigStringHidesValues(t *testing.T) {
	c := Config("token = \"s3cret\"\nroot = \"abc\"\n")
	got := fmt.Sprintf("%v", c)
	if strings.Contains(got, "s3cret") {
		t.Errorf("String leaked a value: %s", got)
	}
	if !strings.Contains(got, "token") || !strings.Contains(got, "root") {
		t.Errorf("String should name the keys: %s", got)
	}
}
