// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package plugin

import (
	"io"
	"os"
	"os/signal"
	"strconv"
	"syscall"
)

// A backend process must not outlive the host that launched it.
//
// go-plugin ends a plugin by having the host call Client.Kill, which is what
// process.Close does. Nothing covers the host dying without getting to that
// line: there is no liveness mechanism on the plugin side of go-plugin at all —
// the child inherits the host's own stdin rather than a pipe to it, and a gRPC
// server goes on listening after its last client disconnects. So `kill -9` on a
// mount left its backend running indefinitely, holding that account's
// credentials and its connection to the provider, with nothing driving it.
// Every crash-restart cycle left another one.
//
// MC-40 found it by being the case that goes looking (see
// scripts/multiclient/README.md): `pgrep -af plugin-serve` after killing a host
// is one of the two things that case checks and no Tier A version of it can.
//
// The fix is the mechanism M16 already uses for the daemonize handshake: the
// host passes one end of a pipe it holds open for its own lifetime, and the
// child reads it. A read that ends cleanly means every writer is gone, and the
// only writer was the host — which the kernel guarantees for *any* way the host
// can die, including the ones that run no code. Three alternatives were weighed
// and rejected:
//
//   - SysProcAttr.Pdeathsig, which is Linux and FreeBSD only, and in Go is
//     delivered when the *thread* that forked exits rather than the process, so
//     the runtime retiring that thread kills a healthy backend (golang/go#27505).
//   - Polling getppid, which is portable and simple but up to a tick late and
//     has to reason about PID 1 and subreapers to decide what "reparented" means.
//   - Watching the gRPC connection, which needs the plugin to care about its own
//     transport and says nothing when the host is alive but wedged.
//
// This is a cleanup mechanism and not a security boundary: a plugin is code
// drivel launched and trusted (M9 rule 8), and one that wanted to survive its
// host could simply not read the pipe. What it buys is that an honest backend
// stops when there is no longer anything to serve.
const hostFDEnv = "DRIVEL_PLUGIN_HOST_FD"

// hostFD is where the pipe lands in the child. exec.Cmd.ExtraFiles starts at 3,
// after the three standard descriptors — the same numbering cmd/drivel's fstab
// daemonize handshake relies on.
const hostFD = 3

// hostGoneExit is the status a backend exits with when its host has vanished.
//
// Nothing reads it: the only process that could have is the one that died. It is
// nonzero because this is not a run that finished, and it is its own number so
// that a reaper's log distinguishes it from a backend that failed.
const hostGoneExit = 3

// hostAlivePipe returns the end of the liveness pipe to hand to a child, the end
// for the host to keep, and the environment entry that tells the child where to
// look.
//
// The host keeps the WRITE end: a read ends at EOF when the last writer closes,
// so the host must be the only writer, and it must not also pass the write end
// to the child or the child would hold its own pipe open forever.
func hostAlivePipe() (childEnd, hostEnd *os.File, env string, err error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, nil, "", err
	}
	return r, w, hostFDEnv + "=" + strconv.Itoa(hostFD), nil
}

// watchHost exits this process when the host that launched it goes away. It
// returns immediately; the watch runs until it fires or the process ends.
//
// A plugin reached any other way — run by hand, or launched by an older host
// that passes no pipe — has nothing at hostFD and is left alone, which is what
// makes this safe to call unconditionally.
func watchHost() {
	if os.Getenv(hostFDEnv) != strconv.Itoa(hostFD) {
		return
	}
	f := os.NewFile(hostFD, "drivel-host")
	if f == nil {
		return
	}
	ignoreTermination()
	go func() {
		// Only a clean end counts. io.Copy returns a nil error at EOF, which
		// happens when every writer has closed; any other error means the
		// descriptor is unusable, which is not evidence about the host and must
		// not take a working backend down with it.
		if _, err := io.Copy(io.Discard, f); err != nil {
			return
		}
		// Exit rather than shut down: the in-flight call, if there is one, is
		// already reading from or writing to a dead host, and there is no polite
		// close to perform against a caller that is gone. What is lost by not
		// unwinding is a provider connection the remote end will reap anyway.
		os.Exit(hostGoneExit)
	}()
}

// ignoreTermination stops SIGTERM from ending a backend whose host is watched.
//
// `systemctl stop` on an fstab mount (M16) unmounts and then signals every
// process in the unit's cgroup at once — and the backend is in that cgroup,
// because it is the daemon's child. go-plugin traps SIGINT and nothing else, so
// the backend died on the same signal the host was draining through: every push
// the drain still had to send went to a dead process, waited out a relaunch
// backoff, and was abandoned or cut off by the drain deadline. The host shrugs
// SIGTERM off by design (it only cancels the context the unmount already ended),
// so the backend was the one process in the unit that a stop could take down
// before its work was done.
//
// Ignoring it is safe only because of the pipe: a watched backend is ended by the
// host's Kill, or by EOF here when the host is gone however it died, and a stop
// that never finishes is still ended by systemd's SIGKILL at TimeoutStopSec. That
// is why this runs only once the pipe is confirmed — a plugin run by hand, or by
// an older host, keeps the default and stops on SIGTERM as anything else does.
//
// signal.Notify with a drained channel rather than signal.Ignore, because an
// ignored disposition is inherited across exec by anything the backend starts,
// and a handler is not.
func ignoreTermination() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM)
	go func() {
		for range ch {
		}
	}()
}
