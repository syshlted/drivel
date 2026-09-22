// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package plugin

import (
	"bytes"
	"crypto/md5"  //nolint:gosec // G501: reproducing a backend's content digest, as the code under test does
	"crypto/sha1" //nolint:gosec // G505: same
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zishmusic/drivel/provider"
)

// hasher turns a hash constructor into the shape a backend's HashContent has, so
// the tests below can stand in for one.
func hasher(newHash func() hash.Hash, encode func([]byte) string) func(io.Reader) (string, error) {
	return func(r io.Reader) (string, error) {
		h := newHash()
		if _, err := io.Copy(h, r); err != nil {
			return "", err
		}
		return encode(h.Sum(nil)), nil
	}
}

func lowerHex(b []byte) string { return hex.EncodeToString(b) }

// A backend using a digest this build can compute is identified, so its content
// stops crossing the socket to be hashed.
func TestMatchLocalHashIdentifiesTheBackendsDigest(t *testing.T) {
	for _, tc := range []struct {
		want string
		new  func() hash.Hash
	}{
		{"md5", md5.New},
		{"sha1", sha1.New},
		{"sha256", sha256.New},
	} {
		t.Run(tc.want, func(t *testing.T) {
			name, newHash, err := matchLocalHash(hasher(tc.new, lowerHex))
			if err != nil {
				t.Fatalf("matchLocalHash: %v", err)
			}
			if name != tc.want || newHash == nil {
				t.Fatalf("identified %q (nil ctor: %v), want %q", name, newHash == nil, tc.want)
			}
			// The adopted constructor has to agree with the backend on bytes the
			// probe never saw, or the gate it feeds would start skipping real pushes.
			body := bytes.Repeat([]byte("content the probe did not use\n"), 97)
			mine, err := localDigest(newHash, bytes.NewReader(body))
			if err != nil {
				t.Fatalf("localDigest: %v", err)
			}
			theirs, err := hasher(tc.new, lowerHex)(bytes.NewReader(body))
			if err != nil {
				t.Fatalf("backend hash: %v", err)
			}
			if mine != theirs {
				t.Fatalf("adopted digest = %s, backend says %s", mine, theirs)
			}
		})
	}
}

// Anything the host cannot reproduce exactly is declined, and declining is not an
// error — it is the streaming path every backend used before this existed.
func TestMatchLocalHashDeclinesWhatItCannotReproduce(t *testing.T) {
	for _, tc := range []struct {
		name string
		ask  func(io.Reader) (string, error)
	}{
		{
			// The right algorithm in the wrong encoding. Adopting it would produce
			// digests that never match the remote's, so the gate would stop helping.
			name: "md5 in uppercase hex",
			ask:  hasher(md5.New, func(b []byte) string { return strings.ToUpper(hex.EncodeToString(b)) }),
		},
		{
			name: "md5 in base64",
			ask:  hasher(md5.New, base64.StdEncoding.EncodeToString),
		},
		{
			// What plugin/testdata/drivel-provider-fake actually returns.
			name: "a digest that is not one",
			ask: func(r io.Reader) (string, error) {
				n, err := io.Copy(io.Discard, r)
				return fmt.Sprintf("len:%d", n), err
			},
		},
		{
			name: "a constant",
			ask:  func(io.Reader) (string, error) { return "always-the-same", nil },
		},
		{
			// A backend with no digest to offer. contentMatches already reads an
			// empty remote hash as "cannot tell", so there is nothing to identify.
			name: "nothing at all",
			ask:  func(io.Reader) (string, error) { return "", nil },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name, newHash, err := matchLocalHash(tc.ask)
			if err != nil {
				t.Fatalf("declining must not be an error, got %v", err)
			}
			if newHash != nil {
				t.Fatalf("adopted %q, want the streaming fallback", name)
			}
		})
	}
}

// The probe uses more than one vector, and this is the test that says why: a
// backend that agrees on one input and not the next must not be adopted on the
// strength of the first.
func TestMatchLocalHashChecksEveryVector(t *testing.T) {
	honest := hasher(md5.New, lowerHex)
	asked := 0
	ask := func(r io.Reader) (string, error) {
		asked++
		sum, err := honest(r)
		if err != nil {
			return "", err
		}
		if asked == 1 {
			return sum, nil // honest md5 the first time...
		}
		return strings.Repeat("0", len(sum)), nil // ...and something else after
	}

	name, newHash, err := matchLocalHash(ask)
	if err != nil {
		t.Fatalf("matchLocalHash: %v", err)
	}
	if newHash != nil {
		t.Fatalf("adopted %q on one matching vector; every vector has to agree", name)
	}
	if asked < 2 {
		t.Fatalf("probed with %d vector(s); a single one is what this test exists to prevent", asked)
	}
}

// A backend that refuses the probe keeps its content streaming. It must not fail
// the push that triggered it.
func TestMatchLocalHashPropagatesAProbeFailure(t *testing.T) {
	boom := errors.New("backend said no")
	_, newHash, err := matchLocalHash(func(io.Reader) (string, error) { return "", boom })
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	if newHash != nil {
		t.Fatal("a failed probe must not adopt a digest")
	}
}

