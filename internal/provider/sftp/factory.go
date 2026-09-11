package sftp

import (
	"context"
	"fmt"

	"github.com/zishmusic/drivel/internal/provider"
)

// Kind is the name this backend is registered and configured under.
const Kind = "sftp"

// Factory opens an SFTP store from an undecoded config (M8). Register it under
// the name a config file should use:
//
//	reg := provider.NewRegistry()
//	_ = reg.Register(sftp.Kind, sftp.Factory)
//
// Registration is by hand rather than by init(), for the reasons the Registry
// documents: no process-global mutable state, and a dependency that is named
// where it is wired rather than hidden in an import for side effect.
func Factory(ctx context.Context, p provider.Params) (provider.Store, error) {
	var cfg Config
	if err := p.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("sftp provider config: %w", err)
	}
	s, err := Open(ctx, cfg, p.Log)
	if err != nil {
		// Explicitly the untyped nil: returning s would hand back a non-nil
		// provider.Store wrapping a (*Store)(nil), and every later "is there a
		// store?" check would answer yes.
		return nil, err
	}
	return s, nil
}

var _ provider.Factory = Factory
