package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/execstub"
	"github.com/yasyf/cc-context/internal/ghapi"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/workspace"
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

func TestGhTargetRepo(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		argv   []string
		ghRepo string
		want   string
	}{
		{name: "none", argv: []string{"pr", "checks", "12"}},
		{name: "repo flag", argv: []string{"run", "view", "1", "--log", "--repo", "o/r"}, ghRepo: "g/h", want: "o/r"},
		{name: "GH_REPO", argv: []string{"run", "view", "1", "--log"}, ghRepo: "github.com/g/h", want: "g/h"},
		{name: "api endpoint", argv: []string{"api", "--allow-escape-sequences", "repos/o/r/actions/jobs/1/logs"}, ghRepo: "g/h", want: "o/r"},
		{name: "api leading slash", argv: []string{"api", "/repos/o/r/pulls/1"}, want: "o/r"},
		{name: "api url", argv: []string{"api", "https://api.github.com/repos/o/r/pulls?state=open"}, want: "o/r"},
		{name: "api query", argv: []string{"api", "repos/o/r?per_page=1"}, want: "o/r"},
		{name: "api after header", argv: []string{"api", "-H", "Accept: application/vnd.github.raw", "repos/o/r/readme"}, want: "o/r"},
		{name: "api placeholder", argv: []string{"api", "repos/{owner}/{repo}/pulls"}, ghRepo: "g/h", want: "g/h"},
		{name: "api placeholder outside a checkout", argv: []string{"api", "repos/{owner}/{repo}/pulls"}},
		{name: "api no repo", argv: []string{"api", "rate_limit"}},
		{name: "api owner only", argv: []string{"api", "repos/o"}},
		{name: "api user repos", argv: []string{"api", "users/o/repos"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := render.WithEnv(t.Context(), "GH_REPO="+tt.ghRepo)
			if got := ghTargetRepo(ctx, tt.argv); got != tt.want {
				t.Errorf("ghTargetRepo(%q) = %q, want %q", tt.argv, got, tt.want)
			}
		})
	}
}

func TestGhReads(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		argv  []string
		stdin string
		want  bool
	}{
		{name: "pr checks", argv: []string{"pr", "checks", "12", "--watch"}, want: true},
		{name: "run watch", argv: []string{"run", "watch", "34", "--exit-status"}, want: true},
		{name: "repo view", argv: []string{"repo", "view", "--json", "name"}, want: true},
		{name: "search", argv: []string{"search", "prs", "--author", "@me"}, want: true},
		{name: "api get", argv: []string{"api", "repos/o/r/pulls/1", "--jq", ".state"}, want: true},
		{name: "api named get", argv: []string{"api", "-X", "GET", "search/issues", "-f", "q=is:open"}, want: true},
		{name: "api lowercase get", argv: []string{"api", "--method=get", "repos/o/r"}, want: true},
		{name: "api header", argv: []string{"api", "-H", "Accept: application/vnd.github.raw", "repos/o/r/readme"}, want: true},
		{name: "api job log", argv: []string{"api", "--allow-escape-sequences", "repos/o/r/actions/jobs/1/logs"}, want: true},
		{name: "run job log", argv: []string{"run", "view", "--job", "1", "--log"}, want: true},
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
		{name: "api graphql query", argv: []string{"api", "graphql", "-f", "query={ viewer { login } }"}, want: true},
		{name: "api graphql named query", argv: []string{"api", "graphql", "--raw-field=query=query Q { viewer { login } }"}, want: true},
		{name: "api graphql get", argv: []string{"api", "-X", "GET", "graphql", "-f", "query={ viewer { login } }"}, want: true},
		{name: "api graphql stdin query", argv: []string{"api", "graphql", "--input", "-"}, stdin: `{"query":"query { viewer { login } }","variables":{}}`, want: true},
		{name: "api graphql stdin field", argv: []string{"api", "graphql", "-F", "query=@-"}, stdin: "{ viewer { login } }", want: true},
		{name: "api graphql mutation", argv: []string{"api", "graphql", "-f", "query= mutation { addStar(input: {}) { clientMutationId } }"}},
		{name: "api graphql stdin mutation", argv: []string{"api", "graphql", "--input", "-"}, stdin: `{"query":"mutation { addStar(input: {}) { clientMutationId } }"}`},
		{name: "api graphql unreadable input", argv: []string{"api", "graphql", "--input", "missing.json"}},
		{name: "api graphql commented mutation", argv: []string{"api", "graphql", "-f", "query=# merge it\nmutation { mergePullRequest(input: {}) { clientMutationId } }"}},
		{name: "api graphql mutation after a query", argv: []string{"api", "graphql", "-f", "query=query A { viewer { login } } mutation B { addStar(input: {}) { clientMutationId } }"}},
		{name: "api graphql leading slash", argv: []string{"api", "/graphql", "-f", "query={ viewer { login } }"}, want: true},
		{name: "api graphql after terminator", argv: []string{"api", "--", "graphql"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var stdin []byte
			if tt.stdin != "" {
				stdin = []byte(tt.stdin)
			}
			if got := ghReads(tt.argv, stdin); got != tt.want {
				t.Errorf("ghReads(%q) = %v, want %v", tt.argv, got, tt.want)
			}
		})
	}
}

