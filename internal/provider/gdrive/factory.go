package gdrive

import (
	"context"
	"errors"
	"fmt"

	"github.com/zishmusic/drivel/provider"
)

// Factory opens a Drive store from an undecoded config (M8). Register it under
// whatever name a config file should use for this backend:
//
//	reg := provider.NewRegistry()
//	_ = reg.Register("gdrive", gdrive.Factory)
//
// Registration is by hand rather than by init(), so nothing is registered as a
// side effect of importing this package and a test can register the same factory
// twice under different names — which is exactly M8's seam proof: two
// independently-configured Drive stores in one process surface any Drive-shaped
// assumption that leaked above the seam.
func Factory(ctx context.Context, p provider.Params) (provider.Store, error) {
	var cfg Config
	if err := p.Config.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("drive provider config: %w", err)
	}
	if cfg.Credentials == "" {
		// Not the same thing as "no provider at all" — a mount with no provider
		// runs log-only and never reaches a Factory. Getting here means a Drive
		// backend was asked for and cannot possibly work, so say so now rather than
		// failing on the first push.
		return nil, errors.New("no credentials configured (run 'drivel login')")
	}
	d, err := open(ctx, cfg, p.Log)
	if err != nil {
		// Explicitly the untyped nil. Returning d here would hand back a non-nil
		// provider.Store wrapping a (*Drive)(nil) — the Go trap the M5 wiring
		// already documents in cmd/drivel — and every later "is there a store?"
		// check would answer yes.
		return nil, err
	}
	return d, nil
}

var _ provider.Factory = Factory
