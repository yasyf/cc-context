package nativeapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/render"
)

func fastMergePoll(t *testing.T) {
	t.Helper()
	poll := mergePoll
	mergePoll = 0
	t.Cleanup(func() { mergePoll = poll })
}

func TestReadMergeListsTheOpenPullRequestsBelowAStackMemberBottomUp(t *testing.T) {
	pull := func(n int, state string) string {
		return fmt.Sprintf(`{"number":%d,"state":%q,"headRefName":"b%d","headRefOid":"h%d","commits":{"nodes":[{"commit":{"statusCheckRollup":null}}]}}`, n, state, n, n)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reply := func(body string) {
			if _, err := fmt.Fprint(w, body); err != nil {
				t.Errorf("write fixture response: %v", err)
			}
		}
		switch call := r.Method + " " + r.URL.Path; call {
		case "GET /repos/org/repo/pulls":
			if got := r.URL.Query().Get("head"); got != "org:b5" {
				t.Errorf("head lookup = %q, want org:b5", got)
			}
			reply(`[{"number":5,"state":"open"}]`)
		case "POST /graphql":
			query, err := io.ReadAll(r.Body)
			if err != nil || !strings.Contains(string(query), "pullRequest(number:5)") {
				t.Errorf("merge read = %s, %v; want PR #5", query, err)
			}
			reply(fmt.Sprintf(`{"data":{"repository":{"defaultBranchRef":{"name":"main"},"pullRequest":{"number":5,"state":"OPEN","headRefName":"b5","headRefOid":"h5","commits":{"nodes":[]},"mergeCommit":null,"stackEntry":{"position":4},"stack":{"entries":{"nodes":[%s,%s,%s,%s,%s],"pageInfo":{"hasNextPage":false}}}}}}}`,
				`{"position":5,"pullRequest":`+pull(6, "OPEN")+`}`,
				`{"position":3,"pullRequest":`+pull(4, "OPEN")+`}`,
				`{"position":1,"pullRequest":`+pull(2, "MERGED")+`}`,
				`{"position":2,"pullRequest":`+pull(3, "OPEN")+`}`,
				`{"position":4,"pullRequest":`+pull(5, "OPEN")+`}`))
		default:
			t.Errorf("unexpected %s", call)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	plan, err := NewWithToken(srv.URL, "key").ReadMerge(context.Background(), MergeRequest{RepoOwner: "org", RepoName: "repo", Head: "b5"})
	if err != nil {
		t.Fatalf("read merge: %v", err)
	}
	below := make([]string, 0, len(plan.Below))
	for _, pr := range plan.Below {
		below = append(below, fmt.Sprintf("#%d %s %s", pr.Number, pr.Head, pr.HeadSHA))
	}
	if want := []string{"#3 b3 h3", "#4 b4 h4"}; !reflect.DeepEqual(below, want) || !plan.Stacked || plan.Pull.HeadSHA != "h5" || plan.DefaultBranch != "main" {
		t.Fatalf("plan = %+v, below %v, want stacked #5 at h5 on main over %v", plan, below, want)
	}
}

func TestMergePullRequestLandsAStackMemberWithTheOpenOnesBelowAndReadsEachBack(t *testing.T) {
	fastMergePoll(t)
	var asked map[string]any
	polls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reply := func(body string) {
			if _, err := fmt.Fprint(w, body); err != nil {
				t.Errorf("write fixture response: %v", err)
			}
		}
		switch call := r.Method + " " + r.URL.Path; call {
		case "PUT /repos/org/repo/pulls/5/merge-async":
			if err := json.NewDecoder(r.Body).Decode(&asked); err != nil {
				t.Errorf("decode merge body: %v", err)
			}
			w.WriteHeader(http.StatusAccepted)
			reply(`{"status":"pending","details":{"message":"Merge request enqueued.","uuid":"u-1"}}`)
		case "GET /repos/org/repo/pulls/5/merge-async/u-1":
			polls++
			if polls == 1 {
				reply(`{"status":"pending","details":{"message":"Merge request is in progress.","uuid":"u-1"}}`)
				return
			}
			reply(`{"status":"merged","details":{"message":"Pull request was merged.","sha":"m5"}}`)
		case "GET /repos/org/repo/pulls/4":
			reply(`{"number":4,"state":"closed","merged":true,"merge_commit_sha":"m4","head":{"ref":"low"}}`)
		case "GET /repos/org/repo/pulls/5":
			reply(`{"number":5,"state":"closed","merged":true,"merge_commit_sha":"m5","head":{"ref":"mid"}}`)
		default:
			t.Errorf("unexpected %s", call)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	plan := MergePlan{Pull: MergeCandidate{Number: 5, Head: "mid", HeadSHA: "head5"}, State: PROpen, Stacked: true, Below: []MergeCandidate{{Number: 4, Head: "low", HeadSHA: "head4"}}}
	got, err := NewWithToken(srv.URL, "key").MergePullRequest(context.Background(), MergeRequest{RepoOwner: "org", RepoName: "repo", Method: "squash", Admin: true}, plan)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	want := []MergedPR{{Number: 4, Head: "low", Status: "merged", SHA: "m4"}, {Number: 5, Head: "mid", Status: "merged", SHA: "m5"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merged = %+v, want %+v", got, want)
	}
	if wantBody := (map[string]any{"sha": "head5", "merge_method": "squash", "merge_action": "direct_merge", "bypass_rules": true}); !reflect.DeepEqual(asked, wantBody) {
		t.Errorf("merge body = %v, want %v", asked, wantBody)
	}
}

func TestMergePullRequestResumesAPendingRequestAndReportsItsOutcome(t *testing.T) {
	fastMergePoll(t)
	for _, tc := range []struct {
		name, result string
		want         []MergedPR
		err          string
	}{
		{name: "merged", result: `{"status":"merged","details":{"sha":"m7"}}`, want: []MergedPR{{Number: 7, Head: "tip", Status: "merged", SHA: "m7"}}},
		{name: "enqueued", result: `{"status":"enqueued","details":{"message":"Pull request was added to the merge queue."}}`, want: []MergedPR{{Number: 7, Head: "tip", Status: "enqueued"}}},
		{name: "failed", result: `{"status":"failed","details":{"message":"Required status check \"test\" is expected."}}`, err: `merge PR #7 failed: Required status check "test" is expected.`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var asked map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reply := func(body string) {
					if _, err := fmt.Fprint(w, body); err != nil {
						t.Errorf("write fixture response: %v", err)
					}
				}
				switch call := r.Method + " " + r.URL.Path; call {
				case "PUT /repos/org/repo/pulls/7/merge-async":
					if err := json.NewDecoder(r.Body).Decode(&asked); err != nil {
						t.Errorf("decode merge body: %v", err)
					}
					w.WriteHeader(http.StatusConflict)
					reply(`{"status":"pending","details":{"message":"A merge request is already pending.","uuid":"u-2"}}`)
				case "GET /repos/org/repo/pulls/7/merge-async/u-2":
					reply(tc.result)
				case "GET /repos/org/repo/pulls/7":
					reply(`{"number":7,"state":"closed","merged":true,"merge_commit_sha":"m7","head":{"ref":"tip"}}`)
				default:
					t.Errorf("unexpected %s", call)
					http.Error(w, "unexpected", http.StatusInternalServerError)
				}
			}))
			defer srv.Close()

			plan := MergePlan{Pull: MergeCandidate{Number: 7, Head: "tip", HeadSHA: "head7"}, State: PROpen, Stacked: true}
			got, err := NewWithToken(srv.URL, "key").MergePullRequest(context.Background(), MergeRequest{RepoOwner: "org", RepoName: "repo", Method: "rebase"}, plan)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("merge error = %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("merged = %+v, %v, want %+v", got, err, tc.want)
			}
			if wantBody := (map[string]any{"sha": "head7", "merge_method": "rebase", "merge_action": "default", "bypass_rules": false}); !reflect.DeepEqual(asked, wantBody) {
				t.Errorf("merge body = %v, want %v", asked, wantBody)
			}
		})
	}
}

