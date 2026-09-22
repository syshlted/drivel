// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package gdrive

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	drive "google.golang.org/api/drive/v3"
	"google.golang.org/api/option"
)

// fakeDrive is enough of the Drive v3 REST surface to exercise path↔ID
// resolution: metadata get, a name/parent query, create, update, delete,
// download and about.
//
// It counts requests, because most of what M7 claims is about how many round
// trips a resolution costs.
type fakeDrive struct {
	mu    sync.Mutex
	files map[string]*drive.File // id -> file

	// content is the byte store behind those files. Bytes were deliberately
	// absent while M7 was the only customer, and the same-name sibling cases
	// (MC-30) are what brought them in: three clients that create one path each
	// hold a different body, and "two of these are now unreachable" is a claim
	// about content, not about IDs.
	content map[string][]byte

	// srv is this fake's server, kept so that further clients can be attached to
	// one remote — several machines syncing one Drive folder. See client.
	srv *httptest.Server

	gets    int // Files.Get (metadata only; a download is not a resolution cost)
	lists   int // Files.List
	creates int // Files.Create
	updates int // Files.Update with a body
	deletes int // Files.Delete

	// nextID mints an opaque ID per create, as Drive does. It is what makes a
	// same-name sibling possible at all: keying a created object by its name
	// would fold three concurrent creates into one file and test the fake instead
	// of the provider.
	nextID int

	// clock stamps modifiedTime, one tick per mutation, so "the most recently
	// modified sibling" is a total order that a tie-break can be asserted
	// against rather than a coincidence of wall-clock resolution.
	clock int

	// nameGate, when set, holds every name query until the expected number of
	// them are in flight at once. That one interleaving is the whole of MC-30:
	// each client asks "does this path exist?", every one of them is told no,
	// and only then does any create land. Left to chance the calls serialise,
	// the second client finds the first client's file and updates it, and the
	// situation under test cannot arise.
	nameGate *nameBarrier

	// listOrder fixes the order of the flat enumeration listing (by fileID). A
	// real listing has no parent-before-child guarantee, so tests set this to
	// reproduce the orders that guarantee would have hidden.
	listOrder []string

	// expirePageTokens makes the next paginated request answer 410, as Drive does
	// for a listing or change token that has aged out.
	expirePageTokens bool

	// childDelay stands in for a network round trip on the one request shape M7c
	// issues concurrently. Without it the fake answers instantly and a sweep that
	// fans out is indistinguishable from one that does not — every request here
	// serialises behind f.mu, so the concurrency claim is untestable by default
	// rather than merely unmeasured. The sleep is taken *before* the lock, for the
	// same reason nameGate is: holding f.mu across it would serialise the very
	// thing being measured.
	childDelay time.Duration

	// childRateLimit makes the next N child listings answer 403
	// userRateLimitExceeded, which is how Drive says "too fast". It counts down
	// under f.mu, so a fanned-out batch consumes several at once — which is the
	// case worth testing, since the whole question M7c leaves open is what happens
	// when several concurrent listings are throttled together.
	childRateLimit int

	// childFailEvery makes every Nth child listing answer 403, so a throttle lands
	// on part of a fanned-out batch rather than on all of it. That is the case the
	// counter above cannot reach and the one that says what concurrency costs when
	// Drive pushes back.
	childFailEvery int
	childSeen      int

	// childInFlight / childPeak record how many child listings are actually in
	// the server at once. Atomics rather than f.mu because the interesting window
	// spans childDelay, which is taken *outside* that lock — measuring it inside
	// would report 1 forever and prove nothing.
	childInFlight atomic.Int32
	childPeak     atomic.Int32

	// childPage overrides fakeChildPage. A concurrency measurement wants folders
	// that do not page, because paging inside one folder is inherently sequential
	// (the next token comes from the previous response) and would muddy the ratio.
	childPage int

	// changeLog is the changes.list feed, and it is modelled the way Drive's own
	// is: one entry per *object* whose state changed, carrying that object's
	// current metadata. There is deliberately no way to record "this file left
	// that path" — Drive has none either, which is exactly the property the
	// rename cases are about.
	changeLog []*drive.Change

	// permID is what about.get reports. Two fakes in one test give different
	// answers, which is how the persistent index tells the accounts apart (M7).
	permID string
}

