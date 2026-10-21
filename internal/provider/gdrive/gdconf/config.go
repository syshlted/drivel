// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

// Package gdconf holds the Google Drive backend's configuration vocabulary: the
// settings table a config file writes, and the two enumerated values that table
// can carry.
//
// It is a leaf with no dependencies, and it is split out of gdrive for one
// reason: since M9 the Drive backend runs in a separate process, so the drivel
// binary no longer imports it — but the drivel binary still owns the `-drive-*`
// flags, still has to build a settings table from them, and still generates the
// shell completions for their enumerated values. Rule 3 of the completions
// design says those words must be the program's own constants, so that a renamed
// mode cannot leave the completions offering something the backend refuses. This
// package is what lets both sides keep one definition without the host linking
// the Drive SDK.
//
// Nothing here talks to Drive, and nothing here should start. A value that needs
// the API to check it is checked in gdrive, at open.
package gdconf

// Config is what a Drive provider needs to open.
//
// The struct tags are what a `[mount.provider]` table in drivel's config file
// decodes into (M8), and what provider.EncodeConfig renders on the flag path.
// They exist because this IS the provider's configuration — the same reason a
// type carries json tags — and naming the keys explicitly beats the decoder's
// default case-insensitive field matching, which would spell RootID as "rootid".
type Config struct {
	// Credentials is the desktop OAuth client secret JSON.
	Credentials string `toml:"credentials"`
	// Token caches the user token across runs (written by `drivel login`).
	Token string `toml:"token"`
	// RootID is the Drive folder ID mapped to the mount root ("" or "root" => My
	// Drive root).
	RootID string `toml:"root"`
	// IndexPath is where the persistent path↔fileID index lives (M7). Empty
	// disables persistence, which costs API round trips and nothing else. It must
	// not sit inside the backing tree, or it would sync itself to Drive.
	IndexPath string `toml:"index"`
	// SweepMode selects how the M7b/M7c enumeration sweep walks the tree:
	// "flat" lists the whole account, "scoped" descends from the mount root, and
	// "" or "auto" picks by whether RootID names a concrete folder. See
	// gdrive/enumerate_scoped.go for why neither is right for every shape of
	// Drive.
	SweepMode SweepMode `toml:"sweep-mode"`
	// Delete selects what a removal does to the remote object: "trash" (or "",
	// the default) moves it to the Drive trash, "permanent" unlinks it outright.
	// See DeleteMode — the default is the recoverable one because not every
	// deletion drivel performs was asked for by a user.
	Delete DeleteMode `toml:"delete"`
	// OverwriteCheck selects which rewrites look, once they have finished, for
	// an edit somebody else made while they were uploading: "large" (or "", the
	// default) checks rewrites of OverwriteCheckMinSize or more, "always" every
	// rewrite of a synced file, "off" none. See OverwriteMode.
	OverwriteCheck OverwriteMode `toml:"overwrite-check"`
	// Scope is the OAuth scope the token was granted, as `drivel login` recorded
	// it. Empty means gauth.ScopeDrive.
	//
	// It matters little to a refresh — the refresh token carries the scopes the
	// user actually consented to, whatever we ask for — but asking for a scope the
	// user declined is a lie in the one place a reader would go to find out what
	// this mount can do. A read-only account should say so.
	Scope string `toml:"scope"`
}

// SweepMode selects how Enumerate walks the tree.
type SweepMode string

const (
	// SweepAuto descends when RootID names a concrete folder and lists the account
	// when it names the whole Drive. It is the zero value's meaning too.
	SweepAuto SweepMode = "auto"
	// SweepFlat always lists the account (M7b behaviour).
	SweepFlat SweepMode = "flat"
	// SweepScoped always descends from the mount root.
	SweepScoped SweepMode = "scoped"
)

// SweepModes lists every accepted value, in the order a menu should offer them.
// The shell completions are generated from it, so a mode added here reaches them
// without a second edit.
var SweepModes = []SweepMode{SweepAuto, SweepFlat, SweepScoped}

