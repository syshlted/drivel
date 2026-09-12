package app

import (
	"bytes"
	"log"
	"strings"
	"sync"
	"testing"

	"github.com/zishmusic/drivel/provider"
)

// safeBuffer is a bytes.Buffer that survives -race: the log package writes from
// whichever goroutine reached it.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLog redirects the default logger for the duration of a test.
func captureLog(t *testing.T) *safeBuffer {
	t.Helper()
	var buf safeBuffer
	oldWriter, oldFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(oldWriter)
		log.SetFlags(oldFlags)
	})
	return &buf
}

// With more than one mount, every line has to say which mount it came from.
func TestSeveralMountsAttributeTheirLogs(t *testing.T) {
	buf := captureLog(t)
	dir := t.TempDir()

	a, err := New(t.Context(), []MountSpec{spec(dir, "alpha"), spec(dir, "beta")}, provider.NewRegistry())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close() //nolint:errcheck // not what this test is about

	out := buf.String()
	for _, want := range []string{"[alpha] ", "[beta] "} {
		if !strings.Contains(out, want) {
			t.Errorf("log is missing the %q prefix:\n%s", want, out)
		}
	}
	// Every non-empty line must be attributed, or the unattributed ones are
	// exactly the ones you cannot act on.
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "[alpha] ") && !strings.HasPrefix(line, "[beta] ") {
			t.Errorf("unattributed log line: %q", line)
		}
	}
}

// A single mount keeps the bare default logger, so its output is exactly what
// drivel printed before several mounts were possible.
func TestOneMountIsNotPrefixed(t *testing.T) {
	buf := captureLog(t)
	dir := t.TempDir()

	a, err := New(t.Context(), []MountSpec{spec(dir, "solo")}, provider.NewRegistry())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close() //nolint:errcheck // not what this test is about

	if out := buf.String(); strings.Contains(out, "[solo]") {
		t.Errorf("single mount gained a prefix:\n%s", out)
	}
}

// The prefix comes from the mount's name, and falls back to the mountpoint's
// last component — never the whole absolute path, which would bury the message.
func TestLogNameFallsBackToBasename(t *testing.T) {
	s := MountSpec{Mountpoint: "/very/long/path/to/a/mountpoint"}
	if got := s.logName(); got != "mountpoint" {
		t.Errorf("logName() = %q; want the basename", got)
	}
	s.Name = "chosen"
	if got := s.logName(); got != "chosen" {
		t.Errorf("logName() = %q; want the name", got)
	}
}
