// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package app

import (
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// A write made through the mount while another process holds an exclusive
// flock on the file — `flock f -c 'echo x >> f'`, or a database's lock beside a
// tool appending to it — is not uploaded while the lock stands, and is uploaded
// once it goes. The first half is the engine's existing refusal to publish a
// locked file, which a lock taken through the mount could not reach until flocks
// were forwarded to the backing file; the second is what forwarding owes in
// return, since the holder here never writes and so its close would have
// announced nothing. Linux only: forwarding is a capability the Linux kernel
// offers.
func TestAWriteUnderAFlockIsSentWhenTheLockGoes(t *testing.T) {
	f := newFleet(t, 1)
	c1 := f.peers[0]
	c1.write("log", "line 1\n")
	if !waitFor(10*time.Second, func() bool { b, _ := f.store.content("log"); return string(b) == "line 1\n" }) {
		t.Fatal("the file never reached the remote")
	}

	holder, err := os.Open(c1.mnt("log"))
	if err != nil {
		t.Fatal(err)
	}
	// Registered after the fleet's own cleanup, so it runs first: a descriptor
	// left open on the mount would keep the unmount busy for good.
	t.Cleanup(func() { holder.Close() })
	if err := unix.Flock(int(holder.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	app, err := os.OpenFile(c1.mnt("log"), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.WriteString("line 2\n"); err != nil {
		t.Fatal(err)
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	if !waitFor(10*time.Second, func() bool { return c1.logw.saw("log: another process holds it locked for writing") }) {
		t.Fatal("the push under the lock was not declined, so the test did not exercise the case")
	}
	if b, _ := f.store.content("log"); string(b) != "line 1\n" {
		t.Fatalf("remote = %q while the lock stood; want it unchanged", b)
	}

	if err := unix.Flock(int(holder.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if !waitFor(10*time.Second, func() bool { b, _ := f.store.content("log"); return string(b) == "line 1\nline 2\n" }) {
		b, _ := f.store.content("log")
		t.Fatalf("remote = %q after the lock went; the write made under it was never sent", b)
	}
}
