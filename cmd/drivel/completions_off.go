//go:build !completions

package main

// runExtraCommand is where a build-tagged subcommand hooks into the dispatch in
// main. In an ordinary build there are none, and this is the whole of it.
//
// The on/off file pair is the same shape the platform-specific files use
// (mountopts_linux.go and friends): no init(), no package-level registry to
// mutate, and the compiler — not a runtime check — is what decides which half
// exists. See completions.go for the other half.
func runExtraCommand(_ string, _ []string) (handled bool, err error) { return false, nil }
