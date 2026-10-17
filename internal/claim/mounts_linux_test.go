// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package claim

import "testing"

func TestParseMountinfo(t *testing.T) {
	cases := []struct {
		line, mp, fstype string
	}{
		{`36 35 98:0 /mnt1 /mnt/parent rw,noatime master:1 - ext3 /dev/root rw,errors=continue`, "/mnt/parent", "ext3"},
		{`700 29 0:63 / /home/j/My\040Drive rw,nosuid,nodev - fuse.drivel work rw,user_id=1000`, "/home/j/My Drive", "fuse.drivel"},
		{`700 29 0:63 / /x rw shared:5 master:2 - fuse.drivel drivel rw`, "/x", "fuse.drivel"},
	}
	for _, c := range cases {
		mp, fstype, ok := parseMountinfo(c.line)
		if !ok || mp != c.mp || fstype != c.fstype {
			t.Errorf("parseMountinfo(%q) = %q, %q, %v; want %q, %q", c.line, mp, fstype, ok, c.mp, c.fstype)
		}
	}
	if _, _, ok := parseMountinfo("garbage"); ok {
		t.Error("parseMountinfo accepted a line with no separator")
	}
	if got := unescapeOctal(`a\134b\011c\0`); got != "a\\b\tc\\0" {
		t.Errorf("unescapeOctal = %q", got)
	}
}
