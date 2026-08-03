package vcstest

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
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

	// gt 1.8.6's settled probes. Its detached cache refresher then re-runs an
	// environment-dependent subset — a warm dev machine sees more repeats than
	// CI does — so requiring an exact multiset would pin a background race
	// inside a third-party tool. The claim under test is that gt's children
	// land one depth deeper than gt itself, which the presence of these and the
	// absence of any non-git row establish.
	want := [][]string{
		{"git", "rev-parse", "--path-format=absolute", "--show-toplevel", "--git-common-dir", "--git-dir"},
		{"git", "--version"},
		{"git", "for-each-ref", "--format=%(refname):%(objectname)", "refs/branch-metadata/"},
		{"git", "for-each-ref", "--format=%(refname:short):%(objectname)", "refs/heads/"},
		{"git", "branch", "--show-current"},
		{"git", "remote", "get-url", "origin", "--push"},
		{"git", "config", "--get", "user.email"},
	}
	kids := InvocationsAtDepth(t, f.ArgvLog, 1)
	if len(kids) == 0 {
		t.Fatal("no depth-1 invocations — gt's children never re-entered the shim")
	}
	seen := map[string]bool{}
	for _, inv := range kids {
		if inv[0] != "git" {
			t.Errorf("depth-1 invocation %v, want only git children of gt", inv)
		}
		seen[strings.Join(inv, " ")] = true
	}
	for _, w := range want {
		if key := strings.Join(w, " "); !seen[key] {
			t.Errorf("depth-1 missing gt's settled probe %q, got %q", key, slices.Sorted(maps.Keys(seen)))
		}
	}
}

// fakeScriptTool writes a tool whose shebang names its interpreter by name —
// the shape npm's gt has, `#!/usr/bin/env node` — with the interpreter in a
// directory of its own, and puts both on PATH. It returns the tool's path.
func fakeScriptTool(t *testing.T) string {
	t.Helper()
	base := realTempDir(t)
	interpDir := filepath.Join(base, "interp")
	mkdir(t, interpDir)
	writeExec(t, filepath.Join(interpDir, "ccxfakenode"), "#!/bin/sh\nshift\necho \"ran $*\"\n")

	toolDir := filepath.Join(base, "tool")
	mkdir(t, toolDir)
	tool := filepath.Join(toolDir, "ccxfaketool")
	writeExec(t, tool, "#!/usr/bin/env ccxfakenode\n")

	t.Setenv("PATH", toolPATH(toolDir, interpDir))
	return tool
}

