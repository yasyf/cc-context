package cli

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/vcstest"
)

func TestStackConflictWorkspaceStartsSparse(t *testing.T) {
	t.Parallel()
	f := sparseConflictRepo(t)

	_, _, err := runStackCmd(t, f, "rebase", "--no-push")
	if err == nil {
		t.Fatal("stack rebase succeeded, want the conflict on feature")
	}
	if !strings.Contains(err.Error(), "(detached, sparse,") {
		t.Errorf("brief = %v, want the workspace named sparse", err)
	}
	if got := sparseConflicted(err); !slices.Equal(got, []string{"conflict/deep/c.txt"}) {
		t.Errorf("conflicted files = %q, want [conflict/deep/c.txt]", got)
	}
	ws := stackWorkspaceOf(t, err)
	var roots []string
	for line := range strings.Lines(gitAt(t, f.Env(), ws, "ls-tree", "HEAD")) {
		if fields := strings.Fields(line); fields[1] == "blob" {
			roots = append(roots, fields[3])
		}
	}
	if !slices.Contains(roots, "top.txt") {
		t.Fatalf("root files = %q, want top.txt among them", roots)
	}
	for _, p := range append(roots, "conflict/deep/c.txt") {
		if _, err := os.Stat(filepath.Join(ws, p)); err != nil {
			t.Errorf("%s missing from the workspace: %v", p, err)
		}
	}
	if _, err := os.Stat(filepath.Join(ws, "other")); !os.IsNotExist(err) {
		t.Errorf("other/ checked out in the workspace: %v", err)
	}
	if got := gitAt(t, f.Env(), ws, "sparse-checkout", "list"); got != "conflict/deep" {
		t.Errorf("sparse-checkout list = %q, want conflict/deep", got)
	}
	for key, want := range map[string]string{"core.sparseCheckout": "true", "core.sparseCheckoutCone": "true", "index.sparse": "false", "core.hooksPath": "/dev/null"} {
		if got := gitAt(t, f.Env(), ws, "config", "--worktree", "--get", key); got != want {
			t.Errorf("worktree %s = %q, want %q", key, got, want)
		}
	}
	if got := gitAt(t, f.Env(), ws, "ls-files", "-t", "--", "other/x.txt"); got != "S other/x.txt" {
		t.Errorf("ls-files -t other/x.txt = %q, want it tracked with skip-worktree", got)
	}
}