// rootID is the concrete ID of My Drive, which is what a file's parents actually
// contain — the "root" alias never appears there, and telling the two apart is
// half of what verification does.
const fakeRootID = "root-concrete"

var (
	reFileID   = regexp.MustCompile(`^/files/([^/]+)$`)
	reQueryOne = regexp.MustCompile(`name = '((?:[^'\\]|\\.)*)'`)
	reQueryTwo = regexp.MustCompile(`'([^']*)' in parents`)
)

func newFakeDrive(t *testing.T, files ...*drive.File) (*Drive, *fakeDrive) {
	t.Helper()
	f := &fakeDrive{
		permID:  "perm-1",
		content: map[string][]byte{},
		files: map[string]*drive.File{
			fakeRootID: {Id: fakeRootID, Name: "My Drive", MimeType: folderMIME},
		},
	}
	for _, file := range files {
		f.files[file.Id] = file
	}

	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)

	return f.client(t), f
}

// client attaches another independent Drive to this same remote — one more
// machine syncing one folder. Each gets its own maps, its own mutex and its own
// idea of what exists; nothing is shared but the server, which is exactly the
// sharing a fleet has.
func (f *fakeDrive) client(t *testing.T) *Drive {
	t.Helper()
	svc, err := drive.NewService(context.Background(),
		option.WithHTTPClient(f.srv.Client()), option.WithoutAuthentication())
	if err != nil {
		t.Fatalf("drive service: %v", err)
	}
	svc.BasePath = f.srv.URL + "/"

	return &Drive{
		svc:      svc,
		close:    func() error { return nil },
		root:     "root",
		idByPath: map[string]string{"": "root"},
		pathByID: map[string]string{"root": ""},
	}
}

// nameBarrier holds arriving requests until n of them are waiting together, then
// releases them all. It is how a deterministic test reproduces a genuine race
// rather than emulating its outcome.
//
// It gives up after a while instead of hanging: a barrier that never fills means
// the interleaving under test did not form, and a test that says so beats a
// package that times out ten minutes later with no explanation.
type nameBarrier struct {
	mu      sync.Mutex
	n       int
	arrived int
	stuck   bool
	open    chan struct{}
}

func newNameBarrier(n int) *nameBarrier {
	return &nameBarrier{n: n, open: make(chan struct{})}
}

func (b *nameBarrier) arrive() {
	b.mu.Lock()
	b.arrived++
	if b.arrived == b.n {
		close(b.open)
	}
	ch := b.open
	b.mu.Unlock()

	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		b.mu.Lock()
		b.stuck = true
		b.mu.Unlock()
	}
}

// stalled reports that some caller left the barrier on the timeout rather than
// because it filled.
func (b *nameBarrier) stalled() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.stuck
}

// file builds a remote file under the given parent.
func file(id, name, parent string) *drive.File {
	return &drive.File{
		Id: id, Name: name, Parents: []string{parent},
		Md5Checksum: "hash-" + id, Version: 1, Size: 10,
		ModifiedTime: "2026-09-01T10:00:00Z",
	}
}

func folder(id, name, parent string) *drive.File {
	f := file(id, name, parent)
	f.MimeType = folderMIME
	f.Md5Checksum = ""
	return f
}

func (f *fakeDrive) counts() (gets, lists, creates, deletes int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gets, f.lists, f.creates, f.deletes
}

// updateCount is the number of content-bearing Files.Update calls. It is
// separate from counts() because the sibling cases turn on the difference
// between "this client replaced the file that was already there" and "this
// client made a second one".
func (f *fakeDrive) updateCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.updates
}