func TestGhResource(t *testing.T) {
	t.Parallel()
	tests := []struct {
		argv []string
		want string
	}{
		{argv: []string{"pr", "view", "12"}, want: ghapi.ResourceGraphQL},
		{argv: []string{"pr", "merge", "12", "--squash"}, want: ghapi.ResourceGraphQL},
		{argv: []string{"run", "view", "34", "--log"}, want: ghapi.ResourceCore},
		{argv: []string{"api", "repos/o/r/pulls/1"}, want: ghapi.ResourceCore},
		{argv: []string{"api", "-H", "Accept: application/json", "/repos/o/r/pulls/1"}, want: ghapi.ResourceCore},
		{argv: []string{"api", "graphql", "-f", "query={ viewer { login } }"}, want: ghapi.ResourceGraphQL},
		{argv: []string{"api", "search/issues?q=is:open"}, want: ghapi.ResourceSearch},
		{argv: []string{"search", "prs", "--author", "@me"}, want: ghapi.ResourceSearch},
	}
	for _, tt := range tests {
		if got := ghResource(tt.argv); got != tt.want {
			t.Errorf("ghResource(%q) = %q, want %q", tt.argv, got, tt.want)
		}
	}
}

func TestRunVcsGhRefusesAMerge(t *testing.T) {
	t.Parallel()
	for _, argv := range [][]string{
		{"pr", "merge", "12", "--squash", "--admin"},
		{"api", "-X", "PUT", "repos/o/r/pulls/1/merge", "-f", "merge_method=merge"},
		{"api", "-X", "PUT", "/repos/o/r/branches/dev/protection", "--input", "rule.json"},
		{"api", "-X", "POST", "repos/o/r/rulesets", "--input", "ruleset.json"},
		{"api", "-X", "PATCH", "orgs/o/rulesets/9", "-f", "enforcement=disabled"},
		{"api", "graphql", "-f", "query=mutation { mergePullRequest(input: {pullRequestId: \"x\"}) { clientMutationId } }"},
		{"api", "graphql", "-f", "query=mutation { updateBranchProtectionRule(input: {}) { clientMutationId } }"},
		{"api", "graphql", "-f", "query=# a comment\nmutation { mergePullRequest(input: {pullRequestId: \"x\"}) { clientMutationId } }"},
		{"api", "/graphql", "-f", "query=mutation { mergePullRequest(input: {pullRequestId: \"x\"}) { clientMutationId } }"},
		{"api", "https://api.github.com/graphql", "-f", "query=mutation { enablePullRequestAutoMerge(input: {}) { clientMutationId } }"},
		{"api", "graphql", "-f", "query=mutation { enqueuePullRequest(input: {pullRequestId: \"x\"}) { clientMutationId } }"},
		{"api", "graphql", "-f", "query=mutation { mergeBranch(input: {repositoryId: \"x\", base: \"main\", head: \"b\"}) { clientMutationId } }"},
		{"api", "-X", "POST", "repos/o/r/merges", "-f", "base=main", "-f", "head=b"},
	} {
		err := runVcsGh(&cobra.Command{}, argv, time.Minute)
		if err == nil || !strings.HasSuffix(err.Error(), ": "+ghCommand(argv)) || !strings.Contains(err.Error(), "merge with gh pr merge") {
			t.Errorf("runVcsGh(%q) = %v, want a refusal naming the plain gh command", argv, err)
		}
	}
}

