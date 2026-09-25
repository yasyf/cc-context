package ghapi

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/yasyf/cc-context/internal/render"
)

// gh stubs: the argv guard makes every non-`auth token` call a failure, so the
// tests assert the subprocess arm's arguments as well as its output.
const (
	ghArgvGuard = "#!/bin/sh\nif [ \"$1 $2\" != 'auth token' ]; then echo \"unexpected argv: $*\" >&2; exit 2; fi\n"
	ghPrints    = ghArgvGuard + "printf 'from-gh\\n'\n"
	ghSilent    = ghArgvGuard + "exit 0\n"
	ghFails     = ghArgvGuard + "echo 'gh: not logged in' >&2\nexit 1\n"
)

// stubGH writes script as a gh executable in a fresh directory and returns that
// directory, to be carried as PATH on the context under test. An empty script
// leaves the directory empty, reporting gh absent.
func stubGH(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	if script != "" {
		if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil { //nolint:gosec // test-only stub must be executable
			t.Fatalf("write gh stub: %v", err)
		}
	}
	return dir
}

func TestResolveToken(t *testing.T) {
	tests := []struct {
		name      string
		ghEnv     string
		githubEnv string
		gh        string
		want      string
		wantErr   error
	}{
		{name: "GH_TOKEN wins over GITHUB_TOKEN", ghEnv: "from-gh-env", githubEnv: "from-github-env", gh: ghPrints, want: "from-gh-env"},
		{name: "GITHUB_TOKEN when GH_TOKEN is unset", githubEnv: "from-github-env", gh: ghPrints, want: "from-github-env"},
		{name: "gh auth token when neither env var is set", gh: ghPrints, want: "from-gh"},
		{name: "env token is trimmed", ghEnv: " padded \n", gh: ghPrints, want: "padded"},
		{name: "no env and no gh", wantErr: ErrNoToken},
		{name: "gh auth token fails", gh: ghFails, wantErr: ErrNoToken},
		{name: "gh auth token prints nothing", gh: ghSilent, wantErr: ErrNoToken},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := render.WithEnv(t.Context(),
				"GH_TOKEN="+tt.ghEnv,
				"GITHUB_TOKEN="+tt.githubEnv,
				"PATH="+stubGH(t, tt.gh))

			got, err := resolveToken(ctx)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("resolveToken error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveToken: %v", err)
			}
			if got != tt.want {
				t.Errorf("resolveToken = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTokenSourceCaches(t *testing.T) {
	calls := 0
	s := &tokenSource{resolve: func(context.Context) (string, error) {
		calls++
		return fmt.Sprintf("token-%d", calls), nil
	}}

	for i := range 3 {
		got, err := s.get(context.Background())
		if err != nil {
			t.Fatalf("get %d: %v", i, err)
		}
		if got != "token-1" {
			t.Fatalf("get %d = %q, want token-1", i, got)
		}
	}
	if calls != 1 {
		t.Errorf("resolutions = %d, want 1", calls)
	}

	if err := s.refresh(context.Background(), "token-1"); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if calls != 2 {
		t.Errorf("resolutions after refresh = %d, want 2", calls)
	}
	got, err := s.get(context.Background())
	if err != nil {
		t.Fatalf("get after refresh: %v", err)
	}
	if got != "token-2" {
		t.Errorf("get after refresh = %q, want token-2", got)
	}

	if err := s.refresh(context.Background(), "token-1"); err != nil {
		t.Fatalf("refresh with a stale token another caller already replaced: %v", err)
	}
	if calls != 2 {
		t.Errorf("resolutions after a redundant refresh = %d, want 2", calls)
	}
}
