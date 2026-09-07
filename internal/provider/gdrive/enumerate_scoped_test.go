package gdrive

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"google.golang.org/api/drive/v3"

	"github.com/zishmusic/drivel/internal/provider"
)

// mountSubfolder points a Drive at one folder rather than at My Drive, which is
// the configuration M7c exists for and the one every test here uses.
func mountSubfolder(d *Drive, id string) {
	d.root = id
	d.rootID = ""
	d.idByPath = map[string]string{"": id}
	d.pathByID = map[string]string{id: ""}
	d.kids = map[string]map[string]struct{}{}
}

// The descent's whole claim: a mount of one folder pays for that folder, not for
// the account around it. Asserted as a request count against the flat sweep over
// the identical fixture, because "scoped is cheaper" is the milestone.
func TestScopedSweepCostsTheSubtreeNotTheAccount(t *testing.T) {
	files := []*drive.File{
		folder("id-mine", "mine", fakeRootID),
		file("id-in", "in.txt", "id-mine"),
		folder("id-other", "other", fakeRootID),
	}
	// A big neighbour: everything the mount does not want, which the flat sweep
	// pages through anyway and the descent never asks about.
	for i := range 20 {
		files = append(files, file(fmt.Sprintf("id-out-%02d", i), fmt.Sprintf("out%02d.txt", i), "id-other"))
	}

	scoped, fake := newFakeDrive(t, files...)
	mountSubfolder(scoped, "id-mine")
	scoped.sweepMode = SweepScoped
	got, _ := sweep(t, scoped)
	_, scopedLists, _, _ := fake.counts()

	if len(got) != 1 {
		t.Fatalf("descent emitted %v; want only in.txt", keys(got))
	}
	if _, ok := got["in.txt"]; !ok {
		t.Fatalf("the one file under the mount root was not emitted: %v", keys(got))
	}

	flat := fake.client(t)
	mountSubfolder(flat, "id-mine")
	flat.sweepMode = SweepFlat
	if _, _ = sweep(t, flat); true {
		_, total, _, _ := fake.counts()
		flatLists := total - scopedLists
		if scopedLists >= flatLists {
			t.Fatalf("descent cost %d listing(s) and the flat sweep %d; the descent is supposed to be the cheap one",
				scopedLists, flatLists)
		}
		t.Logf("descent %d listing(s), flat %d, for one file in a 23-object account", scopedLists, flatLists)
	}
}

// Every path in the subtree, once, with the directory flag intact — the same
// contract the flat sweep has, reached without any parking.
func TestScopedSweepEmitsTheWholeSubtree(t *testing.T) {
	d, _ := newFakeDrive(t,
		folder("id-mine", "mine", fakeRootID),
		file("id-a", "a.txt", "id-mine"),
		folder("id-sub", "sub", "id-mine"),
		file("id-b", "b.txt", "id-sub"),
		folder("id-deep", "deep", "id-sub"),
		file("id-c", "c.txt", "id-deep"),
	)
	mountSubfolder(d, "id-mine")

	got, _ := sweep(t, d)
	want := []string{"a.txt", "sub", "sub/b.txt", "sub/deep", "sub/deep/c.txt"}
	have := keys(got)
	sort.Strings(have)
	if fmt.Sprint(have) != fmt.Sprint(want) {
		t.Fatalf("descent emitted %v; want %v", have, want)
	}
	if !got["sub"].IsDir || !got["sub/deep"].IsDir || got["sub/deep/c.txt"].IsDir {
		t.Fatalf("directory flags wrong: %+v", got)
	}
}

// A folder holding more children than one page is put back on the frontier
// rather than drained inside one call, so no single Enumerate can be unbounded.
func TestScopedSweepPagesOneLargeFolder(t *testing.T) {
	files := []*drive.File{folder("id-mine", "mine", fakeRootID)}
	var want []string
	for i := range fakeChildPage*3 + 1 {
		files = append(files, file(fmt.Sprintf("id-f%02d", i), fmt.Sprintf("f%02d.txt", i), "id-mine"))
		want = append(want, fmt.Sprintf("f%02d.txt", i))
	}
	d, _ := newFakeDrive(t, files...)
	mountSubfolder(d, "id-mine")

	got, calls := sweep(t, d)
	have := keys(got)
	sort.Strings(have)
	sort.Strings(want)
	if fmt.Sprint(have) != fmt.Sprint(want) {
		t.Fatalf("descent emitted %v; want %v", have, want)
	}
	if calls < 2 {
		t.Fatalf("a folder of %d children finished in %d call(s); the page boundary was never crossed",
			len(want), calls)
	}
}

