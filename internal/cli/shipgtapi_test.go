// The Graphite API stub the gt-lane ship tests submit against: an httptest
// server the test carries on its context, serving the routes gtSubmitStack
// calls and recording every request so a test can assert the HTTP half of a
// submit beside the argv log.
package cli

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/yasyf/cc-context/internal/gtapi"
	"github.com/yasyf/cc-context/internal/vcstest"
)

const gtStubSubmitRoute = "/graphite/submit/pull-requests"

type gtAPIStub struct {
	t       *testing.T
	client  *gtapi.Client
	mu      sync.Mutex
	carried bool

	synced         gtapi.RepoSyncStatus
	syncMessage    string
	prs            map[string]int
	merged         map[string]gtStubMerged
	bodies         map[string]string
	unauthorized   bool
	presubmitError string
	submitErrors   map[string]string
	queued         map[string]bool
	untracked      map[int]gtStubUntracked
	nextPR         int
	// parked maps a branch to the graphite-base branch pre-submit moved its
	// pull request onto; remote reads and writes that branch on origin.
	parked map[string]string
	remote func(args ...string) string
	bases  map[string]string

	routes    []string
	infoHeads [][]string
	submits   []gtStubSubmit
}

// gtStubMerged is a pull request Graphite reports MERGED, or CLOSED when state
// says so, and the head its newest version carried.
type gtStubMerged struct {
	number int
	head   string
	state  gtapi.PRState
}

type gtStubUntracked struct {
	base  string
	head  string
	stuck bool
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

// stubGTAPI is [newGTAPIStub] under the guard a migration off the package-var
// client needs: a stub no context carries is one the code never reaches, and
// every assertion a test makes over it then passes while pinning nothing.
func stubGTAPI(t *testing.T) *gtAPIStub {
	t.Helper()
	s := newGTAPIStub(t)
	t.Cleanup(func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if !s.carried {
			t.Error("stubGTAPI: no context carried this stub, so the code under test never reached it")
		}
	})
	return s
}

// newGTAPIStub serves a fresh stub for the test's duration, reached by the
// contexts [gtAPIStub.ctx] returns. The last decorator wins, so a test needing
// configuration stubs again over the default its fixture helper installed —
// and that default, superseded by design, is the one caller that goes
// unguarded.
func newGTAPIStub(t *testing.T) *gtAPIStub {
	t.Helper()
	s := &gtAPIStub{
		t:            t,
		synced:       gtapi.RepoSynced,
		prs:          map[string]int{},
		merged:       map[string]gtStubMerged{},
		bodies:       map[string]string{},
		submitErrors: map[string]string{},
		queued:       map[string]bool{},
		untracked:    map[int]gtStubUntracked{},
		parked:       map[string]string{},
		bases:        map[string]string{},
		nextPR:       100,
	}
	srv := httptest.NewServer(http.HandlerFunc(s.serve))
	s.client = gtapi.NewWithToken(srv.URL, "gt-stub-token")
	t.Cleanup(srv.Close)
	return s
}

