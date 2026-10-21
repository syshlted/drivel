// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package vfs

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/syshlted/drivel/internal/fsevent"
	"github.com/syshlted/drivel/internal/mount"
	"github.com/syshlted/drivel/internal/testenv"
)

// Ready is the moment a process may open files through a mount it is serving
// itself, and the reason is go-fuse's poll hack rather than the mount. On Linux
// the Go runtime registers every regular file it opens with epoll, by a raw
// syscall that holds its P; on a FUSE file that makes the kernel send POLL and
// wait for the answer, until the first ENOSYS tells it never to ask again. In
// that gap a GC cannot stop the world, the server goroutines that would answer
// are parked behind it, and the process hangs past its own -timeout with a
// thread no signal can end. The hack spends the first POLL on a file of its own
// before fs.Mount returns, and Ready comes after, so every test that waits for
// Ready is safe and every test that polls for a probe file through the
// mountpoint was not: the probe is visible as soon as INIT is answered. Linux
// only, because elsewhere Go keeps regular files out of its poller.
func TestReadyComesAfterThePollHack(t *testing.T) {
	if _, err := os.Stat("/dev/fuse"); err != nil {
		testenv.Unavailable(t, testenv.FUSE, "no /dev/fuse: "+err.Error())
	}
	// go-fuse traces to its own logger, which is the log package's default.
	var trace lockedBuf
	log.SetOutput(&trace)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	backing, mnt := t.TempDir(), t.TempDir()
	ready := make(chan string, 1)
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() {
		served <- NewBackend().Serve(ctx, mount.Options{
			Mountpoint: mnt, Backing: backing, Events: make(chan fsevent.Event, 64),
			FsName: "drivel-test", Logger: log.New(&lockedBuf{}, "", 0), Debug: true,
			Ready: func() { ready <- trace.String() },
		})
	}()
	var atReady string
	select {
	case atReady = <-ready:
	case err := <-served:
		cancel()
		testenv.Unavailable(t, testenv.FUSE, "Serve returned before the mount came up: "+errString(err))
	case <-time.After(10 * time.Second):
		cancel()
		testenv.Unavailable(t, testenv.FUSE, "mount did not come up within 10s")
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-served:
		case <-time.After(10 * time.Second):
			t.Log("mount did not unmount cleanly; try: fusermount3 -u " + mnt)
		}
	})

	// The hack's file is the node go-fuse reserves for it. A request is traced
	// when it arrives, before it is answered, and the hack's poll(2) has to
	// return before fs.Mount can: so a POLL on that node in the trace as Ready
	// saw it was answered before Ready was called.
	hackPoll := regexp.MustCompile(`rx (\d+): POLL n18446744073709551615 `).FindStringSubmatch(atReady)
	if hackPoll == nil {
		t.Fatalf("Ready was called before go-fuse's poll hack had sent POLL; trace so far:\n%s", atReady)
	}
	// And the answer was the one that switches POLL off for the connection.
	answer := regexp.MustCompile(`(?m)^.*tx ` + hackPoll[1] + `: +(.*)$`)
	if !waitUntil(2*time.Second, func() bool { return answer.MatchString(trace.String()) }) {
		t.Fatalf("no answer traced to the hack's POLL (request %s)", hackPoll[1])
	}
	if got := answer.FindStringSubmatch(trace.String())[1]; !strings.HasPrefix(got, fuse.ENOSYS.String()) {
		t.Fatalf("the hack's POLL was answered %s; only %s stops the kernel asking", got, fuse.ENOSYS)
	}

	// Which is the property the tests rely on: from Ready on, opening a file
	// through the mount sends no POLL at all.
	for _, name := range []string{"a", "b", "c"} {
		f, err := os.OpenFile(filepath.Join(mnt, name), os.O_CREATE|os.O_RDWR, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	for _, line := range strings.Split(trace.String(), "\n") {
		if strings.Contains(line, ": POLL n") && !strings.Contains(line, "POLL n18446744073709551615 ") {
			t.Errorf("a file opened after Ready still sent POLL: %s", line)
		}
	}
}

// waitUntil polls cond every 10ms for up to d.
func waitUntil(d time.Duration, cond func() bool) bool {
	for deadline := time.Now().Add(d); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return true
		}
	}
	return cond()
}
