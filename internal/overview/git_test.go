package overview

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// stubGit installs a canned git runner keyed by the space-joined argv and restores
// the real runner when the test ends.
func stubGit(t *testing.T, transcript map[string]string) {
	t.Helper()
	prev := git
	t.Cleanup(func() { git = prev })
	git = func(_ context.Context, _ string, args ...string) (string, error) {
		key := strings.Join(args, " ")
		out, ok := transcript[key]
		if !ok {
			return "", fmt.Errorf("no canned output for %q", key)
		}
		return out, nil
	}
}

func TestGitSection(t *testing.T) {
	tests := []struct {
		name       string
		transcript map[string]string
		want       string
	}{
		{
			name: "branch, dirty, commits",
			transcript: map[string]string{
				"log -1 --format=%h%x00%s":    "a1b2c3d\x00release: v0.22.0\n",
				"rev-parse --abbrev-ref HEAD": "main\n",
				"status --porcelain -z":       " M a.go\x00?? b.txt\x00",
				"rev-list --count HEAD":       "1240\n",
			},
			want: `git: main @ a1b2c3d "release: v0.22.0" · 2 dirty · 1240 commits`,
		},
		{
			name: "detached HEAD drops branch",
			transcript: map[string]string{
				"log -1 --format=%h%x00%s":    "deadbee\x00wip\n",
				"rev-parse --abbrev-ref HEAD": "HEAD\n",
				"status --porcelain -z":       "",
				"rev-list --count HEAD":       "5\n",
			},
			want: `git: @ deadbee "wip" · 5 commits`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stubGit(t, tt.transcript)
			if got := gitSection(context.Background(), "/repo"); got != tt.want {
				t.Errorf("gitSection = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGitSectionNoCommits(t *testing.T) {
	prev := git
	t.Cleanup(func() { git = prev })
	git = func(_ context.Context, _ string, _ ...string) (string, error) {
		return "", fmt.Errorf("fatal: your current branch does not have any commits yet")
	}
	if got := gitSection(context.Background(), "/repo"); got != "" {
		t.Errorf("gitSection with no commits = %q, want \"\"", got)
	}
}

func TestCountPorcelain(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want int
	}{
		{"empty", "", 0},
		{"single modified", " M a.go\x00", 1},
		{"staged and untracked", "M  a\x00?? b\x00", 2},
		{"rename skips origin path", "R  new\x00old\x00 M other\x00", 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := countPorcelain(tt.out); got != tt.want {
				t.Errorf("countPorcelain(%q) = %d, want %d", tt.out, got, tt.want)
			}
		})
	}
}

func TestHotLine(t *testing.T) {
	stubGit(t, map[string]string{
		"log --since=90.days --name-only --format=": "internal/cli/a.go\ninternal/cli/b.go\n\ninternal/web/c.go\ncmd/ccx/main.go\nREADME.md\n",
	})
	// internal/cli leads at 2; cmd/ccx and internal/web tie at 1 → name-ascending;
	// the root-level README.md is not attributable to a dir and is dropped.
	want := "hot (90d): internal/cli (2), cmd/ccx (1), internal/web (1)"
	if got := hotLine(context.Background(), "/repo"); got != want {
		t.Errorf("hotLine = %q, want %q", got, want)
	}
}

func TestHotKey(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"internal/cli/foo.go", "internal/cli"},
		{"internal/cli/sub/foo.go", "internal/cli"},
		{"cmd/ccx/main.go", "cmd/ccx"},
		{"docs/x.md", "docs"},
		{"README.md", ""},
		{"./internal/web/a.go", "internal/web"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := hotKey(tt.path); got != tt.want {
				t.Errorf("hotKey(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}