// ctx returns parent carrying this stub as the Graphite API every gt-lane call
// under it reaches, in place of the package-wide client a parallel test would
// otherwise be racing another test to overwrite.
func (s *gtAPIStub) ctx(parent context.Context) context.Context {
	s.mu.Lock()
	s.carried = true
	s.mu.Unlock()
	return withGTAPI(parent, s.client)
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
			PRNumbers      json.RawMessage `json:"prNumbers"`
			PRHeadRefNames []string        `json:"prHeadRefNames"`
		}
		s.decode(r, &req)
		if !bytes.HasPrefix(req.PRNumbers, []byte("[")) {
			s.refuse(w, fmt.Sprintf("pull-request-info prNumbers = %s, and graphite answers 400 unless it is an array", cmp.Or(string(req.PRNumbers), "absent")))
			return
		}
		s.infoHeads = append(s.infoHeads, req.PRHeadRefNames)
		prs := []map[string]any{}
		var numbers []int
		_ = json.Unmarshal(req.PRNumbers, &numbers)
		for _, number := range numbers {
			if u, ok := s.untracked[number]; ok {
				prs = append(prs, map[string]any{
					"prNumber": number, "state": "OPEN", "url": gtStubPRURL(number),
					"versions": []map[string]any{{"headSha": u.head, "baseName": u.base, "createdAt": "2026-09-02T00:00:00.000Z"}},
				})
			}
		}
		for _, branch := range req.PRHeadRefNames {
			if number := s.prs[branch]; number != 0 {
				pr := map[string]any{
					"prNumber": number, "headRefName": branch, "state": "OPEN", "url": gtStubPRURL(number), "body": s.bodies[branch],
				}
				if s.queued[branch] {
					pr["mergeQueueStatus"] = map[string]any{"isInGraphiteMq": true}
				}
				if entry, ok := s.lastEntry(branch); ok {
					pr["baseRefName"] = entry.Base
					pr["versions"] = []map[string]any{{"headSha": entry.HeadSha, "baseSha": entry.BaseSha, "baseName": entry.Base, "createdAt": "2026-09-02T00:00:00.000Z"}}
				}
				if base := s.bases[branch]; base != "" {
					pr["baseRefName"] = base
				}
				if base := s.parked[branch]; base != "" {
					pr["baseRefName"], pr["isBaseRefGraphiteBase"] = base, true
				}
				prs = append(prs, pr)
			}
			if m, ok := s.merged[branch]; ok {
				prs = append(prs, map[string]any{
					"prNumber": m.number, "headRefName": branch, "state": cmp.Or(m.state, gtapi.PRMerged), "url": gtStubPRURL(m.number),
					"versions": []map[string]any{
						{"headSha": m.head, "createdAt": "2026-09-02T00:00:00.000Z"},
						{"headSha": strings.Repeat("0", 40), "createdAt": "2026-09-01T00:00:00.000Z"},
					},
				})
			}
		}
		s.write(w, map[string]any{"result": map[string]any{"status": "ok", "prs": prs}})
	case "/graphite/mergeability-status":
		var req struct {
			PRNumbers []int `json:"prNumbers"`
		}
		s.decode(r, &req)
		rows := []map[string]any{}
		for _, number := range req.PRNumbers {
			if _, untracked := s.untracked[number]; !untracked {
				rows = append(rows, map[string]any{"prNumber": number, "forgeSource": "github", "mergeabilityStatus": "READY_TO_MERGE"})
			}
		}
		s.write(w, map[string]any{"mergeabilityStatuses": rows})
	case "/graphite/cli/submit/pre-submit-pull-requests":
		if s.presubmitError != "" {
			s.write(w, map[string]any{"result": map[string]any{"error": s.presubmitError}})
			return
		}
		var req struct {
			Branches []gtapi.PreSubmitBranch `json:"branches"`
		}
		s.decode(r, &req)
		s.write(w, map[string]any{"result": map[string]any{"retargetedPrs": s.parkChildren(req.Branches)}})
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
	if base := s.parked[entry.Head]; base != "" {
		switch s.remote("for-each-ref", "--format=%(objectname)", "refs/heads/"+base) {
		case "":
			delete(s.parked, entry.Head)
		case entry.BaseSha:
			delete(s.parked, entry.Head)
			s.remote("update-ref", "-d", "refs/heads/"+base)
		default:
			s.remote("update-ref", "refs/heads/"+base, entry.BaseSha)
		}
	}
	if u, ok := s.untracked[entry.PRNumber]; ok && !u.stuck && entry.HeadSha != u.head {
		delete(s.untracked, entry.PRNumber)
	}
	number, status := s.nextPR, "created"
	if entry.Action == gtapi.SubmitUpdate {
		number, status = entry.PRNumber, "updated"
	} else {
		s.nextPR++
	}
	s.write(w, map[string]any{"prs": []map[string]any{{"head": entry.Head, "prNumber": number, "prURL": gtStubPRURL(number), "status": status}}})
}

// parkChildren moves each open pull request stacked on a branch the submit
// names, but not named itself, onto graphite-base/<number> at its parent's
// remote head, as Graphite's pre-submit does, once a test wires the stub to
// origin with parkOn. It returns the numbers it moved.
func (s *gtAPIStub) parkChildren(branches []gtapi.PreSubmitBranch) []int {
	if s.remote == nil {
		return []int{}
	}
	named := map[string]bool{}
	for _, b := range branches {
		named[b.HeadRefName] = true
	}
	moved := []int{}
	for _, branch := range slices.Sorted(maps.Keys(s.prs)) {
		entry, ok := s.lastEntry(branch)
		if !ok || named[branch] || !named[entry.Base] || s.parked[branch] != "" {
			continue
		}
		number := s.prs[branch]
		base := fmt.Sprintf("graphite-base/%d", number)
		s.remote("update-ref", "refs/heads/"+base, s.remote("rev-parse", "refs/heads/"+entry.Base))
		s.parked[branch] = base
		moved = append(moved, number)
	}
	return moved
}

