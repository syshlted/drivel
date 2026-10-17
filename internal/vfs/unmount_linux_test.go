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
	"strings"
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

// One failed unmount is not final. A mountpoint something is still using answers
// EBUSY — at shutdown, most often a FUSE request still in flight — and trying
// once left server.Wait waiting forever for an unmount nothing would retry.
// Here a held-open file stands in for the in-flight request: the first attempt
// fails, and once the file is closed the mount must come down by itself.
func TestABusyUnmountIsRetried(t *testing.T) {
	if _, err := os.Stat("/dev/fuse"); err != nil {
		testenv.Unavailable(t, testenv.FUSE, "no /dev/fuse: "+err.Error())
	}
	backing, mnt := t.TempDir(), t.TempDir()
	if err := os.WriteFile(backing+"/held.txt", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
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
	case <-time.After(10 * time.Second):
		testenv.Unavailable(t, testenv.FUSE, "mount did not come up within 10s")
	}

	held, err := os.Open(mnt + "/held.txt")
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	time.Sleep(1500 * time.Millisecond)
	select {
	case <-served:
		held.Close()
		t.Skip("the unmount succeeded with a file held open; this kernel does not report EBUSY here")
	default:
	}
	if !bytes.Contains([]byte(logs.String()), []byte("failed")) {
		t.Errorf("no failed attempt was logged while the file was held:\n%s", logs)
	}
	held.Close()
	select {
	case <-served:
	case <-time.After(10 * time.Second):
		exec.Command("fusermount3", "-u", "-z", mnt).Run() //nolint:errcheck // test cleanup
		t.Fatal("the mount stayed up after the file was closed: the busy unmount was never retried")
	}
	// Serve returns as soon as the kernel lets go, which can be a moment before the
	// retry loop gets to say so.
	for deadline := time.Now().Add(2 * time.Second); !strings.Contains(logs.String(), "unmounted") && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(logs.String(), "unmounted") {
		t.Errorf("the eventual unmount was not reported:\n%s", logs)
	}
}
