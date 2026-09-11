package sftp

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"net"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	libsftp "github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

// posixRenameExt is OpenSSH's atomic-overwrite rename. Plain SSH_FXP_RENAME is
// specified to fail when the destination exists, so without this extension a
// replace is remove-then-rename and briefly not atomic. See conn.rename.
const posixRenameExt = "posix-rename@openssh.com"

// conn is one live SSH transport, its SFTP session, and everything about the
// server we only learn by connecting: where the mount root actually is, and which
// optional extensions this session advertises.
//
// It is immutable once built. A dropped connection is not repaired in place — the
// Store discards the whole conn and the next operation builds another (see
// Store.session), which is what keeps "which extensions does this session have"
// a property of the session rather than of the provider.
type conn struct {
	ssh  *ssh.Client
	cli  *libsftp.Client
	root string // absolute remote path the mount root maps to

	// posixRename records whether this session may overwrite atomically. The M18
	// entry calls for taking the extension when advertised; the fallback is not
	// merely slower, it has a window where the destination does not exist.
	posixRename bool
}

func (c *conn) close() error {
	err := c.cli.Close()
	if serr := c.ssh.Close(); serr != nil && err == nil {
		err = serr
	}
	return err
}

// abs maps a root-relative slash path (the seam's currency) to an absolute remote
// path. The empty path is the mount root itself.
//
// Containment is structural rather than checked. Cleaning p against a virtual "/"
// first turns any leading "..", however many, into nothing, so the result is
// always inside c.root; joining the two directly would not, because path.Join
// resolves "root/../x" by *climbing out* of root rather than by collapsing to it.
// Nothing above here should ever produce such a path — the engine's come from
// filepath.Rel against the backing dir, and Enumerate's from joining onto a path
// this function already produced — but a path that names the wrong file on
// someone else's server is not a failure mode worth leaving to convention.
func (c *conn) abs(p string) string {
	if p == "" {
		return c.root
	}
	return path.Join(c.root, path.Clean("/"+p))
}

// rename moves oldAbs onto newAbs, replacing whatever is there.
//
// The two branches differ in more than speed. posix-rename@openssh.com replaces
// atomically, so a reader sees either the old file or the new one. The fallback
// has to unlink the destination first, which leaves a window where the path does
// not exist and where a concurrent writer can win the race — unavoidable, since
// SSH_FXP_RENAME is specified to fail on an existing destination and a server
// without the extension offers nothing better.
func (c *conn) rename(oldAbs, newAbs string) error {
	if c.posixRename {
		return c.cli.PosixRename(oldAbs, newAbs)
	}
	if err := c.cli.Remove(newAbs); err != nil && !isNotExist(err) {
		// Not fatal on its own: the destination may be a non-empty directory, or
		// the server may simply disagree. Let the rename report the real problem.
		_ = err
	}
	return c.cli.Rename(oldAbs, newAbs)
}

