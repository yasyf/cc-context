package gtapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/yasyf/cc-context/internal/version"
)

func testClient(baseURL string) *Client {
	c := New(baseURL)
	c.tokens.resolve = func() (string, error) { return "gt-token", nil }
	return c
}

func assertHeaders(t *testing.T, r *http.Request, wantBody bool) {
	t.Helper()
	if got := r.Header.Get("Authorization"); got != "token gt-token" {
		t.Errorf("Authorization = %q, want %q", got, "token gt-token")
	}
	if want := "ccx-gt/" + version.String(); r.Header.Get("User-Agent") != want {
		t.Errorf("User-Agent = %q, want %q", r.Header.Get("User-Agent"), want)
	}
	if wantBody {
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
	}
}

func decodeBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var sent map[string]any
	if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	return sent
}

func TestCheckAuth(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/graphite/check-auth" {
			t.Errorf("request = %s %s, want POST /graphite/check-auth", r.Method, r.URL.Path)
		}
		assertHeaders(t, r, true)
		sent := decodeBody(t, r)
		if sent["repoOwner"] != "Forge-AI" || sent["repoName"] != "cc-context" {
			t.Errorf("params = %v, want repoOwner=Forge-AI repoName=cc-context", sent)
		}
		_, _ = fmt.Fprint(w, `{"githubLogin":"yasyf","canSubmitPrs":false}`)
	}))
	t.Cleanup(ts.Close)

	got, err := testClient(ts.URL).CheckAuth(context.Background(), "Forge-AI", "cc-context")
	if err != nil {
		t.Fatalf("CheckAuth: %v", err)
	}
	if got != (CheckAuthResponse{GithubLogin: "yasyf", CanSubmitPrs: false}) {
		t.Errorf("CheckAuth = %+v, want {yasyf false}", got)
	}
}

func TestIsRepoSynced(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/graphite/cli/is-repo-synced" {
			t.Errorf("request = %s %s, want GET /graphite/cli/is-repo-synced", r.Method, r.URL.Path)
		}
		assertHeaders(t, r, false)
		q := r.URL.Query()
		if q.Get("repoOwner") != "Forge-AI" || q.Get("repoName") != "monorepo" {
			t.Errorf("query = %v, want repoOwner=Forge-AI repoName=monorepo", q)
		}
		_, _ = fmt.Fprint(w, `{"result":{"status":"SYNCED"}}`)
	}))
	t.Cleanup(ts.Close)

	got, err := testClient(ts.URL).IsRepoSynced(context.Background(), "Forge-AI", "monorepo")
	if err != nil {
		t.Fatalf("IsRepoSynced: %v", err)
	}
	if got.Status != RepoSynced {
		t.Errorf("status = %q, want SYNCED", got.Status)
	}
}

