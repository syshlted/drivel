// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package control

import (
	"github.com/syshlted/drivel/internal/syncengine"
)

// The wire types. They are this package's own rather than syncengine's on
// purpose: a field renamed inside the engine must not rename a field a
// third-party tool reads. docs/dev/control-api.md is the schema they implement,
// and a change here is a change there.

type versionBody struct {
	API                 int     `json:"api"`
	PID                 int     `json:"pid"`
	Started             string  `json:"started"`
	DrainTimeoutSeconds float64 `json:"drain_timeout_seconds"`
}

type mountsBody struct {
	Mounts []mountSummary `json:"mounts"`
}

type mountSummary struct {
	Name          string `json:"name"`
	Mountpoint    string `json:"mountpoint"`
	Provider      string `json:"provider"`
	Lazy          bool   `json:"lazy"`
	Reachability  string `json:"reachability"`
	Paused        bool   `json:"paused"`
	Saturated     bool   `json:"saturated"`
	PathsInMotion int    `json:"paths_in_motion"`
	Sweeping      bool   `json:"sweeping"`
}

type mountDetail struct {
	mountSummary
	At        string        `json:"at"`
	Outbound  outboundBody  `json:"outbound"`
	Inbound   inboundBody   `json:"inbound"`
	Hydration hydrationBody `json:"hydration"`
	Queue     queueBody     `json:"queue"`
	Reach     reachBody     `json:"reach"`
	Sweep     sweepBody     `json:"sweep"`
	Pause     pauseState    `json:"pause"`
}

type outboundBody struct {
	Uploads              int64 `json:"uploads"`
	UploadBytes          int64 `json:"upload_bytes"`
	RangePuts            int64 `json:"range_puts"`
	RangeBytes           int64 `json:"range_bytes"`
	UnchangedSkips       int64 `json:"unchanged_skips"`
	UnchangedBytes       int64 `json:"unchanged_bytes"`
	PlaceholdersDeclined int64 `json:"placeholders_declined"`
	Mkdirs               int64 `json:"mkdirs"`
	Deletes              int64 `json:"deletes"`
	Renames              int64 `json:"renames"`
	Succeeded            int64 `json:"succeeded"`
	Retries              int64 `json:"retries"`
	Abandoned            int64 `json:"abandoned"`
}

type inboundBody struct {
	Downloads            int64  `json:"downloads"`
	DownloadBytes        int64  `json:"download_bytes"`
	PlaceholdersCreated  int64  `json:"placeholders_created"`
	Restamps             int64  `json:"restamps"`
	RemoteDeletes        int64  `json:"remote_deletes"`
	RemoteMoves          int64  `json:"remote_moves"`
	Conflicts            int64  `json:"conflicts"`
	SuppressedOwnDeletes int64  `json:"suppressed_own_deletes"`
	Polls                int64  `json:"polls"`
	PollErrors           int64  `json:"poll_errors"`
	LastPoll             string `json:"last_poll,omitempty"`
	LastCursorAdvance    string `json:"last_cursor_advance,omitempty"`
}

type hydrationBody struct {
	Hydrations int64 `json:"hydrations"`
	Bytes      int64 `json:"bytes"`
	Failures   int64 `json:"failures"`
}

type queueBody struct {
	Queued             int64  `json:"queued"`
	InFlight           int64  `json:"in_flight"`
	Coalescing         int64  `json:"coalescing"`
	PathsInMotion      int    `json:"paths_in_motion"`
	Saturated          bool   `json:"saturated"`
	SaturatedSince     string `json:"saturated_since,omitempty"`
	BackPressureEvents int64  `json:"back_pressure_episodes"`
}

type reachBody struct {
	State               string `json:"state"`
	LastSuccess         string `json:"last_success,omitempty"`
	LastFailure         string `json:"last_failure,omitempty"`
	ConsecutiveFailures int    `json:"consecutive_failures"`
	LastError           string `json:"last_error,omitempty"`
}

type sweepBody struct {
	Running   bool         `json:"running"`
	Started   string       `json:"started,omitempty"`
	Completed int64        `json:"completed"`
	Last      *sweepReport `json:"last,omitempty"`
}

type sweepReport struct {
	Started           string `json:"started"`
	Finished          string `json:"finished"`
	Complete          bool   `json:"complete"`
	Listed            int    `json:"listed"`
	Reconciled        int    `json:"reconciled"`
	Pushed            int    `json:"pushed"`
	NotFetched        int    `json:"not_fetched"`
	ExportOnly        int    `json:"export_only"`
	DeletedLocally    int    `json:"deleted_locally"`
	DeletedRemotely   int    `json:"deleted_remotely"`
	Kept              int    `json:"kept"`
	Orphaned          int    `json:"orphaned"`
	Special           int    `json:"special"`
	Repaired          int    `json:"repaired"`
	Repushed          int    `json:"repushed"`
	Unverified        int    `json:"unverified"`
	Suppressed        int    `json:"suppressed"`
	TombstonesExpired int    `json:"tombstones_expired"`
}

type pauseState struct {
	Paused         bool   `json:"paused"`
	Since          string `json:"since,omitempty"`
	Until          string `json:"until,omitempty"`
	Held           int    `json:"held"`
	Cap            int    `json:"cap"`
	LastAutoResume string `json:"last_auto_resume,omitempty"`
}

