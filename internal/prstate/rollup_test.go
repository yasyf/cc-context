package prstate

import (
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
