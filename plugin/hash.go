// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package plugin

import (
	"bytes"
	"crypto/md5"  //nolint:gosec // G501: reproducing a backend's content digest, never a security property
	"crypto/sha1" //nolint:gosec // G505: same
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"io"
)

// Working out which digest a backend computes, so its bytes do not have to
// travel to be hashed.
//
// provider.ContentHasher is M6's third gate: hash the local file and compare
// against what the remote says it holds, so an unchanged file is not uploaded
// again. In process that is a local read. Across the plugin seam the naive
// implementation streams the whole local file to the backend and gets a hex
// string back — so every candidate file crosses the socket to decide whether it
// should cross the network, and one that does need pushing crosses twice.
//
// Drive is the case that makes this matter rather than a hypothetical: it
// declines RangePutter deliberately, so M6's gate 2 always falls through and
// gate 3 runs on every content push at or above one block.
//
// The fix is not to let the host assume an algorithm. It identifies the
// backend's by asking for digests of bytes it can check for itself, and uses a
// local implementation only when one reproduces them exactly. Anything else
// streams, exactly as before.

// hashCandidates are the digests this build can compute, tried in order.
//
// The string is lowercase hex of the raw digest, which is what Drive's
// md5Checksum is and what every backend in the tree returns. A backend encoding
// the same algorithm differently — base64, uppercase — simply does not match,
// and streams. That is the right outcome rather than a missing case: guessing at
// encodings multiplies the candidates for a backend nobody has written yet, and
// the penalty for not matching is bandwidth rather than correctness.
//
// MD5 and SHA-1 are here because backends use them as content digests, not as
// security properties. Nothing in this file authenticates anything: a digest
// that matched by collision would skip one upload of content the remote already
// claims to hold under that same digest.
var hashCandidates = []struct {
	name string
	new  func() hash.Hash
}{
	{"md5", md5.New},
	{"sha1", sha1.New},
	{"sha256", sha256.New},
}

// hashProbeVectors is the fixed input a backend is asked to digest.
//
// Two vectors rather than one, because a single input that two different
// functions happened to agree on would be adopted for every file afterwards. The
// second crosses several 64-byte compression blocks, so a backend that digests
// only its first block, or truncates, does not pass by accident.
//
// Deterministic by construction. A random probe would make the one branch that
// decides whether file content crosses a socket depend on the draw.
func hashProbeVectors() [][]byte {
	long := make([]byte, 1000)
	for i := range long {
		long[i] = byte(i*7 + 11)
	}
	return [][]byte{[]byte("drivel content digest probe\n"), long}
}

// localDigest computes one candidate's digest of r, in the encoding
// hashCandidates documents.
func localDigest(newHash func() hash.Hash, r io.Reader) (string, error) {
	h := newHash()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// matchLocalHash identifies the digest behind ask, which is a backend's own
// HashContent.
//
// The backend is never asked to *name* anything, and its answer is not taken on
// trust: the host digests the same bytes with each algorithm it can compute and
// adopts the one that reproduces every vector. That is the same reason capability
// detection is negotiated rather than asserted — a backend cannot be wrong about
// an answer that is checked against its own output, and a plugin that lied here
// would be caught by the check rather than believed.
//
// No match is an ordinary outcome, not a failure: it returns a nil constructor
// and no error, and the caller keeps streaming.
func matchLocalHash(ask func(io.Reader) (string, error)) (string, func() hash.Hash, error) {
	vectors := hashProbeVectors()
	want := make([]string, len(vectors))
	for i, v := range vectors {
		got, err := ask(bytes.NewReader(v))
		if err != nil {
			return "", nil, err
		}
		if got == "" {
			// A backend that returns no digest has nothing to identify, and
			// contentMatches already treats an empty remote hash as "cannot tell".
			return "", nil, nil
		}
		want[i] = got
	}

	for _, c := range hashCandidates {
		matched := true
		for i, v := range vectors {
			got, err := localDigest(c.new, bytes.NewReader(v))
			if err != nil || got != want[i] {
				matched = false
				break
			}
		}
		if matched {
			return c.name, c.new, nil
		}
	}
	return "", nil, nil
}
