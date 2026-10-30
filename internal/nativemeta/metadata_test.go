package nativemeta

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestNativeCatalogTracksParentMovementAndDeletedParents(t *testing.T) {
	dir := repo(t)
	common := filepath.Join(dir, ".git")
	main := git(t, dir, "rev-parse", "main")
	git(t, dir, "switch", "-qc", "base")
	commit(t, dir, "base")
	base := git(t, dir, "rev-parse", "base")
	git(t, dir, "switch", "-qc", "child")
	commit(t, dir, "child")
	writeCatalog(t, common, catalog{SchemaVersion: 1, Repository: "github.com:owner/repo", Stacks: []stack{{ID: "stack-id", Number: 123, Trunk: branchRef{Branch: "main", Head: main}, Branches: []branchRef{{Branch: "base", Base: main}, {Branch: "child", Base: base}}}}})
	before, err := Read(t.Context(), common)
	if err != nil {
		t.Fatal(err)
	}
	if before["child"].NeedsRestack || before["child"].Parents[0] != (Ref{Ref: "base", SHA: base}) {
		t.Fatalf("unexpected child state: %+v", before["child"])
	}
	git(t, dir, "switch", "-q", "base")
	commit(t, dir, "changed")
	after, err := Read(t.Context(), common)
	if err != nil {
		t.Fatal(err)
	}
	if !after["child"].NeedsRestack {
		t.Fatal("parent movement must require restack")
	}
	git(t, dir, "switch", "-q", "main")
	git(t, dir, "branch", "-qD", "base")
	state, orphans, err := ReadOriginOrphans(t.Context(), common)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := state["child"]; ok {
		t.Fatal("deleted parent must leave an orphan")
	}
	if !reflect.DeepEqual(orphans, []Orphan{{Branch: "child", Parent: "base", ParentRevision: base}}) {
		t.Fatalf("orphans = %+v", orphans)
	}
}

func TestNativeRestackRecordsForkpointAndPreservesPRIdentity(t *testing.T) {
	dir := repo(t)
	common := filepath.Join(dir, ".git")
	main := git(t, dir, "rev-parse", "main")
	git(t, dir, "switch", "-qc", "feature")
	commit(t, dir, "feature")
	writeCatalog(t, common, catalog{SchemaVersion: 1, Repository: "github.com:owner/repo", Stacks: []stack{{ID: "S_123", Number: 123, Trunk: branchRef{Branch: "main", Head: main}, Branches: []branchRef{{Branch: "feature", Base: main, PullRequest: &pullRequestRef{Number: 42, ID: "PR_42", URL: "https://github.com/owner/repo/pull/42"}}}}}})
	git(t, dir, "switch", "-q", "main")
	commit(t, dir, "moved")
	moved := git(t, dir, "rev-parse", "main")
	if err := RecordRestacked(t.Context(), common, map[string]string{"feature": moved}); err != nil {
		t.Fatal(err)
	}
	state, err := Read(t.Context(), common)
	if err != nil {
		t.Fatal(err)
	}
	if state["feature"].NeedsRestack || state["feature"].Parents[0].SHA != moved {
		t.Fatalf("state = %+v", state)
	}
	c, _, err := load(common)
	if err != nil {
		t.Fatal(err)
	}
	if c.Stacks[0].ID != "S_123" || c.Stacks[0].Branches[0].PullRequest.Number != 42 {
		t.Fatalf("lost stack or PR identity: %+v", c.Stacks[0])
	}
}

