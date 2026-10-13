// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package plugin

import (
	"context"
	"log"
	"os"
	"slices"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/syshlted/drivel/provider"
)

// The pipe has to reach the child, and it reaches it as a descriptor plus the
// environment entry naming it. Both halves are asserted on the exec.Cmd,
// because a missing one is silent: the child simply never watches, which is
// indistinguishable from the behaviour this replaces.
func TestLaunchPassesTheLivenessPipeToTheChild(t *testing.T) {
	childEnd, hostEnd, env, err := hostAlivePipe()
	if err != nil {
		t.Fatalf("hostAlivePipe: %v", err)
	}
	defer childEnd.Close()
	defer hostEnd.Close()

	p := &process{launchSpec: launchSpec{kind: "fake", path: "/x/" + BinaryPrefix + "fake"}}
	cmd := p.clientConfig(childEnd, env).Cmd

	if len(cmd.ExtraFiles) != 1 || cmd.ExtraFiles[0] != childEnd {
		t.Errorf("ExtraFiles = %v, want exactly the child's end of the pipe", cmd.ExtraFiles)
	}
	if want := hostFDEnv + "=" + strconv.Itoa(hostFD); !slices.Contains(cmd.Env, want) {
		t.Errorf("%s is missing from the child's environment: %v", want, cmd.Env)
	}
	// ExtraFiles[0] is fd 3 in the child, so the number in the environment has to
	// be 3 — a mismatch leaves the child watching a descriptor that is not the
	// pipe, which is the one way this can fail and still look wired up.
	if hostFD != 3 {
		t.Errorf("hostFD = %d, but ExtraFiles[0] is always fd 3 in the child", hostFD)
	}
}

// The mechanism itself: a backend whose host stops holding the pipe exits, with
// nobody having asked it to. Closing the host's end without calling Kill is
// exactly what the kernel does for a host that dies without running any code,
// which is the case (`kill -9` on a mount, MC-40) that left a backend running
// indefinitely with the account's credentials.
func TestABackendExitsWhenItsHostLetsGoOfThePipe(t *testing.T) {
	dir := pluginDir(t)
	lg := log.New(&logBuf{}, "", 0)

	f, err := NewLoader([]string{dir}, lg).Factory("fake")
	if err != nil {
		t.Fatalf("Factory: %v", err)
	}
	store, err := f(context.Background(), provider.Params{
		Config: provider.MustEncodeConfig(map[string]any{}),
		Log:    lg,
	})
	if err != nil {
		t.Fatalf("opening the fake plugin: %v", err)
	}
	ps, ok := store.(*pluginStore)
	if !ok {
		t.Fatalf("Factory returned %T, want the plugin proxy", store)
	}
	proc := ps.proc

	proc.mu.Lock()
	pid := proc.client.ReattachConfig().Pid
	alive := proc.alive
	proc.mu.Unlock()
	if alive == nil {
		t.Fatal("the launch kept no end of the liveness pipe; nothing can tell the child to stop")
	}
	if !running(pid) {
		t.Fatalf("the backend (pid %d) is not running before the test begins", pid)
	}

	// Drop the host's end WITHOUT Kill: the child must decide for itself.
	if err := alive.Close(); err != nil {
		t.Fatalf("closing the host's end: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for running(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("the backend (pid %d) outlived its host's end of the pipe", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Tidy up the host side. Kill on an exited process is a reap, which is what
	// the restart path does too.
	proc.mu.Lock()
	proc.alive = nil // already closed above; do not double-close in Close
	proc.mu.Unlock()
	_ = ps.Close()
}

// A plugin reached without a pipe — run by hand, or launched by a host older
// than this mechanism — must be left alone. watchHost returning without
// starting a watcher is what makes it safe to call unconditionally from Serve.
func TestWatchHostIgnoresAMissingPipe(t *testing.T) {
	t.Setenv(hostFDEnv, "")
	watchHost() // must not panic, and must not exit this test binary

	// A value that does not name the agreed descriptor is also not ours.
	t.Setenv(hostFDEnv, "9")
	watchHost()
}

// running reports whether a pid is still a live process. Signal 0 performs the
// permission checks and nothing else, which is how you ask.
func running(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}
