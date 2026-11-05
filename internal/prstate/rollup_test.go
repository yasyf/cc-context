package prstate

import (
	"encoding/json"
	"slices"
	"testing"
	"time"
)

func TestLatestLetsEachCheckNamesLatestRunStand(t *testing.T) {
	t.Parallel()
	at := func(minute int) time.Time { return time.Date(2026, 10, 16, 5, minute, 0, 0, time.UTC) }
	run := func(name, conclusion string, started time.Time, id int64) Context {
		status := "COMPLETED"
		if conclusion == "" {
			status = "QUEUED"
		}
		return Context{Typename: "CheckRun", Name: name, Conclusion: conclusion, Status: status, StartedAt: started, DatabaseID: id}
	}
	inSuite := func(c Context, suite, workflow int64) Context {
		c.CheckSuite.DatabaseID, c.CheckSuite.WorkflowRun.Workflow.DatabaseID = suite, workflow
		return c
	}
	status := Context{Typename: "StatusContext", Context: "buildkite/test", State: "SUCCESS"}
	tests := []struct {
		name      string
		state     string
		total     int
		nodes     []Context
		wantState string
		want      []Context
	}{
		{
			name:      "cancelled run listed before the success that superseded it",
			state:     "FAILURE",
			total:     3,
			nodes:     []Context{run("grade", "CANCELLED", at(16), 1), run("grade", "SUCCESS", at(17), 2), status},
			wantState: "SUCCESS",
			want:      []Context{run("grade", "SUCCESS", at(17), 2), status},
		},
		{
			name:      "cancelled run listed after the success that superseded it",
			state:     "FAILURE",
			total:     3,
			nodes:     []Context{run("grade", "SUCCESS", at(17), 2), run("grade", "CANCELLED", at(16), 1), status},
			wantState: "SUCCESS",
			want:      []Context{run("grade", "SUCCESS", at(17), 2), status},
		},
		{
			name:      "cancelled run with no later run",
			state:     "FAILURE",
			total:     3,
			nodes:     []Context{run("grade", "CANCELLED", at(18), 3), run("grade", "SUCCESS", at(17), 2), status},
			wantState: "FAILURE",
			want:      []Context{run("grade", "CANCELLED", at(18), 3), status},
		},
		{
			name:      "queued run after a cancelled one",
			state:     "FAILURE",
			total:     2,
			nodes:     []Context{run("grade", "CANCELLED", at(16), 1), run("grade", "", time.Time{}, 2)},
			wantState: "PENDING",
			want:      []Context{run("grade", "", time.Time{}, 2)},
		},
		{
			name:      "later skip under a graded success",
			state:     "SUCCESS",
			total:     2,
			nodes:     []Context{run("grade", "SKIPPED", at(42), 4), run("grade", "SUCCESS", at(41), 3)},
			wantState: "SUCCESS",
			want:      []Context{run("grade", "SUCCESS", at(41), 3)},
		},
		{
			name:      "later skip under a graded failure",
			state:     "FAILURE",
			total:     2,
			nodes:     []Context{run("grade", "FAILURE", at(41), 3), run("grade", "SKIPPED", at(42), 4)},
			wantState: "FAILURE",
			want:      []Context{run("grade", "FAILURE", at(41), 3)},
		},
		{
			name:      "same start broken by run id",
			state:     "FAILURE",
			total:     2,
			nodes:     []Context{run("grade", "SUCCESS", at(16), 6), run("grade", "CANCELLED", at(16), 5)},
			wantState: "SUCCESS",
			want:      []Context{run("grade", "SUCCESS", at(16), 6)},
		},
		{
			name:      "two workflows' jobs of one name",
			state:     "FAILURE",
			total:     2,
			nodes:     []Context{inSuite(run("test", "FAILURE", at(2), 1), 71, 7), inSuite(run("test", "SUCCESS", at(5), 2), 81, 8)},
			wantState: "FAILURE",
			want:      []Context{inSuite(run("test", "FAILURE", at(2), 1), 71, 7), inSuite(run("test", "SUCCESS", at(5), 2), 81, 8)},
		},
		{
			name:      "one workflow's push run failing under its later pull request run",
			state:     "FAILURE",
			total:     2,
			nodes:     []Context{inSuite(run("test", "FAILURE", at(16), 1), 71, 7), inSuite(run("test", "SUCCESS", at(17), 2), 72, 7)},
			wantState: "FAILURE",
			want:      []Context{inSuite(run("test", "FAILURE", at(16), 1), 71, 7), inSuite(run("test", "SUCCESS", at(17), 2), 72, 7)},
		},
		{
			name:      "re-run attempt in the failed run's suite",
			state:     "FAILURE",
			total:     2,
			nodes:     []Context{inSuite(run("test", "FAILURE", at(16), 1), 71, 7), inSuite(run("test", "SUCCESS", at(20), 2), 71, 7)},
			wantState: "SUCCESS",
			want:      []Context{inSuite(run("test", "SUCCESS", at(20), 2), 71, 7)},
		},
		{
			name:      "run cancelled for a newer run of its workflow",
			state:     "FAILURE",
			total:     2,
			nodes:     []Context{inSuite(run("grade", "CANCELLED", at(16), 1), 71, 7), inSuite(run("grade", "SUCCESS", at(17), 2), 72, 7)},
			wantState: "SUCCESS",
			want:      []Context{inSuite(run("grade", "SUCCESS", at(17), 2), 72, 7)},
		},
		{
			name:      "cancelled run beside another workflow's job of its name",
			state:     "FAILURE",
			total:     2,
			nodes:     []Context{inSuite(run("test", "CANCELLED", at(16), 1), 71, 7), inSuite(run("test", "SUCCESS", at(17), 2), 81, 8)},
			wantState: "FAILURE",
			want:      []Context{inSuite(run("test", "CANCELLED", at(16), 1), 71, 7), inSuite(run("test", "SUCCESS", at(17), 2), 81, 8)},
		},
		{
			name:      "truncated rollup keeps GitHub's state",
			state:     "FAILURE",
			total:     150,
			nodes:     []Context{run("grade", "CANCELLED", at(16), 1), run("grade", "SUCCESS", at(17), 2)},
			wantState: "FAILURE",
			want:      []Context{run("grade", "SUCCESS", at(17), 2)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rollup := &Rollup{State: tt.state}
			rollup.Contexts.TotalCount, rollup.Contexts.Nodes = tt.total, tt.nodes
			got := rollup.Latest()
			if got.State != tt.wantState || !slices.Equal(got.Contexts.Nodes, tt.want) {
				t.Errorf("Latest() = %s %+v, want %s %+v", got.State, got.Contexts.Nodes, tt.wantState, tt.want)
			}
		})
	}
	if (*Rollup)(nil).Latest() != nil {
		t.Error("Latest() of no rollup is not nil")
	}
}

func TestRESTRollupGradesEachSuitesRunOfOneName(t *testing.T) {
	t.Parallel()
	var runs restCheckRuns
	var workflows restWorkflowRuns
	if err := json.Unmarshal([]byte(`{"check_runs":[
		{"id":1,"name":"test","status":"completed","conclusion":"failure","started_at":"2026-11-05T08:02:56Z","check_suite":{"id":11}},
		{"id":2,"name":"test","status":"completed","conclusion":"success","started_at":"2026-11-05T08:03:14Z","check_suite":{"id":12}}]}`), &runs); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{"workflow_runs":[
		{"check_suite_id":11,"workflow_id":7,"status":"completed"},
		{"check_suite_id":12,"workflow_id":7,"status":"in_progress"}]}`), &workflows); err != nil {
		t.Fatal(err)
	}
	rollup, _ := restRollup(runs, restStatus{}, workflows)
	if rollup.State != "FAILURE" || len(rollup.Contexts.Nodes) != 2 || rollup.UnfinishedActions != 1 {
		t.Errorf("rollup = %+v, want both suites' test runs standing, graded FAILURE, with one run unfinished", rollup)
	}
}
