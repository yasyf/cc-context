package ghapi

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// maxRateLimitWait caps how long a rate-limited request waits before retrying.
// A reset further out than this is not waited on at all: the error goes back to
// the caller, whose own cadence — a 30s review poll — decides what an hour-long
// primary-limit reset means. Only a secondary limit, which GitHub answers with a
// Retry-After of seconds, is short enough to sit out inside one call.
const maxRateLimitWait = 60 * time.Second

// maxRateLimitRetries bounds the waits one request may sit through, so a server
// answering 429 forever fails instead of looping.
const maxRateLimitRetries = 3

// retryDelay reports how long to wait before resending, and whether waiting is
// worth it: only a 403/429 whose Retry-After — or whose exhausted-quota reset —
// lands within maxRateLimitWait is waited on.
func retryDelay(status int, header http.Header, now time.Time) (time.Duration, bool) {
	if status != http.StatusForbidden && status != http.StatusTooManyRequests {
		return 0, false
	}
	if after := strings.TrimSpace(header.Get("Retry-After")); after != "" {
		if seconds, err := strconv.Atoi(after); err == nil {
			return boundedWait(time.Duration(seconds) * time.Second)
		}
		if when, err := http.ParseTime(after); err == nil {
			return boundedWait(when.Sub(now))
		}
	}
	if header.Get("X-RateLimit-Remaining") == "0" {
		if reset, err := strconv.ParseInt(header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			return boundedWait(time.Unix(reset, 0).Sub(now))
		}
	}
	return 0, false
}

func boundedWait(d time.Duration) (time.Duration, bool) {
	if d > maxRateLimitWait {
		return 0, false
	}
	return max(d, 0), true
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("ghapi: waiting out rate limit: %w", ctx.Err())
	case <-timer.C:
		return nil
	}
}
