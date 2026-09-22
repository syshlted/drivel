// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package app

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // G401: a stand-in for Drive's md5Checksum, not a security property
	"encoding/hex"
	"io"
	"path"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/zishmusic/drivel/provider"
)

// memStore is a complete provider in memory: mutations, a cursor change feed and
// a flat enumeration. It exists so one test can thread mount → push → pull →
// reconcile through a single provider, which is the seam-crossing path the echo
// model (§4) is supposed to hold across and which was previously only ever
// tested in halves (DESIGN.md §9, M0 item 4).
type memStore struct {
	mu      sync.Mutex
	files   map[string]*memFile
	changes []provider.RemoteChange
	puts    map[string]int
	gets    map[string]int
}

type memFile struct {
	data     []byte
	isDir    bool
	version  int
	modified time.Time
}

func newMemStore() *memStore {
	return &memStore{files: map[string]*memFile{}, puts: map[string]int{}, gets: map[string]int{}}
}

var (
	_ provider.Store         = (*memStore)(nil)
	_ provider.ChangeSource  = (*memStore)(nil)
	_ provider.Enumerator    = (*memStore)(nil)
	_ provider.ContentHasher = (*memStore)(nil)
)

func hashOf(b []byte) string {
	sum := md5.Sum(b) //nolint:gosec // G401: see the import note
	return hex.EncodeToString(sum[:])
}

// viewLocked is the provider-agnostic view of one path.
func (s *memStore) viewLocked(p string, f *memFile) provider.RemoteFile {
	rf := provider.RemoteFile{
		Path:     p,
		IsDir:    f.isDir,
		Size:     int64(len(f.data)),
		Version:  strconv.Itoa(f.version),
		Modified: f.modified,
	}
	if !f.isDir {
		rf.Hash = hashOf(f.data)
	}
	return rf
}

// recordLocked appends to the change feed, which is what the pull loop reads.
func (s *memStore) recordLocked(p string, f *memFile) {
	if f == nil {
		s.changes = append(s.changes, provider.RemoteChange{Path: p, Removed: true})
		return
	}
	rf := s.viewLocked(p, f)
	s.changes = append(s.changes, provider.RemoteChange{Path: p, File: &rf})
}

// mkdirAllLocked creates missing ancestors, as the seam requires of Put/Mkdir.
func (s *memStore) mkdirAllLocked(p string) {
	if p == "" || p == "." {
		return
	}
	if _, ok := s.files[p]; ok {
		return
	}
	s.mkdirAllLocked(path.Dir(p))
	f := &memFile{isDir: true, version: 1, modified: time.Now()}
	s.files[p] = f
	s.recordLocked(p, f)
}

func (s *memStore) Put(_ context.Context, p string, r io.Reader) (provider.RemoteFile, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return provider.RemoteFile{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if dir := path.Dir(p); dir != "." {
		s.mkdirAllLocked(dir)
	}
	f := &memFile{data: b, version: 1, modified: time.Now()}
	if old, ok := s.files[p]; ok {
		f.version = old.version + 1
	}
	s.files[p] = f
	s.puts[p]++
	s.recordLocked(p, f)
	return s.viewLocked(p, f), nil
}

func (s *memStore) Mkdir(_ context.Context, p string) (provider.RemoteFile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mkdirAllLocked(p)
	return s.viewLocked(p, s.files[p]), nil
}

func (s *memStore) Move(_ context.Context, oldPath, newPath string) (provider.RemoteFile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.files[oldPath]
	if !ok {
		return provider.RemoteFile{}, provider.ErrNotExist
	}
	delete(s.files, oldPath)
	f.version++
	s.files[newPath] = f
	s.recordLocked(oldPath, nil)
	s.recordLocked(newPath, f)
	return s.viewLocked(newPath, f), nil
}

func (s *memStore) Remove(_ context.Context, p string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for name := range s.files {
		if name == p || (len(name) > len(p) && name[:len(p)] == p && name[len(p)] == '/') {
			delete(s.files, name)
			s.recordLocked(name, nil)
		}
	}
	return nil
}

func (s *memStore) Get(_ context.Context, p string) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.files[p]
	if !ok {
		return nil, provider.ErrNotExist
	}
	s.gets[p]++
	return io.NopCloser(bytes.NewReader(f.data)), nil
}

func (s *memStore) Stat(_ context.Context, p string) (provider.RemoteFile, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.files[p]
	if !ok {
		return provider.RemoteFile{}, false, nil
	}
	return s.viewLocked(p, f), true, nil
}

func (s *memStore) HashContent(r io.Reader) (string, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	return hashOf(b), nil
}

func (s *memStore) StartCursor(context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strconv.Itoa(len(s.changes)), nil
}

func (s *memStore) Changes(_ context.Context, cursor string) ([]provider.RemoteChange, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, err := strconv.Atoi(cursor)
	if err != nil || n < 0 || n > len(s.changes) {
		// A cursor this store cannot place is a dead cursor, and it says so rather
		// than quietly restarting from "now". Drive answers a token it no longer
		// retains with 410 and a malformed one with 400/pageToken, and gdrive
		// classifies both as ErrCursorExpired; treating it as "start from now"
		// instead would paper over the exact case M7b's re-enumeration exists for,
		// and would leave that recovery path unreachable from a test.
		return nil, "", provider.ErrCursorExpired
	}
	out := append([]provider.RemoteChange(nil), s.changes[n:]...)
	return out, strconv.Itoa(len(s.changes)), nil
}

// Enumerate returns everything in one page, which is all a small test needs; the
// paging and parent-parking cases live in the gdrive tests where they belong.
func (s *memStore) Enumerate(_ context.Context, _ string) ([]provider.RemoteFile, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	paths := make([]string, 0, len(s.files))
	for p := range s.files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	out := make([]provider.RemoteFile, 0, len(paths))
	for _, p := range paths {
		out = append(out, s.viewLocked(p, s.files[p]))
	}
	return out, "", nil
}

// -- test-side accessors --

func (s *memStore) content(p string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.files[p]
	if !ok {
		return nil, false
	}
	return append([]byte(nil), f.data...), true
}

func (s *memStore) putCount(p string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.puts[p]
}

func (s *memStore) getCount(p string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gets[p]
}

// seed adds a file without recording a change, standing in for content that was
// already on the remote before drivel ever ran — the case the enumeration sweep
// exists for (M7b).
func (s *memStore) seed(p string, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	before := len(s.changes)
	if dir := path.Dir(p); dir != "." {
		s.mkdirAllLocked(dir)
	}
	s.files[p] = &memFile{data: body, version: 1, modified: time.Now()}
	// Seeded content predates the feed, so drop whatever the ancestors recorded:
	// a sweep, not the change feed, is the only thing that can find it.
	s.changes = s.changes[:before]
}

// manifest is the remote's content: path -> digest, files only. It is the fourth
// party to a fleet's convergence check — three clients agreeing with each other
// proves they converged, not that they converged on what the provider holds.
func (s *memStore) manifest() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]string, len(s.files))
	for p, f := range s.files {
		if f.isDir {
			continue
		}
		out[p] = hashOf(f.data)
	}
	return out
}

// totalPuts and totalGets are the whole fleet's traffic, for the assertion that
// it stopped. Per-path counts answer "how did this file get here"; the totals
// answer "is anything still moving", which is the question a fixed point is.
func (s *memStore) totalPuts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.puts {
		n += c
	}
	return n
}

func (s *memStore) totalGets() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.gets {
		n += c
	}
	return n
}
