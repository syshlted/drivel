// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"

	"github.com/syshlted/drivel/provider"
)

// faultStore is one peer's view of the fleet's shared memStore, with the faults
// Group V of docs/dev/multiclient-test-plan.md needs: a client cut off from the
// remote, a transfer that stops partway, and an enumeration that can be held open
// at a page boundary.
//
// It wraps rather than replaces, so the remote every peer agrees about is still
// the one memStore (and f.store.manifest() is still the fourth party to a
// convergence check). Everything not overridden below is promoted from the
// embedded store, which is also how the capability set stays identical to
// memStore's: a decorator that implemented fewer interfaces would silently change
// which inbound path the mount wires up, and the case would then be measuring the
// harness.
//
// What it deliberately does NOT model: a process that dies. Every fault here is a
// provider answering badly, which is the half a fake can be honest about; kill -9
// (MC-40, MC-41's literal form) needs a real process and stays Tier B.
type faultStore struct {
	*memStore

	mu       sync.Mutex
	down     error            // every remote call fails with this
	truncate map[string]int64 // Get yields this many bytes, then fails
	pageSize int              // Enumerate pages at this size; 0 = one page
	hold     chan struct{}    // closed to release a held enumeration
	held     chan struct{}    // closed once an enumeration has been held
	stalled  bool             // Put and Mkdir wait for their context: a provider that stopped answering
}

// stallWrites makes every Put and Mkdir wait until its context is cancelled — a
// provider that accepted the connection and then stopped answering, which is the
// shape that fills the engine's queues and holds the run loop in a send.
func (s *faultStore) stallWrites() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stalled = true
}

func (s *faultStore) stall(ctx context.Context) error {
	s.mu.Lock()
	on := s.stalled
	s.mu.Unlock()
	if !on {
		return nil
	}
	<-ctx.Done()
	return ctx.Err()
}

// offlineError is what a partitioned peer's calls fail with. Retryable, because
// that is what an outage is to every real provider in the tree — both
// gdrive.transientError and sftp.transientError report true — and because the
// engine and the downloader each take a visibly different branch for the other
// kind. A permanent error here would exercise neither the retry budget nor the
// pull loop's indefinite re-poll, which is the asymmetry MC-44b is about.
type offlineError struct{ op string }

func (e offlineError) Error() string   { return "faultStore: " + e.op + ": the network is down" }
func (offlineError) Retryable() bool   { return true }
func (e offlineError) Unwrap() error   { return errOffline }
func (e offlineError) Is(t error) bool { return t == errOffline }

// errOffline is the sentinel an assertion can match without depending on the
// wording of the message above.
var errOffline = errors.New("faultStore: offline")

// -- fault control. Every setter is safe to call while the peer is running, which
// is the point: a partition that could only be configured before Open would never
// catch a mount mid-flight.

// goOffline fails every remote call until goOnline. The peer keeps serving its
// mountpoint throughout — nothing below the FUSE path touches the provider — so
// this is "this laptop lost the network", not "drivel stopped".
func (s *faultStore) goOffline() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.down = offlineError{op: "offline"}
}

func (s *faultStore) goOnline() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.down = nil
}

// truncateGet makes Get(path) hand back n bytes of the real body and then fail.
//
// It fails on the read side, which is a transfer that dropped. MC-46's own fault
// is ENOSPC, which fails on the write side instead — but both reach
// Downloader.download as an error out of the same io.Copy, and what the case is
// actually about is what the downloader does with a transfer that stopped partway:
// whether a short file can end up at the real path, and whether anything later
// pushes it back up as the remote's new content. The filesystem half (a real full
// disk, and whether ENOSPC is reported rather than swallowed) stays Tier B.
func (s *faultStore) truncateGet(path string, n int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.truncate == nil {
		s.truncate = map[string]int64{}
	}
	s.truncate[path] = n
}

func (s *faultStore) healGet(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.truncate, path)
}

// pageEnumeration makes Enumerate hand back size objects at a time. The sweep
// persists its cursor after every page (syncengine.runSweep), so paging is what
// makes an interruption resumable at all — with one page there is nothing to
// resume from and MC-41 cannot be posed.
func (s *faultStore) pageEnumeration(size int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pageSize = size
}

// holdAfterFirstPage blocks the enumeration at its first page boundary — the
// second Enumerate call, the one asking for page two — and returns a channel that
// is closed when that happens. releaseHold lets it continue; cancelling the
// peer's context releases it too, which is what stopping a peer mid-sweep does.
func (s *faultStore) holdAfterFirstPage() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hold = make(chan struct{})
	s.held = make(chan struct{})
	return s.held
}

func (s *faultStore) releaseHold() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hold != nil {
		close(s.hold)
		s.hold = nil
	}
}

// -- the faults themselves

func (s *faultStore) check(op string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.down != nil {
		return offlineError{op: op}
	}
	return nil
}

