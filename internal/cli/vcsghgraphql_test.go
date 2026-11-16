package cli

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/ghapi"
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
		{
			name: "run view printed", argv: []string{"run", "view", "123", "-R", "o/r"}, rewritten: true,
			query: []string{`resource(url: "https://github.com/o/r/actions/runs/123")`, `checkRuns(first: 100) { nodes { databaseId name status conclusion detailsUrl } }`},
			jq:    []string{`def sym:`, `could not find any workflow run with ID 123`, `"JOBS"`, `join("\n")`},
		},
		{name: "run view exit status", argv: []string{"run", "view", "123", "--exit-status"}},
		{name: "run view verbose", argv: []string{"run", "view", "123", "-v"}},
		{name: "run view one job", argv: []string{"run", "view", "--job", "456"}},
		{name: "run view an attempt", argv: []string{"run", "view", "123", "--attempt", "2"}},
		{name: "run view web", argv: []string{"run", "view", "123", "--web"}},
		{name: "run view without an id", argv: []string{"run", "view"}},
		{name: "run view jq without json", argv: []string{"run", "view", "123", "-q", ".status"}},
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

func TestGhParseRunWatch(t *testing.T) {
	tests := []struct {
		name string
		argv []string
		ok   bool
		want ghRunWatch
	}{
		{name: "bare", argv: []string{"run", "watch", "123"}, ok: true, want: ghRunWatch{run: "123", interval: 30 * time.Second}},
		{
			name: "every flag", argv: []string{"run", "watch", "123", "-R", "o/r", "--exit-status", "--compact", "-i", "90"}, ok: true,
			want: ghRunWatch{repo: "o/r", run: "123", interval: 90 * time.Second, exitStatus: true},
		},
		{name: "interval under the floor", argv: []string{"run", "watch", "--interval=3", "123", "--repo=o/r"}, ok: true, want: ghRunWatch{repo: "o/r", run: "123", interval: 30 * time.Second}},
		{name: "glued short flags", argv: []string{"run", "watch", "123", "-Ro/r", "-i60"}, ok: true, want: ghRunWatch{repo: "o/r", run: "123", interval: 60 * time.Second}},
		{name: "no run id", argv: []string{"run", "watch"}},
		{name: "run picked by prompt", argv: []string{"run", "watch", "--exit-status"}},
		{name: "two ids", argv: []string{"run", "watch", "1", "2"}},
		{name: "interval not a number", argv: []string{"run", "watch", "123", "-i", "soon"}},
		{name: "interval with no value", argv: []string{"run", "watch", "123", "-i"}},
		{name: "unknown flag", argv: []string{"run", "watch", "123", "--web"}},
		{name: "exit status with a value", argv: []string{"run", "watch", "123", "--exit-status=false"}},
		{name: "run view", argv: []string{"run", "view", "123"}},
		{name: "pr checks watch", argv: []string{"pr", "checks", "1", "--watch"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ghParseRunWatch(tt.argv)
			if ok != tt.ok || got != tt.want {
				t.Fatalf("ghParseRunWatch(%q) = %+v, %v; want %+v, %v", tt.argv, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestGhRunWatchArgvReadsOneRun(t *testing.T) {
	watch := ghRunWatch{run: "123", interval: 30 * time.Second}
	argv := watch.argv("o/r")
	if !ghReadsOneRun(argv) || !ghReadsCIState(argv, nil) || !ghCacheable(argv, nil, ghGraphQLRepo(argv)) {
		t.Errorf("watch query %q: want a read of one run, cacheable for ghRunStateMaxAge", argv)
	}
	if repo := ghGraphQLRepo(argv); repo != "o/r" {
		t.Errorf("ghGraphQLRepo(%q) = %q, want o/r", argv, repo)
	}
	if ghWatches(argv) || ghResource(argv) != ghapi.ResourceGraphQL {
		t.Errorf("watch query %q: want a GraphQL read that is no gh watch", argv)
	}
}
