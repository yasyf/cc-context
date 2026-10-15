package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/ghapi"
	"github.com/yasyf/cc-context/internal/gtapi"
)

func stubGraphQLDownREST(t *testing.T) *gtapi.Client {
	t.Helper()
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/graphql":
			w.WriteHeader(http.StatusBadGateway)
		case "/repos/Forge-AI/monorepo":
			_, _ = io.WriteString(w, `{"default_branch":"dev"}`)
		case "/repos/Forge-AI/monorepo/commits":
			_, _ = io.WriteString(w, `[]`)
		case "/repos/Forge-AI/monorepo/pulls/25121":
			_, _ = io.WriteString(w, `{"number":25121,"state":"open","title":"t","base":{"ref":"dev"},"head":{"ref":"yasyf/pr-25121","sha":"a37cf143"},"mergeable":true,"mergeable_state":"clean"}`)
		case "/repos/Forge-AI/monorepo/pulls/25116":
			_, _ = io.WriteString(w, `{"number":25116,"state":"closed","title":"t","merged_at":"2026-09-30T06:00:00Z","merge_commit_sha":"9cc33f055dc4db19da6eb13a210a810297ccdc05","base":{"ref":"dev"},"head":{"ref":"yasyf/pr-25116","sha":"b37cf143"},"mergeable_state":"unknown"}`)
		case "/repos/Forge-AI/monorepo/pulls/25121/reviews", "/repos/Forge-AI/monorepo/pulls/25116/reviews":
			_, _ = io.WriteString(w, `[]`)
		case "/repos/Forge-AI/monorepo/commits/a37cf143/check-runs", "/repos/Forge-AI/monorepo/commits/b37cf143/check-runs":
			_, _ = io.WriteString(w, `{"check_runs":[]}`)
		case "/repos/Forge-AI/monorepo/commits/a37cf143/status", "/repos/Forge-AI/monorepo/commits/b37cf143/status":
			_, _ = io.WriteString(w, `{"state":"pending","statuses":[]}`)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(gh.Close)
	gt := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }))
	t.Cleanup(gt.Close)

	priorAPI, priorRoot := reviewsAPI, prStateRoot
	root := t.TempDir()
	reviewsAPI = func() *ghapi.Client { return ghapi.New(gh.URL) }
	prStateRoot = func() (string, error) { return root, nil }
	t.Cleanup(func() { reviewsAPI, prStateRoot = priorAPI, priorRoot })
	t.Setenv("GH_TOKEN", "pr-state-test-token")
	return gtapi.NewWithToken(gt.URL, "gt-stub-token")
}

func TestPRStatusAnswersFromRESTWhenGraphiteAndGraphQLFail(t *testing.T) {
	client := stubGraphQLDownREST(t)

	out, err := runPRStatusCmd(t, client, "--repo", "Forge-AI/monorepo", "25121", "25116")
	if err != nil {
		t.Fatalf("pr status: %v", err)
	}
	want := "#25121  unknown · open · ci none · no review · blocked:queue-unknown,no-ci,unapproved · via rest\n" +
		"#25116  landed · squash 9cc33f05 on dev · no review · landed · via rest\n"
	if !strings.HasSuffix(out, want) || !strings.Contains(out, "answering from github rest") {
		t.Errorf("report =\n%s\nwant it to end with\n%s", out, want)
	}
}

func TestPRStatusJSONNamesTheRESTPath(t *testing.T) {
	client := stubGraphQLDownREST(t)

	out, err := runPRStatusCmd(t, client, "--repo", "Forge-AI/monorepo", "--json", "25121")
	if err != nil {
		t.Fatalf("pr status --json: %v", err)
	}
	var got []prQueueReport
	if err := json.Unmarshal([]byte(out[strings.Index(out, "[\n"):]), &got); err != nil {
		t.Fatalf("unmarshal %q: %v", out, err)
	}
	if len(got) != 1 || got[0].Queue != prQueueUnknown || got[0].Via != "rest" {
		t.Errorf("report = %+v, want an unknown queue answered via rest", got)
	}
}

func TestPRStatePrintsTheRESTPathThatAnswered(t *testing.T) {
	client := stubGraphQLDownREST(t)
	cmd := newVcsPRCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"state", "--repo", "Forge-AI/monorepo", "25121"})
	if err := cmd.ExecuteContext(withGTAPI(t.Context(), client)); err != nil {
		t.Fatalf("pr state: %v", err)
	}
	var got prStateReport
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal %q: %v", out.String(), err)
	}
	if pr := got.PRs[25121]; got.Trunk != "dev" || pr.Via != "rest" || pr.Graphite != nil || pr.HeadRefOid != "a37cf143" {
		t.Errorf("report = %+v", got)
	}
}
