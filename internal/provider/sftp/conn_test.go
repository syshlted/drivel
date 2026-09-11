package sftp

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/zishmusic/drivel/internal/provider"
)

// ssh.InsecureIgnoreHostKey must not appear in this tree, not even behind a flag
// with a frightening name (DESIGN.md §9, M18). An fstab mount comes up at boot
// with nobody to answer a trust-on-first-use prompt, so the only safe answer is
// to refuse — the same reasoning that makes `drivel mount` non-interactive.
//
// This is a grep rather than a behavioural assertion on purpose: the failure it
// guards against is someone adding the escape hatch later, in a package that does
// not exist yet, which no test of this package's behaviour could ever see.
func TestInsecureIgnoreHostKeyIsAbsentFromTheTree(t *testing.T) {
	root := "../../.."
	needle := "InsecureIgnoreHostKey"
	self, err := filepath.Abs("conn_test.go")
	if err != nil {
		t.Fatalf("locating this file: %v", err)
	}

	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "bin" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") {
			return nil
		}
		abs, err := filepath.Abs(p)
		if err != nil || abs == self {
			return nil //nolint:nilerr // this file names it in order to forbid it
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if strings.Contains(string(b), needle) {
			t.Errorf("%s references ssh.%s: host key verification must fail closed", p, needle)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the tree: %v", err)
	}
}

// An unknown host is refused at Open, without the network. That turns the
// commonest misconfiguration from a retry loop against an unreachable server into
// one message naming the file and the command that fixes it.
func TestOpenRefusesAnUnknownHost(t *testing.T) {
	ts := startServer(t)
	cfg := ts.config()
	cfg.KnownHosts = writeFile(t, "known_hosts", []byte("")) // valid, but empty

	_, err := Open(t.Context(), cfg, discardLogger())
	if err == nil {
		t.Fatal("Open accepted a host with no key in known_hosts")
	}
	if !strings.Contains(err.Error(), "known_hosts") {
		t.Errorf("error %q does not say where to add the key", err)
	}
}

func TestOpenRefusesAMissingKnownHostsFile(t *testing.T) {
	ts := startServer(t)
	cfg := ts.config()
	cfg.KnownHosts = filepath.Join(t.TempDir(), "absent")

	_, err := Open(t.Context(), cfg, discardLogger())
	if err == nil {
		t.Fatal("Open accepted a missing known_hosts file")
	}
	if !strings.Contains(err.Error(), "ssh-keyscan") {
		t.Errorf("error %q does not say how to create it", err)
	}
}

// A host that IS listed passes the offline check, so the check cannot be
// satisfied by refusing everything.
func TestOpenAcceptsAKnownHost(t *testing.T) {
	ts := startServer(t)
	if _, err := Open(t.Context(), ts.config(), discardLogger()); err != nil {
		t.Fatalf("Open of a known host: %v", err)
	}
}

// The offline check cannot see a key that is merely *different*, so the handshake
// has to refuse it — loudly, and permanently rather than retryably, because
// retrying a server whose key changed is exactly the wrong response.
func TestDialRefusesAHostKeyMismatch(t *testing.T) {
	ts := startServer(t)
	other, _ := genKey(t)

	cfg := ts.config()
	cfg.KnownHosts = writeKnownHosts(t, ts.addr, other.PublicKey())

	s, err := Open(t.Context(), cfg, discardLogger())
	if err != nil {
		t.Fatalf("Open: %v", err) // the host IS listed; only the key differs
	}
	defer s.Close() //nolint:errcheck // test

	_, _, err = s.Stat(t.Context(), "anything")
	if err == nil {
		t.Fatal("connected to a server whose host key did not match")
	}
	if !strings.Contains(err.Error(), "MISMATCH") {
		t.Errorf("error %q does not name the mismatch", err)
	}
	if provider.IsRetryable(err) {
		t.Error("a host key mismatch was reported as retryable; the engine would hammer a possibly-intercepted server")
	}
}

// A dropped connection must surface as retryable and must not poison the store:
// the engine's backoff re-drives the operation, and the retry has to find a fresh
// connection. That is the whole reconnection strategy (see Store.session).
func TestConnectionLossIsRetryableAndRecovers(t *testing.T) {
	ts := startServer(t)
	s := ts.open(t)

	if _, err := s.Put(t.Context(), "before.txt", strings.NewReader("x")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	ts.cutConnections()

	// The operation that meets the dead connection fails, and how it fails matters:
	// a permanent error here would stop the engine retrying and inbound and
	// outbound sync would both stall until a restart.
	if _, err := s.Put(t.Context(), "during.txt", strings.NewReader("x")); err != nil {
		if !provider.IsRetryable(err) {
			t.Errorf("a dropped connection reported as permanent: %v", err)
		}
	}

	// ...and the retry reconnects.
	if _, err := s.Put(t.Context(), "after.txt", strings.NewReader("x")); err != nil {
		t.Fatalf("the store did not reconnect: %v", err)
	}
	if b, err := os.ReadFile(ts.path("after.txt")); err != nil || string(b) != "x" {
		t.Fatalf("after reconnecting: %q, %v", b, err)
	}
}

func TestConfigValidation(t *testing.T) {
	base := Config{Host: "h", User: "u"}
	for name, mutate := range map[string]func(*Config){
		"no host":               func(c *Config) { c.Host = "" },
		"no user":               func(c *Config) { c.User = "" },
		"a port inside host":    func(c *Config) { c.Host = "h:2222" },
		"a port out of range":   func(c *Config) { c.Port = 70000 },
		"negative concurrency":  func(c *Config) { c.Concurrency = -1 },
		"a certificate, no key": func(c *Config) { c.Certificate = "cert.pub" },
	} {
		t.Run(name, func(t *testing.T) {
			c := base
			mutate(&c)
			if err := c.validate(); err == nil {
				t.Errorf("validate accepted %s", name)
			}
		})
	}
	if err := base.validate(); err != nil {
		t.Errorf("validate rejected a good config: %v", err)
	}
}

// There is no password option and no passphrase option, so an encrypted key has
// to say what to do instead rather than failing as a parse error.
func TestPassphraseProtectedKeyExplainsItself(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating a key: %v", err)
	}
	blk, err := ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte("hunter2"))
	if err != nil {
		t.Fatalf("encrypting a key: %v", err)
	}
	path := writeFile(t, "enc_key", pem.EncodeToMemory(blk))

	_, err = loadKey(path, "")
	if err == nil {
		t.Fatal("loadKey accepted a passphrase-protected key")
	}
	if !strings.Contains(err.Error(), "agent") {
		t.Errorf("error %q does not point at the agent", err)
	}
}