func TestStackConflictWorkspaceWidensOnEveryStop(t *testing.T) {
	t.Parallel()
	f := shipGTRepo(t)
	stubStackPRs(t, f, nil)
	tabbed := "ünïcode/tab\tq\"uote.txt"
	rename := func(mark string) string {
		var s strings.Builder
		for i := range 20 {
			line := "line " + string(rune('a'+i))
			if i == 10 {
				line = mark
			}
			s.WriteString(line + "\n")
		}
		return s.String()
	}
	for _, p := range []string{"space dir/a.txt", "-dash/b.txt", "star*dir/c.txt", tabbed, "gone/m.txt"} {
		writeShipFile(t, f.Dir, p, "base\n")
	}
	writeShipFile(t, f.Dir, "moved/r.txt", rename("base"))
	writeShipFile(t, f.Dir, "keep/k.txt", "keep\n")
	sparseCommit(t, f, "layout")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "main")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "feature")
	for _, p := range []string{"space dir/a.txt", "-dash/b.txt", "star*dir/c.txt", tabbed, "gone/m.txt"} {
		writeShipFile(t, f.Dir, p, "branch\n")
		sparseCommit(t, f, "branch "+p)
	}
	mustRun(t, f.Env(), f.Dir, "git", "rm", "-q", "--", "moved/r.txt")
	writeShipFile(t, f.Dir, "renamed/deep/r.txt", rename("branch"))
	sparseCommit(t, f, "branch rename")
	mustRun(t, f.Env(), f.Dir, "gt", "track", "-f", "--no-interactive")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "main")
	for _, p := range []string{"space dir/a.txt", "-dash/b.txt", "star*dir/c.txt", tabbed} {
		writeShipFile(t, f.Dir, p, "trunk\n")
	}
	mustRun(t, f.Env(), f.Dir, "git", "rm", "-q", "--", "gone/m.txt")
	writeShipFile(t, f.Dir, "moved/r.txt", rename("trunk"))
	sparseCommit(t, f, "trunk")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "main")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "feature")

	stops := []struct {
		path    string
		dir     string
		resolve func(ws string)
	}{
		{"space dir/a.txt", "space dir", nil},
		{"-dash/b.txt", "-dash", nil},
		{"star*dir/c.txt", "star*dir", nil},
		{tabbed, "ünïcode", nil},
		{"gone/m.txt", "gone", func(ws string) { mustRun(t, f.Env(), ws, "git", "rm", "-q", "--", "gone/m.txt") }},
		{"renamed/deep/r.txt", "renamed/deep", func(ws string) {
			writeShipFile(t, ws, "renamed/deep/r.txt", rename("resolved"))
			mustRun(t, f.Env(), ws, "git", "add", "--", "renamed/deep/r.txt")
		}},
	}
	_, _, err := runStackCmd(t, f, "rebase", "--no-push")
	var ws string
	cone := make([]string, 0, len(stops))
	var kept []string
	for i, stop := range stops {
		if err == nil {
			t.Fatalf("stop %d: succeeded, want the conflict on %q", i, stop.path)
		}
		if i == 0 {
			ws = stackWorkspaceOf(t, err)
		}
		if got := sparseConflicted(err); !slices.Equal(got, []string{stop.path}) {
			t.Fatalf("stop %d: conflicted files = %q, want [%q]\n%v", i, got, stop.path, err)
		}
		cone = append(cone, stop.dir)
		slices.Sort(cone)
		listed := strings.Split(gitAt(t, f.Env(), ws, "-c", "core.quotePath=false", "sparse-checkout", "list"), "\n")
		if !slices.Equal(listed, cone) {
			t.Errorf("stop %d: sparse-checkout list = %q, want %q", i, listed, cone)
		}
		for _, p := range append(slices.Clone(kept), stop.path) {
			if _, err := os.Stat(filepath.Join(ws, p)); err != nil {
				t.Errorf("stop %d: %q is not on disk in the workspace: %v", i, p, err)
			}
		}
		if _, err := os.Stat(filepath.Join(ws, "keep")); !os.IsNotExist(err) {
			t.Errorf("stop %d: keep/ checked out though nothing there conflicted: %v", i, err)
		}
		if stop.resolve == nil {
			writeShipFile(t, ws, stop.path, "resolved\n")
			mustRun(t, f.Env(), ws, "git", "add", "--", stop.path)
			kept = append(kept, stop.path)
		} else {
			stop.resolve(ws)
		}
		_, _, err = runStackCmd(t, f, "continue")
	}
	if err != nil {
		t.Fatalf("final continue: %v", err)
	}
	if !stackOnto(t, f, "main", "feature") {
		t.Error("feature is not on the new trunk")
	}
	for _, p := range kept {
		if got := gitAt(t, f.Env(), f.Dir, "show", "feature:"+p); got != "resolved" {
			t.Errorf("feature:%q = %q, want the resolution", p, got)
		}
	}
	if got := gitAt(t, f.Env(), f.Dir, "show", "feature:renamed/deep/r.txt"); got+"\n" != rename("resolved") {
		t.Errorf("feature:renamed/deep/r.txt = %q, want the resolved rename", got)
	}
	tree := strings.Split(strings.TrimRight(mustRun(t, f.Env(), f.Dir, "git", "ls-tree", "-r", "-z", "--name-only", "feature"), "\x00"), "\x00")
	for _, p := range []string{"gone/m.txt", "moved/r.txt"} {
		if slices.Contains(tree, p) {
			t.Errorf("feature still holds %s: %q", p, tree)
		}
	}
	if !slices.Contains(tree, tabbed) {
		t.Errorf("feature's tree = %q, want %q in it", tree, tabbed)
	}
}

