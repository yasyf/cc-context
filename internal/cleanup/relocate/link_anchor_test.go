package relocate

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRetirementLinkStaysWithCapturedTreeAfterPathReplacement(t *testing.T) {
	f := newFixture(t)
	job := f.pend()
	tree, err := openRetirementTree(&job)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tree.Close() }()
	kept := f.worktree + ".kept"
	if err := os.Rename(f.worktree, kept); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(f.worktree, 0o700); err != nil {
		t.Fatal(err)
	}
	replacementLink := filepath.Join(f.worktree, ".git")
	f.write(replacementLink, job.Links.DotGit)
	replacement := f.id(f.worktree)
	if err := swapRetirementLink(tree, job.Links.DotGit, retiredPointer(&job)); err != nil {
		t.Fatal(err)
	}
	if got := f.read(replacementLink); got != job.Links.DotGit {
		t.Errorf("replacement .git = %q, want %q unchanged", got, job.Links.DotGit)
	}
	if got := f.id(f.worktree); got != replacement {
		t.Errorf("replacement identity = %v, want %v", got, replacement)
	}
	if got := f.read(filepath.Join(kept, ".git")); got != retiredPointer(&job) {
		t.Errorf("captured .git = %q, want %q", got, retiredPointer(&job))
	}
}

func TestRetirementAnchorRefusesReplacementTree(t *testing.T) {
	f := newFixture(t)
	job := f.pend()
	if err := os.Rename(f.worktree, f.worktree+".kept"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(f.worktree, 0o700); err != nil {
		t.Fatal(err)
	}
	f.write(filepath.Join(f.worktree, ".git"), job.Links.DotGit)
	if tree, err := openRetirementTree(&job); err == nil {
		_ = tree.Close()
		t.Fatal("opened replacement as the captured tree")
	}
	if got := f.read(filepath.Join(f.worktree, ".git")); got != job.Links.DotGit {
		t.Errorf("replacement .git = %q, want %q unchanged", got, job.Links.DotGit)
	}
}
