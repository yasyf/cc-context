package relocate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/cleanup"
)

const isolation = "-c core.fsmonitor=false -c core.hooksPath=/dev/null --no-optional-locks "

func (f *fixture) recorder() (git, log string) {
	f.t.Helper()
	git, log = filepath.Join(f.root, "recording-git"), filepath.Join(f.root, "git-argv")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" >> %s\nexec %s \"$@\"\n", log, f.git)
	if err := os.WriteFile(git, []byte(script), 0o755); err != nil {
		f.t.Fatalf("write recorder: %v", err)
	}
	return git, log
}

func (f *fixture) failingHook() string {
	f.t.Helper()
	marker := filepath.Join(f.root, "hook-ran")
	hooks := filepath.Join(f.common, "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		f.t.Fatalf("create hooks dir: %v", err)
	}
	script := "#!/bin/sh\necho ran >> " + marker + "\nexit 1\n"
	if err := os.WriteFile(filepath.Join(hooks, "reference-transaction"), []byte(script), 0o755); err != nil {
		f.t.Fatalf("write hook: %v", err)
	}
	if out, err := f.try(f.repo, "update-ref", "refs/probe", "refs/heads/main"); err == nil {
		f.t.Fatalf("update-ref succeeded under a failing reference-transaction hook: %s", out)
	}
	if _, err := os.Stat(marker); err != nil {
		f.t.Fatalf("the hook left no marker: %v", err)
	}
	if err := os.Remove(marker); err != nil {
		f.t.Fatalf("remove marker: %v", err)
	}
	return marker
}

func (f *fixture) recorded(log string, want []string) {
	f.t.Helper()
	got := strings.Split(strings.TrimSuffix(f.read(log), "\n"), "\n")
	for i := range want {
		want[i] = isolation + want[i]
	}
	if !reflect.DeepEqual(got, want) {
		f.t.Errorf("git ran with\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestEveryGitChildOfARemovalIsIsolatedFromTheProject(t *testing.T) {
	f := newFixture(t)
	marker := f.failingHook()
	git, log := f.recorder()
	job, err := f.relocator.Accept(context.Background(), 1, cleanup.Request{Worktree: f.worktree, Git: git})
	if err != nil {
		t.Fatalf("Accept() error = %v", err)
	}

	f.advance(&job)

	f.finished(&job)
	status := "-C " + f.worktree + " status --porcelain=v1 -z --untracked-files=normal --ignore-submodules=none"
	f.recorded(log, []string{
		status,
		"-C " + f.worktree + " rev-parse --verify --quiet HEAD^{commit}",
		"-C " + f.worktree + " symbolic-ref --quiet --short HEAD",
		status,
		"--git-dir=" + f.common + " update-ref " + job.RecoveryRef + " " + job.Head,
		"--git-dir=" + f.common + " -c worktree.useRelativePaths=false worktree move " + f.worktree + " " + job.Registered,
		"-C " + job.Registered + " rev-parse --verify --quiet HEAD^{commit}",
		"--git-dir=" + f.common + " worktree remove " + job.Registered,
	})
	f.absent(marker)
	if got := f.run(f.repo, "rev-parse", job.RecoveryRef); got != job.Head {
		t.Errorf("recovery ref = %s, want %s", got, job.Head)
	}
	if got, want := f.listing(), f.mainEntry(); got != want {
		t.Errorf("worktree list = %q, want %q", got, want)
	}
}

func TestEveryGitChildOfAnAdoptionIsIsolatedFromTheProject(t *testing.T) {
	q := newQuarantine(t)
	marker := q.failingHook()
	git, log := q.recorder()
	q.request.Git = git
	job := q.adopt()

	q.advance(&job)

	q.finished(&job)
	pinned := "--git-dir=" + q.common + " rev-parse --verify --quiet " + q.ref + "^{commit}"
	q.recorded(log, []string{pinned, pinned})
	q.absent(marker)
	if got := q.run(q.repo, "rev-parse", q.ref); got != q.head {
		t.Errorf("legacy ref = %s, want %s", got, q.head)
	}
}
