package overview

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/backend"
)

func TestRunNoVCS(t *testing.T) {
	// A git runner that always errors: even if an ancestor of the temp dir is a repo,
	// the git/hot sections resolve to "" so the assertion holds regardless of Detect.
	prev := git
	t.Cleanup(func() { git = prev })
	git = func(_ context.Context, _ string, _ ...string) (string, error) {
		return "", fmt.Errorf("no git")
	}

	root := scaffold(t, map[string]string{
		"go.mod":               "module github.com/x/app\n\ngo 1.26.5\n\nrequire (\n\tgithub.com/a/b v1\n)\n",
		"cmd/app/main.go":      "package main\n",
		"internal/x/a.go":      "package x\n",
		"internal/x/a_test.go": "package x\n",
		"README.md":            "# app\n",
	})
	out, err := Run(context.Background(), backend.Args{Scope: root})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"# " + filepath.Base(root) + " — go module github.com/x/app (go 1.26)",
		"languages: go (3), md (1)",
		"dirs: internal/x · cmd/app",
		"entry: cmd/app/main.go",
		"manifests: go.mod (1 direct deps)",
		"tests: 1 test files (go)",
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("Run output missing %q; full output:\n%s", w, out)
		}
	}
	if strings.Contains(out, "git:") || strings.Contains(out, "hot (90d)") {
		t.Errorf("Run output has git/hot with erroring git runner:\n%s", out)
	}
	// Header is first, churn tail last: most-orienting-first ordering.
	if !strings.HasPrefix(out, "# ") {
		t.Errorf("output does not start with the header line:\n%s", out)
	}
}

// TestRunIncludesGitWhenGitAnswers proves the gate is "git answers", not a .git on
// disk: when the stubbed git resolves --git-dir, the git and hot sections appear.
func TestRunIncludesGitWhenGitAnswers(t *testing.T) {
	stubGit(t, map[string]string{
		"rev-parse --git-dir":                       ".git\n",
		"log -1 --format=%h%x00%s":                  "a1b2c3d\x00init\n",
		"rev-parse --abbrev-ref HEAD":               "main\n",
		"status --porcelain -z":                     "",
		"rev-list --count HEAD":                     "3\n",
		"log --since=90.days --name-only --format=": "internal/x/a.go\n",
	})
	root := scaffold(t, map[string]string{"internal/x/a.go": "package x\n"})
	out, err := Run(context.Background(), backend.Args{Scope: root})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `git: main @ a1b2c3d "init"`) {
		t.Errorf("git section missing when git answers:\n%s", out)
	}
	if !strings.Contains(out, "hot (90d): internal/x") {
		t.Errorf("hot section missing when git answers:\n%s", out)
	}
}

// TestLiveExample renders the overview for this repository and logs it, producing the
// verbatim example. Run with -v to see it.
func TestLiveExample(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	repoRoot := filepath.Clean(filepath.Join(cwd, "..", ".."))
	out, err := Run(context.Background(), backend.Args{Scope: repoRoot})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("live overview for %s:\n%s", repoRoot, out)
	if !strings.HasPrefix(out, "# ") {
		t.Errorf("live overview did not start with a header:\n%s", out)
	}
}
