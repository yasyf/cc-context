package nativeapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func trunkClient(answer func(ask int) *http.Response) (*Client, *int) {
	asks := 0
	client := New("http://github.invalid").WithQueueProvider("trunk")
	client.queueBase = "http://trunk.invalid"
	client.http = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		asks++
		return answer(asks), nil
	})}
	return client, &asks
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func trunkAnswer(status int, retryAfter string) *http.Response {
	header := http.Header{}
	if retryAfter != "" {
		header.Set("Retry-After", retryAfter)
	}
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(`{"ok":true}`))}
}

func TestTrunkReadWaitsOutARateLimit(t *testing.T) {
	t.Setenv("TRUNK_MERGE_API_KEY", "trunk-key")
	cases := []struct {
		name       string
		retryAfter func() string
		want       time.Duration
	}{
		{"Retry-After in seconds", func() string { return "7" }, 7 * time.Second},
		{"Retry-After as a date", func() string { return time.Now().Add(20 * time.Second).UTC().Format(http.TimeFormat) }, 20 * time.Second},
		{"no Retry-After takes the floor", func() string { return "" }, time.Second},
		{"Retry-After below the floor takes the floor", func() string { return "0" }, time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				client, asks := trunkClient(func(ask int) *http.Response {
					if ask == 1 {
						return trunkAnswer(http.StatusTooManyRequests, tc.retryAfter())
					}
					return trunkAnswer(http.StatusOK, "")
				})
				start := time.Now()
				var out struct{ OK bool }
				if err := client.queueGet(t.Context(), "/v2/pull-requests", nil, &out); err != nil {
					t.Fatal(err)
				}
				elapsed := time.Since(start)
				if *asks != 2 || !out.OK || elapsed < tc.want || elapsed > tc.want*5/4 {
					t.Fatalf("asks=%d out=%+v elapsed=%s, want 2 asks and a wait of %s plus at most a quarter", *asks, out, elapsed, tc.want)
				}
			})
		})
	}
}

func TestTrunkReadGivesUpOnALimitPastItsCeiling(t *testing.T) {
	t.Setenv("TRUNK_MERGE_API_KEY", "trunk-key")
	cases := []struct {
		name                 string
		retryAfter           string
		minAsks, maxAsks     int
		minWaited, maxWaited time.Duration
	}{
		{"a persistent limit stops after five waits", "", 6, 6, 31 * time.Second, 39 * time.Second},
		{"a long Retry-After stops inside the ceiling", "50", 2, 3, 50 * time.Second, trunkLimitPatience},
		{"a Retry-After past the ceiling fails at once", "3600", 1, 1, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				client, asks := trunkClient(func(int) *http.Response {
					return trunkAnswer(http.StatusTooManyRequests, tc.retryAfter)
				})
				start := time.Now()
				err := client.queueGet(t.Context(), "/v2/pull-requests", nil, &struct{}{})
				elapsed := time.Since(start)
				var refused *StatusError
				if !errors.As(err, &refused) || refused.Status != http.StatusTooManyRequests {
					t.Fatalf("err=%v, want the 429", err)
				}
				if *asks < tc.minAsks || *asks > tc.maxAsks || elapsed < tc.minWaited || elapsed > tc.maxWaited {
					t.Fatalf("asks=%d elapsed=%s, want %d-%d asks in %s-%s", *asks, elapsed, tc.minAsks, tc.maxAsks, tc.minWaited, tc.maxWaited)
				}
			})
		})
	}
}

func TestTrunkReadStopsWaitingWhenItsContextEnds(t *testing.T) {
	t.Setenv("TRUNK_MERGE_API_KEY", "trunk-key")
	synctest.Test(t, func(t *testing.T) {
		client, asks := trunkClient(func(int) *http.Response {
			return trunkAnswer(http.StatusTooManyRequests, "60")
		})
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		start := time.Now()
		err := client.queueGet(ctx, "/v2/pull-requests", nil, &struct{}{})
		var refused *StatusError
		if !errors.Is(err, context.DeadlineExceeded) || !errors.As(err, &refused) || *asks != 1 || time.Since(start) != 5*time.Second {
			t.Fatalf("err=%v asks=%d elapsed=%s, want the deadline and the 429 after one ask and 5s", err, *asks, time.Since(start))
		}
	})
}

func TestTrunkReadNeverWaitsOnAnotherRefusal(t *testing.T) {
	t.Setenv("TRUNK_MERGE_API_KEY", "trunk-key")
	synctest.Test(t, func(t *testing.T) {
		client, asks := trunkClient(func(int) *http.Response {
			return trunkAnswer(http.StatusForbidden, "7")
		})
		start := time.Now()
		err := client.queueGet(t.Context(), "/v2/pull-requests", nil, &struct{}{})
		var refused *StatusError
		if !errors.As(err, &refused) || refused.Status != http.StatusForbidden || *asks != 1 || time.Since(start) != 0 {
			t.Fatalf("err=%v asks=%d elapsed=%s, want the 403 at once", err, *asks, time.Since(start))
		}
	})
}