// A page token that ages out mid-descent cannot be renewed, so the folder is
// re-listed from the start. The sweep must still finish and still report every
// path — the repeat is a cost, not a correctness problem.
func TestScopedSweepSurvivesAnExpiredFolderPageToken(t *testing.T) {
	files := []*drive.File{folder("id-mine", "mine", fakeRootID)}
	var want []string
	for i := range fakeChildPage * 3 {
		files = append(files, file(fmt.Sprintf("id-f%02d", i), fmt.Sprintf("f%02d.txt", i), "id-mine"))
		want = append(want, fmt.Sprintf("f%02d.txt", i))
	}
	d, fake := newFakeDrive(t, files...)
	mountSubfolder(d, "id-mine")
	fake.expirePageTokens = true

	got := map[string]bool{}
	cursor := ""
	for range 50 {
		out, next, err := d.Enumerate(ctx, cursor)
		if err != nil {
			t.Fatalf("enumerate: %v", err)
		}
		for _, f := range out {
			got[f.Path] = true
		}
		if next == "" {
			break
		}
		cursor = next
	}
	for _, w := range want {
		if !got[w] {
			t.Fatalf("%q went missing across the token expiry; got %d of %d paths", w, len(got), len(want))
		}
	}
}

// auto is the only mode most mounts will ever set, so what it decides is part of
// the contract: descend a folder, list the account.
func TestSweepModeAutoPicksByRoot(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mode       SweepMode
		root       string
		wantScoped bool
	}{
		{"auto, a concrete folder", SweepAuto, "id-mine", true},
		{"auto, the root alias", SweepAuto, "root", false},
		{"auto, the whole drive", SweepAuto, "", false},
		{"the zero value behaves as auto", "", "id-mine", true},
		{"flat overrides a concrete folder", SweepFlat, "id-mine", false},
		{"scoped overrides the root alias", SweepScoped, "root", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &Drive{root: tc.root, sweepMode: tc.mode}
			if got := d.scopedSweep(); got != tc.wantScoped {
				t.Fatalf("scopedSweep() = %v; want %v", got, tc.wantScoped)
			}
		})
	}
}

// An unreadable mode is refused at open rather than silently sweeping the wrong
// way, which is M8 rule 6 applied to a value instead of a key.
func TestOpenRejectsAnUnknownSweepMode(t *testing.T) {
	_, err := Open(ctx, Config{SweepMode: "sideways"})
	if err == nil {
		t.Fatal("an unknown sweep-mode opened without complaint")
	}
	for _, want := range []string{"sideways", "auto", "flat", "scoped"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q, so it cannot be acted on", err, want)
		}
	}
}

// The wall-clock half of M7c's claim, which the request-count tests above cannot
// reach: the descent's listings are issued concurrently, so a sweep costs about
// one round trip per *batch* of folders rather than one per folder.
//
// It is measured as a ratio between two runs of the same fixture at two fanouts,
// not against an absolute time, because the number that matters is what the
// concurrency buys and because a wall-clock threshold on a shared runner is a
// flake waiting to happen. The serial run is what makes it load-bearing: without
// it a fake that ignored childDelay entirely would pass.
func TestScopedSweepListsFoldersConcurrently(t *testing.T) {
	const (
		folders = 32
		delay   = 30 * time.Millisecond
	)
	build := func() []*drive.File {
		out := []*drive.File{folder("id-mine", "mine", fakeRootID)}
		for i := range folders {
			out = append(out, folder(fmt.Sprintf("id-d%02d", i), fmt.Sprintf("d%02d", i), "id-mine"))
		}
		return out
	}

	run := func(t *testing.T, fanout int) time.Duration {
		t.Helper()
		d, fake := newFakeDrive(t, build()...)
		mountSubfolder(d, "id-mine")
		d.fanout = fanout
		// One page per folder, so the only sequencing left is the descent's own.
		fake.childPage = 1000
		fake.childDelay = delay

		// Not the shared sweep helper: at fanout 1 this fixture needs one call per
		// folder, well past that helper's 20-call termination guard, which the
		// other tests want kept tight.
		start := time.Now()
		seen, cursor := 0, ""
		for range folders * 2 {
			out, next, err := d.Enumerate(ctx, cursor)
			if err != nil {
				t.Fatalf("enumerate at fanout %d: %v", fanout, err)
			}
			seen += len(out)
			if next == "" {
				break
			}
			cursor = next
		}
		elapsed := time.Since(start)

		if seen != folders {
			t.Fatalf("fanout %d emitted %d path(s); want %d", fanout, seen, folders)
		}
		return elapsed
	}

	serial := run(t, 1)
	concurrent := run(t, enumFanout)

	// The delay has to be real, or both runs are measuring nothing. Listing the
	// root plus every folder under it is folders+1 requests, so even at full
	// concurrency the descent cannot finish faster than one delay.
	if concurrent < delay {
		t.Fatalf("the whole sweep took %v, under one %v round trip: childDelay is not being applied",
			concurrent, delay)
	}
	// A generous bound: at fanout 8 over 32 folders the ideal is ~5x, and anything
	// that has genuinely stopped fanning out lands at 1x.
	if serial < 2*concurrent {
		t.Fatalf("serial %v vs concurrent %v: the listings are not being issued concurrently",
			serial, concurrent)
	}
	t.Logf("fanout 1: %v, fanout %d: %v (%.1fx) over %d folders at %v each",
		serial, enumFanout, concurrent, float64(serial)/float64(concurrent), folders, delay)
}

