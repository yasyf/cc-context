// The Graphite API stub the gt-lane ship tests submit against: an httptest
// server behind gtAPIClient serving the routes gtSubmitStack calls, recording
// every request so a test can assert the HTTP half of a submit beside the
// argv log.
package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/yasyf/cc-context/internal/gtapi"
)

const gtStubSubmitRoute = "/graphite/submit/pull-requests"

type gtAPIStub struct {
	t  *testing.T
	mu sync.Mutex

	synced         gtapi.RepoSyncStatus
	syncMessage    string
	prs            map[string]int
	unauthorized   bool
	presubmitError string
	submitErrors   map[string]string
	nextPR         int

	routes  []string
	submits []map[string]any
}

// stubGTAPI points gtAPIClient at a fresh stub for the test's duration. The
// last install wins, so a test needing configuration calls it again after its
// fixture helper installed the default.
func stubGTAPI(t *testing.T) *gtAPIStub {
	t.Helper()
	s := &gtAPIStub{
		t:            t,
		synced:       gtapi.RepoSynced,
		prs:          map[string]int{},
		submitErrors: map[string]string{},
		nextPR:       100,
	}
	srv := httptest.NewServer(http.HandlerFunc(s.serve))
	prev := gtAPIClient
	gtAPIClient = func() *gtapi.Client { return gtapi.NewWithToken(srv.URL, "gt-stub-token") }
	t.Cleanup(func() {
		gtAPIClient = prev
		srv.Close()
	})
	return s
}

func (s *gtAPIStub) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.routes = append(s.routes, r.URL.Path)
	if got := r.Header.Get("Authorization"); got != "token gt-stub-token" {
		s.t.Errorf("Authorization = %q, want the stub token", got)
	}
	if s.unauthorized {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprint(w, `{"message":"invalid token"}`)
		return
	}
	switch r.URL.Path {
	case "/graphite/cli/is-repo-synced":
		s.write(w, map[string]any{"result": map[string]any{"status": s.synced, "message": s.syncMessage}})
	case "/graphite/cli/pull-request-info":
		var req struct {
			PRHeadRefNames []string `json:"prHeadRefNames"`
		}
		s.decode(r, &req)
		prs := []map[string]any{}
		for _, branch := range req.PRHeadRefNames {
			if number := s.prs[branch]; number != 0 {
				prs = append(prs, map[string]any{
					"prNumber": number, "headRefName": branch, "state": "OPEN", "url": gtStubPRURL(number),
				})
			}
		}
		s.write(w, map[string]any{"result": map[string]any{"status": "ok", "prs": prs}})
	case "/graphite/cli/submit/pre-submit-pull-requests":
		if s.presubmitError != "" {
			s.write(w, map[string]any{"result": map[string]any{"error": s.presubmitError}})
			return
		}
		s.write(w, map[string]any{"result": map[string]any{"retargetedPrs": []int{}}})
	case gtStubSubmitRoute:
		var req map[string]any
		s.decode(r, &req)
		s.submits = append(s.submits, req)
		out := []map[string]any{}
		for _, raw := range req["prs"].([]any) {
			pr := raw.(map[string]any)
			head := pr["head"].(string)
			if message := s.submitErrors[head]; message != "" {
				out = append(out, map[string]any{"head": head, "status": "error", "error": message})
				continue
			}
			number, status := s.nextPR, "created"
			if existing, ok := pr["prNumber"].(float64); ok {
				number, status = int(existing), "updated"
			} else {
				s.nextPR++
			}
			out = append(out, map[string]any{"head": head, "prNumber": number, "prURL": gtStubPRURL(number), "status": status})
		}
		s.write(w, map[string]any{"prs": out})
	default:
		s.t.Errorf("unexpected graphite API request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (s *gtAPIStub) write(w http.ResponseWriter, payload any) {
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		s.t.Errorf("encode stub response: %v", err)
	}
}

func (s *gtAPIStub) decode(r *http.Request, into any) {
	if err := json.NewDecoder(r.Body).Decode(into); err != nil {
		s.t.Errorf("decode %s request: %v", r.URL.Path, err)
	}
}

func gtStubPRURL(number int) string {
	return fmt.Sprintf("https://app.graphite.dev/github/pr/yasyf/cc-context/%d", number)
}

// submitCalls counts the submit posts the stub served, the API counterpart of
// the exactly-one-gt-submit assertions.
func (s *gtAPIStub) submitCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, route := range s.routes {
		if route == gtStubSubmitRoute {
			n++
		}
	}
	return n
}

// lastSubmit returns the branch entries of the last submit post, keyed by
// head.
func (s *gtAPIStub) lastSubmit() map[string]map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.submits) == 0 {
		s.t.Fatal("no submit reached the graphite API stub")
	}
	req := s.submits[len(s.submits)-1]
	out := map[string]map[string]any{}
	for _, raw := range req["prs"].([]any) {
		pr := raw.(map[string]any)
		out[pr["head"].(string)] = pr
	}
	return out
}

// gtPushInv is the force-push an API submit makes for one branch, in its
// default shape: a bare lease and the gt lane's default --no-verify.
func gtPushInv(branch, sha string) []string {
	return []string{"git", "push", "origin", "--force-with-lease", "--progress", sha + ":refs/heads/" + branch, "--no-verify", "--atomic"}
}

// gtPushInvLease is gtPushInv with the lease pinned to the last submitted
// head.
func gtPushInvLease(branch, sha, lease string) []string {
	return []string{"git", "push", "origin", "--force-with-lease=refs/heads/" + branch + ":" + lease, "--progress", sha + ":refs/heads/" + branch, "--no-verify", "--atomic"}
}

// gtCreateLogInv is the commit read that derives a created PR's title and
// body.
func gtCreateLogInv(base, branch string) []string {
	return []string{"git", "log", "--reverse", "--format=%s%x00%b%x00", base + ".." + branch}
}

// gtPushedRefs reads the refspecs of every ship-made force-push out of the
// argv log, branch names only.
func gtPushedRefs(invocations [][]string) []string {
	var refs []string
	for _, inv := range invocations {
		if len(inv) < 2 || inv[0] != "git" || inv[1] != "push" {
			continue
		}
		for _, arg := range inv {
			if _, ref, ok := strings.Cut(arg, ":refs/heads/"); ok && !strings.HasPrefix(arg, "--") {
				refs = append(refs, ref)
			}
		}
	}
	return refs
}
