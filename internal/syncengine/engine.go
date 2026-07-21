// Package syncengine consumes filesystem change events and applies them to a
// cloud provider (outbound/push), and — from M3 — polls the provider's change
// feed and applies remote deltas back to the underlying directory (inbound/pull).
//
// M2 status: outbound push only. The path↔fileID index is in-memory; the
// persistent bbolt store, echo suppression, debounce, and the pull loop
// (DESIGN.md §3–§4) arrive in M3–M4. With no provider configured, the engine
// runs in log-only mode (M1 behaviour).
package syncengine

import (
	"context"
	"log"
	"os"
	"path"
	"path/filepath"
	"sync"

	"github.com/zishmusic/drivel/internal/provider"
	"github.com/zishmusic/drivel/internal/fsevent"
)

// Engine applies local change events to a provider.
type Engine struct {
	prov    provider.Provider // nil => log-only mode
	dataDir string            // underlying dir; content is read from here
	rootID  string            // provider folder ID mapped to the mount root

	mu       sync.Mutex
	idByPath map[string]string // root-relative path -> provider fileID (in-memory; bbolt in M3)
}

// Config parameterises an Engine.
type Config struct {
	Provider provider.Provider // nil for log-only mode
	DataDir  string            // underlying directory (source of truth)
	RootID   string            // provider folder ID for the mount root ("" => provider default)
}

// New constructs an Engine.
func New(cfg Config) *Engine {
	return &Engine{
		prov:     cfg.Provider,
		dataDir:  cfg.DataDir,
		rootID:   cfg.RootID,
		idByPath: make(map[string]string),
	}
}

// Run consumes events until events is closed or ctx is cancelled.
func (e *Engine) Run(ctx context.Context, events <-chan fsevent.Event) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			e.handle(ctx, ev)
		}
	}
}

func (e *Engine) handle(ctx context.Context, ev fsevent.Event) {
	if e.prov == nil {
		// Log-only mode: no provider wired (e.g. no credentials).
		if ev.Op == fsevent.OpRename {
			log.Printf("[sync] %-7s %s -> %s", ev.Op, ev.Path, ev.NewPath)
		} else {
			log.Printf("[sync] %-7s %s", ev.Op, ev.Path)
		}
		return
	}
	if err := e.push(ctx, ev); err != nil {
		// M4 adds ret/requeue with backoff; for now surface and drop.
		log.Printf("[sync] push %s %s: %v", ev.Op, ev.Path, err)
	}
}

// push maps one event to provider calls. Retries/debounce/echo-suppression are
// deliberately out of scope until M3–M4.
func (e *Engine) push(ctx context.Context, ev fsevent.Event) error {
	switch ev.Op {
	case fsevent.OpMkdir:
		parentID, err := e.ensureParent(ctx, ev.Path)
		if err != nil {
			return err
		}
		rf, err := e.prov.Mkdir(ctx, parentID, path.Base(ev.Path))
		if err != nil {
			return err
		}
		e.setID(ev.Path, rf.ID)

	case fsevent.OpCreate, fsevent.OpWrite:
		return e.pushContent(ctx, ev.Path)

	case fsevent.OpUnlink, fsevent.OpRmdir:
		if id, ok := e.getID(ev.Path); ok {
			if err := e.prov.Delete(ctx, id); err != nil {
				return err
			}
			e.delID(ev.Path)
		}

	case fsevent.OpRename:
		id, ok := e.getID(ev.Path)
		if !ok {
			// Unknown source (e.g. never uploaded); treat destination as new content.
			return e.pushContent(ctx, ev.NewPath)
		}
		parentID, err := e.ensureParent(ctx, ev.NewPath)
		if err != nil {
			return err
		}
		if _, err := e.prov.Move(ctx, id, parentID, path.Base(ev.NewPath)); err != nil {
			return err
		}
		e.delID(ev.Path)
		e.setID(ev.NewPath, id)

	case fsevent.OpSetattr:
		// Metadata-only change; no content push in M2.
	}
	return nil
}

// pushContent uploads (or updates) the file at virtual path p from the underlying dir.
func (e *Engine) pushContent(ctx context.Context, p string) error {
	f, err := os.Open(filepath.Join(e.dataDir, filepath.FromSlash(p)))
	if err != nil {
		if os.IsNotExist(err) {
			return nil // raced with a delete; nothing to push
		}
		return err
	}
	defer f.Close()

	if id, ok := e.getID(p); ok {
		_, err := e.prov.Update(ctx, id, f)
		return err
	}
	parentID, err := e.ensureParent(ctx, p)
	if err != nil {
		return err
	}
	rf, err := e.prov.Upload(ctx, parentID, path.Base(p), f)
	if err != nil {
		return err
	}
	e.setID(p, rf.ID)
	return nil
}

// ensureParent returns the provider folder ID for the parent of virtual path p,
// creating ancestor folders on demand. Top-level parent is the configured rootID.
func (e *Engine) ensureParent(ctx context.Context, p string) (string, error) {
	dir := path.Dir(p)
	if dir == "." || dir == "/" || dir == "" {
		return e.rootID, nil
	}
	if id, ok := e.getID(dir); ok {
		return id, nil
	}
	grandParent, err := e.ensureParent(ctx, dir)
	if err != nil {
		return "", err
	}
	rf, err := e.prov.Mkdir(ctx, grandParent, path.Base(dir))
	if err != nil {
		return "", err
	}
	e.setID(dir, rf.ID)
	return rf.ID, nil
}

func (e *Engine) getID(p string) (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	id, ok := e.idByPath[p]
	return id, ok
}

func (e *Engine) setID(p, id string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.idByPath[p] = id
}

func (e *Engine) delID(p string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.idByPath, p)
}
