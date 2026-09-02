package provider

import (
	"errors"
	"fmt"
	"testing"
)

type classified struct{ retry bool }

func (classified) Error() string     { return "classified" }
func (e classified) Retryable() bool { return e.retry }

func TestIsRetryable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain error", errors.New("boom"), false},
		{"retryable", classified{retry: true}, true},
		{"non-retryable", classified{retry: false}, false},
		{"wrapped retryable", fmt.Errorf("context: %w", classified{retry: true}), true},
		{"wrapped non-retryable", fmt.Errorf("context: %w", classified{retry: false}), false},
		{"ErrNotExist is permanent", ErrNotExist, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsRetryable(c.err); got != c.want {
				t.Fatalf("IsRetryable(%v) = %v; want %v", c.err, got, c.want)
			}
		})
	}
}
