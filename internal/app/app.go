package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"

	"github.com/zishmusic/drivel/internal/provider"
)

// App is a set of mounts sharing one process.
//
// The mounts are independent by construction — separate engines, state stores,
// providers and event channels — so App owns only what genuinely crosses them:
// validating them against each other before any is opened, and making sure a
// partial failure leaves nothing mounted.
type App struct {
	mounts []*Mount
}

// New validates the specs against each other and opens every mount. If any fails
// to open, the mounts already opened are closed before returning: bringing up
// three of five and exiting would leave two backing dirfds and two bbolt locks
// held by a process that is on its way out.
func New(ctx context.Context, specs []MountSpec, reg *provider.Registry) (*App, error) {
	if err := Validate(specs); err != nil {
		return nil, err
	}
	// One mount keeps the bare default logger, so its output is exactly what
	// drivel printed before several were possible. From two, every line has to
	// say which mount it came from or the log is unreadable.
	if len(specs) > 1 {
		for i := range specs {
			if specs[i].Logger == nil {
				specs[i].Logger = log.New(log.Writer(), "["+specs[i].logName()+"] ", log.Flags())
			}
		}
	}
	a := &App{}
	for _, s := range specs {
		m, err := Open(ctx, s, reg)
		if err != nil {
			_ = a.Close()
			return nil, fmt.Errorf("mount %s: %w", s.label(), err)
		}
		a.mounts = append(a.mounts, m)
	}
	return a, nil
}

// Len reports how many mounts are open.
func (a *App) Len() int { return len(a.mounts) }

// Run serves every mount concurrently and returns once they have all unmounted
// and drained. It returns the joined errors, so one mount failing still reports
// what the others did.
//
// A mount that ends while ctx is still live — a failed mount(2), an unmount from
// outside the process — cancels the rest. The alternative is a process that was
// asked for four mounts, is serving three, and says nothing.
func (a *App) Run(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	errs := make([]error, len(a.mounts))
	var wg sync.WaitGroup
	for i, m := range a.mounts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Each mount runs its own shutdown sequence inside Run: unmount, then
			// close(events), then wait for the engine's bounded drain (§7). The
			// aggregation must stay outside that, never in place of it.
			errs[i] = m.Run(runCtx)
			cancel()
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

// Close releases every mount. Call it after Run returns.
func (a *App) Close() error {
	var errs []error
	for _, m := range a.mounts {
		errs = append(errs, m.Close())
	}
	return errors.Join(errs...)
}
