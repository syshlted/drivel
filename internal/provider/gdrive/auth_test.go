// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package gdrive

import (
	"context"
	"testing"

	"golang.org/x/oauth2"

	"github.com/syshlted/drivel/internal/transport"
)

// oauth2 holds one context for the life of a token source, which is the life of
// the store — so the context the store is BUILT with cannot be the one it keeps.
//
// Out of process (M9, and the default build since M23) a Factory runs inside the
// plugin's gRPC Open handler, and grpc-go cancels that context the moment the
// handler returns. A token source that inherited it refreshed against a dead
// context on the first API call that needed a fresh access token — which is
// every mount outside the hour after `drivel login`, inbound and outbound both,
// reporting only "context canceled". The three assertions below are the three
// ways to get this wrong.
func TestRefreshContextOutlivesTheCallThatBuiltTheStore(t *testing.T) {
	base, closer := transport.New()
	t.Cleanup(func() { _ = closer.Close() })

	open, cancel := context.WithCancel(context.Background())
	refresh := refreshContext(open, base)

	cancel() // out of process: grpc-go retiring the Open handler

	if err := refresh.Err(); err != nil {
		t.Errorf("the refresh context inherited the open call's cancellation: %v", err)
	}
	// A deadline is the same bug with a timer: this context serves every refresh
	// for the life of the mount, so one would not bound a refresh — it would
	// break all of them once it elapsed.
	if deadline, ok := refresh.Deadline(); ok {
		t.Errorf("the refresh context carries a deadline (%s); it serves every refresh this mount makes", deadline)
	}
	// And it still has to route refreshes over the HTTP/3 transport, which is
	// what oauth2 reads off the context rather than off the client.
	if got := refresh.Value(oauth2.HTTPClient); got != base {
		t.Errorf("oauth2.HTTPClient = %v, want the transport's own client; refreshes would go over http.DefaultClient", got)
	}
}