func (f *fakeDrive) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets, f.lists, f.creates, f.deletes, f.updates = 0, 0, 0, 0, 0
}

func (f *fakeDrive) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Before the lock, and only for the one request shape the barrier is about:
	// waiting while holding f.mu would deadlock every peer trying to reach it.
	// nameGate is set before any client is started, so reading it here is not a
	// race with the test goroutine that wrote it.
	if f.nameGate != nil && isNameQuery(r) {
		f.nameGate.arrive()
	}
	// Likewise before the lock, and likewise safe to read unsynchronised: both are
	// set before any client is started.
	if isChildQuery(r) {
		n := f.childInFlight.Add(1)
		for {
			peak := f.childPeak.Load()
			if n <= peak || f.childPeak.CompareAndSwap(peak, n) {
				break
			}
		}
		defer f.childInFlight.Add(-1)
		if f.childDelay > 0 {
			time.Sleep(f.childDelay)
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")

	switch {
	case r.URL.Path == "/about":
		writeJSON(w, map[string]any{"user": map[string]string{"permissionId": f.permID}})

	case r.URL.Path == "/changes/startPageToken" && r.Method == http.MethodGet:
		// A cursor taken now covers everything after what has already happened.
		writeJSON(w, map[string]any{"startPageToken": strconv.Itoa(len(f.changeLog))})

	case r.URL.Path == "/changes" && r.Method == http.MethodGet:
		if f.expirePageTokens {
			// What Drive answers for a token that has aged out: 410, not a 5xx, and
			// retrying it never succeeds.
			writeErr(w, http.StatusGone, "Page token expired")
			return
		}
		f.serveChanges(w, r.URL.Query().Get("pageToken"))

	case r.URL.Path == "/files" && r.Method == http.MethodGet:
		f.lists++
		switch q := r.URL.Query().Get("q"); {
		case q == sweepQuery:
			f.serveSweep(w, r.URL.Query().Get("pageToken"))
		case reQueryOne.MatchString(q):
			// name = '<name>' and '<parent>' in parents — a path lookup (M7).
			writeJSON(w, map[string]any{"files": f.query(q)})
		case reQueryTwo.MatchString(q):
			// '<parent>' in parents, with no name — a scoped descent listing one
			// folder's children (M7c). Real Drive tells the two apart by result;
			// the fake has to tell them apart by shape, because only this one pages.
			f.serveChildren(w, q, r.URL.Query().Get("pageToken"))
		default:
			writeJSON(w, map[string]any{"files": []*drive.File{}})
		}

	case r.URL.Path == "/files" && r.Method == http.MethodPost:
		// Metadata-only create: a folder, or an empty file. The body-bearing form
		// goes to the upload endpoint below.
		f.creates++
		var in drive.File
		_ = json.NewDecoder(r.Body).Decode(&in)
		writeJSON(w, f.createLocked(&in, nil))

	case strings.HasPrefix(r.URL.Path, uploadPath):
		f.serveUpload(w, r)

	case reFileID.MatchString(r.URL.Path) && r.Method == http.MethodGet:
		id := f.normalize(reFileID.FindStringSubmatch(r.URL.Path)[1])
		got, ok := f.files[id]
		if !ok {
			writeErr(w, http.StatusNotFound, "File not found: "+id)
			return
		}
		if r.URL.Query().Get("alt") == "media" {
			// Deliberately not counted as a get: the round-trip assertions elsewhere
			// are about what a resolution costs, and a download is not one of those.
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(f.content[id])
			return
		}
		f.gets++
		writeJSON(w, got)

	case reFileID.MatchString(r.URL.Path) && r.Method == http.MethodPatch:
		// Metadata-only update. The one shape implemented is the trash flag, which
		// is how a default removal reaches Drive. A move (addParents/removeParents)
		// is refused outright rather than half-applied: silently ignoring the
		// parents would let a future Move test pass against a tree this fake never
		// rearranged.
		if r.URL.Query().Get("addParents") != "" || r.URL.Query().Get("removeParents") != "" {
			writeErr(w, http.StatusNotImplemented, "fakeDrive: move is not implemented")
			return
		}
		id := f.normalize(reFileID.FindStringSubmatch(r.URL.Path)[1])
		got, ok := f.files[id]
		if !ok {
			writeErr(w, http.StatusNotFound, "File not found: "+id)
			return
		}
		var in drive.File
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.updates++
		if in.Name != "" {
			got.Name = in.Name
		}
		if in.Trashed {
			// Drive trashes a folder's whole subtree with it, and every query the
			// provider issues carries "trashed = false", so a fake that trashed only
			// the named object would show children the real API hides.
			f.trashLocked(got)
		}
		writeJSON(w, got)

	case reFileID.MatchString(r.URL.Path) && r.Method == http.MethodDelete:
		f.deletes++
		delete(f.files, f.normalize(reFileID.FindStringSubmatch(r.URL.Path)[1]))
		w.WriteHeader(http.StatusNoContent)

	default:
		writeErr(w, http.StatusNotImplemented, "fakeDrive: "+r.Method+" "+r.URL.Path)
	}
}

// uploadPath is where googleapi sends a request that carries a body; the
// metadata-only endpoint is /files. Keeping the two apart in the fake is what
// keeps "created a file" and "created a file with these bytes" distinguishable.
const uploadPath = "/upload/drive/v3/files"

// isNameQuery reports the one request the barrier gates: the name-and-parent
// lookup that answers "does this path already exist?". The sweep's flat listing
// and the change feed go past untouched.
func isNameQuery(r *http.Request) bool {
	return r.URL.Path == "/files" && r.Method == http.MethodGet &&
		strings.Contains(r.URL.Query().Get("q"), "name = '")
}

// isChildQuery matches the one listing shape a scoped descent issues: a folder's
// children, with no name clause to make it a path lookup.
func isChildQuery(r *http.Request) bool {
	if r.URL.Path != "/files" || r.Method != http.MethodGet {
		return false
	}
	q := r.URL.Query().Get("q")
	return q != sweepQuery && reQueryTwo.MatchString(q) && !reQueryOne.MatchString(q)
}

// serveUpload handles create-with-body (POST) and update-with-body (PATCH).
func (f *fakeDrive) serveUpload(w http.ResponseWriter, r *http.Request) {
	in, body, err := readUpload(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "fakeDrive: "+err.Error())
		return
	}
	rest := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, uploadPath), "/")
	if rest == "" {
		f.creates++
		writeJSON(w, f.createLocked(in, body))
		return
	}
	id := f.normalize(rest)
	got, ok := f.files[id]
	if !ok {
		writeErr(w, http.StatusNotFound, "File not found: "+id)
		return
	}
	f.updates++
	if in.Name != "" {
		got.Name = in.Name
	}
	f.writeBodyLocked(got, body)
	writeJSON(w, got)
}

