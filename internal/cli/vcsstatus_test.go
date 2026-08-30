package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/vcstest"
)

// ghStatusPRArgv is the one batched pull request lookup ccx vcs status makes,
// spelled the way the fixture's argv log records it.
func ghStatusPRArgv(branches ...string) []string {
	argv := []string{"gh", "api", "graphql", "-F", "owner={owner}", "-F", "repo={repo}"}
	for i, b := range branches {
		argv = append(argv, "-f", fmt.Sprintf("b%d=%s", i, b))
	}
	return append(argv, "-f", "query="+statusPRQuery(len(branches)))
}

func runVcsStatusCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newVcsStatusCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func runVcsStatusJSON(t *testing.T, args ...string) vcsStatus {
	t.Helper()
	out, err := runVcsStatusCmd(t, append([]string{"--json"}, args...)...)
	if err != nil {
		t.Fatalf("status error = %v", err)
	}
	var got vcsStatus
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal report: %v\n%s", err, out)
	}
	return got
}

// TestVcsStatusArgvIsTheRecordedOne pins every query production builds to the
// one GitHub answered, so a replayed payload is the answer to ccx's own call.
// It is also what fails when scripts/record-gh-goldens.sh drifts from the Go.
func TestVcsStatusArgvIsTheRecordedOne(t *testing.T) {
	t.Parallel()
	tests := []struct {
		golden string
		argv   []string
	}{
		{"status-graphql-one", ghStatusPRArgv(downstackOne...)[1:]},
		{"status-graphql-three", ghStatusPRArgv(downstackThree...)[1:]},
		{"status-comment-graphql", []string{
			"api", "graphql",
			"-f", "c0=IC_kwDODKw3uc8AAAABM0KrfA",
			"-f", "query=" + statusCommentQuery(1),
		}},
		{"status-draft-graphql", []string{
			"api", "graphql",
			"-F", "owner=cli", "-F", "repo=cli",
			"-F", "d0=13982", "-F", "d1=13084",
			"-f", "query=" + statusDraftQuery(2),
		}},
	}
	for _, tt := range tests {
		t.Run(tt.golden, func(t *testing.T) {
			t.Parallel()
			if want := loadGHGolden(t, tt.golden).argv; !slices.Equal(tt.argv, want) {
				t.Errorf("argv = %q, want the recorded %q", tt.argv, want)
			}
		})
	}
}

// TestVcsStatusGTLane reports the whole stack from one batched query, against
// the pull request GitHub answered that query with.
func TestVcsStatusGTLane(t *testing.T) {
	status := loadGHGolden(t, "status-graphql-one")
	f := infoGTRepo(t, downstackOne...)
	ghReplay(t, f, status)
	writeInfoFile(t, f.Dir, "f.txt", "dirty\n")

	out, err := runVcsStatusCmd(t, "--no-queue-probe")
	if err != nil {
		t.Fatalf("status error = %v", err)
	}
	want := strings.Join([]string{
		"lane        gt",
		"root        " + f.Dir,
		"repo        yasyf/cc-context",
		"trunk       main",
		"dirty       yes (1 file) · f.txt",
		"",
		"branch      fix-ship-help-graphite-demote · here · current · ahead 1 · behind 0",
		"pr          #3 · merged · 2 files · a1673e91 · https://github.com/yasyf/cc-context/pull/3",
		"checks      guides / render skipped · reconcile skipped · test (ubuntu-latest) success · " +
			"test (macos-latest) success · lint success · guides / pr-check success · vuln success · " +
			"hook-tests success · descriptor-agreement success · Socket Security: Project Report success · " +
			"Socket Security: Pull Request Alerts success",
		"",
	}, "\n")
	if out != want {
		t.Errorf("report =\n%s\nwant\n%s", out, want)
	}
}

// TestVcsStatusMergedPullRequestBlocksNothing holds the report to the verdict
// that matters: a landed pull request is not waiting on anything, even though
// GitHub still answers its mergeability UNKNOWN.
func TestVcsStatusMergedPullRequestBlocksNothing(t *testing.T) {
	status := loadGHGolden(t, "status-graphql-one")
	f := infoGTRepo(t, downstackOne...)
	ghReplay(t, f, status)

	got := runVcsStatusJSON(t, "--no-queue-probe")
	if len(got.Branches) != 1 {
		t.Fatalf("branches = %d, want 1", len(got.Branches))
	}
	b := got.Branches[0]
	if b.PR == nil {
		t.Fatal("no pull request on the branch the golden answers for")
	}
	if !b.PR.Merged || b.PR.State != "MERGED" {
		t.Errorf("PR = %s/merged %v, want MERGED", b.PR.State, b.PR.Merged)
	}
	if b.PR.Mergeable != statusUnknown {
		t.Errorf("Mergeable = %q, want the %q GitHub answered", b.PR.Mergeable, statusUnknown)
	}
	if b.PR.Queue != nil {
		t.Errorf("Queue = %+v, want none — no merge label and no activity comment", b.PR.Queue)
	}
	if len(b.Blockers) != 0 {
		t.Errorf("Blockers = %q, want none", b.Blockers)
	}
	if len(got.Required) != 0 {
		t.Errorf("Required = %q, want none — the recorded repository publishes no rule", got.Required)
	}
}