func TestIsRepoSyncedUnaddableCarriesMessage(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"result":{"status":"NOT_SYNCED_UNADDABLE","message":"repo is private to another org"}}`)
	}))
	t.Cleanup(ts.Close)

	got, err := testClient(ts.URL).IsRepoSynced(context.Background(), "o", "r")
	if err != nil {
		t.Fatalf("IsRepoSynced: %v", err)
	}
	if got.Status != RepoNotSyncedUnaddable || got.Message != "repo is private to another org" {
		t.Errorf("IsRepoSynced = %+v, want {NOT_SYNCED_UNADDABLE, repo is private to another org}", got)
	}
}

const pullRequestInfoFixture = `{"result":{"status":"ok","prs":[{"prNumber":17096,"title":"sandsql: dormant markers keep the pool parked across deploys","body":"## Context\n\nBlue/green SandSQL deploys hand the lease from blue to green","authorGithubHandle":"yasyf","state":"OPEN","reviewDecision":"APPROVED","url":"https://app.graphite.com/github/pr/Forge-AI/monorepo/17096","headRefName":"yasyf/park-across-deploys","baseRefName":"dev","isBaseRefGraphiteBase":false,"isDraft":false,"versions":[{"headSha":"5374451bde131fb6f5353136eaed91c70f48ffbc","baseSha":"24b2e2e374b934ca7fed076641d7bd4a77cefb09","baseName":"yasyf/boot-epoch-restamp","createdAt":"2026-08-30T12:01:48.213Z","authorGithubHandle":"yasyf","isGraphiteGenerated":true},{"headSha":"4ba73b7062dc5c3e8b4097beb159d52faf279248","baseSha":"4e0f6df1f02ffe4be418177a8626a630170063ad","baseName":"dev","createdAt":"2026-08-30T14:55:30.756Z","isGraphiteGenerated":false}],"cliTrunk":"dev"}]}}`

func TestPullRequestInfo(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/graphite/cli/pull-request-info" {
			t.Errorf("request = %s %s, want POST /graphite/cli/pull-request-info", r.Method, r.URL.Path)
		}
		assertHeaders(t, r, true)
		sent := decodeBody(t, r)
		if sent["repoOwner"] != "Forge-AI" || sent["repoName"] != "monorepo" {
			t.Errorf("params = %v, want repoOwner=Forge-AI repoName=monorepo", sent)
		}
		if nums, ok := sent["prNumbers"].([]any); !ok || len(nums) != 1 || nums[0] != float64(17096) {
			t.Errorf("prNumbers = %v, want [17096]", sent["prNumbers"])
		}
		if sent["consistent"] != false {
			t.Errorf("consistent = %v, want false", sent["consistent"])
		}
		if sent["callsite"] != "ccx" {
			t.Errorf("callsite = %v, want ccx", sent["callsite"])
		}
		_, _ = fmt.Fprint(w, pullRequestInfoFixture)
	}))
	t.Cleanup(ts.Close)

	got, err := testClient(ts.URL).PullRequestInfo(context.Background(), PullRequestInfoRequest{
		RepoOwner: "Forge-AI",
		RepoName:  "monorepo",
		PRNumbers: []int{17096},
		Callsite:  "ccx",
	})
	if err != nil {
		t.Fatalf("PullRequestInfo: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("prs = %d, want 1", len(got))
	}
	pr := got[0]
	if pr.PRNumber != 17096 || pr.State != PROpen || pr.HeadRefName != "yasyf/park-across-deploys" || pr.BaseRefName != "dev" {
		t.Errorf("pr = %+v, want 17096 OPEN yasyf/park-across-deploys dev", pr)
	}
	if pr.ReviewDecision != "APPROVED" || pr.IsDraft || pr.CliTrunk != "dev" {
		t.Errorf("pr = %+v, want APPROVED, not draft, cliTrunk dev", pr)
	}
	if len(pr.Versions) != 2 || pr.Versions[0].BaseName != "yasyf/boot-epoch-restamp" || !pr.Versions[0].IsGraphiteGenerated {
		t.Errorf("versions = %+v, want 2 with first based on yasyf/boot-epoch-restamp", pr.Versions)
	}
}

func TestPullRequestInfoErrorResultIsTyped(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"result":{"status":"error","message":"repo not found"}}`)
	}))
	t.Cleanup(ts.Close)

	_, err := testClient(ts.URL).PullRequestInfo(context.Background(), PullRequestInfoRequest{PRNumbers: []int{1}})
	var resultErr *ResultError
	if !errors.As(err, &resultErr) || resultErr.Message != "repo not found" {
		t.Fatalf("PullRequestInfo error = %v, want *ResultError{repo not found}", err)
	}
}

