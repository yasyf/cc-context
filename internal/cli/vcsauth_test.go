package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/ghapi"
)

func TestWriteAuth(t *testing.T) {
	t.Parallel()
	reset := time.Date(2026, 10, 1, 20, 0, 0, 0, time.Local)
	user := ghapi.Identity{Kind: "user", Name: "octocat", Quota: &ghapi.Quota{Limit: 5000, Remaining: 4000, ResetAt: reset}}
	app := ghapi.Identity{Kind: "app", Name: "vulcan", Installation: 7, ExpiresAt: &reset, Quota: &ghapi.Quota{Limit: 10000, Remaining: 9999, ResetAt: reset}}
	tests := []struct {
		name string
		auth ghapi.Auth
		want string
	}{
		{
			name: "reads through the app",
			auth: ghapi.Auth{Repo: "o/r", Reads: app, Writes: user},
			want: `o/r
  reads   app vulcan (installation 7), token valid until 8:00PM
  writes  user octocat
  quota   app vulcan: 9999/10000 GraphQL left, resets 8:00PM
          user octocat: 4000/5000 GraphQL left, resets 8:00PM
`,
		},
		{
			name: "reads on the user",
			auth: ghapi.Auth{Repo: "o/elsewhere", Reads: user, Writes: user, Reason: "vulcan is not installed on o/elsewhere"},
			want: `o/elsewhere
  reads   user octocat (vulcan is not installed on o/elsewhere)
  writes  user octocat
  quota   user octocat: 4000/5000 GraphQL left, resets 8:00PM
`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var out strings.Builder
			writeAuth(&out, tt.auth)
			if out.String() != tt.want {
				t.Errorf("writeAuth =\n%s\nwant\n%s", out.String(), tt.want)
			}
		})
	}
}
