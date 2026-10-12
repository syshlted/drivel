// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

// Package pathindex is a persistent path↔native-ID map for providers whose API
// addresses objects by an opaque ID rather than by path — Drive's fileIDs today
// (DESIGN.md §2.5, §9 M7).
//
// It lives BELOW the provider seam. The sync engine never sees it, never learns
// what a native ID is, and holds no reference to it; a provider composes one
// privately. That is also why it is not part of internal/state, which is
// engine-level and provider-agnostic: this store's contents are meaningless
// outside the one provider (and the one account) that wrote them.
//
// # It is a cache, and the distinction is load-bearing
//
// Everything here is a *hint about the past*. The process that wrote an entry may
// have exited months ago, and the remote is free to have moved, replaced or
// deleted the object since. So a stored mapping is never authority: the provider
// must verify an entry against the remote before acting on it, and must be able
// to rebuild the mapping from scratch when it is missing or wrong. Deleting this
// file costs latency and API quota, never correctness — that property is the
// whole design constraint, and anything that makes the index load-bearing has
// broken it.
//
// # Binding
//
// An index is only meaningful for the account and root folder it was built
// against. Bind records that identity and wipes the store whenever it changes, so
// pointing a reused DB at a second account (or a different -drive-root) cannot
// resolve one account's paths to another's IDs. Until Bind succeeds the store
// reads as empty and drops writes: an unidentified index is treated as no index,
// which is the fail-safe direction.
package pathindex

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	bolt "go.etcd.io/bbolt"
	bolterrors "go.etcd.io/bbolt/errors"
)

// schema is bumped whenever the key encoding changes; it is folded into the
// identity, so a bump wipes older data instead of misreading it.
const schema = "v1"

var (
	bucketMeta   = []byte("meta")
	bucketByPath = []byte("path")
	bucketByID   = []byte("id")
	keyIdentity  = []byte("identity")
)

// boltOptions are the options this store opens with.
//
// FreelistType is set explicitly because the zero value is not the hashmap one:
// bbolt's default is the array freelist, which it serialises in full on every
// commit. A bbolt file never shrinks — deleted pages go on the freelist and are
// reused, but the file keeps its high-water mark — so after a mass delete leaves
// a few hundred thousand free pages, that array is megabytes written on every
// subsequent single-key write. The hashmap freelist also allocates in better than
// linear time when the free set is large and fragmented, which is exactly the
// state a mass delete leaves behind.
var boltOptions = &bolt.Options{
	Timeout:      5 * time.Second,
	FreelistType: bolt.FreelistMapType,
}

// Store is the bbolt-backed index. It is safe for concurrent use (bbolt
// serialises writers and gives readers a snapshot).
type Store struct {
	db *bolt.DB

	// commits counts write transactions. See update.
	commits atomic.Uint64

	// recovered records that Open replaced an unreadable file. See Recovered.
	recovered string

	mu    sync.RWMutex
	bound bool
}

// CorruptSuffix is appended to the path of an index DB that Open could not read
// and had to replace. Only the most recent one is kept.
const CorruptSuffix = ".corrupt"

