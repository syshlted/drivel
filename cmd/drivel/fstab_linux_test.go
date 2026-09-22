// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

//go:build linux

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zishmusic/drivel/internal/testenv"
)

// The unsets are the load-bearing half of the run-as environment.
//
// A boot mount inherits root's environment. If that carries
// XDG_CONFIG_HOME=/root/.config, then after the ids have already changed every
// path drivel derives still points into root's home — so the mount runs as the
// user while looking for the wrong account's token, and fails in a way that reads
// like a login problem rather than an environment one.
func TestRunAsEnvUnsetsInheritedXDG(t *testing.T) {
	got := map[string]envChange{}
	for _, c := range runAsEnv(&runAsUser{name: "j", uid: 1000, gid: 1000, home: "/home/j"}) {
		got[c.key] = c
	}

	for _, key := range []string{"XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME"} {
		c, ok := got[key]
		if !ok {
			t.Errorf("%s is not touched; root's value would survive the drop", key)
			continue
		}
		if !c.unset {
			t.Errorf("%s is set to %q; it must be unset so it re-derives from HOME", key, c.value)
		}
	}
	if c := got["HOME"]; c.unset || c.value != "/home/j" {
		t.Errorf("HOME = %+v; want /home/j", c)
	}
	for _, key := range []string{"USER", "LOGNAME"} {
		if c := got[key]; c.unset || c.value != "j" {
			t.Errorf("%s = %+v; want j", key, c)
		}
	}
}

// applyRunAsEnv has to actually clear an inherited value, not merely report that
// it would.
func TestApplyRunAsEnvClearsInherited(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/root/.config")
	t.Setenv("XDG_STATE_HOME", "/root/.local/state")
	t.Setenv("HOME", "/root")

	applyRunAsEnv(&runAsUser{name: "j", uid: 1000, gid: 1000, home: "/home/j"})

	if v, ok := os.LookupEnv("XDG_CONFIG_HOME"); ok {
		t.Errorf("XDG_CONFIG_HOME = %q; want it unset", v)
	}
	if v, ok := os.LookupEnv("XDG_STATE_HOME"); ok {
		t.Errorf("XDG_STATE_HOME = %q; want it unset", v)
	}
	if got := os.Getenv("HOME"); got != "/home/j" {
		t.Errorf("HOME = %q; want /home/j", got)
	}
}

func TestLookupRunAs(t *testing.T) {
	t.Run("by name", func(t *testing.T) {
		u, err := lookupRunAs("root")
		if err != nil {
			t.Fatalf("lookupRunAs(root): %v", err)
		}
		if u.uid != 0 {
			t.Errorf("uid = %d; want 0", u.uid)
		}
	})

	t.Run("by numeric id", func(t *testing.T) {
		u, err := lookupRunAs("0")
		if err != nil {
			t.Fatalf("lookupRunAs(0): %v", err)
		}
		if u.uid != 0 {
			t.Errorf("uid = %d; want 0", u.uid)
		}
	})

	// Refusing to mount beats mounting as the wrong user.
	t.Run("an unknown account is refused", func(t *testing.T) {
		_, err := lookupRunAs("no-such-account-here")
		if err == nil || !strings.Contains(err.Error(), "run-as=") {
			t.Fatalf("err = %v; want a run-as refusal", err)
		}
	})
}

// Only root can change user, and saying so beats a bare EPERM from deep inside
// the drop.
func TestDropToRunAsNeedsRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; this asserts the unprivileged refusal")
	}
	err := dropToRunAs(&runAsUser{name: "root", uid: 0, gid: 0})
	if err == nil || !strings.Contains(err.Error(), "only root can change user") {
		t.Fatalf("err = %v; want the unprivileged refusal", err)
	}
}

// Dropping to the account already running is a no-op rather than an error: it is
// what mounting by hand as the owning user does, and setgroups would fail there
// even though nothing needs to change.
func TestDropToRunAsIsANoOpForTheCurrentUser(t *testing.T) {
	self := &runAsUser{name: "self", uid: os.Getuid(), gid: os.Getgid()}
	if err := dropToRunAs(self); err != nil {
		t.Fatalf("dropToRunAs(self): %v", err)
	}
}

func TestReadReady(t *testing.T) {
	t.Run("carries the child's message", func(t *testing.T) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close() //nolint:errcheck // test cleanup
		if _, err := w.WriteString("ok\n"); err != nil {
			t.Fatal(err)
		}
		w.Close() //nolint:errcheck // half-closed on purpose: the reader wants EOF

		msg, err := readReady(r, 5*time.Second)
		if err != nil || msg != "ok" {
			t.Fatalf("readReady = %q, %v; want ok", msg, err)
		}
	})

	t.Run("a child that dies without reporting reads empty", func(t *testing.T) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close() //nolint:errcheck // test cleanup
		w.Close()       //nolint:errcheck // the child exiting, before it said anything

		msg, err := readReady(r, 5*time.Second)
		if err != nil || msg != "" {
			t.Fatalf("readReady = %q, %v; want an empty message", msg, err)
		}
	})

	// A child that neither mounts nor dies must not hang `mount -a` forever when
	// the fstab line asked for a bound.
	t.Run("mount-timeout bounds the wait", func(t *testing.T) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close() //nolint:errcheck // test cleanup
		defer w.Close() //nolint:errcheck // held open: the child is "hung"

		_, err = readReady(r, 50*time.Millisecond)
		if !errors.Is(err, errReadyTimeout) {
			t.Fatalf("err = %v; want errReadyTimeout", err)
		}
	})
}

