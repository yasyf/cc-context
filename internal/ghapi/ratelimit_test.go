package ghapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"
)

func TestRetryDelay(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		status int
		header map[string]string
		want   time.Duration
		wantOK bool
	}{
		{name: "200 never waits", status: http.StatusOK, header: map[string]string{"Retry-After": "5"}},
		{name: "403 without limit headers", status: http.StatusForbidden},
		{name: "429 with Retry-After seconds", status: http.StatusTooManyRequests, header: map[string]string{"Retry-After": "5"}, want: 5 * time.Second, wantOK: true},
		{name: "403 with Retry-After seconds", status: http.StatusForbidden, header: map[string]string{"Retry-After": "30"}, want: 30 * time.Second, wantOK: true},
		{name: "Retry-After beyond the cap", status: http.StatusTooManyRequests, header: map[string]string{"Retry-After": "61"}},
		{
			name:   "Retry-After as an HTTP date",
			status: http.StatusTooManyRequests,
			header: map[string]string{"Retry-After": now.Add(20 * time.Second).Format(http.TimeFormat)},
			want:   20 * time.Second,
			wantOK: true,
		},
		{
			name:   "exhausted quota resetting soon",
			status: http.StatusForbidden,
			header: map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": epoch(now.Add(45 * time.Second))},
			want:   45 * time.Second,
			wantOK: true,
		},
		{
			name:   "exhausted quota resetting in an hour",
			status: http.StatusForbidden,
			header: map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": epoch(now.Add(time.Hour))},
		},
		{
			name:   "quota left is a permission 403, not a rate limit",
			status: http.StatusForbidden,
			header: map[string]string{"X-RateLimit-Remaining": "4999", "X-RateLimit-Reset": epoch(now.Add(10 * time.Second))},
		},
		{
			name:   "reset already passed waits not at all",
			status: http.StatusForbidden,
			header: map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": epoch(now.Add(-time.Minute))},
			wantOK: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			header := http.Header{}
			for k, v := range tt.header {
				header.Set(k, v)
			}
			got, ok := retryDelay(tt.status, header, now)
			if ok != tt.wantOK || got != tt.want {
				t.Errorf("retryDelay = (%s, %v), want (%s, %v)", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestSleepHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleep(ctx, time.Minute); !errors.Is(err, context.Canceled) {
		t.Errorf("sleep = %v, want context.Canceled", err)
	}
	if err := sleep(ctx, 0); err != nil {
		t.Errorf("sleep(0) = %v, want nil", err)
	}
}

func epoch(t time.Time) string {
	return strconv.FormatInt(t.Unix(), 10)
}
