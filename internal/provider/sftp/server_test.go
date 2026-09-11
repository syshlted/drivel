package sftp

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	libsftp "github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// The tests in this package run against a real SSH server carrying a real SFTP
// subsystem, in-process and over loopback, serving a real temporary directory.
//
// That is deliberate and it is the same argument the completion renderers make:
// an assertion that cannot fail is worse than none. A hand-written fake of an
// SFTP client would encode this package's own beliefs about the protocol — that a
// rename onto an existing name fails, that a write at an offset does not resize,
// that READDIR reports a symlink rather than its target — and those beliefs are
// exactly what the provider gets wrong. Here the server disagrees when we are
// wrong. It also means the connection, authentication and host-key paths are
// covered rather than stubbed out, which matters more here than anywhere else in
// the tree, because "host key verification fails closed" IS the milestone.

// testServer is a running SSH+SFTP server over loopback.
type testServer struct {
	addr       string
	root       string // the directory it serves
	knownHosts string // a known_hosts file naming addr
	keyFile    string // a client private key the server accepts

	mu    sync.Mutex
	conns []net.Conn // live server-side connections, so a test can cut them
}

// cutConnections drops every live connection, simulating the server going away
// mid-session. What the provider owes the engine after this is narrow and
// testable: a *retryable* error, and a working connection on the next attempt.
func (ts *testServer) cutConnections() {
	ts.mu.Lock()
	conns := ts.conns
	ts.conns = nil
	ts.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

func (ts *testServer) track(c net.Conn) {
	ts.mu.Lock()
	ts.conns = append(ts.conns, c)
	ts.mu.Unlock()
}

// startServer brings up the server and returns its coordinates. It stops when the
// test ends.
func startServer(t *testing.T) *testServer {
	t.Helper()

	hostSigner, _ := genKey(t)
	clientSigner, clientKeyPEM := genKey(t)

	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if string(key.Marshal()) != string(clientSigner.PublicKey().Marshal()) {
				return nil, errors.New("unknown public key")
			}
			return &ssh.Permissions{}, nil
		},
	}
	cfg.AddHostKey(hostSigner)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	root := t.TempDir()
	ts := &testServer{
		addr:       ln.Addr().String(),
		root:       root,
		knownHosts: writeKnownHosts(t, ln.Addr().String(), hostSigner.PublicKey()),
		keyFile:    writeFile(t, "client_key", clientKeyPEM),
	}

	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			ts.track(nc)
			go serveConn(nc, cfg)
		}
	}()
	return ts
}

// config returns a Config pointed at this server, rooted at its temp dir.
func (ts *testServer) config() Config {
	host, port := splitHostPort(ts.addr)
	no := false
	return Config{
		Host:       host,
		Port:       port,
		User:       "tester",
		Key:        ts.keyFile,
		KnownHosts: ts.knownHosts,
		Root:       ts.root,
		Agent:      &no, // never consult a developer's real agent
	}
}

// open builds a Store against this server.
func (ts *testServer) open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.Context(), ts.config(), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// path returns the on-disk path of a root-relative one, for asserting against the
// filesystem rather than through the provider.
func (ts *testServer) path(rel string) string { return filepath.Join(ts.root, filepath.FromSlash(rel)) }

func serveConn(nc net.Conn, cfg *ssh.ServerConfig) {
	sc, chans, reqs, err := ssh.NewServerConn(nc, cfg)
	if err != nil {
		_ = nc.Close()
		return
	}
	defer sc.Close() //nolint:errcheck // test server
	go ssh.DiscardRequests(reqs)
	for nch := range chans {
		if nch.ChannelType() != "session" {
			_ = nch.Reject(ssh.UnknownChannelType, "only sessions")
			continue
		}
		ch, chReqs, err := nch.Accept()
		if err != nil {
			return
		}
		go func() {
			for req := range chReqs {
				// "subsystem" carries a 4-byte length then the name.
				ok := req.Type == "subsystem" && len(req.Payload) > 4 && string(req.Payload[4:]) == "sftp"
				if req.WantReply {
					_ = req.Reply(ok, nil)
				}
				if ok {
					go func() {
						srv, err := libsftp.NewServer(ch)
						if err != nil {
							return
						}
						_ = srv.Serve()
						_ = srv.Close()
						_ = ch.Close()
					}()
				}
			}
		}()
	}
}

func genKey(t *testing.T) (ssh.Signer, []byte) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	blk, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatalf("marshalling key: %v", err)
	}
	return signer, pem.EncodeToMemory(blk)
}

func writeKnownHosts(t *testing.T, addr string, pub ssh.PublicKey) string {
	t.Helper()
	return writeFile(t, "known_hosts", []byte(knownhosts.Line([]string{addr}, pub)+"\n"))
}

func writeFile(t *testing.T, name string, b []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
	return p
}

func splitHostPort(addr string) (string, int) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		panic(err)
	}
	var n int
	for _, c := range port {
		n = n*10 + int(c-'0')
	}
	return host, n
}
