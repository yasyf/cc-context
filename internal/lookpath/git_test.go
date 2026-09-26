package lookpath

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/execstub"
)

func fakeGit(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "git")
	execstub.Write(t, path, "#!/bin/sh\n")
	return path
}

func setStubDirs(t *testing.T, dirs ...string) {
	t.Helper()
	prev := stubDirs
	stubDirs = dirs
	t.Cleanup(func() { stubDirs = prev })
}

// envFor is the environment a child would carry with dirs as its PATH. A
// resolution reads it directly, so no test here replaces the process's own.
func envFor(dirs ...string) Env {
	return Env{PATH: strings.Join(dirs, string(os.PathListSeparator))}
}

func TestBinPassesThroughEveryNameButGit(t *testing.T) {
	if got := (Env{}).Bin("gh"); got != "gh" {
		t.Fatalf("Bin(gh) = %q, want gh", got)
	}
}

func TestBinPrefersAGitOutsideTheStubDirs(t *testing.T) {
	stub, fast := t.TempDir(), t.TempDir()
	fakeGit(t, stub)
	want := fakeGit(t, fast)
	setStubDirs(t, stub)

	if got := envFor(stub, fast).Bin("git"); got != want {
		t.Fatalf("Bin(git) = %q, want %q", got, want)
	}
}

func TestBinKeepsExecsOwnAnswerWhenItIsNotAStub(t *testing.T) {
	first, later := t.TempDir(), t.TempDir()
	want := fakeGit(t, first)
	fakeGit(t, later)
	setStubDirs(t)

	if got := envFor(first, later).Bin("git"); got != want {
		t.Fatalf("Bin(git) = %q, want %q", got, want)
	}
}

func TestBinFallsBackWhenOnlyTheStubIsReachable(t *testing.T) {
	stub := t.TempDir()
	want := fakeGit(t, stub)
	setStubDirs(t, stub)

	if got := envFor(stub).Bin("git"); got != want {
		t.Fatalf("Bin(git) = %q, want the stub %q", got, want)
	}
}

func TestBinHonoursTheEnvOverride(t *testing.T) {
	fast := t.TempDir()
	fakeGit(t, fast)
	setStubDirs(t)
	env := envFor(fast)
	env.Git = "/elsewhere/git"

	if got := env.Bin("git"); got != "/elsewhere/git" {
		t.Fatalf("Bin(git) = %q, want the %s override", got, GitEnv)
	}
}

func TestGitPATHLeadsWithTheResolvedGitsDirectory(t *testing.T) {
	stub, fast := t.TempDir(), t.TempDir()
	fakeGit(t, stub)
	fakeGit(t, fast)
	setStubDirs(t, stub)

	want := strings.Join([]string{fast, stub, fast}, string(os.PathListSeparator))
	if got := envFor(stub, fast).GitPATH(); got != want {
		t.Fatalf("GitPATH() = %q, want %q", got, want)
	}
}

func TestGitPATHLeavesTheStubWhereItIs(t *testing.T) {
	stub := t.TempDir()
	fakeGit(t, stub)
	setStubDirs(t, stub)

	if got := envFor(t.TempDir(), stub).GitPATH(); got != "" {
		t.Fatalf("GitPATH() = %q, want empty", got)
	}
}

func TestGitPATHIsEmptyWhenPATHAlreadyLeadsThere(t *testing.T) {
	fast := t.TempDir()
	fakeGit(t, fast)
	setStubDirs(t)

	if got := envFor(fast, t.TempDir()).GitPATH(); got != "" {
		t.Fatalf("GitPATH() = %q, want empty", got)
	}
}
