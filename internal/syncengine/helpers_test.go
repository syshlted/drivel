// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package syncengine

import (
	"testing"
	"time"
)

// workerFor must be deterministic (same path → same worker, so a path's ops stay
// ordered on one worker) and always in range.
func TestWorkerForDeterministicAndInRange(t *testing.T) {
	const n = 4
	paths := []string{"a.txt", "sub/b.txt", "sub/deep/c.txt", "", "d", "a.txt"}
	for _, p := range paths {
		w1 := workerFor(p, n)
		w2 := workerFor(p, n)
		if w1 != w2 {
			t.Errorf("workerFor(%q) not deterministic: %d vs %d", p, w1, w2)
		}
		if w1 < 0 || w1 >= n {
			t.Errorf("workerFor(%q) = %d out of range [0,%d)", p, w1, n)
		}
	}
}

// jitter stays within [d/2, d] and collapses to 0 for a non-positive base.
func TestJitterBounds(t *testing.T) {
	const d = 800 * time.Millisecond
	for i := 0; i < 1000; i++ {
		got := jitter(d)
		if got < d/2 || got > d {
			t.Fatalf("jitter(%v) = %v; want within [%v, %v]", d, got, d/2, d)
		}
	}
	if jitter(0) != 0 {
		t.Fatal("jitter(0) should be 0")
	}
}