func TestRunVcsGhRunsAWriteOnTheUserToken(t *testing.T) {
	home := t.TempDir()
	app := filepath.Join(home, "config", "ccx", "github-app.toml")
	if err := os.MkdirAll(filepath.Dir(app), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(app, []byte("client_id = \"Iv1\"\nprivate_key_command = [\"cat\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	shims := t.TempDir()
	execstub.Write(t, filepath.Join(shims, "gh"), `#!/bin/sh
printf 'token=%s argv=%s\n' "${GH_TOKEN:-user}" "$*"
`)
	t.Setenv("PATH", shims+string(os.PathListSeparator)+os.Getenv("PATH"))
	prior := reviewsAPI
	reviewsAPI = func() *ghapi.Client { return ghapi.New("http://127.0.0.1:1") }
	t.Cleanup(func() { reviewsAPI = prior })

	ctx := render.WithEnv(workspace.WithRoot(t.Context(), t.TempDir()),
		"HOME="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, "config"),
		"CLAUDE_PLUGIN_DATA="+filepath.Join(home, "data"),
		"GH_TOKEN=", "GITHUB_TOKEN=", "GH_REPO=o/r",
	)
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	cmd.SetOut(&out)
	if err := runVcsGh(cmd, []string{"pr", "close", "12", "--delete-branch"}, time.Minute); err != nil {
		t.Fatalf("runVcsGh(pr close) = %v, want gh run on the user's token", err)
	}
	if got, want := out.String(), "token=user argv=pr close 12 --delete-branch\n"; got != want {
		t.Errorf("gh printed %q, want %q", got, want)
	}
}

func TestRunVcsGhOutsideACheckoutRunsOnTheUserToken(t *testing.T) {
	home := t.TempDir()
	app := filepath.Join(home, "config", "ccx", "github-app.toml")
	if err := os.MkdirAll(filepath.Dir(app), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(app, []byte("client_id = \"Iv1\"\nprivate_key_command = [\"cat\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	shims := t.TempDir()
	execstub.Write(t, filepath.Join(shims, "gh"), `#!/bin/sh
if [ "$1" = repo ]; then
  echo "failed to run git: fatal: not a git repository" >&2
  exit 1
fi
printf 'token=%s argv=%s\n' "${GH_TOKEN:-user}" "$*"
`)
	t.Setenv("PATH", shims+string(os.PathListSeparator)+os.Getenv("PATH"))
	prior := reviewsAPI
	reviewsAPI = func() *ghapi.Client { return ghapi.New("http://127.0.0.1:1") }
	t.Cleanup(func() { reviewsAPI = prior })

	ctx := render.WithEnv(workspace.WithRoot(t.Context(), t.TempDir()),
		"HOME="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, "config"),
		"CLAUDE_PLUGIN_DATA="+filepath.Join(home, "data"),
		"GH_TOKEN=", "GITHUB_TOKEN=", "GH_REPO=",
	)
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	cmd.SetOut(&out)
	if err := runVcsGh(cmd, []string{"api", "rate_limit"}, 0); err != nil {
		t.Fatalf("runVcsGh outside a checkout = %v, want gh run on the user's token", err)
	}
	if got, want := out.String(), "token=user argv=api rate_limit\n"; got != want {
		t.Errorf("gh printed %q, want %q", got, want)
	}
}

func TestGhReadsOutsideInstallation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		argv []string
		want bool
	}{
		{name: "user", argv: []string{"api", "user", "--jq", ".login"}, want: true},
		{name: "leading slash", argv: []string{"api", "/user"}, want: true},
		{name: "url", argv: []string{"api", "https://api.github.com/user"}, want: true},
		{name: "user subpath", argv: []string{"api", "user/repos?per_page=1"}, want: true},
		{name: "after header", argv: []string{"api", "-H", "Accept: application/json", "user"}, want: true},
		{name: "users", argv: []string{"api", "users/o"}, want: true},
		{name: "org installations", argv: []string{"api", "/orgs/Forge-FDE/installations"}, want: true},
		{name: "app", argv: []string{"api", "apps/ccx"}, want: true},
		{name: "search", argv: []string{"api", "search/issues?q=org:o"}, want: true},
		{name: "repo", argv: []string{"api", "repos/o/r/pulls/1"}},
		{name: "repo placeholder", argv: []string{"api", "repos/{owner}/{repo}/pulls"}},
		{name: "rate limit", argv: []string{"api", "rate_limit"}},
		{name: "graphql", argv: []string{"api", "graphql", "-f", "query={ viewer { login } }"}},
		{name: "pr list", argv: []string{"pr", "list", "--author", "@me"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := ghReadsOutsideInstallation(tt.argv); got != tt.want {
				t.Errorf("ghReadsOutsideInstallation(%q) = %v, want %v", tt.argv, got, tt.want)
			}
		})
	}
}

func TestGhResolveMe(t *testing.T) {
	shims := t.TempDir()
	execstub.Write(t, filepath.Join(shims, "gh"), `#!/bin/sh
[ "$*" = "config get --host github.com user" ] && echo octocat
`)
	t.Setenv("PATH", shims+string(os.PathListSeparator)+os.Getenv("PATH"))
	argv := []string{
		"pr", "list", "--author", "@me", "--assignee=@me",
		"--search", "review-requested:@me is:open", "--label", "@meeting", "--jq", ".[].email",
	}
	got, err := ghResolveMe(t.Context(), render.Dir(t.TempDir()), argv)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"pr", "list", "--author", "octocat", "--assignee=octocat",
		"--search", "review-requested:octocat is:open", "--label", "@meeting", "--jq", ".[].email",
	}
	if !slices.Equal(got, want) {
		t.Errorf("ghResolveMe(%q) = %q, want %q", argv, got, want)
	}
}

func TestRunVcsGhViewerRunsOnTheUserToken(t *testing.T) {
	home := t.TempDir()
	app := filepath.Join(home, "config", "ccx", "github-app.toml")
	if err := os.MkdirAll(filepath.Dir(app), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(app, []byte("client_id = \"Iv1\"\nprivate_key_command = [\"cat\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	shims := t.TempDir()
	execstub.Write(t, filepath.Join(shims, "gh"), `#!/bin/sh
printf 'token=%s argv=%s\n' "${GH_TOKEN:-user}" "$*"
`)
	t.Setenv("PATH", shims+string(os.PathListSeparator)+os.Getenv("PATH"))
	prior := reviewsAPI
	reviewsAPI = func() *ghapi.Client { return ghapi.New("http://127.0.0.1:1") }
	t.Cleanup(func() { reviewsAPI = prior })

	ctx := render.WithEnv(workspace.WithRoot(t.Context(), t.TempDir()),
		"HOME="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, "config"),
		"CLAUDE_PLUGIN_DATA="+filepath.Join(home, "data"),
		"GH_TOKEN=", "GITHUB_TOKEN=", "GH_REPO=o/r",
	)
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	cmd.SetOut(&out)
	if err := runVcsGh(cmd, []string{"api", "user", "--jq", ".login"}, 0); err != nil {
		t.Fatalf("runVcsGh(api user) = %v, want gh run on the user's token", err)
	}
	if got, want := out.String(), "token=user argv=api user --jq .login\n"; got != want {
		t.Errorf("gh printed %q, want %q", got, want)
	}
}

func TestVcsGhPassesGhFlagsThrough(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		argv   []string
		want   []string
		maxAge time.Duration
	}{
		{name: "after --", argv: []string{"--", "api", "--allow-escape-sequences", "repos/o/r/actions/jobs/1/logs"}, want: []string{"api", "--allow-escape-sequences", "repos/o/r/actions/jobs/1/logs"}},
		{name: "without --", argv: []string{"api", "--allow-escape-sequences", "repos/o/r/actions/jobs/1/logs"}, want: []string{"api", "--allow-escape-sequences", "repos/o/r/actions/jobs/1/logs"}},
		{name: "max-age before the command", argv: []string{"--max-age", "5m", "run", "view", "1", "--log-failed"}, want: []string{"run", "view", "1", "--log-failed"}, maxAge: 5 * time.Minute},
		{name: "a gh --max-age stays gh's", argv: []string{"api", "x", "--max-age", "1m"}, want: []string{"api", "x", "--max-age", "1m"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cmd := newVcsGhCmd()
			if err := cmd.ParseFlags(tt.argv); err != nil {
				t.Fatalf("parse %q: %v", tt.argv, err)
			}
			args := cmd.Flags().Args()
			if err := cmd.Args(cmd, args); err != nil {
				t.Fatalf("args %q: %v", args, err)
			}
			if !slices.Equal(args, tt.want) {
				t.Errorf("gh args = %q, want %q", args, tt.want)
			}
			if got, _ := cmd.Flags().GetDuration("max-age"); got != tt.maxAge {
				t.Errorf("max-age = %s, want %s", got, tt.maxAge)
			}
		})
	}
}