// Valid reports whether m is a mode the backend will accept. "" is valid and
// means SweepAuto.
func (m SweepMode) Valid() bool {
	switch m {
	case "", SweepAuto, SweepFlat, SweepScoped:
		return true
	}
	return false
}

// DeleteMode selects what Remove does to an object remotely.
//
// The distinction exists because a deletion drivel performs is not always one a
// user asked for. Every other guard in the tree fails towards keeping bytes —
// M5 refuses to push a placeholder, M6 falls back to a whole-file Put, M7b
// abandons a delete pass it cannot justify — and this one operation used to be
// the exception: a mount pointed at the wrong -drive-root, or an inference
// from a baseline that turned out to be stale, destroyed the remote copy with no
// undo anywhere. The trash is Drive's own answer to that, so the default is to
// use it and the old behaviour is what has to be asked for.
type DeleteMode string

const (
	// DeleteTrash moves the object to the Drive trash, where the web UI can
	// restore it (for 30 days, after which Drive purges it). The default, and
	// what "" means.
	DeleteTrash DeleteMode = "trash"
	// DeletePermanent unlinks the object outright, which is what every release
	// through M16 did. There is no undo, and the bytes are not recoverable from
	// anywhere.
	// Worth choosing when a mount is how storage gets reclaimed: trashed objects
	// still count against the account's quota until the trash is emptied.
	DeletePermanent DeleteMode = "permanent"
)

// DeleteModes lists every accepted value, in the order a menu should offer them
// — the recoverable one first, since it is the default.
var DeleteModes = []DeleteMode{DeleteTrash, DeletePermanent}

// Valid reports whether m is a mode the backend will accept. "" is valid and
// means DeleteTrash.
func (m DeleteMode) Valid() bool {
	return m == "" || m == DeleteTrash || m == DeletePermanent
}

// OverwriteMode selects which rewrites of a file look afterwards for an edit
// they replaced.
//
// Drive cannot make an upload conditional on what it replaces, so an edit that
// reaches a file between the engine's last look and the end of a rewrite is
// overwritten unseen. A checked rewrite lists the file's revisions before it
// starts and after it ends, and any revision that appeared in between and is
// not its own is somebody's edit, which the engine keeps as a conflict copy.
// Each check costs two requests. What an unchecked rewrite leaves open is the
// time from the engine's look to the end of the upload: about two round trips
// for a small file, the whole transfer for a large one.
type OverwriteMode string

const (
	// OverwriteLarge checks rewrites of OverwriteCheckMinSize or more, where the
	// transfer is long enough for an edit to land during it. The default, and
	// what "" means.
	OverwriteLarge OverwriteMode = "large"
	// OverwriteAlways checks every rewrite of a file this mount has synced
	// before, for a folder where several people edit the same small files.
	OverwriteAlways OverwriteMode = "always"
	// OverwriteOff checks nothing. An edit that lands during an upload is
	// replaced with no conflict copy, and survives only in the file's Drive
	// revision history, for as long as Drive keeps it (30 days).
	OverwriteOff OverwriteMode = "off"
)

// OverwriteModes is every OverwriteMode, in the order the docs list them.
var OverwriteModes = []OverwriteMode{OverwriteLarge, OverwriteAlways, OverwriteOff}

// OverwriteCheckMinSize is the size from which OverwriteLarge checks a rewrite.
// Below it the two listings cost about what the upload does, to narrow a window
// that is already about one upload's round trip; the engine draws the same line
// for the same reason (its unchanged-content gate).
const OverwriteCheckMinSize = 4 << 20

// Valid reports whether m is a mode the backend will accept. "" is valid and
// means OverwriteLarge.
func (m OverwriteMode) Valid() bool {
	return m == "" || m == OverwriteLarge || m == OverwriteAlways || m == OverwriteOff
}
