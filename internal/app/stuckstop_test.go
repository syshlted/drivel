// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package app

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/syshlted/drivel/internal/state"
)

// A shutdown must end even when the provider has stopped answering. That
// provider fills the engine's worker queues, the run loop parks in a send, FUSE
// requests park in emit behind it — and before the shutdown deadline was armed
// from the request, a stop waited for all of that forever.
func TestAStopEndsWhileTheProviderIsNotAnswering(t *testing.T) {
	saved := engineDrainTimeout
	engineDrainTimeout = time.Second
	t.Cleanup(func() { engineDrainTimeout = saved })

	f := newFleet(t, 1, func(s *MountSpec) { s.UploadWorkers = 1 })
	c1 := f.peers[0]
	c1.faults.stallWrites()

	// Mkdirs dispatch at once, so a burst saturates the one worker's queue and
	// then the event buffer behind it; the writer then blocks inside mkdir(2),
	// which is the user's view of back-pressure.
	const dirs = 1500
	wrote := make(chan int, 1)
	go func() {
		n := 0
		for i := range dirs {
			if os.Mkdir(c1.mnt(fmt.Sprintf("d%04d", i)), 0o755) != nil {
				break
			}
			n++
		}
		wrote <- n
	}()
	if !waitFor(10*time.Second, func() bool { return c1.logw.saw("[sync] back-pressure: writes through the mount are now waiting") }) {
		t.Log("no back-pressure line seen; continuing — the stop below is the assertion")
	}

	stopped := make(chan struct{})
	go func() { c1.stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(25 * time.Second):
		t.Fatal("the mount did not stop while its provider was not answering")
	}
	select {
	case <-wrote:
	case <-time.After(5 * time.Second):
		t.Error("the writer is still blocked inside mkdir after the mount stopped")
	}

	st, err := state.Open(c1.spec.StateDB)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, ok, _ := st.Running(); !ok {
		t.Error("a stop that shed work cleared the unclean-shutdown mark; the next start would not sweep")
	}
	if owed, _ := st.RepairsOwed(); !owed {
		t.Error("nothing was recorded for repair, so what the stop shed is lost")
	}
}
