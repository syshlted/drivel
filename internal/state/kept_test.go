// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package state

import (
	"testing"
	"time"
)

// A kept record costs one commit when it changes and none when it does not,
// and lasts only until the path's next echo or its removal.
func TestKeptIsCheapAndDischarged(t *testing.T) {
	s := openTestStore(t)
	if err := s.SetKept("a.txt", "v2"); err != nil {
		t.Fatal(err)
	}
	before := s.Commits()
	if err := s.SetKept("a.txt", "v2"); err != nil {
		t.Fatal(err)
	}
	if got := s.Commits() - before; got != 0 {
		t.Errorf("re-recording the same version cost %d commit(s); want 0", got)
	}
	if id, ok, _ := s.Kept("a.txt"); !ok || id != "v2" {
		t.Errorf("Kept = %q, %v; want v2", id, ok)
	}

	if err := s.SetEcho("a.txt", Echo{Version: "v3", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Kept("a.txt"); ok {
		t.Error("an echo did not discharge the kept record")
	}

	_ = s.SetKept("b.txt", "v1")
	if err := s.ForgetMany([]string{"b.txt"}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Kept("b.txt"); ok {
		t.Error("forgetting the path did not discharge the kept record")
	}
}
