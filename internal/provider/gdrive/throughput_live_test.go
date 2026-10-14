// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

// Constrained because getrusage is how the CPU half of this measurement is
// taken and x/sys/unix has no Windows build: §2.9's claim is that every
// cross-compile failure in this tree traces to go-fuse, and a test file is part
// of the tree. These are the platforms drivel supports anyway, and the live rig
// is Linux.
//go:build linux || darwin || freebsd

package gdrive

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// MC-44's throughput half: what a bulk transfer costs over each protocol.
//
// The transport half of that case ran without an account at all, by driving
// internal/transport against a public endpoint — which is where both of its
// findings were, and which answered when the fallback fires and whether HTTP/3
// ever comes back. It could not answer the comparison the case specifies,
// because what it measured was a request-rate test against 384 KB bodies, and
// because the HTTP/3 transport is injected into *this* provider and nowhere
// else. So the measurement has to go through Put and Get, and it has to go
// through real Drive.
//
// This test measures ONE protocol per run and does not know which. That is
// deliberate: there is no product knob for selecting a protocol, and adding one
// to be testable would put a lever in a release binary for the benefit of a
// measurement. What selects it is the environment — dropping UDP/443 for this
// process's cgroup makes the fallback carry the traffic — and what attributes it
// is nftables counters outside the process. scripts/multiclient/mc44-throughput.sh
// is the harness that does both halves and prints the comparison;
// scripts/multiclient/README.md has the procedure and the warning about proving
// the scope before believing a number.
//
// Off unless DRIVEL_LIVE_RIG is set, and it writes: it insists on a folder ID of
// its own and refuses "root". Everything it creates it deletes.
func TestLiveTransferThroughput(t *testing.T) {
	if os.Getenv("DRIVEL_LIVE_RIG") == "" {
		t.Skip("set DRIVEL_LIVE_RIG=1 with -creds/-token/-root env to run")
	}
	root := os.Getenv("DRIVEL_LIVE_ROOT")
	if root == "" || root == "root" {
		t.Fatal("DRIVEL_LIVE_ROOT must name a dedicated folder ID, never \"root\"")
	}

	// Default 64 MiB: four of mediaOptions' 16 MiB chunks, so the number
	// describes steady-state transfer rather than one chunk's handshake, while
	// still costing a minute rather than an hour of a free account's quota.
	mib := 64
	if s := os.Getenv("DRIVEL_LIVE_MB"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 {
			t.Fatalf("DRIVEL_LIVE_MB=%q: want a positive number of MiB", s)
		}
		mib = n
	}
	size := int64(mib) << 20

	// A deadline generous enough that a slow link is a slow measurement rather
	// than a failure: the thing being measured is how long this takes.
	lctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	d, err := Open(lctx, Config{
		Credentials: os.Getenv("DRIVEL_CREDS"),
		Token:       os.Getenv("DRIVEL_TOKEN"),
		RootID:      root,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Before anything it has to outlive: t.Cleanup runs after every deferred
	// call, so `defer d.Close()` would shut the transport before the delete.
	t.Cleanup(func() { _ = d.Close() })

	local := filepath.Join(t.TempDir(), "payload.bin")
	if err := writePayload(local, size); err != nil {
		t.Fatalf("generating %d MiB of payload: %v", mib, err)
	}
	want, err := hashFile(d, local)
	if err != nil {
		t.Fatalf("hashing the payload: %v", err)
	}
	t.Logf("payload: %d MiB, md5 %s", mib, want)

	name := fmt.Sprintf("drivel-live-throughput-%d.bin", os.Getpid())
	t.Cleanup(func() {
		ctx, c := context.WithTimeout(context.Background(), 2*time.Minute)
		defer c()
		id, ok := resolve(t, d, name)
		if !ok {
			return
		}
		// Permanent: a trashed object still costs the quota the next run needs.
		if err := d.svc.Files.Delete(id).Context(ctx).Do(); err != nil {
			t.Logf("cleanup: %s (%s) left behind: %v", name, id, err)
		}
	})

	// ---- upload ----
	f, err := os.Open(local)
	if err != nil {
		t.Fatalf("open payload: %v", err)
	}
	upCPU, upWall := func() (time.Duration, time.Duration) {
		defer f.Close() //nolint:errcheck // read-only
		cpu0, wall0 := cpuTime(), time.Now()
		rf, perr := d.Put(lctx, name, f)
		cpu, wall := cpuTime()-cpu0, time.Since(wall0)
		if perr != nil {
			t.Fatalf("put after %v: %v", wall, perr)
		}
		// Drive's own digest of what it stored, which makes the upload leg
		// self-verifying: a fast transfer of the wrong bytes is not a result.
		if rf.Hash != "" && rf.Hash != want {
			t.Errorf("uploaded md5 %s, want %s — the transfer corrupted the file", rf.Hash, want)
		}
		if rf.Size != size {
			t.Errorf("remote size %d, want %d", rf.Size, size)
		}
		return cpu, wall
	}()
	report(t, "upload", size, upWall, upCPU)

	// ---- download ----
	rc, err := d.Get(lctx, name)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	dnCPU, dnWall, got, n := func() (time.Duration, time.Duration, string, int64) {
		defer rc.Close() //nolint:errcheck // read side
		cpu0, wall0 := cpuTime(), time.Now()
		// Hash while streaming: buffering 64 MiB to hash it afterwards would
		// measure this machine's memory bandwidth as part of the download.
		cw := &countingReader{r: rc}
		h, herr := d.HashContent(cw)
		cpu, wall := cpuTime()-cpu0, time.Since(wall0)
		if herr != nil {
			t.Fatalf("download after %v (%d of %d bytes): %v", wall, cw.n, size, herr)
		}
		return cpu, wall, h, cw.n
	}()
	if n != size {
		t.Errorf("downloaded %d bytes, want %d", n, size)
	}
	if got != want {
		t.Errorf("downloaded md5 %s, want %s — the round trip corrupted the file", got, want)
	}
	report(t, "download", size, dnWall, dnCPU)
}

// report prints one line per leg in a shape a harness can grep and a human can
// read. The CPU figure is the whole process's, which for this test is the
// transport plus the digest and little else; it is reported per MiB because that
// is the only form in which the two protocols are comparable across runs of
// different sizes.
func report(t *testing.T, leg string, size int64, wall, cpu time.Duration) {
	t.Helper()
	mib := float64(size) / (1 << 20)
	t.Logf("MC44 %s: %.1f MiB in %v = %.2f MiB/s, cpu %v (%.1f ms/MiB)",
		leg, mib, wall.Round(time.Millisecond), mib/wall.Seconds(),
		cpu.Round(time.Millisecond), float64(cpu.Milliseconds())/mib)
}

// cpuTime is this process's user+system time. getrusage rather than a wall clock
// because the question "what does each protocol cost a client" is about CPU as
// much as about seconds — quic-go does in userspace what the kernel does for
// TCP, which is the one place HTTP/3 is expected to lose.
func cpuTime() time.Duration {
	var ru unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	tv := func(t unix.Timeval) time.Duration {
		return time.Duration(t.Sec)*time.Second + time.Duration(t.Usec)*time.Microsecond
	}
	return tv(ru.Utime) + tv(ru.Stime)
}

// writePayload fills path with size bytes of incompressible data.
//
// Incompressible so the number is a transfer rate and not a property of the
// payload, and deterministic — a fixed seed — so the HTTP/3 and HTTP/2 runs push
// identical bytes and the comparison is of the transport alone.
func writePayload(path string, size int64) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck // the Sync below is the durability point

	rng := rand.New(rand.NewPCG(0x4d433434, 0x74687075))
	buf := make([]byte, 1<<20)
	for left := size; left > 0; {
		for i := 0; i+8 <= len(buf); i += 8 {
			binary.LittleEndian.PutUint64(buf[i:], rng.Uint64())
		}
		chunk := buf
		if left < int64(len(chunk)) {
			chunk = chunk[:left]
		}
		if _, err := f.Write(chunk); err != nil {
			return err
		}
		left -= int64(len(chunk))
	}
	return f.Sync()
}

// countingReader reports how far a stream actually got, so a download that ends
// early says so with a byte count instead of only a digest mismatch.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// hashFile is the provider's own digest of a local file, so the test names no
// hash algorithm of its own and compares like against like with what Drive
// reports.
func hashFile(d *Drive, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close() //nolint:errcheck // read-only
	return d.HashContent(f)
}
