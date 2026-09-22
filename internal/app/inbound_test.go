// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package app

import (
	"context"
	"testing"

	"github.com/zishmusic/drivel/provider"
)

// A store needs a change feed OR the ability to enumerate to sync inbound, not
// both. Drive has both; every filesystem backend in the M17–M21 group (SFTP,
// WebDAV, a plain directory) has only the sweep, and for those the sweep is the
// whole inbound path rather than a backstop under a feed.
//
// Wiring the downloader on ChangeSource alone was silently upload-only for such a
// provider: no sweep, no reconcile, and nothing in the log to say so.

// enumOnlyStore can enumerate but cannot tail — an SFTP-shaped provider.
type enumOnlyStore struct {
	*fakeStore
}

func (e enumOnlyStore) Enumerate(context.Context, string) ([]provider.RemoteFile, string, error) {
	return nil, "", nil
}

// feedOnlyStore can tail but cannot enumerate — the pre-M7b shape.
type feedOnlyStore struct {
	*fakeStore
}

func (f feedOnlyStore) StartCursor(context.Context) (string, error) { return "t", nil }

func (f feedOnlyStore) Changes(_ context.Context, c string) ([]provider.RemoteChange, string, error) {
	return nil, c, nil
}

func TestInboundSyncIsWiredForEitherCapability(t *testing.T) {
	for name, store := range map[string]provider.Store{
		"enumerate only":   enumOnlyStore{newFakeStore()},
		"change feed only": feedOnlyStore{newFakeStore()},
	} {
		t.Run(name, func(t *testing.T) {
			spec := baseSpec(t)
			spec.Provider = "fake"
			m, err := Open(t.Context(), spec, registryWith(t, "fake", store))
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer m.Close() //nolint:errcheck // test
			if m.down == nil {
				t.Error("no downloader: this mount would never sync inbound")
			}
		})
	}
}

// A store with neither capability gets no downloader at all, which is right: it
// is outbound-only, and constructing a loop with nothing to do would only add a
// goroutine and a misleading log line.
func TestInboundSyncIsAbsentWithNeitherCapability(t *testing.T) {
	spec := baseSpec(t)
	spec.Provider = "fake"
	m, err := Open(t.Context(), spec, registryWith(t, "fake", newFakeStore()))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer m.Close() //nolint:errcheck // test
	if m.down != nil {
		t.Error("a store that can neither tail nor enumerate was given a downloader")
	}
}
