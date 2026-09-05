package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yasyf/cc-context/internal/gtmeta"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
	"github.com/yasyf/cc-context/internal/vcstest"
)

func TestPruneReparent(t *testing.T) {
	tests := []struct {
		name      string
		rows      []gtmeta.Row
		forgotten []string
		want      map[string]string
		wantErr   string
	}{
		{
			name: "moves a child onto the grandparent the prune keeps",
			rows: []gtmeta.Row{
				{Branch: "main"},
				{Branch: "mid", Parent: "main"},
				{Branch: "gone", Parent: "mid"},
				{Branch: "kid", Parent: "gone"},
			},
			forgotten: []string{"gone"},
			want:      map[string]string{"kid": "mid"},
		},
		{
			name: "collapses a chain of forgotten ancestors onto trunk",
			rows: []gtmeta.Row{
				{Branch: "main"},
				{Branch: "a", Parent: "main"},
				{Branch: "b", Parent: "a"},
				{Branch: "kid", Parent: "b"},
			},
			forgotten: []string{"a", "b"},
			want:      map[string]string{"kid": "main"},
		},
		{
			name: "floors at trunk when the chain leaves gt's rows",
			rows: []gtmeta.Row{
				{Branch: "main"},
				{Branch: "orphan", Parent: "never-tracked"},
				{Branch: "kid", Parent: "orphan"},
			},
			forgotten: []string{"orphan"},
			want:      map[string]string{"kid": "main"},
		},
		{
			name: "leaves a row whose parent survives",
			rows: []gtmeta.Row{
				{Branch: "main"},
				{Branch: "kid", Parent: "main"},
				{Branch: "gone", Parent: "main"},
			},
			forgotten: []string{"gone"},
			want:      map[string]string{},
		},
		{
			name: "moves nothing when a forgotten row's children go too",
			rows: []gtmeta.Row{
				{Branch: "main"},
				{Branch: "a", Parent: "main"},
				{Branch: "b", Parent: "a"},
			},
			forgotten: []string{"a", "b"},
			want:      map[string]string{},
		},
		{
			name: "moves every child of one forgotten row",
			rows: []gtmeta.Row{
				{Branch: "main"},
				{Branch: "gone", Parent: "main"},
				{Branch: "kid", Parent: "gone"},
				{Branch: "sibling", Parent: "gone"},
			},
			forgotten: []string{"gone"},
			want:      map[string]string{"kid": "main", "sibling": "main"},
		},
		{
			name: "refuses a parent chain that cycles",
			rows: []gtmeta.Row{
				{Branch: "main"},
				{Branch: "a", Parent: "b"},
				{Branch: "b", Parent: "a"},
				{Branch: "kid", Parent: "a"},
			},
			forgotten: []string{"a", "b"},
			wantErr:   "prune: gt parent chain of kid cycles at a — run gt track a",
		},
		{
			name: "refuses a chain that cycles back through the row being moved",
			rows: []gtmeta.Row{
				{Branch: "main"},
				{Branch: "gone", Parent: "kid"},
				{Branch: "kid", Parent: "gone"},
			},
			forgotten: []string{"gone"},
			wantErr:   "prune: gt parent chain of kid cycles at kid — run gt track kid",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			forgotten := make(map[string]bool, len(tt.forgotten))
			for _, branch := range tt.forgotten {
				forgotten[branch] = true
			}
			got, err := pruneReparent(tt.rows, forgotten, "main")
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("pruneReparent() error = %v, want %q", err, tt.wantErr)
				}
				if got != nil {
					t.Errorf("pruneReparent() = %v, want no moves alongside a refusal", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("pruneReparent(): %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("pruneReparent() = %v, want %v", got, tt.want)
			}
			for branch, parent := range tt.want {
				if got[branch] != parent {
					t.Errorf("pruneReparent()[%s] = %q, want %q", branch, got[branch], parent)
				}
			}
		})
	}
}