// readUpload parses the multipart/related body googleapi sends when the payload
// fits in one chunk: a JSON metadata part, then the content.
//
// A payload larger than uploadChunkSize becomes a resumable session instead,
// which this fake deliberately does not implement. That protocol is Tier B's
// business (MC-10 in docs/dev/multiclient-test-plan.md) — it is exactly the part of
// an upload that only a real server can be wrong about, so speaking it here
// would prove nothing and would hide the day a test starts needing it.
func readUpload(r *http.Request) (*drive.File, []byte, error) {
	_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return nil, nil, fmt.Errorf("upload content type: %w", err)
	}
	boundary := params["boundary"]
	if boundary == "" {
		return nil, nil, fmt.Errorf("upload is not multipart (uploadType=%s); "+
			"fakeDrive speaks no resumable protocol", r.URL.Query().Get("uploadType"))
	}
	mr := multipart.NewReader(r.Body, boundary)
	var (
		meta  drive.File
		body  []byte
		parts int
	)
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, err
		}
		b, err := io.ReadAll(part)
		if err != nil {
			return nil, nil, err
		}
		if parts == 0 {
			if err := json.Unmarshal(b, &meta); err != nil {
				return nil, nil, fmt.Errorf("upload metadata: %w", err)
			}
		} else {
			body = b
		}
		parts++
	}
	return &meta, body, nil
}

