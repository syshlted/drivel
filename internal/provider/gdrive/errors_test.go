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
