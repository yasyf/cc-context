package cli

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

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

func TestGhReads(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		argv []string
		want bool
	}{
		{name: "pr checks", argv: []string{"pr", "checks", "12", "--watch"}, want: true},
		{name: "run watch", argv: []string{"run", "watch", "34", "--exit-status"}, want: true},
		{name: "repo view", argv: []string{"repo", "view", "--json", "name"}, want: true},
		{name: "search", argv: []string{"search", "prs", "--author", "@me"}, want: true},
		{name: "api get", argv: []string{"api", "repos/o/r/pulls/1", "--jq", ".state"}, want: true},
		{name: "api named get", argv: []string{"api", "-X", "GET", "search/issues", "-f", "q=is:open"}, want: true},
		{name: "api lowercase get", argv: []string{"api", "--method=get", "repos/o/r"}, want: true},
		{name: "api header", argv: []string{"api", "-H", "Accept: application/vnd.github.raw", "repos/o/r/readme"}, want: true},
		{name: "pr merge", argv: []string{"pr", "merge", "12", "--squash"}},
		{name: "pr comment", argv: []string{"pr", "comment", "12", "--body", "hi"}},
		{name: "run rerun", argv: []string{"run", "rerun", "34"}},
		{name: "bare group", argv: []string{"pr"}},
		{name: "leading flag", argv: []string{"-R", "o/r", "pr", "view"}},
		{name: "empty"},
		{name: "api put", argv: []string{"api", "-X", "PUT", "repos/o/r/pulls/1/merge", "-f", "merge_method=merge"}},
		{name: "api field implies post", argv: []string{"api", "repos/o/r/pulls/1/merge", "-f", "merge_method=merge"}},
		{name: "api long method", argv: []string{"api", "--method", "PUT", "repos/o/r/pulls/1/merge"}},
		{name: "api method equals", argv: []string{"api", "--method=PUT", "repos/o/r/pulls/1/merge"}},
		{name: "api glued method", argv: []string{"api", "-XPUT", "repos/o/r/pulls/1/merge"}},
		{name: "api method after path", argv: []string{"api", "repos/o/r/pulls/1/merge", "-X", "PUT"}},
		{name: "api delete", argv: []string{"api", "-X", "DELETE", "repos/o/r/git/refs/heads/b"}},
		{name: "api glued field", argv: []string{"api", "repos/o/r/issues/1/comments", "-fbody=hi"}},
		{name: "api typed field", argv: []string{"api", "repos/o/r/issues/1/labels", "-F", "labels[]=bug"}},
		{name: "api long field", argv: []string{"api", "repos/o/r/issues/1/comments", "--raw-field=body=hi"}},
		{name: "api input", argv: []string{"api", "repos/o/r/pulls/1/merge", "--input", "body.json"}},
		{name: "api graphql", argv: []string{"api", "graphql", "-f", "query={ viewer { login } }"}},
		{name: "api graphql get", argv: []string{"api", "-X", "GET", "graphql", "-f", "query={ viewer { login } }"}},
		{name: "api graphql after terminator", argv: []string{"api", "--", "graphql"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := ghReads(tt.argv); got != tt.want {
				t.Errorf("ghReads(%q) = %v, want %v", tt.argv, got, tt.want)
			}
		})
	}
}

func TestRunVcsGhRefusesWrite(t *testing.T) {
	t.Parallel()
	err := runVcsGh(&cobra.Command{}, []string{"api", "-X", "PUT", "repos/o/r/pulls/1/merge", "-f", "merge_method=merge"})
	if err == nil || !strings.HasSuffix(err.Error(), ": gh api -X PUT repos/o/r/pulls/1/merge -f merge_method=merge") {
		t.Fatalf("runVcsGh(write) = %v, want a refusal naming the plain gh command", err)
	}
}
