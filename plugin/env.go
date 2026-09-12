package plugin

import (
	"os"
	"sort"
	"strings"
)

// A plugin's environment is BUILT, not inherited.
//
// M9's brief says a plugin should not inherit the mount's ambient credentials,
// and the environment is where almost all of them live. The rule this file
// implements is the narrow, honest version of that: drivel passes on what a
// program needs in order to run correctly on the machine it is running on, and
// nothing that answers the question "who am I?" on the user's behalf.
//
// Concretely, this is what it prevents:
//
//   - GOOGLE_APPLICATION_CREDENTIALS, AWS_*, AZURE_*, and every other
//     ambient-credential convention. A backend authenticates with what its own
//     configuration names, which the user can read in their config file, and not
//     with something that happened to be exported in the shell that started the
//     mount. This is the difference between "this mount uses this account" being
//     a statement about a file and being a statement about a login session.
//   - XDG_CONFIG_HOME and friends. With several accounts, these are what decide
//     *whose* token a program finds. M16 already learned this the hard way: an
//     inherited XDG_CONFIG_HOME surviving the privilege drop left a mount running
//     as the user while reading root's account. A plugin gets absolute paths in
//     its config — `drivel login` writes them that way — so it never needs these,
//     and having them can only make it find the wrong one.
//   - DRIVEL_*. Nothing in drivel's own environment is a plugin's business, and
//     DRIVEL_PLUGIN_PATH in particular must not travel: a plugin that could load
//     plugins would turn one vetted search path into an unbounded chain of them.
//
// What it is NOT is a sandbox, and the limit is worth being plain about: the
// plugin runs as the same user, with that user's whole filesystem. Scrubbing the
// environment stops a credential being handed over by accident; it does not stop
// a plugin that wants one from going and reading it. Loading a plugin is
// trusting it. See the package doc.

// envAllowed is the exact set of variables a plugin inherits. Each entry is
// there because a plugin cannot behave correctly without it — not because it
// seemed harmless.
var envAllowed = map[string]string{
	// Finding programs, a home directory and scratch space. HOME is load-bearing
	// rather than conventional: the SFTP backend's known_hosts defaults to
	// ~/.ssh/known_hosts, and host key verification failing closed means a plugin
	// that cannot find it cannot connect at all.
	"PATH":   "where to find helper programs",
	"HOME":   "the user's home directory; SFTP's known_hosts default lives under it",
	"TMPDIR": "scratch space, so a plugin does not write to a /tmp the operator moved",

	// Locale and time, so a plugin's dates and messages match the rest of the
	// system's.
	"TZ":   "local time zone",
	"LANG": "locale",

	// TLS trust. A container or a corporate build may have its roots somewhere
	// other than the compiled-in default, and a plugin that cannot verify a
	// certificate cannot reach its backend.
	"SSL_CERT_FILE": "TLS root bundle, when the system's is not in the default place",
	"SSL_CERT_DIR":  "TLS root directory, likewise",

	// The proxy a user configured is a statement about how this machine reaches
	// the network, not a credential. A backend that ignored it would bypass the
	// only route out on many networks.
	"HTTP_PROXY":  "outbound proxy",
	"HTTPS_PROXY": "outbound proxy",
	"NO_PROXY":    "proxy exceptions",
	"http_proxy":  "outbound proxy (lowercase spelling)",
	"https_proxy": "outbound proxy (lowercase spelling)",
	"no_proxy":    "proxy exceptions (lowercase spelling)",

	// SSH_AUTH_SOCK is the one entry that IS a credential, and it is here
	// deliberately. The SFTP backend documents the agent as an authentication
	// method and has a setting for it (`agent = false` to decline); scrubbing the
	// socket would leave that setting quietly doing nothing, which is the failure
	// M8 rule 6 exists to prevent, and would break the commonest working SFTP
	// configuration there is. The disclosure that goes with it: every plugin
	// drivel launches can ask the agent to sign, so an agent forwarded into a
	// machine running an untrusted plugin is reachable by it.
	"SSH_AUTH_SOCK": "ssh-agent, which the SFTP backend offers as an auth method",
}

// pluginEnv builds the environment for a plugin process: the allowed variables
// that are actually set, and nothing else. LC_* is matched by prefix, since the
// set of those is open-ended and every one of them only selects a locale.
func pluginEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		k, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if _, allowed := envAllowed[k]; allowed || strings.HasPrefix(k, "LC_") {
			out = append(out, kv)
		}
	}
	sort.Strings(out)
	return out
}

// EnvAllowed lists the environment variables a plugin inherits, sorted. It is
// exported so that the documentation of this list can be generated from the list
// rather than written beside it and left to drift.
func EnvAllowed() []string {
	out := make([]string, 0, len(envAllowed)+1)
	for k := range envAllowed {
		out = append(out, k)
	}
	out = append(out, "LC_*")
	sort.Strings(out)
	return out
}
