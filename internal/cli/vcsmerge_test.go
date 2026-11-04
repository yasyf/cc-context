package cli

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/gtapi"
	"github.com/yasyf/cc-context/internal/nativeapi"
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

func TestVcsMergeReportsEachMergeCommitOfTheCurrentBranch(t *testing.T) {
	f := shipRepo(t, vcstest.Remote())
	ctx := stubMergeQueueRead(t, f, "git@github.com:org/repo.git")
	shipGTUntracked(t, f, "tip")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := ""
		switch call := r.Method + " " + r.URL.Path; call {
		case "GET /repos/org/repo/pulls":
			if got := r.URL.Query().Get("head"); got != "org:tip" {
				t.Errorf("head lookup = %q, want org:tip", got)
			}
			body = `[{"number":7,"state":"open"}]`
		case "GET /repos/org/repo/pulls/7":
			body = `{"number":7,"state":"open","head":{"ref":"tip","sha":"head7"},"base":{"ref":"main"}}`
		case "PUT /repos/org/repo/pulls/7/merge-async":
			body = `{"status":"merged","details":{"message":"Pull request was merged.","sha":"m7"}}`
		default:
			t.Errorf("unexpected %s", call)
			http.Error(w, "unexpected", http.StatusInternalServerError)
			return
		}
		if _, err := fmt.Fprint(w, body); err != nil {
			t.Errorf("write fixture response: %v", err)
		}
	}))
	defer srv.Close()

	out, err := runMergeCmd(withMergeClient(ctx, nativeapi.NewWithToken(srv.URL, "key")), t, "--repo", "org/repo", "--admin")
	if err != nil {
		t.Fatalf("merge: %v\n%s", err, out)
	}
	if want := "merged #7 tip m7\n"; out != want {
		t.Errorf("report = %q, want %q", out, want)
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

func TestVcsMergeRefusesToSkipAnotherAccountsQueueEvenWithAdmin(t *testing.T) {
	for _, tc := range []struct {
		name, queue, repo, want string
		args                    []string
	}{
		{name: "this checkout's queue", queue: "trunk", repo: "yasyf/cc-context", want: "yasyf/cc-context lands through the trunk queue"},
		{name: "this checkout's queue with --admin", queue: "trunk", repo: "yasyf/cc-context", want: "yasyf/cc-context lands through the trunk queue", args: []string{"--admin"}},
		{name: "a mixed queue", queue: "mixed", repo: "yasyf/cc-context", want: "yasyf/cc-context lands through the mixed queue"},
		{name: "a mixed queue with --admin", queue: "mixed", repo: "yasyf/cc-context", want: "yasyf/cc-context lands through the mixed queue", args: []string{"--admin"}},
		{name: "another repository with --admin", queue: "trunk", repo: "org/elsewhere", want: "is not a checkout of org/elsewhere", args: []string{"--admin"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := vcsMergeQueueRepo(t, tc.queue)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if call := r.Method + " " + r.URL.Path; call == "GET /user" {
					if _, err := fmt.Fprint(w, `{"login":"someone"}`); err != nil {
						t.Errorf("write fixture response: %v", err)
					}
					return
				}
				t.Errorf("merge reached GitHub: %s %s", r.Method, r.URL.Path)
				http.Error(w, "unexpected", http.StatusInternalServerError)
			}))
			defer srv.Close()

			_, err := runMergeCmd(withMergeClient(ctx, nativeapi.NewWithToken(srv.URL, "key")), t, append([]string{"7", "--repo", tc.repo}, tc.args...)...)
			if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "pass --admin") {
				t.Fatalf("merge = %v, want %q with no --admin way past it", err, tc.want)
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
			_, name, _ := strings.Cut(tc.repo, "/")
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body := ""
				switch call := r.Method + " " + r.URL.Path; call {
				case "GET /user":
					body = `{"login":"YASYF"}`
				case "GET /repos/yasyf/" + name + "/pulls/7":
					body = `{"number":7,"state":"open","head":{"ref":"tip","sha":"head7"},"base":{"ref":"main"}}`
				case "PUT /repos/yasyf/" + name + "/pulls/7/merge-async":
					body = `{"status":"merged","details":{"message":"Pull request was merged.","sha":"m7"}}`
				default:
					t.Errorf("unexpected %s", call)
					http.Error(w, "unexpected", http.StatusInternalServerError)
					return
				}
				if _, err := fmt.Fprint(w, body); err != nil {
					t.Errorf("write fixture response: %v", err)
				}
			}))
			defer srv.Close()

			out, err := runMergeCmd(withMergeClient(ctx, nativeapi.NewWithToken(srv.URL, "key")), t, "7", "--repo", tc.repo, "--admin")
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

	_, err := runMergeCmd(withMergeClient(ctx, nativeapi.NewWithToken(srv.URL, "key")), t, "7", "--repo", "yasyf/cc-context", "--admin")
	if err == nil || !strings.Contains(err.Error(), "lands through the trunk queue") || !strings.Contains(err.Error(), "read the signed-in account") {
		t.Fatalf("merge = %v, want the queue refusal and the failed account read", err)
	}
}