func TestMergePullRequestReportsAPlainMergeGitHubRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query, err := io.ReadAll(r.Body)
		if call := r.Method + " " + r.URL.Path; call != "POST /graphql" || err != nil || !strings.Contains(string(query), `expectedHeadOid:\"head9\"`) {
			t.Errorf("unexpected %s %s (%v)", call, query, err)
			http.Error(w, "unexpected", http.StatusInternalServerError)
			return
		}
		if _, err := fmt.Fprint(w, `{"data":{"mergePullRequest":null},"errors":[{"type":"UNPROCESSABLE","path":["mergePullRequest"],"message":"Head branch was modified. Review and try the merge again."}]}`); err != nil {
			t.Errorf("write fixture response: %v", err)
		}
	}))
	defer srv.Close()

	plan := MergePlan{Pull: MergeCandidate{Number: 9, Head: "tip", HeadSHA: "head9"}, PullID: "PR_9", State: PROpen}
	_, err := NewWithToken(srv.URL, "key").MergePullRequest(context.Background(), MergeRequest{RepoOwner: "org", RepoName: "repo", Method: "squash"}, plan)
	if err == nil || !strings.Contains(err.Error(), "merge PR #9") || !strings.Contains(err.Error(), "Head branch was modified") {
		t.Fatalf("merge = %v, want GitHub's refusal of PR #9", err)
	}
}

