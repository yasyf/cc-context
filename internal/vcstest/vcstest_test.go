package vcstest

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// out runs name via PATH (through the shim) in dir, failing on nonzero exit.
func out(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...) //nolint:gosec // resolves through the shim on the test's own PATH; args are test-authored
	cmd.Dir = dir
	stdout, err := cmd.Output()
	if err != nil {
		stderr := ""
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("%s %v: %v\n%s", name, args, err, stderr)
	}
	return string(stdout)
}

// exitCode runs name via PATH in dir and returns its exit code and stderr,
// failing on any non-exit error.
func exitCode(t *testing.T, dir, name string, args ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(name, args...) //nolint:gosec // resolves through the shim on the test's own PATH; args are test-authored
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return 0, ""
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("%s %v: %v", name, args, err)
	}
	return ee.ExitCode(), stderr.String()
}

func TestShimEmptyArgvRoundTrip(t *testing.T) {
	_, log := Shim(t, "git")
	cmd := exec.Command("git", "", "a|b *", "--version")
	cmd.Dir = t.TempDir()
	_ = cmd.Run()

	got := Invocations(t, log)
	want := [][]string{{"git", "", "a|b *", "--version"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Invocations() = %v, want %v", got, want)
	}
}

func TestShimPassthrough(t *testing.T) {
	f := Repo(t)

	sha := strings.TrimSpace(out(t, f.Dir, "git", "rev-parse", "HEAD"))
	if len(sha) != 40 {
		t.Fatalf("rev-parse HEAD = %q, want a 40-hex sha", sha)
	}

	if code, _ := exitCode(t, f.Dir, "git", "rev-parse", "--verify", "-q", "refs/heads/nope"); code != 1 {
		t.Errorf("rev-parse --verify -q missing ref exit = %d, want 1", code)
	}
	code, stderr := exitCode(t, f.Dir, "git", "--git-dir=/nonexistent-repo", "rev-parse", "--git-dir")
	if code != 128 {
		t.Errorf("rev-parse in broken git dir exit = %d, want 128", code)
	}
	if !strings.Contains(stderr, "fatal:") {
		t.Errorf("broken git dir stderr = %q, want a fatal: line", stderr)
	}

	blob := strings.TrimSpace(out(t, f.Dir, "git", "rev-parse", "HEAD:f.txt"))
	hash := exec.Command("git", "hash-object", "--stdin")
	hash.Dir = f.Dir
	hash.Stdin = strings.NewReader("base\n")
	var stdout, errBuf bytes.Buffer
	hash.Stdout = &stdout
	hash.Stderr = &errBuf
	if err := hash.Run(); err != nil {
		t.Fatalf("hash-object --stdin: %v\n%s", err, errBuf.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != blob {
		t.Errorf("hash-object --stdin = %q, want %q (HEAD:f.txt)", got, blob)
	}
	if errBuf.Len() != 0 {
		t.Errorf("hash-object stderr = %q, want empty", errBuf.String())
	}
}

func TestShimUnderDash(t *testing.T) {
	dash, err := exec.LookPath("dash")
	if err != nil {
		t.Skip("dash not installed")
	}
	bin, log := Shim(t, "git")
	cmd := exec.Command(dash, filepath.Join(bin, "git"), "--version", "") //nolint:gosec // dash is LookPath-resolved and the script path is the test's own shim
	cmd.Dir = t.TempDir()
	_ = cmd.Run()

	got := Invocations(t, log)
	want := [][]string{{"git", "--version", ""}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Invocations() = %v, want %v", got, want)
	}
}

func TestShimDepthSeparatesGTChildren(t *testing.T) {
	f := Repo(t, GT())
	if got := Invocations(t, f.ArgvLog); got != nil {
		t.Fatalf("fixture construction leaked into the argv log: %v", got)
	}

	state := out(t, f.Dir, "gt", "state")
	if !strings.Contains(state, `"trunk": true`) {
		t.Fatalf("gt state = %q, want a trunk entry", state)
	}
	Quiesce(t, f.ArgvLog)

	top := Invocations(t, f.ArgvLog)
	if want := [][]string{{"gt", "state"}}; !reflect.DeepEqual(top, want) {
		t.Errorf("depth-0 invocations = %v, want %v", top, want)
	}

	// gt 1.8.6's settled spawn set: seven probes, then its detached cache
	// refresher re-runs four of them — spawn order races, so the assertion
	// sorts. Measured stable at 11 across repeated quiesced runs.
	want := [][]string{
		{"git", "rev-parse", "--path-format=absolute", "--show-toplevel", "--git-common-dir", "--git-dir"},
		{"git", "--version"},
		{"git", "for-each-ref", "--format=%(refname):%(objectname)", "refs/branch-metadata/"},
		{"git", "for-each-ref", "--format=%(refname:short):%(objectname)", "refs/heads/"},
		{"git", "branch", "--show-current"},
		{"git", "remote", "get-url", "origin", "--push"},
		{"git", "config", "--get", "user.email"},
		{"git", "rev-parse", "--path-format=absolute", "--show-toplevel", "--git-common-dir", "--git-dir"},
		{"git", "--version"},
		{"git", "remote", "get-url", "origin", "--push"},
		{"git", "remote", "get-url", "origin", "--push"},
	}
	kids := InvocationsAtDepth(t, f.ArgvLog, 1)
	sortRows := func(rows [][]string) []string {
		keys := make([]string, len(rows))
		for i, inv := range rows {
			keys[i] = strings.Join(inv, " ")
		}
		slices.Sort(keys)
		return keys
	}
	if got, wantSorted := sortRows(kids), sortRows(want); !reflect.DeepEqual(got, wantSorted) {
		t.Errorf("depth-1 invocations (sorted):\n got: %q\nwant: %q", got, wantSorted)
	}
}

func TestInvocationsConcurrentAppend(t *testing.T) {
	_, log := Shim(t, "git")
	dir := t.TempDir()

	var wg sync.WaitGroup
	for i := range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command("git", "rev-parse", fmt.Sprintf("marker-%03d", i)) //nolint:gosec // fixed argv; only the loop index varies
			cmd.Dir = dir
			_ = cmd.Run()
		}()
	}
	wg.Wait()

	got := Invocations(t, log)
	if len(got) != 200 {
		t.Fatalf("Invocations() = %d rows, want 200", len(got))
	}
	seen := map[string]bool{}
	for _, inv := range got {
		if len(inv) != 3 || inv[0] != "git" || inv[1] != "rev-parse" {
			t.Fatalf("malformed record %v", inv)
		}
		seen[inv[2]] = true
	}
	for i := range 200 {
		if marker := fmt.Sprintf("marker-%03d", i); !seen[marker] {
			t.Errorf("record %s missing", marker)
		}
	}
}

func TestRepoShimLeadsBrewFreePATH(t *testing.T) {
	f := Repo(t)
	resolved, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("LookPath(git): %v", err)
	}
	if want := filepath.Join(f.ShimBin, "git"); resolved != want {
		t.Errorf("LookPath(git) = %q, want the shim at %q", resolved, want)
	}
	if path := os.Getenv("PATH"); strings.Contains(path, "homebrew") || strings.Contains(path, "Homebrew") {
		t.Errorf("PATH = %q, want no brew dir", path)
	}
}

