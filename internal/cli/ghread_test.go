package cli

import (
	"errors"
	"strconv"
	"testing"
	"time"
)

func TestARateLimitedReadWaitsForTheCoreReset(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	if got := rateLimitUntil(now, strconv.Itoa(1_000_600)); !got.Equal(time.Unix(1_000_602, 0)) {
		t.Fatalf("until %v, want the reset plus two seconds", got)
	}
	if got := rateLimitUntil(now, ""); !got.Equal(now.Add(ghRateLimitWait)) {
		t.Fatalf("until %v, want the fixed wait when the reset is unknown", got)
	}
	if got := rateLimitUntil(now, strconv.Itoa(999_000)); !got.Equal(now.Add(ghRateLimitWait)) {
		t.Fatalf("until %v, want the fixed wait when the reset has passed", got)
	}
}

func TestOnlyARateLimitRefusalIsWaitedOut(t *testing.T) {
	for message, want := range map[string]bool{
		"HTTP 403: API rate limit exceeded for user ID 709645": true,
		"You have exceeded a secondary rate limit":             true,
		"HTTP 404: Not Found":                                  false,
	} {
		if got := ghRateLimited(errors.New(message)); got != want {
			t.Errorf("%q: %v, want %v", message, got, want)
		}
	}
	if ghRateLimited(nil) {
		t.Error("nil is not a rate limit")
	}
}
