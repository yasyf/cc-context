package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/hunk"
)

// initCliGitRepo stands up a real, config-isolated git repo, chdirs into it, and
// returns its path, so the hunks listing runs against genuine `git show` output.
func initCliGitRepo(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("live git repo setup is POSIX-only here")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...) //nolint:gosec // fixed git argv; dir is a TempDir, args are literals
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "t@t.t")
	run("config", "user.name", "t")

	old, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	return dir
}

func runHunksCmd(t *testing.T, args ...string) string {
	t.Helper()
	cmd := newHunksCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("hunks error = %v", err)
	}
	return out.String()
}

func TestVcsHunksListing(t *testing.T) {
	const base = "a\nb\nc\nd\ne\n"
	const current = "A\nb\nc\nd\nE\n"
	dir := initCliGitRepo(t)
	commitFile(t, dir, "f.txt", base)
	if err := os.WriteFile("f.txt", []byte(current), 0o644); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write f.txt: %v", err)
	}

	hunks := hunk.Compute([]byte(base), []byte(current))
	if len(hunks) != 2 {
		t.Fatalf("fixture must produce 2 hunks, got %d", len(hunks))
	}
	want := []string{
		"f.txt:1#" + hunks[0].Digest.String() + "\t-1+1\tA",
		"f.txt:5#" + hunks[1].Digest.String() + "\t-1+1\tE",
	}

	tests := []struct {
		name string
		args []string
	}{
		{"enumerated (no paths)", nil},
		{"explicit path", []string{"f.txt"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := runHunksCmd(t, tt.args...)
			got := strings.Split(strings.TrimRight(out, "\n"), "\n")
			if len(got) != len(want) {
				t.Fatalf("got %d lines, want %d\noutput:\n%s", len(got), len(want), out)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Errorf("line %d = %q, want %q", i, got[i], want[i])
				}
			}
		})
	}
}

func TestVcsHunksNoChanges(t *testing.T) {
	dir := initCliGitRepo(t)
	commitFile(t, dir, "f.txt", "a\nb\nc\n")

	if out := runHunksCmd(t); out != "" {
		t.Errorf("clean repo must list no hunks, got %q", out)
	}
	if out := runHunksCmd(t, "f.txt"); out != "" {
		t.Errorf("unchanged file must list no hunks, got %q", out)
	}
}

// commitFile writes content to dir/name and commits it, so it has a HEAD base.
func commitFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write %s: %v", name, err)
	}
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-qm", "seed"}} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...) //nolint:gosec // fixed git argv; dir is a TempDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}