func TestStackConflictWorkspaceLeavesTheSourceAlone(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup func(t *testing.T, f *vcstest.Fixture)
		gains bool
	}{
		{"full source", func(*testing.T, *vcstest.Fixture) {}, true},
		{"per-worktree sparse source", func(t *testing.T, f *vcstest.Fixture) {
			mustRun(t, f.Env(), f.Dir, "git", "config", "extensions.worktreeConfig", "true")
			mustRun(t, f.Env(), f.Dir, "git", "config", "--worktree", "core.sparseCheckout", "true")
			mustRun(t, f.Env(), f.Dir, "git", "sparse-checkout", "set", "--cone", "other")
		}, false},
		{"shared sparse settings in the common config", func(t *testing.T, f *vcstest.Fixture) {
			mustRun(t, f.Env(), f.Dir, "git", "config", "core.sparseCheckout", "true")
			mustRun(t, f.Env(), f.Dir, "git", "config", "core.sparseCheckoutCone", "false")
			mustRun(t, f.Env(), f.Dir, "git", "config", "index.sparse", "true")
			writeShipFile(t, filepath.Join(f.Dir, ".git", "info"), "sparse-checkout", "/*\n!/other/\n")
			mustRun(t, f.Env(), f.Dir, "git", "read-tree", "-mu", "HEAD")
		}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := sparseConflictRepo(t)
			tt.setup(t, f)
			sparseSettleIndex(t, f)
			before := sparseSourceState(t, f)

			_, _, err := runStackCmd(t, f, "rebase", "--no-push")
			if err == nil {
				t.Fatal("stack rebase succeeded, want the conflict on feature")
			}
			ws := stackWorkspaceOf(t, err)
			for key, want := range map[string]string{"core.sparseCheckout": "true", "core.sparseCheckoutCone": "true", "index.sparse": "false"} {
				if got := gitAt(t, f.Env(), ws, "config", "--type=bool", "--get", key); got != want {
					t.Errorf("workspace %s = %q, want %q", key, got, want)
				}
			}
			if got := gitAt(t, f.Env(), ws, "sparse-checkout", "list"); got != "conflict/deep" {
				t.Errorf("workspace sparse-checkout list = %q, want conflict/deep", got)
			}
			if _, _, err := runStackCmd(t, f, "abort"); err != nil {
				t.Fatalf("abort: %v", err)
			}

			after := sparseSourceState(t, f)
			for _, key := range []string{"index", "config", "config.worktree", "sparse-checkout", "files"} {
				if before[key] != after[key] {
					t.Errorf("the source's %s changed:\nbefore: %q\nafter:  %q", key, before[key], after[key])
				}
			}
			var want []string
			if tt.gains {
				want = []string{"\tworktreeConfig = true"}
				if !slices.Contains(strings.Split(before["common"], "\n"), "[extensions]") {
					want = []string{"[extensions]", "\tworktreeConfig = true"}
				}
			}
			if added, ok := sparseAdded(before["common"], after["common"]); !ok || !slices.Equal(added, want) {
				t.Errorf("the common config gained %q (every old line kept: %v), want %q\nbefore:\n%s\nafter:\n%s", added, ok, want, before["common"], after["common"])
			}
		})
	}
}

func TestStackContinueFromInsideTheWorkspaceKeepsIt(t *testing.T) {
	t.Parallel()
	for _, inside := range []bool{true, false} {
		t.Run(map[bool]string{true: "from the workspace", false: "from the source"}[inside], func(t *testing.T) {
			t.Parallel()
			f := shipGTRepo(t, vcstest.GTStack("base"))
			stubStackPRs(t, f, nil)
			stackConflicting(t, f)
			_, _, err := runStackCmd(t, f, "rebase", "--no-push")
			if err == nil {
				t.Fatal("stack rebase succeeded, want the conflict on feature")
			}
			ws := stackWorkspaceOf(t, err)
			writeShipFile(t, ws, "c.txt", "trunk\nfeature\n")
			mustRun(t, f.Env(), ws, "git", "add", "c.txt")

			from := f.Dir
			if inside {
				from = ws
			}
			out, _, err := runStackCmdIn(t, f, from, "continue")
			if err != nil {
				t.Fatalf("continue: %v", err)
			}
			if !strings.Contains(out, "resolved feature") {
				t.Errorf("continue output = %q, want the resolution named", out)
			}
			if got := strings.Contains(out, "left "+ws+" in place"); got != inside {
				t.Errorf("continue output = %q, names the workspace kept: %v, want %v", out, got, inside)
			}
			if !stackOnto(t, f, "base", "feature") || !stackOnto(t, f, "main", "base") {
				t.Error("the stack did not land on the new trunk")
			}
			if states, _ := filepath.Glob(filepath.Join(f.Dir, ".git", stackRebaseStateDir, "*", stackRebaseState)); len(states) != 0 {
				t.Errorf("continue left run state behind: %q", states)
			}
			sparseAssertWorkspace(t, f, ws, inside)
			if !inside {
				return
			}
			if got, want := gitAt(t, f.Env(), ws, "rev-parse", "HEAD"), gitAt(t, f.Env(), f.Dir, "rev-parse", "feature"); got != want {
				t.Errorf("the kept workspace sits at %s, want feature's new head %s", got, want)
			}
			if got := gitAt(t, f.Env(), ws, "status", "--porcelain"); got != "" {
				t.Errorf("the kept workspace is dirty:\n%s", got)
			}
		})
	}
}

