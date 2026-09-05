package gauth

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// token.json is the file `drivel mount` cannot start without, and the one that
// holds a standing credential for the user's whole Drive.

// Every field oauth2 needs to keep using the account has to survive the trip,
// the expiry included: a token that comes back looking valid when it is not
// makes the first API call of every mount fail instead of refreshing.
func TestTokenRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token.json")
	want := &oauth2.Token{
		AccessToken:  "ya29.access",
		TokenType:    "Bearer",
		RefreshToken: "1//refresh",
		Expiry:       time.Date(2031, 2, 3, 4, 5, 6, 700000000, time.UTC),
	}
	if err := SaveToken(path, want); err != nil {
		t.Fatal(err)
	}

	got, err := LoadToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != want.AccessToken || got.TokenType != want.TokenType || got.RefreshToken != want.RefreshToken {
		t.Fatalf("got %+v; want %+v", got, want)
	}
	if !got.Expiry.Equal(want.Expiry) {
		t.Fatalf("Expiry = %v; want %v", got.Expiry, want.Expiry)
	}
	if !got.Valid() {
		t.Error("a token with an expiry in the future came back invalid")
	}
}

// The other side of the same field: a saved-expired token must come back
// expired, so the refresh happens instead of a 401 the user has to interpret.
func TestExpiredTokenStaysExpired(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token.json")
	if err := SaveToken(path, &oauth2.Token{
		AccessToken:  "stale",
		RefreshToken: "1//refresh",
		Expiry:       time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := LoadToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Valid() {
		t.Error("a token saved past its expiry came back valid")
	}
	if got.RefreshToken == "" {
		t.Error("the refresh token was dropped; nothing could renew this")
	}
}

// A refresh token is a standing credential. It must be 0600 even when the file
// was already there with a wider mode — restored from a backup, copied between
// machines, or written by a build that did not tighten it.
func TestSaveTokenIsAlwaysPrivate(t *testing.T) {
	dir := t.TempDir()

	fresh := filepath.Join(dir, "fresh.json")
	if err := SaveToken(fresh, &oauth2.Token{AccessToken: "a"}); err != nil {
		t.Fatal(err)
	}
	assertMode600(t, fresh)

	wide := writeFile(t, dir, "wide.json", `{"access_token":"old"}`, 0o644)
	if err := SaveToken(wide, &oauth2.Token{AccessToken: "a"}); err != nil {
		t.Fatal(err)
	}
	assertMode600(t, wide)
}

// Refreshes rewrite this file for the life of the mount, and each new token is
// usually shorter than the last. A tail left over from the previous write would
// make the file undecodable at the next start.
func TestSaveTokenTruncates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token.json")
	if err := SaveToken(path, &oauth2.Token{AccessToken: strings.Repeat("x", 8192)}); err != nil {
		t.Fatal(err)
	}
	if err := SaveToken(path, &oauth2.Token{AccessToken: "short"}); err != nil {
		t.Fatal(err)
	}

	got, err := LoadToken(path)
	if err != nil {
		t.Fatalf("re-reading the rewritten token: %v", err)
	}
	if got.AccessToken != "short" {
		t.Fatalf("AccessToken = %q; want the token written last", got.AccessToken)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > 1024 {
		t.Errorf("the rewritten file is %d bytes; the previous token was not truncated away", len(b))
	}
}

// A corrupt token.json must name itself. The user's next move is to delete it
// and run `drivel login` again, and they can only do that if the error says
// which file it is.
func TestLoadTokenRejectsGarbage(t *testing.T) {
	dir := t.TempDir()
	for _, tt := range []struct{ name, body string }{
		{"empty", ""},
		{"not json", "?!"},
		{"truncated", `{"access_token":"ya29`},
		{"json but not an object", `["access_token"]`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := writeFile(t, dir, tt.name+".json", tt.body, 0o600)
			got, err := LoadToken(path)
			if err == nil {
				t.Fatalf("LoadToken accepted %s and returned %+v", tt.name, got)
			}
			if !strings.Contains(err.Error(), path) {
				t.Errorf("error does not name the file: %v", err)
			}
		})
	}
}

// "Never logged in" has to stay distinguishable from "logged in, file broken":
// gdrive turns the first into "run 'drivel login' first", and mount is
// non-interactive, so this is the only guidance the user gets.
func TestLoadTokenOnAMissingFileIsNotExist(t *testing.T) {
	_, err := LoadToken(filepath.Join(t.TempDir(), "absent.json"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v; want it to wrap fs.ErrNotExist", err)
	}
}

func TestSaveTokenReportsAnUnwritableDestination(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no-such-dir", "token.json")
	if err := SaveToken(path, &oauth2.Token{AccessToken: "a"}); err == nil {
		t.Fatal("writing into a directory that does not exist reported success")
	}
}
