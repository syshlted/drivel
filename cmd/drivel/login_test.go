package main

import (
	"bufio"
	"strings"
	"testing"
)

// The completions offer loginScopeNames; resolveScope is what actually accepts a
// scope. Two lists in two places is how a completion comes to suggest a value the
// program rejects, so this asserts every offered name resolves.
//
// A recognised name returns before anything is read, which is why an empty reader
// is enough: if one of these ever stops resolving, the test blocks on the prompt
// rather than passing, and the name that did it is in the failure.
func TestLoginScopeNamesAllResolve(t *testing.T) {
	for _, name := range loginScopeNames {
		got := resolveScope(bufio.NewReader(strings.NewReader("")), name)
		if got == "" || !strings.HasSuffix(got, name) {
			t.Errorf("resolveScope(%q) = %q; the completions offer a name the program does not accept", name, got)
		}
	}
}
