package cli

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/gtmeta"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
	"github.com/yasyf/cc-context/internal/vcstest"
)

func TestPruneReparent(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	f := vcstest.Repo(t, vcstest.Remote())
	dir := render.Dir(f.Dir)
	trunkHead := gitAt(t, f.Env(), f.Dir, "rev-parse", "main")
	gitAt(t, f.Env(), f.Dir, "branch", "worktree-wf")
	gitAt(t, f.Env(), f.Dir, "switch", "-qc", "feature")
	if err := os.WriteFile(filepath.Join(f.Dir, "feature.txt"), []byte("feature\n"), 0o600); err != nil {
		t.Fatalf("write feature.txt: %v", err)
	}
	gitAt(t, f.Env(), f.Dir, "add", "feature.txt")
	gitAt(t, f.Env(), f.Dir, "commit", "-qm", "feature")

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
	api := stubGTAPI(t)
	plan, err := prunePlanFor(prunePRs(api.ctx(t.Context()), nil), dir, pruneGTLane, trunk, commonDir)
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
		if !gitBranchExists(t, f.Env(), f.Dir, "worktree-wf") {
			t.Error("worktree-wf deleted before an apply")
		}
	})

	if err := pruneApply(api.ctx(t.Context()), dir, pruneGTLane, plan, commonDir); err != nil {
		t.Fatalf("pruneApply: %v", err)
	}
	want := "deleted 1 branches merged into main: worktree-wf · " +
		"forgot 1 graphite rows for deleted branches · " +
		"reparented 1 graphite rows onto a surviving parent: feature → main"
	if got := prunePlanReport(plan, trunk, false); got != want {
		t.Errorf("prunePlanReport() = %q, want %q", got, want)
	}
	if gitBranchExists(t, f.Env(), f.Dir, "worktree-wf") {
		t.Error("worktree-wf survived the prune")
	}
	if got := pruneParentOf(t, commonDir, "feature"); got != "main" {
		t.Fatalf("feature's recorded parent = %q, want main", got)
	}

	state, err := gtStateQuery(api.ctx(t.Context()), dir, "ship")
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

// pruneGTLane is the graphite lane with its repository already resolved, so a
// squash lookup names it without asking gh.
var pruneGTLane = lane{kind: vcs.Git, gt: true, repo: &personalRepo}

// pruneSquashFixture cuts each branch off main with a commit of its own, tracks
// every one of them on main, and returns the repository ready to plan.
func pruneSquashFixture(t *testing.T, branches ...string) (*vcstest.Fixture, render.Dir, vcs.Trunk, string) {
	t.Helper()
	f := vcstest.Repo(t, vcstest.Remote())
	dir := render.Dir(f.Dir)
	trunkHead := gitAt(t, f.Env(), f.Dir, "rev-parse", "main")
	state := `{"main":{"trunk":true}`
	for _, branch := range branches {
		gitAt(t, f.Env(), f.Dir, "switch", "-qc", branch, "main")
		if err := os.WriteFile(filepath.Join(f.Dir, branch+".txt"), []byte(branch+"\n"), 0o600); err != nil {
			t.Fatalf("write %s.txt: %v", branch, err)
		}
		gitAt(t, f.Env(), f.Dir, "add", branch+".txt")
		gitAt(t, f.Env(), f.Dir, "commit", "-qm", branch)
		state += `,"` + branch + `":{"parents":[{"ref":"main","sha":"` + trunkHead + `"}]}`
	}
	gitAt(t, f.Env(), f.Dir, "switch", "-q", "main")
	commonDir, err := gtCommonDir(t.Context(), dir, "prune")
	if err != nil {
		t.Fatalf("gtCommonDir: %v", err)
	}
	vcstest.WriteGraphiteMeta(t, commonDir, state+"}")
	trunk, err := vcs.ResolveTrunk(t.Context(), dir, "origin")
	if err != nil {
		t.Fatalf("ResolveTrunk: %v", err)
	}
	return f, dir, trunk, commonDir
}

// TestPruneDeletesMergedBranchesFromADetachedHead pins the merged listing to
// branches: git branch --merged lists a detached HEAD as "(HEAD detached at
// <ref>)", which git branch -d then refused as a branch name.
func TestPruneDeletesMergedBranchesFromADetachedHead(t *testing.T) {
	t.Parallel()
	f, dir, trunk, commonDir := pruneSquashFixture(t)
	gitAt(t, f.Env(), f.Dir, "branch", "merged", "main")
	gitAt(t, f.Env(), f.Dir, "switch", "-q", "--detach", "main")
	api := stubGTAPI(t)

	plan, err := prunePlanFor(api.ctx(t.Context()), dir, pruneGTLane, trunk, commonDir)
	if err != nil {
		t.Fatalf("prunePlanFor: %v", err)
	}
	if !slices.Equal(plan.merged, []string{"merged"}) {
		t.Fatalf("merged = %q, want [merged]", plan.merged)
	}
	if err := pruneApply(api.ctx(t.Context()), dir, pruneGTLane, plan, commonDir); err != nil {
		t.Fatalf("pruneApply: %v", err)
	}
	if gitBranchExists(t, f.Env(), f.Dir, "merged") {
		t.Error("merged survived the prune")
	}
}

