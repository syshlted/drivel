// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package syncengine

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

type retryableErr struct{}

func (retryableErr) Error() string   { return "503" }
func (retryableErr) Retryable() bool { return true }

// Observe is the entry point for outcomes seen outside this package (lazy
// hydration), so it has to classify exactly as the internal call sites do: an
// answer of any kind is contact, only a retryable failure is against it, and a
// cancellation is ours and says nothing.
func TestObserveClassifiesOutcomes(t *testing.T) {
	m := NewMonitor("work")
	if got := m.Snapshot().Reachability; got != ReachUnknown {
		t.Fatalf("before any traffic: %s; want %s", got, ReachUnknown)
	}

	m.Observe(fmt.Errorf("fetch: %w", retryableErr{}))
	if s := m.Snapshot(); s.Reachability != ReachOffline || s.ConsecutiveFails != 1 {
		t.Errorf("after a retryable failure: %s, %d fails; want %s, 1", s.Reachability, s.ConsecutiveFails, ReachOffline)
	}

	m.Observe(context.Canceled)
	if s := m.Snapshot(); s.Reachability != ReachOffline || s.ConsecutiveFails != 1 {
		t.Errorf("a cancellation changed reachability: %s, %d fails", s.Reachability, s.ConsecutiveFails)
	}

	m.Observe(errors.New("permission denied"))
	if s := m.Snapshot(); s.Reachability != ReachOnline || s.ConsecutiveFails != 0 {
		t.Errorf("after a non-retryable answer: %s, %d fails; want %s, 0", s.Reachability, s.ConsecutiveFails, ReachOnline)
	}

	m.Observe(retryableErr{})
	m.Observe(nil)
	if got := m.Snapshot().Reachability; got != ReachOnline {
		t.Errorf("after a success: %s; want %s", got, ReachOnline)
	}

	var nilMon *Monitor
	nilMon.Observe(nil) // must not panic
}
