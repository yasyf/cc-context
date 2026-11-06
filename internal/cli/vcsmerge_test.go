package cli

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/gtapi"
	"github.com/yasyf/cc-context/internal/nativeapi"
	"github.com/yasyf/cc-context/internal/prstate"
	"github.com/yasyf/cc-context/internal/vcstest"
)

func runMergeCmd(ctx context.Context, t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newVcsMergeCmd() //nolint:contextcheck // ExecuteContext(ctx) below is what sets cmd's context; contextcheck cannot see through cobra's two-step wiring
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(ctx)
	return out.String(), err
}

func stubMergeQueueRead(t *testing.T, f *vcstest.Fixture, origin string) context.Context {
	t.Helper()
	mustRun(t, f.Env(), f.Dir, "git", "remote", "set-url", "origin", origin)
	client := gtapi.NewNativeWithToken("https://example.invalid", "unused")
	previous := gtAPINativeDefault
	gtAPINativeDefault = func() *gtapi.Client { return client }
	t.Cleanup(func() { gtAPINativeDefault = previous })
	return withStackProvider(f.Context(), true)
}

type mergeGitHub struct {
	calls []string
	asked map[string]map[string]any
}

func serveMergeGitHub(t *testing.T, routes map[string]string) (*mergeGitHub, *nativeapi.Client) {
	t.Helper()
	gh := &mergeGitHub{asked: map[string]map[string]any{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := r.Method + " " + r.URL.Path
		var asked map[string]any
		if r.Method != http.MethodGet {
			if err := json.NewDecoder(r.Body).Decode(&asked); err != nil {
				t.Errorf("decode %s body: %v", call, err)
			}
		}
		switch query, _ := asked["query"].(string); {
		case strings.HasPrefix(query, "mutation"):
			call = mergeMutation
		case query == "{viewer{login}}":
			call = mergeViewer
		}
		gh.calls = append(gh.calls, call)
		gh.asked[call] = asked
		body, ok := routes[call]
		if !ok {
			t.Errorf("unexpected %s", call)
			http.Error(w, "unexpected", http.StatusInternalServerError)
			return
		}
		if refusal, refused := strings.CutPrefix(body, mergeRefusalPrefix); refused {
			code, text, _ := strings.Cut(refusal, " ")
			status, err := strconv.Atoi(code)
			if err != nil {
				t.Errorf("route %s refuses with %q, not a status", call, refusal)
			}
			http.Error(w, text, status)
			return
		}
		if _, err := fmt.Fprint(w, body); err != nil {
			t.Errorf("write fixture response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return gh, nativeapi.NewWithToken(srv.URL, "key")
}

const (
	mergeMutation      = "POST /graphql mergePullRequest"
	mergeViewer        = "POST /graphql viewer"
	mergeRefusalPrefix = "refuse "
)

func refused(status int, text string) string {
	return fmt.Sprintf("%s%d %s", mergeRefusalPrefix, status, text)
}

func viewer(login string) string {
	return fmt.Sprintf(`{"data":{"viewer":{"login":%q}}}`, login)
}

func (gh *mergeGitHub) merged() bool {
	return slices.ContainsFunc(gh.calls, func(call string) bool { return strings.HasPrefix(call, "PUT ") || call == mergeMutation })
}

func mergedByGraphQL(number int, head string) string {
	return fmt.Sprintf(`{"data":{"mergePullRequest":{"pullRequest":{"merged":true,"headRefName":%q,"mergeCommit":{"oid":"m%d"}}}}}`, head, number)
}

func mergeCheck(name, conclusion string, workflow int64, started string) prstate.Context {
	c := prstate.Context{Typename: "CheckRun", Name: name, Conclusion: conclusion, Status: "COMPLETED", DatabaseID: 1}
	c.StartedAt, _ = time.Parse(time.RFC3339, started)
	c.CheckSuite.DatabaseID = workflow
	c.CheckSuite.WorkflowRun.Workflow.DatabaseID = workflow
	return c
}

func runningCheck(name string) prstate.Context {
	c := mergeCheck(name, "", 11, "2026-11-05T10:05:00Z")
	c.Status = "IN_PROGRESS"
	return c
}

func mergePull(number int, state, head string, checks ...prstate.Context) string {
	rollup := rollupOf("PENDING", checks...)
	rollup.Contexts.TotalCount = len(checks)
	encoded, err := json.Marshal(rollup)
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf(`"id":"PR_%d","number":%d,"state":%q,"headRefName":%q,"headRefOid":"head%d","commits":{"nodes":[{"commit":{"statusCheckRollup":%s}}]}`, number, number, state, head, number, encoded)
}

func mergeRead(pull string, stack ...string) string {
	if len(stack) == 0 {
		return fmt.Sprintf(`{"data":{"repository":{"defaultBranchRef":{"name":"main"},"pullRequest":{%s,"mergeCommit":null,"stackEntry":null,"stack":null}}}}`, pull)
	}
	entries := make([]string, len(stack))
	position := 0
	for i, entry := range stack {
		entries[i] = fmt.Sprintf(`{"position":%d,"pullRequest":{%s}}`, i+1, entry)
		if entry == pull {
			position = i + 1
		}
	}
	return fmt.Sprintf(`{"data":{"repository":{"defaultBranchRef":{"name":"main"},"pullRequest":{%s,"mergeCommit":null,"stackEntry":{"position":%d},"stack":{"entries":{"nodes":[%s],"pageInfo":{"hasNextPage":false}}}}}}}`, pull, position, strings.Join(entries, ","))
}

func landedPull(number int, head string) string {
	return fmt.Sprintf(`{"number":%d,"state":"closed","merged":true,"merge_commit_sha":"m%d","head":{"ref":%q}}`, number, number, head)
}

func TestVcsMergeLandsAPlainPullRequestThroughGraphQLPinnedToItsHead(t *testing.T) {
	f := shipRepo(t, vcstest.Remote())
	ctx := stubMergeQueueRead(t, f, "git@github.com:org/repo.git")
	shipGTUntracked(t, f, "tip")
	gh, client := serveMergeGitHub(t, map[string]string{
		mergeViewer:                 viewer("org"),
		"GET /repos/org/repo/pulls": `[{"number":7,"state":"open"}]`,
		"POST /graphql":             mergeRead(mergePull(7, "OPEN", "tip", mergeCheck("test", "FAILURE", 11, "2026-11-05T10:00:00Z"), mergeCheck("test", "SUCCESS", 11, "2026-11-05T10:05:00Z"), runningCheck("lint"))),
		mergeMutation:               mergedByGraphQL(7, "tip"),
	})

	out, err := runMergeCmd(withMergeClient(ctx, client), t, "--repo", "org/repo", "--admin", "--match-head-commit", "head7")
	if err != nil {
		t.Fatalf("merge: %v\n%s", err, out)
	}
	if want := "merged #7 tip m7\n"; out != want {
		t.Errorf("report = %q, want %q", out, want)
	}
	mutation, _ := gh.asked[mergeMutation]["query"].(string)
	for _, want := range []string{`pullRequestId:"PR_7"`, `expectedHeadOid:"head7"`, "mergeMethod:SQUASH"} {
		if !strings.Contains(mutation, want) {
			t.Errorf("merge mutation = %q, want it to carry %s", mutation, want)
		}
	}
	if slices.ContainsFunc(gh.calls, func(call string) bool { return strings.HasPrefix(call, "PUT ") }) {
		t.Errorf("a plain PR merged through REST: %v", gh.calls)
	}
}

func TestVcsMergeLandsAStackMemberThroughTheAsyncMergeAPI(t *testing.T) {
	low := mergePull(4, "MERGED", "low")
	mid := mergePull(5, "OPEN", "mid", mergeCheck("test", "SUCCESS", 11, "2026-11-05T10:00:00Z"))
	gh, client := serveMergeGitHub(t, map[string]string{
		mergeViewer:     viewer("yasyf"),
		"POST /graphql": mergeRead(mid, low, mid, mergePull(6, "OPEN", "top")),
		"PUT /repos/yasyf/cc-context/pulls/5/merge-async": `{"status":"merged","details":{"message":"Pull request was merged.","sha":"m5"}}`,
		"GET /repos/yasyf/cc-context/pulls/5":             landedPull(5, "mid"),
	})

	out, err := runMergeCmd(withMergeClient(t.Context(), client), t, "5", "--repo", "yasyf/cc-context", "--admin", "--match-head-commit", "head5")
	if err != nil {
		t.Fatalf("merge: %v\n%s", err, out)
	}
	if want := "merged #5 mid m5\n"; out != want {
		t.Errorf("report = %q, want %q", out, want)
	}
	if want := (map[string]any{"sha": "head5", "merge_method": "squash", "merge_action": "direct_merge", "bypass_rules": true}); !reflect.DeepEqual(gh.asked["PUT /repos/yasyf/cc-context/pulls/5/merge-async"], want) {
		t.Errorf("merge body = %v, want %v", gh.asked["PUT /repos/yasyf/cc-context/pulls/5/merge-async"], want)
	}
}

func TestVcsMergeLandsAnOpenDownstackOnlyWhenAsked(t *testing.T) {
	low := mergePull(4, "OPEN", "low", mergeCheck("test", "SUCCESS", 11, "2026-11-05T10:00:00Z"))
	mid := mergePull(5, "OPEN", "mid")
	routes := map[string]string{
		mergeViewer:     viewer("yasyf"),
		"POST /graphql": mergeRead(mid, mergePull(3, "MERGED", "base"), low, mid),
		"PUT /repos/yasyf/cc-context/pulls/5/merge-async": `{"status":"merged","details":{"sha":"m5"}}`,
		"GET /repos/yasyf/cc-context/pulls/4":             landedPull(4, "low"),
		"GET /repos/yasyf/cc-context/pulls/5":             landedPull(5, "mid"),
	}

	t.Run("refused", func(t *testing.T) {
		gh, client := serveMergeGitHub(t, routes)
		_, err := runMergeCmd(withMergeClient(t.Context(), client), t, "5", "--repo", "yasyf/cc-context", "--admin", "--match-head-commit", "head5")
		if err == nil || !strings.Contains(err.Error(), "would also land #4 below it") || !strings.Contains(err.Error(), "--with-downstack") {
			t.Fatalf("merge = %v, want a refusal naming #4 and --with-downstack", err)
		}
		if gh.merged() {
			t.Errorf("merge reached GitHub: %v", gh.calls)
		}
	})
	t.Run("with --with-downstack", func(t *testing.T) {
		_, client := serveMergeGitHub(t, routes)
		out, err := runMergeCmd(withMergeClient(t.Context(), client), t, "5", "--repo", "yasyf/cc-context", "--admin", "--match-head-commit", "head5", "--with-downstack")
		if err != nil {
			t.Fatalf("merge: %v\n%s", err, out)
		}
		if want := "merged #4 low m4\nmerged #5 mid m5\n"; out != want {
			t.Errorf("report = %q, want %q", out, want)
		}
	})
}

func TestVcsMergeRefusesARedItCannotProveInherited(t *testing.T) {
	red := mergePull(7, "OPEN", "tip", mergeCheck("test", "FAILURE", 11, "2026-11-05T10:00:00Z"), mergeCheck("lint", "SUCCESS", 11, "2026-11-05T10:00:00Z"))
	for _, tc := range []struct {
		name, read, run, jobs, want string
		args                        []string
	}{
		{name: "a moved head", read: mergeRead(mergePull(7, "OPEN", "tip")), args: []string{"--match-head-commit", "head6"}, want: "PR #7's head is head7, not head6"},
		{name: "a merged PR at another head", read: mergeRead(mergePull(7, "MERGED", "tip")), args: []string{"--match-head-commit", "head6"}, want: "PR #7's head is head7, not head6"},
		{name: "a failed check", read: mergeRead(red), want: "#7 test failed; fix it and merge the new head, or pass --red-on-main"},
		{name: "more checks than one read covers", read: mergeRead(strings.Replace(mergePull(7, "OPEN", "tip", mergeCheck("lint", "SUCCESS", 11, "2026-11-05T10:00:00Z")), `"totalCount":1`, `"totalCount":101`, 1)), want: "PR #7's head carries 101 checks, more than the 1 one read covers"},
		{name: "a red stack member below", read: mergeRead(mergePull(5, "OPEN", "mid"), mergePull(4, "OPEN", "low", mergeCheck("test", "TIMED_OUT", 11, "2026-11-05T10:00:00Z")), mergePull(5, "OPEN", "mid")), args: []string{"--match-head-commit", "head5", "--with-downstack"}, want: "#4 test failed"},
		{
			name: "a run on a pull request branch", read: mergeRead(red), args: []string{"--red-on-main", "99"},
			run: `{"head_branch":"main","event":"pull_request","workflow_id":11}`, jobs: `{"jobs":[{"name":"test","status":"completed","conclusion":"failure"}]}`,
			want: "--red-on-main 99 names a pull_request run of main; it takes a run on main itself",
		},
		{
			name: "a run that passes the check", read: mergeRead(red), args: []string{"--red-on-main", "99"},
			run: `{"head_branch":"main","event":"push","workflow_id":11}`, jobs: `{"jobs":[{"name":"test","status":"completed","conclusion":"success"},{"name":"lint","status":"completed","conclusion":"failure"}]}`,
			want: "#7 test failed, and run 99 on main does not fail the same tests, or the same job where a log names no test",
		},
		{
			name: "a run of another workflow", read: mergeRead(red), args: []string{"--red-on-main", "99"},
			run: `{"head_branch":"main","event":"push","workflow_id":12}`, jobs: `{"jobs":[{"name":"test","status":"completed","conclusion":"failure"}]}`,
			want: "#7 test failed, and run 99 on main does not fail the same tests, or the same job where a log names no test",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			number := "7"
			if slices.Contains(tc.args, "--with-downstack") {
				number = "5"
			}
			gh, client := serveMergeGitHub(t, map[string]string{
				mergeViewer:     viewer("yasyf"),
				"POST /graphql": tc.read,
				"GET /repos/yasyf/cc-context/actions/runs/99":      tc.run,
				"GET /repos/yasyf/cc-context/actions/runs/99/jobs": tc.jobs,
				"GET /repos/yasyf/cc-context/actions/jobs/1/logs":  "",
			})
			args := []string{number, "--repo", "yasyf/cc-context", "--admin"}
			if !slices.Contains(tc.args, "--match-head-commit") {
				args = append(args, "--match-head-commit", "head7")
			}
			_, err := runMergeCmd(withMergeClient(t.Context(), client), t, append(args, tc.args...)...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("merge = %v, want %q", err, tc.want)
			}
			if gh.merged() {
				t.Errorf("merge reached GitHub: %v", gh.calls)
			}
		})
	}
}

func TestVcsMergeLandsPastARedTheMainRunFailsTheSameWay(t *testing.T) {
	gh, client := serveMergeGitHub(t, map[string]string{
		mergeViewer:     viewer("yasyf"),
		"POST /graphql": mergeRead(mergePull(7, "OPEN", "tip", mergeCheck("test", "FAILURE", 11, "2026-11-05T10:00:00Z"), mergeCheck("lint", "SUCCESS", 11, "2026-11-05T10:00:00Z"))),
		"GET /repos/yasyf/cc-context/actions/runs/99":      `{"head_branch":"main","event":"push","workflow_id":11}`,
		"GET /repos/yasyf/cc-context/actions/runs/99/jobs": `{"jobs":[{"name":"lint","status":"completed","conclusion":"success"},{"name":"test","status":"completed","conclusion":"failure"}]}`,
		"GET /repos/yasyf/cc-context/actions/jobs/1/logs":  "",
		mergeMutation: mergedByGraphQL(7, "tip"),
	})

	out, err := runMergeCmd(withMergeClient(t.Context(), client), t, "7", "--repo", "yasyf/cc-context", "--admin", "--match-head-commit", "head7", "--red-on-main", "99")
	if err != nil {
		t.Fatalf("merge: %v\n%s", err, out)
	}
	if want := "merged #7 tip m7\n"; out != want {
		t.Errorf("report = %q, want %q", out, want)
	}
	if !slices.Contains(gh.calls, mergeMutation) {
		t.Errorf("merge never reached GitHub: %v", gh.calls)
	}
}

func TestVcsMergeComparesARedJobsFailingTestsAcrossTheMainRunsJobs(t *testing.T) {
	const inherited, introduced = "tests/test_fail_open.py::test_every_refusal[go_posts_parse - kept]", "tests/test_fail_open.py::test_every_refusal[go_posts_parse - new]"
	shard := mergeCheck("pytest 1/12", "FAILURE", 11, "2026-11-05T10:00:00Z")
	shard.DatabaseID = 101
	jobs := `{"jobs":[{"id":201,"name":"pytest 1/12","status":"completed","conclusion":"success"},{"id":212,"name":"pytest 12/12","status":"completed","conclusion":"failure"}]}`
	for _, tc := range []struct {
		name, jobs, onPR, onMain, refusal string
	}{
		{name: "the same test red in another shard on main", onPR: pytestJobLog(inherited), onMain: pytestJobLog(inherited)},
		{name: "a test red only on the PR", onPR: pytestJobLog(introduced), onMain: pytestJobLog(inherited), refusal: "#7 pytest 1/12 (" + introduced + ") failed, and run 99 on main does not fail the same tests"},
		{
			name: "a different test red in the same shard on main", onPR: pytestJobLog(introduced), onMain: pytestJobLog(inherited),
			jobs:    `{"jobs":[{"id":212,"name":"pytest 1/12","status":"completed","conclusion":"failure"}]}`,
			refusal: "#7 pytest 1/12 (" + introduced + ") failed, and run 99 on main does not fail the same tests",
		},
		{name: "a PR log that names no test", onPR: "", onMain: pytestJobLog(inherited), refusal: "#7 pytest 1/12 failed, and run 99 on main does not fail the same tests"},
		{
			name: "a read PR log that names no test, its job red on main", onPR: "", onMain: pytestJobLog(inherited),
			jobs: `{"jobs":[{"id":212,"name":"pytest 1/12","status":"completed","conclusion":"failure"}]}`,
		},
		{name: "a PR log it cannot read", onPR: refused(http.StatusGone, "Log expired"), onMain: pytestJobLog(inherited), refusal: "read job 101's log"},
		{name: "a main log it cannot read", onPR: pytestJobLog(introduced), onMain: refused(http.StatusGone, "Log expired"), refusal: "read job 212's log"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gh, client := serveMergeGitHub(t, map[string]string{
				mergeViewer:     viewer("yasyf"),
				"POST /graphql": mergeRead(mergePull(7, "OPEN", "tip", shard)),
				"GET /repos/yasyf/cc-context/actions/runs/99":       `{"head_branch":"main","event":"push","workflow_id":11}`,
				"GET /repos/yasyf/cc-context/actions/runs/99/jobs":  cmp.Or(tc.jobs, jobs),
				"GET /repos/yasyf/cc-context/actions/jobs/101/logs": tc.onPR,
				"GET /repos/yasyf/cc-context/actions/jobs/212/logs": tc.onMain,
				mergeMutation: mergedByGraphQL(7, "tip"),
			})
			out, err := runMergeCmd(withMergeClient(t.Context(), client), t, "7", "--repo", "yasyf/cc-context", "--admin", "--match-head-commit", "head7", "--red-on-main", "99")
			if tc.refusal == "" {
				if err != nil || out != "merged #7 tip m7\n" {
					t.Fatalf("merge = %v, %q; want merged #7 tip m7", err, out)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.refusal) {
				t.Fatalf("merge = %v, want %q", err, tc.refusal)
			}
			if gh.merged() {
				t.Errorf("merge reached GitHub: %v", gh.calls)
			}
		})
	}
}

func vcsMergeQueueRepo(t *testing.T, queue string) context.Context {
	t.Helper()
	if queue == "mixed" {
		f := shipRepo(t, vcstest.GT(), vcstest.Remote())
		shipGTStack(t, f, "legacy")
		return stubMergeQueueRead(t, f, "git@github.com:yasyf/cc-context.git")
	}
	f := shipRepo(t, vcstest.Remote())
	ctx := stubMergeQueueRead(t, f, "git@github.com:yasyf/cc-context.git")
	mustRun(t, f.Env(), f.Dir, "git", "config", "ccx.queue", queue)
	return ctx
}

func TestVcsMergeRefusesAQueueWithoutAdminAndAdminOnAnotherAccountsRepository(t *testing.T) {
	for _, tc := range []struct {
		name, queue, repo, want string
		args                    []string
	}{
		{name: "this checkout's queue", queue: "trunk", repo: "yasyf/cc-context", want: "yasyf/cc-context lands through the trunk queue"},
		{name: "a mixed queue", queue: "mixed", repo: "yasyf/cc-context", want: "yasyf/cc-context lands through the mixed queue"},
		{name: "another repository", queue: "trunk", repo: "org/elsewhere", want: "is not a checkout of org/elsewhere"},
		{name: "--admin on a repository another account owns", queue: "trunk", repo: "yasyf/cc-context", want: "--admin works only on a repository your own account owns, and yasyf/cc-context belongs to yasyf, not someone", args: []string{"--admin"}},
		{name: "--admin on a repository outside this checkout", queue: "trunk", repo: "org/elsewhere", want: "--admin works only on a repository your own account owns", args: []string{"--admin"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := vcsMergeQueueRepo(t, tc.queue)
			gh, client := serveMergeGitHub(t, map[string]string{mergeViewer: viewer("someone")})
			_, err := runMergeCmd(withMergeClient(ctx, client), t, append([]string{"7", "--repo", tc.repo, "--match-head-commit", "head7"}, tc.args...)...)
			if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "pass --admin") {
				t.Fatalf("merge = %v, want %q with no --admin way past it", err, tc.want)
			}
			if slices.ContainsFunc(gh.calls, func(call string) bool { return call != mergeViewer }) {
				t.Errorf("merge reached GitHub: %v", gh.calls)
			}
		})
	}
}

// TestVcsMergeAdminPassesTheQueueOfARepositoryTheViewerOwns is yasyf/cc-skills
// #2535: a leftover Graphite config read as a mixed queue, and neither ccx vcs
// merge --admin nor gh pr merge could land a native stack member, on a
// repository whose owner merges with --admin.
func TestVcsMergeAdminPassesTheQueueOfARepositoryTheViewerOwns(t *testing.T) {
	for _, tc := range []struct{ name, queue, repo string }{
		{"trunk", "trunk", "yasyf/cc-context"},
		{"mixed", "mixed", "yasyf/cc-context"},
		{"not this checkout", "trunk", "yasyf/elsewhere"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := vcsMergeQueueRepo(t, tc.queue)
			_, client := serveMergeGitHub(t, map[string]string{
				mergeViewer:     viewer("YASYF"),
				"POST /graphql": mergeRead(mergePull(7, "OPEN", "tip")),
				mergeMutation:   mergedByGraphQL(7, "tip"),
			})

			out, err := runMergeCmd(withMergeClient(ctx, client), t, "7", "--repo", tc.repo, "--admin", "--match-head-commit", "head7")
			if err != nil {
				t.Fatalf("merge: %v\n%s", err, out)
			}
			if want := "merged #7 tip m7\n"; out != want {
				t.Errorf("report = %q, want %q", out, want)
			}
		})
	}
}

func TestVcsMergeRefusesWhenItCannotTellTheAccount(t *testing.T) {
	ctx := vcsMergeQueueRepo(t, "trunk")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "Resource not accessible by integration", http.StatusForbidden)
	}))
	defer srv.Close()

	_, err := runMergeCmd(withMergeClient(ctx, nativeapi.NewWithToken(srv.URL, "key")), t, "7", "--repo", "yasyf/cc-context", "--admin", "--match-head-commit", "head7")
	if err == nil || !strings.Contains(err.Error(), "read the signed-in account") {
		t.Fatalf("merge = %v, want the failed account read", err)
	}
}