func TestRepoStates(t *testing.T) {
	tests := []struct {
		name  string
		opts  []Opt
		check func(t *testing.T, f *Fixture)
	}{
		{
			name: "default",
			check: func(t *testing.T, f *Fixture) {
				if branch := strings.TrimSpace(out(t, f.Dir, "git", "branch", "--show-current")); branch != "main" {
					t.Errorf("branch = %q, want main", branch)
				}
				if count := strings.TrimSpace(out(t, f.Dir, "git", "rev-list", "--count", "HEAD")); count != "1" {
					t.Errorf("commit count = %s, want 1", count)
				}
				if status := out(t, f.Dir, "git", "status", "--porcelain"); status != "" {
					t.Errorf("status = %q, want clean", status)
				}
				if remotes := out(t, f.Dir, "git", "remote"); remotes != "" {
					t.Errorf("remotes = %q, want none", remotes)
				}
			},
		},
		{
			name: "master trunk",
			opts: []Opt{Trunk("master")},
			check: func(t *testing.T, f *Fixture) {
				if branch := strings.TrimSpace(out(t, f.Dir, "git", "branch", "--show-current")); branch != "master" {
					t.Errorf("branch = %q, want master", branch)
				}
			},
		},
		{
			name: "branch",
			opts: []Opt{Branch("feat")},
			check: func(t *testing.T, f *Fixture) {
				if branch := strings.TrimSpace(out(t, f.Dir, "git", "branch", "--show-current")); branch != "feat" {
					t.Errorf("branch = %q, want feat", branch)
				}
				out(t, f.Dir, "git", "rev-parse", "--verify", "-q", "refs/heads/main")
			},
		},
		{
			name: "remote",
			opts: []Opt{Remote()},
			check: func(t *testing.T, f *Fixture) {
				if f.RemoteDir == "" {
					t.Fatal("RemoteDir empty")
				}
				if bare := strings.TrimSpace(out(t, f.RemoteDir, "git", "rev-parse", "--is-bare-repository")); bare != "true" {
					t.Errorf("origin bare = %s, want true", bare)
				}
				if count := strings.TrimSpace(out(t, f.RemoteDir, "git", "rev-list", "--count", "main")); count != "1" {
					t.Errorf("origin commit count = %s, want 1", count)
				}
				out(t, f.Dir, "git", "rev-parse", "--verify", "-q", "refs/remotes/origin/main")
				if head := strings.TrimSpace(out(t, f.Dir, "git", "symbolic-ref", "refs/remotes/origin/HEAD")); head != "refs/remotes/origin/main" {
					t.Errorf("origin/HEAD = %q, want refs/remotes/origin/main", head)
				}
			},
		},
		{
			name: "no origin head",
			opts: []Opt{Remote(), NoOriginHead()},
			check: func(t *testing.T, f *Fixture) {
				out(t, f.Dir, "git", "rev-parse", "--verify", "-q", "refs/remotes/origin/main")
				if code, _ := exitCode(t, f.Dir, "git", "symbolic-ref", "-q", "refs/remotes/origin/HEAD"); code != 1 {
					t.Errorf("symbolic-ref origin/HEAD exit = %d, want 1", code)
				}
			},
		},
		{
			name: "master trunk without origin head",
			opts: []Opt{Trunk("master"), Remote(), NoOriginHead()},
			check: func(t *testing.T, f *Fixture) {
				out(t, f.Dir, "git", "rev-parse", "--verify", "-q", "refs/remotes/origin/master")
				if code, _ := exitCode(t, f.Dir, "git", "symbolic-ref", "-q", "refs/remotes/origin/HEAD"); code != 1 {
					t.Errorf("symbolic-ref origin/HEAD exit = %d, want 1", code)
				}
			},
		},
		{
			name: "detached",
			opts: []Opt{Detached()},
			check: func(t *testing.T, f *Fixture) {
				out(t, f.Dir, "git", "rev-parse", "HEAD")
				if code, _ := exitCode(t, f.Dir, "git", "symbolic-ref", "-q", "HEAD"); code != 1 {
					t.Errorf("symbolic-ref HEAD exit = %d, want 1 (detached)", code)
				}
			},
		},
		{
			name: "dirty",
			opts: []Opt{Dirty()},
			check: func(t *testing.T, f *Fixture) {
				if status := out(t, f.Dir, "git", "status", "--porcelain"); status != " M f.txt\n" {
					t.Errorf("status = %q, want %q", status, " M f.txt\n")
				}
			},
		},
		{
			name: "staged",
			opts: []Opt{Staged()},
			check: func(t *testing.T, f *Fixture) {
				if status := out(t, f.Dir, "git", "status", "--porcelain"); status != "M  f.txt\n" {
					t.Errorf("status = %q, want %q", status, "M  f.txt\n")
				}
			},
		},
		{
			name: "conflicted",
			opts: []Opt{Conflicted()},
			check: func(t *testing.T, f *Fixture) {
				if unmerged := out(t, f.Dir, "git", "ls-files", "-u"); unmerged == "" {
					t.Error("ls-files -u empty, want unmerged f.txt entries")
				}
				if status := out(t, f.Dir, "git", "status", "--porcelain"); !strings.Contains(status, "UU f.txt") {
					t.Errorf("status = %q, want UU f.txt", status)
				}
			},
		},
		{
			name: "jj",
			opts: []Opt{JJ()},
			check: func(t *testing.T, f *Fixture) {
				for _, marker := range []string{".git", ".jj"} {
					if _, err := os.Stat(filepath.Join(f.Dir, marker)); err != nil {
						t.Errorf("stat %s: %v", marker, err)
					}
				}
				if desc := strings.TrimSpace(out(t, f.Dir, "jj", "log", "-r", "@-", "--no-graph", "-T", "description")); desc != "init" {
					t.Errorf("@- description = %q, want init", desc)
				}
				if bookmarks := out(t, f.Dir, "jj", "bookmark", "list", "-T", `name ++ "\n"`); !strings.Contains(bookmarks, "main") {
					t.Errorf("bookmarks = %q, want main", bookmarks)
				}
			},
		},
		{
			name: "jj remote",
			opts: []Opt{JJ(), Remote()},
			check: func(t *testing.T, f *Fixture) {
				if desc := strings.TrimSpace(out(t, f.Dir, "jj", "log", "-r", "trunk()", "--no-graph", "-T", "description")); desc != "init" {
					t.Errorf("trunk() description = %q, want init", desc)
				}
				if count := strings.TrimSpace(out(t, f.RemoteDir, "git", "rev-list", "--count", "main")); count != "1" {
					t.Errorf("origin commit count = %s, want 1", count)
				}
				out(t, f.Dir, "git", "rev-parse", "--verify", "-q", "refs/remotes/origin/main")
			},
		},
		{
			name: "jj conflicted",
			opts: []Opt{JJ(), Conflicted()},
			check: func(t *testing.T, f *Fixture) {
				if got := out(t, f.Dir, "jj", "log", "-r", "@", "--no-graph", "-T", `if(conflict, "conflicted", "clean")`); got != "conflicted" {
					t.Errorf("@ conflict state = %q, want conflicted", got)
				}
			},
		},
		{
			name: "conflicted bookmark",
			opts: []Opt{JJ(), ConflictedBookmark()},
			check: func(t *testing.T, f *Fixture) {
				got := out(t, f.Dir, "jj", "bookmark", "list", "-T", `name ++ if(conflict, " conflicted") ++ "\n"`)
				if !strings.Contains(got, "feat conflicted") {
					t.Errorf("bookmark list = %q, want feat conflicted", got)
				}
			},
		},
		{
			name: "worktree",
			opts: []Opt{Worktree("feat")},
			check: func(t *testing.T, f *Fixture) {
				porcelain := out(t, f.Dir, "git", "worktree", "list", "--porcelain")
				if !strings.Contains(porcelain, "worktree "+f.WorktreePath("feat")+"\n") {
					t.Errorf("worktree list = %q, want %s", porcelain, f.WorktreePath("feat"))
				}
				if !strings.Contains(porcelain, "branch refs/heads/feat\n") {
					t.Errorf("worktree list = %q, want branch refs/heads/feat", porcelain)
				}
			},
		},
		{
			name: "prunable worktree",
			opts: []Opt{PrunableWorktree()},
			check: func(t *testing.T, f *Fixture) {
				if porcelain := out(t, f.Dir, "git", "worktree", "list", "--porcelain"); !strings.Contains(porcelain, "\nprunable") {
					t.Errorf("worktree list = %q, want a prunable entry", porcelain)
				}
			},
		},
		{
			name: "locked worktree",
			opts: []Opt{LockedWorktree()},
			check: func(t *testing.T, f *Fixture) {
				if porcelain := out(t, f.Dir, "git", "worktree", "list", "--porcelain"); !strings.Contains(porcelain, "\nlocked") {
					t.Errorf("worktree list = %q, want a locked entry", porcelain)
				}
			},
		},
		{
			name: "index lock",
			opts: []Opt{IndexLock()},
			check: func(t *testing.T, f *Fixture) {
				if _, err := os.Stat(filepath.Join(f.Dir, ".git", "index.lock")); err != nil {
					t.Fatalf("stat index.lock: %v", err)
				}
				code, stderr := exitCode(t, f.Dir, "git", "add", "f.txt")
				if code != 128 || !strings.Contains(stderr, "index.lock") {
					t.Errorf("git add under held lock = exit %d, stderr %q; want 128 naming index.lock", code, stderr)
				}
			},
		},
		{
			name: "broken git dir",
			opts: []Opt{BrokenGitDir()},
			check: func(t *testing.T, f *Fixture) {
				if code, _ := exitCode(t, f.Dir, "git", "rev-parse", "--git-dir"); code != 128 {
					t.Errorf("rev-parse exit = %d, want 128", code)
				}
			},
		},
		{
			name: "gt",
			opts: []Opt{GT()},
			check: func(t *testing.T, f *Fixture) {
				if _, err := os.Stat(filepath.Join(f.Dir, ".git", ".graphite_repo_config")); err != nil {
					t.Fatalf("stat .graphite_repo_config: %v", err)
				}
				if state := out(t, f.Dir, "gt", "state"); !strings.Contains(state, `"trunk": true`) {
					t.Errorf("gt state = %q, want a trunk entry", state)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start := time.Now()
			f := Repo(t, tt.opts...)
			t.Logf("fixture %s built in %s", tt.name, time.Since(start))
			tt.check(t, f)
		})
	}
}
