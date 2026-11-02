package nativeapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func fastMergePoll(t *testing.T) {
	t.Helper()
	poll := mergePoll
	mergePoll = 0
	t.Cleanup(func() { mergePoll = poll })
}

func TestMergePullRequestMergesAStackedPullRequestWithEveryOpenOneBelow(t *testing.T) {
	fastMergePoll(t)
	var calls []string
	var asked map[string]any
	polls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reply := func(body string) {
			if _, err := fmt.Fprint(w, body); err != nil {
				t.Errorf("write fixture response: %v", err)
			}
		}
		call := r.Method + " " + r.URL.Path
		calls = append(calls, call)
		merged := polls > 1
		switch call {
		case "GET /repos/org/repo/pulls":
			if got := r.URL.Query().Get("head"); got != "org:mid" {
				t.Errorf("head lookup = %q, want org:mid", got)
			}
			reply(`[{"number":5,"state":"open","head":{"ref":"mid"}}]`)
		case "GET /repos/org/repo/pulls/4":
			reply(fmt.Sprintf(`{"number":4,"state":"closed","merged":%t,"merge_commit_sha":"m4","head":{"ref":"low"}}`, merged))
		case "GET /repos/org/repo/pulls/5":
			if merged {
				reply(`{"number":5,"state":"closed","merged":true,"merge_commit_sha":"m5","head":{"ref":"mid","sha":"head5"}}`)
				return
			}
			reply(`{"number":5,"state":"open","head":{"repo":{"full_name":"org/repo"},"ref":"mid","sha":"head5"},"base":{"ref":"low"},"stack":{"number":8,"position":2,"size":3,"base":{"ref":"main"}}}`)
		case "GET /repos/org/repo/stacks/8":
			reply(`{"number":8,"pull_requests":[{"number":3,"state":"closed"},{"number":4,"state":"open"},{"number":5,"state":"open"},{"number":6,"state":"open"}]}`)
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
		default:
			t.Errorf("unexpected %s", call)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	got, err := NewWithToken(srv.URL, "key").MergePullRequest(context.Background(), MergeRequest{RepoOwner: "org", RepoName: "repo", Head: "mid", Method: "squash", Admin: true})
	if err != nil {
		t.Fatalf("merge: %v (calls %v)", err, calls)
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
				case "GET /repos/org/repo/pulls/7":
					reply(`{"number":7,"state":"open","head":{"ref":"tip","sha":"head7"},"base":{"ref":"main"}}`)
				case "PUT /repos/org/repo/pulls/7/merge-async":
					if err := json.NewDecoder(r.Body).Decode(&asked); err != nil {
						t.Errorf("decode merge body: %v", err)
					}
					w.WriteHeader(http.StatusConflict)
					reply(`{"status":"pending","details":{"message":"A merge request is already pending.","uuid":"u-2"}}`)
				case "GET /repos/org/repo/pulls/7/merge-async/u-2":
					reply(tc.result)
				default:
					t.Errorf("unexpected %s", call)
					http.Error(w, "unexpected", http.StatusInternalServerError)
				}
			}))
			defer srv.Close()

			got, err := NewWithToken(srv.URL, "key").MergePullRequest(context.Background(), MergeRequest{RepoOwner: "org", RepoName: "repo", PRNumber: 7, Method: "rebase"})
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

func TestMergePullRequestReportsAnAlreadyMergedPullRequestWithoutMergingAgain(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if call := r.Method + " " + r.URL.Path; call != "GET /repos/org/repo/pulls/9" {
			t.Errorf("unexpected %s", call)
			http.Error(w, "unexpected", http.StatusInternalServerError)
			return
		}
		if _, err := fmt.Fprint(w, `{"number":9,"state":"closed","merged":true,"merge_commit_sha":"m9","head":{"ref":"done"}}`); err != nil {
			t.Errorf("write fixture response: %v", err)
		}
	}))
	defer srv.Close()

	got, err := NewWithToken(srv.URL, "key").MergePullRequest(context.Background(), MergeRequest{RepoOwner: "org", RepoName: "repo", PRNumber: 9, Method: "squash"})
	if want := []MergedPR{{Number: 9, Head: "done", Status: "merged", SHA: "m9"}}; err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("merged = %+v, %v, want %+v", got, err, want)
	}
}
