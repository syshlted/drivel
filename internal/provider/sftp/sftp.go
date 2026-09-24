// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

// Package sftp implements provider.Store over SSH's file transfer subsystem
// (M18, DESIGN.md §9).
//
// It is the mirror image of the Drive provider, and every difference follows from
// one fact: SFTP is natively path-addressed. The seam is already path-addressed
// (DESIGN.md §2.5), so each Store method is one protocol call and there is no
// identity to invent — which is why nothing here resembles gdrive's index.go, and
// why internal/pathindex stays Drive-private. A path index over a path-addressed
// store would be a cache keyed by its own value.
//
// Three consequences of that shape, all of them stated once in the M17–M21
// preamble and all of them visible in this package:
//
//   - There is no change feed, so this store implements provider.Enumerator and
//     not provider.ChangeSource, and the M7b sweep is the whole inbound path
//     rather than a safety net under a feed. -sweep-interval is therefore the
//     poll interval, and its 24h default is wrong here (see docs/user/sftp.md).
//   - A write at an offset is the protocol's native operation, so provider.
//     RangePutter is real — this is the first backend in the tree where M6's
//     range-write path runs against a server rather than a fake.
//   - No server-computed digest is available, so provider.ContentHasher is NOT
//     implemented and M6's unchanged-content gate declines. See the note above
//     RemoteFile assembly in stat.
package sftp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/syshlted/drivel/provider"
)

// defaultPort is SSH's, and the only sensible default.
const defaultPort = 22

// defaultTimeout bounds the TCP connect and the SSH handshake. It is not a bound
// on a transfer: a slow upload is not a failure, and the ChunkTransferTimeout
// mistake gdrive documents — a deadline that never resets on progress silently
// capping the slowest link that can ever succeed — is the same mistake here.
const defaultTimeout = 30 * time.Second

// tempPrefix names an upload in progress. See Store.Put for why uploads land on a
// temporary name first, and Store.Enumerate for why the sweep steps over them.
const tempPrefix = ".drivel-upload."

// Config is what an SFTP provider needs to open. The toml tags are the keys a
// `[account.NAME]` table in drivel's config file uses (M8) — spelled explicitly
// rather than left to the decoder's case-insensitive field matching, which would
// spell KnownHosts as "knownhosts".
type Config struct {
	// Host is the server, without a port.
	Host string `toml:"host"`
	// Port defaults to 22.
	Port int `toml:"port"`
	// User is the SSH account to log in as. Required: guessing it from the local
	// username would make the same config file mean different things on different
	// machines, and an fstab mount runs as whoever mounted it.
	User string `toml:"user"`

	// Key is a PEM private key file. It must not be passphrase-protected — see
	// authMethods for why there is no setting to supply one.
	Key string `toml:"key"`
	// Certificate pairs a signed certificate with Key.
	Certificate string `toml:"certificate"`
	// Agent uses $SSH_AUTH_SOCK. It is a pointer so that an explicit `agent =
	// false` is distinguishable from an absent key (M8 rule 6): unset means "use
	// the agent if there is one", while false means "do not", and collapsing the
	// two would make a deliberate opt-out silently do nothing.
	Agent *bool `toml:"agent"`

	// KnownHosts is the OpenSSH known_hosts file to verify the server against.
	// Defaults to ~/.ssh/known_hosts. There is no way to disable verification.
	KnownHosts string `toml:"known-hosts"`

	// Root is the remote directory the mount root maps to. Empty means the login
	// directory; a relative path resolves against it.
	Root string `toml:"root"`

	// Timeout bounds connecting, not transferring. Zero means defaultTimeout.
	Timeout Duration `toml:"connect-timeout"`
	// Concurrency is how many read/write packets may be in flight for a single
	// file — what turns one transfer from latency-bound into bandwidth-bound over
	// a long link. Zero takes the library's (conservative) default.
	Concurrency int `toml:"concurrent-requests"`
}

// Duration is a time.Duration that decodes from a TOML string ("30s"), because
// TOML has no duration type and a bare integer would silently mean nanoseconds.
type Duration time.Duration

// UnmarshalText satisfies the TOML decoder.
func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return fmt.Errorf("not a duration (want e.g. \"30s\"): %w", err)
	}
	*d = Duration(v)
	return nil
}

func (c Config) port() int {
	if c.Port == 0 {
		return defaultPort
	}
	return c.Port
}

func (c Config) addr() string { return c.Host + ":" + strconv.Itoa(c.port()) }

