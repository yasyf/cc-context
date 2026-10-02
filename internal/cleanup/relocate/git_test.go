package relocate

import (
	"context"
	"errors"
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
	if err := os.WriteFile(git, []byte(script), 0o700); err != nil { //nolint:gosec // the recorder stands in for git and is executed
		f.t.Fatalf("write recorder: %v", err)
	}
	return git, log
}

func (f *fixture) failingHook() string {
	f.t.Helper()
	marker := filepath.Join(f.root, "hook-ran")
	hooks := filepath.Join(f.common, "hooks")
	if err := os.MkdirAll(hooks, 0o700); err != nil {
		f.t.Fatalf("create hooks dir: %v", err)
	}
	script := "#!/bin/sh\necho ran >> " + marker + "\nexit 1\n"
	if err := os.WriteFile(filepath.Join(hooks, "reference-transaction"), []byte(script), 0o700); err != nil { //nolint:gosec // git executes the hook
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

func TestPushedUsesExactRemoteWitnessBeforeScanning(t *testing.T) {
	for _, tt := range []struct {
		name     string
		remote   string
		exact    string
		other    bool
		want     bool
		fallback bool
	}{
		{name: "origin exact witness", remote: "origin", exact: "feature", want: true},
		{name: "configured remote witness", remote: "upstream", exact: "feature", want: true},
		{name: "missing exact ref", remote: "origin", other: true, want: true, fallback: true},
		{name: "non-containing exact ref", remote: "origin", exact: "main", other: true, want: true, fallback: true},
		{name: "no witness", remote: "origin", exact: "main", fallback: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			job := f.accept()
			if tt.remote != "origin" {
				f.run(f.repo, "config", "branch.feature.remote", tt.remote)
			}
			ref := "refs/remotes/" + tt.remote + "/feature"
			if tt.exact != "" {
				f.run(f.repo, "update-ref", ref, tt.exact)
			}
			if tt.other {
				f.run(f.repo, "update-ref", "refs/remotes/another/witness", "feature")
			}
			git, log := f.recorder()
			got, err := f.relocator.pushed(context.Background(), git, f.common, job.Branch, job.Head)
			if err != nil || got != tt.want {
				t.Fatalf("pushed = %t, %v; want %t", got, err, tt.want)
			}
			prefix := "--git-dir=" + f.common + " "
			want := []string{prefix + "config --get branch.feature.remote", prefix + "show-ref --verify --quiet " + ref}
			if tt.exact != "" {
				want = append(want, prefix+"merge-base --is-ancestor "+job.Head+" "+ref)
			}
			if tt.fallback {
				want = append(want, prefix+"for-each-ref --count=1 --contains "+job.Head+" --format=%(refname) refs/remotes/")
			}
			f.recorded(log, want)
		})
	}
}

func TestExactPushedWitnessDoesNotBypassActivity(t *testing.T) {
	for _, tt := range []struct {
		name     string
		activate func(*fixture) string
	}{
		{name: "active holder", activate: func(f *fixture) string { return f.holdAt(f.worktree).Error() }},
		{name: "unreadable evidence", activate: func(f *fixture) string {
			f.guard = func(context.Context, string) error { return errors.New("native process evidence unreadable") }
			return "could not verify that " + f.worktree + " is idle: native process evidence unreadable"
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.run(f.repo, "update-ref", "refs/remotes/origin/feature", "feature")
			job := f.accept()
			var detail string
			f.watchers.retire = func(context.Context, string) error {
				detail = tt.activate(f)
				return errListingTimedOut
			}
			f.advance(&job)
			f.blocked(&job, cleanup.PhasePrepared, "activity", detail)
			if f.id(f.worktree) != job.Tree {
				t.Fatal("the active tree moved despite its native guard")
			}
			f.absent(job.Registered)
			f.absent(job.Payload)
		})
	}
}

func TestUnprobedMarksOnlyAReadThatOutlivedItsBound(t *testing.T) {
	expired, cancelExpired := context.WithTimeout(context.Background(), 0)
	defer cancelExpired()
	<-expired.Done()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	live := context.Background()
	failed := errors.New("git status: signal: killed")

	tests := []struct {
		name       string
		ctx        context.Context
		bounded    context.Context
		err        error
		wantReason string
	}{
		{"a read past its bound while the step is live", live, expired, failed, "timeout"},
		{"a read the step's own cancellation stopped", cancelled, expired, failed, "git"},
		{"a read that failed inside its bound", live, live, failed, "git"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := unprobed(tt.ctx, tt.bounded, tt.err)
			if !errors.Is(err, failed) {
				t.Errorf("unprobed = %v, want it to wrap %v", err, failed)
			}
			if got := gitReason(err); got != tt.wantReason {
				t.Errorf("gitReason(%v) = %q, want %q", err, got, tt.wantReason)
			}
		})
	}
	if err := unprobed(live, expired, nil); err != nil {
		t.Errorf("unprobed of a read that succeeded = %v, want nil", err)
	}
}
