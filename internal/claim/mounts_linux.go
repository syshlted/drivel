// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package claim

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// MountsUnder returns the root-relative slash paths of every drivel FUSE mount
// strictly below root, read from /proc/self/mountinfo.
//
// It is the third shape MC-47 found and the one a marker cannot catch: another
// drivel's *mountpoint* inside this backing tree. A walk descending into it reads
// and writes through a live FUSE mount, so one write there became three remote
// paths. Recognising a drivel mount specifically, by its fstype, is deliberately
// narrower than refusing to cross filesystems at all — whether a backing tree may
// span filesystems is the user's decision, and another drivel's mount is not.
//
// root may be /proc/self/fd/N (in-place mode); it is resolved to the directory it
// names, because mountinfo speaks in real paths.
func MountsUnder(root string) map[string]bool {
	resolved, err := resolve(root)
	if err != nil {
		return nil
	}
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil
	}
	defer f.Close()
	out := map[string]bool{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		mp, fstype, ok := parseMountinfo(sc.Text())
		if !ok || fstype != "fuse.drivel" {
			continue
		}
		rel, err := filepath.Rel(resolved, mp)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
			continue
		}
		out[filepath.ToSlash(rel)] = true
	}
	return out
}

func resolve(root string) (string, error) {
	if strings.HasPrefix(root, "/proc/self/fd/") {
		return os.Readlink(root)
	}
	return filepath.EvalSymlinks(root)
}

// parseMountinfo extracts the mount point and filesystem type from one line of
// /proc/self/mountinfo: "id parent maj:min root mountpoint opts [optional...] -
// fstype source superopts". The optional fields vary in number, so the type is
// found from the " - " separator rather than by position.
func parseMountinfo(line string) (mountpoint, fstype string, ok bool) {
	pre, post, found := strings.Cut(line, " - ")
	if !found {
		return "", "", false
	}
	f := strings.Fields(pre)
	g := strings.Fields(post)
	if len(f) < 5 || len(g) < 1 {
		return "", "", false
	}
	return unescapeOctal(f[4]), g[0], true
}

// unescapeOctal undoes mountinfo's \040-style escapes for space, tab, newline
// and backslash.
func unescapeOctal(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			if v, ok := octal(s[i+1 : i+4]); ok {
				b.WriteByte(v)
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func octal(s string) (byte, bool) {
	if len(s) != 3 {
		return 0, false
	}
	var v int
	for _, c := range s {
		if c < '0' || c > '7' {
			return 0, false
		}
		v = v*8 + int(c-'0')
	}
	if v > 255 {
		return 0, false
	}
	return byte(v), true
}