// Open opens (creating if needed) the index DB at path and ensures its buckets
// exist. The caller must Bind it before it returns anything, and must Close it.
//
// A file bbolt cannot read at all is moved aside and replaced with an empty one.
// Nothing about that needs justifying beyond this package's own contract: the
// contents are a cache that must be rebuildable from scratch, and Bind already
// empties the store outright whenever the identity it was built against changes.
// The alternative is worse than it looks — the caller's fallback is to run from
// memory, which is correct but permanent, so a single bad write would cost every
// later session a cold index with only a log line to say why.
func Open(path string) (*Store, error) {
	db, aside, err := openOrReplace(path)
	if err != nil {
		return nil, err
	}
	s := &Store{db: db, recovered: aside}
	// Through s.update, not db.Update, so Commits() counts every commit this store
	// has made rather than every one after the first.
	err = s.update(func(tx *bolt.Tx) error {
		for _, b := range [][]byte{bucketMeta, bucketByPath, bucketByID} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("init index buckets: %w", err)
	}
	return s, nil
}

// Bind ties the store to an identity — the provider's account plus the root it
// maps the mount to — and reports whether the existing contents were kept.
//
// A different identity (or a schema bump) empties the store rather than failing:
// the data is a cache, so discarding it is always allowed, while reading another
// account's mappings would resolve paths to IDs that are not ours.
func (s *Store) Bind(identity string) (reused bool, err error) {
	want := []byte(schema + "\x00" + identity)
	// The overwhelmingly common case is an index being reused by the same account
	// and root, where there is nothing to write — and a write transaction that
	// changes nothing still costs ~20 KB and two fdatasyncs, because bbolt advances
	// the meta page regardless. Checking in a read transaction costs nothing. The
	// Update below re-checks, so this is a fast path and not the decision.
	if s.boundTo(want) {
		s.mu.Lock()
		s.bound = true
		s.mu.Unlock()
		return true, nil
	}
	err = s.update(func(tx *bolt.Tx) error {
		meta := tx.Bucket(bucketMeta)
		if got := meta.Get(keyIdentity); bytes.Equal(got, want) {
			reused = true
			return nil
		}
		for _, b := range [][]byte{bucketByPath, bucketByID} {
			if err := tx.DeleteBucket(b); err != nil && !errors.Is(err, bolterrors.ErrBucketNotFound) {
				return err
			}
			if _, err := tx.CreateBucket(b); err != nil {
				return err
			}
		}
		return meta.Put(keyIdentity, want)
	})
	if err != nil {
		return false, fmt.Errorf("bind index: %w", err)
	}
	s.mu.Lock()
	s.bound = true
	s.mu.Unlock()
	return reused, nil
}

// update runs fn in a write transaction and counts the commit.
//
// A bbolt commit is the unit of disk cost, and a far bigger one than it looks: it
// fdatasyncs twice and rewrites every page it touched — ~36 KB on a 10k-entry DB
// however small the key is, and flat as the DB grows. Counting commits is what
// makes that cost assertable in a test, deterministically, where a byte count
// would depend on the filesystem underneath. See Set for what the count bought.
func (s *Store) update(fn func(*bolt.Tx) error) error {
	if err := s.db.Update(fn); err != nil {
		return err
	}
	s.commits.Add(1)
	return nil
}

// Commits reports how many write transactions this store has committed.
func (s *Store) Commits() uint64 { return s.commits.Load() }

// Recovered names the file Open moved aside, or "" if the store opened normally.
// The caller logs it: this package has no logger.
func (s *Store) Recovered() string { return s.recovered }

// openOrReplace opens path, replacing it if and only if bbolt reports that it is
// not a readable database. It returns the path the old file was moved to.
func openOrReplace(path string) (*bolt.DB, string, error) {
	db, err := bolt.Open(path, 0o600, boltOptions)
	if err == nil {
		return db, "", nil
	}
	if !unreadable(err) {
		return nil, "", fmt.Errorf("open index db %s: %w", path, err)
	}
	aside := path + CorruptSuffix
	if rerr := os.Rename(path, aside); rerr != nil {
		return nil, "", fmt.Errorf("open index db %s: %w (moving it aside failed: %w)", path, err, rerr)
	}
	db, oerr := bolt.Open(path, 0o600, boltOptions)
	if oerr != nil {
		return nil, "", fmt.Errorf("replacing unreadable index db %s: %w", path, oerr)
	}
	return db, aside, nil
}

// unreadable reports whether err means the file is not a bbolt database we can
// read, as against any of the other reasons an open fails. The same three
// sentinels, for the same reason, as state.unreadable — kept separate because
// these are two independent leaf packages and the predicate is five lines.
//
// ErrTimeout must never be included: it means another process holds the file
// lock, and moving *that* file aside would pull the index out from under a live
// mount.
func unreadable(err error) bool {
	return errors.Is(err, bolterrors.ErrInvalid) ||
		errors.Is(err, bolterrors.ErrVersionMismatch) ||
		errors.Is(err, bolterrors.ErrChecksum)
}

// boundTo reports whether the stored identity already equals want. A read error
// reports false, so Bind takes the write path — the safe direction.
func (s *Store) boundTo(want []byte) bool {
	same := false
	_ = s.db.View(func(tx *bolt.Tx) error {
		same = bytes.Equal(tx.Bucket(bucketMeta).Get(keyIdentity), want)
		return nil
	})
	return same
}

// Close flushes and closes the DB.
func (s *Store) Close() error { return s.db.Close() }

// Len reports how many path mappings are stored, for logging.
func (s *Store) Len() (n int, err error) {
	if !s.usable() {
		return 0, nil
	}
	err = s.db.View(func(tx *bolt.Tx) error {
		n = tx.Bucket(bucketByPath).Stats().KeyN
		return nil
	})
	return n, err
}

func (s *Store) usable() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.bound
}

