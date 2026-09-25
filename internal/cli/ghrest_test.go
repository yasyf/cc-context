package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/render"
)

type ghRoute struct {
	argv   []string
	stdout string
}

func ghRouteKey(argv []string) string {
	sum := sha256.Sum256([]byte(strings.Join(argv, "\n") + "\n"))
	return hex.EncodeToString(sum[:])[:16]
}

func installGHRoutes(t *testing.T, routes ...ghRoute) string {
	t.Helper()
	dir := t.TempDir()
	for _, r := range routes {
		if err := os.WriteFile(filepath.Join(dir, ghRouteKey(r.argv)+".out"), []byte(r.stdout), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	script := `#!/bin/sh
printf '%s\n' "$*" >> "` + dir + `/calls"
key=$(printf '%s\n' "$@" | shasum -a 256 | cut -c1-16)
if [ -r "` + dir + `/$key.out" ]; then cat "` + dir + `/$key.out"; exit 0; fi
printf 'gh: Not Found (HTTP 404): %s\n' "$*" >&2
exit 1
`
	writeShipExecutable(t, dir, "gh", script)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return filepath.Join(dir, "calls")
}

func ghRecordedRoute(t *testing.T, scenario string) ghRoute {
	t.Helper()
	g := loadGHGolden(t, scenario)
	return ghRoute{argv: g.argv, stdout: g.stdout}
}

func ghNewestPullArgv(branch string) []string {
	return []string{"api", "repos/{owner}/{repo}/pulls?head={owner}%3A" + strings.ReplaceAll(branch, "/", "%2F") + "&state=all&sort=created&direction=desc&per_page=1"}
}

func assertNoGraphQL(t *testing.T, calls string) {
	t.Helper()
	data, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "graphql") {
		t.Errorf("a stack read went through GraphQL:\n%s", data)
	}
}

func TestStackQueryPRsReadsOverREST(t *testing.T) {
	const openBranch = "o1/add-latest-pre-release-and-pin-flags-to-gh-extension-upgrade/nysoxynolqlo"
	var open ghPull
	if err := json.Unmarshal([]byte(loadGHGolden(t, "rest-pull-open").stdout), &open); err != nil {
		t.Fatal(err)
	}
	calls := installGHRoutes(t,
		ghRecordedRoute(t, "rest-pulls-head-merged"),
		ghRecordedRoute(t, "rest-pulls-head-newest"),
		ghRecordedRoute(t, "rest-pulls-head-closed"),
		ghRecordedRoute(t, "rest-pulls-head-none"),
		ghRecordedRoute(t, "rest-issue-closed-by"),
		ghRoute{argv: ghNewestPullArgv(openBranch), stdout: loadGHGolden(t, "rest-pulls-head-open").stdout},
		ghRoute{argv: []string{"api", "repos/{owner}/{repo}/pulls/13982"}, stdout: loadGHGolden(t, "rest-pull-open").stdout},
	)
	branches := []string{"fix-ship-help-graphite-demote", "yasyf/transcript-ccx-issues", "stack-rebase-per-root", "no-such-branch", openBranch}
	prs, err := stackQueryPRs(context.Background(), render.Dir(t.TempDir()), "main", branches)
	if err != nil {
		t.Fatalf("stackQueryPRs: %v", err)
	}
	type want struct {
		number    int
		state     string
		mergeable string
		landed    bool
	}
	for branch, w := range map[string]want{
		"fix-ship-help-graphite-demote": {3, "MERGED", statusUnknown, true},
		"yasyf/transcript-ccx-issues":   {2, "MERGED", statusUnknown, true},
		"stack-rebase-per-root":         {64, "CLOSED", statusUnknown, false},
		openBranch:                      {13982, "OPEN", "CONFLICTING", false},
	} {
		pr := prs[branch]
		if pr == nil {
			t.Errorf("%s: no pull request", branch)
			continue
		}
		if pr.Number != w.number || pr.State != w.state || pr.Mergeable != w.mergeable || pr.Landed != w.landed {
			t.Errorf("%s = #%d %s %s landed=%v, want #%d %s %s landed=%v",
				branch, pr.Number, pr.State, pr.Mergeable, pr.Landed, w.number, w.state, w.mergeable, w.landed)
		}
	}
	if pr := prs[openBranch]; pr != nil && (pr.Head != open.Head.SHA || pr.Base != open.Base.Ref || pr.URL != open.HTMLURL || pr.Title != open.Title) {
		t.Errorf("open PR = %+v, want head %s base %s url %s title %q", pr, open.Head.SHA, open.Base.Ref, open.HTMLURL, open.Title)
	}
	if pr, ok := prs["no-such-branch"]; ok {
		t.Errorf("no-such-branch resolved to %+v", pr)
	}
	assertNoGraphQL(t, calls)
}

func TestStackQueryPRsReadsTheQueueCloseOverREST(t *testing.T) {
	closed := ghRecordedRoute(t, "rest-pulls-head-closed")
	for _, tt := range []struct {
		name     string
		comments string
		landed   bool
	}{
		{"queue landed it", `[[{"user":{"login":"graphite-app[bot]"},"body":"* **Sep 25**: Merged by the Graphite merge queue."}]]`, true},
		{"queue dropped it", `[[{"user":{"login":"graphite-app[bot]"},"body":"* **Sep 25**: removed from the merge queue."}]]`, false},
		{"queue left no comment", loadGHGolden(t, "rest-issue-comments").stdout, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := installGHRoutes(t, closed,
				ghRoute{argv: []string{"api", "repos/{owner}/{repo}/issues/64", "--jq", `.closed_by.login // ""`}, stdout: "graphite-app[bot]\n"},
				ghRoute{argv: []string{"api", "--paginate", "--slurp", "repos/{owner}/{repo}/issues/64/comments?per_page=100"}, stdout: tt.comments},
			)
			prs, err := stackQueryPRs(context.Background(), render.Dir(t.TempDir()), "main", []string{"stack-rebase-per-root"})
			if err != nil {
				t.Fatalf("stackQueryPRs: %v", err)
			}
			if got := prs["stack-rebase-per-root"].Landed; got != tt.landed {
				t.Errorf("landed = %v, want %v", got, tt.landed)
			}
			assertNoGraphQL(t, calls)
		})
	}
}
