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

	"github.com/zishmusic/drivel/internal/gauth"
	"github.com/zishmusic/drivel/internal/transport"
)

// buildHTTPClient constructs the OAuth-authenticated *http.Client whose transport
// is the HTTP/3→HTTP/2 fallback client. The returned func shuts down QUIC conns.
//
// It is non-interactive: it requires an already-cached token (created by
// `drivel login`). This keeps `drivel mount` from unexpectedly blocking on stdin.
func buildHTTPClient(ctx context.Context, credentialsPath, tokenPath string) (*http.Client, func() error, error) {
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
	ctx = context.WithValue(ctx, oauth2.HTTPClient, base)

	config := creds.Config("", gauth.ScopeDrive)
	client := &http.Client{
		Transport: &oauth2.Transport{
			Source: config.TokenSource(ctx, tok),
			Base:   base.Transport,
		},
	}
	return client, closer.Close, nil
}
