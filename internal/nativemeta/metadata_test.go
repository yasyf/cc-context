package nativemeta

import (
	"context"
	"errors"
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

func TestNativeAdoptOpensASecondChildChainAsItsOwnStackOnTheSharedParent(t *testing.T) {
	dir := repo(t)
	common := filepath.Join(dir, ".git")
	main := git(t, dir, "rev-parse", "main")
	for _, branch := range []string{"base", "child", "fork", "fork-tip"} {
		git(t, dir, "branch", branch)
	}
	writeCatalog(t, common, catalog{SchemaVersion: 1, Stacks: []stack{{ID: "S_7", Number: 7, Trunk: branchRef{Branch: "main", Head: main}, Branches: []branchRef{{Branch: "base", Base: main, Head: main}, {Branch: "child", Base: main}}}}})
	if err := AdoptRoot(t.Context(), common, "fork", "base", main, main); err != nil {
		t.Fatal(err)
	}
	if err := AdoptRoot(t.Context(), common, "fork-tip", "fork", main, main); err != nil {
		t.Fatal(err)
	}
	c, _, err := load(common)
	if err != nil {
		t.Fatal(err)
	}
	want := []stack{
		{ID: "S_7", Number: 7, Trunk: branchRef{Branch: "main", Head: main}, Branches: []branchRef{{Branch: "base", Base: main, Head: main}, {Branch: "child", Base: main}}},
		{Trunk: branchRef{Branch: "base", Head: main}, Branches: []branchRef{{Branch: "fork", Base: main, Head: main}, {Branch: "fork-tip", Base: main, Head: main}}},
	}
	if !reflect.DeepEqual(c.Stacks, want) {
		t.Fatalf("stacks = %+v, want %+v", c.Stacks, want)
	}
	state, err := Read(t.Context(), common)
	if err != nil {
		t.Fatal(err)
	}
	if state["child"].Parents[0].Ref != "base" || state["fork"].Parents[0].Ref != "base" || state["fork-tip"].Parents[0].Ref != "fork" {
		t.Fatalf("state = %+v", state)
	}
	if err := Forget(t.Context(), common, []string{"base"}); err != nil {
		t.Fatal(err)
	}
	if err := RecordSubmitted(t.Context(), common, map[string]Version{"fork-tip": {HeadSha: main}}); err != nil {
		t.Fatal(err)
	}
	state, err = Read(t.Context(), common)
	if err != nil {
		t.Fatal(err)
	}
	if state["child"].Parents[0].Ref != "main" || state["fork"].Parents[0].Ref != "main" || state["fork-tip"].Parents[0].Ref != "fork" {
		t.Fatalf("state after the shared parent lands = %+v", state)
	}
}

func TestNativeReparentOntoAStackedBranchOpensAForkStack(t *testing.T) {
	dir := repo(t)
	common := filepath.Join(dir, ".git")
	main := git(t, dir, "rev-parse", "main")
	for _, branch := range []string{"base", "child", "other"} {
		git(t, dir, "branch", branch)
	}
	writeCatalog(t, common, catalog{SchemaVersion: 1, Stacks: []stack{{ID: "base-stack", Trunk: branchRef{Branch: "main", Head: main}, Branches: []branchRef{{Branch: "base", Base: main}, {Branch: "child", Base: main}}}, {ID: "other-stack", Trunk: branchRef{Branch: "main", Head: main}, Branches: []branchRef{{Branch: "other", Base: main}}}}})
	if err := Reparent(t.Context(), common, map[string]string{"other": "base"}); err != nil {
		t.Fatal(err)
	}
	c, _, err := load(common)
	if err != nil {
		t.Fatal(err)
	}
	want := []stack{
		{ID: "base-stack", Trunk: branchRef{Branch: "main", Head: main}, Branches: []branchRef{{Branch: "base", Base: main}, {Branch: "child", Base: main}}},
		{ID: "other-stack", Trunk: branchRef{Branch: "base"}, Branches: []branchRef{{Branch: "other", Base: main}}},
	}
	if !reflect.DeepEqual(c.Stacks, want) {
		t.Fatalf("stacks = %+v, want %+v", c.Stacks, want)
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

func TestNativeAdoptRefusesToMintATrunkFromAnUntrackedParent(t *testing.T) {
	dir := repo(t)
	common := filepath.Join(dir, ".git")
	main := git(t, dir, "rev-parse", "main")
	for _, branch := range []string{"feature", "untracked", "child"} {
		git(t, dir, "branch", branch)
	}
	writeCatalog(t, common, catalog{SchemaVersion: 1, Stacks: []stack{{Trunk: branchRef{Branch: "main", Head: main}, Branches: []branchRef{{Branch: "feature", Base: main}}}}})
	before, err := os.ReadFile(filepath.Join(common, "gh-stack"))
	if err != nil {
		t.Fatal(err)
	}
	err = AdoptRoot(t.Context(), common, "child", "untracked", main, main)
	if err == nil || !strings.Contains(err.Error(), "untracked is in no native stack on main") {
		t.Fatalf("adopt onto untracked parent = %v", err)
	}
	after, err := os.ReadFile(filepath.Join(common, "gh-stack"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("refused adoption changed native catalog")
	}
	if err := AdoptRoot(t.Context(), common, "child", "feature", main, main); err != nil {
		t.Fatal(err)
	}
}

func strayRootFixture(t *testing.T, history func(context.Context, string, string) (bool, bool, error)) (string, string) {
	t.Helper()
	dir := repo(t)
	common := filepath.Join(dir, ".git")
	main := git(t, dir, "rev-parse", "main")
	for _, branch := range []string{"feature", "other", "child"} {
		git(t, dir, "branch", branch)
	}
	writeCatalog(t, common, catalog{SchemaVersion: 1, Stacks: []stack{{Trunk: branchRef{Branch: "main", Head: main}, Branches: []branchRef{{Branch: "feature", Base: main}}}, {Trunk: branchRef{Branch: "other", Head: main}, Branches: []branchRef{{Branch: "child", Base: main}}}}})
	if err := writeJSON(common, "ccx-stack-receipts.json", receipts{Frozen: map[string]bool{"child": true}, Submitted: map[string]Version{"child": {HeadSha: main}}}); err != nil {
		t.Fatal(err)
	}
	judged := branchHistory
	t.Cleanup(func() { branchHistory = judged })
	branchHistory = func(ctx context.Context, commonDir, branch string) (bool, bool, error) {
		if branch != "other" {
			t.Errorf("judged root %s; want only the non-origin/HEAD root other", branch)
		}
		return history(ctx, commonDir, branch)
	}
	return common, main
}

func TestNativeWritesKeepARootThatIsARealTrunk(t *testing.T) {
	common, main := strayRootFixture(t, func(context.Context, string, string) (bool, bool, error) { return true, false, nil })
	if err := RecordSubmitted(t.Context(), common, map[string]Version{"feature": {HeadSha: main, BaseSha: main, BaseName: "main"}}); err != nil {
		t.Fatal(err)
	}
	c, r, err := load(common)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Stacks) != 2 || c.Stacks[1].Trunk.Branch != "other" || !r.Frozen["child"] {
		t.Fatalf("catalog=%+v receipts=%+v; want the release-style trunk and its receipts kept", c.Stacks, r)
	}
}

func TestNativeWritesDropARootThatWasAPullRequestHead(t *testing.T) {
	common, main := strayRootFixture(t, func(context.Context, string, string) (bool, bool, error) { return true, true, nil })
	if err := RecordSubmitted(t.Context(), common, map[string]Version{"feature": {HeadSha: main, BaseSha: main, BaseName: "main"}}); err != nil {
		t.Fatal(err)
	}
	c, r, err := load(common)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Stacks) != 1 || c.Stacks[0].Trunk.Branch != "main" {
		t.Fatalf("catalog = %+v; want only the stack on main", c.Stacks)
	}
	if _, ok := r.Submitted["child"]; ok || r.Frozen["child"] || r.Submitted["feature"].BaseName != "main" {
		t.Fatalf("receipts = %+v; want child's dropped and feature's recorded", r)
	}
	if err := AdoptRoot(t.Context(), common, "child", "feature", main, main); err != nil {
		t.Fatalf("adopt the dropped branch onto its real parent = %v", err)
	}
}

func TestNativeWritesDropARootMissingFromOrigin(t *testing.T) {
	common, main := strayRootFixture(t, func(context.Context, string, string) (bool, bool, error) { return false, false, nil })
	if err := RecordSubmitted(t.Context(), common, map[string]Version{"feature": {HeadSha: main, BaseSha: main, BaseName: "main"}}); err != nil {
		t.Fatal(err)
	}
	c, _, err := load(common)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Stacks) != 1 || c.Stacks[0].Trunk.Branch != "main" {
		t.Fatalf("catalog = %+v; want only the stack on main", c.Stacks)
	}
}

func TestNativeWritesRefuseWhenGitHubCannotJudgeARoot(t *testing.T) {
	common, main := strayRootFixture(t, func(context.Context, string, string) (bool, bool, error) {
		return false, false, errors.New("GET /repos/owner/repo/branches/other: 502")
	})
	before, err := os.ReadFile(filepath.Join(common, "gh-stack"))
	if err != nil {
		t.Fatal(err)
	}
	err = RecordSubmitted(t.Context(), common, map[string]Version{"feature": {HeadSha: main, BaseSha: main, BaseName: "main"}})
	if err == nil || !strings.Contains(err.Error(), "native stack root other") || !strings.Contains(err.Error(), "502") {
		t.Fatalf("write with an unjudgeable root = %v; want the GitHub failure", err)
	}
	after, err := os.ReadFile(filepath.Join(common, "gh-stack"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("refused write changed the native catalog")
	}
}

func TestGitHubRepoParsesOnlyGitHubOrigins(t *testing.T) {
	for origin, want := range map[string]string{
		"https://github.com/org/repo.git":        "org/repo",
		"git@github.com:org/repo.git":            "org/repo",
		"ssh://git@github.com:22/org/repo.git":   "org/repo",
		"https://GitHub.com/org/repo/":           "org/repo",
		"git@github.company.com:org/repo.git":    "",
		"https://gitlab.com/github.com/repo.git": "",
		"https://github.com/org/repo/extra":      "",
	} {
		owner, name, err := githubRepo(origin)
		got := ""
		if err == nil {
			got = owner + "/" + name
		}
		if got != want {
			t.Fatalf("githubRepo(%s) = %q, %v; want %q", origin, got, err, want)
		}
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

func TestEmptyCatalogOnAFreshCloneSeedsTheTrunkFromTheRemoteDefaultBranch(t *testing.T) {
	upstream := t.TempDir()
	git(t, upstream, "init", "-q", "-b", "dev")
	git(t, upstream, "config", "user.name", "test")
	git(t, upstream, "config", "user.email", "test@example.com")
	git(t, upstream, "commit", "--allow-empty", "-qm", "initial")
	git(t, upstream, "branch", "main")
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "scratch")
	git(t, dir, "remote", "add", "origin", upstream)
	git(t, dir, "fetch", "-q", "origin")
	common := filepath.Join(dir, ".git")

	trunk, err := ReadTrunk(t.Context(), common)
	if err != nil {
		t.Fatal(err)
	}
	if trunk != "dev" {
		t.Fatalf("trunk = %q, want the remote's default branch dev", trunk)
	}
	if got := git(t, dir, "symbolic-ref", "refs/remotes/origin/HEAD"); got != "refs/remotes/origin/dev" {
		t.Fatalf("origin/HEAD = %q, want it seeded to refs/remotes/origin/dev", got)
	}
	if _, err := os.Stat(filepath.Join(common, "gh-stack")); !os.IsNotExist(err) {
		t.Fatalf("reading the trunk created native metadata: %v", err)
	}
}

func TestCatalogMutationHonorsNativeOperationLock(t *testing.T) {
	dir := repo(t)
	common := filepath.Join(dir, ".git")
	held, err := lockCatalog(t.Context(), common, "gh-stack-operation.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
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
	cmd := exec.Command("git", args...) // #nosec G204 -- arguments are authored by repository fixtures, never external input.
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
