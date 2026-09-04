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
	"errors"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"
	bolterrors "go.etcd.io/bbolt/errors"
)

var (
	bucketCursor    = []byte("cursor")
	bucketEcho      = []byte("echo")
	bucketHydration = []byte("hydration")
	bucketSweep     = []byte("sweep")
	bucketSeen      = []byte("seen")
	keyCursor       = []byte("changefeed")
	keySweep        = []byte("current")
	keySweepDone    = []byte("done")
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

// Store is the bbolt-backed sync-state store.
type Store struct {
	db *bolt.DB
}

// Open opens (creating if needed) the state DB at path and ensures its buckets
// exist. Callers must Close it.
func Open(path string) (*Store, error) {
	db, err := bolt.Open(path, 0o600, boltOptions)
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

// Forget drops every record for path — the echo baseline and the hydration
// bitmap — in ONE transaction.
//
// The two used to be separate calls, which meant two commits and two fsyncs per
// deleted path. See ForgetMany for why that matters.
func (s *Store) Forget(path string) error { return s.ForgetMany([]string{path}) }

// ForgetMany drops the records for many paths in one transaction.
//
// A bbolt commit is an fsync, so a delete-per-path costs one fsync per path:
// measured here at ~1.35ms, which is ~27s for 20k paths and ~22 minutes for a
// million. That is the shape of a mass delete arriving over the change feed, and
// none of it is hidden behind network latency the way an outbound delete is —
// the feed already delivered the whole page. Batching to the page turns it into
// one commit, the same trade SetMany makes on the index side.
//
// Callers must not batch across a durability boundary they depend on: the point
// where this is called is the point the records are gone, so it belongs with the
// cursor commit that decides the page will not be replayed.
func (s *Store) ForgetMany(paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		echo, hydration := tx.Bucket(bucketEcho), tx.Bucket(bucketHydration)
		for _, p := range paths {
			k := []byte(p)
			if err := echo.Delete(k); err != nil {
				return err
			}
			if err := hydration.Delete(k); err != nil {
				return err
			}
		}
		return nil
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

// FinishSweep ends a sweep: it drops the in-progress record and its seen-set, and
// stamps when the sweep completed. It is called only after the reconcile has
// finished with both.
//
// The three go in ONE transaction because the completion stamp is what the
// periodic-sweep schedule reads: a stamp without the clear would let a resume and
// a schedule disagree about whether a sweep is running, and a clear without the
// stamp would make the next mount think a sweep is overdue and run another.
func (s *Store) FinishSweep(done time.Time) error {
	stamp, err := done.UTC().MarshalText()
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		sweep := tx.Bucket(bucketSweep)
		if err := sweep.Delete(keySweep); err != nil {
			return err
		}
		if err := sweep.Put(keySweepDone, stamp); err != nil {
			return err
		}
		if err := tx.DeleteBucket(bucketSeen); err != nil && !errors.Is(err, bolterrors.ErrBucketNotFound) {
			return err
		}
		_, err := tx.CreateBucket(bucketSeen)
		return err
	})
}

// SweepDone returns when the last sweep completed. ok is false when none ever
// has — a state DB that predates the stamp, or one whose every sweep was
// interrupted.
//
// It is persisted rather than counted from process start because the gap a
// periodic sweep exists to close is the one where drivel was NOT running. A user
// who mounts for an hour a day would never reach any interval measured from
// startup, and so would never sweep at all.
func (s *Store) SweepDone() (at time.Time, ok bool, err error) {
	err = s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(bucketSweep).Get(keySweepDone)
		if v == nil {
			return nil
		}
		if uErr := at.UnmarshalText(v); uErr != nil {
			return uErr
		}
		ok = true
		return nil
	})
	return at, ok, err
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

// EachUnseenEcho calls fn for every baseline (echo) record whose path this
// generation's sweep did NOT observe remotely — the candidates for "deleted
// remotely while we were not running".
//
// They are candidates, not decisions: the caller still has to look at the local
// side, and deletion is the irreversible direction, so anything ambiguous keeps
// the file. Restricting the answer to paths that have a baseline at all is the
// M7b rule in one line — absence alone never implies a delete.
//
// It streams instead of returning a map because the input that most needs
// refusing is also the largest: a state DB pointed at the wrong -drive-root makes
// every path unseen, so collecting them all first would spend the memory on
// precisely the pass that is about to be abandoned. fn's error aborts the walk
// and is returned unwrapped.
//
// fn runs inside a read transaction and must not write to this store.
func (s *Store) EachUnseenEcho(gen string, fn func(path string, e Echo) error) error {
	if gen == "" {
		return nil
	}
	return s.db.View(func(tx *bolt.Tx) error {
		seen := tx.Bucket(bucketSeen)
		return tx.Bucket(bucketEcho).ForEach(func(k, v []byte) error {
			if seen.Get(seenKey(gen, string(k))) != nil {
				return nil
			}
			var e Echo
			if err := json.Unmarshal(v, &e); err != nil {
				return err
			}
			return fn(string(k), e)
		})
	})
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
// invisible to the next one even before FinishSweep empties the bucket.
func seenKey(gen, path string) []byte {
	return []byte(gen + "\x00" + path)
}