// TestPruneSeesSquashLandings pins the landing git branch --merged cannot see:
// a pull request GitHub reports landed at the branch's own head is deleted,
// while one whose branch moved past the landed head, one a worktree holds, one
// gt reports as diverged, and one with no landed pull request all survive.
func TestPruneSeesSquashLandings(t *testing.T) {
	t.Parallel()
	f, dir, trunk, commonDir := pruneSquashFixture(t, "landed", "moved", "held", "diverged", "open")
	api := stubGTAPI(t)
	prs := map[string]*stackPR{}
	for _, branch := range []string{"landed", "moved", "held", "diverged"} {
		prs[branch] = &stackPR{Head: gitAt(t, f.Env(), f.Dir, "rev-parse", branch), Landed: true}
	}
	prs["open"] = &stackPR{Head: gitAt(t, f.Env(), f.Dir, "rev-parse", "open")}
	gitAt(t, f.Env(), f.Dir, "switch", "-q", "moved")
	gitAt(t, f.Env(), f.Dir, "commit", "-q", "--allow-empty", "-m", "past the merge")
	gitAt(t, f.Env(), f.Dir, "switch", "-q", "main")
	gitAt(t, f.Env(), f.Dir, "worktree", "add", "-q", filepath.Join(t.TempDir(), "held"), "held")
	pruneMarkDiverged(t, commonDir, "diverged")

	plan, err := prunePlanFor(prunePRs(api.ctx(t.Context()), prs), dir, pruneGTLane, trunk, commonDir)
	if err != nil {
		t.Fatalf("prunePlanFor: %v", err)
	}
	if got := pruneSquashedNames(plan); !slices.Equal(got, []string{"landed"}) {
		t.Fatalf("squashed = %v, want [landed]", got)
	}
	if !slices.Equal(plan.held, []string{"held"}) {
		t.Errorf("held = %v, want [held]", plan.held)
	}
	if !slices.Equal(plan.diverged, []string{"diverged"}) {
		t.Errorf("diverged = %v, want [diverged]", plan.diverged)
	}
	want := "would delete 0 branches merged into main · would delete 1 branches whose pull request squash-landed: landed · " +
		"1 merged branches held by a worktree · 1 diverged, left alone — gt track or gt untrack each"
	if got := prunePlanReport(plan, trunk, true); got != want {
		t.Errorf("dry-run report = %q, want %q", got, want)
	}

	if err := pruneApply(api.ctx(t.Context()), dir, pruneGTLane, plan, commonDir); err != nil {
		t.Fatalf("pruneApply: %v", err)
	}
	if gitBranchExists(t, f.Env(), f.Dir, "landed") {
		t.Error("landed survived the prune")
	}
	rows, err := gtmeta.Rows(api.ctx(t.Context()), commonDir)
	if err != nil {
		t.Fatalf("gtmeta.Rows: %v", err)
	}
	for _, row := range rows {
		if row.Branch == "landed" {
			t.Error("landed's graphite row survived the prune")
		}
	}
	for _, branch := range []string{"moved", "held", "diverged", "open"} {
		if !gitBranchExists(t, f.Env(), f.Dir, branch) {
			t.Errorf("%s was deleted", branch)
		}
	}
}

