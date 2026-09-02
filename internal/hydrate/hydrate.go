// Package hydrate implements M5 lazy hydration: a remote file can exist in the
// backing directory as a *placeholder* — correct name, size and mtime, no bytes
// resident — and is materialised on first read (DESIGN.md §9, M5).
//
// Two records describe a placeholder, and the split is deliberate:
//
//   - An xattr on the backing file itself (user.drivel.placeholder) is the
//     AUTHORITATIVE marker. It survives losing the state DB and travels with the
//     file, which matters because the failure mode it prevents is data loss: a
//     placeholder mistaken for a genuinely empty file gets uploaded as zero bytes,
//     destroying the remote content it was standing in for.
//   - A ranges.Set in the engine state store is a cache and a forward hook. M5
//     only ever stores the degenerate all-or-nothing cases, but the bitmap schema
//     is the one M5b (per-block faulting) needs, and M6 reads the same structure
//     from the other side as its dirty-range map (see internal/ranges).
//
// The package is provider-agnostic: it downloads through provider.Store, and uses
// provider.RangeGetter when the store offers ranged reads.
package hydrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/zishmusic/drivel/internal/provider"
	"github.com/zishmusic/drivel/internal/ranges"
)

// XattrName is the extended attribute carrying the placeholder Marker. It lives
// in the "user." namespace deliberately: "security."/"trusted." are the
// namespaces the kernel treats as authoritative, and a sync tool has no business
// writing there (see DESIGN.md §10.4).
const XattrName = "user.drivel.placeholder"

// markerVersion is bumped if the on-disk Marker encoding changes. An unknown
// version is treated as "placeholder, contents unknown" — fail safe, never fail
// open, because the consequence of guessing wrong is an empty upload.
const markerVersion = 1

// Marker is the authoritative record that a backing file is a placeholder. It is
// stored as JSON in XattrName and removed only once the file is fully resident.
type Marker struct {
	V        int       `json:"v"`
	Size     int64     `json:"size"`
	Hash     string    `json:"hash,omitempty"`
	Version  string    `json:"ver,omitempty"`
	Modified time.Time `json:"mod,omitempty"`
}

// Cache is the optional fast path for range bitmaps — satisfied by *state.Store.
// It is only ever a cache: the xattr Marker is authoritative, so a Hydrator with
// a nil Cache is fully correct, just chattier with the filesystem.
type Cache interface {
	Hydration(path string) ([]byte, bool, error)
	SetHydration(path string, v []byte) error
	DeleteHydration(path string) error
}

// Hydrator materialises placeholder content on demand.
//
// Hydration is whole-file in M5. The ranges.Set plumbing is already in place, so
// M5b can switch Hydrate to fault individual blocks through RangeGetter without
// changing this type's callers or the on-disk schema.
type Hydrator struct {
	dataDir string
	store   provider.Store
	ranges  provider.RangeGetter // nil when the store has no ranged reads
	cache   Cache

	mu       sync.Mutex
	inflight map[string]*fetch // singleflight: concurrent opens fault once
}

// fetch is one in-progress hydration that later arrivals wait on.
type fetch struct {
	done chan struct{}
	err  error
}

// New returns a Hydrator writing into dataDir (the backing store path — in
// in-place mode this is the /proc/self/fd/N handle, never the mountpoint).
// cache may be nil.
func New(dataDir string, store provider.Store, cache Cache) *Hydrator {
	h := &Hydrator{
		dataDir:  dataDir,
		store:    store,
		cache:    cache,
		inflight: map[string]*fetch{},
	}
	// Ranged reads are an optional provider capability; absent it, M5b would fall
	// back to whole-file fetches anyway.
	if rg, ok := store.(provider.RangeGetter); ok {
		h.ranges = rg
	}
	return h
}

// SupportsRanges reports whether the store offers ranged reads (M5b/M6 input).
func (h *Hydrator) SupportsRanges() bool { return h.ranges != nil }

// XattrsUsable reports whether the backing store can hold the authoritative
// marker. When false, placeholder state rests on the state DB alone and losing
// that DB downgrades placeholders to apparently-empty files — the caller should
// warn, and should not enable lazy mode silently.
func (h *Hydrator) XattrsUsable() bool {
	if !xattrSupported {
		return false
	}
	probe := filepath.Join(h.dataDir, ".drivel-xattr-probe")
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return false
	}
	f.Close()
	defer os.Remove(probe)
	return setxattr(probe, XattrName, []byte("1")) == nil
}

// abs maps a root-relative slash path to its backing-store path.
func (h *Hydrator) abs(rel string) string {
	return filepath.Join(h.dataDir, filepath.FromSlash(rel))
}

// Marker reads the placeholder marker for rel. ok is false when the file is fully
// resident (or absent).
func (h *Hydrator) Marker(rel string) (m Marker, ok bool, err error) {
	b, err := getxattr(h.abs(rel), XattrName)
	if err != nil {
		if isNoAttr(err) || isNoSupport(err) || errors.Is(err, os.ErrNotExist) {
			return Marker{}, false, nil
		}
		return Marker{}, false, err
	}
	if len(b) == 0 {
		return Marker{}, false, nil
	}
	if err := json.Unmarshal(b, &m); err != nil {
		// Corrupt marker: treat as an unhydrated placeholder. The safe direction is
		// to re-download content we may already have, never to upload content we
		// may not have.
		return Marker{V: markerVersion}, true, nil
	}
	return m, true, nil
}

// IsPlaceholder reports whether rel is an unhydrated placeholder. It is the guard
// the uploader consults before pushing content (DESIGN.md §9, M5): pushing a
// placeholder would replace real remote content with zero bytes.
//
// It fails SAFE — an I/O error reading the marker reports true, suppressing the
// upload — because a spurious skip costs one deferred sync and a spurious upload
// costs the user their data.
func (h *Hydrator) IsPlaceholder(rel string) bool {
	_, ok, err := h.Marker(rel)
	if err != nil {
		return true
	}
	return ok
}

