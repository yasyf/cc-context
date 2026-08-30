package workspace_test

import (
	"context"
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

func TestAContextRootSurvivesALaterRepin(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	pin(t, first)
	ctx := workspace.WithRoot(context.Background(), workspace.Declared())

	workspace.SetRoot(second)

	got, err := workspace.RootFrom(ctx)
	if err != nil {
		t.Fatalf("RootFrom: %v", err)
	}
	if got != first {
		t.Fatalf("RootFrom after a re-pin = %q, want the root the context carries %q", got, first)
	}
	if got := workspace.DeclaredFrom(ctx); got != first {
		t.Fatalf("DeclaredFrom after a re-pin = %q, want %q", got, first)
	}
}

func TestAContextCarryingNoDeclarationReadsThePin(t *testing.T) {
	dir := t.TempDir()
	pin(t, dir)

	got, err := workspace.RootFrom(context.Background())
	if err != nil {
		t.Fatalf("RootFrom: %v", err)
	}
	if got != dir {
		t.Fatalf("RootFrom with no context root = %q, want the pin %q", got, dir)
	}
}

func TestAContextDeclaringNoRootReadsTheWorkingDirectory(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	pin(t, t.TempDir())
	ctx := workspace.WithRoot(context.Background(), "")

	got, err := workspace.RootFrom(ctx)
	if err != nil {
		t.Fatalf("RootFrom: %v", err)
	}
	if got != cwd {
		t.Fatalf("RootFrom with an empty context root = %q, want the process cwd %q", got, cwd)
	}
}
