// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package syncengine

import (
	"context"
	"time"

	"github.com/syshlted/drivel/provider"
)

// refetchDelay is how long after a failed inbound apply the path is retried,
// and maxRefetchBackoff how many times that may double for a retry that fails
// again (to 64 minutes).
//
// Shorter than repairDelay by design, because what it buys is different. A
// repair is a sweep — the most expensive and most dangerous thing drivel does —
// so M27 waits five minutes to collapse an outage into one. A refetch is one
// Stat and one download of a path the feed already named, so there is nothing
// to amortise and the only reason to wait at all is to let a transient failure
// (the outage of seconds MC-46 was built on) clear before spending a request on
// it. The backoff is for the failure that does not clear: a full disk fails
// every retry, and an hourly attempt is what it should cost.
//
// A constant and not a flag, for repairDelay's reason: it describes how long a
// failure takes to clear, not a preference.
const refetchDelay = time.Minute

const maxRefetchBackoff = 6

// oweRefetch records that a remote change could not be applied, so a later pass
// fetches the path again, and arms that pass.
//
// It exists because the pull loop cannot hold its cursor for one change: a
// change that can never apply would then stop inbound sync for every other path
// for good. So the cursor advances, and before this the object was simply never
// offered again — repaired only by the next sweep, which is a day away on
// Drive's default and never at -sweep-interval 0 (MC-46).
//
// A removal is declined, for the reason M27 declines an abandoned deletion.
// Re-applying one later means deleting a local path on the strength of a report
// that may be hours old, and in the meantime the user may have written new work
// there that is not yet pushed. A removal that failed has left the local copy
// in place, which is the state every delete guard in this program prefers; the
// sweep's delete pass is the thing with the evidence to finish it.
func (d *Downloader) oweRefetch(ch provider.RemoteChange) {
	if d.state == nil || ch.Removed || ch.File == nil || ch.Path == "" {
		return
	}
	d.mon.count(cApplyFailures, 1)
	if err := d.state.OweRefetch(ch.Path, time.Now()); err != nil {
		d.logf("[pull] recording %s for retry: %v", ch.Path, err)
		return
	}
	d.armRefetch()
}

// resumeRefetches arms a retry pass at once for records left by an earlier run.
// That debt predates this process, so there is nothing to wait out — the delay
// exists to let a live failure clear, and this one has had a restart's worth of
// time.
//
// Called once the initial sweep is over, so a pass already armed means that
// sweep recorded a failure of its own, which is this run's and waits as usual.
func (d *Downloader) resumeRefetches() {
	if d.state == nil || !d.refetchDue.IsZero() {
		return
	}
	owed, err := d.state.RefetchesOwed()
	if err != nil {
		d.logf("[pull] reading the refetch list: %v", err)
		return
	}
	if owed {
		d.logf("[pull] a remote change failed to apply before this mount last stopped: retrying it")
		d.refetchDue = time.Now()
	}
}

// armRefetch schedules the next retry pass, unless one is already scheduled —
// so a second failure joins the pass already due instead of pushing it out.
// Announced when armed, because a minute of silence reads as nothing happening.
func (d *Downloader) armRefetch() {
	if !d.refetchDue.IsZero() {
		return
	}
	base := d.refetchBase
	if base <= 0 {
		base = refetchDelay // a Downloader built as a literal: never "now"
	}
	wait := base << min(d.refetchFails, maxRefetchBackoff)
	d.refetchDue = time.Now().Add(wait)
	d.logf("[pull] a remote change could not be applied: retrying in %s", wait)
}

// dueRefetch runs the retry pass if it has fallen due. It runs on the pull
// loop's goroutine, between polls, so it can never interleave with an apply
// from the feed — the same single-writer property the rest of the downloader
// relies on for the backing tree.
func (d *Downloader) dueRefetch(ctx context.Context) {
	if d.refetchDue.IsZero() || time.Now().Before(d.refetchDue) {
		return
	}
	d.refetchDue = time.Time{}
	left, ok := d.refetchPass(ctx)
	if !ok {
		return // cancelled, or the list was unreadable; nothing to reschedule on
	}
	if left == 0 {
		d.refetchFails = 0
		return
	}
	d.refetchFails++
	d.armRefetch()
}

// refetchPass retries every owed path and reports how many are still owed.
//
// Each retry asks the provider what is at the path NOW and applies that, through
// the same apply the feed uses — so §4's echo test, the tombstone, the
// placeholder branch and §6's conflict copy all hold exactly as they would have
// for the original change. Asking afresh, rather than replaying the change that
// failed, is the point: by the time the retry runs the remote may have moved
// again, and applying a stale description of it is the one thing worse than
// applying nothing.
//
// A path the provider no longer has is dropped and nothing local is touched.
// The removal is the feed's to report (and if it already has, ForgetMany has
// discharged the record before this ever saw it); see oweRefetch for why a
// retry never deletes.
func (d *Downloader) refetchPass(ctx context.Context) (left int, ok bool) {
	var owed []string
	if err := d.state.EachRefetch(func(p string, _ time.Time) error {
		owed = append(owed, p)
		return nil
	}); err != nil {
		d.logf("[pull] reading the refetch list: %v", err)
		return 0, false
	}
	for i, rel := range owed {
		if ctx.Err() != nil {
			return 0, false
		}
		rf, exists, err := d.store.Stat(ctx, rel)
		d.mon.observe(err)
		if err != nil {
			if ctx.Err() != nil {
				return 0, false
			}
			d.logf("[pull] refetch %s: %v", rel, err)
			left++
			if !provider.IsRetryable(err) {
				continue
			}
			// A retryable Stat failure is the backend being unavailable, which
			// every remaining path would meet too. Leave them for the next pass
			// rather than spending a request each on finding that out.
			left += len(owed) - i - 1
			return left, true
		}
		if !exists {
			d.logf("[pull] refetch %s: no longer exists remotely; nothing to fetch", rel)
			if err := d.state.ClearRefetch(rel); err != nil {
				d.logf("[pull] clearing the refetch record for %s: %v", rel, err)
			}
			continue
		}
		rf.Path = rel
		d.logf("[pull] refetch %s (an earlier apply failed)", rel)
		if err := d.apply(ctx, provider.RemoteChange{Path: rel, File: &rf}); err != nil {
			if ctx.Err() != nil {
				return 0, false
			}
			d.logf("[pull] apply %s: %v", rel, err)
			left++
			continue
		}
		// Usually free: an apply that wrote anything has already dropped the
		// record inside SetEcho. What is left is the apply that found nothing to
		// do — the remote is back to what we hold, or a sweep got there first.
		if err := d.state.ClearRefetch(rel); err != nil {
			d.logf("[pull] clearing the refetch record for %s: %v", rel, err)
		}
		d.mon.count(cRefetched, 1)
	}
	return left, true
}
