package syncengine

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"io"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/zishmusic/drivel/internal/provider"
	"github.com/zishmusic/drivel/internal/state"
)

// Cadence bounds the adaptive poll interval (DESIGN.md §3.4): poll at Fast while
// the feed is active, backing off geometrically toward Slow once it goes quiet.
type Cadence struct {
	Fast time.Duration
	Slow time.Duration
}

// DefaultCadence is the ~2s-active / ~30s-idle default.
var DefaultCadence = Cadence{Fast: 2 * time.Second, Slow: 30 * time.Second}

// Downloader runs the inbound pull loop: it polls a provider.ChangeSource and
// applies remote deltas to the backing directory, using the echo-suppression
// store so it never re-applies the uploader's own writes (DESIGN.md §3–§4).
//
// It writes directly into the backing dir (never through the mountpoint), so its
// writes don't re-enter the FUSE layer and never generate local events — which is
// why §4.3 ("suppress downloader-originated local events") needs no extra
// plumbing here. When a remote edit collides with a divergent local edit it
// applies the §6 conflict policy (last-writer-wins by mtime, loser kept as a
// conflict copy); see resolveConflict.
type Downloader struct {
	src     provider.ChangeSource
	store   provider.Store
	dataDir string
	state   *state.Store
	cad     Cadence
}

// NewDownloader constructs the pull loop. cad zero-values fall back to
// DefaultCadence.
func NewDownloader(src provider.ChangeSource, store provider.Store, dataDir string, st *state.Store, cad Cadence) *Downloader {
	if cad.Fast <= 0 {
		cad.Fast = DefaultCadence.Fast
	}
	if cad.Slow < cad.Fast {
		cad.Slow = DefaultCadence.Slow
	}
	return &Downloader{src: src, store: store, dataDir: dataDir, state: st, cad: cad}
}

// Run polls until ctx is cancelled. It resumes from the persisted cursor, or
// obtains a fresh start token on first run so it only ever sees changes from
// "now" forward.
func (d *Downloader) Run(ctx context.Context) {
	cursor, err := d.resumeCursor(ctx)
	if err != nil {
		log.Printf("[pull] cannot start change feed: %v", err)
		return
	}

	wait := d.cad.Fast
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}

		changes, next, err := d.src.Changes(ctx, cursor)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			// M4 adds classification/backoff on transient errors; for now log and
			// retry at the slow cadence rather than hot-looping.
			log.Printf("[pull] changes: %v", err)
			wait = d.cad.Slow
			continue
		}

		for _, ch := range changes {
			if err := d.apply(ctx, ch); err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Printf("[pull] apply %s: %v", ch.Path, err)
			}
		}

		if next != "" && next != cursor {
			if err := d.state.SetCursor(next); err != nil {
				log.Printf("[pull] persist cursor: %v", err)
			}
			cursor = next
		}

		// Adaptive cadence: stay fast while the feed is active, else back off
		// geometrically toward Slow.
		if len(changes) > 0 {
			wait = d.cad.Fast
		} else if wait *= 2; wait > d.cad.Slow {
			wait = d.cad.Slow
		}
	}
}

// resumeCursor returns the persisted cursor, or a fresh start token (persisted)
// on first run.
func (d *Downloader) resumeCursor(ctx context.Context) (string, error) {
	if cursor, ok, err := d.state.Cursor(); err != nil {
		return "", err
	} else if ok {
		return cursor, nil
	}
	cursor, err := d.src.StartCursor(ctx)
	if err != nil {
		return "", err
	}
	if err := d.state.SetCursor(cursor); err != nil {
		return "", err
	}
	return cursor, nil
}

