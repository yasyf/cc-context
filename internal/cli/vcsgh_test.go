package cli

import "testing"

func TestGhRepoFlag(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		argv []string
		want string
	}{
		{name: "none", argv: []string{"pr", "checks", "12"}},
		{name: "long", argv: []string{"pr", "view", "12", "--repo", "o/r"}, want: "o/r"},
		{name: "long equals", argv: []string{"pr", "view", "--repo=o/r", "12"}, want: "o/r"},
		{name: "short", argv: []string{"-R", "o/r", "pr", "view", "12"}, want: "o/r"},
		{name: "short joined", argv: []string{"pr", "view", "-Ro/r"}, want: "o/r"},
		{name: "short equals", argv: []string{"pr", "view", "-R=o/r"}, want: "o/r"},
		{name: "host", argv: []string{"run", "list", "--repo", "github.com/o/r"}, want: "o/r"},
		{name: "after terminator", argv: []string{"api", "graphql", "--", "--repo", "o/r"}},
		{name: "dangling", argv: []string{"pr", "view", "--repo"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := ghRepoFlag(tt.argv); got != tt.want {
				t.Errorf("ghRepoFlag(%q) = %q, want %q", tt.argv, got, tt.want)
			}
		})
	}
}
