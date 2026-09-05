package gauth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// The wizard's interactive half is not worth harnessing, but its loopback server
// is: it is a listener drivel opens on the user's machine that accepts a
// credential from a browser. These tests drive that server directly. Everything
// here stops short of the token exchange, which is the only part that needs
// Google — so the suite stays hermetic.

type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type loginRun struct {
	errc     <-chan error
	out      *syncBuf
	redirect string   // where the browser is told to come back to
	authURL  *url.URL // what the user is told to open
}

// startLogin runs Login on an OS-assigned port and waits until it is listening.
func startLogin(t *testing.T, in io.Reader) (loginRun, context.CancelFunc) {
	t.Helper()
	if in == nil {
		in = strings.NewReader("")
	}
	ctx, cancel := context.WithCancel(t.Context())

	out := &syncBuf{}
	errc := make(chan error, 1)
	// done, not errc, is what the cleanup waits on: the test itself usually
	// consumes errc, and a cleanup that insisted on a second value would hang.
	done := make(chan struct{})
	go func() {
		_, err := Login(ctx,
			Credentials{ClientID: "client-id-1", ClientSecret: "client-secret-1"},
			[]string{ScopeDrive},
			LoginOptions{Port: 0, Out: out, In: in})
		errc <- err
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Login did not return after its context was cancelled")
		}
	})

	const marker = "Listening for the redirect on "
	deadline := time.Now().Add(10 * time.Second)
	for {
		printed := out.String()
		if i := strings.Index(printed, marker); i >= 0 {
			rest := printed[i+len(marker):]
			if j := strings.IndexByte(rest, '\n'); j >= 0 {
				run := loginRun{errc: errc, out: out, redirect: strings.TrimSpace(rest[:j])}
				run.authURL = parseAuthURL(t, printed)
				return run, cancel
			}
		}
		select {
		case err := <-errc:
			cancel()
			t.Fatalf("Login returned before it was listening: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("Login never reported a listening address; it printed:\n%s", printed)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func parseAuthURL(t *testing.T, printed string) *url.URL {
	t.Helper()
	for _, line := range strings.Split(printed, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "https://") {
			continue
		}
		u, err := url.Parse(line)
		if err != nil {
			t.Fatalf("the printed authorization URL does not parse: %v", err)
		}
		return u
	}
	t.Fatalf("no authorization URL was printed:\n%s", printed)
	return nil
}

// visit issues the browser's redirect back to the loopback server.
func visit(t *testing.T, base string, query url.Values) *http.Response {
	t.Helper()
	u := strings.TrimSuffix(base, "/") + "/?" + query.Encode()
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(u)
	if err != nil {
		t.Fatalf("GET %s: %v", u, err)
	}
	return resp
}

func awaitLogin(t *testing.T, run loginRun) error {
	t.Helper()
	select {
	case err := <-run.errc:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("Login never returned")
		return nil
	}
}

// What the user is told to open decides what drivel gets back. Offline access is
// what yields a refresh token, and without one a mount stops working an hour
// after login with no way to renew short of logging in again.
func TestLoginAsksForOfflineAccessOnItsOwnLoopback(t *testing.T) {
	run, cancel := startLogin(t, nil)
	defer cancel()

	q := run.authURL.Query()
	if got := q.Get("access_type"); got != "offline" {
		t.Errorf("access_type = %q; want offline, or there is no refresh token", got)
	}
	if got := q.Get("response_type"); got != "code" {
		t.Errorf("response_type = %q; want code", got)
	}
	if got := q.Get("client_id"); got != "client-id-1" {
		t.Errorf("client_id = %q", got)
	}
	if got := q.Get("scope"); got != ScopeDrive {
		t.Errorf("scope = %q; want the scope Login was given", got)
	}
	if got := q.Get("redirect_uri"); got != run.redirect {
		t.Errorf("redirect_uri = %q but the server listens on %q", got, run.redirect)
	}
	if q.Get("state") == "" {
		t.Error("no state parameter; the CSRF check has nothing to compare against")
	}

	// The container hint has to name the port actually in use, since it is the
	// port the user has to forward.
	port := run.redirect[strings.LastIndexByte(run.redirect, ':')+1 : len(run.redirect)-1]
	if !strings.Contains(run.out.String(), ":"+port+":"+port) {
		t.Errorf("the docker hint does not name port %s:\n%s", port, run.out.String())
	}
}

// The state parameter is the whole CSRF defence: it is what stops a page the
// user is browsing from walking an attacker's authorization code into their
// drivel install. A mismatch must abort the login, not merely be logged.
func TestLoginRefusesACodeWithTheWrongState(t *testing.T) {
	run, cancel := startLogin(t, nil)
	defer cancel()

	resp := visit(t, run.redirect, url.Values{"code": {"4/attacker-code"}, "state": {"not-the-state"}})
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	err := awaitLogin(t, run)
	if err == nil {
		t.Fatal("Login accepted a code carrying the wrong state")
	}
	if !strings.Contains(err.Error(), "state") {
		t.Errorf("error = %v; it should say the state did not match", err)
	}
	if !strings.Contains(string(body), "state mismatch") {
		t.Errorf("the browser was not told what happened: %q", body)
	}
}

// A denial is a normal outcome — the user clicked Cancel — and it has to end the
// wait rather than leave the terminal blocked on a redirect that will not come.
// Whatever Google puts in the error parameter is reflected into the page, so it
// is escaped and served as inert plain text.
func TestLoginReportsAnAuthorizationDenial(t *testing.T) {
	run, cancel := startLogin(t, nil)
	defer cancel()

	resp := visit(t, run.redirect, url.Values{"error": {`access_denied<script>alert(1)</script>`}})
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Errorf("Content-Type = %q; want text/plain", got)
	}
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q; want nosniff", got)
	}
	if strings.Contains(string(body), "<script>") {
		t.Errorf("the error parameter was reflected unescaped: %q", body)
	}

	err := awaitLogin(t, run)
	if err == nil {
		t.Fatal("Login ignored an authorization denial")
	}
	if !strings.Contains(err.Error(), "access_denied") {
		t.Errorf("error = %v; it should carry the reason Google gave", err)
	}
}

