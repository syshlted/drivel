// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package syncengine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/syshlted/drivel/internal/fsevent"
)

// Pausing a mount (M14).
//
// **Pause stops dispatch, never consumption**, and the obvious implementation
// breaks the filesystem. vfs.node.emit blocks rather than drops, deliberately, so
// a pause that stopped reading the event channel would fill it and then block
// FUSE: a `cp` hangs until someone resumes. So the run loop goes on reading and
// coalescing exactly as before, and what changes is the hand-off to a worker:
// while paused, dispatch appends to a held list instead of sending. Workers wait
// at a gate before each operation, which is safe only because nothing is being
// sent to them — the gate is closed by the run loop itself, after it has
// stopped dispatching, never by the caller asking.
//
// What is bounded is distinct paths in the debounce map plus operations held,
// not bytes written; the coalescer is what makes the first of those small. It is
// still not bounded absolutely, so PauseCap is the overflow rule: past it the
// pause ends on its own and says so loudly. **Discarding the held set is not an
// option** — a sweep does not reliably repair a lost modification to an
// already-synced file (DESIGN.md §9/M14) — and nor is blocking.
//
// A pause also holds the pull loop and the sweep (they wait at the same gate),
// and the reconciler's direct pushes. It does NOT hold hydration: a read of a
// placeholder is a user waiting on the FUSE path, and refusing it would be an
// EIO for a file that exists. An upload already in flight when the pause lands
// is allowed to finish.
//
// A pause is never persisted and always expires. A mount that stays paused
// across a restart looks mounted and silently never syncs, which is the failure
// mode this project exists to avoid.

// DefaultPauseFor is how long a pause lasts when the request does not say.
const DefaultPauseFor = time.Hour

// MaxPauseFor is the longest pause that may be asked for. A durable pause, if it
// is ever wanted, belongs in the config file where the user can see it.
const MaxPauseFor = 24 * time.Hour

// DefaultPauseCap is how many paths and operations a pause may hold before it
// gives up and resumes on its own.
const DefaultPauseCap = 10000

// ErrNotRunning is returned by Pause and Resume when the engine's run loop is not
// there to receive them.
var ErrNotRunning = errors.New("the sync engine is not running")

// ErrLogOnly is returned when there is nothing to pause: a mount with no
// provider sends nothing anywhere.
var ErrLogOnly = errors.New("this mount has no provider (log-only); there is nothing to pause")

// gate is open unless the run loop has closed it.
type gate struct {
	mu     sync.Mutex
	closed chan struct{} // non-nil while paused; closed (released) on resume
}

// shut closes the gate. Only the run loop calls it.
func (g *gate) shut() {
	g.mu.Lock()
	if g.closed == nil {
		g.closed = make(chan struct{})
	}
	g.mu.Unlock()
}

// release opens it, waking everything waiting.
func (g *gate) release() {
	g.mu.Lock()
	if g.closed != nil {
		close(g.closed)
		g.closed = nil
	}
	g.mu.Unlock()
}

// wait blocks while the gate is shut, reporting false if ctx ended first.
func (g *gate) wait(ctx context.Context) bool {
	for {
		g.mu.Lock()
		ch := g.closed
		g.mu.Unlock()
		if ch == nil {
			return ctx.Err() == nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return false
		}
	}
}

// WaitUnpaused blocks while this engine is paused. It is how the pull loop and
// the sweep stop with the push side; see Downloader.PausedBy.
func (e *Engine) WaitUnpaused(ctx context.Context) bool { return e.gate.wait(ctx) }

type ctlOp int

const (
	ctlPause ctlOp = iota
	ctlResume
)

type ctlReq struct {
	op    ctlOp
	d     time.Duration
	reply chan ctlReply
}

type ctlReply struct {
	until time.Time
	err   error
}

// Pause holds this engine's uploads, and the pull loop and sweep waiting on it,
// for d (DefaultPauseFor if zero). Pausing an already-paused engine moves the
// expiry. It returns when the pause has taken effect, or ctx ends.
func (e *Engine) Pause(ctx context.Context, d time.Duration) (time.Time, error) {
	if d == 0 {
		d = DefaultPauseFor
	}
	if d < 0 || d > MaxPauseFor {
		return time.Time{}, fmt.Errorf("a pause lasts between 1s and %s, not %s", MaxPauseFor, d)
	}
	return e.control(ctx, ctlReq{op: ctlPause, d: d})
}

// Resume ends a pause. Resuming an engine that is not paused is not an error.
func (e *Engine) Resume(ctx context.Context) error {
	_, err := e.control(ctx, ctlReq{op: ctlResume})
	return err
}

func (e *Engine) control(ctx context.Context, req ctlReq) (time.Time, error) {
	if e.store == nil {
		return time.Time{}, ErrLogOnly
	}
	req.reply = make(chan ctlReply, 1)
	select {
	case e.ctl <- req:
	case <-e.stopped:
		return time.Time{}, ErrNotRunning
	case <-ctx.Done():
		return time.Time{}, ctx.Err()
	}
	select {
	case r := <-req.reply:
		return r.until, r.err
	case <-ctx.Done():
		return time.Time{}, ctx.Err()
	}
}

// pauser is the run loop's half: the held list and the expiry, owned by that
// goroutine alone.
type pauser struct {
	e      *Engine
	on     bool
	since  time.Time
	until  time.Time
	held   []fsevent.Event
	timer  *time.Timer
	expiry <-chan time.Time
}

func (p *pauser) state(reason string) PauseState {
	return PauseState{Paused: p.on, Since: p.since, Until: p.until, Held: len(p.held), Cap: p.e.pauseCap, LastAutoResume: reason}
}

// begin enters (or extends) a pause.
func (p *pauser) begin(d time.Duration) time.Time {
	now := time.Now()
	if !p.on {
		p.on, p.since = true, now
		p.e.gate.shut()
		p.e.logf("[sync] paused for %s: uploads, the pull loop and the sweep are held; writes through the mount are still accepted", d)
	} else {
		p.e.logf("[sync] pause extended by %s", d)
	}
	p.until = now.Add(d)
	if p.timer != nil {
		p.timer.Stop()
	}
	p.timer = time.NewTimer(d)
	p.expiry = p.timer.C
	p.e.mon.paused(p.state(""))
	return p.until
}

// end leaves the pause, handing back what it held. The caller dispatches it.
func (p *pauser) end(reason string) []fsevent.Event {
	if !p.on {
		return nil
	}
	if p.timer != nil {
		p.timer.Stop()
		p.timer, p.expiry = nil, nil
	}
	held := p.held
	p.on, p.held = false, nil
	p.e.gate.release()
	if reason == "" {
		p.e.logf("[sync] resumed after %s: sending %d held operation(s)", time.Since(p.since).Round(time.Second), len(held))
	} else {
		p.e.logf("[sync] RESUMED after %s without being asked (%s): sending %d held operation(s)",
			time.Since(p.since).Round(time.Second), reason, len(held))
	}
	p.e.mon.paused(p.state(reason))
	return held
}
