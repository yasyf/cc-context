package cli

import (
	"fmt"
	"strings"
	"testing"
)

func TestStatusClassify(t *testing.T) {
	t.Parallel()
	tests := []struct {
		state string
		want  statusClass
	}{
		{"SUCCESS", statusPassed},
		{"SKIPPED", statusSkipped},
		{"NEUTRAL", statusNeutral},
		{"FAILURE", statusFailed},
		{"CANCELLED", statusFailed},
		{"TIMED_OUT", statusFailed},
		{"ACTION_REQUIRED", statusFailed},
		{"IN_PROGRESS", statusRunning},
		{"EXPECTED", statusRunning},
		{"A_STATE_ADDED_LATER", statusFailed},
		{"", statusFailed},
	}
	for _, tt := range tests {
		t.Run(tt.state, func(t *testing.T) {
			t.Parallel()
			if got := statusClassify(tt.state); got != tt.want {
				t.Errorf("statusClassify(%q) = %v, want %v", tt.state, got, tt.want)
			}
		})
	}
}

// TestStatusCheckBlockers pins the readings a green rollup hides. Each case is
// a pull request whose rollup says success while something under it does not.
func TestStatusCheckBlockers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		pr   statusPR
		want string
	}{
		{
			name: "a neutral run is a hold, not a pass",
			pr: statusPR{ChecksState: "SUCCESS", Checks: []statusCheck{
				{Name: "buildkite/test", State: "SUCCESS", Required: true},
				{Name: "ai-review", State: "NEUTRAL"},
			}},
			want: "check ai-review is neutral",
		},
		{
			name: "a required check that skipped reported without running",
			pr: statusPR{ChecksState: "SUCCESS", Required: []string{"buildkite/test"}, Checks: []statusCheck{
				{Name: "buildkite/test", State: "SKIPPED", Required: true},
			}},
			want: "was skipped — it reported without running",
		},
		{
			name: "a skipped check no rule requires is not a blocker",
			pr: statusPR{ChecksState: "SUCCESS", Checks: []statusCheck{
				{Name: "guides / render", State: "SKIPPED"},
			}},
			want: "",
		},
		{
			name: "a required check that never reported",
			pr: statusPR{ChecksState: "SUCCESS", Head: "abcdef1234", Required: []string{"buildkite/test"}, Checks: []statusCheck{
				{Name: "lint", State: "SUCCESS"},
			}},
			want: "required check buildkite/test has not reported on abcdef12",
		},
		{
			name: "a cancelled build owning the commit status",
			pr: statusPR{ChecksState: "FAILURE", Checks: []statusCheck{
				{Name: "lint", State: "SUCCESS"},
			}},
			want: "a cancelled build owns the commit status",
		},
		{
			name: "a failing rollup with a failing check under it needs no extra line",
			pr: statusPR{ChecksState: "FAILURE", Checks: []statusCheck{
				{Name: "lint", State: "FAILURE", Required: true},
			}},
			want: "required check lint is failure",
		},
		{
			name: "a red no rule requires is not a blocker",
			pr: statusPR{ChecksState: "SUCCESS", Checks: []statusCheck{
				{Name: "codeql", State: "FAILURE"},
			}},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := strings.Join(statusCheckBlockers(&tt.pr), "\n")
			if tt.want == "" {
				if got != "" {
					t.Errorf("blockers = %q, want none", got)
				}
				return
			}
			if !strings.Contains(got, tt.want) {
				t.Errorf("blockers = %q, want one containing %q", got, tt.want)
			}
		})
	}
}

