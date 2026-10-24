package cli

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	prSkipHead      = "a37cf143f3cf1022fcb1c106059b3fb336e44203"
	prSkipChildHead = "c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00"
	prSkipBuild     = "https://buildkite.com/forge/test/builds/78197"
	prFullBuild     = "https://buildkite.com/forge/test/builds/78300"
)

var prBuildkiteJobs = map[string]string{
	"78197": `[{"name":":graphite: Redundant within its stack"},{"name":"label"}]`,
	"78300": `[{"name":"unit"},{"name":"lint"},{"name":"label"}]`,
}

func stubBuildkite(t *testing.T, token string) {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		number := r.URL.Path[strings.LastIndexByte(r.URL.Path, '/')+1:]
		if r.Header.Get("Authorization") != "Bearer bk-test" || prBuildkiteJobs[number] == "" {
			http.Error(w, "unexpected", http.StatusNotFound)
			return
		}
		_, _ = fmt.Fprintf(w, `{"state":"passed","jobs":%s}`, prBuildkiteJobs[number])
	}))
	t.Cleanup(ts.Close)
	prior := buildkiteAPI
	buildkiteAPI = ts.URL
	t.Cleanup(func() { buildkiteAPI = prior })
	t.Setenv(buildkiteEnv, token)
}

func prSkippedNode() string {
	checks := `"checks":{"nodes":[{"commit":{"status":{"state":"SUCCESS"},"statusCheckRollup":{"state":"SUCCESS","contexts":{"nodes":[` +
		`{"__typename":"StatusContext","context":"buildkite/test","state":"SUCCESS","targetUrl":"` + prSkipBuild + `"}]}}}}]}`
	return strings.Replace(prNode(25131, "OPEN", prComments()), `"checks":{"nodes":[]}`, checks, 1)
}

func TestPRStatusGradesARedundantSkipByTheFullRunItDeferredTo(t *testing.T) {
	closed := `[{"number":42690,"state":"closed","merged_at":null,"head":{"ref":"child","sha":"` + prSkipChildHead + `"}}]`
	open := `[{"number":42690,"state":"open","merged_at":null,"head":{"ref":"child","sha":"` + prSkipChildHead + `"}}]`
	tests := []struct {
		name  string
		child string
		token string
		want  string
	}{
		{"a closed child", closed, "bk-test", "ci unproven: redundant skip, no full run · approved · blocked:ci-unproven"},
		{"no child", `[]`, "bk-test", "ci unproven: redundant skip, no full run · approved · blocked:ci-unproven"},
		{"an open child whose full run holds the head", open, "bk-test", "ci green: full run on #42690 · approved · landable"},
		{"an unreadable build", closed, "", "ci green · approved · landable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, client := stubPRInfo(t, prInfoOpen)
			github := stubPRState(t, prPoll(`"p0":`+prSkippedNode()))
			github.pull("/repos/Forge-AI/monorepo/pulls", tt.child)
			github.pull("/repos/Forge-AI/monorepo/commits/"+prSkipChildHead+"/status", `{"statuses":[{"context":"buildkite/test","state":"success","target_url":"`+prFullBuild+`"}]}`)
			github.pull("/repos/Forge-AI/monorepo/compare/"+prSkipHead+"..."+prSkipChildHead, `{"status":"ahead"}`)
			stubBuildkite(t, tt.token)

			out, err := runPRStatusCmd(t, client, "--repo", "Forge-AI/monorepo", "25131")
			if err != nil {
				t.Fatalf("pr status: %v", err)
			}
			if want := "#25131  not queued · open · " + tt.want + "\n"; !strings.HasSuffix(out, want) || strings.Contains(out, "kept GitHub's verdict") != (tt.token == "") {
				t.Errorf("report = %q, want it to end %q and warn only on an unreadable build", out, want)
			}
		})
	}
}
