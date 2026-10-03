// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

//go:build !nobundle

package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/syshlted/drivel/plugin"
)

// Every backend that ships in the tree is bundled in the default build. The
// cross-check against driveKind is the one that matters: the flag path selects
// that kind by name, so a mount with -credentials and no plugin directory
// depends on this map holding exactly that spelling.
func TestEveryShippedBackendIsBundled(t *testing.T) {
	t.Parallel()

	for _, kind := range []string{driveKind, sftpKind} {
		if bundledBackends[kind] == nil {
			t.Errorf("no bundled %q backend; bundled: %q", kind, bundledKinds())
		}
	}
	// One executable per backend still exists as a build target, because that is
	// how an out-of-tree backend is built at all — and because the installed
	// launch path rots if nothing exercises it.
	for _, kind := range bundledKinds() {
		dir := filepath.Join("..", plugin.BinaryPrefix+kind)
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("cmd/%s%s is gone, so nothing builds %s as an installable plugin: %v",
				plugin.BinaryPrefix, kind, kind, err)
		}
	}
}

// The other half of the bundled launch: that `drivel plugin-serve KIND` reaches
// a real backend and speaks the real protocol.
//
// It runs the built binary rather than calling plugin.Serve, because the thing
// under test is argv dispatch — and it reads go-plugin's handshake line, which
// is the first and only thing a host trusts before it dials. No network and no
// FUSE: the factory is not called until Open, so this is the launch and the
// handshake alone.
func TestPluginServeServesEveryBundledBackend(t *testing.T) {
	bin := drivelBinary(t)

	for _, kind := range bundledKinds() {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
			defer cancel()

			cmd := exec.CommandContext(ctx, bin, plugin.ServeCommand, kind)
			// The cookie is what the host sets; without it go-plugin refuses to
			// serve, which is why running this subcommand by hand is already inert.
			cmd.Env = append(os.Environ(), plugin.MagicCookieKey+"="+plugin.MagicCookieValue)
			out, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var stderr strings.Builder
			cmd.Stderr = &stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
			}()

			line, err := bufio.NewReader(out).ReadString('\n')
			if err != nil {
				t.Fatalf("no handshake from `drivel %s %s`: %v\nstderr: %s",
					plugin.ServeCommand, kind, err, stderr.String())
			}

			// go-plugin's greeting: core protocol | our version | network | address
			// | protocol. Three of the five are properties drivel decides.
			f := strings.Split(strings.TrimSpace(line), "|")
			if len(f) < 5 {
				t.Fatalf("handshake %q is not go-plugin's five fields", line)
			}
			if got, want := f[1], strconv.Itoa(plugin.ProtocolVersion); got != want {
				t.Errorf("handshake protocol version = %s, want %s", got, want)
			}
			if f[2] != "unix" {
				t.Errorf("handshake network = %q; a backend is reached over a unix socket only", f[2])
			}
			if f[4] != "grpc" {
				t.Errorf("handshake protocol = %q, want grpc; net/rpc loses every sentinel in the seam", f[4])
			}
		})
	}
}

// drivelBinary builds this command once per test binary. The default tags, so
// what it builds is the bundled binary this file is testing.
var drivelBinary = func() func(*testing.T) string {
	var once sync.Once
	var path string
	var err error
	return func(t *testing.T) string {
		t.Helper()
		once.Do(func() {
			dir, derr := os.MkdirTemp("", "drivel-bundle-test-")
			if derr != nil {
				err = derr
				return
			}
			path = filepath.Join(dir, "drivel")
			if combined, berr := exec.Command("go", "build", "-o", path, ".").CombinedOutput(); berr != nil {
				err = fmt.Errorf("building drivel: %w\n%s", berr, combined)
			}
		})
		if err != nil {
			t.Fatalf("%v", err)
		}
		return path
	}
}()