func (c Config) timeout() time.Duration {
	if c.Timeout <= 0 {
		return defaultTimeout
	}
	return time.Duration(c.Timeout)
}

func (c Config) useAgent() bool { return c.Agent == nil || *c.Agent }

func (c Config) knownHosts() string {
	if c.KnownHosts != "" {
		return c.KnownHosts
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".ssh/known_hosts"
	}
	return path.Join(home, ".ssh", "known_hosts")
}

// validate checks everything that can be checked without the network.
func (c Config) validate() error {
	switch {
	case c.Host == "":
		return errors.New("no host configured")
	case strings.Contains(c.Host, ":"):
		return fmt.Errorf("host %q must not carry a port; use port = %s", c.Host, "N")
	case c.User == "":
		return errors.New("no user configured")
	case c.Port < 0 || c.Port > 65535:
		return fmt.Errorf("port %d is out of range", c.Port)
	case c.Concurrency < 0:
		return fmt.Errorf("concurrent-requests %d must not be negative", c.Concurrency)
	case c.Certificate != "" && c.Key == "":
		return errors.New("certificate is set but key is not: a certificate signs a key, it does not replace one")
	}
	return nil
}

// Store is the SFTP provider.
//
// It is safe for concurrent use: the SFTP client itself is (one session
// multiplexes many requests), and the mutex here guards only the cached
// connection pointer, never an operation.
type Store struct {
	cfg Config
	lg  *log.Logger

	mu   sync.Mutex
	conn *conn // nil until first use, and after a connection is dropped
}

var (
	_ provider.Store       = (*Store)(nil)
	_ provider.Enumerator  = (*Store)(nil)
	_ provider.RangeGetter = (*Store)(nil)
	_ provider.RangePutter = (*Store)(nil)
)

// provider.ChangeSource is deliberately NOT implemented, and the absence is the
// design rather than an omission to fill in later.
//
// SFTP has no change notification of any kind: no cursor, no feed, nothing to
// subscribe to. The tempting substitute — poll the tree, diff it against last
// time, synthesise changes — is exactly what the M7b sweep already is, except
// that the sweep does it with the delete guards and the baseline rules that make
// an inferred deletion safe (a delete only from a baseline that predates the
// sweep, a diverged local copy kept and pushed back, -max-deletes abandoning a
// pass it cannot justify). A hand-rolled differ here would reproduce those or,
// far more likely, quietly not.
//
// provider.ContentHasher is absent for a different reason: not "no honest
// implementation is possible" but "none is possible today". A server-computed
// digest needs the check-file extension from the filexfer draft, which OpenSSH's
// server does not implement and which github.com/pkg/sftp offers no way to send —
// the client has no API for arbitrary extended requests. So Stat leaves Hash
// empty, and the engine's gate 3 short-circuits on that before it reads a single
// local byte (see syncengine.contentMatches), which makes the absence free rather
// than merely safe. Content pushes are therefore whole-file whenever gate 2
// declines. If the extension ever becomes reachable, this is a per-*session*
// capability — one server advertises it and the next does not — so the interface
// has to be satisfied by a wrapper type chosen at dial time, not by adding a
// method to Store.

