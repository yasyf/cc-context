package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/ghapi"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcstest"
)

var (
	stackGoldenOwnBranches     = []string{"fix-ship-help-graphite-demote", "yasyf/transcript-ccx-issues", "stack-rebase-per-root", "no-such-branch"}
	stackGoldenForeignBranches = []string{"o1/add-latest-pre-release-and-pin-flags-to-gh-extension-upgrade/nysoxynolqlo", "williammartin-clean-git-test-seams"}
)

func useGitHubAPI(t *testing.T, client *ghapi.Client) {
	t.Helper()
	prior := reviewsAPI
	reviewsAPI = func() *ghapi.Client { return client }
	t.Cleanup(func() { reviewsAPI = prior })
}

func serveGitHubStatus(t *testing.T, status int, body string) {
	t.Helper()
	t.Setenv("GH_TOKEN", "stack-test-token")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if _, err := io.WriteString(w, body); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	t.Cleanup(ts.Close)
	useGitHubAPI(t, ghapi.New(ts.URL))
}

func limitGraphQL(t *testing.T) {
	t.Helper()
	serveGitHubStatus(t, http.StatusForbidden, `{"message":"API rate limit exceeded for user ID 1."}`)
}

func assertNoGH(t *testing.T, calls string) {
	t.Helper()
	if data, err := os.ReadFile(calls); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a GraphQL stack read ran gh:\n%s", data)
	}
}

func TestStackPRGraphQLIsTheRecordedOne(t *testing.T) {
	t.Parallel()
	for golden, branches := range map[string][]string{"stack-graphql-own": stackGoldenOwnBranches, "stack-graphql-foreign": stackGoldenForeignBranches} {
		t.Run(golden, func(t *testing.T) {
			t.Parallel()
			fields := map[string]string{}
			argv := loadGHAPIGolden(t, golden).argv
			for i, arg := range argv[:len(argv)-1] {
				if arg == "-f" || arg == "-F" {
					key, value, _ := strings.Cut(argv[i+1], "=")
					fields[key] = value
				}
			}
			if got := stackPRGraphQL(len(branches)); got != fields["query"] {
				t.Errorf("stackPRGraphQL(%d) = %q, want the recorded %q", len(branches), got, fields["query"])
			}
			for i, branch := range branches {
				if got := fields[reviewsAlias(i)]; got != branch {
					t.Errorf("recorded %s = %q, want %q", reviewsAlias(i), got, branch)
				}
			}
		})
	}
}

