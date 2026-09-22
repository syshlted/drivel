// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

//go:build linux || darwin || freebsd

package hydrate

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zishmusic/drivel/internal/testenv"
)

// probeFile returns a file on a filesystem that stores user xattrs, skipping (or
// failing, under DRIVEL_REQUIRE_TESTENV) when there is none.
func probeFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte("body"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := setxattr(p, XattrName, []byte("probe")); err != nil {
		if isNoSupport(err) {
			testenv.Unavailable(t, testenv.Xattr, "backing filesystem does not store user.* attributes")
		}
		t.Fatalf("setxattr: %v", err)
	}
	if err := removexattr(p, XattrName); err != nil {
		t.Fatalf("removexattr: %v", err)
	}
	return p
}

// The three primitives are the whole per-platform surface of M5's marker, and
// this is the test that says the platform underneath them behaves. It is
// deliberately about the syscall layer rather than about placeholders: on a
// kernel drivel has not run on before, this failing is a different diagnosis from
// hydrate_test.go failing.
func TestXattrRoundTrip(t *testing.T) {
	p := probeFile(t)

	if err := setxattr(p, XattrName, []byte(`{"v":1}`)); err != nil {
		t.Fatalf("setxattr: %v", err)
	}
	got, err := getxattr(p, XattrName)
	if err != nil {
		t.Fatalf("getxattr: %v", err)
	}
	if string(got) != `{"v":1}` {
		t.Errorf("value = %q; want %q", got, `{"v":1}`)
	}

	// Overwriting in place, which is what re-stamping a placeholder does.
	if err := setxattr(p, XattrName, []byte(`{"v":1,"size":9}`)); err != nil {
		t.Fatalf("setxattr overwrite: %v", err)
	}
	if got, err = getxattr(p, XattrName); err != nil {
		t.Fatalf("getxattr after overwrite: %v", err)
	}
	if string(got) != `{"v":1,"size":9}` {
		t.Errorf("value after overwrite = %q", got)
	}

	if err := removexattr(p, XattrName); err != nil {
		t.Fatalf("removexattr: %v", err)
	}
	if _, err := getxattr(p, XattrName); !isNoAttr(err) {
		t.Errorf("getxattr after remove = %v; want an isNoAttr error", err)
	}
}

// isNoAttr is the one thing that is spelled differently on each kernel — ENODATA
// on Linux, ENOATTR on macOS — and the cost of getting it wrong is not a wrong
// answer but a permanent one: Marker returns the error, IsPlaceholder fails safe
// by reporting "placeholder", and the file is never pushed again.
func TestMissingAttributeIsNotAnError(t *testing.T) {
	p := probeFile(t)

	if _, err := getxattr(p, XattrName); !isNoAttr(err) {
		t.Errorf("getxattr of an unset attribute = %v; want an isNoAttr error", err)
	}
	if err := removexattr(p, XattrName); !isNoAttr(err) {
		t.Errorf("removexattr of an unset attribute = %v; want an isNoAttr error", err)
	}
	if _, err := getxattr(filepath.Join(filepath.Dir(p), "absent"), XattrName); !isNoAttr(err) {
		t.Errorf("getxattr of a missing file = %v; want an isNoAttr error", err)
	}

	// And the classification the caller actually makes: a marker that is not there
	// is "not a placeholder", never an error to fail safe on.
	h := New(filepath.Dir(p), nil, nil, 0)
	if _, ok, err := h.Marker(filepath.Base(p)); err != nil || ok {
		t.Errorf("Marker on an unmarked file = (ok %v, err %v); want (false, nil)", ok, err)
	}
	if h.IsPlaceholder(filepath.Base(p)) {
		t.Error("IsPlaceholder on an unmarked file = true; a resident file would never be pushed")
	}
}

// XattrsUsable must not answer yes for a store that merely accepted the write:
// on macOS an AppleDouble sidecar accepts it too. Here it is the positive case
// that is checkable — a temp dir on a native filesystem — with the negative one
// reachable only on a volume xattr_darwin.go describes.
func TestXattrsUsableOnANativeStore(t *testing.T) {
	dir := filepath.Dir(probeFile(t))
	if !New(dir, nil, nil, 0).XattrsUsable() {
		t.Error("XattrsUsable = false on a filesystem that just stored an attribute")
	}
	if _, err := os.Lstat(filepath.Join(dir, ".drivel-xattr-probe")); !os.IsNotExist(err) {
		t.Errorf("the probe file outlived the probe: %v", err)
	}
}
