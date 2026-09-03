package gdrive

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/api/googleapi"

	"github.com/zishmusic/drivel/internal/provider"
)

func TestClassify(t *testing.T) {
	rateLimited := &googleapi.Error{Code: 403}
	rateLimited.Errors = []googleapi.ErrorItem{{Reason: "userRateLimitExceeded"}}

	cases := []struct {
		name string
		err  error
		want bool // provider.IsRetryable(classify(err))
	}{
		{"nil", nil, false},
		{"503 service unavailable", &googleapi.Error{Code: 503}, true},
		{"429 too many requests", &googleapi.Error{Code: 429}, true},
		{"500 internal", &googleapi.Error{Code: 500}, true},
		{"404 not found", &googleapi.Error{Code: 404}, false},
		{"403 forbidden (permission)", &googleapi.Error{Code: 403}, false},
		{"403 rate-limited", rateLimited, true},
		{"context canceled", context.Canceled, false},
		{"context deadline", context.DeadlineExceeded, false},
		{"plain error", errors.New("boom"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := provider.IsRetryable(classify(c.err)); got != c.want {
				t.Fatalf("IsRetryable(classify(%v)) = %v; want %v", c.err, got, c.want)
			}
		})
	}

	// nil must stay nil (not wrapped into a non-nil transientError).
	if classify(nil) != nil {
		t.Fatal("classify(nil) should be nil")
	}
}

// A dead change cursor must classify as expired, not as retryable and not as a
// generic failure: the recovery is a resync (M7b), and no amount of retrying gets
// back changes Drive no longer holds.
func TestIsPageTokenExpired(t *testing.T) {
	badToken := &googleapi.Error{Code: 400}
	badToken.Errors = []googleapi.ErrorItem{{Reason: "invalidPageToken"}}
	badRequest := &googleapi.Error{Code: 400}
	badRequest.Errors = []googleapi.ErrorItem{{Reason: "invalidParameter"}}

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"410 gone", &googleapi.Error{Code: 410}, true},
		{"400 invalid page token", badToken, true},
		{"400 by message", &googleapi.Error{Code: 400, Message: "Invalid page token."}, true},
		{"400 unrelated", badRequest, false},
		{"503", &googleapi.Error{Code: 503}, false},
		{"404", &googleapi.Error{Code: 404}, false},
		{"plain error", errors.New("boom"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isPageTokenExpired(c.err); got != c.want {
				t.Fatalf("isPageTokenExpired(%v) = %v; want %v", c.err, got, c.want)
			}
		})
	}
}

// End to end: a 410 from changes.list surfaces above the seam as
// provider.ErrCursorExpired, which is what makes the pull loop resync instead of
// retrying a token forever.
func TestChangesReportsExpiredCursor(t *testing.T) {
	d, fake := newFakeDrive(t)
	fake.expirePageTokens = true

	_, _, err := d.Changes(ctx, "stale-token")
	if !errors.Is(err, provider.ErrCursorExpired) {
		t.Fatalf("Changes = %v; want provider.ErrCursorExpired", err)
	}
	if provider.IsRetryable(err) {
		t.Fatal("an expired cursor was classified as retryable; the loop would retry a token that can never answer")
	}
}
