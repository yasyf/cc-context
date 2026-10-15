package prstate

import (
	"errors"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/ghapi"
)

func TestUserFallbackReadsAsTheUserOnlyAboveTheFloor(t *testing.T) {
	now := time.Date(2026, 10, 15, 11, 30, 0, 0, time.UTC)
	refusal := &ghapi.StatusError{Method: "GET", URL: "https://api.github.com/repos/o/r/pulls/1", Status: 403, Message: "API rate limit exceeded for installation ID 140742353", Limited: true, RetryAfter: 36*time.Minute + 48*time.Second, AsApp: true}
	userReset := time.Date(2026, 10, 15, 12, 15, 0, 0, time.UTC)
	tests := []struct {
		name      string
		remaining int
		want      string
	}{
		{name: "budget left", remaining: 4941},
		{name: "one over the floor", remaining: rateFloor + 1},
		{
			name:      "at the floor",
			remaining: rateFloor,
			want:      "the app installation's REST quota is spent until 2026-10-15T12:06:48Z and the user token has 100 requests left, at or under the floor of 100, until 2026-10-15T12:15:00Z: ghapi: GET https://api.github.com/repos/o/r/pulls/1: 403 API rate limit exceeded for installation ID 140742353",
		},
		{
			name:      "spent",
			remaining: 0,
			want:      "the app installation's REST quota is spent until 2026-10-15T12:06:48Z and the user token has 0 requests left, at or under the floor of 100, until 2026-10-15T12:15:00Z: ghapi: GET https://api.github.com/repos/o/r/pulls/1: 403 API rate limit exceeded for installation ID 140742353",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := userFallback(refusal, ghapi.Quota{Limit: 5000, Remaining: tt.remaining, ResetAt: userReset}, now)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("userFallback = %v, want the read retried as the user", err)
				}
				return
			}
			if err == nil || err.Error() != tt.want {
				t.Fatalf("userFallback = %v, want %q", err, tt.want)
			}
			if !errors.Is(err, refusal) {
				t.Errorf("userFallback = %v, want it to wrap the app's refusal", err)
			}
		})
	}
}
