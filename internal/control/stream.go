// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package control

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/syshlted/drivel/internal/syncengine"
)

// streamWriteTimeout is how long one write to a stream subscriber may take. A
// client that stops reading fills its socket buffer and then this; it is
// dropped, rather than holding a goroutine for as long as it stays connected.
const streamWriteTimeout = 10 * time.Second

// events streams status transitions as newline-delimited JSON, for one mount
// (?mount=NAME) or for all of them.
//
// The first line is always a resync marker: a client holds no picture yet, so
// the rule a client implements — "after a resync, re-read what you display" —
// covers connecting as well as falling behind. A client that falls behind is
// sent one final resync marker and the stream ends; it should reconnect.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	targets := s.mounts
	if name := r.URL.Query().Get("mount"); name != "" {
		m, ok := s.byName[name]
		if !ok {
			writeErr(w, http.StatusNotFound, "no_such_mount", fmt.Sprintf("no mount named %q", name))
			return
		}
		targets = []Mount{m}
	}
	rc := http.NewResponseController(w)
	sub := syncengine.NewSubscription(0)
	debounce := syncengine.DefaultPushDelay
	for _, m := range targets {
		mon := m.Monitor()
		mon.Subscribe(sub)
		if d := mon.Debounce(); d > debounce {
			debounce = d
		}
	}
	defer func() {
		for _, m := range targets {
			m.Monitor().Unsubscribe(sub)
		}
	}()

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	send := func(l streamLine) bool {
		_ = rc.SetWriteDeadline(time.Now().Add(streamWriteTimeout))
		if err := enc.Encode(l); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	if !send(streamLine{Type: "resync", Reason: "stream start: read the current state, then apply what follows", At: ts(time.Now())}) {
		return
	}

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sub.Ready():
		}
		// Coalesce over the uploader's own window, so a burst of transitions for
		// one path arrives as the one it settled on.
		t := time.NewTimer(debounce)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		items, resyncs, dropped := sub.Drain()
		if dropped {
			send(streamLine{Type: "resync", Reason: "this client fell behind and is being disconnected: reconnect and re-read", At: tsNow()})
			return
		}
		for _, x := range resyncs {
			if !send(streamLine{Type: "resync", Mount: x.Mount, Reason: x.Status, At: tsOf(x.At)}) {
				return
			}
		}
		for _, x := range items {
			l := streamLine{Type: "status", Mount: x.Mount, Path: x.Path, Status: x.Status, At: tsOf(x.At)}
			if x.Path == "" {
				l.Type = "mount"
			}
			if !send(l) {
				return
			}
		}
	}
}

func tsNow() string           { return ts(time.Now()) }
func tsOf(t time.Time) string { return ts(t) }