// TestVcsStatusBranchWithoutPullRequest holds the whole stack in one report,
// including the branch GitHub answered with no pull request at all.
func TestVcsStatusBranchWithoutPullRequest(t *testing.T) {
	status := loadGHGolden(t, "status-graphql-three")
	f := infoGTRepo(t, downstackThree...)
	ghReplay(t, f, status)
	resetArgvLog(t, f)

	got := runVcsStatusJSON(t, "--no-queue-probe")
	if len(got.Branches) != 3 {
		t.Fatalf("branches = %d, want 3", len(got.Branches))
	}
	for i, want := range []int{3, 2, 0} {
		b := got.Branches[i]
		switch {
		case want == 0 && b.PR != nil:
			t.Errorf("%s carries PR #%d, want none", b.Name, b.PR.Number)
		case want == 0:
			if !slices.Contains(b.Blockers, "no pull request — run ccx vcs ship") {
				t.Errorf("%s blockers = %q, want the missing pull request named", b.Name, b.Blockers)
			}
		case b.PR == nil || b.PR.Number != want:
			t.Errorf("%s carries %v, want PR #%d", b.Name, b.PR, want)
		}
	}
	if got.Branches[2].Name != "no-such-branch" {
		t.Errorf("stack order = %q, want the branch with no pull request last", got.Branches[2].Name)
	}
	vcstest.Quiesce(t, f.ArgvLog)
	var graphql [][]string
	for _, inv := range vcstest.Invocations(t, f.ArgvLog) {
		if len(inv) > 2 && inv[0] == "gh" && inv[1] == "api" && inv[2] == "graphql" {
			graphql = append(graphql, inv)
		}
	}
	assertInvocations(t, graphql, [][]string{ghStatusPRArgv(downstackThree...)})
}

// TestStatusChecksKeepsTheLatestRun holds a re-run to one entry: the rollup
// carries every attempt under the same name, and only the last is the verdict.
func TestStatusChecksKeepsTheLatestRun(t *testing.T) {
	t.Parallel()
	rollup := &statusRollup{State: "FAILURE"}
	rollup.Contexts.Nodes = []statusContextNode{
		{Typename: "CheckRun", Name: "label", Conclusion: "FAILURE", Status: "COMPLETED"},
		{Typename: "CheckRun", Name: "label", Conclusion: "SUCCESS", Status: "COMPLETED"},
		{Typename: "CheckRun", Name: "build", Status: "IN_PROGRESS"},
		{Typename: "StatusContext", Context: "buildkite/tests", State: "PENDING"},
	}
	want := []statusCheck{
		{Name: "label", State: "SUCCESS"},
		{Name: "build", State: "IN_PROGRESS"},
		{Name: "buildkite/tests", State: "PENDING"},
	}
	if got := statusChecks(rollup); !slices.Equal(got, want) {
		t.Errorf("statusChecks() = %+v, want %+v", got, want)
	}
	if got := statusChecks(nil); got != nil {
		t.Errorf("statusChecks(nil) = %+v, want nil", got)
	}
}

// TestClassifyQueueProbe pins gt's own wording, which is the only answer left
// when Graphite declines to post its activity comment — as it did on two pull
// requests that entered the queue and merged the same day.
func TestClassifyQueueProbe(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		output  string
		code    int
		verdict queueVerdict
		detail  string
	}{
		{
			name:    "already merging is the queue holding it",
			output:  "🥞 Validating that this Graphite stack is ready to merge...\n\n🎉 The stack is already merging.\n",
			verdict: queueMerging,
			detail:  "The stack is already merging",
		},
		{
			name:    "already merged",
			output:  "Running merge in 'dry-run' mode. No PRs will be merged.\n\n🎉 The stack has already merged.\n",
			verdict: queueMerged,
			detail:  "The stack has already merged",
		},
		{
			name:    "a clean dry run means nothing has it",
			output:  "Running merge in 'dry-run' mode. No PRs will be merged.\n\nWould merge PR #10.\n",
			verdict: queueReady,
		},
		{
			name:    "an unrecognized failure quotes gt's first line",
			output:  "gt exploded\nand kept going\n",
			code:    1,
			verdict: queueUnknown,
			detail:  "gt exploded",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			verdict, detail := classifyQueueProbe(tt.output, tt.code)
			if verdict != tt.verdict {
				t.Errorf("verdict = %q, want %q", verdict, tt.verdict)
			}
			if detail != tt.detail {
				t.Errorf("detail = %q, want %q", detail, tt.detail)
			}
		})
	}
}