func TestNativeReparentRejectsForkWithoutWriting(t *testing.T) {
	dir := repo(t)
	common := filepath.Join(dir, ".git")
	main := git(t, dir, "rev-parse", "main")
	for _, branch := range []string{"base", "child", "other"} {
		git(t, dir, "branch", branch)
	}
	writeCatalog(t, common, catalog{SchemaVersion: 1, Stacks: []stack{{Trunk: branchRef{Branch: "main", Head: main}, Branches: []branchRef{{Branch: "base", Base: main}, {Branch: "child", Base: main}}}, {Trunk: branchRef{Branch: "main", Head: main}, Branches: []branchRef{{Branch: "other", Base: main}}}}})
	before, err := os.ReadFile(filepath.Join(common, "gh-stack"))
	if err != nil {
		t.Fatal(err)
	}
	err = Reparent(t.Context(), common, map[string]string{"other": "base"})
	if err == nil || !strings.Contains(err.Error(), "linear") {
		t.Fatalf("fork error = %v", err)
	}
	after, err := os.ReadFile(filepath.Join(common, "gh-stack"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("rejected fork changed native catalog")
	}
}

func TestNativeReparentMovesAChainAndRetainsRecordedBase(t *testing.T) {
	dir := repo(t)
	common := filepath.Join(dir, ".git")
	main := git(t, dir, "rev-parse", "main")
	for _, branch := range []string{"base", "child", "other"} {
		git(t, dir, "branch", branch)
	}
	writeCatalog(t, common, catalog{SchemaVersion: 1, Stacks: []stack{{ID: "base-stack", Trunk: branchRef{Branch: "main", Head: main}, Branches: []branchRef{{Branch: "base", Base: main}, {Branch: "child", Base: "old-base"}}}, {ID: "other-stack", Trunk: branchRef{Branch: "main", Head: main}, Branches: []branchRef{{Branch: "other", Base: main}}}}})
	if err := Reparent(t.Context(), common, map[string]string{"child": "other"}); err != nil {
		t.Fatal(err)
	}
	state, err := Read(t.Context(), common)
	if err != nil {
		t.Fatal(err)
	}
	if state["child"].Parents[0] != (Ref{Ref: "other", SHA: "old-base"}) || !state["child"].NeedsRestack {
		t.Fatalf("child = %+v", state["child"])
	}
}

func TestSubmissionReceiptsAreAllOrNothing(t *testing.T) {
	dir := repo(t)
	common := filepath.Join(dir, ".git")
	main := git(t, dir, "rev-parse", "main")
	git(t, dir, "branch", "feature")
	if err := Init(t.Context(), common, "main"); err != nil {
		t.Fatal(err)
	}
	if err := AdoptRoot(t.Context(), common, "feature", "main", main, main); err != nil {
		t.Fatal(err)
	}
	wanted := map[string]Version{"feature": {HeadSha: main, BaseSha: main, BaseName: "main"}}
	if err := RecordSubmitted(t.Context(), common, wanted); err != nil {
		t.Fatal(err)
	}
	if err := RecordSubmitted(t.Context(), common, map[string]Version{"feature": {HeadSha: "bad"}, "untracked": {HeadSha: "bad"}}); err == nil {
		t.Fatal("untracked receipt accepted")
	}
	got, err := LastSubmitted(t.Context(), common)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, wanted) {
		t.Fatalf("receipts = %+v, want %+v", got, wanted)
	}
}

func TestFrozenReceiptSurvivesNativeCatalogRewrite(t *testing.T) {
	dir := repo(t)
	common := filepath.Join(dir, ".git")
	main := git(t, dir, "rev-parse", "main")
	git(t, dir, "branch", "feature")
	if err := Init(t.Context(), common, "main"); err != nil {
		t.Fatal(err)
	}
	if err := AdoptFrozen(t.Context(), common, "feature", "main", main, main); err != nil {
		t.Fatal(err)
	}
	c, _, err := load(common)
	if err != nil {
		t.Fatal(err)
	}
	c.Stacks[0].Number = 72
	writeCatalog(t, common, c)
	state, err := Read(t.Context(), common)
	if err != nil {
		t.Fatal(err)
	}
	if state["feature"].State != StateFrozen {
		t.Fatalf("freeze lost after native rewrite: %+v", state["feature"])
	}
}

func TestNoCatalogReadsNativeRemoteTrunk(t *testing.T) {
	dir := repo(t)
	common := filepath.Join(dir, ".git")
	state, err := Read(t.Context(), common)
	if err != nil {
		t.Fatal(err)
	}
	if len(state) != 1 || !state["main"].Trunk {
		t.Fatalf("state = %+v", state)
	}
	if _, err := os.Stat(filepath.Join(common, "gh-stack")); !os.IsNotExist(err) {
		t.Fatalf("read created native metadata: %v", err)
	}
}

func TestCatalogMutationHonorsNativeOperationLock(t *testing.T) {
	dir := repo(t)
	common := filepath.Join(dir, ".git")
	held, err := lockCatalog(t.Context(), filepath.Join(common, "gh-stack-operation.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	if err := Init(ctx, common, "main"); err == nil {
		t.Fatal("mutated catalog while native operation lock held")
	}
}

func repo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "config", "user.name", "test")
	git(t, dir, "config", "user.email", "test@example.com")
	git(t, dir, "commit", "--allow-empty", "-qm", "initial")
	git(t, dir, "update-ref", "refs/remotes/origin/main", git(t, dir, "rev-parse", "main"))
	git(t, dir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	return dir
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func commit(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", name)
	git(t, dir, "commit", "-qm", name)
}

func writeCatalog(t *testing.T, common string, c catalog) {
	t.Helper()
	if err := writeJSON(common, "gh-stack", c); err != nil {
		t.Fatal(err)
	}
}