type pathBody struct {
	Path        string   `json:"path"`
	Status      string   `json:"status"`
	Kind        string   `json:"kind,omitempty"`
	Size        int64    `json:"size"`
	Detail      string   `json:"detail,omitempty"`
	Attempt     int      `json:"attempt,omitempty"`
	NextAttempt string   `json:"next_attempt,omitempty"`
	Error       string   `json:"error,omitempty"`
	SyncedAt    string   `json:"synced_at,omitempty"`
	Conflicts   []string `json:"conflicts,omitempty"`
}

type childrenBody struct {
	Path      string     `json:"path"`
	Entries   []pathBody `json:"entries"`
	Truncated bool       `json:"truncated"`
	NextAfter string     `json:"next_after,omitempty"`
}

type pauseBody struct {
	Paused bool   `json:"paused"`
	Until  string `json:"until,omitempty"`
}

type shutdownBody struct {
	ShuttingDown        bool    `json:"shutting_down"`
	DrainTimeoutSeconds float64 `json:"drain_timeout_seconds"`
}

// streamLine is one line of /v1/events.
type streamLine struct {
	Type   string `json:"type"` // "status", "mount", "resync"
	Mount  string `json:"mount,omitempty"`
	Path   string `json:"path,omitempty"`
	Status string `json:"status,omitempty"`
	Reason string `json:"reason,omitempty"`
	At     string `json:"at"`
}

func summaryOf(m Mount, s syncengine.Stats) mountSummary {
	return mountSummary{
		Name: m.Name(), Mountpoint: m.Mountpoint(), Provider: m.Provider(), Lazy: m.Lazy(),
		Reachability: s.Reachability, Paused: s.Pause.Paused, Saturated: s.Saturated,
		PathsInMotion: s.PathsInMotion, Sweeping: s.Sweeping,
	}
}

func detailOf(m Mount, s syncengine.Stats) mountDetail {
	d := mountDetail{
		mountSummary: summaryOf(m, s),
		At:           ts(s.At),
		Outbound: outboundBody{
			Uploads: s.Uploads, UploadBytes: s.UploadBytes, RangePuts: s.RangePuts, RangeBytes: s.RangeBytes,
			UnchangedSkips: s.UnchangedSkips, UnchangedBytes: s.UnchangedBytes,
			PlaceholdersDeclined: s.PlaceholdersDeclined, Mkdirs: s.Mkdirs, Deletes: s.Deletes,
			Renames: s.Renames, Succeeded: s.Pushed, Retries: s.Retries, Abandoned: s.Abandoned,
		},
		Inbound: inboundBody{
			Downloads: s.Downloads, DownloadBytes: s.DownloadBytes,
			PlaceholdersCreated: s.PlaceholdersCreated, Restamps: s.Restamps,
			RemoteDeletes: s.RemoteDeletes, RemoteMoves: s.RemoteMoves, Conflicts: s.Conflicts,
			SuppressedOwnDeletes: s.SuppressedOwnDeletes, Polls: s.Polls, PollErrors: s.PollErrors,
			LastPoll: ts(s.LastPoll), LastCursorAdvance: ts(s.LastCursorAdvance),
		},
		Hydration: hydrationBody{Hydrations: s.Hydrations, Bytes: s.HydratedBytes, Failures: s.HydrationFailures},
		Queue: queueBody{
			Queued: s.Queued, InFlight: s.InFlight, Coalescing: s.Coalescing, PathsInMotion: s.PathsInMotion,
			Saturated: s.Saturated, SaturatedSince: ts(s.SaturatedSince), BackPressureEvents: s.BackPressureEvents,
		},
		Reach: reachBody{
			State: s.Reachability, LastSuccess: ts(s.LastSuccess), LastFailure: ts(s.LastFailure),
			ConsecutiveFailures: s.ConsecutiveFails, LastError: s.LastError,
		},
		Sweep: sweepBody{Running: s.Sweeping, Started: ts(s.SweepStarted), Completed: s.SweepsCompleted},
		Pause: pauseState{
			Paused: s.Pause.Paused, Since: ts(s.Pause.Since), Until: ts(s.Pause.Until),
			Held: s.Pause.Held, Cap: s.Pause.Cap, LastAutoResume: s.Pause.LastAutoResume,
		},
	}
	if !s.Sweeping {
		d.Sweep.Started = ""
	}
	if r := s.LastSweep; r != nil {
		d.Sweep.Last = &sweepReport{
			Started: ts(r.Started), Finished: ts(r.Finished), Complete: r.Complete,
			Listed: r.Listed, Reconciled: r.Reconciled, Pushed: r.Pushed, NotFetched: r.NotFetched,
			ExportOnly: r.ExportOnly, DeletedLocally: r.DeletedLocally, DeletedRemotely: r.DeletedRemotely,
			Kept: r.Kept, Orphaned: r.Orphaned, Special: r.Special, Repaired: r.Repaired,
			Repushed: r.Repushed, Unverified: r.Unverified, Suppressed: r.Suppressed,
			TombstonesExpired: r.TombstonesExpired,
		}
	}
	return d
}

func pathOf(p syncengine.PathStatus) pathBody {
	return pathBody{
		Path: p.Path, Status: p.Status, Kind: p.Kind, Size: p.Size, Detail: p.Detail,
		Attempt: p.Attempt, NextAttempt: ts(p.NextAttempt), Error: p.Error,
		SyncedAt: ts(p.SyncedAt), Conflicts: p.Conflicts,
	}
}