// TestStatusBlockersNamesAReparent covers the hazard of a repo-global stack:
// concurrent gt runs move a branch under another lane's tip, and the pull
// request keeps its original base while the branch no longer sits on it.
func TestStatusBlockersNamesAReparent(t *testing.T) {
	t.Parallel()
	branch := statusBranch{
		Name:   "feature",
		Parent: "someone-elses-lane",
		PR: &statusPR{
			Number:         12,
			State:          "OPEN",
			Base:           "dev",
			HasBody:        true,
			Mergeable:      "MERGEABLE",
			ReviewDecision: "APPROVED",
		},
	}
	want := "gt parents this on someone-elses-lane but PR #12 bases on dev — the branch was reparented; restack and re-submit"
	got := statusBlockers(branch)
	if len(got) != 1 || got[0] != want {
		t.Errorf("statusBlockers() = %q, want exactly %q", got, want)
	}
	branch.Parent = "dev"
	if got := statusBlockers(branch); len(got) != 0 {
		t.Errorf("statusBlockers() = %q, want none once the parent and the base agree", got)
	}
}

func TestStatusRequiredMatchesTheBase(t *testing.T) {
	t.Parallel()
	rules := []statusProtectionRule{
		{Pattern: "dev", RequiredStatusChecks: []struct {
			Context string `json:"context"`
		}{{Context: "buildkite/tests"}}},
		{Pattern: "release/*", RequiredStatusChecks: []struct {
			Context string `json:"context"`
		}{{Context: "buildkite/release"}}},
	}
	tests := []struct {
		base string
		want []string
	}{
		{"dev", []string{"buildkite/tests"}},
		{"release/1.2", []string{"buildkite/release"}},
		{"yasyf/some-stacked-branch", nil},
	}
	for _, tt := range tests {
		t.Run(tt.base, func(t *testing.T) {
			t.Parallel()
			if got := statusRequired(rules, tt.base); !slices.Equal(got, tt.want) {
				t.Errorf("statusRequired(%q) = %q, want %q", tt.base, got, tt.want)
			}
		})
	}
}

// TestStatusBlockersNamesEveryHold is the report's whole point: one branch, one
// list, worst first — and a queue holding an older commit outranks the checks
// that pass on the newer one.
func TestStatusBlockersNamesEveryHold(t *testing.T) {
	t.Parallel()
	branch := statusBranch{
		Name:         "feature",
		NeedsRestack: true,
		PR: &statusPR{
			State:          "OPEN",
			Head:           "8259d97ba",
			HasBody:        true,
			Mergeable:      "MERGEABLE",
			ReviewDecision: "APPROVED",
			Checks: []statusCheck{
				{Name: "buildkite/tests", State: "FAILURE", Required: true},
				{Name: "ai-review", State: "FAILURE"},
			},
			Reviews: []statusReview{
				{Author: "forge-pr-reviewer", Bot: true, State: "APPROVED", Commit: "434c1117a", Stale: true},
				{Author: "yasyf", State: "APPROVED", Commit: "8259d97ba"},
			},
			Queue: &statusQueue{
				Phase:   mqPhaseQueued,
				Held:    "861f15e6c",
				Dropped: []statusCommit{{OID: "8259d97ba"}},
			},
		},
	}
	want := []string{
		"sits off its parent — run ccx vcs stack restack",
		"the merge queue is holding 861f15e6, so 1 commit would not land",
		"required check buildkite/tests is failure",
		"forge-pr-reviewer's approval is stale — it was cast on 434c1117",
	}
	if got := statusBlockers(branch); !slices.Equal(got, want) {
		t.Errorf("statusBlockers() = %q, want %q", got, want)
	}
}

func TestStatusBlockersOnAnUnmergeableDraft(t *testing.T) {
	t.Parallel()
	branch := statusBranch{
		Name: "feature",
		PR: &statusPR{
			State:          "OPEN",
			Draft:          true,
			Mergeable:      "CONFLICTING",
			ReviewDecision: "REVIEW_REQUIRED",
		},
	}
	want := []string{
		"the pull request is a draft",
		"the pull request has no body",
		"the branch conflicts with its base",
		"the pull request is not approved",
	}
	if got := statusBlockers(branch); !slices.Equal(got, want) {
		t.Errorf("statusBlockers() = %q, want %q", got, want)
	}
}
