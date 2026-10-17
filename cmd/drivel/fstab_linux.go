// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

//go:build linux

package main

// The parts of the fstab helper that are specific to how Linux mounts a
// filesystem: becoming the account that owns the files, and getting out of
// mount(8)'s way once the filesystem is live.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/syshlted/drivel/internal/app"
	"github.com/syshlted/drivel/internal/control"
)

const (
	// helperChildEnv marks the re-executed child, so it mounts instead of forking
	// another child. Its value is the protocol version, not a boolean, so that a
	// future change to the handshake can be detected rather than guessed at.
	helperChildEnv = "DRIVEL_MOUNT_HELPER_CHILD"
	helperChildTag = "1"

	// helperControlEnv carries the control socket path the parent resolved to the
	// child. The parent has to resolve it, because only the parent knows whether
	// the helper started as root: by the time the child runs, root is gone.
	helperControlEnv = "DRIVEL_MOUNT_HELPER_CONTROL"

	// helperReadyFD is the pipe the child reports through. Fd 3 because
	// exec.Cmd.ExtraFiles starts there, after the three standard descriptors.
	helperReadyFD = 3
)

// runMountHelper is the mount(8) helper entry point.
//
// The order below is the whole design. The log file is opened while still
// privileged, so a boot mount can write into a root-owned directory. The
// environment moves to the target account before anything resolves a path, so
// "~", the config file and the state DB all mean that account's copies. The ids
// drop before the token is read, so a mount that was asked to run as someone else
// cannot read credentials as root even once. And only then does the process fork,
// so every error above this line is reported synchronously to mount(8) instead of
// disappearing into a background process nobody is watching.
func runMountHelper(args []string) error {
	h, err := parseHelperArgs(args)
	if err != nil {
		return err
	}
	c, err := h.resolve()
	if err != nil {
		return err
	}
	if os.Getenv(helperChildEnv) == helperChildTag {
		return runHelperChild(h, c)
	}

	var target *runAsUser
	if c.runAs != "" {
		if target, err = lookupRunAs(c.runAs); err != nil {
			return err
		}
		// Environment first: it decides where "~", the config file and the state
		// DB resolve, and it has to be right whether or not the ids are dropped
		// (they are not, for a dry run).
		applyRunAsEnv(target)
	}

	if h.fake {
		// `mount -f`: everything except the mount. The ids are deliberately left
		// alone — a dry run must not make an irreversible change to the process —
		// and the log file is deliberately not opened, because creating it would be
		// a side effect of a run that is supposed to have none. Everything else
		// still runs, which is the point of checking a line this way: a run-as
		// typo, a path that does not resolve, and a mount app.Validate refuses all
		// fail here rather than at the next boot.
		spec, specErr := checkedSpec(h, c)
		if specErr != nil {
			return specErr
		}
		// Resolved but not created: where the socket would go is checkable, and
		// making the directory is exactly the side effect a dry run must not have.
		_, err := helperControlSocket(c, helperSystemDir(c, target), spec.Name)
		return err
	}

	logFile, err := openHelperLog(c.logfile, target)
	if err != nil {
		return err
	}

	// The control socket's directory, while root can still make it: under /run,
	// which nobody else can write, handed to the account the mount will run as.
	systemDir := helperSystemDir(c, target)
	if systemDir != "" {
		uid, gid := 0, 0
		if target != nil {
			uid, gid = target.uid, target.gid
		}
		if _, err := control.PrepareSystemDir(uid, gid); err != nil {
			return fmt.Errorf("option control: %w", err)
		}
	}

	if target != nil {
		if err := dropToRunAs(target); err != nil {
			return err
		}
	}

	// Check before forking, so a bad option is an error from `mount` rather than a
	// background process that exits after mount(8) has already succeeded.
	spec, err := checkedSpec(h, c)
	if err != nil {
		return err
	}
	socket, err := helperControlSocket(c, systemDir, spec.Name)
	if err != nil {
		return err
	}
	if h.verbose {
		//nolint:gosec // G706: the fstab line is root's to write; nothing less privileged reaches here.
		log.Printf("mounting %s at %s (backing %s)", spec.FsName, spec.Mountpoint, backingLabel(spec))
	}

	if c.foreground {
		if logFile != nil {
			log.SetOutput(logFile)
			defer logFile.Close() //nolint:errcheck // the process is exiting
		}
		return serveHelperMount(h, c, socket, nil)
	}
	return daemonize(c, socket, logFile)
}