// TestPruneDeletesALandedBranchWithItsConfig pins what a squash delete takes
// with it: the branch goes with its git config, and a pull request that closed
// without landing leaves its branch alone.
func TestPruneDeletesALandedBranchWithItsConfig(t *testing.T) {
	f, dir, trunk, commonDir := pruneSquashFixture(t, "queued", "abandoned")
	api := stubGTAPI(t)
	prs := map[string]*stackPR{
		"queued":    {Number: 30, Head: gitAt(t, f.Env(), f.Dir, "rev-parse", "queued"), Landed: true},
		"abandoned": {Number: 31, Head: gitAt(t, f.Env(), f.Dir, "rev-parse", "abandoned"), State: "CLOSED"},
	}
	gitAt(t, f.Env(), f.Dir, "config", "branch.queued.remote", "origin")
	gitAt(t, f.Env(), f.Dir, "config", "branch.queued.merge", "refs/heads/queued")

	plan, err := prunePlanFor(prunePRs(api.ctx(t.Context()), prs), dir, pruneGTLane, trunk, commonDir)
	if err != nil {
		t.Fatalf("prunePlanFor: %v", err)
	}
	if got := pruneSquashedNames(plan); !slices.Equal(got, []string{"queued"}) {
		t.Fatalf("squashed = %v, want [queued]", got)
	}
	if err := pruneApply(api.ctx(t.Context()), dir, pruneGTLane, plan, commonDir); err != nil {
		t.Fatalf("pruneApply: %v", err)
	}
	if gitBranchExists(t, f.Env(), f.Dir, "queued") {
		t.Error("queued survived the prune")
	}
	if config := restackRead(t, filepath.Join(f.Dir, ".git", "config")); strings.Contains(config, `[branch "queued"]`) {
		t.Errorf("queued's git config survived the prune:\n%s", config)
	}
	if !gitBranchExists(t, f.Env(), f.Dir, "abandoned") {
		t.Error("abandoned was deleted with no squash on trunk")
	}
}

// TestPruneRefusesASquashBranchThatMoved pins the delete's own guard: the plan
// read the branch at its merged head, and a commit landing on it before the
// apply is work the squash never took.
func TestPruneRefusesASquashBranchThatMoved(t *testing.T) {
	f, dir, trunk, commonDir := pruneSquashFixture(t, "landed")
	api := stubGTAPI(t)
	prs := map[string]*stackPR{"landed": {Number: 10, Head: gitAt(t, f.Env(), f.Dir, "rev-parse", "landed"), Landed: true}}

	plan, err := prunePlanFor(prunePRs(api.ctx(t.Context()), prs), dir, pruneGTLane, trunk, commonDir)
	if err != nil {
		t.Fatalf("prunePlanFor: %v", err)
	}
	gitAt(t, f.Env(), f.Dir, "switch", "-q", "landed")
	gitAt(t, f.Env(), f.Dir, "commit", "-q", "--allow-empty", "-m", "after the plan")
	gitAt(t, f.Env(), f.Dir, "switch", "-q", "main")

	if err := pruneApply(api.ctx(t.Context()), dir, pruneGTLane, plan, commonDir); err == nil {
		t.Fatal("pruneApply deleted a branch that moved past its merged head")
	}
	if got := gitAt(t, f.Env(), f.Dir, "log", "-1", "--format=%s", "landed"); got != "after the plan" {
		t.Errorf("landed's head = %q, want the commit made after the plan", got)
	}
}

// TestPruneRefusesASquashBranchCheckedOutAfterThePlan pins the other guard:
// update-ref deletes a checked-out branch without a word, so a worktree that
// took the branch after the plan stops the delete.
func TestPruneRefusesASquashBranchCheckedOutAfterThePlan(t *testing.T) {
	f, dir, trunk, commonDir := pruneSquashFixture(t, "landed")
	api := stubGTAPI(t)
	prs := map[string]*stackPR{"landed": {Number: 10, Head: gitAt(t, f.Env(), f.Dir, "rev-parse", "landed"), Landed: true}}

	plan, err := prunePlanFor(prunePRs(api.ctx(t.Context()), prs), dir, pruneGTLane, trunk, commonDir)
	if err != nil {
		t.Fatalf("prunePlanFor: %v", err)
	}
	gitAt(t, f.Env(), f.Dir, "worktree", "add", "-q", filepath.Join(t.TempDir(), "landed"), "landed")

	if err := pruneApply(api.ctx(t.Context()), dir, pruneGTLane, plan, commonDir); err == nil {
		t.Fatal("pruneApply deleted a branch a worktree checked out after the plan")
	}
	if !gitBranchExists(t, f.Env(), f.Dir, "landed") {
		t.Error("landed was deleted out from under its worktree")
	}
}

// prunePRs answers prune's pull-request reads from prs in place of GitHub.
func prunePRs(ctx context.Context, prs map[string]*stackPR) context.Context {
	return withStackPRs(ctx, func(_ context.Context, _ render.Dir, _ string, branches []string) (map[string]*stackPR, error) {
		asked := map[string]*stackPR{}
		for _, branch := range branches {
			if pr, ok := prs[branch]; ok {
				asked[branch] = pr
			}
		}
		return asked, nil
	})
}

func pruneMarkDiverged(t *testing.T, commonDir, branch string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(commonDir, ".graphite_metadata.db"))
	if err != nil {
		t.Fatalf("open graphite metadata: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`UPDATE branch_metadata SET validation_result = 'BAD_PARENT_NAME' WHERE branch_name = ?`, branch); err != nil {
		t.Fatalf("mark %s diverged: %v", branch, err)
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
