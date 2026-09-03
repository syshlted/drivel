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
	bucketCursor    = []byte("cursor")
	bucketEcho      = []byte("echo")
	bucketHydration = []byte("hydration")
	bucketSweep     = []byte("sweep")
	bucketSeen      = []byte("seen")
	keyCursor       = []byte("changefeed")
	keySweep        = []byte("current")
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
		for _, b := range [][]byte{bucketCursor, bucketEcho, bucketHydration, bucketSweep, bucketSeen} {
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

// --- hydration (M5) ---------------------------------------------------------
//
// The hydration bucket caches per-path present-ranges bitmaps for lazy hydration.
// It is deliberately opaque here — the encoding belongs to internal/hydrate, which
// keeps this package free of any dependency on the hydration model. It is only a
// CACHE: the authoritative placeholder marker is an xattr on the backing file, so
// losing this bucket costs a stat, not correctness.

// Hydration returns the cached range bitmap for path, if one is stored.
func (s *Store) Hydration(path string) (v []byte, ok bool, err error) {
	err = s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketHydration).Get([]byte(path))
		if b == nil {
			return nil
		}
		// bbolt values are only valid inside the transaction; copy before escaping.
		v = append([]byte(nil), b...)
		ok = true
		return nil
	})
	return v, ok, err
}

// SetHydration stores the range bitmap for path.
func (s *Store) SetHydration(path string, v []byte) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketHydration).Put([]byte(path), v)
	})
}

// DeleteHydration drops the range bitmap for path (fully hydrated, or removed).
func (s *Store) DeleteHydration(path string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketHydration).Delete([]byte(path))
	})
}

// --- enumeration sweep (M7b) ------------------------------------------------
//
// A sweep is one pass of provider.Enumerator over the whole remote tree, plus the
// reconcile it feeds. Three facts have to survive a restart for that to be safe:
//
//   - Token: the change-feed start token taken BEFORE the sweep began. It is
//     handed to the pull loop only once the sweep completes. The other order
//     loses every change made while the sweep was running.
//   - Cursor: how far the sweep itself got, so an interrupted sweep of a large
//     Drive resumes instead of starting over.
//   - Gen: which sweep the seen-set below belongs to, so a restarted sweep cannot
//     read a previous one's marks as its own.

// Sweep is the persisted progress of an enumeration sweep.
type Sweep struct {
	Gen     string    `json:"gen"`
	Token   string    `json:"token"`
	Cursor  string    `json:"cursor"`
	Started time.Time `json:"started"`
}

// Sweep returns the in-progress sweep record, if one is stored. ok is false when
// no sweep is running — the steady state, since a completed sweep clears it.
func (s *Store) Sweep() (sw Sweep, ok bool, err error) {
	err = s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(bucketSweep).Get(keySweep)
		if v == nil {
			return nil
		}
		if uErr := json.Unmarshal(v, &sw); uErr != nil {
			return uErr
		}
		ok = true
		return nil
	})
	return sw, ok, err
}

// SetSweep persists sweep progress. Callers write it after every page, so the
// cost of an interruption is one page rather than the whole sweep.
func (s *Store) SetSweep(sw Sweep) error {
	v, err := json.Marshal(sw)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketSweep).Put(keySweep, v)
	})
}

// ClearSweep drops the sweep record and its seen-set, ending the sweep. It is
// called only after the reconcile has finished with both.
func (s *Store) ClearSweep() error {
	return s.db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket(bucketSweep).Delete(keySweep); err != nil {
			return err
		}
		if err := tx.DeleteBucket(bucketSeen); err != nil && err != bolt.ErrBucketNotFound {
			return err
		}
		_, err := tx.CreateBucket(bucketSeen)
		return err
	})
}

// MarkSeen records that this generation's sweep observed these paths remotely.
//
// It is what lets a delete be inferred from a *baseline* rather than from absence
// alone: at the end of a complete sweep, a path with an echo record but no mark
// is one the remote no longer has. The marks are persistent, not in-memory,
// precisely so a sweep that resumed after a restart still knows about the pages
// its predecessor consumed — an in-memory set would report every one of them as
// remotely deleted.
//
// One transaction per call, so a page of a thousand objects costs one fsync.
func (s *Store) MarkSeen(gen string, paths []string) error {
	if gen == "" || len(paths) == 0 {
		return nil
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketSeen)
		for _, p := range paths {
			if err := b.Put(seenKey(gen, p), []byte{1}); err != nil {
				return err
			}
		}
		return nil
	})
}

// SeenPath reports whether this generation's sweep observed path remotely.
func (s *Store) SeenPath(gen, path string) (ok bool, err error) {
	if gen == "" {
		return false, nil
	}
	err = s.db.View(func(tx *bolt.Tx) error {
		ok = tx.Bucket(bucketSeen).Get(seenKey(gen, path)) != nil
		return nil
	})
	return ok, err
}

// UnseenEchoes returns every baseline (echo) record whose path this generation's
// sweep did NOT observe remotely — the candidates for "deleted remotely while we
// were not running".
//
// They are candidates, not decisions: the caller still has to look at the local
// side, and deletion is the irreversible direction, so anything ambiguous keeps
// the file. Restricting the answer to paths that have a baseline at all is the
// M7b rule in one line — absence alone never implies a delete.
func (s *Store) UnseenEchoes(gen string) (map[string]Echo, error) {
	out := map[string]Echo{}
	if gen == "" {
		return out, nil
	}
	err := s.db.View(func(tx *bolt.Tx) error {
		seen := tx.Bucket(bucketSeen)
		return tx.Bucket(bucketEcho).ForEach(func(k, v []byte) error {
			if seen.Get(seenKey(gen, string(k))) != nil {
				return nil
			}
			var e Echo
			if err := json.Unmarshal(v, &e); err != nil {
				return err
			}
			out[string(k)] = e
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// HasEchoes reports whether any baseline record exists at all. A store with none
// has never synced anything, which is what "first-ever run" means — and the first
// run deletes nothing on either side.
func (s *Store) HasEchoes() (has bool, err error) {
	err = s.db.View(func(tx *bolt.Tx) error {
		k, _ := tx.Bucket(bucketEcho).Cursor().First()
		has = k != nil
		return nil
	})
	return has, err
}

// seenKey namespaces a mark by generation, so the marks of an abandoned sweep are
// invisible to the next one even before ClearSweep empties the bucket.
func seenKey(gen, path string) []byte {
	return []byte(gen + "\x00" + path)
}