// TestStatusChecksValueIsBounded pins the budget: a pull request carrying two
// hundred checks costs the report the same as one carrying six.
func TestStatusChecksValueIsBounded(t *testing.T) {
	t.Parallel()
	pr := statusPR{ChecksState: "FAILURE"}
	for i := range 200 {
		pr.Checks = append(pr.Checks, statusCheck{Name: fmt.Sprintf("shard-%d", i), State: "FAILURE", Required: true})
	}
	tally := statusChecksValue(pr)
	if !strings.Contains(tally, "0 passed") || !strings.Contains(tally, "200 failed") {
		t.Errorf("tally = %q, want 0 passed and 200 failed", tally)
	}
	if !strings.Contains(tally, "rollup failure") {
		t.Errorf("tally = %q, want the rollup named beside the counts", tally)
	}
	notable := statusNotableValue(pr.Checks)
	if n := strings.Count(notable, shipSep); n != statusNamedChecks {
		t.Errorf("notable names %d segments, want %d", n, statusNamedChecks)
	}
	if !strings.Contains(notable, fmt.Sprintf("+%d more", 200-statusNamedChecks)) {
		t.Errorf("notable = %q, want it to say how many it left unnamed", notable)
	}
}

// TestStatusNotableValueOrdersWorstFirst pins that a reader sees the failure
// before the things that merely have not finished.
func TestStatusNotableValueOrdersWorstFirst(t *testing.T) {
	t.Parallel()
	checks := []statusCheck{
		{Name: "running-one", State: "IN_PROGRESS"},
		{Name: "skipped-required", State: "SKIPPED", Required: true},
		{Name: "neutral-one", State: "NEUTRAL"},
		{Name: "failed-one", State: "FAILURE", Required: true},
		{Name: "passed-one", State: "SUCCESS"},
		{Name: "skipped-optional", State: "SKIPPED"},
	}
	got := statusNotableValue(checks)
	want := strings.Join([]string{
		"failed-one failure (required)",
		"neutral-one neutral",
		"running-one in_progress",
		"skipped-required skipped (required)",
	}, shipSep)
	if got != want {
		t.Errorf("notable = %q, want %q", got, want)
	}
	if statusNotableValue([]statusCheck{{Name: "only", State: "SUCCESS"}}) != "" {
		t.Error("notable named something on an all-green pull request")
	}
}

// TestStatusUnverifiedValue pins the caveat on a green this report cannot see
// inside: an external status is one aggregate, and its shards never arrive.
func TestStatusUnverifiedValue(t *testing.T) {
	t.Parallel()
	got := statusUnverifiedValue([]statusCheck{
		{Name: "buildkite/test", State: "SUCCESS", External: true, Required: true},
		{Name: "lint", State: "SUCCESS"},
		{Name: "buildkite/other", State: "FAILURE", External: true},
	})
	if !strings.Contains(got, "buildkite/test") {
		t.Errorf("unverified = %q, want the green external status named", got)
	}
	if strings.Contains(got, "lint") {
		t.Errorf("unverified = %q, want no check whose jobs are visible here", got)
	}
	if strings.Contains(got, "buildkite/other") {
		t.Errorf("unverified = %q, want no check that already reports red", got)
	}
	if statusUnverifiedValue([]statusCheck{{Name: "lint", State: "SUCCESS"}}) != "" {
		t.Error("unverified spoke up with no external status to caveat")
	}
}

// TestStatusRulesetChecks pins where a required check is read from. The older
// branch-protection field answers empty for a base a ruleset governs, so a
// report reading only that one marks nothing required and blocks on nothing.
func TestStatusRulesetChecks(t *testing.T) {
	t.Parallel()
	ref := &statusBaseRef{Name: "dev"}
	ref.Rules.Nodes = []statusRule{
		{Type: statusRuleRequiredChecks, Parameters: &statusRuleParameters{
			RequiredStatusChecks: []struct {
				Context string `json:"context"`
			}{{Context: "buildkite/test"}},
		}},
		{Type: "PULL_REQUEST", Parameters: &statusRuleParameters{}},
		{Type: "NON_FAST_FORWARD"},
	}
	got := statusRulesetChecks(ref)
	if len(got) != 1 || got[0] != "buildkite/test" {
		t.Errorf("statusRulesetChecks = %v, want [buildkite/test]", got)
	}
	if statusRulesetChecks(nil) != nil {
		t.Error("statusRulesetChecks invented a requirement for a base branch that is gone")
	}
	if statusRulesetChecks(&statusBaseRef{Name: "dev"}) != nil {
		t.Error("statusRulesetChecks invented a requirement from an empty rule set")
	}
}
