// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/syshlted/drivel/internal/testenv"
)

// The whole path, once, through one provider: an initial sweep materialises what
// was already on the remote, a write through the mount is pushed, a remote change
// is pulled into the backing dir, and the pulled file is NOT pushed back.
//
// Every layer has its own tests against its own fake. This is the one that
// crosses the seams, which is where the echo model (§4) is supposed to hold and
// where it was previously only ever tested in halves (DESIGN.md §9, M0 item 4).
func TestEndToEndPushPullReconcile(t *testing.T) {
	if _, err := os.Stat("/dev/fuse"); err != nil {
		testenv.Unavailable(t, testenv.FUSE, "no /dev/fuse: "+err.Error())
	}

	store := newMemStore()
	// Content that existed before drivel ever ran. The change feed starts at
	// "now", so nothing but an enumeration sweep can discover it (M7b).
	store.seed("existing.txt", []byte("was here first"))

	spec := baseSpec(t)
	spec.Provider = "mem"
	spec.Materialize = true // eager mode: materialising is a real download

	m, err := Open(t.Context(), spec, registryWith(t, "mem", store))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer m.Close() //nolint:errcheck // best effort in cleanup

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Errorf("Run did not return; try: fusermount3 -u %s", spec.Mountpoint)
		}
	}()

	probe := ".mount-probe"
	if err := os.WriteFile(filepath.Join(spec.DataDir, probe), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !waitFor(10*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(spec.Mountpoint, probe))
		return err == nil
	}) {
		testenv.Unavailable(t, testenv.FUSE, "mount did not come up within 10s")
	}

	// 1. Reconcile: the sweep finds what the change feed never reported.
	if !waitFor(20*time.Second, func() bool {
		b, err := os.ReadFile(filepath.Join(spec.DataDir, "existing.txt"))
		return err == nil && string(b) == "was here first"
	}) {
		t.Fatal("the initial sweep did not materialise pre-existing remote content")
	}

	// 2. Push: a write through the mount reaches the provider.
	if err := os.WriteFile(filepath.Join(spec.Mountpoint, "local.txt"), []byte("written locally"), 0o644); err != nil {
		t.Fatalf("write through the mount: %v", err)
	}
	if !waitFor(20*time.Second, func() bool {
		b, ok := store.content("local.txt")
		return ok && string(b) == "written locally"
	}) {
		t.Fatal("a local write never reached the provider")
	}

	// 3. Pull: a change made on the remote lands in the backing dir.
	if _, err := store.Put(t.Context(), "remote.txt", strings.NewReader("written remotely")); err != nil {
		t.Fatalf("remote Put: %v", err)
	}
	if !waitFor(20*time.Second, func() bool {
		b, err := os.ReadFile(filepath.Join(spec.DataDir, "remote.txt"))
		return err == nil && string(b) == "written remotely"
	}) {
		t.Fatal("a remote change never reached the backing dir")
	}
	// ...and is visible through the mount, not merely underneath it.
	if b, err := os.ReadFile(filepath.Join(spec.Mountpoint, "remote.txt")); err != nil || string(b) != "written remotely" {
		t.Errorf("pulled file not readable through the mount: %q, %v", b, err)
	}

	// 4. The loop reaches a fixed point. Both directions have now run over the
	// same paths, and drivel's own writes are reported back to it — by the change
	// feed on the way down, and as mount events on the way up. Nothing may keep
	// moving after the content agrees.
	//
	// Which of the three loop breakers earns the credit is deliberately not
	// asserted here: §4's echo record, the on-disk hash comparison in apply, and
	// M6's unchanged-content gate are redundant with each other by design for
	// identical content, and each has its own tests in internal/syncengine. What
	// only an end-to-end test can show is that they compose into a system that
	// settles instead of ping-ponging.
	settle(3 * time.Second)
	if n := store.putCount("remote.txt"); n != 1 {
		t.Errorf("remote.txt was pushed back %d times after being pulled", n-1)
	}
	if n := store.putCount("existing.txt"); n != 0 {
		t.Errorf("the materialised file was pushed back %d times", n)
	}
	if n := store.getCount("local.txt"); n != 0 {
		t.Errorf("local.txt was downloaded %d times; our own push came back and was applied", n)
	}

	// And no conflict copies: §6 resolves genuine divergence, and manufacturing
	// one where both sides agree would publish a duplicate of every synced file.
	entries, err := os.ReadDir(spec.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "(conflict ") {
			t.Errorf("a conflict copy was created with no divergence: %s", e.Name())
		}
	}
}

// settle waits for anything already in flight to finish. There is nothing to
// poll for here — the assertion is that something did NOT happen.
func settle(d time.Duration) { time.Sleep(d) }
