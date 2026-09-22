// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package gauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// WhoAmI is the step that tells the user which account they just logged in as —
// and the only place, before a mount exists, where a token is actually used. It
// is tested without touching Google: oauth2 takes its base HTTP client from the
// context (the same seam gdrive uses to put the HTTP/3 transport underneath the
// token source), so a client that rewrites the host reaches a test server with
// the real OAuth plumbing still in front of it.

type rewriteHost struct {
	base *url.URL
	next http.RoundTripper
}

func (rt rewriteHost) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.URL.Scheme, clone.URL.Host = rt.base.Scheme, rt.base.Host
	return rt.next.RoundTrip(clone)
}

func whoAmIAgainst(t *testing.T, h http.HandlerFunc) (string, error) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	base, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(t.Context(), oauth2.HTTPClient,
		&http.Client{Transport: rewriteHost{base: base, next: http.DefaultTransport}})

	// A token that is still valid, so oauth2 has no reason to go refresh it
	// against the real endpoint.
	tok := &oauth2.Token{AccessToken: "ya29.test", TokenType: "Bearer", Expiry: time.Now().Add(time.Hour)}
	return WhoAmI(ctx, Credentials{ClientID: "id-1", ClientSecret: "sec-1"}, ScopeDrive, tok)
}

func TestWhoAmIReportsTheAccount(t *testing.T) {
	var gotAuth, gotPath, gotFields string
	got, err := whoAmIAgainst(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath, gotFields = r.Header.Get("Authorization"), r.URL.Path, r.URL.Query().Get("fields")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"user":{"emailAddress":"a@example.com","displayName":"A Person"}}`))
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := "A Person <a@example.com>"; got != want {
		t.Errorf("WhoAmI = %q; want %q", got, want)
	}

	// The token has to be on the request. Attaching it twice, or not at all, is
	// the classic way to wire oauth2 and a custom transport together wrongly.
	if gotAuth != "Bearer ya29.test" {
		t.Errorf("Authorization = %q; want the bearer token", gotAuth)
	}
	if gotPath != "/drive/v3/about" {
		t.Errorf("path = %q; want Drive's about.get", gotPath)
	}
	if !strings.Contains(gotFields, "emailAddress") {
		t.Errorf("fields = %q; want it to ask for the address it reports", gotFields)
	}
}

// Not every account has a display name, and a bare address is still a useful
// answer to "which account is this?".
func TestWhoAmIFallsBackToTheAddress(t *testing.T) {
	got, err := whoAmIAgainst(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"user":{"emailAddress":"a@example.com"}}`))
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "a@example.com" {
		t.Errorf("WhoAmI = %q; want the bare address", got)
	}
}

// A token that does not work has to say so at login, while the user is still
// there to do something about it — not at the first mount.
func TestWhoAmIReportsARejectedToken(t *testing.T) {
	got, err := whoAmIAgainst(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"code":401}}`, http.StatusUnauthorized)
	})
	if err == nil {
		t.Fatalf("WhoAmI accepted a 401 and returned %q", got)
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error = %v; it should carry the status Drive returned", err)
	}
}

func TestWhoAmIReportsAnUnreadableAnswer(t *testing.T) {
	if got, err := whoAmIAgainst(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`this is not json`))
	}); err == nil {
		t.Fatalf("WhoAmI accepted a non-JSON body and returned %q", got)
	}
}
