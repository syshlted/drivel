// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package app

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/syshlted/drivel/internal/testenv"
)

// A backing store that folds names is said so at open, once, before any of the
// skips it causes. DRIVEL_CASEFOLD_DIR names a case-folding directory to run it
// in (see syncengine's TestOnARealFoldingFilesystem); the control below runs
// everywhere.
func TestOpenWarnsAboutAFoldingBackingStore(t *testing.T) {
	root := testenv.FoldingDir(t)
	dir, err := os.MkdirTemp(root, "drivel-open-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	spec := baseSpec(t)
	spec.DataDir = filepath.Join(dir, "data")
	if got := openAndLog(t, spec); !strings.Contains(got, "treats names that differ only in letter case") {
		t.Fatalf("no warning for a folding backing store:\n%s", got)
	}
}

func TestOpenDoesNotWarnAboutACaseSensitiveBackingStore(t *testing.T) {
	spec := baseSpec(t)
	if err := os.MkdirAll(spec.DataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(spec.DataDir, "A"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(spec.DataDir, "a")); err == nil {
		t.Skip("TMPDIR folds case here; the control has nothing to say")
	}
	if got := openAndLog(t, spec); strings.Contains(got, "differ only in") {
		t.Fatalf("a case-sensitive backing store was warned about:\n%s", got)
	}
}

func openAndLog(t *testing.T, spec MountSpec) string {
	t.Helper()
	var buf bytes.Buffer
	spec.Logger = log.New(&buf, "", 0)
	spec.Provider = "fake"
	m, err := Open(t.Context(), spec, registryWith(t, "fake", newFakeStore()))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}
