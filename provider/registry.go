package provider

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
)

// ErrUnknownKind reports a provider kind no Factory was registered for. It is a
// sentinel so a config layer can tell "you asked for a backend that does not
// exist" (a typo, or a plugin that failed to load) apart from "that backend
// exists and failed to open" (bad credentials, no network).
var ErrUnknownKind = errors.New("provider: unknown kind")

// Params is everything a Factory receives besides its context. It is a struct so
// that giving providers something new — a metrics sink, a rate limiter — does not
// churn every Factory signature in the tree.
type Params struct {
	// Config is this provider's own configuration, undecoded: gdrive's Config, an
	// S3 bucket and region, a WebDAV URL. The registry never learns what a Drive
	// folder ID is, and adding a provider touches neither this package nor
	// internal/config.
	//
	// The zero value is valid and decodes to the destination's zero value, so a
	// Factory need not check before calling Config.Decode — it validates the
	// decoded struct instead, which is where the useful error message lives
	// anyway.
	Config Config

	// Log is where this provider writes. Never nil, and with several mounts in one
	// process it is what says which mount a line came from — the reason it is
	// passed rather than taken from the log package's default.
	//
	// Out of process it is still this mount's logger: the plugin's own writes are
	// carried back over the seam and printed here, so a plugin's diagnostics
	// interleave with the mount's rather than landing on some other stream.
	Log *log.Logger
}

// Factory opens one Store of a single provider kind (M8).
type Factory func(ctx context.Context, p Params) (Store, error)

// Registry maps a provider kind name to the Factory that opens it. The zero
// Registry is not usable; call NewRegistry.
//
// It is an explicit value rather than a package-level map filled by init()
// (the database/sql shape), deliberately:
//
//   - It would be the only process-global mutable state in the tree. Every other
//     component is constructed per-instance from an explicit config, which is
//     exactly what lets one process serve N mounts — a global registry would cut
//     against the grain of the milestone that introduces it.
//   - Import-for-side-effect (`_ "internal/provider/gdrive"`) hides a dependency
//     from anyone reading the wiring. Registering by hand names it.
//   - It is what makes M8's seam proof possible without shipping anything: a test
//     registers gdrive under two names and mounts both, with no build tag and no
//     env var leaking a pseudo-provider into a user-facing binary.
//
// Safe for concurrent use, though in practice every Register call happens during
// startup, before any Open.
type Registry struct {
	mu    sync.RWMutex
	kinds map[string]Factory
	hint  func(kind string) string
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{kinds: make(map[string]Factory)}
}

// Hint installs a function that describes why a kind might be missing, for the
// error an unregistered kind produces. Its return value is appended to that
// error; an empty string adds nothing.
//
// It exists because "unknown kind" stopped being the whole story when a backend
// became something installed rather than compiled in (M9). The registry itself
// must not learn what a plugin is — it holds factories and nothing else — so the
// layer that does know, the plugin loader, contributes the sentence that names
// the file it looked for and the directories it looked in. Without it the error
// is accurate and unactionable: a user who has not installed a backend is told
// only that the name is unknown.
//
// Set once, during startup, before any Open.
func (r *Registry) Hint(fn func(kind string) string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hint = fn
}

// Register adds a kind. It errors on a duplicate name rather than overwriting:
// two factories answering to one kind means one of them is silently unreachable,
// and which one you got would depend on registration order.
func (r *Registry) Register(kind string, f Factory) error {
	if kind == "" {
		return errors.New("provider: cannot register an empty kind")
	}
	if f == nil {
		return fmt.Errorf("provider: nil factory for kind %q", kind)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.kinds[kind]; dup {
		return fmt.Errorf("provider: kind %q already registered", kind)
	}
	r.kinds[kind] = f
	return nil
}

// Open builds a Store of the named kind, handing the Factory its undecoded
// config. An unregistered kind returns an error wrapping ErrUnknownKind and
// naming what is available, because the overwhelmingly likely cause is a typo in
// a hand-written config file.
func (r *Registry) Open(ctx context.Context, kind string, p Params) (Store, error) {
	r.mu.RLock()
	f, ok := r.kinds[kind]
	hint := r.hint
	r.mu.RUnlock()
	if !ok {
		var because string
		if hint != nil {
			if h := hint(kind); h != "" {
				because = "; " + h
			}
		}
		known := r.Kinds()
		if len(known) == 0 {
			return nil, fmt.Errorf("%w: %q (no providers available)%s", ErrUnknownKind, kind, because)
		}
		return nil, fmt.Errorf("%w: %q (available: %s)%s", ErrUnknownKind, kind, strings.Join(known, ", "), because)
	}
	if p.Log == nil {
		p.Log = log.Default()
	}
	store, err := f(ctx, p)
	if err != nil {
		return nil, fmt.Errorf("opening %s provider: %w", kind, err)
	}
	if store == nil {
		// A Factory that reports success must return something usable; a nil Store
		// here would panic much later, on the first push, with nothing pointing
		// back at the provider that produced it.
		return nil, fmt.Errorf("provider %q returned a nil Store with no error", kind)
	}
	return store, nil
}

// Kinds lists the registered names, sorted, for error messages and `-help`.
func (r *Registry) Kinds() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.kinds))
	for k := range r.kinds {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