// helperSystemDir is the control socket directory a root-started helper uses,
// or "" when the line asked for no socket, named its own, or the helper is not
// root (see helperControlSocket for why that is the rule).
func helperSystemDir(c *helperOptions, target *runAsUser) string {
	if !c.control || c.controlSocket != "" || os.Geteuid() != 0 {
		return ""
	}
	uid := 0
	if target != nil {
		uid = target.uid
	}
	return control.SystemDirFor(uid)
}

// checkedSpec builds this line's mount and runs the cross-mount validation over
// it. app.Validate is pure path arithmetic and touches no files, which is what
// lets the -f dry run use it as well.
func checkedSpec(h *helperArgs, c *helperOptions) (app.MountSpec, error) {
	spec, err := helperSpec(h, c)
	if err != nil {
		return spec, err
	}
	return spec, app.Validate([]app.MountSpec{spec})
}

func backingLabel(spec app.MountSpec) string {
	if spec.DataDir == "" {
		return "in-place"
	}
	return spec.DataDir
}

// serveHelperMount runs the one mount this fstab line describes, with its
// control socket at socket when that is not "".
func serveHelperMount(h *helperArgs, c *helperOptions, socket string, ready func()) error {
	spec, err := helperSpec(h, c)
	if err != nil {
		return err
	}
	spec.Ready = ready

	reg, err := newRegistry()
	if err != nil {
		return err
	}

	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// As on the flag path: the control API's shutdown is the signal's context one
	// level down, so `drivel stop` unmounts and drains exactly as umount does.
	ctx, shutdown := context.WithCancelCause(sigCtx)
	defer shutdown(nil)

	a, err := app.New(ctx, []app.MountSpec{spec}, reg)
	if err != nil {
		return err
	}
	defer a.Close() //nolint:errcheck // the process is exiting; Run's error is the one that matters
	// Before Run, and so before the mount reports ready: a socket that cannot be
	// served fails the mount through the handshake, rather than leaving mount(8)
	// to report success for a line whose control option silently did nothing.
	if socket != "" {
		stopControl, err := startControl(socket, a, shutdown)
		if err != nil {
			return err
		}
		defer stopControl()
	}
	return a.Run(ctx)
}

// runHelperChild is the forked half: mount, then tell the parent.
func runHelperChild(h *helperArgs, c *helperOptions) error {
	ready := &readyPipe{f: os.NewFile(helperReadyFD, "drivel-ready")}
	socket := os.Getenv(helperControlEnv)
	_ = os.Unsetenv(helperControlEnv)
	if c.control && socket == "" {
		// The parent always sends one when the line asks for a socket, so this is
		// a parent and child from two different builds.
		err := errors.New("option control: the mount process was not told where its control socket goes")
		ready.fail(err)
		return err
	}
	err := serveHelperMount(h, c, socket, ready.ok)
	if err != nil {
		// A no-op once the mount has been reported live: by then the parent has
		// exited and nothing is reading. An unmount error belongs in the log.
		ready.fail(err)
	}
	return err
}

// readyPipe is the child's end of the startup handshake. ok comes from the
// serving goroutine and fail from the main one, so it is mutex-guarded; whichever
// arrives first wins and closes the pipe.
type readyPipe struct {
	f  *os.File
	mu sync.Mutex
	// done is set by the first report. The pipe carries exactly one message: the
	// parent's whole job is to learn whether the filesystem came up.
	done bool
}

func (p *readyPipe) ok()            { p.send("ok\n") }
func (p *readyPipe) fail(err error) { p.send("error: " + err.Error() + "\n") }
func (p *readyPipe) send(msg string) {
	if p == nil || p.f == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.done {
		return
	}
	p.done = true
	_, _ = io.WriteString(p.f, msg)
	_ = p.f.Close()
}