// createLocked records a new object under a fresh opaque ID, the way Drive does.
// Nothing here consults the name: creating "same.txt" in a folder that already
// holds a "same.txt" produces a second one, because that is what the real API
// does and it is the whole subject of the sibling cases.
func (f *fakeDrive) createLocked(in *drive.File, body []byte) *drive.File {
	f.nextID++
	in.Id = fmt.Sprintf("created-%d", f.nextID)
	if len(in.Parents) == 0 {
		in.Parents = []string{fakeRootID}
	}
	in.Parents[0] = f.normalize(in.Parents[0])
	f.files[in.Id] = in
	f.writeBodyLocked(in, body)
	return in
}

// writeBodyLocked stores content and restamps the object, as a write does
// remotely: a new version, a new modifiedTime, and the checksum and size that
// the engine's §4 echo record and M6 hash gate both compare against.
func (f *fakeDrive) writeBodyLocked(file *drive.File, body []byte) {
	f.clock++
	file.Version++
	file.ModifiedTime = f.stampLocked()
	if file.MimeType == folderMIME {
		return
	}
	f.content[file.Id] = body
	sum := md5.Sum(body)
	file.Md5Checksum = hex.EncodeToString(sum[:])
	file.Size = int64(len(body))
}

// fakeEpoch is the modifiedTime the seeded files carry, so every mutation the
// test performs is strictly newer than the tree it started from.
var fakeEpoch = time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)

// stampLocked is modifiedTime for the mutation happening now. Drive stamps
// RFC3339 in UTC, which is the only reason the provider may compare these as
// strings; the fake has to keep that property or the tie-break it is testing
// would be testing something else.
func (f *fakeDrive) stampLocked() string {
	return fakeEpoch.Add(time.Duration(f.clock) * time.Second).Format(time.RFC3339)
}

// seedFile adds a file with content, one clock tick newer than everything
// already there.
func (f *fakeDrive) seedFile(name, parent string, body []byte) *drive.File {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.createLocked(&drive.File{Name: name, Parents: []string{parent}}, body)
}

// has reports whether an object still exists, under the fake's own lock — the
// server's goroutines share this map, so a test must not read it directly.
func (f *fakeDrive) has(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.files[id]
	return ok
}

// trashLocked marks file trashed, and everything beneath it if it is a folder —
// Drive's `trashed` is "explicitly, or from a trashed parent folder".
//
// The fake settles that instantly and real Drive does not: measured against the
// rig, a child still read trashed=false right after its parent was trashed. This
// models the settled state on purpose, because nothing above the seam depends on
// the timing; TestLiveRemoveTrashes is where the propagation itself is asserted.
func (f *fakeDrive) trashLocked(file *drive.File) {
	file.Trashed = true
	if file.MimeType != folderMIME {
		return
	}
	for _, child := range f.files {
		for _, parent := range child.Parents {
			if parent == file.Id && !child.Trashed {
				f.trashLocked(child)
			}
		}
	}
}

// trashed reports whether an object is in the trash: still there, and invisible
// to every query the provider makes. A test distinguishing the two removals
// needs both this and has().
func (f *fakeDrive) trashed(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	file, ok := f.files[id]
	return ok && file.Trashed
}