func TestPreSubmitPullRequests(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/graphite/cli/submit/pre-submit-pull-requests" {
			t.Errorf("request = %s %s, want POST /graphite/cli/submit/pre-submit-pull-requests", r.Method, r.URL.Path)
		}
		assertHeaders(t, r, true)
		sent := decodeBody(t, r)
		branches, ok := sent["branches"].([]any)
		if !ok || len(branches) != 2 {
			t.Fatalf("branches = %v, want 2 entries", sent["branches"])
		}
		first := branches[0].(map[string]any)
		if first["headRefName"] != "yasyf/park-across-deploys" || first["prNumber"] != float64(17096) {
			t.Errorf("branches[0] = %v, want yasyf/park-across-deploys 17096", first)
		}
		second := branches[1].(map[string]any)
		if second["headRefName"] != "yasyf/new-branch" {
			t.Errorf("branches[1] = %v, want yasyf/new-branch", second)
		}
		if _, present := second["prNumber"]; present {
			t.Errorf("branches[1] = %v, want no prNumber", second)
		}
		_, _ = fmt.Fprint(w, `{"result":{"retargetedPrs":[]}}`)
	}))
	t.Cleanup(ts.Close)

	got, err := testClient(ts.URL).PreSubmitPullRequests(context.Background(), "Forge-AI", "monorepo", []PreSubmitBranch{
		{HeadRefName: "yasyf/park-across-deploys", PRNumber: 17096},
		{HeadRefName: "yasyf/new-branch"},
	})
	if err != nil {
		t.Fatalf("PreSubmitPullRequests: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("retargetedPrs = %v, want none", got)
	}
}

