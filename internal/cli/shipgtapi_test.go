// The Graphite API stub the gt-lane ship tests submit against: an httptest
// server behind gtAPIClient serving the routes gtSubmitStack calls, recording
// every request so a test can assert the HTTP half of a submit beside the
// argv log.
package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
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
	submits []gtStubSubmit
}

// gtStubSubmit is one submit post: the raw body ccx sent, and the lone entry
// the recovered contract requires it to carry.
type gtStubSubmit struct {
	body  []byte
	entry gtStubSubmitEntry
}

// gtStubSubmitRequest is a submit post typed to the zod schema recovered from
// graphite-cli: repoOwner, repoName and trunkBranchName are required.
type gtStubSubmitRequest struct {
	RepoOwner             string              `json:"repoOwner"`
	RepoName              string              `json:"repoName"`
	TrunkBranchName       string              `json:"trunkBranchName"`
	TargetTrunkBranchName string              `json:"targetTrunkBranchName"`
	MergeWhenReady        bool                `json:"mergeWhenReady"`
	RerequestReview       bool                `json:"rerequestReview"`
	PRs                   []gtStubSubmitEntry `json:"prs"`
}

// gtStubSubmitEntry is one entry of that schema. Title and Draft are pointers
// because an omitted field and an empty one differ: an update that restates
// neither leaves the pull request's own alone.
type gtStubSubmitEntry struct {
	Action           gtapi.SubmitAction `json:"action"`
	Head             string             `json:"head"`
	HeadSha          string             `json:"headSha"`
	Base             string             `json:"base"`
	BaseSha          string             `json:"baseSha"`
	PRNumber         int                `json:"prNumber"`
	Title            *string            `json:"title"`
	Body             *string            `json:"body"`
	Draft            *bool              `json:"draft"`
	Reviewers        []string           `json:"reviewers"`
	ShouldRetarget   bool               `json:"shouldRetarget"`
	RebaseOnlyChange bool               `json:"rebaseOnlyChange"`
	MaintainRetarget bool               `json:"maintainRetarget"`
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
		s.submit(w, r)
	default:
		s.t.Errorf("unexpected graphite API request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

// submit serves one submit post the way real gt sends one: every required
// top-level field present, every field typed as the recovered schema declares
// it, and exactly one entry in the array.
func (s *gtAPIStub) submit(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.t.Errorf("read submit request: %v", err)
		return
	}
	var req gtStubSubmitRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.refuse(w, fmt.Sprintf("submit request %s does not match the recovered graphite schema: %v", body, err))
		return
	}
	for _, field := range []struct{ name, value string }{
		{"repoOwner", req.RepoOwner}, {"repoName", req.RepoName}, {"trunkBranchName", req.TrunkBranchName},
	} {
		if field.value == "" {
			s.refuse(w, fmt.Sprintf("submit request omits %s, which the recovered graphite schema requires", field.name))
			return
		}
	}
	if len(req.PRs) != 1 {
		s.refuse(w, fmt.Sprintf("submit posted %d entries, want exactly 1 — real gt posts one per request", len(req.PRs)))
		return
	}
	entry := req.PRs[0]
	s.submits = append(s.submits, gtStubSubmit{body: body, entry: entry})
	s.requireSubmitFields(entry)

	if message := s.submitErrors[entry.Head]; message != "" {
		s.write(w, map[string]any{"prs": []map[string]any{{"head": entry.Head, "status": "error", "error": message}}})
		return
	}
	number, status := s.nextPR, "created"
	if entry.Action == gtapi.SubmitUpdate {
		number, status = entry.PRNumber, "updated"
	} else {
		s.nextPR++
	}
	s.write(w, map[string]any{"prs": []map[string]any{{"head": entry.Head, "prNumber": number, "prURL": gtStubPRURL(number), "status": status}}})
}

// refuse answers a schema violation with the 400 graphite's handler returns,
// and fails the test: ccx must never send a request this stub has to reject.
func (s *gtAPIStub) refuse(w http.ResponseWriter, reason string) {
	s.t.Error(reason)
	w.WriteHeader(http.StatusBadRequest)
	s.write(w, map[string]any{"prs": []map[string]any{{"reason": reason}}})
}

// requireSubmitFields enforces what the schema leaves to graphite's handler:
// every entry names its action, head, headSha, base and baseSha; a create
// carries the title graphite requires, an update the pull request number.
func (s *gtAPIStub) requireSubmitFields(entry gtStubSubmitEntry) {
	present := map[string]bool{
		"head":    entry.Head != "",
		"headSha": entry.HeadSha != "",
		"base":    entry.Base != "",
		"baseSha": entry.BaseSha != "",
	}
	switch entry.Action {
	case gtapi.SubmitCreate:
		present["title"] = entry.Title != nil && *entry.Title != ""
	case gtapi.SubmitUpdate:
		present["prNumber"] = entry.PRNumber != 0
	default:
		s.t.Errorf("submit entry action = %q, want create or update", entry.Action)
	}
	for _, field := range slices.Sorted(maps.Keys(present)) {
		if !present[field] {
			s.t.Errorf("submit entry %v omits %s, which the recovered graphite schema requires", entry.Head, field)
		}
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

// submitHeads names the branch of every submit post, in the order the stub
// served them — the bottom-up order a stack must be submitted in.
func (s *gtAPIStub) submitHeads() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	heads := make([]string, 0, len(s.submits))
	for _, submit := range s.submits {
		heads = append(heads, submit.entry.Head)
	}
	return heads
}

// submitEntry returns the entry one branch was submitted under.
func (s *gtAPIStub) submitEntry(head string) gtStubSubmitEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, submit := range s.submits {
		if submit.entry.Head == head {
			return submit.entry
		}
	}
	s.t.Fatalf("no submit reached the graphite API stub for %s", head)
	return gtStubSubmitEntry{}
}

// submitBodies returns the raw JSON of every submit post, in order.
func (s *gtAPIStub) submitBodies() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	bodies := make([][]byte, 0, len(s.submits))
	for _, submit := range s.submits {
		bodies = append(bodies, slices.Clone(submit.body))
	}
	return bodies
}

// gtPushRef is one branch of the submit's force-push: the head the remote
// branch moves to, under the lease of its last submitted version.
type gtPushRef struct {
	branch string
	sha    string
	lease  string
}

func gtHead(branch, sha string) gtPushRef { return gtPushRef{branch: branch, sha: sha} }

// gtLeasedHead is gtHead with the lease pinned to the last submitted head.
func gtLeasedHead(branch, sha, lease string) gtPushRef {
	return gtPushRef{branch: branch, sha: sha, lease: lease}
}

// gtPushInv is the one atomic force-push an API submit makes for a whole stack:
// every branch's lease, then every branch's refspec, plus the gt lane's default
// --no-verify.
func gtPushInv(refs ...gtPushRef) []string {
	argv := []string{"git", "push", "origin"}
	for _, ref := range refs {
		lease := "--force-with-lease"
		if ref.lease != "" {
			lease += "=refs/heads/" + ref.branch + ":" + ref.lease
		}
		argv = append(argv, lease)
	}
	argv = append(argv, "--progress")
	for _, ref := range refs {
		argv = append(argv, ref.sha+":refs/heads/"+ref.branch)
	}
	return append(argv, "--no-verify", "--atomic")
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
