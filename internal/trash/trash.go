// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

// Package trash puts files in the user's home trash, as the freedesktop.org
// Trash specification (version 1.0) lays it out: a file goes to
// $XDG_DATA_HOME/Trash/files/NAME, and an info/NAME.trashinfo beside it records
// where it came from and when, which is what lets a desktop's "Restore" put it
// back.
//
// It is the destination for `conflicts = "local-trash"`, and deliberately the
// home trash only. The specification also describes a per-volume
// $topdir/.Trash-$uid, which avoids a copy when the file is on another
// filesystem; a drivel backing tree is often its own filesystem, and a trash at
// the top of that filesystem may be inside a directory drivel syncs. The home
// trash costs a copy across filesystems and can never be synced by accident,
// because Dir refuses one that is.
//
// The package does no locking beyond what the specification asks for: a name
// is reserved by creating its .trashinfo with O_EXCL, so two writers — two
// drivel mounts, or drivel and a file manager — cannot claim one name.
package trash

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Dir resolves the home trash from the environment the process runs in:
// $XDG_DATA_HOME/Trash, or $HOME/.local/share/Trash when XDG_DATA_HOME is unset.
// The specification says a relative XDG_DATA_HOME is invalid, and so does this.
func Dir() (string, error) {
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		if !filepath.IsAbs(x) {
			return "", fmt.Errorf("XDG_DATA_HOME=%q is not an absolute path", x)
		}
		return filepath.Join(x, "Trash"), nil
	}
	home := os.Getenv("HOME")
	if home == "" || !filepath.IsAbs(home) {
		return "", errors.New("neither XDG_DATA_HOME nor an absolute HOME is set, so there is no home trash")
	}
	return filepath.Join(home, ".local", "share", "Trash"), nil
}

// Trash is one home trash directory.
type Trash struct {
	root string
}

// Open returns the trash at root, creating its files/ and info/ directories
// (mode 0700, as the specification asks) if they are missing.
func Open(root string) (*Trash, error) {
	for _, d := range []string{"files", "info"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o700); err != nil {
			return nil, fmt.Errorf("opening the trash: %w", err)
		}
	}
	return &Trash{root: root}, nil
}

// Root is the trash directory itself.
func (t *Trash) Root() string { return t.root }

// Entry is a reserved name in the trash: Path is where the file's content must
// end up, and the .trashinfo naming its origin already exists.
type Entry struct {
	Path string // files/NAME
	info string // info/NAME.trashinfo
}

// Abandon removes a reserved entry whose content never arrived, so the trash
// does not list a file it does not hold.
func (e Entry) Abandon() {
	_ = os.Remove(e.Path)
	_ = os.Remove(e.info)
}

// Reserve claims a name for a file that was at original (an absolute path) and
// writes its .trashinfo. The caller then puts the content at Entry.Path —
// Move does that for an existing file, and a download can write there itself.
func (t *Trash) Reserve(original string, when time.Time) (Entry, error) {
	if !filepath.IsAbs(original) {
		return Entry{}, fmt.Errorf("trash: %q is not an absolute path", original)
	}
	base := filepath.Base(original)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	for n := 1; n < 10000; n++ {
		name := base
		if n > 1 {
			name = stem + "." + strconv.Itoa(n) + ext
		}
		info := filepath.Join(t.root, "info", name+".trashinfo")
		f, err := os.OpenFile(info, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return Entry{}, fmt.Errorf("trash: %w", err)
		}
		_, werr := fmt.Fprintf(f, "[Trash Info]\nPath=%s\nDeletionDate=%s\n",
			escapePath(original), when.Local().Format("2006-01-02T15:04:05"))
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			_ = os.Remove(info)
			return Entry{}, fmt.Errorf("trash: writing %s: %w", info, werr)
		}
		e := Entry{Path: filepath.Join(t.root, "files", name), info: info}
		if _, err := os.Lstat(e.Path); err == nil {
			// A file with no .trashinfo: someone else's mess, never ours to replace.
			_ = os.Remove(info)
			continue
		}
		return e, nil
	}
	return Entry{}, fmt.Errorf("trash: no free name for %s", base)
}

// Move puts the file at src into the trash as having come from original,
// renaming when it can and copying when src is on another filesystem. It
// returns where the file now is.
func (t *Trash) Move(src, original string, when time.Time) (string, error) {
	e, err := t.Reserve(original, when)
	if err != nil {
		return "", err
	}
	if err := os.Rename(src, e.Path); err == nil {
		return e.Path, nil
	}
	if err := copyFile(src, e.Path); err != nil {
		e.Abandon()
		return "", err
	}
	if err := os.Remove(src); err != nil {
		return "", fmt.Errorf("trash: copied %s but could not remove it: %w", src, err)
	}
	return e.Path, nil
}

// copyFile copies src to dst, keeping the modification time, and syncs dst
// before reporting success: the caller removes src next.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dst)
		return err
	}
	_ = os.Chtimes(dst, fi.ModTime(), fi.ModTime())
	return nil
}

// escapePath percent-encodes a path for a .trashinfo, as RFC 2396 escapes a URL
// path: every byte that is not unreserved, with "/" left alone.
func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}