// Once the digest is identified, hashing touches nothing outside this process.
//
// The session is deliberately nil: if HashContent reached for the backend at all
// it would panic, so this passing is the property — the file's bytes no longer
// cross the socket to be counted.
func TestHashContentComputedLocallyNeverTouchesTheBackend(t *testing.T) {
	s := &remoteStore{hashNew: md5.New}
	s.hashOnce.Do(func() {}) // the probe has already run

	body := []byte("bytes that stay on this side\n")
	got, err := s.HashContent(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("HashContent: %v", err)
	}
	want, err := hasher(md5.New, lowerHex)(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("reference hash: %v", err)
	}
	if got != want {
		t.Fatalf("HashContent = %s, want %s", got, want)
	}
}

// identifyHash records the outcome for every later call, both ways round.
func TestIdentifyHashRecordsTheDecision(t *testing.T) {
	adopted := &remoteStore{}
	adopted.identifyHash(hasher(sha256.New, lowerHex))
	if adopted.hashNew == nil {
		t.Fatal("sha256 backend: want the local path")
	}

	streamed := &remoteStore{}
	streamed.identifyHash(func(io.Reader) (string, error) { return "len:7", nil })
	if streamed.hashNew != nil {
		t.Fatal("unidentifiable backend: want the streaming path")
	}
}

// A local read that fails must not become a digest. A short read would produce a
// digest of content nobody holds, which the unchanged-content gate would then
// compare against the remote.
func TestHashContentReportsALocalReadFailure(t *testing.T) {
	boom := errors.New("disk went away")
	s := &remoteStore{hashNew: md5.New}
	s.hashOnce.Do(func() {})

	_, err := s.HashContent(io.MultiReader(strings.NewReader("some of it"), errReader{boom}))
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap %v", err, boom)
	}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

// End to end, through a real backend process: a digest the host can reproduce
// means the file's bytes never reach that process.
//
// The journal is the evidence. The fake notes the byte count of every
// HashContent it is actually given, so the probe's two vectors appear and the
// megabyte that follows does not — which is the entire point of the change, and
// is not observable from the digest alone.
func TestPluginStopsSendingContentToBeHashed(t *testing.T) {
	journal := filepath.Join(t.TempDir(), "journal")
	store, _ := openFake(t, map[string]any{
		"capabilities": []string{"content-hasher"},
		"digest":       "md5",
		"journal":      journal,
	})

	h, ok := provider.AsContentHasher(store)
	if !ok {
		t.Fatal("no content hasher")
	}

	body := bytes.Repeat([]byte("payload that must not cross the socket\n"), 27000)
	got, err := h.HashContent(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("HashContent: %v", err)
	}
	want, err := hasher(md5.New, lowerHex)(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("reference hash: %v", err)
	}
	if got != want {
		t.Fatalf("HashContent = %s, want %s — the local digest disagrees with the backend's", got, want)
	}

	sent := hashedByteCounts(t, journal)
	for _, n := range sent {
		if n == int64(len(body)) {
			t.Fatalf("the backend was sent all %d bytes to hash; the host was supposed to do it", n)
		}
	}
	// The probe itself is allowed across, and has to have happened for the local
	// path to have been chosen at all.
	if len(sent) == 0 {
		t.Fatal("the backend was never asked to hash anything, so nothing identified its digest")
	}
	var probed int64
	for _, n := range sent {
		probed += n
	}
	if probed > 4096 {
		t.Errorf("the probe moved %d bytes; it is meant to be a couple of short vectors", probed)
	}
}

// A backend whose digest the host cannot reproduce keeps receiving content, and
// keeps returning the right answer. This is the fallback, and it is the path the
// rest of this package's tests run over.
func TestPluginKeepsStreamingAnUnidentifiableDigest(t *testing.T) {
	journal := filepath.Join(t.TempDir(), "journal")
	store, _ := openFake(t, map[string]any{
		"capabilities": []string{"content-hasher"},
		"journal":      journal, // no "digest", so the fake answers "len:N"
	})

	h, _ := provider.AsContentHasher(store)
	body := bytes.Repeat([]byte("x"), 5000)
	got, err := h.HashContent(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("HashContent: %v", err)
	}
	if want := fmt.Sprintf("len:%d", len(body)); got != want {
		t.Fatalf("HashContent = %q, want %q", got, want)
	}

	var sawWholeBody bool
	for _, n := range hashedByteCounts(t, journal) {
		if n == int64(len(body)) {
			sawWholeBody = true
		}
	}
	if !sawWholeBody {
		t.Fatal("the content never reached the backend, but only the backend can compute this digest")
	}
}

// hashedByteCounts reads the "hash N" lines the fake wrote.
func hashedByteCounts(t *testing.T, journal string) []int64 {
	t.Helper()
	b, err := os.ReadFile(journal)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		t.Fatalf("reading the journal: %v", err)
	}
	var out []int64
	for _, line := range strings.Split(string(b), "\n") {
		var n int64
		if _, err := fmt.Sscanf(strings.TrimSpace(line), "hash %d", &n); err == nil {
			out = append(out, n)
		}
	}
	return out
}