// Open connects nothing and returns a Store that logs to lg (nil => the default
// logger). Everything checkable offline is checked here; the network is not
// touched until the first operation.
//
// Not dialling at open is deliberate and follows §2.1. A mount must come up
// whether or not the server is reachable — an fstab mount races the network at
// boot, and drivel's whole premise is that filesystem operations do not block on
// it. What that would normally cost is a late, obscure failure for a
// misconfiguration; checkKnownHost buys most of it back by proving offline that
// this host could ever be trusted.
func Open(ctx context.Context, cfg Config, lg *log.Logger) (*Store, error) {
	if lg == nil {
		lg = log.Default()
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	s := &Store{cfg: cfg, lg: lg}
	if _, err := s.clientConfig(ctx); err != nil {
		return nil, err
	}
	if err := s.checkKnownHost(); err != nil {
		return nil, err
	}
	return s, nil
}

// Close releases the connection, if one was ever made.
func (s *Store) Close() error {
	s.mu.Lock()
	c := s.conn
	s.conn = nil
	s.mu.Unlock()
	if c == nil {
		return nil
	}
	return c.close()
}

// do runs one operation against the live session, dialling if needed, and applies
// the two policies every call shares: a dead session is discarded so the next
// attempt reconnects, and the error is classified so the engine knows whether to
// retry.
func do[T any](ctx context.Context, s *Store, fn func(c *conn) (T, error)) (T, error) {
	var zero T
	c, err := s.session(ctx)
	if err != nil {
		return zero, err
	}
	// The library's calls take no context, so cancellation is observed on either
	// side of a call rather than during one. Shutdown still terminates promptly:
	// Close tears the transport down under any call in flight.
	v, err := fn(c)
	if err != nil {
		if sessionDead(err) {
			s.drop(c)
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			// A call that failed because we are shutting down must not read as a
			// provider fault the engine should retry.
			return zero, ctxErr
		}
		return zero, classify(err)
	}
	return v, nil
}

// --- provider.Store ---------------------------------------------------------

// Put creates or replaces the file at p, creating any missing parents.
//
// The content lands on a temporary name in the destination directory and is
// renamed into place once it is complete. That costs one extra round trip and
// buys the property that matters most on this backend: the path never holds a
// half-written file. A whole-file Put is the fallback for every M6 gate, so it
// runs constantly, and an interrupted one writing in place would leave the remote
// truncated — with no server-side digest (see the ContentHasher note above),
// nothing downstream would ever notice. The engine records its echo from what
// this returns, so a failed Put records nothing and the next sweep re-pushes.
func (s *Store) Put(ctx context.Context, p string, r io.Reader) (provider.RemoteFile, error) {
	return do(ctx, s, func(c *conn) (provider.RemoteFile, error) {
		abs := c.abs(p)
		dir := path.Dir(abs)
		if err := c.cli.MkdirAll(dir); err != nil {
			return provider.RemoteFile{}, fmt.Errorf("creating %s: %w", dir, err)
		}
		tmp := path.Join(dir, tempPrefix+path.Base(abs))
		f, err := c.cli.Create(tmp)
		if err != nil {
			return provider.RemoteFile{}, fmt.Errorf("creating %s: %w", tmp, err)
		}
		if _, err := f.ReadFrom(r); err != nil {
			_ = f.Close()
			_ = c.cli.Remove(tmp)
			return provider.RemoteFile{}, fmt.Errorf("writing %s: %w", tmp, err)
		}
		if err := f.Close(); err != nil {
			_ = c.cli.Remove(tmp)
			return provider.RemoteFile{}, fmt.Errorf("closing %s: %w", tmp, err)
		}
		if err := c.rename(tmp, abs); err != nil {
			_ = c.cli.Remove(tmp)
			return provider.RemoteFile{}, fmt.Errorf("renaming %s into place: %w", tmp, err)
		}
		return c.statFile(p, abs)
	})
}

// Mkdir creates the directory at p and any missing ancestors.
func (s *Store) Mkdir(ctx context.Context, p string) (provider.RemoteFile, error) {
	return do(ctx, s, func(c *conn) (provider.RemoteFile, error) {
		abs := c.abs(p)
		if err := c.cli.MkdirAll(abs); err != nil {
			return provider.RemoteFile{}, fmt.Errorf("creating %s: %w", abs, err)
		}
		return c.statFile(p, abs)
	})
}

// Move renames oldPath to newPath, creating the destination's parents.
//
// A source the server does not have becomes provider.ErrNotExist, which the
// engine turns into "upload the destination as fresh content" — the right answer
// for a rename of a file that was never pushed.
func (s *Store) Move(ctx context.Context, oldPath, newPath string) (provider.RemoteFile, error) {
	return do(ctx, s, func(c *conn) (provider.RemoteFile, error) {
		oldAbs, newAbs := c.abs(oldPath), c.abs(newPath)
		if _, err := c.cli.Lstat(oldAbs); err != nil {
			if isNotExist(err) {
				return provider.RemoteFile{}, provider.ErrNotExist
			}
			return provider.RemoteFile{}, err
		}
		dir := path.Dir(newAbs)
		if err := c.cli.MkdirAll(dir); err != nil {
			return provider.RemoteFile{}, fmt.Errorf("creating %s: %w", dir, err)
		}
		if err := c.rename(oldAbs, newAbs); err != nil {
			return provider.RemoteFile{}, fmt.Errorf("renaming %s to %s: %w", oldAbs, newAbs, err)
		}
		return c.statFile(newPath, newAbs)
	})
}

// Remove deletes p, recursively for a directory.
//
// **There is no recoverable form on this backend and that is not a default that
// can be changed.** gdrive answers the seam's "prefer the recoverable form" by
// trashing, so a reconcile-inferred deletion the -max-deletes cap failed to catch
// is still recoverable for 30 days. SFTP has no trash, and inventing one — a
// hidden directory the deleted files are moved into — would sit inside the mount
// root, where the sweep would enumerate it and pull every deleted file back down.
// So on this provider -max-deletes is not the guard *in front of* a safety net;
// it is the only guard there is. docs/user/sftp.md says so in those words.
//
// A path that is already gone succeeds. The engine can reach here twice for one
// removal (a delete observed by the mount and again inferred by a sweep), and a
// failure the second time would be a retry loop over an outcome already achieved.
func (s *Store) Remove(ctx context.Context, p string) error {
	_, err := do(ctx, s, func(c *conn) (struct{}, error) {
		abs := c.abs(p)
		if p == "" {
			// The mount root itself. Nothing above ever asks for this, and honouring
			// it would delete the share rather than its contents.
			return struct{}{}, errors.New("refusing to remove the mount root")
		}
		if err := c.cli.RemoveAll(abs); err != nil && !isNotExist(err) {
			return struct{}{}, fmt.Errorf("removing %s: %w", abs, err)
		}
		return struct{}{}, nil
	})
	return err
}

// Get opens p for reading. The returned reader holds a remote file handle and
// must be closed.
func (s *Store) Get(ctx context.Context, p string) (io.ReadCloser, error) {
	return do(ctx, s, func(c *conn) (io.ReadCloser, error) {
		f, err := c.cli.Open(c.abs(p))
		if err != nil {
			return nil, fmt.Errorf("opening %s: %w", c.abs(p), err)
		}
		return f, nil
	})
}

// Stat reports the object at p; ok is false if it does not exist.
func (s *Store) Stat(ctx context.Context, p string) (provider.RemoteFile, bool, error) {
	type result struct {
		rf provider.RemoteFile
		ok bool
	}
	res, err := do(ctx, s, func(c *conn) (result, error) {
		rf, err := c.statFile(p, c.abs(p))
		if err != nil {
			if isNotExist(err) {
				return result{}, nil
			}
			return result{}, err
		}
		return result{rf, true}, nil
	})
	return res.rf, res.ok, err
}

// statFile builds the seam's view of one object.
func (c *conn) statFile(p, abs string) (provider.RemoteFile, error) {
	fi, err := c.cli.Stat(abs)
	if err != nil {
		return provider.RemoteFile{}, err
	}
	return remoteFile(p, fi), nil
}

// remoteFile converts a stat result into the seam's RemoteFile.
//
// Hash is left empty: there is no server-computed digest to put in it, and a
// digest we computed ourselves by downloading the file would defeat the only
// thing the field is for. See the ContentHasher note above.
//
// Version is what echo suppression (§4) and M6's gate 2 both compare on, so with
// no digest it has to carry the identity by itself, and it does so as
// modification time and size. That is the rsync heuristic and it inherits the
// rsync caveat: SFTP reports mtime in whole seconds, so a remote edit that
// preserves a file's exact byte length *and* lands in the same second as the
// version we recorded is indistinguishable from our own echo. The consequence is
// bounded — that edit is not pulled until something else touches the file — and
// the alternatives are worse: a size-only version misses far more, and hashing
// means downloading every file on every sweep. It is documented in
// docs/user/sftp.md rather than hidden here.
func remoteFile(p string, fi os.FileInfo) provider.RemoteFile {
	rf := provider.RemoteFile{
		Path:     p,
		IsDir:    fi.IsDir(),
		Size:     fi.Size(),
		Modified: fi.ModTime(),
	}
	if fi.IsDir() {
		// A directory's whole content, as far as this seam is concerned, is that it
		// exists — so its version is constant. Leaving it empty instead is what the
		// first draft did, and it made state.Echo.Matches answer false for every
		// directory on every pass: the downloader then re-created and re-logged
		// each one, which is invisible at Drive's 24h sweep and is the entire log
		// at a poll interval of seconds. Its mtime would be no better, since that
		// moves whenever a child is added.
		rf.Version = dirVersion
		return rf
	}
	rf.Version = strconv.FormatInt(fi.ModTime().Unix(), 10) + ":" + strconv.FormatInt(fi.Size(), 10)
	return rf
}

// dirVersion is the constant version every directory reports. It cannot collide
// with a file's, which always contains a colon-separated size.
const dirVersion = "dir"

// isTemp reports whether name is one of this provider's in-progress uploads.
func isTemp(name string) bool { return strings.HasPrefix(name, tempPrefix) }