// What a fanned-out descent does when Drive throttles it. The answer has to be
// "finishes anyway, with everything", because a sweep that quietly drops a
// subtree is read by reconcile as a remote deletion.
//
// The caller modelled here is Downloader.start, which retries a retryable error
// and gives up on anything else — so the classification is half the test: a 403
// that did not come back retryable would strand inbound sync instead.
func TestScopedSweepFinishesThroughRateLimiting(t *testing.T) {
	const folders = 24
	files := []*drive.File{folder("id-mine", "mine", fakeRootID)}
	want := map[string]bool{}
	for i := range folders {
		name := fmt.Sprintf("d%02d", i)
		files = append(files, folder(fmt.Sprintf("id-%s", name), name, "id-mine"))
		want[name] = true
	}
	d, fake := newFakeDrive(t, files...)
	mountSubfolder(d, "id-mine")
	fake.childPage = 1000
	// Enough 403s to hit several whole batches, including the first.
	fake.childRateLimit = 12

	got := map[string]bool{}
	throttled, cursor := 0, ""
	for range 200 {
		out, next, err := d.Enumerate(ctx, cursor)
		if err != nil {
			if !provider.IsRetryable(err) {
				t.Fatalf("a Drive rate limit came back unretryable, which would strand inbound sync: %v", err)
			}
			throttled++
			continue // Downloader.start's behaviour, minus the backoff sleep
		}
		for _, f := range out {
			got[f.Path] = true
		}
		if next == "" {
			break
		}
		cursor = next
	}

	if throttled == 0 {
		t.Fatal("no call was throttled; the injection is not reaching the descent")
	}
	for name := range want {
		if !got[name] {
			t.Fatalf("%q was lost across %d throttled call(s); got %d of %d",
				name, throttled, len(got), len(want))
		}
	}
	_, lists, _, _ := fake.counts()
	t.Logf("%d folder(s) enumerated through %d throttled call(s), %d listing(s) issued",
		len(got), throttled, lists)
}

// A throttle that lands on part of a fanned-out batch. The descent still has to
// finish with everything, and the cost of getting there is the number worth
// recording: a batch is put back whole, so one 403 discards up to enumFanout-1
// listings that had already succeeded.
func TestScopedSweepThroughPartialBatchThrottling(t *testing.T) {
	const folders = 40
	files := []*drive.File{folder("id-mine", "mine", fakeRootID)}
	want := map[string]bool{}
	for i := range folders {
		name := fmt.Sprintf("d%02d", i)
		files = append(files, folder(fmt.Sprintf("id-%s", name), name, "id-mine"))
		want[name] = true
	}
	d, fake := newFakeDrive(t, files...)
	mountSubfolder(d, "id-mine")
	fake.childPage = 1000
	fake.childFailEvery = 5 // one in five throttled, so most batches lose one

	got := map[string]bool{}
	throttled, cursor := 0, ""
	for range 500 {
		out, next, err := d.Enumerate(ctx, cursor)
		if err != nil {
			if !provider.IsRetryable(err) {
				t.Fatalf("a Drive rate limit came back unretryable: %v", err)
			}
			throttled++
			continue
		}
		for _, f := range out {
			got[f.Path] = true
		}
		if next == "" {
			break
		}
		cursor = next
	}
	for name := range want {
		if !got[name] {
			t.Fatalf("%q lost across %d throttled call(s): got %d of %d", name, throttled, len(got), len(want))
		}
	}
	_, lists, _, _ := fake.counts()
	ideal := folders + 1 // the root, plus one listing per folder under it
	t.Logf("%d folder(s) through %d throttled call(s): %d listings issued against an ideal of %d (%.1fx)",
		len(got), throttled, lists, ideal, float64(lists)/float64(ideal))

	// The injection has to be doing something, or the bound below is vacuous.
	if lists <= ideal {
		t.Fatalf("%d listing(s) against an ideal of %d: nothing was retried, so nothing was throttled",
			lists, ideal)
	}
	// The bound that matters. One in five listings is refused, so a descent that
	// keeps what succeeded should spend a little over 1x. Putting the whole batch
	// back instead measured 97x here, because at fanout 8 a 20% failure rate
	// spoils most batches and re-issuing all eight feeds the throttle it is
	// reacting to.
	if lists > 2*ideal {
		t.Fatalf("%d listing(s) against an ideal of %d (%.1fx): a throttled batch is being re-issued whole",
			lists, ideal, float64(lists)/float64(ideal))
	}
}