// apply reconciles one remote change into the backing directory.
func (d *Downloader) apply(ctx context.Context, ch provider.RemoteChange) error {
	dst := filepath.Join(d.dataDir, filepath.FromSlash(ch.Path))

	if ch.Removed || ch.File == nil {
		// Idempotent: if we (or a prior run) already removed it, RemoveAll is a
		// no-op. Covers our own delete echo without a dedicated record.
		if err := os.RemoveAll(dst); err != nil {
			return err
		}
		log.Printf("[pull] delete  %s", ch.Path)
		return d.state.DeleteEcho(ch.Path)
	}

	f := ch.File

	echo, hasEcho, err := d.state.GetEcho(ch.Path)
	if err != nil {
		return err
	}
	// Echo / loop breaker (§4): if this is the content we last synced at this
	// path — our own upload coming back, or a re-report of an applied download —
	// drop it.
	if hasEcho && echo.Matches(f.Hash, f.Version) {
		return nil
	}

	if f.IsDir {
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return err
		}
		log.Printf("[pull] mkdir   %s", ch.Path)
		return d.rememberApplied(ch.Path, f)
	}

	localHash, localExists, err := statMD5(dst)
	if err != nil {
		return err
	}

	// Loop breaker vs. on-disk content: if the backing file already holds these
	// exact bytes (common on first sync when the dir already mirrors Drive), skip
	// the download entirely and just record the echo.
	if f.Hash != "" && localExists && localHash == f.Hash {
		return d.rememberApplied(ch.Path, f)
	}

	// Conflict (§6): we're past the echo check, so the remote content differs from
	// what we last synced. If the local file also diverged from that baseline (or
	// there's no baseline and it differs from the remote), both sides changed —
	// resolve last-writer-wins by mtime and keep the loser as a conflict copy.
	// Needs content hashes to compare; hashless objects (Google-native docs) fall
	// through to a plain apply.
	if f.Hash != "" && localExists {
		baseline := ""
		if hasEcho {
			baseline = echo.Hash
		}
		if localHash != baseline {
			return d.resolveConflict(ctx, ch.Path, dst, f)
		}
	}

	if err := d.download(ctx, ch.Path, dst, f.Modified); err != nil {
		return err
	}
	log.Printf("[pull] download %s", ch.Path)
	return d.rememberApplied(ch.Path, f)
}

// resolveConflict applies the §6 policy when a remote edit collides with a
// divergent local edit at rel (backing path dst): last-writer-wins by mtime, and
// the losing side is preserved as a sibling conflict copy. Conflict copies are
// local-only in v1 — they are not synced back up.
func (d *Downloader) resolveConflict(ctx context.Context, rel, dst string, f *provider.RemoteFile) error {
	info, err := os.Stat(dst)
	if err != nil {
		return err
	}
	localMtime := info.ModTime()

	if f.Modified.After(localMtime) {
		// Remote is newer: it wins the real path; the local edit becomes the copy.
		copyRel := conflictName(rel, localMtime)
		copyDst := filepath.Join(d.dataDir, filepath.FromSlash(copyRel))
		if err := os.Rename(dst, copyDst); err != nil {
			return err
		}
		if err := d.download(ctx, rel, dst, f.Modified); err != nil {
			return err
		}
		log.Printf("[pull] conflict %s: remote newer, local kept as %s", rel, copyRel)
		return d.rememberApplied(rel, f)
	}

	// Local is newer (or same mtime): it keeps the real path; the remote version is
	// saved alongside as the copy. Leave dst and its echo untouched — the uploader
	// still holds the local edit to push.
	copyRel := conflictName(rel, f.Modified)
	copyDst := filepath.Join(d.dataDir, filepath.FromSlash(copyRel))
	if err := d.download(ctx, rel, copyDst, f.Modified); err != nil {
		return err
	}
	log.Printf("[pull] conflict %s: local newer, remote saved as %s", rel, copyRel)
	return nil
}

// download streams the object at path into dst atomically (temp file + rename in
// the destination directory) and stamps its mtime to the remote modified time.
func (d *Downloader) download(ctx context.Context, path, dst string, modified time.Time) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	rc, err := d.store.Get(ctx, path)
	if err != nil {
		return err
	}
	defer rc.Close()

	tmp, err := os.CreateTemp(filepath.Dir(dst), ".drivel-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename

	if _, err := io.Copy(tmp, rc); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if !modified.IsZero() {
		_ = os.Chtimes(tmpName, modified, modified)
	}
	return os.Rename(tmpName, dst)
}

// rememberApplied records what we just wrote so a subsequent change-feed report
// of the same content is recognised as an echo and dropped.
func (d *Downloader) rememberApplied(path string, f *provider.RemoteFile) error {
	return d.state.SetEcho(path, state.Echo{Hash: f.Hash, Version: f.Version, At: time.Now()})
}

// statMD5 returns the hex md5 of the file at p, reporting exists=false (no error)
// when the file is absent.
func statMD5(p string) (hash string, exists bool, err error) {
	h, err := fileMD5(p)
	if os.IsNotExist(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return h, true, nil
}

// conflictName inserts a " (conflict <ts>)" marker before the extension of a
// root-relative slash path, e.g. "sub/a.txt" -> "sub/a (conflict 2026-07-17 09-30-00).txt".
func conflictName(rel string, t time.Time) string {
	ext := path.Ext(rel)
	return strings.TrimSuffix(rel, ext) + " (conflict " + t.Format("2006-01-02 15-04-05") + ")" + ext
}

// fileMD5 returns the hex md5 of the file at path (Drive's md5Checksum encoding).
func fileMD5(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