func TestStackAbortFromInsideTheWorkspaceKeepsIt(t *testing.T) {
	t.Parallel()
	for _, inside := range []bool{true, false} {
		t.Run(map[bool]string{true: "from the workspace", false: "from the source"}[inside], func(t *testing.T) {
			t.Parallel()
			f := shipGTRepo(t, vcstest.GTStack("base"))
			stubStackPRs(t, f, nil)
			stackConflicting(t, f)
			feature := gitAt(t, f.Env(), f.Dir, "rev-parse", "feature")
			_, _, err := runStackCmd(t, f, "rebase", "--no-push")
			if err == nil {
				t.Fatal("stack rebase succeeded, want the conflict on feature")
			}
			ws := stackWorkspaceOf(t, err)

			from := f.Dir
			if inside {
				from = ws
			}
			out, _, err := runStackCmdIn(t, f, from, "abort")
			if err != nil {
				t.Fatalf("abort: %v", err)
			}
			want := "aborted · no branch moved"
			if inside {
				want += " · left " + ws + " in place"
			}
			if !strings.HasPrefix(out, want) || strings.Contains(out, "left ") != inside {
				t.Errorf("abort output = %q, want %q", out, want)
			}
			if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "feature"); got != feature {
				t.Errorf("feature moved to %s on abort", got)
			}
			sparseAssertWorkspace(t, f, ws, inside)
		})
	}
}

func TestStackConflictWorkspaceRunsNoHook(t *testing.T) {
	t.Parallel()
	f := sparseConflictRepo(t)
	hooks, marker := t.TempDir(), filepath.Join(t.TempDir(), "ran")
	for _, name := range []string{"post-index-change", "post-checkout", "pre-rebase", "post-rewrite", "pre-commit", "prepare-commit-msg", "commit-msg", "post-commit"} {
		writeExecutable(t, filepath.Join(hooks, name), "#!/bin/sh\necho \""+name+" $(pwd -P)\" >> "+marker+"\n")
	}
	gitAt(t, f.Env(), f.Dir, "config", "core.hooksPath", hooks)

	_, _, err := runStackCmd(t, f, "rebase", "--no-push")
	if err == nil {
		t.Fatal("stack rebase succeeded, want the conflict on feature")
	}
	ws, err := filepath.EvalSymlinks(stackWorkspaceOf(t, err))
	if err != nil {
		t.Fatal(err)
	}
	writeShipFile(t, ws, "conflict/deep/c.txt", "both\n")
	mustRun(t, f.Env(), ws, "git", "add", "conflict/deep/c.txt")
	if _, _, err := runStackCmd(t, f, "continue"); err != nil {
		t.Fatalf("continue: %v", err)
	}
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-q", "--allow-empty", "-m", "control")
	raw, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "post-commit") {
		t.Fatalf("hooks = %q, want the control commit's post-commit to prove the hooks live", raw)
	}
	for line := range strings.Lines(string(raw)) {
		if strings.Contains(line, ws) {
			t.Errorf("a hook ran in the conflict workspace: %s", strings.TrimSpace(line))
		}
	}
}

func TestStackConflictWorkspaceChecksOutExactDirectoryNames(t *testing.T) {
	t.Parallel()
	f := shipGTRepo(t)
	stubStackPRs(t, f, nil)
	writeShipFile(t, f.Dir, " padded /c.txt", "base\n")
	writeShipFile(t, f.Dir, " padded /sibling.txt", "sibling\n")
	writeShipFile(t, f.Dir, "padded/decoy.txt", "decoy\n")
	sparseCommit(t, f, "layout")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "main")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "feature")
	writeShipFile(t, f.Dir, " padded /c.txt", "feature\n")
	sparseCommit(t, f, "feature")
	mustRun(t, f.Env(), f.Dir, "gt", "track", "-f", "--no-interactive")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "main")
	writeShipFile(t, f.Dir, " padded /c.txt", "trunk\n")
	sparseCommit(t, f, "trunk")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "main")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "feature")

	_, _, err := runStackCmd(t, f, "rebase", "--no-push")
	if err == nil {
		t.Fatal("stack rebase succeeded, want the conflict on feature")
	}
	if got := sparseConflicted(err); !slices.Equal(got, []string{" padded /c.txt"}) {
		t.Errorf("conflicted files = %q, want [\" padded /c.txt\"]", got)
	}
	ws := stackWorkspaceOf(t, err)
	if _, err := os.Stat(filepath.Join(ws, " padded ", "sibling.txt")); err != nil {
		t.Errorf("the conflicted directory's sibling is not checked out: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws, "padded")); !os.IsNotExist(err) {
		t.Errorf("padded/, which only trims to the conflicted directory's name, is checked out: %v", err)
	}
}

