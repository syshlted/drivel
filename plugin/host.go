// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package plugin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os/exec"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	goplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	"github.com/syshlted/drivel/plugin/internal/pb"
	"github.com/syshlted/drivel/provider"
)

// How a backend that has died is brought back.
//
// The engine already retries a failed push with exponential backoff and the pull
// loop already retries a failed poll, so the host does not need a supervisor of
// its own — it needs to make sure that the next call after a crash relaunches
// the process instead of failing forever, and that a backend that crashes on
// startup cannot be relaunched in a tight loop.
const (
	// restartFloor and restartCeiling bound the wait between relaunches.
	restartFloor   = 1 * time.Second
	restartCeiling = 30 * time.Second
	// healthyRun is how long a process must have lived for its exit to count as
	// a fresh failure rather than as a continuation of a crash loop. Without it a
	// backend that dies after an hour every time would come back with a
	// half-minute delay it did not earn.
	healthyRun = 60 * time.Second
	// launchTimeout bounds the handshake. A plugin that has not said hello by
	// then is not going to.
	launchTimeout = 30 * time.Second
	// closeTimeout bounds the polite Close before the process is killed.
	closeTimeout = 5 * time.Second
)

// process is one backend running in its own process, and the host's session to
// it. It is the piece that makes a crash survivable: every call goes through
// acquire, which relaunches a process that has exited.
type process struct {
	kind string
	path string
	cfg  provider.Config
	lg   *log.Logger

	mu       sync.Mutex
	client   *goplugin.Client
	api      pb.ProviderClient
	content  *contentRegistry
	caps     provider.CapabilitySet
	started  time.Time
	failures int
	nextTry  time.Time
	closed   bool
}

// logf writes one line about this plugin, on the logger belonging to the mount
// that loaded it (M8 rule 8: logging is per mount).
func (p *process) logf(format string, args ...any) {
	if p.lg == nil {
		log.Printf(format, args...)
		return
	}
	p.lg.Printf(format, args...)
}

// clientConfig builds the launch description.
//
// Three of its settings are security decisions rather than tuning, and each is
// noted where it is made.
func (p *process) clientConfig() *goplugin.ClientConfig {
	// Background rather than a caller's context, deliberately: the backend's
	// lifetime is the mount's, not any one call's. Binding it to the context that
	// happened to trigger a launch would kill the process the moment that push or
	// poll finished. Close is what ends it.
	//
	//#nosec G204 -- the path comes from Loader's own scan of its search
	// directories, which it has already vetted; it is never user text.
	cmd := exec.CommandContext(context.Background(), p.path)
	cmd.Cancel = func() error { return nil }
	cmd.Env = pluginEnv()

	return &goplugin.ClientConfig{
		HandshakeConfig: handshake,
		Plugins:         goplugin.PluginSet{pluginName: &grpcPlugin{}},
		Cmd:             cmd,
		// gRPC only. go-plugin can also speak net/rpc, and accepting both would
		// mean a plugin could choose the protocol drivel's error-detail mapping
		// does not exist for — every sentinel in the seam would silently become an
		// opaque string.
		AllowedProtocols: []goplugin.Protocol{goplugin.ProtocolGRPC},
		// The plugin gets a built environment, not this process's. See pluginEnv.
		SkipHostEnv: true,
		// Mutually authenticated TLS over the control connection. With
		// checkLocalTransport refusing anything but a unix socket, the filesystem is
		// what answers "who may connect" — the socket is owner-only — so this is
		// defence in depth behind that rather than the only control, which is what
		// it would have been over the loopback TCP port go-plugin uses on Windows.
		AutoMTLS:     true,
		StartTimeout: launchTimeout,
		// The plugin's stderr is the provider's log output, so it lands on this
		// mount's logger rather than on the process's stream.
		Stderr: &lineWriter{write: func(line string) { p.logf("%s: %s", p.kind, line) }},
		// go-plugin's own chatter is discarded: it is hclog-formatted, it
		// duplicates what this file reports in drivel's own words, and a mount's
		// log should read as one program's output.
		Logger: hclog.New(&hclog.LoggerOptions{Output: io.Discard, Level: hclog.Off}),
		GRPCDialOptions: []grpc.DialOption{
			grpc.WithDefaultCallOptions(
				grpc.MaxCallRecvMsgSize(maxMessage),
				grpc.MaxCallSendMsgSize(maxMessage),
			),
		},
	}
}