// Lookup returns the native ID recorded for a root-relative path.
func (s *Store) Lookup(path string) (id string, ok bool, err error) {
	if !s.usable() {
		return "", false, nil
	}
	err = s.db.View(func(tx *bolt.Tx) error {
		if v := tx.Bucket(bucketByPath).Get(pathKey(path)); v != nil {
			id, ok = string(v), true
		}
		return nil
	})
	return id, ok, err
}

// PathFor returns the root-relative path recorded for a native ID. It is the
// reverse direction the change feed needs: a feed entry names an ID, and
// resolving it by walking its parents costs one request per level.
func (s *Store) PathFor(id string) (path string, ok bool, err error) {
	if !s.usable() {
		return "", false, nil
	}
	err = s.db.View(func(tx *bolt.Tx) error {
		if v := tx.Bucket(bucketByID).Get([]byte(id)); v != nil {
			path, ok = unpathKey(v), true
		}
		return nil
	})
	return path, ok, err
}

// Set records path↔id, displacing whatever either side previously mapped to so
// the two directions stay exact inverses.
//
// Storing a mapping that is already stored is skipped, and the check is a read
// transaction rather than an early return inside the write one. That is not a
// micro-optimisation and the distinction is the whole point: bbolt advances the
// meta page on every commit whether or not a data page changed, so an Update
// that stores a byte-identical value costs the same ~36 KB and two fdatasyncs as
// a real write, and an Update that does nothing at all still costs ~20 KB. A
// View costs no disk writes at all.
//
// It pays off because the common case here is a mapping we already hold: every
// index *hit* goes through verify-then-remember, which re-stores exactly what it
// just read, and a provider that Stats a path before writing it resolves the
// same mapping twice per push.
//
// Both directions must already agree before the write can be skipped. setLocked
// evicts the two stale halves a rewrite leaves behind, so a half-match still has
// work to do; skipping it would leave the buckets as non-inverses, which is the
// state that later resolves an ID to a path it no longer occupies.
func (s *Store) Set(path, id string) error {
	if !s.usable() {
		return nil
	}
	pk, idb := pathKey(path), []byte(id)
	if s.holds(pk, idb) {
		return nil
	}
	return s.update(func(tx *bolt.Tx) error {
		return setLocked(tx, pk, idb)
	})
}

// holds reports whether both buckets already record pk↔id exactly, in both
// directions. A read error reports false, so the caller writes — the safe
// direction, since a redundant write costs disk and a skipped one costs
// correctness.
func (s *Store) holds(pk, id []byte) bool {
	same := false
	_ = s.db.View(func(tx *bolt.Tx) error {
		same = bytes.Equal(tx.Bucket(bucketByPath).Get(pk), id) &&
			bytes.Equal(tx.Bucket(bucketByID).Get(id), pk)
		return nil
	})
	return same
}

// setLocked writes one mapping inside tx, first evicting the two stale halves a
// rewrite can leave behind: the ID this path used to point at, and the path this
// ID used to live at. Without both, a renamed or replaced object leaves an
// orphaned reverse entry that later resolves an ID to a path it no longer has.
func setLocked(tx *bolt.Tx, pk, id []byte) error {
	byPath, byID := tx.Bucket(bucketByPath), tx.Bucket(bucketByID)
	if old := byPath.Get(pk); old != nil && !bytes.Equal(old, id) {
		if err := byID.Delete(old); err != nil {
			return err
		}
	}
	if oldPK := byID.Get(id); oldPK != nil && !bytes.Equal(oldPK, pk) {
		if err := byPath.Delete(oldPK); err != nil {
			return err
		}
	}
	if err := byPath.Put(pk, id); err != nil {
		return err
	}
	return byID.Put(id, pk)
}

