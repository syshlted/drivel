// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

// Command drivel-provider-fake is a drivel provider plugin that exists only to
// be launched by this repository's tests.
//
// It lives under testdata so that the go tool's `./...` never matches it: it is
// a main package that a test builds on demand, not part of the tree's build, and
// nothing ships it. That placement is the same decision M8 made about its
// pseudo-provider — the seam gets proven by something a test constructs, never by
// a second backend leaking into a user's binary.
//
// Everything it does is driven by the TOML settings the host sends it, so one
// binary covers every case the plugin tests need: which capabilities to offer,
// which call to fail and how, and which call to die on.
package main

import (
	"context"
	"crypto/md5" //nolint:gosec // G501: a stand-in for a backend's content digest, in a test fixture
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/syshlted/drivel/plugin"
	"github.com/syshlted/drivel/provider"
	"github.com/syshlted/drivel/ranges"
)

func main() { plugin.Serve(factory) }

// config is what a test writes into the mount's provider settings.
type config struct {
	// Capabilities names the optional interfaces this backend should offer. The
	// fake implements all of them, so it narrows the answer through
	// provider.Declarer — which is exactly what that interface is for, and what
	// the plugin proxy itself does.
	Capabilities []string `toml:"capabilities"`
	// Seed is initial content, as "path=text" entries.
	Seed []string `toml:"seed"`
	// Fail names an operation that should fail, and Kind how.
	Fail string `toml:"fail"`
	Kind string `toml:"kind"`
	// ExitOn names an operation that should kill this process instead of
	// answering, so a test can watch the host notice and relaunch.
	ExitOn string `toml:"exit-on"`
	// Marker is a file appended to on every Open, so a test can count launches
	// across a crash.
	Marker string `toml:"marker"`
	// OpenFails makes Open itself fail, for the "a backend that cannot start"
	// path.
	OpenFails bool `toml:"open-fails"`
	// Journal, if set, gets one line per mutating call. It is how a test running
	// a whole mount over this backend sees what reached it: the store itself is
	// in this process's memory, which the test cannot look into.
	Journal string `toml:"journal"`
	// Digest selects what HashContent returns: "md5" for the real thing in
	// lowercase hex, anything else for a length, which no real digest matches.
	//
	// The two exist so a test can drive both halves of the host's hash probe. A
	// backend whose digest the host can reproduce stops receiving file content
	// altogether; one whose digest it cannot keeps streaming. Which happened is
	// visible in the journal, since HashContent notes the byte count it was
	// actually given.
	Digest string `toml:"digest"`
}

func factory(_ context.Context, p provider.Params) (provider.Store, error) {
	var cfg config
	if err := p.Config.Decode(&cfg); err != nil {
		return nil, err
	}
	if cfg.OpenFails {
		return nil, errors.New("fake: refusing to open, as configured")
	}
	if cfg.Marker != "" {
		f, err := os.OpenFile(cfg.Marker, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, err
		}
		_, _ = f.WriteString("open\n")
		_ = f.Close()
	}
	caps, _ := provider.CapabilitySetFromNames(cfg.Capabilities)
	s := &fake{cfg: cfg, caps: caps, files: map[string][]byte{}}
	for _, seed := range cfg.Seed {
		path, body, _ := strings.Cut(seed, "=")
		s.files[path] = []byte(body)
	}
	if p.Log != nil {
		p.Log.Printf("fake backend open with %d seeded paths", len(s.files))
	}
	return s, nil
}

// retryable is an error the seam classifies as transient, built the way a real
// backend builds one: a type in the error chain answering Retryable.
type retryable struct{ error }

func (retryable) Retryable() bool { return true }

type fake struct {
	cfg  config
	caps provider.CapabilitySet

	mu    sync.Mutex
	files map[string][]byte
	dirs  map[string]bool
}

func (f *fake) Capabilities() provider.CapabilitySet { return f.caps }