func (s *faultStore) Put(ctx context.Context, p string, r io.Reader) (provider.RemoteFile, error) {
	if err := s.check("put " + p); err != nil {
		return provider.RemoteFile{}, err
	}
	if err := s.stall(ctx); err != nil {
		return provider.RemoteFile{}, err
	}
	return s.memStore.Put(ctx, p, r)
}

func (s *faultStore) Mkdir(ctx context.Context, p string) (provider.RemoteFile, error) {
	if err := s.check("mkdir " + p); err != nil {
		return provider.RemoteFile{}, err
	}
	if err := s.stall(ctx); err != nil {
		return provider.RemoteFile{}, err
	}
	return s.memStore.Mkdir(ctx, p)
}

func (s *faultStore) Move(ctx context.Context, oldPath, newPath string) (provider.RemoteFile, error) {
	if err := s.check("move " + oldPath); err != nil {
		return provider.RemoteFile{}, err
	}
	return s.memStore.Move(ctx, oldPath, newPath)
}

func (s *faultStore) Remove(ctx context.Context, p string) error {
	if err := s.check("remove " + p); err != nil {
		return err
	}
	return s.memStore.Remove(ctx, p)
}

func (s *faultStore) Stat(ctx context.Context, p string) (provider.RemoteFile, bool, error) {
	if err := s.check("stat " + p); err != nil {
		return provider.RemoteFile{}, false, err
	}
	return s.memStore.Stat(ctx, p)
}

func (s *faultStore) StartCursor(ctx context.Context) (string, error) {
	if err := s.check("start cursor"); err != nil {
		return "", err
	}
	return s.memStore.StartCursor(ctx)
}

func (s *faultStore) Changes(ctx context.Context, cursor string) ([]provider.RemoteChange, string, error) {
	if err := s.check("changes"); err != nil {
		return nil, "", err
	}
	return s.memStore.Changes(ctx, cursor)
}

func (s *faultStore) Get(ctx context.Context, p string) (io.ReadCloser, error) {
	if err := s.check("get " + p); err != nil {
		return nil, err
	}
	rc, err := s.memStore.Get(ctx, p)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	n, short := s.truncate[p]
	s.mu.Unlock()
	if !short {
		return rc, nil
	}
	return &shortReader{rc: rc, left: n, path: p}, nil
}

// Enumerate pages the embedded store's single page, and holds at a page boundary
// when a test asked it to.
//
// The cursor is an offset into the sorted listing, which is a cursor a *provider*
// would not get away with — content created between pages shifts it. That is
// deliberately not modelled: this exists to make the sweep resumable, and the
// cases that use it keep the remote still while a sweep is in flight.
func (s *faultStore) Enumerate(ctx context.Context, cursor string) ([]provider.RemoteFile, string, error) {
	if err := s.check("enumerate"); err != nil {
		return nil, "", err
	}
	s.mu.Lock()
	hold, held, size := s.hold, s.held, s.pageSize
	s.mu.Unlock()

	if hold != nil && cursor != "" {
		if held != nil {
			// Signal once; a second page boundary in the same test would otherwise
			// close a closed channel.
			select {
			case <-held:
			default:
				close(held)
			}
		}
		select {
		case <-hold:
		case <-ctx.Done():
			// What stopping a peer mid-sweep looks like from inside the provider.
			// runSweep turns this into an error, start() sees the cancelled context
			// and returns, and the sweep record stays on disk unfinished.
			return nil, "", ctx.Err()
		}
	}

	all, _, err := s.memStore.Enumerate(ctx, "")
	if err != nil || size <= 0 {
		return all, "", err
	}
	from := 0
	if cursor != "" {
		if from, err = strconv.Atoi(cursor); err != nil {
			return nil, "", fmt.Errorf("faultStore: unplaceable page cursor %q", cursor)
		}
	}
	if from > len(all) {
		from = len(all)
	}
	to := min(from+size, len(all))
	next := ""
	if to < len(all) {
		next = strconv.Itoa(to)
	}
	return all[from:to], next, nil
}

// shortReader yields left bytes of the real body and then fails, as a transfer
// that dropped mid-stream does. The error is retryable for the same reason
// offlineError's is.
type shortReader struct {
	rc   io.ReadCloser
	left int64
	path string
}

func (r *shortReader) Read(p []byte) (int, error) {
	if r.left <= 0 {
		return 0, offlineError{op: "get " + r.path + ": transfer dropped"}
	}
	if int64(len(p)) > r.left {
		p = p[:r.left]
	}
	n, err := r.rc.Read(p)
	r.left -= int64(n)
	return n, err
}

func (r *shortReader) Close() error { return r.rc.Close() }

var (
	_ provider.Store         = (*faultStore)(nil)
	_ provider.ChangeSource  = (*faultStore)(nil)
	_ provider.Enumerator    = (*faultStore)(nil)
	_ provider.ContentHasher = (*faultStore)(nil)
)
