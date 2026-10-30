package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/nativemeta"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcstest"
)

func adoptedNativeReplayRepo(t *testing.T, change string) (*vcstest.Fixture, string, string) {
	t.Helper()
	f := shipRepo(t, vcstest.Remote())
	f.Decorate(func(ctx context.Context) context.Context { return withStackProvider(ctx, true) })
	common := filepath.Join(f.Dir, ".git")
	trunk := shipHead(t, f)
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "base")
	writeShipFile(t, f.Dir, "base.txt", "base\n")
	mustRun(t, f.Env(), f.Dir, "git", "add", "base.txt")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "base")
	originalBase := shipHead(t, f)
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "feature")
	if change != "empty" {
		writeShipFile(t, f.Dir, "feature.txt", "feature\n")
		mustRun(t, f.Env(), f.Dir, "git", "add", "feature.txt")
	}
	mustRun(t, f.Env(), f.Dir, "git", "commit", "--allow-empty", "-qm", "feature")
	original := shipHead(t, f)
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "base", "feature")
	if err := nativemeta.Init(f.Context(), common, "main"); err != nil {
		t.Fatal(err)
	}
	if err := nativemeta.AdoptRoot(f.Context(), common, "base", "main", trunk, originalBase); err != nil {
		t.Fatal(err)
	}
	if err := nativemeta.AdoptRoot(f.Context(), common, "feature", "base", originalBase, original); err != nil {
		t.Fatal(err)
	}
	if change != "no receipt" {
		payload, err := json.Marshal(stackPublication{Branch: "feature", Source: original, SourceBase: originalBase, Head: original, Base: originalBase, Parent: "base"})
		if err != nil {
			t.Fatal(err)
		}
		oid, err := render.RunCLIStdin(f.Context(), render.Dir(f.Dir), "git", []string{"hash-object", "-w", "--stdin"}, payload)
		if err != nil {
			t.Fatal(err)
		}
		mustRun(t, f.Env(), f.Dir, "git", "update-ref", stackPublicationRef("feature", "receipt"), strings.TrimSpace(oid))
	}
	server := f.WorktreePath("server")
	mustRun(t, f.Env(), f.Dir, "git", "worktree", "add", "-q", "--detach", server, "main")
	writeShipFile(t, server, "upstream.txt", "upstream\n")
	mustRun(t, f.Env(), server, "git", "add", "upstream.txt")
	mustRun(t, f.Env(), server, "git", "commit", "-qm", "upstream")
	mustRun(t, f.Env(), server, "git", "cherry-pick", originalBase)
	replayBase := gitAt(t, f.Env(), server, "rev-parse", "HEAD")
	mustRun(t, f.Env(), server, "git", "cherry-pick", "--allow-empty", "--keep-redundant-commits", original)
	switch change {
	case "patch":
		writeShipFile(t, server, "feature.txt", "different feature\n")
		mustRun(t, f.Env(), server, "git", "add", "feature.txt")
		mustRun(t, f.Env(), server, "git", "commit", "--amend", "--no-edit")
	case "authored":
		mustRun(t, f.Env(), server, "git", "commit", "--amend", "-qm", "different message")
	case "merge":
		side := gitAt(t, f.Env(), server, "commit-tree", replayBase+"^{tree}", "-p", replayBase, "-m", "side")
		merged := gitAt(t, f.Env(), server, "commit-tree", "HEAD^{tree}", "-p", replayBase, "-p", side, "-m", "feature")
		mustRun(t, f.Env(), server, "git", "checkout", "-q", "--detach", merged)
	}
	adopted := gitAt(t, f.Env(), server, "rev-parse", "HEAD")
	mustRun(t, f.Env(), server, "git", "push", "-qf", "origin", "HEAD:feature")
	mustRun(t, f.Env(), server, "git", "checkout", "-q", "--detach", replayBase)
	writeShipFile(t, server, "later-parent.txt", "later parent\n")
	mustRun(t, f.Env(), server, "git", "add", "later-parent.txt")
	mustRun(t, f.Env(), server, "git", "commit", "-qm", "later parent")
	mustRun(t, f.Env(), server, "git", "push", "-qf", "origin", "HEAD:base")
	mustRun(t, f.Env(), f.Dir, "git", "fetch", "-q", "origin")
	mustRun(t, f.Env(), f.Dir, "git", "reset", "--hard", adopted)
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "child")
	writeShipFile(t, f.Dir, "child.txt", "child\n")
	mustRun(t, f.Env(), f.Dir, "git", "add", "child.txt")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "child")
	child := shipHead(t, f)
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "child")
	if err := nativemeta.AdoptRoot(f.Context(), common, "child", "feature", adopted, child); err != nil {
		t.Fatal(err)
	}
	mustRun(t, f.Env(), f.Dir, "git", "reflog", "expire", "--expire=now", "--all")
	api := newGTAPIStub(t)
	api.openPRs("base", "feature", "child")
	f.Decorate(api.ctx)
	stubOpenPRs(t, f, nil, "base", "feature", "child")
	stubStackStanding(t, f, nil)
	return f, replayBase, originalBase
}

func TestNativeAdoptedReplayUsesItsPublicationFork(t *testing.T) {
	f, fork, parent := adoptedNativeReplayRepo(t, "")
	before := shipHead(t, f)
	common := filepath.Join(f.Dir, ".git")
	l, err := resolveLane(f.Context(), "stack rebase", f.Dir, false)
	if err != nil {
		t.Fatal(err)
	}
	run, err := stackPlan(f.Context(), l, common, stackRebaseOpts{tip: "child", tipOnly: true, restack: true, submit: true, dryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	feature := run.branch("feature")
	if feature.OldBase != fork || feature.SourceBase != fork || !feature.Kept {
		t.Fatalf("adopted ancestor=%+v, want kept with fork %s", feature, fork)
	}
	if child := run.branch("child"); child.OldBase != feature.Head || child.Local != before {
		t.Fatalf("child source changed: %+v", child)
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "base"); got != parent {
		t.Fatal("parent source moved during planning")
	}
	if got := shipHead(t, f); got != before {
		t.Fatal("child source moved during planning")
	}
	state, err := nativemeta.Read(f.Context(), common)
	if err != nil {
		t.Fatal(err)
	}
	if state["feature"].Parents[0].SHA != parent {
		t.Fatal("planning rewrote the native catalog fork")
	}
}

func TestNativeAdoptedReplayRefusesUnprovenForks(t *testing.T) {
	for _, change := range []string{"patch", "authored", "merge", "empty", "no receipt"} {
		t.Run(change, func(t *testing.T) {
			f, _, _ := adoptedNativeReplayRepo(t, change)
			before := shipHead(t, f)
			l, err := resolveLane(f.Context(), "stack rebase", f.Dir, false)
			if err != nil {
				t.Fatal(err)
			}
			_, err = stackPlan(f.Context(), l, filepath.Join(f.Dir, ".git"), stackRebaseOpts{tip: "child", tipOnly: true, restack: true, submit: true, dryRun: true})
			if err == nil || !strings.Contains(err.Error(), "feature carries no head of base") {
				t.Fatalf("unproven fork accepted or wrong refusal: %v", err)
			}
			if got := shipHead(t, f); got != before {
				t.Fatal("refused plan moved the child source")
			}
		})
	}
}