func TestPreSubmitErrorResultIsTyped(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"result":{"error":"repo not synced"}}`)
	}))
	t.Cleanup(ts.Close)

	_, err := testClient(ts.URL).PreSubmitPullRequests(context.Background(), "o", "r", nil)
	var resultErr *ResultError
	if !errors.As(err, &resultErr) || resultErr.Message != "repo not synced" {
		t.Fatalf("PreSubmitPullRequests error = %v, want *ResultError{repo not synced}", err)
	}
}

const submitFixture = `{"prs":[{"head":"yasyf/park-across-deploys","headSha":"16bf1a961379f905613734391dd3438f8a3222c9","base":"dev","baseSha":"4e0f6df1f02ffe4be418177a8626a630170063ad","prNumber":17096,"prURL":"https://app.graphite.com/github/pr/Forge-AI/monorepo/17096","status":"updated","repoName":"monorepo","repoOwner":"Forge-AI","githubId":"PR_kwDOJwz6x88AAAABBgD_aA","forgeSource":"github"}]}`

func TestSubmitPullRequests(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/graphite/submit/pull-requests" {
			t.Errorf("request = %s %s, want POST /graphite/submit/pull-requests", r.Method, r.URL.Path)
		}
		assertHeaders(t, r, true)
		sent := decodeBody(t, r)
		if sent["trunkBranchName"] != "dev" || sent["mergeWhenReady"] != false || sent["rerequestReview"] != false {
			t.Errorf("params = %v, want trunkBranchName=dev mergeWhenReady=false rerequestReview=false", sent)
		}
		prs := sent["prs"].([]any)
		if len(prs) != 2 {
			t.Fatalf("prs = %d entries, want 2", len(prs))
		}
		update := prs[0].(map[string]any)
		if update["action"] != "update" || update["prNumber"] != float64(17096) || update["headSha"] != "16bf1a961379f905613734391dd3438f8a3222c9" {
			t.Errorf("prs[0] = %v, want update of 17096", update)
		}
		create := prs[1].(map[string]any)
		if create["action"] != "create" || create["title"] != "vcs: submit over http" || create["body"] != "A body." {
			t.Errorf("prs[1] = %v, want create with title and body", create)
		}
		if _, present := create["prNumber"]; present {
			t.Errorf("prs[1] = %v, want no prNumber", create)
		}
		_, _ = fmt.Fprint(w, submitFixture)
	}))
	t.Cleanup(ts.Close)

	got, err := testClient(ts.URL).SubmitPullRequests(context.Background(), SubmitRequest{
		RepoOwner:       "Forge-AI",
		RepoName:        "monorepo",
		TrunkBranchName: "dev",
		PRs: []SubmitPR{
			{
				Action:   SubmitUpdate,
				Head:     "yasyf/park-across-deploys",
				HeadSha:  "16bf1a961379f905613734391dd3438f8a3222c9",
				Base:     "dev",
				BaseSha:  "4e0f6df1f02ffe4be418177a8626a630170063ad",
				PRNumber: 17096,
			},
			{
				Action:  SubmitCreate,
				Head:    "yasyf/submit-over-http",
				HeadSha: "a41a1f0775d71ff3470db48f998a116cb10f9562",
				Base:    "yasyf/park-across-deploys",
				BaseSha: "16bf1a961379f905613734391dd3438f8a3222c9",
				Title:   "vcs: submit over http",
				Body:    "A body.",
			},
		},
	})
	if err != nil {
		t.Fatalf("SubmitPullRequests: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("submitted = %d, want 1", len(got))
	}
	want := SubmittedPR{
		Head:     "yasyf/park-across-deploys",
		PRNumber: 17096,
		PRURL:    "https://app.graphite.com/github/pr/Forge-AI/monorepo/17096",
		Status:   "updated",
	}
	if got[0].Head != want.Head || got[0].PRNumber != want.PRNumber || got[0].PRURL != want.PRURL || got[0].Status != want.Status {
		t.Errorf("submitted[0] = %+v, want %+v", got[0], want)
	}
}

func TestSubmitPerBranchFailureIsTyped(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"prs":[{"head":"yasyf/landed","prNumber":42,"prURL":"https://app.graphite.com/github/pr/o/r/42","status":"created","repoOwner":"o","repoName":"r"},{"head":"yasyf/refused","error":"base branch not found","status":"error"}]}`)
	}))
	t.Cleanup(ts.Close)

	got, err := testClient(ts.URL).SubmitPullRequests(context.Background(), SubmitRequest{})
	if got != nil {
		t.Errorf("submitted = %+v, want nil on error", got)
	}
	var submitErr *SubmitError
	if !errors.As(err, &submitErr) {
		t.Fatalf("SubmitPullRequests error = %v, want *SubmitError", err)
	}
	if len(submitErr.Submitted) != 1 || submitErr.Submitted[0].Head != "yasyf/landed" || submitErr.Submitted[0].PRNumber != 42 {
		t.Errorf("Submitted = %+v, want [yasyf/landed 42]", submitErr.Submitted)
	}
	if len(submitErr.Failed) != 1 || submitErr.Failed[0] != (BranchSubmitError{Head: "yasyf/refused", Message: "base branch not found"}) {
		t.Errorf("Failed = %+v, want [{yasyf/refused base branch not found}]", submitErr.Failed)
	}
	if want := "gtapi: submit: 1 of 2 branches failed: yasyf/refused: base branch not found"; err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}

func TestUnauthorizedIsTyped(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprint(w, `{"message":"invalid token"}`)
	}))
	t.Cleanup(ts.Close)

	_, err := testClient(ts.URL).CheckAuth(context.Background(), "o", "r")
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("CheckAuth error = %v, want ErrUnauthorized", err)
	}
	var status *StatusError
	if !errors.As(err, &status) || status.Status != http.StatusUnauthorized || status.Message != "invalid token" {
		t.Errorf("CheckAuth error = %v, want *StatusError{401, invalid token}", err)
	}
}

func TestResolveTokenReadsAuthFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".config", "graphite")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "auth"), []byte(`{"authToken":"fixture-token"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	token, err := resolveToken()
	if err != nil {
		t.Fatalf("resolveToken: %v", err)
	}
	if token != "fixture-token" {
		t.Errorf("token = %q, want fixture-token", token)
	}
}

func TestResolveTokenWithoutFileIsTyped(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	_, err := resolveToken()
	if !errors.Is(err, ErrNoToken) {
		t.Fatalf("resolveToken error = %v, want ErrNoToken", err)
	}
}
