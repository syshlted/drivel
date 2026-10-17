// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

// The control API's first client (M14): drivel status, pause, resume and stop.
//
// It is the milestone's acceptance test as much as a convenience, and that
// decides how it is written: it speaks HTTP over the socket and decodes into
// types of its own, importing nothing from internal/control or the engine, so
// it can do nothing a stranger's tool holding the same socket could not. If a
// command here needs something the schema does not offer, the schema is what
// changes — never this file reaching around it. docs/dev/control-api.md is what
// it is written against.

// ctlFlags are the flags every control command shares.
type ctlFlags struct {
	socket string
	name   string
}

func (c *ctlFlags) bind(fset *flag.FlagSet, withName bool) {
	fset.StringVar(&c.socket, "socket", "", "the control socket of the drivel to talk to; default $XDG_RUNTIME_DIR/drivel/control.sock, as `drivel mount -control` creates it, or with -name that mount's own socket if an fstab line made one")
	if withName {
		fset.StringVar(&c.name, "name", "", "act on this mount only, instead of every mount the process serves")
	}
}

type statusCLI struct {
	ctlFlags
	asJSON bool
	self   bool
	limit  int
}

func statusFlagSet(c *statusCLI) *flag.FlagSet {
	fset := flag.NewFlagSet("drivel status", flag.ExitOnError)
	c.bind(fset, true)
	fset.BoolVar(&c.asJSON, "json", false, "print the API's JSON responses instead of a table")
	fset.BoolVar(&c.self, "d", false, "report a directory itself, rather than listing what is in it (as ls -d)")
	fset.IntVar(&c.limit, "limit", 1000, "list at most this many entries of a directory")
	return fset
}

type pauseCLI struct {
	ctlFlags
	dur time.Duration
}

func pauseFlagSet(c *pauseCLI) *flag.FlagSet {
	fset := flag.NewFlagSet("drivel pause", flag.ExitOnError)
	c.bind(fset, true)
	fset.DurationVar(&c.dur, "for", time.Hour, "how long the pause lasts before it ends on its own (at most 24h); a pause is never kept across a restart")
	return fset
}

func resumeFlagSet(c *ctlFlags) *flag.FlagSet {
	fset := flag.NewFlagSet("drivel resume", flag.ExitOnError)
	c.bind(fset, true)
	return fset
}

func stopFlagSet(c *ctlFlags) *flag.FlagSet {
	fset := flag.NewFlagSet("drivel stop", flag.ExitOnError)
	c.bind(fset, false)
	return fset
}

// --- the client ---------------------------------------------------------------

type ctlClient struct {
	http   *http.Client
	socket string
}

// systemSocketDir is where a mount started as root — an fstab line — puts its
// socket: /run/drivel/<uid>/NAME.sock. Restated rather than imported, because
// this client reads the documented schema and nothing else.
const systemSocketDir = "/run/drivel"

