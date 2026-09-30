package ghapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
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
	wait, ok := limitWait(status, header, now)
	if !ok || wait > maxRateLimitWait {
		return 0, false
	}
	return wait, true
}

// limitWait reads the wait a 403/429 names: its Retry-After, else its exhausted
// quota's reset. It reports false when the response names no wait at all.
func limitWait(status int, header http.Header, now time.Time) (time.Duration, bool) {
	if status != http.StatusForbidden && status != http.StatusTooManyRequests {
		return 0, false
	}
	if after := strings.TrimSpace(header.Get("Retry-After")); after != "" {
		if seconds, err := strconv.Atoi(after); err == nil {
			return max(time.Duration(seconds)*time.Second, 0), true
		}
		if when, err := http.ParseTime(after); err == nil {
			return max(when.Sub(now), 0), true
		}
	}
	if header.Get("X-RateLimit-Remaining") == "0" {
		if reset, err := strconv.ParseInt(header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			return max(time.Unix(reset, 0).Sub(now), 0), true
		}
	}
	return 0, false
}

// RateLimited reports whether err is GitHub refusing a request for rate rather
// than failing it, and the wait GitHub named, zero when it named none.
func RateLimited(err error) (time.Duration, bool) {
	var status *StatusError
	if errors.As(err, &status) {
		return status.RetryAfter, status.Limited
	}
	var gql *GraphQLError
	if errors.As(err, &gql) {
		return 0, slices.ContainsFunc(gql.Messages, func(m GraphQLMessage) bool { return m.Type == "RATE_LIMITED" })
	}
	return 0, false
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