func writeExec(t *testing.T, path, script string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // the fake tool must be owner-executable to run from PATH
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestScriptToolNeedsItsInterpreterOnPATH(t *testing.T) {
	tool := fakeScriptTool(t)

	// The condition CI hit: the brew-free PATH holds no node, so the
	// shebang's env lookup fails and the exec returns 127 before the tool
	// runs a line.
	bare := exec.Command(tool) //nolint:gosec // the test wrote tool itself
	bare.Env = []string{"PATH=" + strings.Join(systemPATH, string(os.PathListSeparator))}
	var ee *exec.ExitError
	if err := bare.Run(); !errors.As(err, &ee) || ee.ExitCode() != 127 {
		t.Fatalf("run under a bare system PATH = %v, want exit 127", err)
	}

	_, log := Shim(t, filepath.Base(tool))
	if got := strings.TrimSpace(out(t, t.TempDir(), filepath.Base(tool), "--version")); got != "ran --version" {
		t.Errorf("shimmed run = %q, want %q", got, "ran --version")
	}
	got := Invocations(t, log)
	if want := [][]string{{filepath.Base(tool), "--version"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("Invocations() = %v, want %v", got, want)
	}
}

func TestIsolateEnvReachesScriptInterpreter(t *testing.T) {
	tool := fakeScriptTool(t)
	isolateEnv(t, realTempDir(t), resolveTools(t, []string{filepath.Base(tool)}))

	// Fixture construction predates the shim and runs each tool by absolute
	// path, but already under isolateEnv's PATH — the lane gt init takes.
	if got := strings.TrimSpace(run(t, t.TempDir(), tool, "--version")); got != "ran --version" {
		t.Errorf("run under isolateEnv's PATH = %q, want %q", got, "ran --version")
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

// mustNotSkip runs body as a subtest and fails when it skips: a skip reads as
// a pass in the run summary, so a fixture that silently skipped would report
// success having verified nothing.
func mustNotSkip(t *testing.T, name string, body func(t *testing.T)) {
	t.Helper()
	var skipped bool
	t.Run(name, func(t *testing.T) {
		t.Cleanup(func() { skipped = t.Skipped() })
		body(t)
	})
	if skipped {
		t.Errorf("subtest %s skipped, want it to run", name)
	}
}

// requireHostTool skips when tool really is absent from the host PATH, so the
// mustNotSkip assertions below only fire on a shim that lost a tool it had.
func requireHostTool(t *testing.T, tool string) {
	t.Helper()
	if _, err := exec.LookPath(tool); err != nil {
		t.Skipf("%s not installed on the host", tool)
	}
}

func TestSecondFixtureResolvesToolTheFirstDidNot(t *testing.T) {
	requireHostTool(t, "jj")
	mustNotSkip(t, "jj after git", func(t *testing.T) {
		Repo(t)
		f := Repo(t, JJ())
		if v := out(t, f.Dir, "jj", "--version"); !strings.HasPrefix(v, "jj ") {
			t.Errorf("jj --version = %q, want a jj version line", v)
		}
	})
}

func TestSecondFixtureKeepsFirstFixtureTools(t *testing.T) {
	requireHostTool(t, "jj")
	mustNotSkip(t, "git after jj", func(t *testing.T) {
		Repo(t, JJ())
		f := Repo(t, BrokenGitDir())
		if v := out(t, f.Dir, "jj", "--version"); !strings.HasPrefix(v, "jj ") {
			t.Errorf("jj --version = %q, want a jj version line", v)
		}
		got := Invocations(t, f.ArgvLog)
		if want := [][]string{{"jj", "--version"}}; !reflect.DeepEqual(got, want) {
			t.Errorf("Invocations() = %v, want %v — jj bypassed the second fixture's shim", got, want)
		}
	})
}

func TestSecondFixtureShimLeadsABrewFreePATHOverBothToolsets(t *testing.T) {
	requireHostTool(t, "jj")
	mustNotSkip(t, "shim after two fixtures", func(t *testing.T) {
		Repo(t, JJ())
		f := Repo(t)

		for _, tool := range []string{"git", "jj"} {
			resolved, err := exec.LookPath(tool)
			if err != nil {
				t.Fatalf("LookPath(%s): %v", tool, err)
			}
			if want := filepath.Join(f.ShimBin, tool); resolved != want {
				t.Errorf("LookPath(%s) = %q, want the second fixture's shim at %q", tool, resolved, want)
			}
		}
		path := os.Getenv("PATH")
		if strings.Contains(path, "homebrew") || strings.Contains(path, "Homebrew") {
			t.Errorf("PATH = %q, want no brew dir", path)
		}
		if want := toolPATH(f.ShimBin); path != want {
			t.Errorf("PATH = %q, want %q", path, want)
		}
	})
}

func TestSecondFixtureShimWrapsTheRealBinary(t *testing.T) {
	f1 := Repo(t)
	f2 := Repo(t)

	script, err := os.ReadFile(filepath.Join(f2.ShimBin, "git"))
	if err != nil {
		t.Fatalf("read the second fixture's shim: %v", err)
	}
	if strings.Contains(string(script), f1.ShimBin) {
		t.Errorf("the second fixture's shim execs the first fixture's shim:\n%s", script)
	}
	if got := Invocations(t, f1.ArgvLog); got != nil {
		t.Errorf("the second fixture's construction leaked into the first fixture's log: %v", got)
	}

	out(t, f2.Dir, "git", "--version")
	got := Invocations(t, f2.ArgvLog)
	if want := [][]string{{"git", "--version"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("second fixture invocations = %v, want %v", got, want)
	}
	if got := Invocations(t, f1.ArgvLog); got != nil {
		t.Errorf("the first fixture's log kept recording after the second took PATH: %v", got)
	}
}

// TestLinkPATHNarrowsToItsOwnTools pins the contract that separates LinkPATH
// from the shim: the shim accumulates so a second fixture keeps the first's
// tools, but LinkPATH links only what it was asked for. Callers use it to take
// a tool AWAY — to prove ccx refuses without it — which accumulating would
// silently defeat.
func TestLinkPATHNarrowsToItsOwnTools(t *testing.T) {
	requireHostTool(t, "gt")
	mustNotSkip(t, "gt dropped after a gt fixture", func(t *testing.T) {
		Repo(t, GT())
		if _, err := exec.LookPath("gt"); err != nil {
			t.Fatalf("gt unreachable before LinkPATH: %v", err)
		}
		LinkPATH(t, "git")
		if _, err := exec.LookPath("gt"); err == nil {
			t.Error("gt still on PATH after LinkPATH(git), want it dropped")
		}
		if _, err := exec.LookPath("git"); err != nil {
			t.Errorf("git unreachable after LinkPATH(git): %v", err)
		}
	})
}

func TestAbsentToolSkips(t *testing.T) {
	var skipped bool
	t.Run("absent", func(t *testing.T) {
		t.Cleanup(func() { skipped = t.Skipped() })
		resolveTools(t, []string{"ccx-no-such-binary-9f3a"})
		t.Error("resolveTools returned for a tool that cannot exist, want a skip")
	})
	if !skipped {
		t.Error("resolveTools did not skip for a tool that cannot exist")
	}
}

func TestLookPath(t *testing.T) {
	base := realTempDir(t)
	binDir := filepath.Join(base, "bin")
	mkdir(t, binDir)
	tool := filepath.Join(binDir, "ccxlookme")
	writeExec(t, tool, "#!/bin/sh\nexit 0\n")
	plain := filepath.Join(binDir, "ccxplain")
	writeFile(t, plain, "not executable\n")
	mkdir(t, filepath.Join(binDir, "ccxdir"))
	searchPATH := strings.Join([]string{filepath.Join(base, "absent"), binDir}, string(os.PathListSeparator))

	tests := []struct {
		name    string
		lookup  string
		want    string
		wantErr bool
	}{
		{"bare name on the path", "ccxlookme", tool, false},
		{"bare name absent", "ccxnothere", "", true},
		{"bare name naming a non-executable", "ccxplain", "", true},
		{"bare name naming a directory", "ccxdir", "", true},
		{"path with a separator resolves to itself", tool, tool, false},
		{"path with a separator to a non-executable", plain, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := lookPath(searchPATH, tt.lookup)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("lookPath(%q) = %q, want an error", tt.lookup, got)
				}
				if !errors.Is(err, exec.ErrNotFound) {
					t.Errorf("lookPath(%q) error = %v, want exec.ErrNotFound", tt.lookup, err)
				}
				if !strings.Contains(err.Error(), tt.lookup) {
					t.Errorf("lookPath(%q) error = %v, want it to name the tool", tt.lookup, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("lookPath(%q): %v", tt.lookup, err)
			}
			if got != tt.want {
				t.Errorf("lookPath(%q) = %q, want %q", tt.lookup, got, tt.want)
			}
		})
	}
}

func TestLookPathIgnoresAnEmptyPathElement(t *testing.T) {
	dir := realTempDir(t)
	writeExec(t, filepath.Join(dir, "ccxlookme"), "#!/bin/sh\nexit 0\n")
	t.Chdir(dir)
	if got, err := lookPath(string(os.PathListSeparator), "ccxlookme"); err == nil {
		t.Errorf("lookPath on an empty element = %q, want an error rather than the working directory", got)
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