func TestMergePullRequestRetriesAStackMembersMergeOncePastASecondaryLimit(t *testing.T) {
	limitWaits := mergeLimitWaits
	mergeLimitWaits = []time.Duration{0}
	t.Cleanup(func() { mergeLimitWaits = limitWaits })
	for _, tc := range []struct {
		name    string
		limited int
		err     string
	}{
		{name: "one limit", limited: 1},
		{name: "a second limit", limited: 2, err: "secondary rate limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			puts := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reply := func(body string) {
					if _, err := fmt.Fprint(w, body); err != nil {
						t.Errorf("write fixture response: %v", err)
					}
				}
				switch call := r.Method + " " + r.URL.Path; call {
				case "PUT /repos/org/repo/pulls/5/merge-async":
					puts++
					if puts <= tc.limited {
						w.Header().Set("Retry-After", "0")
						w.WriteHeader(http.StatusForbidden)
						reply(`{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again."}`)
						return
					}
					reply(`{"status":"merged","details":{"sha":"m5"}}`)
				case "GET /repos/org/repo/pulls/5":
					reply(`{"number":5,"state":"closed","merged":true,"merge_commit_sha":"m5","head":{"ref":"mid"}}`)
				default:
					t.Errorf("unexpected %s", call)
					http.Error(w, "unexpected", http.StatusInternalServerError)
				}
			}))
			defer srv.Close()

			var announced bytes.Buffer
			plan := MergePlan{Pull: MergeCandidate{Number: 5, Head: "mid", HeadSHA: "head5"}, State: PROpen, Stacked: true}
			got, err := NewWithToken(srv.URL, "key").MergePullRequest(render.WithSlowWriter(context.Background(), &announced), MergeRequest{RepoOwner: "org", RepoName: "repo", Method: "squash", Admin: true}, plan)
			if puts != 2 {
				t.Errorf("merge-async PUTs = %d, want the request and one retry", puts)
			}
			if want := "ccx: GitHub rate-limited PUT /repos/org/repo/pulls/5/merge-async, retrying in 0s (wait 1 of 1)\n"; announced.String() != want {
				t.Errorf("announced %q, want %q", announced.String(), want)
			}
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("merge = %v, want the second limit's refusal", err)
				}
				return
			}
			if want := []MergedPR{{Number: 5, Head: "mid", Status: "merged", SHA: "m5"}}; err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("merged = %+v, %v, want %+v", got, err, want)
			}
		})
	}
}