// session returns the live connection, dialling one if there is none.
//
// Connection management is the unglamorous half of this provider (DESIGN.md §9,
// M18). The contract it owes the engine is narrow: a dropped connection must
// surface as a *retryable* error and must not poison the provider, so that the
// engine's existing backoff re-drives the operation and the retry finds a fresh
// connection. That is the whole strategy — discard on failure, redial on demand —
// and it is deliberately not a reconnecting wrapper that hides the failure from
// the caller, because an operation interrupted mid-write has to be re-driven from
// the top anyway.
//
// The mutex is held across the dial so that N workers arriving at once produce
// one connection rather than N. They would all block on the network regardless.
func (s *Store) session(ctx context.Context) (*conn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != nil {
		return s.conn, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c, err := s.dial(ctx)
	if err != nil {
		return nil, err
	}
	s.conn = c
	return c, nil
}

// drop discards c if it is still the current connection, so that the next
// operation dials again. Passing the conn that failed is what makes this safe
// under concurrency: two workers failing on the same dead connection close it
// once, and a worker failing on an old connection cannot tear down the new one
// that replaced it.
func (s *Store) drop(c *conn) {
	s.mu.Lock()
	stale := s.conn == c
	if stale {
		s.conn = nil
	}
	s.mu.Unlock()
	if stale {
		_ = c.close()
		s.lg.Printf("[sftp] connection to %s lost; the next operation will reconnect", s.cfg.addr())
	}
}

// dial builds a connection and resolves the mount root against it.
func (s *Store) dial(ctx context.Context) (*conn, error) {
	cfg, err := s.clientConfig(ctx)
	if err != nil {
		return nil, err
	}
	addr := s.cfg.addr()

	// net.Dialer rather than ssh.Dial: this is the only place a context can bound
	// the TCP connect, and an fstab mount coming up before the network does would
	// otherwise sit in a syscall nobody can cancel.
	d := net.Dialer{Timeout: s.cfg.timeout()}
	rawConn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, transient(fmt.Errorf("dialling %s: %w", addr, err))
	}
	// The SSH handshake has no context either, so bound it by deadline and clear
	// the deadline once it is done — leaving it set would kill the session later.
	if dl, ok := ctx.Deadline(); ok {
		_ = rawConn.SetDeadline(dl)
	} else {
		_ = rawConn.SetDeadline(time.Now().Add(s.cfg.timeout()))
	}
	sshConn, chans, reqs, err := ssh.NewClientConn(rawConn, addr, cfg)
	if err != nil {
		_ = rawConn.Close()
		// A host-key failure is permanent and must read as one: retrying it forever
		// against a server whose key changed is exactly the wrong response.
		var ke *knownhosts.KeyError
		if errors.As(err, &ke) {
			return nil, hostKeyError(addr, s.cfg.knownHosts(), ke)
		}
		return nil, sshDialError(addr, err)
	}
	_ = rawConn.SetDeadline(time.Time{})
	client := ssh.NewClient(sshConn, chans, reqs)

	opts := []libsftp.ClientOption{}
	if n := s.cfg.Concurrency; n > 0 {
		// What turns one transfer from latency-bound into bandwidth-bound: the
		// number of read/write packets in flight for a single file. The library's
		// default is deliberately conservative.
		opts = append(opts, libsftp.MaxConcurrentRequestsPerFile(n))
	}
	cli, err := libsftp.NewClient(client, opts...)
	if err != nil {
		_ = client.Close()
		return nil, transient(fmt.Errorf("starting sftp subsystem on %s: %w", addr, err))
	}

	c := &conn{ssh: client, cli: cli}
	_, c.posixRename = cli.HasExtension(posixRenameExt)
	if c.root, err = resolveRoot(cli, s.cfg.Root); err != nil {
		_ = cli.Close()
		_ = client.Close()
		return nil, err
	}
	s.lg.Printf("[sftp] connected to %s as %s, root %s%s",
		addr, s.cfg.User, c.root, extensionNote(c))
	return c, nil
}

func extensionNote(c *conn) string {
	if c.posixRename {
		return " (atomic rename)"
	}
	return " (no posix-rename: replacing a file is briefly not atomic)"
}

// resolveRoot turns the configured root into an absolute remote path.
//
// An empty root means the login directory, which is the useful default for the
// overwhelmingly common "give a user an SSH account and sync their home" case.
// A relative root resolves against it. Both go through RealPath so that what gets
// logged, and what every later path is joined onto, is what the server actually
// resolved rather than what we guessed.
//
// A root that is not a directory is refused here rather than discovered later:
// every subsequent operation would fail in a way that named a child path and not
// the misconfiguration that caused it.
func resolveRoot(cli *libsftp.Client, root string) (string, error) {
	if root == "" {
		root = "."
	}
	abs, err := cli.RealPath(root)
	if err != nil {
		return "", fmt.Errorf("resolving root %q: %w", root, classify(err))
	}
	fi, err := cli.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("root %s: %w", abs, classify(err))
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("root %s is not a directory", abs)
	}
	return abs, nil
}

