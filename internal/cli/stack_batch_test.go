package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/render"
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
	paths := make([]string, 0, 4)
	paths = append(paths, filepath.Join(t.TempDir(), "base"), filepath.Join(t.TempDir(), "left"), f.Dir)
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

func TestStackListBatchPreservesRetiredHolderSymlinkSpelling(t *testing.T) {
	f := shipGTRepo(t, vcstest.GTStack("base", "child"))
	path := filepath.Join(t.TempDir(), "base")
	mustRun(t, f.Env(), f.Dir, "git", "worktree", "add", "-q", path, "base")
	path = gitAt(t, f.Env(), path, "rev-parse", "--show-toplevel")
	common := gitAt(t, f.Env(), f.Dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	admin := gitAt(t, f.Env(), path, "rev-parse", "--absolute-git-dir")
	retired := filepath.Join(common, "retired", filepath.Base(admin))
	if err := os.MkdirAll(filepath.Dir(retired), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(admin, retired); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, ".git"), []byte("gitdir: "+retired+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(path, alias); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(retired, "gitdir"), []byte(filepath.Join(alias, ".git")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	want := stackSingleReport(t, f, path)
	got := stackBatchReports(t, f, path)
	if got[0].Error != "" || got[0].Listing == nil || !reflect.DeepEqual(*got[0].Listing, want) {
		t.Fatalf("retired batch = %+v, want %+v", got[0], want)
	}
	if want.Branches[0].Path != alias || want.Branches[0].Current {
		t.Fatalf("fixture did not preserve raw retired holder alias: %+v", want.Branches[0])
	}
}

func TestStackListBatchSharesRetiredAndExternalHoldingCensus(t *testing.T) {
	for _, tc := range []struct {
		name    string
		retired bool
	}{
		{"retired", true},
		{"external", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := shipGTRepo(t, vcstest.GTStack("base", "one", "two", "three"))
			common := gitAt(t, f.Env(), f.Dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
			admins := filepath.Join(t.TempDir(), "admins")
			if tc.retired {
				admins = filepath.Join(common, "retired")
			}
			if err := os.MkdirAll(admins, 0o700); err != nil {
				t.Fatal(err)
			}
			paths := make([]string, 0, 4)
			for _, branch := range []string{"base", "one", "two"} {
				path := filepath.Join(t.TempDir(), branch)
				mustRun(t, f.Env(), f.Dir, "git", "worktree", "add", "-q", path, branch)
				path = gitAt(t, f.Env(), path, "rev-parse", "--show-toplevel")
				admin := gitAt(t, f.Env(), path, "rev-parse", "--absolute-git-dir")
				moved := filepath.Join(admins, filepath.Base(admin))
				if err := os.Rename(admin, moved); err != nil {
					t.Fatal(err)
				}
				for name, content := range map[string]string{
					filepath.Join(path, ".git"):       "gitdir: " + moved + "\n",
					filepath.Join(moved, "commondir"): common + "\n",
				} {
					if err := os.WriteFile(name, []byte(content), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				paths = append(paths, path)
			}
			paths = append(paths, f.Dir)
			want := make([]stackListReport, len(paths))
			for i, path := range paths {
				want[i] = stackSingleReport(t, f, path)
			}
			shipResetLog(t, f)
			got := stackBatchReports(t, f, paths...)
			for i, result := range got {
				if result.Error != "" || result.Listing == nil || !reflect.DeepEqual(*result.Listing, want[i]) {
					t.Errorf("result %d = %+v, want %+v", i, result, want[i])
				}
			}
			var reads int
			for _, invocation := range vcstest.Invocations(t, f.ArgvLog) {
				if slices.Equal(invocation, []string{"git", "--git-dir", common, "config", "--bool", "core.bare"}) {
					reads++
				}
			}
			if reads != 1 {
				t.Errorf("holding censuses = %d, want 1 for %d queries", reads, len(paths))
			}
		})
	}
}

func TestStackLandedCheckUsesAncestryWithoutAnExistenceProbe(t *testing.T) {
	f := shipGTRepo(t, vcstest.GTStack("parent"))
	parent := gitAt(t, f.Env(), f.Dir, "rev-parse", "parent")
	shipResetLog(t, f)
	landed, err := gtLandedIn(f.Context(), render.Dir(f.Dir), "stack list", parent, "refs/remotes/origin/main")
	if err != nil || landed {
		t.Fatalf("unlanded parent = %v, %v", landed, err)
	}
	var probes, merges int
	for _, invocation := range vcstest.Invocations(t, f.ArgvLog) {
		if slices.Equal(invocation, []string{"git", "cat-file", "-e", parent + "^{commit}"}) {
			probes++
		}
		if slices.Contains(invocation, "merge-tree") {
			merges++
		}
	}
	if probes != 0 || merges != 1 {
		t.Errorf("healthy orphan check used %d existence probes and %d merges, want 0 and 1", probes, merges)
	}
}

func TestStackLandedCheckPreservesMissingObjectAndTrunkErrors(t *testing.T) {
	f := shipGTRepo(t, vcstest.GTStack("parent"))
	parent := gitAt(t, f.Env(), f.Dir, "rev-parse", "parent")
	tree := gitAt(t, f.Env(), f.Dir, "rev-parse", "parent^{tree}")
	for _, tc := range []struct {
		name      string
		rev       string
		trunk     string
		wantError bool
	}{
		{name: "missing parent", rev: strings.Repeat("f", 40), trunk: "refs/remotes/origin/main"},
		{name: "noncommit parent", rev: tree, trunk: "refs/remotes/origin/main"},
		{name: "missing trunk", rev: parent, trunk: strings.Repeat("f", 40), wantError: true},
		{name: "both missing", rev: strings.Repeat("e", 40), trunk: strings.Repeat("f", 40)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			landed, err := gtLandedIn(f.Context(), render.Dir(f.Dir), "stack list", tc.rev, tc.trunk)
			if landed || (err != nil) != tc.wantError {
				t.Fatalf("landed = %v, err = %v, want false and error=%v", landed, err, tc.wantError)
			}
		})
	}
}

func TestStackListBatchObservesChangedReplacement(t *testing.T) {
	f := shipGTRepo(t, vcstest.GTStack("parent", "child"))
	parent := gitAt(t, f.Env(), f.Dir, "rev-parse", "parent")
	restackAdvanceRemote(t, f, "main", "trunk.txt", "trunk content\n")
	mustRun(t, f.Env(), f.Dir, "git", "fetch", "-q", "origin")
	trunk := gitAt(t, f.Env(), f.Dir, "rev-parse", "refs/remotes/origin/main")
	mustRun(t, f.Env(), f.Dir, "git", "branch", "-D", "parent")
	batch := stackBatch{stores: make(map[string]*stackBatchStore)}
	if _, err := batch.list(f.Context(), f.Dir); err == nil {
		t.Fatal("unlanded parent was adopted before replacement")
	}
	mustRun(t, f.Env(), f.Dir, "git", "replace", trunk, parent)
	listing, err := batch.list(f.Context(), f.Dir)
	if err != nil || listing == nil {
		t.Fatalf("listing after replacement = %+v, %v", listing, err)
	}
	if got := pruneParentOf(t, filepath.Join(f.Dir, ".git"), "child"); got != "main" {
		t.Errorf("parent after replacement = %s, want main", got)
	}
}
