// Package syncengine consumes filesystem change events and (from M2 onward)
// applies them to a cloud provider, while a downloader polls the provider's
// change feed and applies remote deltas back into the underlying directory.
//
// M1 status: the engine only drains and logs events, demonstrating the seam.
// The uploader, downloader, state store, and echo-suppression logic (see
// DESIGN.md §4) land in M2–M4.
package syncengine

import (
	"context"
	"log"

	"github.com/zishmusic/dedupfs/internal/vfs"
)

// Engine wires the outbound (local→cloud) and inbound (cloud→local) paths.
type Engine struct {
	// provider provider.Provider // wired in M2
	// store    *store.Store       // wired in M3
}

// New constructs an Engine. Dependencies (provider, store) are added in later
// milestones.
func New() *Engine {
	return &Engine{}
}

// Run consumes change events until events is closed or ctx is cancelled.
// For M1 this just logs; M2 replaces the body with the uploader dispatch.
func (e *Engine) Run(ctx context.Context, events <-chan vfs.Event) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			if ev.Op == vfs.OpRename {
				log.Printf("[sync] %-7s %s -> %s", ev.Op, ev.Path, ev.NewPath)
			} else {
				log.Printf("[sync] %-7s %s", ev.Op, ev.Path)
			}
		}
	}
}