// A browser asking for /favicon.ico is not an authorization result. Treating any
// stray request as one would abort a login that is about to succeed.
func TestLoginIgnoresARequestCarryingNoCode(t *testing.T) {
	run, cancel := startLogin(t, nil)
	defer cancel()

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(strings.TrimSuffix(run.redirect, "/") + "/favicon.ico")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /favicon.ico = %s; want 404", resp.Status)
	}

	select {
	case err := <-run.errc:
		t.Fatalf("Login gave up on a request with no code: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	// ...and it is still the context that ends the wait.
	cancel()
	if err := awaitLogin(t, run); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v; want context.Canceled", err)
	}
}

// The paste fallback is what a container user actually uses, so its failure mode
// has to be a message rather than a hang: a URL with no code in it is the shape
// you get by copying the address bar after a denial.
func TestLoginRejectsAPastedURLWithNoCode(t *testing.T) {
	run, cancel := startLogin(t, strings.NewReader("http://127.0.0.1:53682/?state=xyz&error=access_denied\n"))
	defer cancel()

	err := awaitLogin(t, run)
	if err == nil {
		t.Fatal("Login accepted a pasted URL that carried no code")
	}
	if !strings.Contains(err.Error(), "no code parameter") {
		t.Errorf("error = %v; it should say the paste had no code in it", err)
	}
}

// The documented setup forwards port 53682, and the commonest way that goes
// wrong is a second drivel login (or an rclone) already holding it. That has to
// be an error naming the port, not a failure to bind reported as something else.
func TestLoginReportsABusyLoopbackPort(t *testing.T) {
	var lc net.ListenConfig
	held, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot bind a loopback port here: %v", err)
	}
	defer held.Close()
	port := held.Addr().(*net.TCPAddr).Port

	_, err = Login(t.Context(),
		Credentials{ClientID: "id"}, []string{ScopeDrive},
		LoginOptions{Port: port, Out: io.Discard, In: strings.NewReader("")})
	if err == nil {
		t.Fatal("Login bound a port that was already taken")
	}
	if !strings.Contains(err.Error(), fmt.Sprint(port)) {
		t.Errorf("error = %v; it should name port %d", err, port)
	}
}

// Ctrl-C during the wait has to unwind the wizard rather than leave a listener
// and a stdin reader behind.
func TestLoginStopsWhenItsContextIsCancelled(t *testing.T) {
	run, cancel := startLogin(t, nil)
	cancel()

	if err := awaitLogin(t, run); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v; want context.Canceled", err)
	}
}

// Pressing Enter at the paste prompt — while waiting for the browser to come
// back on its own — must not end the wizard. The auto-capture path is still the
// one most users are on when they do it.
func TestLoginIgnoresABlankPastedLine(t *testing.T) {
	run, cancel := startLogin(t, strings.NewReader("\n"))
	defer cancel()

	select {
	case err := <-run.errc:
		t.Fatalf("Login gave up when a blank line was entered: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	cancel()
	if err := awaitLogin(t, run); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v; want context.Canceled", err)
	}
}
