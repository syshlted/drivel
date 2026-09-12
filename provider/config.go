package provider

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config is one provider's own settings, still in the encoding the user wrote
// them in: a TOML table, with no leading `[header]`.
//
// Before M9 this crossed the seam as a closure — `func(any) error`, built by
// whichever layer had the settings in hand. A closure cannot cross a process
// boundary, so the seam now carries the bytes and the decoding happens wherever
// the provider is. The rule it exists to serve is unchanged and is M8's rule 4:
// **the generic layer holds the shape, the provider holds the meaning.**
// internal/config still never learns what a Drive folder ID is; it just stops
// being the layer that runs the decoder.
//
// TOML rather than JSON for one reason: it is what the user typed. A config
// file's `[account.work]` table arrives here as the same text the user wrote, a
// provider's settings struct carries the same `toml:"…"` tags a hand-written
// config is matched against, and the error for a misspelt key names the key as
// it appears in the file. Round-tripping through a second encoding would put a
// translation between the message and the thing the reader has to fix.
//
// The zero Config is valid and decodes to the destination's zero value: a mount
// that supplies no settings for its kind is not an error here, it is an error
// in the Factory, which is where the message that helps ("no credentials
// configured — run 'drivel login'") can actually be written.
type Config []byte

// EncodeConfig renders an already-built settings struct as a Config, for callers
// that construct one directly instead of reading it out of a file — the flag
// path, the fstab option table, and tests.
//
// v must be encodable as a TOML table (a struct or a map), which in practice
// means the same type the provider decodes into. Encoding the provider's own
// struct is deliberate: it round-trips through the very `toml:"…"` tags that a
// hand-written config file is matched against, so a tag that is wrong is wrong
// on both paths rather than on only the one nobody tests.
func EncodeConfig(v any) (Config, error) {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(v); err != nil {
		return nil, fmt.Errorf("provider: encoding %T as config: %w", v, err)
	}
	return Config(buf.Bytes()), nil
}

// MustEncodeConfig is EncodeConfig for a value known at author time to be
// encodable — a literal built a few lines above by the same function. It panics
// on failure, because the only way to reach it is a type that could never have
// worked, in code that has no user input in it yet.
func MustEncodeConfig(v any) Config {
	c, err := EncodeConfig(v)
	if err != nil {
		panic(err)
	}
	return c
}

// Decode fills a provider-defined struct — gdrive's Config, an SFTP host and
// key, a WebDAV URL — from these settings.
//
// An unrecognised key is an error, not a warning. That is M8's rule 6 and it is
// the same failure as a flag that silently stopped being read: `lazzy = true`
// doing nothing looks exactly like lazy mode being on. The error names every
// unknown key at once, sorted, because someone fixing a config file would rather
// see all three typos than find them one run at a time.
func (c Config) Decode(dst any) error {
	if len(c) == 0 {
		return nil
	}
	md, err := toml.Decode(string(c), dst)
	if err != nil {
		return fmt.Errorf("provider settings: %w", err)
	}
	if u := md.Undecoded(); len(u) > 0 {
		keys := make([]string, 0, len(u))
		for _, k := range u {
			keys = append(keys, k.String())
		}
		sort.Strings(keys)
		noun := "key"
		if len(keys) > 1 {
			noun = "keys"
		}
		return fmt.Errorf("provider settings: unknown %s: %s", noun, strings.Join(keys, ", "))
	}
	return nil
}

// String renders the settings for a log line with the VALUES REMOVED — just the
// key names, sorted, in braces.
//
// It exists because Config is a []byte, so an ordinary `%v`, `%s` or `%q` would
// otherwise print a provider's whole configuration into the log, and that is the
// one place a credential could end up in a file the user then pastes into a bug
// report. Credentials are a *path* in every backend drivel ships, which makes
// the current risk small; the point is that it stays small when a backend that
// takes an inline secret is added, without that backend's author having to know
// this existed.
//
// Settings that cannot be parsed render as "{unparsed}" rather than as their
// text: a Config that failed to decode is exactly the one someone is about to
// print while debugging.
func (c Config) String() string {
	if len(c) == 0 {
		return "{}"
	}
	var m map[string]any
	if _, err := toml.Decode(string(c), &m); err != nil {
		return "{unparsed}"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return "{" + strings.Join(keys, " ") + "}"
}
