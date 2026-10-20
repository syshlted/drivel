// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package syncengine

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// A backing filesystem that folds names — letter case on macOS's APFS and HFS+
// and on NTFS (WSL2's /mnt/c), Unicode normalisation on APFS too, or a Linux
// directory with the casefold attribute — cannot hold two names a
// case-sensitive backend can: Foo.txt and foo.txt are one local file. Nothing
// noticed. The pull writes through a temporary and a rename, and a rename onto
// foo.txt replaces Foo.txt's bytes and keeps Foo.txt's name, so a remote
// foo.txt silently overwrote the local Foo.txt — and the next push of that file
// sent foo.txt's content to the remote Foo.txt. Two remote folders differing in
// case merged into one local one.
//
// The project's rule for this (CLAUDE.md, the MC-30 same-name siblings, of
// which this is the third route) is to detect and report, not to emulate a
// case-sensitive namespace: which of the two names should exist here is the
// user's call. What is not anybody's call is writing one file's bytes into
// another's, so the pull refuses to apply a remote path that would land on a
// local entry stored under a different name, and says which one.

// Folding is what a backing filesystem treats as one name.
type Folding struct {
	Case          bool // Foo.txt and foo.txt name one file
	Normalization bool // é composed and decomposed name one file
}

// Any reports whether the filesystem folds anything at all.
func (f Folding) Any() bool { return f.Case || f.Normalization }

// String names what folds, for a log line.
func (f Folding) String() string {
	switch {
	case f.Case && f.Normalization:
		return "letter case and Unicode normalisation"
	case f.Case:
		return "letter case"
	case f.Normalization:
		return "Unicode normalisation"
	}
	return "nothing"
}

// ProbeFolding asks the filesystem holding dir what it folds, by creating one
// probe file and looking it up by other spellings of its name. It is a property
// of the filesystem — or, for Linux's casefold, of the directory — so it is
// asked of the backing root, which is where a mount's tree starts. The probe is
// a .drivel- name, so a leftover one is never synced.
func ProbeFolding(dir string) (Folding, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return Folding{}, err
	}
	tag := hex.EncodeToString(b[:])
	// "É" composed (U+00C9): its lower case is "é", and decomposed it is
	// "E" + U+0301 — one name for each question.
	name := ".drivel-fold-" + tag + "-\u00c9"
	p := filepath.Join(dir, name)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return Folding{}, err
	}
	f.Close()
	defer os.Remove(p)
	var out Folding
	if _, err := os.Lstat(filepath.Join(dir, ".drivel-fold-"+tag+"-\u00e9")); err == nil {
		out.Case = true
	}
	if _, err := os.Lstat(filepath.Join(dir, ".drivel-fold-"+tag+"-E\u0301")); err == nil {
		out.Normalization = true
	}
	return out, nil
}

// aliasOf reports whether rel, a root-relative slash path, would land on a local
// entry stored under a different name — and returns that entry's own path. It
// walks rel's components from the root, because a folded directory name hides
// everything under it: remote sub/x would be written into local Sub/x.
//
// It answers false whenever a component does not exist, since nothing can be
// overwritten there, and on any error, which leaves the apply exactly as it
// was before this check existed. It costs a Lstat and a name lookup per
// existing component, which is why it runs only on a filesystem ProbeFolding
// found folding.
func aliasOf(root, rel string) (string, bool) {
	if rel == "" || !relPathOK(rel) {
		return "", false // nothing to look up, or a path that would climb out of root
	}
	parts := strings.Split(rel, "/")
	dir := root
	for i, part := range parts {
		p := filepath.Join(dir, part)
		fi, err := os.Lstat(p)
		if err != nil {
			return "", false
		}
		stored, err := storedName(dir, part, p, fi)
		if err != nil {
			return "", false
		}
		// One name, never a path: Base makes that so whatever the lookup said.
		stored = filepath.Base(stored)
		if stored != part {
			alias := strings.Join(append(append([]string(nil), parts[:i]...), stored), "/")
			// One component the kernel or the listing named, under components rel
			// already had: inside root by construction, and checked anyway, since
			// callers go on to use it as a path.
			return alias, relPathOK(alias)
		}
		dir = p
	}
	return "", false
}

// moveEntry renames src to dst. When dst is src under another spelling (recase),
// one rename is not enough: a folding filesystem may treat it as a rename onto
// itself, which rename(2) answers by doing nothing — measured on a casefold
// tmpfs — so the entry goes through a temporary name in the same directory.
func moveEntry(src, dst string, recase bool) error {
	if !recase {
		return os.Rename(src, dst)
	}
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(src), ".drivel-recase-"+hex.EncodeToString(b[:]))
	if err := os.Rename(src, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		// Put it back where it was rather than leave it under a .drivel- name the
		// walk would never sync.
		return errors.Join(err, os.Rename(tmp, src))
	}
	return nil
}

// storedNameByListing is the portable answer to "what name does dir store the
// entry that name resolves to under?": the directory's own listing, and the
// entry in it that is the same file. A listing is O(directory), so the
// platforms that can ask the kernel directly do (alias_linux.go).
func storedNameByListing(dir, name string, fi os.FileInfo) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	match := ""
	for _, e := range entries {
		if e.Name() == name {
			return name, nil // stored as asked: the common case, and hard links' too
		}
		if match != "" {
			continue
		}
		if efi, err := os.Lstat(filepath.Join(dir, e.Name())); err == nil && os.SameFile(efi, fi) {
			match = e.Name()
		}
	}
	if match == "" {
		return "", errors.Join(fs.ErrNotExist, errors.New("no entry in the listing is that file"))
	}
	return match, nil
}