// clientConfig assembles auth and host-key verification.
func (s *Store) clientConfig(ctx context.Context) (*ssh.ClientConfig, error) {
	auths, err := s.authMethods(ctx)
	if err != nil {
		return nil, err
	}
	cb, err := s.hostKeyCallback()
	if err != nil {
		return nil, err
	}
	return &ssh.ClientConfig{
		User:            s.cfg.User,
		Auth:            auths,
		HostKeyCallback: cb,
		Timeout:         s.cfg.timeout(),
	}, nil
}

// authMethods builds the auth list, in the order ssh(1) would try it.
//
// There is deliberately no password method and no key-passphrase setting. A
// password in a config file is a credential in cleartext on disk that the server
// accepts for a full shell session, and the M18 entry says the option should not
// exist rather than that it should be discouraged. An encrypted key is the same
// argument one level down: the passphrase would have to live beside the key it
// protects. The answer to both is the agent.
func (s *Store) authMethods(ctx context.Context) ([]ssh.AuthMethod, error) {
	var auths []ssh.AuthMethod

	if s.cfg.Key != "" {
		signer, err := loadKey(s.cfg.Key, s.cfg.Certificate)
		if err != nil {
			return nil, err
		}
		auths = append(auths, ssh.PublicKeys(signer))
	}

	if s.cfg.useAgent() {
		sock := os.Getenv("SSH_AUTH_SOCK")
		switch {
		case sock == "" && s.cfg.Agent != nil:
			// Explicitly asked for, explicitly unavailable. Silence here would
			// present as an authentication failure with no hint at the cause —
			// and an fstab mount runs without the environment that sets this.
			return nil, errors.New("agent = true but SSH_AUTH_SOCK is unset (no agent to ask)")
		case sock == "":
			// Defaulted on, not available: not an error, just not a method.
		default:
			// gosec flags the environment-derived address as untrusted input. It is
			// the ssh agent's own socket, named by the variable ssh(1) itself reads,
			// and a unix socket path reaches no network; refusing to trust it would
			// mean refusing agent authentication.
			d := net.Dialer{Timeout: s.cfg.timeout()}
			ac, err := d.DialContext(ctx, "unix", sock) //nolint:gosec // G704: SSH_AUTH_SOCK is a local unix socket, not a URL
			if err != nil {
				if s.cfg.Agent != nil {
					return nil, fmt.Errorf("connecting to the ssh agent at %s: %w", sock, err)
				}
				s.lg.Printf("[sftp] ssh agent at %s unavailable: %v", sock, err)
			} else {
				// The agent connection lives as long as the process; closing it
				// would break the reconnect path, which authenticates again.
				auths = append(auths, ssh.PublicKeysCallback(agent.NewClient(ac).Signers))
			}
		}
	}

	if len(auths) == 0 {
		return nil, errors.New("no authentication configured: set key = \"/path/to/private-key\" or run an ssh agent")
	}
	return auths, nil
}

// loadKey reads a private key, optionally pairing it with a certificate.
func loadKey(keyPath, certPath string) (ssh.Signer, error) {
	pem, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("reading key: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(pem)
	if err != nil {
		var pass *ssh.PassphraseMissingError
		if errors.As(err, &pass) {
			return nil, fmt.Errorf("key %s is passphrase-protected; drivel cannot prompt for one (mount is non-interactive) — load it into an ssh agent instead", keyPath)
		}
		return nil, fmt.Errorf("parsing key %s: %w", keyPath, err)
	}
	if certPath == "" {
		return signer, nil
	}
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, fmt.Errorf("reading certificate: %w", err)
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey(certPEM)
	if err != nil {
		return nil, fmt.Errorf("parsing certificate %s: %w", certPath, err)
	}
	cert, ok := pub.(*ssh.Certificate)
	if !ok {
		return nil, fmt.Errorf("%s is a public key, not a certificate", certPath)
	}
	cs, err := ssh.NewCertSigner(cert, signer)
	if err != nil {
		return nil, fmt.Errorf("pairing certificate %s with key %s: %w", certPath, keyPath, err)
	}
	return cs, nil
}

// hostKeyCallback builds the known_hosts verifier.
//
// It fails closed, and that is the milestone rather than a setting on it: the ssh
// package's ignore-any-host-key callback appears nowhere in this tree, not even
// behind a flag with a frightening name, and a test greps for it by name to keep
// it that way. A drivel mount can be brought up by
// /etc/fstab at boot (M16) with nobody watching, so there is no one to answer a
// trust-on-first-use prompt — the same reasoning that makes `drivel mount`
// non-interactive, reaching the same answer.
func (s *Store) hostKeyCallback() (ssh.HostKeyCallback, error) {
	file := s.cfg.knownHosts()
	cb, err := knownhosts.New(file)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no known_hosts file at %s: add the server's key (ssh-keyscan -H %s >> %s) before mounting",
				file, s.cfg.Host, file)
		}
		return nil, fmt.Errorf("reading %s: %w", file, err)
	}
	return cb, nil
}