// daemonize re-executes this helper as a background child and waits to hear that
// the filesystem is live.
//
// mount(8) does not return until the helper exits, so a helper that simply served
// the mount would hang `mount -a` — and with it the boot. Exiting *before* the
// mount is live would be worse: mount(8) would report success for a filesystem
// that may never appear. So the parent exits only once the child has mounted, and
// carries the child's failure back as its own.
func daemonize(c *helperOptions, socket string, logFile *os.File) error {
	r, w, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("ready pipe: %w", err)
	}
	defer r.Close() //nolint:errcheck // read end, parent is exiting

	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("opening %s: %w", os.DevNull, err)
	}
	defer devNull.Close() //nolint:errcheck

	// The daemon must not hold the caller's stdout and stderr open. Whoever ran
	// mount(8) may be capturing them through a pipe — `out=$(mount -a 2>&1)`, or
	// any program using Go's CombinedOutput — and such a caller waits for EOF on
	// that pipe, not merely for the helper to exit. An inherited descriptor stays
	// open for the life of the mount, so the caller would block until the
	// filesystem is unmounted, which is the opposite of what daemonizing is for.
	//
	// So the log goes to logfile= or to /dev/null, which is what every other FUSE
	// helper does. Nothing is lost at the point it matters: a failure *before* the
	// mount is live comes back through the handshake and is printed by the parent.
	out := devNull
	if logFile != nil {
		out = logFile
	}

	cmd := &exec.Cmd{
		Path: "/proc/self/exe",
		// The original argv, argv[0] included: the child has to reach the helper
		// the same way this process did, whether that was through a /sbin/mount.*
		// symlink or `drivel mount-helper`.
		Args:        os.Args,
		Env:         append(os.Environ(), helperChildEnv+"="+helperChildTag, helperControlEnv+"="+socket),
		Stdin:       devNull,
		Stdout:      out,
		Stderr:      out,
		ExtraFiles:  []*os.File{w},
		SysProcAttr: &syscall.SysProcAttr{Setsid: true},
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting the mount process: %w", err)
	}
	// The parent's copy of the write end has to go, or the read below never sees
	// EOF when the child dies.
	_ = w.Close()
	if logFile != nil {
		_ = logFile.Close()
	}

	msg, readErr := readReady(r, c.mountTimeout)
	switch {
	case errors.Is(readErr, errReadyTimeout):
		// A half-started daemon that mounts later, behind mount(8)'s back, is
		// worse than a clean failure: nothing would be tracking it.
		_ = cmd.Process.Kill()
		return fmt.Errorf("the mount did not come up within mount-timeout=%s", c.mountTimeout)
	case readErr != nil:
		return fmt.Errorf("waiting for the mount to come up: %w", readErr)
	}

	switch msg {
	case "ok":
		return nil
	case "":
		// The pipe closed with nothing on it, so the child died without getting far
		// enough to report — a panic or a signal, since every returned error is sent
		// first. There is no reason to relay, because the child's stderr went to
		// logfile= or to /dev/null, so say where to look for one.
		reason := "the mount process exited before mounting"
		if waitErr := cmd.Wait(); waitErr != nil {
			reason = fmt.Sprintf("%s: %v", reason, waitErr)
		}
		if logFile == nil {
			reason += " (set logfile= to capture why)"
		}
		return errors.New(reason)
	default:
		_, _ = cmd.Process.Wait()
		return errors.New(strings.TrimPrefix(msg, "error: "))
	}
}

var errReadyTimeout = errors.New("timed out")

// readReady reads the child's single handshake message.
func readReady(r *os.File, timeout time.Duration) (string, error) {
	type result struct {
		msg string
		err error
	}
	ch := make(chan result, 1)
	go func() {
		b, err := io.ReadAll(r)
		ch <- result{strings.TrimSpace(string(b)), err}
	}()

	if timeout <= 0 {
		// The default. systemd already bounds a mount unit's startup, and a second
		// timeout here with a different default would be a second answer to the
		// same question. mount-timeout= is for the fstab that wants one anyway.
		res := <-ch
		return res.msg, res.err
	}
	select {
	case res := <-ch:
		return res.msg, res.err
	case <-time.After(timeout):
		return "", errReadyTimeout
	}
}

// openHelperLog opens the logfile= destination while still privileged, so that a
// boot mount can log into a root-owned directory such as /var/log even when it is
// about to become an unprivileged user.
func openHelperLog(path string, target *runAsUser) (*os.File, error) {
	if path == "" {
		return nil, nil
	}
	p, err := expandHelperPath(path)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return nil, fmt.Errorf("option logfile: %w", err)
	}
	// Hand it to the account that will be writing it, so the log does not end up
	// owned by root in that account's own directory. Best effort: failing to chown
	// is not a reason to refuse the mount, but it is worth saying.
	if target != nil && os.Geteuid() == 0 {
		if err := f.Chown(target.uid, target.gid); err != nil {
			log.Printf("warning: could not give %s to %s: %v", p, target.name, err)
		}
	}
	return f, nil
}

