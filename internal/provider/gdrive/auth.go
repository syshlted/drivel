// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

// Package gdrive implements provider.Store (and provider.ChangeSource) for Google Drive.
//
// All Drive traffic rides the HTTP/3-preferred transport from internal/transport,
// with OAuth folded on top: the token source wraps the transport's RoundTripper
// (see DESIGN.md §2.6), rather than being handed to the Drive client separately.
package gdrive

import (
	"context"
	"fmt"
	"net/http"

	"golang.org/x/oauth2"

	"github.com/syshlted/drivel/internal/gauth"
	"github.com/syshlted/drivel/internal/transport"
)

// buildHTTPClient constructs the OAuth-authenticated *http.Client whose transport
// is the HTTP/3→HTTP/2 fallback client. The returned func shuts down QUIC conns.
//
// It is non-interactive: it requires an already-cached token (created by
// `drivel login`). This keeps `drivel mount` from unexpectedly blocking on stdin.
func buildHTTPClient(ctx context.Context, credentialsPath, tokenPath, scope string) (*http.Client, func() error, error) {
	creds, err := gauth.LoadCredentials(credentialsPath)
	if err != nil {
		return nil, nil, fmt.Errorf("reading credentials: %w", err)
	}
	tok, err := gauth.LoadToken(tokenPath)
	if err != nil {
		return nil, nil, fmt.Errorf("no cached OAuth token at %s — run 'drivel login' first: %w", tokenPath, err)
	}

	// The HTTP/3 transport is the base for BOTH API calls and token refreshes:
	// putting it on the context makes oauth2's refresh requests use it too.
	base, closer := transport.New()

	if scope == "" {
		scope = gauth.ScopeDrive
	}
	config := creds.Config("", scope)
	client := &http.Client{
		Transport: &oauth2.Transport{
			Source: config.TokenSource(refreshContext(ctx, base), tok),
			Base:   base.Transport,
		},
	}
	return client, closer.Close, nil
}

// refreshContext is the context oauth2 keeps for the life of the token source,
// which is the life of the store and not the life of the call that opened it.
//
// Both halves of it are load-bearing, and the cancellation half is a bug fix.
// provider.Factory's context bounds *opening* the store: out of process that is
// one gRPC Open handler, and grpc-go cancels it the moment Open returns. oauth2
// holds this context for every later refresh, so inheriting the cancellation
// meant that the first API call needing a fresh access token posted to the token
// endpoint with a dead context — which is to say every mount after the hour
// following `drivel login`, in both directions, with `context canceled` as the
// only symptom. Dropping the cancellation and keeping the values is what makes
// the store's HTTP client outlive the call that built it.
//
// It must not be given a deadline either, for the same reason: this one context
// serves every refresh for the life of the mount, so a timeout would not bound a
// refresh — it would break all of them once it elapsed. What ends an in-flight
// refresh is the closer returned above, which shuts the transport's connections
// down when the store is closed.
func refreshContext(ctx context.Context, base *http.Client) context.Context {
	return context.WithValue(context.WithoutCancel(ctx), oauth2.HTTPClient, base)
}
