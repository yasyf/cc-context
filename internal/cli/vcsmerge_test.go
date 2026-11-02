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

func TestVcsMergeReportsEachMergeCommitOfTheCurrentBranch(t *testing.T) {
	f := shipRepo(t, vcstest.Remote())
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

	out, err := runMergeCmd(withMergeClient(f.Context(), nativeapi.NewWithToken(srv.URL, "key")), t, "--repo", "org/repo", "--admin")
	if err != nil {
		t.Fatalf("merge: %v\n%s", err, out)
	}
	if want := "merged #7 tip m7\n"; out != want {
		t.Errorf("report = %q, want %q", out, want)
	}
}

func TestVcsMergeRefusesToSkipAnExternalQueueWithoutAdmin(t *testing.T) {
	for _, tc := range []struct {
		name, repo, want string
	}{
		{name: "this checkout's queue", repo: "yasyf/cc-context", want: "yasyf/cc-context lands through the trunk queue"},
		{name: "another repository", repo: "org/elsewhere", want: "is not a checkout of org/elsewhere"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := shipRepo(t, vcstest.Remote())
			mustRun(t, f.Env(), f.Dir, "git", "remote", "set-url", "origin", "git@github.com:yasyf/cc-context.git")
			mustRun(t, f.Env(), f.Dir, "git", "config", "ccx.queue", "trunk")
			client := gtapi.NewNativeWithToken("https://example.invalid", "unused")
			previous := gtAPINativeDefault
			gtAPINativeDefault = func() *gtapi.Client { return client }
			t.Cleanup(func() { gtAPINativeDefault = previous })
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("merge reached GitHub: %s %s", r.Method, r.URL.Path)
				http.Error(w, "unexpected", http.StatusInternalServerError)
			}))
			defer srv.Close()

			ctx := withMergeClient(withStackProvider(f.Context(), true), nativeapi.NewWithToken(srv.URL, "key"))
			_, err := runMergeCmd(ctx, t, "7", "--repo", tc.repo)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "--admin") {
				t.Fatalf("merge = %v, want %q and --admin offered", err, tc.want)
			}
		})
	}
}