// note appends one line to the journal, if there is one. A failure to write is
// ignored: this is a test aid, and making it fatal would turn a full disk into a
// confusing backend error.
func (f *fake) note(format string, args ...any) {
	if f.cfg.Journal == "" {
		return
	}
	jf, err := os.OpenFile(f.cfg.Journal, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(jf, format+"\n", args...)
	_ = jf.Close()
}

// gate applies the configured failure or exit for an operation.
func (f *fake) gate(op string) error {
	if f.cfg.ExitOn == op {
		// Not a panic: a panic unwinds and go-plugin would report it politely. The
		// case under test is a backend that stops existing.
		os.Exit(7)
	}
	if f.cfg.Fail != op {
		return nil
	}
	switch f.cfg.Kind {
	case "not-exist":
		return fmt.Errorf("fake %s: %w", op, provider.ErrNotExist)
	case "cursor-expired":
		return fmt.Errorf("fake %s: %w", op, provider.ErrCursorExpired)
	case "retryable":
		return retryable{fmt.Errorf("fake %s: try again", op)}
	default:
		return fmt.Errorf("fake %s: no", op)
	}
}

func (f *fake) Put(_ context.Context, path string, r io.Reader) (provider.RemoteFile, error) {
	if err := f.gate("put"); err != nil {
		return provider.RemoteFile{}, err
	}
	body, err := io.ReadAll(r)
	if err != nil {
		return provider.RemoteFile{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[path] = body
	f.note("put %s %d", path, len(body))
	return f.stat(path), nil
}

func (f *fake) Mkdir(_ context.Context, path string) (provider.RemoteFile, error) {
	if err := f.gate("mkdir"); err != nil {
		return provider.RemoteFile{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dirs == nil {
		f.dirs = map[string]bool{}
	}
	f.dirs[path] = true
	f.note("mkdir %s", path)
	return provider.RemoteFile{Path: path, IsDir: true, Modified: fixedTime}, nil
}

func (f *fake) Move(_ context.Context, oldPath, newPath string) (provider.RemoteFile, error) {
	if err := f.gate("move"); err != nil {
		return provider.RemoteFile{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	body, ok := f.files[oldPath]
	if !ok {
		return provider.RemoteFile{}, fmt.Errorf("fake: %q: %w", oldPath, provider.ErrNotExist)
	}
	delete(f.files, oldPath)
	f.files[newPath] = body
	f.note("move %s %s", oldPath, newPath)
	return f.stat(newPath), nil
}

func (f *fake) Remove(_ context.Context, path string) error {
	if err := f.gate("remove"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.files, path)
	f.note("remove %s", path)
	return nil
}

func (f *fake) Get(_ context.Context, path string) (io.ReadCloser, error) {
	if err := f.gate("get"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	body, ok := f.files[path]
	if !ok {
		return nil, fmt.Errorf("fake: %q: %w", path, provider.ErrNotExist)
	}
	return io.NopCloser(strings.NewReader(string(body))), nil
}

func (f *fake) Stat(_ context.Context, path string) (provider.RemoteFile, bool, error) {
	if err := f.gate("stat"); err != nil {
		return provider.RemoteFile{}, false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.files[path]; !ok {
		return provider.RemoteFile{}, false, nil
	}
	return f.stat(path), true, nil
}

// fixedTime is the modification time every fake object carries, so a test can
// assert that a timestamp survived the wire without depending on the clock.
var fixedTime = time.Date(2026, 9, 11, 12, 0, 0, 123456000, time.UTC)

// stat builds a RemoteFile. The caller holds f.mu.
func (f *fake) stat(path string) provider.RemoteFile {
	body := f.files[path]
	return provider.RemoteFile{
		Path:     path,
		Size:     int64(len(body)),
		Hash:     fmt.Sprintf("len:%d", len(body)),
		Version:  fmt.Sprintf("v%d", len(body)),
		Modified: fixedTime,
	}
}

// ---------------------------------------------------- optional capabilities

func (f *fake) StartCursor(context.Context) (string, error) {
	if err := f.gate("start-cursor"); err != nil {
		return "", err
	}
	return "cursor-0", nil
}

func (f *fake) Changes(_ context.Context, cursor string) ([]provider.RemoteChange, string, error) {
	if err := f.gate("changes"); err != nil {
		return nil, "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []provider.RemoteChange{}
	for _, p := range f.sorted() {
		rf := f.stat(p)
		out = append(out, provider.RemoteChange{Path: p, File: &rf})
	}
	out = append(out, provider.RemoteChange{Path: "gone.txt", Removed: true})
	return out, cursor + "+", nil
}

func (f *fake) Enumerate(context.Context, string) ([]provider.RemoteFile, string, error) {
	if err := f.gate("enumerate"); err != nil {
		return nil, "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []provider.RemoteFile
	for _, p := range f.sorted() {
		out = append(out, f.stat(p))
	}
	return out, "", nil
}

func (f *fake) GetRange(_ context.Context, path string, off, length int64) (io.ReadCloser, error) {
	if err := f.gate("get-range"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	body, ok := f.files[path]
	if !ok {
		return nil, fmt.Errorf("fake: %q: %w", path, provider.ErrNotExist)
	}
	if off > int64(len(body)) {
		off = int64(len(body))
	}
	body = body[off:]
	if length > 0 && length < int64(len(body)) {
		body = body[:length]
	}
	return io.NopCloser(strings.NewReader(string(body))), nil
}

// PutRange reads each extent out of the host's copy of the local file, in
// DESCENDING order. That is not decoration: provider.RangePutter promises an
// io.ReaderAt so an implementation may seek where it likes, and reading
// backwards is the cheapest way to prove the host really serves random access
// rather than a stream wearing a ReaderAt's clothes.
func (f *fake) PutRange(_ context.Context, path string, src io.ReaderAt, size int64, extents []ranges.Range) (provider.RemoteFile, error) {
	if err := f.gate("put-range"); err != nil {
		return provider.RemoteFile{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	body, ok := f.files[path]
	if !ok {
		return provider.RemoteFile{}, fmt.Errorf("fake: %q: %w", path, provider.ErrNotExist)
	}
	if int64(len(body)) != size {
		return provider.RemoteFile{}, fmt.Errorf("fake: %q is %d bytes, not %d", path, len(body), size)
	}
	patched := append([]byte(nil), body...)
	for i := len(extents) - 1; i >= 0; i-- {
		e := extents[i]
		buf := make([]byte, e.Len)
		if _, err := src.ReadAt(buf, e.Off); err != nil && !errors.Is(err, io.EOF) {
			return provider.RemoteFile{}, fmt.Errorf("fake: reading extent %d+%d: %w", e.Off, e.Len, err)
		}
		copy(patched[e.Off:], buf)
	}
	f.files[path] = patched
	return f.stat(path), nil
}

func (f *fake) HashContent(r io.Reader) (string, error) {
	if err := f.gate("hash-content"); err != nil {
		return "", err
	}
	h := md5.New() //nolint:gosec // G401: a stand-in for a backend's content digest, in a test fixture
	n, err := io.Copy(h, r)
	if err != nil {
		return "", err
	}
	// Noted even though hashing mutates nothing: the byte count is how a test sees
	// whether content reached this process at all, which is the whole question the
	// host's hash probe decides.
	f.note("hash %d", n)
	if f.cfg.Digest == "md5" {
		return hex.EncodeToString(h.Sum(nil)), nil
	}
	return fmt.Sprintf("len:%d", n), nil
}

// sorted lists the seeded paths in a stable order. The caller holds f.mu.
func (f *fake) sorted() []string {
	out := make([]string, 0, len(f.files))
	for p := range f.files {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

var (
	_ provider.Store         = (*fake)(nil)
	_ provider.Declarer      = (*fake)(nil)
	_ provider.ChangeSource  = (*fake)(nil)
	_ provider.Enumerator    = (*fake)(nil)
	_ provider.RangeGetter   = (*fake)(nil)
	_ provider.RangePutter   = (*fake)(nil)
	_ provider.ContentHasher = (*fake)(nil)
)