// findSocket is the socket to talk to when -socket was not given.
//
// Without -name it is the one `drivel mount -control` makes. With -name, the
// mount's OWN socket comes first, wherever an fstab line would have put it —
// $XDG_RUNTIME_DIR/drivel/NAME.sock for a helper run without root,
// /run/drivel/<uid>/NAME.sock for one started by root — and the shared one
// after it. Most specific first is the whole rule: an fstab process serves only
// the mount it is named for, so finding its socket answers the question, while
// the shared socket may belong to a process serving other mounts entirely.
// Only a socket that exists is chosen, so nothing here is a precedence between
// two live answers.
func findSocket(name string) (string, error) {
	xdg := os.Getenv("XDG_RUNTIME_DIR")
	if !filepath.IsAbs(xdg) {
		xdg = ""
	}
	var shared string
	if xdg != "" {
		shared = filepath.Join(xdg, "drivel", "control.sock")
	}
	if name == "" || strings.ContainsAny(name, `/\`) {
		if shared == "" {
			return "", errors.New("XDG_RUNTIME_DIR is not set; pass -socket PATH")
		}
		return shared, nil
	}
	var own []string
	if xdg != "" {
		own = append(own, filepath.Join(xdg, "drivel", name+".sock"))
	}
	own = append(own, filepath.Join(systemSocketDir, strconv.Itoa(os.Getuid()), name+".sock"))
	for _, p := range own {
		// gosec G703: name is the caller's own -name, refused above if it has a
		// separator, and this only asks whether their own socket exists.
		if fi, err := os.Lstat(p); err == nil && fi.Mode()&os.ModeSocket != 0 { //nolint:gosec // G703: see above
			return p, nil
		}
	}
	if shared == "" {
		return "", fmt.Errorf("no control socket for mount %q at %s, and XDG_RUNTIME_DIR is not set; pass -socket PATH",
			name, strings.Join(own, " or "))
	}
	return shared, nil
}

func dialControl(socket, name string) (*ctlClient, error) {
	if socket == "" {
		var err error
		if socket, err = findSocket(name); err != nil {
			return nil, err
		}
	}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}
	return &ctlClient{http: &http.Client{Transport: tr, Timeout: 30 * time.Second}, socket: socket}, nil
}

// apiError is the API's error body.
type apiError struct {
	Body struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (e *apiError) Error() string { return e.Body.Message }

// do sends one request and decodes the response into out, or returns the API's
// own error message.
func (c *ctlClient) do(method, path string, q url.Values, out any) error {
	u := "http://drivel" + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(context.Background(), method, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		var se *net.OpError
		if errors.As(err, &se) {
			return fmt.Errorf("no drivel is answering at %s (is it running with -control?): %w", c.socket, se.Err)
		}
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var ae apiError
		if json.Unmarshal(body, &ae) == nil && ae.Body.Message != "" {
			return &ae
		}
		return fmt.Errorf("%s %s: %s", method, path, resp.Status)
	}
	if raw, ok := out.(*json.RawMessage); ok {
		*raw = body
		return nil
	}
	return json.Unmarshal(body, out)
}

// The client's view of the schema: only the fields it prints.

type ctlMount struct {
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

type ctlPath struct {
	Path      string   `json:"path"`
	Status    string   `json:"status"`
	Kind      string   `json:"kind"`
	Detail    string   `json:"detail"`
	Error     string   `json:"error"`
	Attempt   int      `json:"attempt"`
	Conflicts []string `json:"conflicts"`
}

type ctlChildren struct {
	Entries   []ctlPath `json:"entries"`
	Truncated bool      `json:"truncated"`
	NextAfter string    `json:"next_after"`
}

func (c *ctlClient) mounts() ([]ctlMount, error) {
	var out struct {
		Mounts []ctlMount `json:"mounts"`
	}
	err := c.do(http.MethodGet, "/v1/mounts", nil, &out)
	return out.Mounts, err
}

// selected returns the mounts a command acts on: the one named, or all.
func (c *ctlClient) selected(name string) ([]ctlMount, error) {
	ms, err := c.mounts()
	if err != nil {
		return nil, err
	}
	if name == "" {
		return ms, nil
	}
	for _, m := range ms {
		if m.Name == name {
			return []ctlMount{m}, nil
		}
	}
	names := make([]string, len(ms))
	for i, m := range ms {
		names[i] = m.Name
	}
	return nil, fmt.Errorf("no mount named %q (this process serves: %s)", name, strings.Join(names, ", "))
}

// --- commands -----------------------------------------------------------------

func runStatus(args []string) error {
	var c statusCLI
	fset := statusFlagSet(&c)
	_ = fset.Parse(args)
	if c.limit < 1 {
		return fmt.Errorf("-limit %d is not a count (1 or more)", c.limit)
	}
	cl, err := dialControl(c.socket, c.name)
	if err != nil {
		return err
	}
	if fset.NArg() == 0 {
		return statusMounts(cl, c)
	}
	ms, err := cl.selected(c.name)
	if err != nil {
		return err
	}
	failed := false
	for _, arg := range fset.Args() {
		if err := statusPath(cl, c, ms, arg); err != nil {
			fmt.Fprintf(os.Stderr, "drivel status: %s: %v\n", arg, err)
			failed = true
		}
	}
	if failed {
		return errors.New("some paths could not be reported")
	}
	return nil
}

func statusMounts(cl *ctlClient, c statusCLI) error {
	if c.asJSON {
		var raw json.RawMessage
		path := "/v1/mounts"
		if c.name != "" {
			path += "/" + url.PathEscape(c.name)
		}
		if err := cl.do(http.MethodGet, path, nil, &raw); err != nil {
			return err
		}
		_, err := os.Stdout.Write(raw)
		return err
	}
	ms, err := cl.selected(c.name)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tPROVIDER\tSTATE\tBACKEND\tIN MOTION\tMOUNTPOINT")
	for _, m := range ms {
		provider := m.Provider
		if provider == "" {
			provider = "(log-only)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\n", m.Name, provider, mountState(m), m.Reachability, m.PathsInMotion, m.Mountpoint)
	}
	return tw.Flush()
}

func mountState(m ctlMount) string {
	var parts []string
	if m.Paused {
		parts = append(parts, "paused")
	}
	if m.Saturated {
		parts = append(parts, "saturated")
	}
	if m.Sweeping {
		parts = append(parts, "sweeping")
	}
	if len(parts) == 0 {
		if m.PathsInMotion > 0 {
			return "syncing"
		}
		return "idle"
	}
	return strings.Join(parts, ",")
}

// statusPath reports one local path, found by which mount's mountpoint holds it.
func statusPath(cl *ctlClient, c statusCLI, ms []ctlMount, arg string) error {
	abs, err := filepath.Abs(arg)
	if err != nil {
		return err
	}
	var m *ctlMount
	for i := range ms {
		mp := filepath.Clean(ms[i].Mountpoint)
		if abs == mp || strings.HasPrefix(abs, mp+string(filepath.Separator)) {
			if m == nil || len(mp) > len(m.Mountpoint) {
				m = &ms[i]
			}
		}
	}
	if m == nil {
		return errors.New("not inside any mount this drivel serves")
	}
	rel, err := filepath.Rel(filepath.Clean(m.Mountpoint), abs)
	if err != nil {
		return err
	}
	rel = filepath.ToSlash(rel)
	if rel == "." {
		rel = ""
	}
	base := "/v1/mounts/" + url.PathEscape(m.Name)
	q := url.Values{"path": {rel}}

	var self ctlPath
	var rawSelf json.RawMessage
	if err := cl.do(http.MethodGet, base+"/status", q, &rawSelf); err != nil {
		return err
	}
	if err := json.Unmarshal(rawSelf, &self); err != nil {
		return err
	}
	if self.Kind != "dir" || c.self {
		if c.asJSON {
			_, err := os.Stdout.Write(rawSelf)
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		printPath(tw, arg, self)
		return tw.Flush()
	}

	// A directory: list it. Its own status is not a roll-up of what is inside,
	// and the listing is what answers "is this folder synced?".
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	after, shown := "", 0
	for shown < c.limit {
		page := url.Values{"path": {rel}, "limit": {fmt.Sprint(min(c.limit-shown, 1000))}}
		if after != "" {
			page.Set("after", after)
		}
		if c.asJSON {
			var raw json.RawMessage
			if err := cl.do(http.MethodGet, base+"/children", page, &raw); err != nil {
				return err
			}
			if _, err := os.Stdout.Write(raw); err != nil {
				return err
			}
			var ch ctlChildren
			if err := json.Unmarshal(raw, &ch); err != nil {
				return err
			}
			shown += len(ch.Entries)
			if !ch.Truncated {
				return nil
			}
			after = ch.NextAfter
			continue
		}
		var ch ctlChildren
		if err := cl.do(http.MethodGet, base+"/children", page, &ch); err != nil {
			return err
		}
		for _, e := range ch.Entries {
			name := filepath.Base(e.Path)
			if e.Kind == "dir" {
				name += "/"
			}
			printPath(tw, name, e)
		}
		shown += len(ch.Entries)
		if !ch.Truncated {
			return tw.Flush()
		}
		after = ch.NextAfter
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if !c.asJSON {
		fmt.Fprintf(os.Stderr, "(stopped after %d entries; raise -limit for more)\n", shown)
	}
	return nil
}

func printPath(w io.Writer, label string, p ctlPath) {
	note := p.Detail
	switch {
	case p.Error != "" && p.Status == "retrying":
		note = fmt.Sprintf("attempt %d: %s", p.Attempt, p.Error)
	case p.Error != "":
		note = p.Error
	case len(p.Conflicts) > 0:
		note = "conflict copies: " + strings.Join(p.Conflicts, ", ")
	}
	fmt.Fprintf(w, "%s\t%s\t%s\n", p.Status, label, note)
}

func runPause(args []string) error {
	var c pauseCLI
	_ = pauseFlagSet(&c).Parse(args)
	cl, err := dialControl(c.socket, c.name)
	if err != nil {
		return err
	}
	ms, err := cl.selected(c.name)
	if err != nil {
		return err
	}
	for _, m := range ms {
		var out struct {
			Until string `json:"until"`
		}
		if err := cl.do(http.MethodPost, "/v1/mounts/"+url.PathEscape(m.Name)+"/pause", url.Values{"for": {c.dur.String()}}, &out); err != nil {
			return fmt.Errorf("%s: %w", m.Name, err)
		}
		fmt.Printf("%s: paused until %s\n", m.Name, out.Until)
	}
	return nil
}

func runResume(args []string) error {
	var c ctlFlags
	_ = resumeFlagSet(&c).Parse(args)
	cl, err := dialControl(c.socket, c.name)
	if err != nil {
		return err
	}
	ms, err := cl.selected(c.name)
	if err != nil {
		return err
	}
	for _, m := range ms {
		var out json.RawMessage
		if err := cl.do(http.MethodPost, "/v1/mounts/"+url.PathEscape(m.Name)+"/resume", nil, &out); err != nil {
			return fmt.Errorf("%s: %w", m.Name, err)
		}
		fmt.Printf("%s: resumed\n", m.Name)
	}
	return nil
}

// runStop asks the process to shut down, then waits for the socket to close,
// which is when the drain has finished: the API answers before draining, so a
// caller that needs the uploads done waits for this rather than for the reply.
func runStop(args []string) error {
	var c ctlFlags
	_ = stopFlagSet(&c).Parse(args)
	cl, err := dialControl(c.socket, c.name)
	if err != nil {
		return err
	}
	var out struct {
		DrainTimeoutSeconds float64 `json:"drain_timeout_seconds"`
	}
	if err := cl.do(http.MethodPost, "/v1/shutdown", nil, &out); err != nil {
		return err
	}
	drain := time.Duration(out.DrainTimeoutSeconds * float64(time.Second))
	fmt.Printf("stopping: unmounting and draining queued uploads (up to %s)…\n", drain)
	deadline := time.Now().Add(drain + time.Minute)
	for time.Now().Before(deadline) {
		if !socketAnswers(cl.socket) {
			fmt.Println("stopped")
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("the control socket at %s is still open after %s; the drain may be stuck", cl.socket, drain+time.Minute)
}

// socketAnswers reports whether something is still accepting on the socket.
func socketAnswers(path string) bool {
	d := net.Dialer{Timeout: time.Second}
	conn, err := d.DialContext(context.Background(), "unix", path)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