// With no key and no agent there is nothing to authenticate with, and saying so
// beats a handshake failure that names neither.
func TestNoAuthConfiguredIsRefused(t *testing.T) {
	no := false
	s := &Store{cfg: Config{Host: "h", User: "u", Agent: &no}, lg: discardLogger()}
	_, err := s.authMethods(t.Context())
	if err == nil {
		t.Fatal("authMethods accepted a config with no key and no agent")
	}
	if !strings.Contains(err.Error(), "key") {
		t.Errorf("error %q does not say what to configure", err)
	}
}

// agent = true with no agent to talk to is an error, not a silent no-op: an fstab
// mount runs without the environment that sets SSH_AUTH_SOCK, and a silent
// fallback would present as an authentication failure naming nothing.
func TestExplicitAgentWithoutOneIsRefused(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	yes := true
	s := &Store{cfg: Config{Host: "h", User: "u", Agent: &yes}, lg: discardLogger()}
	if _, err := s.authMethods(t.Context()); err == nil {
		t.Fatal("authMethods accepted agent = true with no SSH_AUTH_SOCK")
	}
}

// Defaulted-on is different: no agent is simply not a method, and a configured
// key still authenticates.
func TestDefaultedAgentWithoutOneIsNotAnError(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	ts := startServer(t)
	cfg := ts.config()
	cfg.Agent = nil

	s := &Store{cfg: cfg, lg: discardLogger()}
	auths, err := s.authMethods(t.Context())
	if err != nil {
		t.Fatalf("authMethods: %v", err)
	}
	if len(auths) != 1 {
		t.Errorf("got %d auth methods, want just the key", len(auths))
	}
}

func TestRootMustBeADirectory(t *testing.T) {
	ts := startServer(t)
	if err := os.WriteFile(ts.path("afile"), []byte("x"), 0o600); err != nil {
		t.Fatalf("writing: %v", err)
	}
	cfg := ts.config()
	cfg.Root = ts.path("afile")

	s, err := Open(t.Context(), cfg, discardLogger())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close() //nolint:errcheck // test
	if _, _, err := s.Stat(t.Context(), "x"); err == nil {
		t.Fatal("a root that is a file was accepted")
	} else if !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("error %q does not name the problem", err)
	}
}

// An empty root means the login directory. The test server's working directory is
// the process's, so this only asserts that resolution happens and produces an
// absolute path — not what that path is.
func TestEmptyRootResolvesToTheLoginDirectory(t *testing.T) {
	ts := startServer(t)
	cfg := ts.config()
	cfg.Root = ""

	s, err := Open(t.Context(), cfg, discardLogger())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close() //nolint:errcheck // test

	c, err := s.session(t.Context())
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	if !strings.HasPrefix(c.root, "/") {
		t.Errorf("root %q is not absolute", c.root)
	}
}

func TestDurationDecoding(t *testing.T) {
	var d Duration
	if err := d.UnmarshalText([]byte("45s")); err != nil {
		t.Fatalf("UnmarshalText: %v", err)
	}
	if got := (Config{Timeout: d}).timeout(); got.String() != "45s" {
		t.Errorf("timeout = %s", got)
	}
	// A bare number is refused rather than silently meaning nanoseconds.
	if err := d.UnmarshalText([]byte("45")); err == nil {
		t.Error("UnmarshalText accepted a bare number")
	}
	if got := (Config{}).timeout(); got != defaultTimeout {
		t.Errorf("an unset timeout = %s, want %s", got, defaultTimeout)
	}
}

// Cancellation is intent, not a fault: marking it retryable would make shutdown
// look like a transient failure and keep the engine retrying through it.
func TestCancellationIsNotRetryable(t *testing.T) {
	ts := startServer(t)
	s := ts.open(t)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := s.Put(ctx, "f.txt", strings.NewReader("x"))
	if err == nil {
		t.Skip("the operation completed before cancellation was observed")
	}
	if provider.IsRetryable(err) {
		t.Errorf("cancellation reported as retryable: %v", err)
	}
	if !errors.Is(err, ctx.Err()) {
		t.Errorf("error %v does not carry the context's", err)
	}
}

func discardLogger() *log.Logger { return log.New(io.Discard, "", 0) }