func TestStackQueryPRsReadsOverGraphQL(t *testing.T) {
	type want struct {
		number    int
		state     string
		mergeable string
		landed    bool
	}
	tests := []struct {
		golden   string
		repo     laneSeed
		branches []string
		want     map[string]want
	}{
		{"stack-graphql-own", laneSeed{}, stackGoldenOwnBranches, map[string]want{
			"fix-ship-help-graphite-demote": {3, "MERGED", statusUnknown, true},
			"yasyf/transcript-ccx-issues":   {2, "MERGED", statusUnknown, true},
			"stack-rebase-per-root":         {64, "CLOSED", "CONFLICTING", false},
		}},
		{"stack-graphql-foreign", laneSeed{nameWithOwner: "cli/cli", owner: "cli", public: true}, stackGoldenForeignBranches, map[string]want{
			"williammartin-clean-git-test-seams": {14355, "OPEN", "MERGEABLE", false},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.golden, func(t *testing.T) {
			f := vcstest.Repo(t)
			seedLaneRecords(f.Context(), t, f.Dir, tt.repo)
			useGitHubAPI(t, serveGHAPIGolden(t, loadGHAPIGolden(t, tt.golden)))
			calls := installGHRoutes(t, f.ShimBin)

			prs, err := stackQueryPRs(f.Context(), render.Dir(f.Dir), "main", tt.branches)
			if err != nil {
				t.Fatalf("stackQueryPRs: %v", err)
			}
			for _, branch := range tt.branches {
				pr, w := prs[branch], tt.want[branch]
				switch {
				case pr == nil && w.number == 0:
				case pr == nil:
					t.Errorf("%s: no pull request, want #%d", branch, w.number)
				case w.number == 0:
					t.Errorf("%s resolved to %+v, want none", branch, pr)
				case pr.Number != w.number || pr.State != w.state || pr.Mergeable != w.mergeable || pr.Landed != w.landed:
					t.Errorf("%s = #%d %s %s landed=%v, want #%d %s %s landed=%v",
						branch, pr.Number, pr.State, pr.Mergeable, pr.Landed, w.number, w.state, w.mergeable, w.landed)
				}
			}
			assertNoGH(t, calls)
		})
	}
}

func TestStackQueryPRsFailsOnAnyOtherGraphQLRefusal(t *testing.T) {
	f := shipRepo(t)
	serveGitHubStatus(t, http.StatusBadGateway, `{"message":"Server Error"}`)
	calls := installGHRoutes(t, f.ShimBin)

	_, err := stackQueryPRs(f.Context(), render.Dir(f.Dir), "main", stackGoldenOwnBranches)
	var status *ghapi.StatusError
	if !errors.As(err, &status) || status.Status != http.StatusBadGateway {
		t.Fatalf("stackQueryPRs error = %v, want GitHub's 502", err)
	}
	assertNoGH(t, calls)
}

func TestStackQueryPRsReadsABranchCrowdedOutByForksOverREST(t *testing.T) {
	f := shipRepo(t)
	fork := `{"number":99,"state":"OPEN","isCrossRepository":true,"timelineItems":{"nodes":[]}}`
	serveGitHubStatus(t, http.StatusOK, `{"data":{"repository":{`+
		`"p0":{"totalCount":11,"nodes":[`+fork+`]},`+
		`"p1":{"totalCount":1,"nodes":[`+fork+`]}}}}`)
	installGHRoutes(t, f.ShimBin, ghRecordedRoute(t, "rest-pulls-head-merged"))

	prs, err := stackQueryPRs(f.Context(), render.Dir(f.Dir), "main", []string{"fix-ship-help-graphite-demote", "no-such-branch"})
	if err != nil {
		t.Fatalf("stackQueryPRs: %v", err)
	}
	if pr := prs["fix-ship-help-graphite-demote"]; pr == nil || pr.Number != 3 || !pr.Landed {
		t.Errorf("crowded branch = %+v, want REST's landed #3", pr)
	}
	if pr, ok := prs["no-such-branch"]; ok {
		t.Errorf("fork-only branch resolved to %+v", pr)
	}
}

// TestStackQueryPRsLandsAnOpenPullRequestTheQueueSquashed is #31976 and #31979:
// the queue squashed both onto dev while GitHub answered 500s, so neither was
// ever closed, and a stack submit from their child replayed both onto dev. A
// head committed after its squash is work pushed since, so it has not landed.
func TestStackQueryPRsLandsAnOpenPullRequestTheQueueSquashed(t *testing.T) {
	f := shipRepo(t, vcstest.Remote())
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "a")
	writeShipFile(t, f.Dir, "a.txt", "a\n")
	mustRun(t, f.Env(), f.Dir, "git", "add", "a.txt")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "a")
	landed := gitAt(t, f.Env(), f.Dir, "rev-parse", "HEAD")
	writeShipFile(t, f.Dir, "c.txt", "c\n")
	mustRun(t, f.Env(), f.Dir, "git", "add", "c.txt")
	mustRun(t, append(f.Env(), "GIT_COMMITTER_DATE=4000000000 +0000"), f.Dir, "git", "commit", "-qm", "c")
	pushedSince := gitAt(t, f.Env(), f.Dir, "rev-parse", "HEAD")
	restackSquashRemote(t, f, "main", "a (#41)", "a")
	restackSquashRemote(t, f, "main", "c (#43)", "c")
	mustRun(t, f.Env(), f.Dir, "git", "fetch", "-q", "origin")
	open := func(number int, head string) string {
		return fmt.Sprintf(`{"totalCount":1,"nodes":[{"number":%d,"state":"OPEN","isCrossRepository":false,"baseRefName":"main","headRefOid":%q,"timelineItems":{"nodes":[]}}]}`, number, head)
	}
	serveGitHubStatus(t, http.StatusOK, `{"data":{"repository":{"p0":`+open(41, landed)+`,"p1":`+open(42, landed)+`,"p2":`+open(43, pushedSince)+`}}}`)

	prs, err := stackQueryPRs(f.Context(), render.Dir(f.Dir), "main", []string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("stackQueryPRs: %v", err)
	}
	if pr := prs["a"]; pr == nil || pr.Number != 41 || !pr.Landed {
		t.Errorf("a = %+v, want #41 landed by its squash on main", pr)
	}
	if pr := prs["b"]; pr == nil || pr.Number != 42 || pr.Landed {
		t.Errorf("b = %+v, want #42 open and not landed", pr)
	}
	if pr := prs["c"]; pr == nil || pr.Number != 43 || pr.Landed {
		t.Errorf("c = %+v, want #43 not landed: its head postdates its squash", pr)
	}
}