// The descent moves its own concurrency toward what Drive tolerates: it halves on
// a throttled listing and grows back on a clean one. Measured as the peak number
// of listings actually in the server at once, because request counts cannot see
// it — keeping partial results already bounds those, and would leave a descent
// firing eight at a time at a provider that is saying no.
func TestScopedSweepBacksOffConcurrencyWhenThrottled(t *testing.T) {
	const folders = 48
	build := func() []*drive.File {
		out := []*drive.File{folder("id-mine", "mine", fakeRootID)}
		for i := range folders {
			out = append(out, folder(fmt.Sprintf("id-d%02d", i), fmt.Sprintf("d%02d", i), "id-mine"))
		}
		return out
	}
	run := func(t *testing.T, failEvery int) int32 {
		t.Helper()
		d, fake := newFakeDrive(t, build()...)
		mountSubfolder(d, "id-mine")
		fake.childPage = 1000
		fake.childDelay = 5 * time.Millisecond // so concurrent calls actually overlap
		fake.childFailEvery = failEvery

		// Steady state, not the whole run: the first fanned-out batch necessarily
		// goes out at the ceiling, because the descent has not been told "no" yet.
		// Measuring across that would report 8 however well the backoff works.
		const settle = 4
		cursor := ""
		for call := range 400 {
			if call == settle {
				fake.resetChildPeak()
			}
			_, next, err := d.Enumerate(ctx, cursor)
			if err != nil {
				continue
			}
			if next == "" {
				break
			}
			cursor = next
		}
		return fake.childPeak.Load()
	}

	clean := run(t, 0)
	throttled := run(t, 3)

	if clean < enumFanout {
		t.Fatalf("an unthrottled descent peaked at %d concurrent listing(s); the ceiling is %d",
			clean, enumFanout)
	}
	if throttled >= clean {
		t.Fatalf("a throttled descent peaked at %d against an unthrottled %d: it is not backing off",
			throttled, clean)
	}
	t.Logf("peak concurrency: %d unthrottled, %d when one listing in three is refused", clean, throttled)
}

// Backing off is only half of it. A descent that halves on a throttle and never
// grows back would let one transient 403 near the start pin the rest of the sweep
// at one listing at a time — which on a large tree costs more than the throttle
// ever did. So the recovery is asserted separately: throttle briefly, stop, and
// require the concurrency to climb back to the ceiling.
func TestScopedSweepRecoversConcurrencyAfterThrottling(t *testing.T) {
	const folders = 80
	files := []*drive.File{folder("id-mine", "mine", fakeRootID)}
	for i := range folders {
		files = append(files, folder(fmt.Sprintf("id-d%02d", i), fmt.Sprintf("d%02d", i), "id-mine"))
	}
	d, fake := newFakeDrive(t, files...)
	mountSubfolder(d, "id-mine")
	fake.childPage = 1000
	fake.childDelay = 3 * time.Millisecond
	// A short burst, over well before the sweep is: enough to drive the descent
	// down, not enough to keep it there.
	fake.childRateLimit = 6

	// Forget the batches issued while the burst was still in progress, so what is
	// measured is only what happened once Drive stopped saying no.
	const afterBurst = 4
	cursor := ""
	for call := range 400 {
		if call == afterBurst {
			fake.resetChildPeak()
		}
		_, next, err := d.Enumerate(ctx, cursor)
		if err != nil {
			continue
		}
		if next == "" {
			break
		}
		cursor = next
	}

	if peak := fake.childPeak.Load(); peak < enumFanout {
		t.Fatalf("after the throttling stopped the descent only reached %d concurrent listing(s) of %d: "+
			"it backs off but never recovers", peak, enumFanout)
	}
}
