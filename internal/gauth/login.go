package gauth

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

// LoginOptions configure the interactive OAuth flow.
type LoginOptions struct {
	Port        int       // loopback port; 0 = OS-assigned. DefaultLoopbackPort recommended.
	OpenBrowser bool      // best-effort attempt to open the auth URL via the OS handler
	Out         io.Writer // prompts/URL sink (default os.Stdout)
	In          io.Reader // manual-paste source (default os.Stdin)
}

// Login runs the OAuth authorization-code flow with PKCE-less loopback redirect,
// exactly like rclone's auto config: it starts a local server on 127.0.0.1:<port>,
// prints the authorization URL, and waits for Google to redirect back with the
// code. Because Drivel commonly runs in a container whose port may not be
// reachable from the host browser, it ALSO accepts the redirect URL (or bare code)
// pasted on stdin — whichever arrives first wins.
func Login(ctx context.Context, c Credentials, scopes []string, opts LoginOptions) (*oauth2.Token, error) {
	out := opts.Out
	if out == nil {
		out = os.Stdout
	}
	in := opts.In
	if in == nil {
		in = os.Stdin
	}

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", opts.Port))
	if err != nil {
		return nil, fmt.Errorf("starting loopback listener on port %d: %w", opts.Port, err)
	}
	defer ln.Close()
	redirectURL := fmt.Sprintf("http://%s/", ln.Addr().String())

	config := c.Config(redirectURL, scopes...)
	state, err := randomState()
	if err != nil {
		return nil, err
	}
	// AccessTypeOffline yields a refresh token; ApprovalForce ensures we get one
	// even if the user previously consented.
	authURL := config.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.ApprovalForce)

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Everything this handler writes is plain text. Pinning the type stops
		// the sniffer from ever deciding otherwise about a value that arrives
		// in the query string.
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		q := r.URL.Query()
		if e := q.Get("error"); e != "" {
			fmt.Fprintf(w, "Drivel: authorization failed (%s). You can close this tab.", html.EscapeString(e))
			trySend(errCh, fmt.Errorf("authorization denied: %s", e))
			return
		}
		code := q.Get("code")
		if code == "" {
			http.NotFound(w, r) // e.g. /favicon.ico
			return
		}
		if q.Get("state") != state {
			fmt.Fprint(w, "Drivel: state mismatch. You can close this tab.")
			trySend(errCh, fmt.Errorf("state parameter mismatch (possible CSRF)"))
			return
		}
		fmt.Fprint(w, "Drivel: authorization received — you can close this tab and return to the terminal.")
		select {
		case codeCh <- code:
		default:
		}
	})
	srv := &http.Server{
		Handler: mux,
		// Without this a peer that opens a connection and dribbles headers holds
		// the loopback server open indefinitely (Slowloris).
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		// ErrServerClosed is the expected outcome of the deferred shutdown; anything
		// else means the redirect can never arrive, so fail instead of waiting
		// for a code that is not coming.
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			trySend(errCh, fmt.Errorf("loopback redirect server: %w", err))
		}
	}()
	// Graceful, not srv.Close(): the handler has just written the page the user
	// is looking at, and closing the connection out from under it replaces
	// drivel's explanation with a browser error — on exactly the paths (denied,
	// state mismatch) where the user most needs to be told what happened.
	// Detached from ctx because cancellation is one of the ways we get here.
	defer func() {
		stopCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer stop()
		_ = srv.Shutdown(stopCtx)
	}()

	fmt.Fprintf(out, "\nOpen this URL in your browser to authorize Drivel:\n\n  %s\n\n", authURL)
	if opts.OpenBrowser {
		if err := openBrowser(authURL); err != nil {
			fmt.Fprintf(out, "(could not open a browser automatically: %v)\n", err)
		}
	}
	fmt.Fprintf(out, "Listening for the redirect on %s\n", redirectURL)
	fmt.Fprintf(out, "  · If this runs in a container, forward the port from your host, e.g.\n")
	fmt.Fprintf(out, "      docker run -p 127.0.0.1:%d:%d ...\n", portOf(ln), portOf(ln))
	fmt.Fprintf(out, "  · Otherwise, when the browser cannot reach that address, copy the FULL\n")
	fmt.Fprintf(out, "    redirect URL from its address bar (or just the code) and paste it here:\n> ")

	// Manual-paste fallback. This read intentionally outlives the function on the
	// auto-capture path; acceptable because Login is used by short-lived commands.
	go func() {
		sc := bufio.NewScanner(in)
		if sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				return
			}
			code, err := extractCode(line)
			if err != nil {
				trySend(errCh, err)
				return
			}
			select {
			case codeCh <- code:
			default:
			}
		}
	}()

	var code string
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case err := <-errCh:
		return nil, err
	case code = <-codeCh:
	}

	tok, err := config.Exchange(ctx, code)
	if err != nil {
		return nil, fmt.Errorf("exchanging authorization code: %w", err)
	}
	return tok, nil
}

// extractCode pulls the OAuth code from either a bare code or a pasted redirect
// URL / query string (…?code=…&state=…).
func extractCode(s string) (string, error) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		u, err := url.Parse(s)
		if err != nil {
			return "", fmt.Errorf("parsing pasted URL: %w", err)
		}
		if code := u.Query().Get("code"); code != "" {
			return code, nil
		}
		return "", fmt.Errorf("no code parameter in the pasted URL")
	}
	if strings.Contains(s, "code=") {
		if q, err := url.ParseQuery(s); err == nil {
			if code := q.Get("code"); code != "" {
				return code, nil
			}
		}
		return "", fmt.Errorf("no code parameter in the pasted input")
	}
	return s, nil // assume the whole line is the code
}

func randomState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating state: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func trySend(ch chan<- error, err error) {
	select {
	case ch <- err:
	default:
	}
}

func portOf(ln net.Listener) int {
	if a, ok := ln.Addr().(*net.TCPAddr); ok {
		return a.Port
	}
	return 0
}

// openBrowser makes a best-effort attempt to open rawURL via the OS URL handler.
// In a headless container these handlers are absent and this returns an error,
// which the caller reports before falling back to the printed URL.
func openBrowser(rawURL string) error {
	var name string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		name, args = "open", []string{rawURL}
	case "windows":
		name, args = "rundll32", []string{"url.dll,FileProtocolHandler", rawURL}
	default:
		name, args = "xdg-open", []string{rawURL}
	}
	// Deliberately not CommandContext: the browser must outlive the login flow,
	// and CommandContext kills the child when ctx is cancelled — which is exactly
	// what happens the moment the code arrives.
	return exec.Command(name, args...).Start() //nolint:noctx // see above
}
