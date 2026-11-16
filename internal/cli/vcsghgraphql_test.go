package cli

import (
	"slices"
	"strings"
	"testing"
)

func TestGhRunViaGraphQL(t *testing.T) {
	checkout := func() string { return "o/checkout" }
	tests := []struct {
		name      string
		argv      []string
		rewritten bool
		query     []string
		jq        []string
	}{
		{
			name: "run view", argv: []string{"run", "view", "123", "-R", "o/r", "--json", "status,conclusion"}, rewritten: true,
			query: []string{`repository(owner: "o", name: "r")`, `resource(url: "https://github.com/o/r/actions/runs/123")`},
			jq:    []string{`status: (.checkSuite.status // "" | ascii_downcase)`, `conclusion: (.checkSuite.conclusion // "" | ascii_downcase)`},
		},
		{
			name: "run view in the checkout with a jq", argv: []string{"run", "view", "123", "--json", "status", "-q", ".status"}, rewritten: true,
			query: []string{`repository(owner: "o", name: "checkout")`}, jq: []string{`| .status`},
		},
		{
			name: "run list on a commit", argv: []string{"run", "list", "--repo=o/r", "--commit", strings.Repeat("a", 40), "--limit", "5", "--json", "databaseId,workflowName,status,url", "--workflow", "CI"}, rewritten: true,
			query: []string{`object(expression: "` + strings.Repeat("a", 40) + `")`, `checkSuites(first: 100)`}, jq: []string{`select(.workflow.name == "CI")`, `.[:5]`},
		},
		{name: "run view log", argv: []string{"run", "view", "123", "--log-failed"}},
		{name: "run view jobs", argv: []string{"run", "view", "123", "--json", "status,jobs"}},
		{name: "run view without json", argv: []string{"run", "view", "123"}},
		{name: "run view template", argv: []string{"run", "view", "123", "--json", "status", "-t", "{{.status}}"}},
		{name: "run list on a branch", argv: []string{"run", "list", "-b", "main", "--json", "status"}},
		{name: "run list by workflow file", argv: []string{"run", "list", "-c", strings.Repeat("a", 40), "-w", "ci.yml", "--json", "status"}},
		{name: "run watch", argv: []string{"run", "watch", "123"}},
		{name: "pr view", argv: []string{"pr", "view", "1", "--json", "state"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ghRunViaGraphQL(tt.argv, checkout)
			if !tt.rewritten {
				if !slices.Equal(got, tt.argv) {
					t.Fatalf("ghRunViaGraphQL(%q) = %q, want it unchanged", tt.argv, got)
				}
				return
			}
			if len(got) != 6 || got[0] != "api" || got[1] != "graphql" || got[2] != "-f" || got[4] != "--jq" {
				t.Fatalf("ghRunViaGraphQL(%q) = %q, want a gh api graphql -f query=... --jq ... call", tt.argv, got)
			}
			for _, want := range tt.query {
				if !strings.Contains(got[3], want) {
					t.Errorf("query %q lacks %q", got[3], want)
				}
			}
			for _, want := range tt.jq {
				if !strings.Contains(got[5], want) {
					t.Errorf("jq %q lacks %q", got[5], want)
				}
			}
			if repo := ghGraphQLRepo(got); repo == "" {
				t.Errorf("ghGraphQLRepo(%q) = \"\", want the read's repository so the app token and cache scope it", got)
			}
		})
	}
}
