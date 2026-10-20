// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package vfs

import (
	"context"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"
)

// heldLock is a wrapped handle whose lock is always taken by someone else.
type heldLock struct{ tries atomic.Int32 }

func (h *heldLock) Setlk(context.Context, uint64, *fuse.FileLock, uint32) syscall.Errno {
	h.tries.Add(1)
	return syscall.EWOULDBLOCK
}

// A waiting flock gives up when FUSE interrupts the request (go-fuse cancels its
// context), instead of blocking in flock(2) where no interrupt reaches it and
// taking the lock later for a caller that has already been told EINTR.
func TestAWaitingFlockGivesUpWhenInterrupted(t *testing.T) {
	h := &heldLock{}
	f := &fileHandle{wrapped: h}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan syscall.Errno, 1)
	go func() { done <- f.Setlkw(ctx, 1, &fuse.FileLock{Typ: syscall.F_WRLCK}, fuse.FUSE_LK_FLOCK) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case errno := <-done:
		if errno != syscall.EINTR {
			t.Fatalf("Setlkw after an interrupt = %v; want EINTR", errno)
		}
	case <-time.After(time.Second):
		t.Fatal("Setlkw did not notice the interrupt")
	}
	if h.tries.Load() < 2 {
		t.Fatalf("tried %d time(s); want it to have retried while waiting", h.tries.Load())
	}
}

// Only flock is forwarded. A POSIX lock request is not one this handle was
// promised (the mount does not ask for them), and answering it with the
// loopback's OFD lock would give it the wrong semantics.
func TestAPOSIXLockIsNotForwarded(t *testing.T) {
	h := &heldLock{}
	f := &fileHandle{wrapped: h}
	if errno := f.Setlk(context.Background(), 1, &fuse.FileLock{Typ: syscall.F_WRLCK}, 0); errno != syscall.ENOSYS {
		t.Fatalf("Setlk without FUSE_LK_FLOCK = %v; want ENOSYS", errno)
	}
	if h.tries.Load() != 0 {
		t.Fatal("a POSIX lock request reached the backing file")
	}
}