// metrics serves the Prometheus text format: a short adapter over the same
// snapshot, which is all it is allowed to be — the internal shape is not
// defined by what a scraper wants.
func (s *Server) metrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	snaps := make([]syncengine.Stats, len(s.mounts))
	for i, m := range s.mounts {
		snaps[i] = m.Stats()
	}
	type metric struct {
		name, kind, help string
		val              func(syncengine.Stats) float64
	}
	b := func(v bool) float64 {
		if v {
			return 1
		}
		return 0
	}
	unix := func(t time.Time) float64 {
		if t.IsZero() {
			return 0
		}
		return float64(t.UnixNano()) / 1e9
	}
	ms := []metric{
		{"drivel_uploads_total", "counter", "Whole-file uploads completed.", func(s syncengine.Stats) float64 { return float64(s.Uploads) }},
		{"drivel_upload_bytes_total", "counter", "Bytes sent by whole-file uploads.", func(s syncengine.Stats) float64 { return float64(s.UploadBytes) }},
		{"drivel_range_puts_total", "counter", "Pushes completed by writing only the changed extents (M6 gate 2).", func(s syncengine.Stats) float64 { return float64(s.RangePuts) }},
		{"drivel_range_bytes_total", "counter", "Bytes sent by range writes.", func(s syncengine.Stats) float64 { return float64(s.RangeBytes) }},
		{"drivel_unchanged_skips_total", "counter", "Pushes skipped because the remote already held the bytes (M6 gate 3).", func(s syncengine.Stats) float64 { return float64(s.UnchangedSkips) }},
		{"drivel_unchanged_bytes_total", "counter", "Bytes not uploaded thanks to the unchanged-content gate.", func(s syncengine.Stats) float64 { return float64(s.UnchangedBytes) }},
		{"drivel_placeholders_declined_total", "counter", "Pushes declined because the file was an unhydrated placeholder (M5).", func(s syncengine.Stats) float64 { return float64(s.PlaceholdersDeclined) }},
		{"drivel_push_succeeded_total", "counter", "Outbound operations that succeeded.", func(s syncengine.Stats) float64 { return float64(s.Pushed) }},
		{"drivel_push_retries_total", "counter", "Outbound attempts that failed and were retried.", func(s syncengine.Stats) float64 { return float64(s.Retries) }},
		{"drivel_push_abandoned_total", "counter", "Outbound operations the engine gave up on.", func(s syncengine.Stats) float64 { return float64(s.Abandoned) }},
		{"drivel_downloads_total", "counter", "Files downloaded into the backing tree.", func(s syncengine.Stats) float64 { return float64(s.Downloads) }},
		{"drivel_download_bytes_total", "counter", "Bytes downloaded.", func(s syncengine.Stats) float64 { return float64(s.DownloadBytes) }},
		{"drivel_hydrations_total", "counter", "Placeholders filled on first read (M5).", func(s syncengine.Stats) float64 { return float64(s.Hydrations) }},
		{"drivel_hydrated_bytes_total", "counter", "Bytes faulted in by hydration.", func(s syncengine.Stats) float64 { return float64(s.HydratedBytes) }},
		{"drivel_hydration_failures_total", "counter", "Hydrations that failed.", func(s syncengine.Stats) float64 { return float64(s.HydrationFailures) }},
		{"drivel_conflicts_total", "counter", "Conflict copies written.", func(s syncengine.Stats) float64 { return float64(s.Conflicts) }},
		{"drivel_polls_total", "counter", "Change-feed pages fetched.", func(s syncengine.Stats) float64 { return float64(s.Polls) }},
		{"drivel_poll_errors_total", "counter", "Change-feed requests that failed.", func(s syncengine.Stats) float64 { return float64(s.PollErrors) }},
		{"drivel_sweeps_completed_total", "counter", "Enumeration sweeps completed.", func(s syncengine.Stats) float64 { return float64(s.SweepsCompleted) }},
		{"drivel_queued", "gauge", "Operations dispatched and not yet started (including any held by a pause).", func(s syncengine.Stats) float64 { return float64(s.Queued) }},
		{"drivel_in_flight", "gauge", "Operations executing now.", func(s syncengine.Stats) float64 { return float64(s.InFlight) }},
		{"drivel_coalescing", "gauge", "Paths waiting out the push delay.", func(s syncengine.Stats) float64 { return float64(s.Coalescing) }},
		{"drivel_saturated", "gauge", "1 while writes through the mount are waiting on uploads.", func(s syncengine.Stats) float64 { return b(s.Saturated) }},
		{"drivel_paused", "gauge", "1 while the mount is paused.", func(s syncengine.Stats) float64 { return b(s.Pause.Paused) }},
		{"drivel_last_poll_timestamp_seconds", "gauge", "When the change feed last answered; 0 if never.", func(s syncengine.Stats) float64 { return unix(s.LastPoll) }},
		{"drivel_last_success_timestamp_seconds", "gauge", "When a provider call last succeeded; 0 if never.", func(s syncengine.Stats) float64 { return unix(s.LastSuccess) }},
	}
	for _, mt := range ms {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", mt.name, mt.help, mt.name, mt.kind)
		for i, m := range s.mounts {
			fmt.Fprintf(w, "%s{mount=%q} %g\n", mt.name, m.Name(), mt.val(snaps[i]))
		}
	}
	writeReach(w, s.mounts, snaps)
}

// writeReach renders reachability one-hot, the Prometheus idiom for an enum.
func writeReach(w io.Writer, mounts []Mount, snaps []syncengine.Stats) {
	fmt.Fprint(w, "# HELP drivel_reachability 1 for the backend's observed state: online, offline or unknown.\n# TYPE drivel_reachability gauge\n")
	for i, m := range mounts {
		for _, st := range []string{syncengine.ReachOnline, syncengine.ReachOffline, syncengine.ReachUnknown} {
			v := 0
			if snaps[i].Reachability == st {
				v = 1
			}
			fmt.Fprintf(w, "drivel_reachability{mount=%q,state=%q} %d\n", m.Name(), st, v)
		}
	}
}