// namedChildren is every non-trashed object called name in parent — the fake's
// own view, which is what "drivel cannot see these" has to be measured against.
func (f *fakeDrive) namedChildren(parent, name string) []*drive.File {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*drive.File
	for _, file := range f.files {
		if file.Name != name || file.Trashed {
			continue
		}
		for _, p := range file.Parents {
			if p == f.normalize(parent) {
				out = append(out, file)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Id < out[j].Id })
	return out
}

// serveChanges answers changes.list from the change log, cursor-indexed. One page
// is enough: paging is the sweep's problem, and the feed's own paging is the same
// nextPageToken loop already covered there.
func (f *fakeDrive) serveChanges(w http.ResponseWriter, token string) {
	from, err := strconv.Atoi(token)
	if err != nil || from < 0 || from > len(f.changeLog) {
		from = len(f.changeLog)
	}
	writeJSON(w, map[string]any{
		"changes":           f.changeLog[from:],
		"newStartPageToken": strconv.Itoa(len(f.changeLog)),
	})
}

// touch records that an object changed, as Drive would: the change carries the
// file's state *now*, under whatever name and parent it now has.
func (f *fakeDrive) touch(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	file, ok := f.files[id]
	if !ok {
		f.changeLog = append(f.changeLog, &drive.Change{FileId: id, Removed: true})
		return
	}
	file.Version++
	f.changeLog = append(f.changeLog, &drive.Change{FileId: id, File: file})
}

// rename moves id to a new name and/or parent and records the change — the
// remote-side rename that another client made while we were watching the feed.
func (f *fakeDrive) rename(id, newName, newParent string) {
	f.mu.Lock()
	file, ok := f.files[id]
	if ok {
		file.Name = newName
		if newParent != "" {
			file.Parents = []string{f.normalize(newParent)}
		}
	}
	f.mu.Unlock()
	f.touch(id)
}

// sweepQuery is the one query Enumerate issues. Matching it exactly keeps the two
// listing shapes apart in the fake, as the real API keeps them apart by result.
const sweepQuery = "trashed = false"

// fakeEnumPage is deliberately tiny so every enumeration test crosses a page
// boundary — the interesting cases (a child listed before its parent, a resumed
// sweep) only exist across pages.
const fakeEnumPage = 2

// serveSweep answers the flat listing, paginated. Order is fixed by listOrder
// when a test sets one, so "the child arrives first" is a property of the test
// rather than of map iteration.
func (f *fakeDrive) serveSweep(w http.ResponseWriter, token string) {
	if f.expirePageTokens && token != "" {
		f.expirePageTokens = false // only the resumed token is stale
		writeErr(w, http.StatusGone, "Page token expired")
		return
	}
	all := f.sweepFiles()
	start := 0
	if token != "" {
		start, _ = strconv.Atoi(strings.TrimPrefix(token, "page-"))
	}
	if start > len(all) {
		start = len(all)
	}
	end := start + fakeEnumPage
	if end > len(all) {
		end = len(all)
	}
	res := map[string]any{"files": all[start:end]}
	if end < len(all) {
		res["nextPageToken"] = fmt.Sprintf("page-%d", end)
	}
	writeJSON(w, res)
}

// fakeChildPage is the scoped descent's page size, tiny for the same reason
// fakeEnumPage is: a folder that pages is the only way to reach the code that
// puts a half-listed folder back on the frontier.
const fakeChildPage = 2

// serveChildren answers one page of one folder's direct children, which is the
// only listing shape M7c's descent issues.
func (f *fakeDrive) serveChildren(w http.ResponseWriter, q, token string) {
	if f.expirePageTokens && token != "" {
		f.expirePageTokens = false // only the resumed token is stale
		writeErr(w, http.StatusGone, "Page token expired")
		return
	}
	if f.childRateLimit > 0 {
		f.childRateLimit--
		writeRateLimit(w)
		return
	}
	if f.childFailEvery > 0 {
		f.childSeen++
		if f.childSeen%f.childFailEvery == 0 {
			writeRateLimit(w)
			return
		}
	}
	m := reQueryTwo.FindStringSubmatch(q)
	kids := f.childrenOf(f.normalize(m[1]))

	start := 0
	if token != "" {
		start, _ = strconv.Atoi(strings.TrimPrefix(token, "kids-"))
	}
	if start > len(kids) {
		start = len(kids)
	}
	size := fakeChildPage
	if f.childPage > 0 {
		size = f.childPage
	}
	end := start + size
	if end > len(kids) {
		end = len(kids)
	}
	res := map[string]any{"files": kids[start:end]}
	if end < len(kids) {
		res["nextPageToken"] = fmt.Sprintf("kids-%d", end)
	}
	writeJSON(w, res)
}

// resetChildPeak forgets the high-water mark, so a measurement can ignore the
// batches a descent issued before it had any throttling to react to.
func (f *fakeDrive) resetChildPeak() { f.childPeak.Store(0) }

// childrenOf is every non-trashed direct child of one folder, in a fixed order so
// paging is reproducible.
func (f *fakeDrive) childrenOf(parentID string) []*drive.File {
	var out []*drive.File
	for _, file := range f.files {
		if file.Trashed || file.Id == parentID {
			continue
		}
		for _, p := range file.Parents {
			if f.normalize(p) == parentID {
				out = append(out, file)
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Id < out[j].Id })
	return out
}

// sweepFiles is everything a flat listing returns: not trashed, and never the
// root folder itself (files.list does not report it, which is why the provider
// has to recognise the root by ID rather than by finding it in the listing).
func (f *fakeDrive) sweepFiles() []*drive.File {
	var out []*drive.File
	if len(f.listOrder) > 0 {
		for _, id := range f.listOrder {
			if file, ok := f.files[id]; ok && !file.Trashed && file.Id != fakeRootID {
				out = append(out, file)
			}
		}
		return out
	}
	for _, file := range f.files {
		if file.Trashed || file.Id == fakeRootID {
			continue
		}
		out = append(out, file)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Id < out[j].Id })
	return out
}

// normalize maps the "root" alias onto the concrete root ID, as Drive does.
func (f *fakeDrive) normalize(id string) string {
	if id == "root" || id == "" {
		return fakeRootID
	}
	return id
}

// query answers the one shape of query the provider issues:
// name = '<name>' and '<parent>' in parents and trashed = false
func (f *fakeDrive) query(q string) []*drive.File {
	name := reQueryOne.FindStringSubmatch(q)
	parent := reQueryTwo.FindStringSubmatch(q)
	if name == nil || parent == nil {
		return nil
	}
	want := strings.NewReplacer(`\\`, `\`, `\'`, `'`).Replace(name[1])
	parentID := f.normalize(parent[1])

	var out []*drive.File
	for _, file := range f.files {
		if file.Name != want || file.Trashed {
			continue
		}
		for _, p := range file.Parents {
			if p == parentID {
				out = append(out, file)
			}
		}
	}
	// Deterministic order, so a test that depends on tie-breaking depends on the
	// provider's rule rather than on map iteration.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Id < out[j-1].Id; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func writeJSON(w http.ResponseWriter, v any) {
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.WriteHeader(code)
	writeJSON(w, map[string]any{"error": map[string]any{"code": code, "message": msg}})
}

// writeRateLimit is what Drive answers when a user is asking too fast, and the
// shape matters: the status is 403, not 429, and only the reason inside tells it
// apart from a permission failure. isTransient reads exactly that, so a fake that
// wrote a bare 403 would be testing the wrong classification.
func writeRateLimit(w http.ResponseWriter) {
	w.WriteHeader(http.StatusForbidden)
	writeJSON(w, map[string]any{"error": map[string]any{
		"code":    http.StatusForbidden,
		"message": "User rate limit exceeded.",
		"errors": []map[string]any{{
			"domain":  "usageLimits",
			"reason":  "userRateLimitExceeded",
			"message": "User rate limit exceeded.",
		}},
	}})
}
