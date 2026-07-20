package gauth

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"

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
// code. Because dedupfs commonly runs in a container whose port may not be
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

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", opts.Port))
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
		q := r.URL.Query()
		if e := q.Get("error"); e != "" {
			fmt.Fprintf(w, "dedupfs: authorization failed (%s). You can close this tab.", e)
			trySend(errCh, fmt.Errorf("authorization denied: %s", e))
			return
		}
		code := q.Get("code")
		if code == "" {
			http.NotFound(w, r) // e.g. /favicon.ico
			return
		}
		if q.Get("state") != state {
			fmt.Fprint(w, "dedupfs: state mismatch. You can close this tab.")
			trySend(errCh, fmt.Errorf("state parameter mismatch (possible CSRF)"))
			return
		}
		fmt.Fprint(w, "dedupfs: authorization received — you can close this tab and return to the terminal.")
		select {
		case codeCh <- code:
		default:
		}
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	defer srv.Close()

	fmt.Fprintf(out, "\nOpen this URL in your browser to authorize dedupfs:\n\n  %s\n\n", authURL)
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
	return exec.Command(name, args...).Start()
}