// runAsUser is the account a mount should run as.
type runAsUser struct {
	name   string
	uid    int
	gid    int
	groups []int
	home   string
}

// lookupRunAs resolves a run-as= value, which may be a user name or a numeric id.
//
// Note this uses the pure-Go /etc/passwd reader in a CGO-free build, so an
// account that exists only in NSS (LDAP, SSSD) will not be found. That is the
// right failure: refusing to mount beats mounting as the wrong user.
func lookupRunAs(spec string) (*runAsUser, error) {
	var (
		u   *user.User
		err error
	)
	if _, convErr := strconv.Atoi(spec); convErr == nil {
		u, err = user.LookupId(spec)
	} else {
		u, err = user.Lookup(spec)
	}
	if err != nil {
		return nil, fmt.Errorf("option run-as=%s: %w", spec, err)
	}

	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return nil, fmt.Errorf("option run-as=%s: uid %q is not numeric", spec, u.Uid)
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return nil, fmt.Errorf("option run-as=%s: gid %q is not numeric", spec, u.Gid)
	}

	// The supplementary groups matter as much as the primary one: without them the
	// daemon silently loses access to anything shared by group, and the symptom is
	// a permission error deep in a sync rather than at the mount.
	ids, err := u.GroupIds()
	if err != nil {
		return nil, fmt.Errorf("option run-as=%s: reading groups: %w", spec, err)
	}
	groups := make([]int, 0, len(ids))
	for _, s := range ids {
		g, convErr := strconv.Atoi(s)
		if convErr != nil {
			return nil, fmt.Errorf("option run-as=%s: group %q is not numeric", spec, s)
		}
		groups = append(groups, g)
	}
	return &runAsUser{name: u.Username, uid: uid, gid: gid, groups: groups, home: u.HomeDir}, nil
}

// envChange is one variable the run-as target needs set or removed.
type envChange struct {
	key   string
	value string
	unset bool
}

// runAsEnv is what the environment has to become for the target account.
//
// The unsets are the load-bearing half. A boot mount inherits root's environment,
// and if that carries XDG_CONFIG_HOME=/root/.config then every path drivel
// derives — the config file, the account's token, the state DB — keeps pointing
// into root's home after the ids have already changed. The mount would then run
// as the user while looking for the wrong account's credentials, which fails in a
// way that reads like a login problem rather than an environment one.
func runAsEnv(u *runAsUser) []envChange {
	return []envChange{
		{key: "HOME", value: u.home},
		{key: "USER", value: u.name},
		{key: "LOGNAME", value: u.name},
		{key: "XDG_CONFIG_HOME", unset: true},
		{key: "XDG_STATE_HOME", unset: true},
		{key: "XDG_CACHE_HOME", unset: true},
		{key: "XDG_DATA_HOME", unset: true},
	}
}

func applyRunAsEnv(u *runAsUser) {
	for _, c := range runAsEnv(u) {
		if c.unset {
			_ = os.Unsetenv(c.key)
			continue
		}
		_ = os.Setenv(c.key, c.value)
	}
}

// dropToRunAs gives up root for the target account, permanently.
//
// Order matters and is not interchangeable: the supplementary groups and the
// primary group have to be set while the process can still do so, which is to say
// before the uid changes. Getting it backwards leaves a process that kept root's
// groups, which is a privilege the fstab line did not ask for.
func dropToRunAs(u *runAsUser) error {
	if os.Getuid() == u.uid && os.Getgid() == u.gid {
		// Already there — mounting by hand as the owning user, typically. Setuid to
		// your own id succeeds, but setgroups does not, so skipping is not just an
		// optimisation.
		return nil
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("option run-as=%s: only root can change user (running as uid %d)", u.name, os.Getuid())
	}
	if err := syscall.Setgroups(u.groups); err != nil {
		return fmt.Errorf("option run-as=%s: setting groups: %w", u.name, err)
	}
	if err := syscall.Setgid(u.gid); err != nil {
		return fmt.Errorf("option run-as=%s: setting gid %d: %w", u.name, u.gid, err)
	}
	if err := syscall.Setuid(u.uid); err != nil {
		return fmt.Errorf("option run-as=%s: setting uid %d: %w", u.name, u.uid, err)
	}
	return nil
}
