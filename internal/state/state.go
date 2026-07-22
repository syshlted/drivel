// Package state is the engine-level sync-state store: a single embedded bbolt DB
// holding the change-feed cursor and the echo-suppression records that keep the
// uploader and downloader from re-processing each other's writes (DESIGN.md §2.4,
// §4).
//
// It holds only *engine-level*, provider-agnostic state. A provider's private
// path↔native-ID translation (Drive's fileIDs) lives below the provider seam and
// is not stored here — see DESIGN.md §2.5.
//
// All methods are safe for concurrent use: the uploader records echoes while the
// downloader reads/updates them from a different goroutine. bbolt serialises
// writers and gives readers a consistent snapshot, so no extra locking is needed.
package state

import (
	"encoding/json"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"
)

var (
	bucketCursor = []byte("cursor")
	bucketEcho   = []byte("echo")
	keyCursor    = []byte("changefeed")
)

// Echo is what we last synced for a path, from either direction. A remote change
// whose content matches this record is our own echo (or a no-op) and is dropped
// by the downloader — the §4 loop breaker. Hash is a content checksum (Drive
// md5Checksum) when the provider exposes one; Version is the opaque fallback for
// objects with no checksum (e.g. Google-native docs have no md5).
type Echo struct {
	Hash    string    `json:"hash,omitempty"`
	Version string    `json:"version,omitempty"`
	At      time.Time `json:"at"`
}

// Matches reports whether an incoming (hash, version) pair is the content this
// record already represents — i.e. an echo to suppress. Hash is preferred; when
// both sides lack a hash we fall back to the opaque version.
func (e Echo) Matches(hash, version string) bool {
	if e.Hash != "" || hash != "" {
		return e.Hash == hash
	}
	return e.Version != "" && e.Version == version
}

// Store is the bbolt-backed sync-state store.
type Store struct {
	db *bolt.DB
}

// Open opens (creating if needed) the state DB at path and ensures its buckets
// exist. Callers must Close it.
func Open(path string) (*Store, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open state db %s: %w", path, err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, b := range [][]byte{bucketCursor, bucketEcho} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("init state buckets: %w", err)
	}
	return &Store{db: db}, nil
}

// Close flushes and closes the DB.
func (s *Store) Close() error { return s.db.Close() }

// Cursor returns the persisted change-feed cursor. ok is false on first run
// (nothing stored yet), signalling the caller to obtain a fresh start token.
func (s *Store) Cursor() (cursor string, ok bool, err error) {
	err = s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(bucketCursor).Get(keyCursor)
		if v != nil {
			cursor, ok = string(v), true
		}
		return nil
	})
	return cursor, ok, err
}

// SetCursor persists the cursor to resume from next poll.
func (s *Store) SetCursor(cursor string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketCursor).Put(keyCursor, []byte(cursor))
	})
}

// GetEcho returns the echo record for path, if any.
func (s *Store) GetEcho(path string) (e Echo, ok bool, err error) {
	err = s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(bucketEcho).Get([]byte(path))
		if v == nil {
			return nil
		}
		if uErr := json.Unmarshal(v, &e); uErr != nil {
			return uErr
		}
		ok = true
		return nil
	})
	return e, ok, err
}

// SetEcho records the content we last synced at path (from an upload or an
// applied download) so a later change-feed report of that same content is
// recognised as an echo and dropped.
func (s *Store) SetEcho(path string, e Echo) error {
	v, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketEcho).Put([]byte(path), v)
	})
}

// DeleteEcho drops the echo record for path (e.g. after the object is removed).
func (s *Store) DeleteEcho(path string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketEcho).Delete([]byte(path))
	})
}
