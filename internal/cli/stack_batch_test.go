package cli

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/vcstest"
)

func stackBatchReports(t *testing.T, f *vcstest.Fixture, paths ...string) []stackBatchResult {
	t.Helper()
	out, errOut, err := runStackCmd(t, f, append([]string{"list-batch"}, paths...)...)
	if err != nil {
		t.Fatalf("stack list-batch: %v\n%s", err, errOut)
	}
	var results []stackBatchResult
	if err := json.Unmarshal([]byte(out), &results); err != nil {
		t.Fatalf("decode batch: %v\n%s", err, out)
	}
	return results
}

func stackSingleReport(t *testing.T, f *vcstest.Fixture, path string) stackListReport {
	t.Helper()
	cmd := newStackCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"list", "--json"})
	if err := cmd.ExecuteContext(f.ContextIn(path)); err != nil {
		t.Fatal(err)
	}
	var result stackListReport
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestStackListBatchSharesGraphAndPreservesForks(t *testing.T) {
	f := shipGTRepo(t, vcstest.GTStack("base", "left"))
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "base")
	shipGTStack(t, f, "right")
	paths := []string{filepath.Join(t.TempDir(), "base"), filepath.Join(t.TempDir(), "left"), f.Dir}
	for i, branch := range []string{"base", "left"} {
		mustRun(t, f.Env(), f.Dir, "git", "worktree", "add", "-q", paths[i], branch)
		paths[i] = gitAt(t, f.Env(), paths[i], "rev-parse", "--show-toplevel")
	}
	paths = append(paths, paths[1])
	want := make([]stackListReport, len(paths))
	for i, path := range paths {
		want[i] = stackSingleReport(t, f, path)
	}
	common := gitAt(t, f.Env(), f.Dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	shipResetLog(t, f)
	results := stackBatchReports(t, f, paths...)
	if len(results) != len(paths) {
		t.Fatalf("got %d reports, want %d", len(results), len(paths))
	}
	for i, result := range results {
		if result.Path != paths[i] || result.Error != "" || result.Listing == nil || !reflect.DeepEqual(*result.Listing, want[i]) {
			t.Errorf("result %d = %+v, want path %s listing %+v", i, result, paths[i], want[i])
		}
	}
	var reads int
	for _, invocation := range vcstest.Invocations(t, f.ArgvLog) {
		if slices.Equal(invocation, gtRefsArgvIn(common)) {
			reads++
		}
	}
	if reads != 1 {
		t.Errorf("shared graph reads = %d, want 1 for %d worktree queries", reads, len(paths))
	}
	for _, branch := range results[1].Listing.Branches {
		if branch.Branch == "right" {
			t.Error("left listing included sibling right")
		}
	}
}

func TestStackListBatchIsolatesStoresAndPathFailures(t *testing.T) {
	f := shipGTRepo(t, vcstest.GTStack("first"))
	other := shipGTRepo(t, vcstest.GTStack("second"))
	missing := filepath.Join(t.TempDir(), "missing")
	results := stackBatchReports(t, f, f.Dir, missing, other.Dir, f.Dir)
	if len(results) != 4 || results[1].Error == "" || results[1].Listing != nil {
		t.Fatalf("missing-path result = %+v", results)
	}
	for _, index := range []int{0, 2, 3} {
		if results[index].Error != "" || results[index].Listing == nil {
			t.Fatalf("valid result %d = %+v", index, results[index])
		}
	}
	for i, want := range map[int]string{0: "first", 2: "second", 3: "first"} {
		if got := results[i].Listing.Branches; len(got) != 1 || got[0].Branch != want {
			t.Errorf("store %d branches = %+v, want %s", i, got, want)
		}
	}
}

func TestStackListBatchAdoptsOnlyRequestedOrphans(t *testing.T) {
	f := shipGTRepo(t, vcstest.GTStack("landed", "child"))
	restackSquashRemote(t, f, "main", "landed (#41)", "landed")
	mustRun(t, f.Env(), f.Dir, "git", "fetch", "-q", "origin")
	mustRun(t, f.Env(), f.Dir, "git", "branch", "-D", "landed")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "main")
	shipGTStack(t, f, "independent")
	child := filepath.Join(t.TempDir(), "child")
	mustRun(t, f.Env(), f.Dir, "git", "worktree", "add", "-q", child, "child")
	shipResetLog(t, f)
	results := stackBatchReports(t, f, f.Dir)
	if results[0].Error != "" {
		t.Fatal(results[0].Error)
	}
	for _, invocation := range vcstest.Invocations(t, f.ArgvLog) {
		if slices.Contains(invocation, "merge-tree") {
			t.Fatalf("independent query examined unrelated orphan: %v", invocation)
		}
	}
	common := filepath.Join(f.Dir, ".git")
	if parent := pruneParentOf(t, common, "child"); parent != "landed" {
		t.Fatalf("unrequested child parent = %s, want landed", parent)
	}
	results = stackBatchReports(t, f, child, f.Dir)
	if results[0].Error != "" || results[1].Error != "" {
		t.Fatalf("requested orphan results = %+v", results)
	}
	if parent := pruneParentOf(t, common, "child"); parent != "main" {
		t.Errorf("requested child parent = %s, want main", parent)
	}
	if got := results[0].Listing.Branches; len(got) != 1 || got[0].Branch != "child" {
		t.Errorf("adopted child listing = %+v", got)
	}
}

func TestStackListBatchPreservesTrunkAndDetachedErrors(t *testing.T) {
	f := shipGTRepo(t, vcstest.GTStack("base"))
	trunk := filepath.Join(t.TempDir(), "trunk")
	detached := filepath.Join(t.TempDir(), "detached")
	mustRun(t, f.Env(), f.Dir, "git", "worktree", "add", "-q", trunk, "main")
	mustRun(t, f.Env(), f.Dir, "git", "worktree", "add", "-q", "--detach", detached, "HEAD")
	results := stackBatchReports(t, f, trunk, detached, f.Dir)
	if !strings.Contains(results[0].Error, "main is trunk") || !strings.Contains(results[1].Error, "detached HEAD") || results[2].Error != "" {
		t.Fatalf("batch errors = %+v", results)
	}
}
