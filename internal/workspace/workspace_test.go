package workspace_test

import (
	"os"
	"testing"

	"github.com/yasyf/cc-context/internal/workspace"
)

func pin(t *testing.T, dir string) {
	t.Helper()
	workspace.SetRoot(dir)
	t.Cleanup(func() { workspace.SetRoot("") })
}

func TestRootFallsBackToTheProcessWorkingDirectory(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	got, err := workspace.Root()
	if err != nil {
		t.Fatalf("Root: %v", err)
	}
	if got != cwd {
		t.Fatalf("unpinned Root = %q, want the process cwd %q", got, cwd)
	}
}

func TestAPinnedRootAnswersInsteadOfTheWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	pin(t, dir)

	got, err := workspace.Root()
	if err != nil {
		t.Fatalf("Root: %v", err)
	}
	if got != dir {
		t.Fatalf("pinned Root = %q, want %q", got, dir)
	}
}

func TestRepinningMovesEveryLaterResolution(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	pin(t, first)

	workspace.SetRoot(second)
	got, err := workspace.Root()
	if err != nil {
		t.Fatalf("Root: %v", err)
	}
	if got != second {
		t.Fatalf("re-pinned Root = %q, want %q", got, second)
	}
}

func TestClearingThePinReturnsToTheWorkingDirectory(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	pin(t, t.TempDir())

	workspace.SetRoot("")
	got, err := workspace.Root()
	if err != nil {
		t.Fatalf("Root: %v", err)
	}
	if got != cwd {
		t.Fatalf("cleared Root = %q, want the process cwd %q", got, cwd)
	}
}
