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

// factoryFor returns a Factory that decodes into a *string and tags the store
// with the result, so a test can prove which config reached which factory.
func factoryFor(tag string) Factory {
	return func(_ context.Context, p Params) (Store, error) {
		var into string
		if err := p.Decode(&into); err != nil {
			return nil, err
		}
		return nopStore{tag: tag + ":" + into}, nil
	}
}

func decodeString(v string) func(any) error {
	return func(dst any) error {
		p, ok := dst.(*string)
		if !ok {
			return fmt.Errorf("want *string, got %T", dst)
		}
		*p = v
		return nil
	}
}

func TestRegistryOpenPassesConfigThrough(t *testing.T) {
	r := NewRegistry()
	if err := r.Register("alpha", factoryFor("alpha")); err != nil {
		t.Fatalf("Register: %v", err)
	}
	s, err := r.Open(context.Background(), "alpha", Params{Decode: decodeString("cfg")})
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
	a, err := r.Open(context.Background(), "drive-a", Params{Decode: decodeString("account-a")})
	if err != nil {
		t.Fatalf("Open a: %v", err)
	}
	b, err := r.Open(context.Background(), "drive-b", Params{Decode: decodeString("account-b")})
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
	s, err := r.Open(context.Background(), "dup", Params{Decode: decodeString("x")})
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
	if !strings.Contains(err.Error(), "no providers registered") {
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
		t.Errorf("error should list known kinds sorted: %v", err)
	}
}

// A Factory never has to nil-check what it was handed: Open substitutes a no-op
// decode and the default logger, so the zero Params is usable.
func TestRegistryZeroParamsIsSafe(t *testing.T) {
	r := NewRegistry()
	called := false
	_ = r.Register("k", func(_ context.Context, p Params) (Store, error) {
		called = true
		var s string
		if err := p.Decode(&s); err != nil {
			return nil, err
		}
		if s != "" {
			return nil, fmt.Errorf("no-op decode wrote %q", s)
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

func TestStaticDecoder(t *testing.T) {
	type driveish struct{ Root string }

	var got driveish
	if err := StaticDecoder(driveish{Root: "abc"})(&got); err != nil {
		t.Fatalf("StaticDecoder: %v", err)
	}
	if got.Root != "abc" {
		t.Errorf("Root = %q; want abc", got.Root)
	}

	// A provider decoding into a type nobody configured is a wiring mistake, and
	// the error has to name both sides or it is unactionable.
	err := StaticDecoder(driveish{})(new(string))
	if err == nil {
		t.Fatal("mismatched destination accepted")
	}
	if !strings.Contains(err.Error(), "driveish") || !strings.Contains(err.Error(), "string") {
		t.Errorf("error should name both types: %v", err)
	}

	if err := StaticDecoder(driveish{})(driveish{}); err == nil {
		t.Error("non-pointer destination accepted")
	}
	var nilp *driveish
	if err := StaticDecoder(driveish{})(nilp); err == nil {
		t.Error("nil pointer destination accepted")
	}
}