// TestPruneRepairsAStackOverAForgottenParent drives the incident shape: a
// harness-created branch sitting at trunk is gt's recorded parent of a live
// stack, so forgetting its row without moving the child leaves gtDownstack
// unable to resolve that stack at all.
func TestPruneRepairsAStackOverAForgottenParent(t *testing.T) {
	f := vcstest.Repo(t, vcstest.Remote())
	dir := render.Dir(f.Dir)
	trunkHead := gitAt(t, f.Dir, "rev-parse", "main")
	gitAt(t, f.Dir, "branch", "worktree-wf")
	gitAt(t, f.Dir, "switch", "-qc", "feature")
	if err := os.WriteFile(filepath.Join(f.Dir, "feature.txt"), []byte("feature\n"), 0o600); err != nil {
		t.Fatalf("write feature.txt: %v", err)
	}
	gitAt(t, f.Dir, "add", "feature.txt")
	gitAt(t, f.Dir, "commit", "-qm", "feature")

	commonDir, err := gtCommonDir(t.Context(), dir, "prune")
	if err != nil {
		t.Fatalf("gtCommonDir: %v", err)
	}
	vcstest.WriteGraphiteMeta(t, commonDir, `{"main":{"trunk":true},`+
		`"worktree-wf":{"parents":[{"ref":"main","sha":"`+trunkHead+`"}]},`+
		`"ghost":{"parents":[{"ref":"main","sha":"`+trunkHead+`"}]},`+
		`"feature":{"parents":[{"ref":"worktree-wf","sha":"`+trunkHead+`"}]}}`)

	trunk, err := vcs.ResolveTrunk(t.Context(), dir, "origin")
	if err != nil {
		t.Fatalf("ResolveTrunk: %v", err)
	}
	plan, err := prunePlanFor(t.Context(), dir, gtLane, trunk)
	if err != nil {
		t.Fatalf("prunePlanFor: %v", err)
	}
	if len(plan.merged) != 1 || plan.merged[0] != "worktree-wf" {
		t.Fatalf("plan.merged = %v, want [worktree-wf]", plan.merged)
	}
	if len(plan.stale) != 1 || plan.stale[0] != "ghost" {
		t.Fatalf("plan.stale = %v, want [ghost]", plan.stale)
	}
	if len(plan.reparent) != 1 || plan.reparent["feature"] != "main" {
		t.Fatalf("plan.reparent = %v, want map[feature:main]", plan.reparent)
	}

	t.Run("planning reports the move and writes nothing", func(t *testing.T) {
		want := "would delete 1 branches merged into main: worktree-wf · " +
			"would forget 1 graphite rows for deleted branches · " +
			"would reparent 1 graphite rows onto a surviving parent: feature → main"
		if got := prunePlanReport(plan, trunk, true); got != want {
			t.Errorf("prunePlanReport() = %q, want %q", got, want)
		}
		if got := pruneParentOf(t, commonDir, "feature"); got != "worktree-wf" {
			t.Errorf("feature's recorded parent = %q, want worktree-wf untouched before an apply", got)
		}
		if !gitBranchExists(t, f.Dir, "worktree-wf") {
			t.Error("worktree-wf deleted before an apply")
		}
	})

	if err := pruneApply(t.Context(), dir, gtLane, plan); err != nil {
		t.Fatalf("pruneApply: %v", err)
	}
	want := "deleted 1 branches merged into main: worktree-wf · " +
		"forgot 1 graphite rows for deleted branches · " +
		"reparented 1 graphite rows onto a surviving parent: feature → main"
	if got := prunePlanReport(plan, trunk, false); got != want {
		t.Errorf("prunePlanReport() = %q, want %q", got, want)
	}
	if gitBranchExists(t, f.Dir, "worktree-wf") {
		t.Error("worktree-wf survived the prune")
	}
	if got := pruneParentOf(t, commonDir, "feature"); got != "main" {
		t.Fatalf("feature's recorded parent = %q, want main", got)
	}

	state, err := gtStateQuery(t.Context(), dir, "ship")
	if err != nil {
		t.Fatalf("gtStateQuery: %v", err)
	}
	chain, err := gtDownstack("ship", state, "feature", "main")
	if err != nil {
		t.Fatalf("gtDownstack after prune: %v", err)
	}
	if len(chain) != 1 || chain[0] != "feature" {
		t.Errorf("gtDownstack = %v, want [feature]", chain)
	}
	if state["feature"].NeedsRestack {
		t.Error("feature reads as needing a restack, but its recorded revision is main's head")
	}
}

func pruneParentOf(t *testing.T, commonDir, branch string) string {
	t.Helper()
	rows, err := gtmeta.Rows(t.Context(), commonDir)
	if err != nil {
		t.Fatalf("gtmeta.Rows: %v", err)
	}
	for _, row := range rows {
		if row.Branch == branch {
			return row.Parent
		}
	}
	t.Fatalf("no branch_metadata row for %s in %v", branch, rows)
	return ""
}
