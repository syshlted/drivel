package gdrive

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	drive "google.golang.org/api/drive/v3"
	"google.golang.org/api/option"
)

// fakeDrive is enough of the Drive v3 REST surface to exercise path↔ID
// resolution: metadata get, a name/parent query, create, delete, and about.
// Content upload is deliberately absent — M7 never touches bytes.
//
// It counts requests, because most of what M7 claims is about how many round
// trips a resolution costs.
type fakeDrive struct {
	mu    sync.Mutex
	files map[string]*drive.File // id -> file

	gets    int // Files.Get
	lists   int // Files.List
	creates int // Files.Create
	deletes int // Files.Delete

	// listOrder fixes the order of the flat enumeration listing (by fileID). A
	// real listing has no parent-before-child guarantee, so tests set this to
	// reproduce the orders that guarantee would have hidden.
	listOrder []string

	// expirePageTokens makes the next paginated request answer 410, as Drive does
	// for a listing or change token that has aged out.
	expirePageTokens bool

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
	f := &fakeDrive{permID: "perm-1", files: map[string]*drive.File{
		fakeRootID: {Id: fakeRootID, Name: "My Drive", MimeType: folderMIME},
	}}
	for _, file := range files {
		f.files[file.Id] = file
	}

	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)

	svc, err := drive.NewService(context.Background(),
		option.WithHTTPClient(srv.Client()), option.WithoutAuthentication())
	if err != nil {
		t.Fatalf("drive service: %v", err)
	}
	svc.BasePath = srv.URL + "/"

	d := &Drive{
		svc:      svc,
		close:    func() error { return nil },
		root:     "root",
		idByPath: map[string]string{"": "root"},
		pathByID: map[string]string{"root": ""},
	}
	return d, f
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

func (f *fakeDrive) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets, f.lists, f.creates, f.deletes = 0, 0, 0, 0
}

func (f *fakeDrive) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")

	switch {
	case r.URL.Path == "/about":
		writeJSON(w, map[string]any{"user": map[string]string{"permissionId": f.permID}})

	case r.URL.Path == "/changes" && r.Method == http.MethodGet:
		if f.expirePageTokens {
			// What Drive answers for a token that has aged out: 410, not a 5xx, and
			// retrying it never succeeds.
			writeErr(w, http.StatusGone, "Page token expired")
			return
		}
		writeJSON(w, map[string]any{"newStartPageToken": "tok-next"})

	case r.URL.Path == "/files" && r.Method == http.MethodGet:
		f.lists++
		if q := r.URL.Query().Get("q"); q == sweepQuery {
			f.serveSweep(w, r.URL.Query().Get("pageToken"))
			return
		}
		writeJSON(w, map[string]any{"files": f.query(r.URL.Query().Get("q"))})

	case r.URL.Path == "/files" && r.Method == http.MethodPost:
		f.creates++
		var in drive.File
		_ = json.NewDecoder(r.Body).Decode(&in)
		in.Id = "created-" + in.Name
		if len(in.Parents) == 0 {
			in.Parents = []string{fakeRootID}
		}
		in.Parents[0] = f.normalize(in.Parents[0])
		f.files[in.Id] = &in
		writeJSON(w, &in)

	case reFileID.MatchString(r.URL.Path) && r.Method == http.MethodGet:
		f.gets++
		id := f.normalize(reFileID.FindStringSubmatch(r.URL.Path)[1])
		got, ok := f.files[id]
		if !ok {
			writeErr(w, http.StatusNotFound, "File not found: "+id)
			return
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