// checkLocalTransport refuses a control address that is not a unix socket.
//
// The plugin chooses it, not the host: go-plugin's handshake line carries the
// network and address the child decided to listen on, and the host resolves and
// dials whatever it reads there — "tcp" included, to any host:port that parses.
// The server half of go-plugin only ever picks TCP on Windows, so no honest
// backend built against this protocol can reach the refusal; what it covers is a
// backend that is neither honest nor built against it, which is the same threat
// the loader's mode checks already assume (§2.10 rule 8).
//
// Two things follow from a TCP control address and both are losses. A unix socket
// answers "who may connect?" with filesystem permissions — owner-only by default
// — while a loopback port answers it with "any process on this machine", so the
// mount's credentials become reachable by every local user. And an address the
// plugin names is an outbound dial the plugin chose, which is a beacon whether or
// not anything answers. AutoMTLS is what keeps the second one from also being a
// working connection; it is not a reason to allow the dial.
func checkLocalTransport(addr net.Addr) error {
	if addr == nil {
		return errors.New("no control address")
	}
	if addr.Network() != "unix" {
		return fmt.Errorf("announced a %q control address (%s); drivel speaks to a backend over a unix socket only",
			addr.Network(), addr)
	}
	return nil
}

// launch starts the process and opens the backend. The caller holds p.mu.
func (p *process) launch(ctx context.Context) error {
	client := goplugin.NewClient(p.clientConfig())

	// Start before Client, so the transport can be checked before anything is
	// dialled: Start launches the process and resolves the address the plugin
	// announced, Client is what connects to it, and Start is idempotent, so the
	// call Client makes itself is free.
	addr, err := client.Start()
	if err != nil {
		client.Kill()
		return transportError("plugin %s: launching %s: %w", p.kind, p.path, err)
	}
	if err := checkLocalTransport(addr); err != nil {
		client.Kill()
		// Retryable like every other launch failure here, though this one will
		// never succeed on a retry. Consistency is the point: the relaunch path is
		// the only thing that reports a backend as unusable, and a second class of
		// launch error that bypasses it would need its own plumbing all the way up
		// to the engine for a case no shipped backend can produce.
		return transportError("plugin %s: %s %w", p.kind, p.path, err)
	}

	rpc, err := client.Client()
	if err != nil {
		client.Kill()
		return transportError("plugin %s: launching %s: %w", p.kind, p.path, err)
	}
	raw, err := rpc.Dispense(pluginName)
	if err != nil {
		client.Kill()
		return transportError("plugin %s: %w", p.kind, err)
	}
	d, ok := raw.(*dispensed)
	if !ok {
		client.Kill()
		return transportError("plugin %s: served an unexpected %T", p.kind, raw)
	}

	// The content channel is opened before Open, because its broker ID has to be
	// in the Open request: the plugin dials it lazily, the first time a range
	// write needs the local file. AcceptAndServe runs until this connection shuts
	// down, which is exactly the lifetime wanted — see content.go.
	reg := newContentRegistry()
	contentID := d.broker.NextId()
	go d.broker.AcceptAndServe(contentID, serveContent(reg))

	resp, err := d.api.Open(ctx, &pb.OpenRequest{
		Config:          []byte(p.cfg),
		ContentBrokerId: contentID,
	})
	if err != nil {
		client.Kill()
		// A refused Open is the backend's own answer — bad credentials, an
		// unreadable sweep-mode — so it is decoded rather than wrapped as a
		// transport failure. Wrapping it would make "run drivel login" retryable.
		return decodeError(err)
	}

	caps, unknown := provider.CapabilitySetFromNames(resp.GetCapabilities())
	if len(unknown) > 0 {
		// Not an error. A plugin built against a newer drivel may offer something
		// this build has no code to call, and refusing it would make every
		// protocol addition a breaking change. Saying so is what stops the user
		// wondering why a documented feature does nothing.
		p.logf("%s: plugin offers %v, which this drivel does not know about — ignoring", p.kind, unknown)
	}
	switch {
	case p.started.IsZero():
		p.caps = caps
	case caps != p.caps:
		p.logf("%s", describeMismatch(p.kind, p.caps, caps))
	}

	p.client, p.api, p.content = client, d.api, reg
	p.started = time.Now()
	return nil
}