// parkOn wires the stub to f's origin, where it keeps the graphite-base
// branches it parks pull requests on.
func (s *gtAPIStub) parkOn(f *vcstest.Fixture) {
	s.remote = func(args ...string) string {
		cmd := exec.Command("git", args...) //nolint:gosec // args are the stub's own literal git verbs
		cmd.Dir = f.RemoteDir
		out, err := cmd.Output()
		if err != nil {
			s.t.Errorf("git %v in origin: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
}

// parkedPRs names the branches whose pull requests still sit on a
// graphite-base branch.
func (s *gtAPIStub) parkedPRs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Sorted(maps.Keys(s.parked))
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

// routeCount counts the requests the stub served on one route.
func (s *gtAPIStub) routeCount(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, route := range s.routes {
		if route == path {
			n++
		}
	}
	return n
}

// infoRequests returns the head refs each pull-request-info request named.
func (s *gtAPIStub) infoRequests() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.infoHeads)
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

// lastEntry is the entry branch was last submitted under, the version Graphite
// reports as its pull request's newest. The caller holds s.mu.
func (s *gtAPIStub) lastEntry(branch string) (gtStubSubmitEntry, bool) {
	for _, submit := range slices.Backward(s.submits) {
		if submit.entry.Head == branch {
			return submit.entry, true
		}
	}
	return gtStubSubmitEntry{}, false
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
	argv := []string{"git", "push", "--no-follow-tags", "--quiet", "origin"}
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

// gtReceiptInv is the publication receipt a plain submit records after its
// push: per branch, the prior read, the receipt blob, and the read
// stackReceiptTx makes, then one update-ref transaction.
func gtReceiptInv(branches ...string) [][]string {
	var inv [][]string
	for _, branch := range branches {
		read := []string{"git", "rev-parse", "--verify", "--quiet", stackPublicationRef(branch, "receipt")}
		inv = append(inv, read, []string{"git", "hash-object", "-w", "--stdin"}, read)
	}
	return append(inv, []string{"git", "update-ref", "--stdin"})
}

// gtCreateLogInv is the commit read that derives a created PR's title and
// body.
func gtCreateLogInv(base, branch string) []string {
	if strings.HasPrefix(base, "refs/remotes/") {
		base = fakeTrunkSHA
	}
	return []string{"git", "log", "--reverse", "--format=%s%x00%b%x00", base + ".." + branch}
}

// gtCherryInv is the patch-identity read a submit makes per branch, naming the
// commits the remote trunk already holds.
func gtCherryInv(_ string, head, base string) []string {
	return []string{"git", "cherry", "--abbrev=12", fakeTrunkSHA, head, base}
}

// gtRemoteTrunk is the remote-tracking ref a submit anchors trunk on.
func gtRemoteTrunk(trunk string) string { return "refs/remotes/origin/" + trunk }

// gtTrunkInv is the trunk resolution a submit runs before it plans anything:
// the remote HEAD tracks, a fetch of that one ref, and the check that it exists.
func gtTrunkInv(trunk string) [][]string {
	ref := gtRemoteTrunk(trunk)
	return [][]string{
		{"git", "config", "--get", "branch.HEAD.remote"},
		{"git", "show-ref", "--verify", "--quiet", ref},
		{"git", "fetch", "--no-tags", "--no-write-fetch-head", "--negotiation-tip=" + ref, "origin", trunk},
		{"git", "show-ref", "--verify", "--quiet", ref},
	}
}

// gtDropTrunkInv takes the trunk resolution out of got so the calls around it
// stay an exact sequence. Ship resolves the trunk on a goroutine overlapping the
// commit, so its calls land where nothing fixes them, and CI has recorded logs
// carrying only some of them. Only the fetch is required; a second occurrence of
// any call stays in the result for the caller's exact comparison.
func gtDropTrunkInv(t *testing.T, got [][]string, trunk string, branches ...string) [][]string {
	t.Helper()
	resolution := gtTrunkInv(trunk)
	for _, branch := range branches {
		resolution = append(resolution, []string{"git", "rev-parse", "--verify", "--quiet", stackPublicationRef(branch, "receipt")})
	}
	const fetch = 2
	dropped := make([]bool, len(resolution))
	rest := make([][]string, 0, len(got))
	for _, inv := range got {
		matched := false
		for i, call := range resolution {
			if !dropped[i] && slices.Equal(inv, call) {
				dropped[i] = true
				matched = true
				break
			}
		}
		if matched {
			continue
		}
		rest = append(rest, inv)
	}
	for i := len(gtTrunkInv(trunk)); i < len(resolution); i++ {
		if !dropped[i] {
			t.Errorf("publication receipt lookup missing: %v", resolution[i])
		}
	}
	if !dropped[fetch] {
		t.Errorf("trunk resolution: no %v in the log\n%v", resolution[fetch], got)
	}
	return rest
}

// gtContainedInv asks whether one head is already in the remote trunk.
func gtContainedInv(trunk, head string) []string {
	return []string{"git", "merge-base", "--is-ancestor", head, gtRemoteTrunk(trunk)}
}

// gtShipSubmitInv is the git work a ship does before its submit pushes: ask
// whether the shipped branch and then each branch of the stack is already in the
// remote trunk, and read the base sha a trunk-based branch submits under. Heads
// arrive bottom-up, shipped one last. The trunk resolution itself floats, so
// gtDropTrunkInv accounts for it rather than this sequence.
func gtShipSubmitInv(trunk string, heads ...string) [][]string {
	inv := make([][]string, 0, 2+len(heads))
	inv = append(inv, gtContainedInv(trunk, heads[len(heads)-1]), []string{"git", "rev-parse", "--verify", gtRemoteTrunk(trunk)})
	for _, head := range heads {
		inv = append(inv, []string{"git", "merge-base", "--is-ancestor", head, fakeTrunkSHA})
	}
	return inv
}

// hasInvocation reports whether the argv log carries one exact invocation.
func hasInvocation(invocations [][]string, want []string) bool {
	return slices.ContainsFunc(invocations, func(inv []string) bool { return slices.Equal(inv, want) })
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
