// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/syshlted/drivel/internal/control"
	"github.com/syshlted/drivel/internal/testenv"
)

// The test M14 names for its shutdown: POST /v1/shutdown drains byte for byte
// like a signal. Both are run, against a push delay long enough that every
// write is still sitting in the debounce map when the shutdown arrives, so
// what is asserted is the drain and not an upload that happened to be quick.
//
// The API path is the real one end to end — a real mount, the real control
// server over a real socket, and a shutdown callback that does what
// cmd/drivel's does: cancel the context the signal would have cancelled.
func TestAnAPIShutdownDrainsLikeASignal(t *testing.T) {
	if _, err := os.Stat("/dev/fuse"); err != nil {
		testenv.Unavailable(t, testenv.FUSE, "no /dev/fuse: "+err.Error())
	}
	for _, how := range []string{"signal", "api"} {
		t.Run(how, func(t *testing.T) {
			store := newMemStore()
			spec := baseSpec(t)
			spec.Provider = "mem"
			spec.PushDelay = time.Hour // nothing goes out until the drain sends it
			spec.Logger = log.New(io.Discard, "", 0)

			a, err := New(t.Context(), []MountSpec{spec}, registryWith(t, "mem", store))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			defer a.Close() //nolint:errcheck // best effort in cleanup

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			dir, err := os.MkdirTemp("", "dctl")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			sock := filepath.Join(dir, "drivel", control.SocketName)
			ln, err := control.Listen(sock)
			if err != nil {
				t.Fatal(err)
			}
			mounts := []control.Mount{a.Mounts()[0]}
			srv := control.New(mounts, func(string) { cancel() }, log.New(io.Discard, "", 0))
			go func() { _ = srv.Serve(ln) }()
			defer srv.Close() //nolint:errcheck // best effort in cleanup

			done := make(chan error, 1)
			go func() { done <- a.Run(ctx) }()

			probe := ".mount-probe"
			if err := os.WriteFile(filepath.Join(spec.DataDir, probe), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			if !waitFor(10*time.Second, func() bool {
				_, err := os.Stat(filepath.Join(spec.Mountpoint, probe))
				return err == nil
			}) {
				cancel()
				testenv.Unavailable(t, testenv.FUSE, "mount did not come up within 10s")
			}

			want := map[string][]byte{}
			for i := 0; i < 20; i++ {
				name := fmt.Sprintf("f%02d.bin", i)
				b := bytes.Repeat([]byte{byte(i)}, 4096+i*1000)
				if err := os.WriteFile(filepath.Join(spec.Mountpoint, name), b, 0o644); err != nil {
					t.Fatal(err)
				}
				want[name] = b
			}

			client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			}}}
			// The status of a write still in the debounce map, through the real
			// mount's wiring: pending, and not synced. Waited for rather than read
			// once, because a new file's event leaves with its RELEASE, which the
			// kernel sends after close(2) has already returned.
			var st struct {
				Status string `json:"status"`
			}
			if !waitFor(5*time.Second, func() bool {
				resp, err := client.Get("http://drivel/v1/mounts/test/status?path=f00.bin")
				if err != nil {
					return false
				}
				_ = json.NewDecoder(resp.Body).Decode(&st)
				resp.Body.Close()
				return st.Status == "pending"
			}) {
				t.Errorf("a write waiting out the push delay reads %q; want pending", st.Status)
			}
			store.mu.Lock()
			n := len(store.files)
			store.mu.Unlock()
			if n > 1 {
				t.Fatalf("%d object(s) uploaded before the shutdown; the push delay did not hold", n)
			}

			switch how {
			case "signal":
				cancel()
			case "api":
				resp, err := client.Post("http://drivel/v1/shutdown", "", nil)
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				if resp.StatusCode != http.StatusAccepted {
					t.Fatalf("shutdown: %s", resp.Status)
				}
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("Run: %v", err)
				}
			case <-time.After(60 * time.Second):
				t.Fatalf("Run did not return; try: fusermount3 -u %s", spec.Mountpoint)
			}

			for name, b := range want {
				got, ok := store.content(name)
				if !ok {
					t.Errorf("%s: never uploaded by the drain", name)
					continue
				}
				if !bytes.Equal(got, b) {
					t.Errorf("%s: drained %d bytes, want %d identical", name, len(got), len(b))
				}
			}
		})
	}
}
