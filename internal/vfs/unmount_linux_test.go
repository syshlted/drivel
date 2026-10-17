// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package vfs

import (
	"bytes"
	"context"
	"log"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/syshlted/drivel/internal/fsevent"
	"github.com/syshlted/drivel/internal/mount"
	"github.com/syshlted/drivel/internal/testenv"
)

// lockedBuf is a bytes.Buffer safe to write from the unmount goroutine while the
// test reads it.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// Every fstab mount ends from outside — umount(8), `systemctl stop` — and the
// process's context is cancelled afterwards, on its way out. Serve's unmount
// goroutine used to wait on that context alone, so it then tried to unmount a
// filesystem that was already gone and printed a failure, to stdout rather than
// the mount's log, after a shutdown that had gone perfectly well.
func TestAnExternalUnmountIsNotUnmountedAgain(t *testing.T) {
	if _, err := os.Stat("/dev/fuse"); err != nil {
		testenv.Unavailable(t, testenv.FUSE, "no /dev/fuse: "+err.Error())
	}
	if _, err := exec.LookPath("fusermount3"); err != nil {
		testenv.Unavailable(t, testenv.FUSE, "no fusermount3: "+err.Error())
	}
	backing, mnt := t.TempDir(), t.TempDir()
	logs := &lockedBuf{}
	ready := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	served := make(chan error, 1)
	go func() {
		served <- NewBackend().Serve(ctx, mount.Options{
			Mountpoint: mnt, Backing: backing, Events: make(chan fsevent.Event, 16),
			FsName: "drivel-test", Logger: log.New(logs, "", 0),
			Ready: func() { close(ready) },
		})
	}()
	select {
	case <-ready:
	case err := <-served:
		testenv.Unavailable(t, testenv.FUSE, "Serve returned before the mount came up: "+errString(err))
	case <-time.After(10 * time.Second):
		testenv.Unavailable(t, testenv.FUSE, "mount did not come up within 10s")
	}

	if out, err := exec.Command("fusermount3", "-u", mnt).CombinedOutput(); err != nil {
		t.Fatalf("external unmount: %v: %s", err, out)
	}
	select {
	case <-served:
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after the filesystem was unmounted from outside")
	}

	// The process winding down, as it does after every fstab stop.
	cancel()
	time.Sleep(300 * time.Millisecond)
	if got := logs.String(); got != "" {
		t.Errorf("after an external unmount and a later cancel, the backend logged:\n%s", got)
	}
}

func errString(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}