// The whole helper, through mount(8)'s contract: the process must return once the
// filesystem is live, leave a working mount behind it, and exit when unmounted.
//
// This is the one test that covers the fork/handshake, and it needs a real binary
// because the child re-executes /proc/self/exe — which under `go test` would be
// the test binary itself.
func TestMountHelperDaemonizes(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary and mounts a filesystem")
	}
	if _, err := os.Stat("/dev/fuse"); err != nil {
		testenv.Unavailable(t, testenv.FUSE, "no /dev/fuse: "+err.Error())
	}

	dir := t.TempDir()
	bin := filepath.Join(dir, "drivel")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the helper: %v\n%s", err, out)
	}

	mnt, data := filepath.Join(dir, "mnt"), filepath.Join(dir, "data")
	for _, d := range []string{mnt, data} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		// Best effort: the test unmounts on the happy path, and this catches the
		// rest so a failure does not leave a mount behind.
		_ = exec.Command("fusermount3", "-u", mnt).Run()
	})

	// No credentials, so the mount is log-only: this exercises the helper without
	// reaching the network.
	start := time.Now()
	out, err := exec.Command(bin, "mount-helper", "work", mnt, "-o", "data="+data).CombinedOutput()
	if err != nil {
		t.Fatalf("mount-helper: %v\n%s", err, out)
	}
	// The point of the handshake: mount(8) does not return until the helper exits,
	// so the helper must exit — and only once the filesystem is actually there.
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Errorf("the helper took %v to return", elapsed)
	}

	// Live *at the moment the helper returned*, with no polling: that is what the
	// Ready signal buys over exiting early and hoping.
	if _, err := os.Stat(filepath.Join(mnt, ".")); err != nil {
		t.Fatalf("mountpoint not usable after the helper returned: %v", err)
	}
	if err := os.WriteFile(filepath.Join(mnt, "through.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("write through the mount: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(data, "through.txt"))
	if err != nil || string(got) != "hello" {
		t.Fatalf("backing file = %q, %v; want hello", got, err)
	}

	if err := exec.Command("fusermount3", "-u", mnt).Run(); err != nil {
		t.Fatalf("unmount: %v", err)
	}
	// The daemon must go when the mount does, or every mount/unmount cycle leaks a
	// process holding a state DB.
	deadline := time.Now().Add(15 * time.Second)
	for {
		if err := exec.Command("pgrep", "-f", "mount-helper work "+mnt).Run(); err != nil {
			break // pgrep found nothing
		}
		if time.Now().After(deadline) {
			t.Fatal("the daemon is still running 15s after unmount")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// A failure before the mount is live has to reach mount(8) as a failure. The
// alternative — exiting 0 and dying in the background — makes `mount` report
// success for a filesystem that will never appear.
//
// The two subtests are two different mechanisms, and only one of them is the
// handshake. A refusal the parent can make before forking never reaches the pipe
// at all, so a test that covered only that case would still pass with the child's
// error report deleted — which is exactly what it did until this was split.
func TestMountHelperReportsAStartupFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "drivel")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("building the helper: %v\n%s", err, out)
	}

	t.Run("before the fork", func(t *testing.T) {
		// A state DB inside the backing tree: app.Validate refuses it, because the
		// DB would sync itself and its own writes would generate more events. The
		// parent catches this, so nothing is ever forked.
		mnt, data := filepath.Join(dir, "mnt"), filepath.Join(dir, "data")
		opts := "data=" + data + ",credentials=" + filepath.Join(dir, "creds.json") +
			",state=" + filepath.Join(data, "state.db")

		out, err := exec.Command(bin, "mount-helper", "work", mnt, "-o", opts).CombinedOutput()
		if err == nil {
			t.Fatalf("the helper reported success for a refused mount:\n%s", out)
		}
		if _, statErr := os.Stat(filepath.Join(mnt, "anything")); statErr == nil {
			t.Error("something was mounted despite the failure")
		}

		// `mount -f` has to refuse it too, or checking a line before a reboot says
		// nothing about whether the reboot will work.
		out, err = exec.Command(bin, "mount-helper", "work", mnt, "-o", opts, "-f").CombinedOutput()
		if err == nil {
			t.Errorf("mount -f accepted a line a real mount refuses:\n%s", out)
		}
	})

	t.Run("after the fork", func(t *testing.T) {
		// Nothing above the fork can know this one fails: the mountpoint sits under
		// a regular file, so it is app.Open's MkdirAll — running in the child, where
		// the handshake pipe is the only way back — that reports it.
		blocker := filepath.Join(dir, "a-file")
		if err := os.WriteFile(blocker, []byte("not a directory"), 0o644); err != nil {
			t.Fatal(err)
		}
		mnt := filepath.Join(blocker, "mnt")

		out, err := exec.Command(bin, "mount-helper", "work", mnt, "-o",
			"data="+filepath.Join(dir, "data2")).CombinedOutput()
		if err == nil {
			t.Fatalf("the helper reported success for a mount that could not start:\n%s", out)
		}
		// The child's reason has to survive the trip, or the admin is left with an
		// exit code and no cause.
		if !strings.Contains(string(out), mnt) {
			t.Errorf("the child's error did not reach the parent:\n%s", out)
		}
	})
}