// SetMany records a batch of path↔id mappings in ONE transaction.
//
// It exists for the M7b enumeration sweep, which learns the whole tree at once:
// Set per object would mean one bbolt commit — and one fsync — per file, turning
// a listing of a large Drive into minutes of disk sync. A page of a thousand
// objects costs one commit here. Entries are applied in order, with the same
// eviction of stale halves Set does.
func (s *Store) SetMany(paths []string, ids []string) error {
	if !s.usable() || len(paths) == 0 {
		return nil
	}
	if len(paths) != len(ids) {
		return fmt.Errorf("pathindex: %d paths vs %d ids", len(paths), len(ids))
	}
	// Same reasoning as Set, and it matters most here: a periodic -sweep-interval
	// re-enumeration of an unchanged tree re-offers every mapping it already
	// stored, one ~750 KB commit per page, for nothing.
	if s.holdsAll(paths, ids) {
		return nil
	}
	return s.update(func(tx *bolt.Tx) error {
		for i, p := range paths {
			if err := setLocked(tx, pathKey(p), []byte(ids[i])); err != nil {
				return err
			}
		}
		return nil
	})
}

// holdsAll reports whether every pair is already recorded in both directions, in
// one read transaction. A read error reports false, so the caller writes.
func (s *Store) holdsAll(paths, ids []string) bool {
	all := false
	_ = s.db.View(func(tx *bolt.Tx) error {
		byPath, byID := tx.Bucket(bucketByPath), tx.Bucket(bucketByID)
		for i, p := range paths {
			pk, id := pathKey(p), []byte(ids[i])
			if !bytes.Equal(byPath.Get(pk), id) || !bytes.Equal(byID.Get(id), pk) {
				return nil
			}
		}
		all = true
		return nil
	})
	return all
}

// Forget drops path and, if it names a directory, everything beneath it.
func (s *Store) Forget(path string) error {
	if !s.usable() {
		return nil
	}
	return s.update(func(tx *bolt.Tx) error {
		byPath, byID := tx.Bucket(bucketByPath), tx.Bucket(bucketByID)
		for _, e := range subtree(tx, path) {
			if err := byPath.Delete(e.key); err != nil {
				return err
			}
			if err := byID.Delete(e.id); err != nil {
				return err
			}
		}
		return nil
	})
}

// Rename rewrites the keys for oldPath and every descendant to sit under
// newPath, preserving their IDs — the index shape of a server-side move.
func (s *Store) Rename(oldPath, newPath string) error {
	if !s.usable() {
		return nil
	}
	return s.update(func(tx *bolt.Tx) error {
		byPath := tx.Bucket(bucketByPath)
		moved := subtree(tx, oldPath)
		// Delete every old key before writing the new ones: the two key ranges can
		// overlap (a move deeper into the same subtree is rejected by the FS, but a
		// sibling rename like a→ab is not), and deleting after writing would then
		// remove what was just written.
		for _, e := range moved {
			if err := byPath.Delete(e.key); err != nil {
				return err
			}
		}
		for _, e := range moved {
			rel := strings.TrimPrefix(unpathKey(e.key), oldPath)
			if err := setLocked(tx, pathKey(newPath+rel), e.id); err != nil {
				return err
			}
		}
		return nil
	})
}

type entry struct {
	key []byte // encoded path key
	id  []byte
}

// subtree collects path and all descendants of it, inside tx.
//
// Encoded keys sort so that a directory's children form one contiguous run
// immediately after it ("/a", "/a/b", then "/ab"), because '/' sorts below every
// character a path component can start with. The scan is therefore a seek plus a
// walk, not a full-bucket iteration — which matters when the index holds a large
// Drive rather than a handful of files.
func subtree(tx *bolt.Tx, path string) []entry {
	var out []entry
	b := tx.Bucket(bucketByPath)
	if id := b.Get(pathKey(path)); id != nil {
		out = append(out, entry{key: pathKey(path), id: append([]byte(nil), id...)})
	}
	prefix := pathKey(path)
	if path != "" {
		prefix = append(prefix, '/')
	}
	c := b.Cursor()
	for k, v := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, v = c.Next() {
		out = append(out, entry{
			key: append([]byte(nil), k...),
			id:  append([]byte(nil), v...),
		})
	}
	return out
}

// pathKey encodes a root-relative path as a bbolt key. The leading slash exists
// because bbolt rejects a zero-length key and the mount root is the empty path;
// it also makes the subtree prefix uniform ("/a/" for every path, root included).
func pathKey(p string) []byte { return []byte("/" + p) }

func unpathKey(k []byte) string { return strings.TrimPrefix(string(k), "/") }