// checkKnownHost reports whether known_hosts holds any key for the target, so
// that an unknown host fails at open with something actionable instead of at
// first push with a handshake error — and, more importantly, without needing the
// network to find out.
//
// The mechanism is the documented one: the callback answers a key it does not
// recognise with a *knownhosts.KeyError whose Want is empty for an unknown host
// and non-empty for a host whose key merely differs. Probing with a key the
// server certainly does not have therefore distinguishes the two offline. The
// probe key is a fixed all-zero ed25519 key: it never matches, which is the
// entire requirement, and using a constant keeps this check deterministic.
func (s *Store) checkKnownHost() error {
	cb, err := s.hostKeyCallback()
	if err != nil {
		return err
	}
	probe, err := ssh.NewPublicKey(ed25519.PublicKey(make([]byte, ed25519.PublicKeySize)))
	if err != nil {
		return fmt.Errorf("building host-key probe: %w", err) // unreachable
	}
	addr := s.cfg.addr()
	tcp, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		// Unresolvable now says nothing about later — a boot mount races DNS — so
		// this is not a reason to refuse the mount. The handshake will verify.
		return nil //nolint:nilerr // a name that does not resolve yet is not a misconfiguration
	}
	err = cb(addr, tcp, probe)
	var ke *knownhosts.KeyError
	if errors.As(err, &ke) && len(ke.Want) == 0 {
		return fmt.Errorf("no host key for %s in %s: add it (ssh-keyscan -H -p %d %s >> %s) or connect once with ssh, then mount",
			addr, s.cfg.knownHosts(), s.cfg.port(), s.cfg.Host, s.cfg.knownHosts())
	}
	// Anything else — a mismatch against the probe (expected), a revocation
	// error, nil — means the host IS listed, which is all this check asks.
	return nil
}

// hostKeyError turns a verification failure into a message that says which of the
// two very different things happened, because the responses differ completely.
func hostKeyError(addr, file string, ke *knownhosts.KeyError) error {
	if len(ke.Want) == 0 {
		return fmt.Errorf("host key for %s is not in %s (drivel never trusts a key on first use): %w", addr, file, ke)
	}
	lines := make([]string, 0, len(ke.Want))
	for _, k := range ke.Want {
		lines = append(lines, k.Filename+":"+strconv.Itoa(k.Line))
	}
	return fmt.Errorf("HOST KEY MISMATCH for %s: the server offered a key that is not the one recorded at %s. "+
		"Either the server was rebuilt, or this connection is being intercepted. Refusing to connect: %w",
		addr, strings.Join(lines, ", "), ke)
}

// sshDialError classifies a handshake failure. Authentication is permanent — a
// key the server rejects now it rejects on every retry — while everything else
// at this stage is the network.
func sshDialError(addr string, err error) error {
	if strings.Contains(err.Error(), "unable to authenticate") || strings.Contains(err.Error(), "no supported methods") {
		return fmt.Errorf("authenticating to %s: %w", addr, err)
	}
	return transient(fmt.Errorf("connecting to %s: %w", addr, err))
}
