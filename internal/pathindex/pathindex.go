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
	"fmt"
	"strings"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
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

// Store is the bbolt-backed index. It is safe for concurrent use (bbolt
// serialises writers and gives readers a snapshot).
type Store struct {
	db *bolt.DB

	mu    sync.RWMutex
	bound bool
}

// Open opens (creating if needed) the index DB at path and ensures its buckets
// exist. The caller must Bind it before it returns anything, and must Close it.
func Open(path string) (*Store, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open index db %s: %w", path, err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
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
	return &Store{db: db}, nil
}

// Bind ties the store to an identity — the provider's account plus the root it
// maps the mount to — and reports whether the existing contents were kept.
//
// A different identity (or a schema bump) empties the store rather than failing:
// the data is a cache, so discarding it is always allowed, while reading another
// account's mappings would resolve paths to IDs that are not ours.
func (s *Store) Bind(identity string) (reused bool, err error) {
	want := []byte(schema + "\x00" + identity)
	err = s.db.Update(func(tx *bolt.Tx) error {
		meta := tx.Bucket(bucketMeta)
		if got := meta.Get(keyIdentity); bytes.Equal(got, want) {
			reused = true
			return nil
		}
		for _, b := range [][]byte{bucketByPath, bucketByID} {
			if err := tx.DeleteBucket(b); err != nil && err != bolt.ErrBucketNotFound {
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
func (s *Store) Set(path, id string) error {
	if !s.usable() {
		return nil
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return setLocked(tx, pathKey(path), []byte(id))
	})
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
	return s.db.Update(func(tx *bolt.Tx) error {
		for i, p := range paths {
			if err := setLocked(tx, pathKey(p), []byte(ids[i])); err != nil {
				return err
			}
		}
		return nil
	})
}

// Forget drops path and, if it names a directory, everything beneath it.
func (s *Store) Forget(path string) error {
	if !s.usable() {
		return nil
	}
	return s.db.Update(func(tx *bolt.Tx) error {
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
	return s.db.Update(func(tx *bolt.Tx) error {
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
