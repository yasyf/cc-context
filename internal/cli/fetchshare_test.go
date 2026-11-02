package cli

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcstest"
)

func TestSharedFetchServesOnlyAFetchBegunInsideTheWindow(t *testing.T) {
	tests := []struct {
		name      string
		stamp     time.Duration
		maxAge    time.Duration
		wantFetch bool
	}{
		{"no stamp fetches", 0, 0, true},
		{"a fetch begun before the call is too old at zero max age", -time.Second, 0, true},
		{"a fetch begun after the call answers it", time.Hour, 0, false},
		{"a fetch inside max age answers it", -10 * time.Second, 30 * time.Second, false},
		{"a fetch past max age is fetched again", -time.Minute, 30 * time.Second, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := vcstest.Repo(t)
			common := strings.TrimSpace(f.Out(t, "git", "-C", f.Dir, "rev-parse", "--path-format=absolute", "--git-common-dir"))
			stamp := filepath.Join(common, sharedFetchDir, fmt.Sprintf("%x", sha256.Sum256([]byte("origin\x00main"))))
			if tt.stamp != 0 {
				if err := os.MkdirAll(filepath.Dir(stamp), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(stamp, []byte(time.Now().Add(tt.stamp).UTC().Format(time.RFC3339Nano)+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			before := time.Now()
			fetched, err := sharedFetch(f.Context(), render.Dir(f.Dir), "fetch", "origin", "main", tt.maxAge, func() error {
				calls++
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if fetched != tt.wantFetch || fetched != (calls == 1) || calls > 1 {
				t.Fatalf("fetched = %v after %d fetches, want %v", fetched, calls, tt.wantFetch)
			}
			if !tt.wantFetch {
				return
			}
			data, err := os.ReadFile(stamp) //nolint:gosec // the test's own temp repository
			if err != nil {
				t.Fatal(err)
			}
			began, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(data)))
			if err != nil || began.Before(before) {
				t.Fatalf("stamp = %q (%v), want a time at or after %v", data, err, before)
			}
		})
	}
}

func TestSharedFetchLeavesNoStampWhenTheFetchFails(t *testing.T) {
	f := vcstest.Repo(t)
	boom := errors.New("remote hung up")
	_, err := sharedFetch(f.Context(), render.Dir(f.Dir), "fetch", "origin", "main", 0, func() error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	fetched, err := sharedFetch(f.Context(), render.Dir(f.Dir), "fetch", "origin", "main", time.Hour, func() error { return nil })
	if err != nil || !fetched {
		t.Fatalf("after a failed fetch: fetched = %v, err = %v, want a fresh fetch", fetched, err)
	}
}