func sparseConflictRepo(t *testing.T) *vcstest.Fixture {
	t.Helper()
	f := shipGTRepo(t)
	stubStackPRs(t, f, nil)
	writeShipFile(t, f.Dir, "top.txt", "top\n")
	writeShipFile(t, f.Dir, "other/x.txt", "x\n")
	writeShipFile(t, f.Dir, "conflict/deep/c.txt", "base\n")
	sparseCommit(t, f, "layout")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "main")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "feature")
	writeShipFile(t, f.Dir, "conflict/deep/c.txt", "feature\n")
	writeShipFile(t, f.Dir, "other/y.txt", "y\n")
	sparseCommit(t, f, "feature")
	mustRun(t, f.Env(), f.Dir, "gt", "track", "-f", "--no-interactive")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "main")
	writeShipFile(t, f.Dir, "conflict/deep/c.txt", "trunk\n")
	writeShipFile(t, f.Dir, "other/x.txt", "x trunk\n")
	sparseCommit(t, f, "trunk")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "main")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "feature")
	return f
}

func sparseCommit(t *testing.T, f *vcstest.Fixture, message string) {
	t.Helper()
	mustRun(t, f.Env(), f.Dir, "git", "add", "-A")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", message)
}

func sparseConflicted(err error) []string {
	_, rest, _ := strings.Cut(err.Error(), "conflicted files:\n")
	var paths []string
	for line := range strings.Lines(rest) {
		p, ok := strings.CutPrefix(strings.TrimSuffix(line, "\n"), "  ")
		if !ok {
			break
		}
		paths = append(paths, p)
	}
	return paths
}

func sparseSettleIndex(t *testing.T, f *vcstest.Fixture) {
	t.Helper()
	root, err := os.OpenRoot(f.Dir)
	if err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour)
	err = fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir() && d.Name() == ".git":
			return fs.SkipDir
		}
		return root.Chtimes(p, past, past)
	})
	if err = errors.Join(err, root.Close()); err != nil {
		t.Fatal(err)
	}
	mustRun(t, f.Env(), f.Dir, "git", "update-index", "-q", "--refresh")
}

func sparseSourceState(t *testing.T, f *vcstest.Fixture) map[string]string {
	t.Helper()
	read := func(name string) string {
		raw, err := os.ReadFile(filepath.Join(f.Dir, ".git", name))
		if os.IsNotExist(err) {
			return "<absent>"
		}
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	var config []string
	for line := range strings.Lines(gitAt(t, f.Env(), f.Dir, "config", "--list", "--show-origin")) {
		if !strings.Contains(line, "extensions.worktreeconfig=") {
			config = append(config, line)
		}
	}
	var files []string
	err := filepath.WalkDir(f.Dir, func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir() && d.Name() == ".git":
			return filepath.SkipDir
		case d.IsDir():
			return nil
		}
		rel, err := filepath.Rel(f.Dir, p)
		files = append(files, rel)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return map[string]string{
		"index":           read("index"),
		"config":          strings.Join(config, ""),
		"config.worktree": read("config.worktree"),
		"sparse-checkout": read(path.Join("info", "sparse-checkout")),
		"files":           strings.Join(files, "\n"),
		"common":          read("config"),
	}
}

func sparseAdded(before, after string) ([]string, bool) {
	old := strings.Split(strings.TrimRight(before, "\n"), "\n")
	var added []string
	i := 0
	for _, line := range strings.Split(strings.TrimRight(after, "\n"), "\n") {
		if i < len(old) && line == old[i] {
			i++
			continue
		}
		added = append(added, line)
	}
	return added, i == len(old)
}

func sparseAssertWorkspace(t *testing.T, f *vcstest.Fixture, ws string, kept bool) {
	t.Helper()
	_, err := os.Stat(ws)
	if onDisk := err == nil; onDisk != kept {
		t.Errorf("workspace %s on disk: %v, want %v", ws, onDisk, kept)
	}
	listed := gitAt(t, f.Env(), f.Dir, "worktree", "list", "--porcelain")
	if registered := slices.Contains(strings.Split(listed, "\n"), "worktree "+ws); registered != kept {
		t.Errorf("workspace %s registered: %v, want %v\n%s", ws, registered, kept, listed)
	}
}