// CreatePlaceholder materialises rel as a placeholder: apparent size and mtime
// from the remote file, no bytes resident.
//
// It builds the file under a temp name and renames it into place so the marker is
// attached *before* the file is visible at its final path. The reverse order has a
// crash window in which a full-size file of zeros exists with no marker — exactly
// the state the uploader would push over good remote content.
func (h *Hydrator) CreatePlaceholder(rel string, f provider.RemoteFile) error {
	dst := h.abs(rel)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(dst), ".drivel-ph-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once renamed

	// Sparse: set the apparent size without allocating blocks, so `ls -l` and any
	// application sizing a buffer from st_size see the truth.
	if err := tmp.Truncate(f.Size); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	m := Marker{V: markerVersion, Size: f.Size, Hash: f.Hash, Version: f.Version, Modified: f.Modified}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if err := setxattr(tmpName, XattrName, b); err != nil {
		if !isNoSupport(err) {
			return fmt.Errorf("marking placeholder %s: %w", rel, err)
		}
		// No xattr support: continue on the state store alone. New() callers are
		// expected to have warned via XattrsUsable.
	}
	if !f.Modified.IsZero() {
		_ = os.Chtimes(tmpName, f.Modified, f.Modified)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return err
	}

	rs := ranges.New(f.Size, ranges.DefaultBlockSize)
	h.putRanges(rel, rs)
	return nil
}

// Discard drops the placeholder marker without fetching anything. It is the
// correct response to a truncating open or an overwrite: the caller is replacing
// the content wholesale, so downloading the old bytes first is pure waste.
func (h *Hydrator) Discard(rel string) error {
	if err := removexattr(h.abs(rel), XattrName); err != nil && !isNoAttr(err) && !isNoSupport(err) {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if h.cache != nil {
		_ = h.cache.DeleteHydration(rel)
	}
	return nil
}

// Hydrate materialises rel's content if it is still a placeholder, blocking until
// the bytes are resident. Concurrent calls for the same path fault once.
//
// This is on the FUSE read path by design — it is the one place Drivel blocks an
// FS operation on the network (DESIGN.md §3 keeps the *proactive* pull loop off
// the streaming path; hydrate-on-read is the deliberate exception).
func (h *Hydrator) Hydrate(ctx context.Context, rel string) error {
	m, ok, err := h.Marker(rel)
	if err != nil {
		return err
	}
	if !ok {
		return nil // already resident
	}

	h.mu.Lock()
	if f, busy := h.inflight[rel]; busy {
		h.mu.Unlock()
		select {
		case <-f.done:
			return f.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f := &fetch{done: make(chan struct{})}
	h.inflight[rel] = f
	h.mu.Unlock()

	f.err = h.fetchWhole(ctx, rel, m)

	h.mu.Lock()
	delete(h.inflight, rel)
	h.mu.Unlock()
	close(f.done)
	return f.err
}

// fetchWhole downloads all of rel and writes it into the existing backing file.
//
// It writes IN PLACE rather than via temp+rename: the FUSE layer may already hold
// an open descriptor on this inode (hydration is triggered from Open), and a
// rename would leave that descriptor pointing at the old, empty inode. The marker
// is removed last and only after a successful fsync, so a crash mid-write leaves a
// still-marked placeholder that is simply re-fetched.
func (h *Hydrator) fetchWhole(ctx context.Context, rel string, m Marker) error {
	rc, err := h.store.Get(ctx, rel)
	if err != nil {
		return fmt.Errorf("hydrate %s: %w", rel, err)
	}
	defer rc.Close()

	dst := h.abs(rel)
	fh, err := os.OpenFile(dst, os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	n, copyErr := io.Copy(fh, rc)
	if copyErr != nil {
		fh.Close()
		return fmt.Errorf("hydrate %s: %w", rel, copyErr)
	}
	// The remote may have changed size since the placeholder was stamped; the
	// bytes we actually received are the truth.
	if err := fh.Truncate(n); err != nil {
		fh.Close()
		return err
	}
	if err := fh.Sync(); err != nil {
		fh.Close()
		return err
	}
	if err := fh.Close(); err != nil {
		return err
	}

	// Writing reset mtime to now; restore the remote timestamp so the downloader's
	// last-writer-wins comparison (§6) doesn't read hydration as a local edit.
	if !m.Modified.IsZero() {
		_ = os.Chtimes(dst, m.Modified, m.Modified)
	}

	if err := removexattr(dst, XattrName); err != nil && !isNoAttr(err) && !isNoSupport(err) {
		return fmt.Errorf("clearing placeholder mark %s: %w", rel, err)
	}

	rs := ranges.New(n, ranges.DefaultBlockSize)
	rs.MarkAll()
	h.putRanges(rel, rs)
	return nil
}

// Ranges returns the cached present-ranges bitmap for rel, if the cache holds one.
func (h *Hydrator) Ranges(rel string) (ranges.Set, bool) {
	if h.cache == nil {
		return ranges.Set{}, false
	}
	b, ok, err := h.cache.Hydration(rel)
	if err != nil || !ok {
		return ranges.Set{}, false
	}
	var rs ranges.Set
	if err := rs.UnmarshalBinary(b); err != nil {
		return ranges.Set{}, false
	}
	return rs, true
}

func (h *Hydrator) putRanges(rel string, rs ranges.Set) {
	if h.cache == nil {
		return
	}
	b, err := rs.MarshalBinary()
	if err != nil {
		return
	}
	_ = h.cache.SetHydration(rel, b)
}
