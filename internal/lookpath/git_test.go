package lookpath

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fakeGit(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "git")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o700); err != nil { //nolint:gosec // a fake git must be executable to be resolved
		t.Fatalf("write fake git: %v", err)
	}
	return path
}

func setStubDirs(t *testing.T, dirs ...string) {
	t.Helper()
	prev := stubDirs
	stubDirs = dirs
	t.Cleanup(func() { stubDirs = prev })
}

func setPATH(t *testing.T, dirs ...string) {
	t.Helper()
	t.Setenv("PATH", strings.Join(dirs, string(os.PathListSeparator)))
}

func TestBinPassesThroughEveryNameButGit(t *testing.T) {
	if got := Bin("gh"); got != "gh" {
		t.Fatalf("Bin(gh) = %q, want gh", got)
	}
}

func TestBinPrefersAGitOutsideTheStubDirs(t *testing.T) {
	stub, fast := t.TempDir(), t.TempDir()
	fakeGit(t, stub)
	want := fakeGit(t, fast)
	setStubDirs(t, stub)
	setPATH(t, stub, fast)

	if got := Bin("git"); got != want {
		t.Fatalf("Bin(git) = %q, want %q", got, want)
	}
}

func TestBinKeepsExecsOwnAnswerWhenItIsNotAStub(t *testing.T) {
	first, later := t.TempDir(), t.TempDir()
	want := fakeGit(t, first)
	fakeGit(t, later)
	setStubDirs(t)
	setPATH(t, first, later)

	if got := Bin("git"); got != want {
		t.Fatalf("Bin(git) = %q, want %q", got, want)
	}
}

func TestBinFallsBackWhenOnlyTheStubIsReachable(t *testing.T) {
	stub := t.TempDir()
	want := fakeGit(t, stub)
	setStubDirs(t, stub)
	setPATH(t, stub)

	if got := Bin("git"); got != want {
		t.Fatalf("Bin(git) = %q, want the stub %q", got, want)
	}
}

func TestBinHonoursTheEnvOverride(t *testing.T) {
	fast := t.TempDir()
	fakeGit(t, fast)
	setStubDirs(t)
	setPATH(t, fast)
	t.Setenv(GitEnv, "/elsewhere/git")

	if got := Bin("git"); got != "/elsewhere/git" {
		t.Fatalf("Bin(git) = %q, want the %s override", got, GitEnv)
	}
}

func TestGitPATHLeadsWithTheResolvedGitsDirectory(t *testing.T) {
	stub, fast := t.TempDir(), t.TempDir()
	fakeGit(t, stub)
	fakeGit(t, fast)
	setStubDirs(t, stub)
	setPATH(t, stub, fast)

	want := strings.Join([]string{fast, stub, fast}, string(os.PathListSeparator))
	if got := GitPATH(); got != want {
		t.Fatalf("GitPATH() = %q, want %q", got, want)
	}
}

func TestGitPATHLeavesTheStubWhereItIs(t *testing.T) {
	stub := t.TempDir()
	fakeGit(t, stub)
	setStubDirs(t, stub)
	setPATH(t, t.TempDir(), stub)

	if got := GitPATH(); got != "" {
		t.Fatalf("GitPATH() = %q, want empty", got)
	}
}

func TestGitPATHIsEmptyWhenPATHAlreadyLeadsThere(t *testing.T) {
	fast := t.TempDir()
	fakeGit(t, fast)
	setStubDirs(t)
	setPATH(t, fast, t.TempDir())

	if got := GitPATH(); got != "" {
		t.Fatalf("GitPATH() = %q, want empty", got)
	}
}