// acquire returns a live connection, relaunching the backend if it has died.
//
// The failure it returns while backing off is retryable, which is the whole
// design: the engine's existing retry loop is what waits out a restart, so a
// crash costs a deferred push rather than a failed one. See transportError.
func (p *process) acquire(ctx context.Context) (pb.ProviderClient, *contentRegistry, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return nil, nil, errors.New("plugin " + p.kind + ": closed")
	}
	if p.client != nil && !p.client.Exited() {
		return p.api, p.content, nil
	}

	if p.client != nil {
		lived := time.Since(p.started)
		p.client.Kill() // reap; it has already exited
		p.client, p.api, p.content = nil, nil, nil
		if lived >= healthyRun {
			p.failures = 0
		}
		p.failures++
		wait := restartFloor << uint(min(p.failures-1, 5))
		if wait > restartCeiling {
			wait = restartCeiling
		}
		p.nextTry = time.Now().Add(wait)
		p.logf("%s: backend exited after %s; restarting in %s", p.kind, lived.Round(time.Millisecond), wait)
	}

	if now := time.Now(); now.Before(p.nextTry) {
		return nil, nil, transportError("plugin %s: restarting in %s", p.kind, time.Until(p.nextTry).Round(time.Millisecond))
	}
	if err := p.launch(ctx); err != nil {
		// A launch that fails leaves nextTry where the exit set it, so the next
		// call waits rather than spinning on a binary that cannot start.
		if p.nextTry.Before(time.Now()) {
			p.nextTry = time.Now().Add(restartFloor)
		}
		return nil, nil, err
	}
	if p.failures > 0 {
		p.logf("%s: backend restarted", p.kind)
	}
	return p.api, p.content, nil
}

// Close shuts the backend down: the polite Close first, so a backend holding a
// local database commits and unlocks, then the process.
//
// A failure of the polite half is logged and not returned. By the time a mount
// is closing there is nothing useful to do about one, and the kill below makes
// it moot.
func (p *process) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	if p.client == nil {
		return nil
	}
	if !p.client.Exited() {
		ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
		if _, err := p.api.Close(ctx, &pb.CloseRequest{}); err != nil {
			p.logf("%s: closing the backend: %v", p.kind, decodeError(err))
		}
		cancel()
	}
	p.client.Kill()
	p.client, p.api, p.content = nil, nil, nil
	return nil
}

// pluginStore is what a Factory hands back: the proxy, plus the process it is
// talking to, so that closing the store stops the process.
type pluginStore struct {
	*remoteStore
	proc *process
}

// Close releases the backend. app.Mount looks for io.Closer on a store for
// exactly this, so nothing above the seam had to learn that a provider might be
// a process.
func (s *pluginStore) Close() error { return s.proc.Close() }

var _ io.Closer = (*pluginStore)(nil)

// lineWriter turns a stream of bytes into calls with one line each. It is how a
// plugin's stderr becomes lines on the mount's logger instead of raw writes
// interleaved with drivel's own.
type lineWriter struct {
	write func(string)
	buf   bytes.Buffer
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.buf.Write(p)
	for {
		i := bytes.IndexByte(w.buf.Bytes(), '\n')
		if i < 0 {
			// No newline yet: the rest of the line is still coming.
			return len(p), nil
		}
		line := trimLine(string(w.buf.Next(i + 1)))
		if line != "" {
			w.write(line)
		}
	}
}

// trimLine strips the trailing newline and any carriage return before it.
func trimLine(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

// describeLaunch is the one line a mount prints when a backend comes up. It
// names the executable because "which gdrive plugin is this?" is the first
// question when two are installed, and the capability list because it is what
// decides whether this mount has a pull loop at all.
func describeLaunch(kind, path string, caps provider.CapabilitySet) string {
	return fmt.Sprintf("loaded %s plugin from %s (capabilities: %s)", kind, path, caps)
}
