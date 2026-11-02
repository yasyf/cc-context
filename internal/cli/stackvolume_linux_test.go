//go:build linux

package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStackOnVolumeOfPicksTheCandidateBesideTheCleanupFolder(t *testing.T) {
	jobs := filepath.Join(t.TempDir(), "jobs")
	other, err := os.MkdirTemp("/dev/shm", "ccx-volume-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(other) })
	away := filepath.Join(other, "worktrees", "repo", "conflict-feature")
	beside := filepath.Join(t.TempDir(), "worktrees", "repo", "conflict-feature")
	if got, err := stackOnVolumeOf(jobs, []string{away, beside}); err != nil || got != beside {
		t.Fatalf("stackOnVolumeOf(away, beside) = %q, %v, want %q", got, err, beside)
	}
	if got, err := stackOnVolumeOf(jobs, []string{away}); err != nil || got != "" {
		t.Fatalf("stackOnVolumeOf(away) = %q, %v, want no candidate", got, err)
	}
}
