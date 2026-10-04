//go:build !windows

package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/cleanup"
	"github.com/yasyf/cc-context/internal/cleanup/relocate"
	"github.com/yasyf/cc-context/internal/cleanup/rmtree"
)

const (
	drainBudget = 20 * time.Second
	drainWait   = time.Minute
)

var realGit = sync.OnceValues(func() (string, error) { return exec.LookPath("git") })

type idleCPU struct{}

func (idleCPU) Sample(context.Context) (time.Duration, error) { return 0, nil }

type idleWatchers struct{}

func (idleWatchers) Retiring(context.Context, string) ([]cleanup.ProcessID, error) { return nil, nil }

func (idleWatchers) Retire(context.Context, string) error { return nil }

func (idleWatchers) CheckQuarantine(context.Context, string) error { return nil }

type gitFixture struct {
	t        *testing.T
	git      string
	wrapper  string
	env      []string
	repo     string
	common   string
	worktree string
	adminDir string
	marker   string
	log      string
	entered  string
	release  string
	journal  *cleanup.Journal
	clock    Clock
}

func newGitFixture(t *testing.T) *gitFixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	git, err := realGit()
	if err != nil {
		t.Fatalf("find git: %v", err)
	}
	home := filepath.Join(root, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatalf("create home: %v", err)
	}
	f := &gitFixture{
		t:       t,
		git:     git,
		wrapper: filepath.Join(root, "holding-git"),
		env: []string{
			"HOME=" + home,
			"GIT_CONFIG_GLOBAL=" + filepath.Join(home, "gitconfig"),
			"GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=Test Author",
			"GIT_AUTHOR_EMAIL=author@example.com",
			"GIT_AUTHOR_DATE=2026-01-02T03:04:05Z",
			"GIT_COMMITTER_NAME=Test Committer",
			"GIT_COMMITTER_EMAIL=committer@example.com",
			"GIT_COMMITTER_DATE=2026-01-02T03:04:05Z",
			"PATH=" + os.Getenv("PATH"),
		},
		repo:     filepath.Join(root, "repo"),
		common:   filepath.Join(root, "repo", ".git"),
		worktree: filepath.Join(root, "wt"),
		adminDir: filepath.Join(root, "repo", ".git", "worktrees", "wt"),
		marker:   filepath.Join(root, "holding"),
		log:      filepath.Join(root, "git-calls"),
		entered:  filepath.Join(root, "entered"),
		release:  filepath.Join(root, "release"),
	}
	f.run(root, "init", "-q", "-b", "main", "repo")
	f.write(filepath.Join(f.repo, "base.txt"), "base\n")
	f.run(f.repo, "add", "base.txt")
	f.run(f.repo, "commit", "-q", "-m", "base")
	f.run(f.repo, "worktree", "add", "-q", "-b", "feature", f.worktree)
	f.write(filepath.Join(f.worktree, "feature.txt"), "feature\n")
	f.run(f.worktree, "add", "feature.txt")
	f.run(f.worktree, "commit", "-q", "-m", "feature")
	for _, fifo := range []string{f.entered, f.release} {
		if err := syscall.Mkfifo(fifo, 0o600); err != nil {
			t.Fatalf("mkfifo %s: %v", fifo, err)
		}
	}
	script := fmt.Sprintf(`#!/bin/sh
echo "$*" >> %[1]s
if [ -e %[2]s ]; then
	read mode pattern < %[2]s
	case " $* " in *" $pattern "*)
		case $mode in
		fail) exit 1 ;;
		hold) echo x > %[3]s; read x < %[4]s ;;
		esac
	esac
fi
exec %[5]s "$@"
`, f.log, f.marker, f.entered, f.release, f.git)
	if err := os.WriteFile(f.wrapper, []byte(script), 0o700); err != nil { //nolint:gosec // the script stands in for git and is executed
		t.Fatalf("write holding git: %v", err)
	}
	journal, err := cleanup.OpenJournal(cleanup.Layout{Root: filepath.Join(root, "state")})
	if err != nil {
		t.Fatalf("open journal: %v", err)
	}
	f.journal = journal
	return f
}

func (f *gitFixture) run(dir string, args ...string) string {
	f.t.Helper()
	cmd := exec.Command(f.git, args...) //nolint:gosec // the fixture's resolved git over argv the tests author
	cmd.Dir = dir
	cmd.Env = f.env
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func (f *gitFixture) write(path, content string) {
	f.t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		f.t.Fatalf("write %s: %v", path, err)
	}
}

func (f *gitFixture) hold(pattern string) { f.write(f.marker, "hold "+pattern+"\n") }

func (f *gitFixture) fail(pattern string) { f.write(f.marker, "fail "+pattern+"\n") }

func (f *gitFixture) pass() {
	f.t.Helper()
	if err := os.Remove(f.marker); err != nil {
		f.t.Fatalf("let git pass: %v", err)
	}
}

func (f *gitFixture) gitCalls() string {
	f.t.Helper()
	calls, err := os.ReadFile(f.log)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		f.t.Fatalf("read the git call log: %v", err)
	}
	return string(calls)
}

func (f *gitFixture) ran(subcommand string) bool {
	f.t.Helper()
	return strings.Contains(f.gitCalls(), " "+subcommand+" ")
}

func (f *gitFixture) relocator() *relocate.Relocator {
	return relocate.New(relocate.Config{
		Journal:   f.journal,
		Guard:     func(context.Context, string) error { return nil },
		Watchers:  idleWatchers{},
		GitEnv:    f.env,
		GitBudget: drainBudget,
		Now:       time.Now,
	})
}

func (f *gitFixture) engine() *Engine {
	f.t.Helper()
	tuning := DefaultTuning()
	tuning.Rate, tuning.SampleEvery, tuning.Recheck = 1_000_000, 5*time.Millisecond, 20*time.Millisecond
	engine, err := New(Config{Journal: f.journal, Relocator: f.relocator(), Deleter: rmtree.Deleter{}, CPU: idleCPU{}, Parent: (&fakeParent{}).lookup, Version: "test", Clock: f.clock, Tuning: tuning})
	if err != nil {
		f.t.Fatalf("New() = %v", err)
	}
	return engine
}

func (f *gitFixture) start(engine *Engine) <-chan error {
	running := make(chan error, 1)
	go func() { running <- engine.Run(context.Background()) }()
	f.t.Cleanup(func() {
		_ = os.Remove(f.marker)
		if file, err := os.OpenFile(f.release, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil { //nolint:gosec // the fifo is the test's own
			_, _ = file.Write([]byte("x\n"))
			_ = file.Close()
		}
		ctx, cancel := context.WithTimeout(context.Background(), drainWait)
		defer cancel()
		_ = engine.Stop(ctx)
	})
	return running
}

func (f *gitFixture) stop(engine *Engine) <-chan error {
	stopped := make(chan error, 1)
	go func() { stopped <- engine.Stop(context.Background()) }()
	<-engine.quit
	return stopped
}

func (f *gitFixture) await(ch <-chan error, what string) error {
	f.t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(drainWait):
		f.t.Fatalf("%s did not return within %s", what, drainWait)
		return nil
	}
}

func (f *gitFixture) awaitEntry(hold string) {
	f.t.Helper()
	entered := make(chan error, 1)
	go func() {
		file, err := os.OpenFile(f.entered, os.O_RDONLY, 0) //nolint:gosec // the fifo is the test's own
		if err != nil {
			entered <- err
			return
		}
		_, err = file.Read(make([]byte, 1))
		entered <- errors.Join(err, file.Close())
	}()
	select {
	case err := <-entered:
		if err != nil {
			f.t.Fatalf("read the held %s's entry: %v", hold, err)
		}
	case <-time.After(drainWait):
		f.t.Fatalf("the held %s never entered within %s", hold, drainWait)
	}
}

func (f *gitFixture) releaseHeld() {
	f.t.Helper()
	opened := make(chan *os.File, 1)
	failed := make(chan error, 1)
	go func() {
		file, err := os.OpenFile(f.release, os.O_WRONLY, 0) //nolint:gosec // the fifo is the test's own
		if err != nil {
			failed <- err
			return
		}
		opened <- file
	}()
	var file *os.File
	select {
	case file = <-opened:
	case err := <-failed:
		f.t.Fatalf("release the held git: %v", err)
	case <-time.After(drainWait):
		f.t.Fatalf("the held git never read the release fifo within %s", drainWait)
	}
	if _, err := file.Write([]byte("x\n")); err != nil {
		f.t.Fatalf("release the held git: %v", err)
	}
	if err := file.Close(); err != nil {
		f.t.Fatalf("close the release fifo: %v", err)
	}
}

func (f *gitFixture) heldGone(hold string) {
	f.t.Helper()
	file, err := os.OpenFile(f.release, os.O_WRONLY|syscall.O_NONBLOCK, 0) //nolint:gosec // the fifo is the test's own
	if errors.Is(err, syscall.ENXIO) {
		return
	}
	if err != nil {
		f.t.Fatalf("probe the release fifo: %v", err)
	}
	f.t.Errorf("the held %s still reads the release fifo after the stop, want its git child gone", hold)
	_, _ = file.Write([]byte("x\n"))
	_ = file.Close()
}

func (f *gitFixture) drain(engine *Engine, hold string, read bool) {
	f.t.Helper()
	stopped := f.stop(engine)
	started := time.Now()
	if !read {
		select {
		case err := <-stopped:
			f.t.Fatalf("Stop() = %v before the held %s was released, want the mutation waited for", err, hold)
		default:
		}
		f.releaseHeld()
	}
	if err := f.await(stopped, "Stop()"); err != nil {
		f.t.Errorf("Stop() = %v", err)
	}
	if !read {
		return
	}
	if elapsed := time.Since(started); elapsed >= drainBudget {
		f.t.Errorf("Stop() took %s with the %s read held, want it killed well inside its %s budget", elapsed, hold, drainBudget)
	}
	f.heldGone(hold)
}

func (f *gitFixture) request() cleanup.Request {
	return cleanup.Request{Worktree: f.worktree, Git: f.wrapper}
}

func (f *gitFixture) accept() cleanup.Job {
	f.t.Helper()
	job, err := f.relocator().Accept(context.Background(), 1, f.request())
	if err != nil {
		f.t.Fatalf("Accept() = %v", err)
	}
	return job
}

func (f *gitFixture) deferral() cleanup.DeferRequest {
	f.t.Helper()
	expected, err := relocate.Observe(f.worktree)
	if err != nil {
		f.t.Fatalf("Observe() = %v", err)
	}
	return cleanup.DeferRequest{Worktree: f.worktree, CommonDir: f.common, Owner: "stack", Expected: expected, Git: f.wrapper}
}

func (f *gitFixture) adoption() cleanup.AdoptRequest {
	f.t.Helper()
	const date, id = "20260928", "0123456789abcdef0123"
	return cleanup.AdoptRequest{
		Source:      filepath.Join(filepath.Dir(f.repo), cleanup.LegacyQuarantinePrefix+date, id+"-wt"),
		Tree:        cleanup.FileID{Dev: 1, Ino: 1},
		CommonDir:   f.common,
		Head:        f.run(f.repo, "rev-parse", "refs/heads/feature"),
		RecoveryRef: cleanup.LegacyRecoveryPrefix + date + "/" + id,
		Original:    f.worktree,
		Owner:       "legacy quarantine import",
		Git:         f.wrapper,
	}
}

func (f *gitFixture) intend() cleanup.Job {
	f.t.Helper()
	job, err := f.relocator().Intend(context.Background(), 1, f.deferral())
	if err != nil {
		f.t.Fatalf("Intend() = %v", err)
	}
	return job
}

func (f *gitFixture) park(job *cleanup.Job, failing string, phase cleanup.Phase, reason string) {
	f.t.Helper()
	f.fail(failing)
	if err := f.relocator().Advance(context.Background(), job); err != nil {
		f.t.Fatalf("Advance() past the failing %s = %v", failing, err)
	}
	if job.Phase != phase || job.Blocked == nil || job.Blocked.Reason != reason {
		f.t.Fatalf("the failing %s left the job at %s with blockage %+v, want %s blocked on %s", failing, job.Phase, job.Blocked, phase, reason)
	}
	job.Blocked = nil
	if err := f.journal.Save(*job); err != nil {
		f.t.Fatalf("Save() = %v", err)
	}
	f.pass()
}

func (f *gitFixture) journaled(id string) cleanup.Job {
	f.t.Helper()
	jobs, damaged, err := f.journal.Load()
	if err != nil || len(damaged) != 0 {
		f.t.Fatalf("Load() = damaged %v, error %v", damaged, err)
	}
	for _, job := range jobs {
		if job.ID == id {
			return job
		}
	}
	f.t.Fatalf("the journal holds no job %s", id)
	return cleanup.Job{}
}

func (f *gitFixture) rests(job cleanup.Job, phase cleanup.Phase) cleanup.Job {
	f.t.Helper()
	got := f.journaled(job.ID)
	if got.Phase != phase || got.Blocked != nil || len(got.Errors) != len(job.Errors) {
		f.t.Errorf("journal after the stop = phase %s, blockage %+v, history %v; want %s, unblocked, the %d entries the seed left", got.Phase, got.Blocked, got.Errors, phase, len(job.Errors))
	}
	return got
}

func (f *gitFixture) id(path string) cleanup.FileID {
	f.t.Helper()
	id, _, err := cleanup.LstatID(path)
	if err != nil {
		f.t.Fatalf("lstat %s: %v", path, err)
	}
	return id
}

func (f *gitFixture) absent(path string) {
	f.t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		f.t.Errorf("lstat %s = %v, want not-exist", path, err)
	}
}

func (f *gitFixture) listing() string {
	f.t.Helper()
	return f.run(f.repo, "worktree", "list", "--porcelain")
}

func (f *gitFixture) mainEntry() string {
	f.t.Helper()
	return fmt.Sprintf("worktree %s\nHEAD %s\nbranch refs/heads/main", f.repo, f.run(f.repo, "rev-parse", "refs/heads/main"))
}

func (f *gitFixture) recovered(job cleanup.Job) {
	f.t.Helper()
	if got := f.run(f.repo, "rev-parse", job.RecoveryRef); got != job.Head {
		f.t.Errorf("recovery ref = %s, want %s", got, job.Head)
	}
}

func (f *gitFixture) resume(job cleanup.Job) cleanup.Job {
	f.t.Helper()
	f.pass()
	engine := f.engine()
	running := f.start(engine)
	ctx, cancel := context.WithTimeout(context.Background(), drainWait)
	defer cancel()
	done, err := engine.Wait(ctx, job.ID)
	if err != nil || done.Phase != cleanup.PhaseDone {
		f.t.Fatalf("Wait() on the restarted engine = phase %s, blockage %+v, %v; want done", done.Phase, done.Blocked, err)
	}
	if err := f.await(f.stop(engine), "Stop()"); err != nil {
		f.t.Errorf("Stop() on the restarted engine = %v", err)
	}
	if err := f.await(running, "Run()"); err != nil {
		f.t.Errorf("Run() on the restarted engine = %v", err)
	}
	if len(done.Errors) != len(job.Errors) {
		f.t.Errorf("history after the restart = %v, want the %d entries the seed left", done.Errors, len(job.Errors))
	}
	return done
}

func (f *gitFixture) finished(job cleanup.Job) {
	f.t.Helper()
	for _, path := range []string{f.worktree, job.Registered, job.Payload, f.adminDir} {
		f.absent(path)
	}
	if got, want := f.listing(), f.mainEntry(); got != want {
		f.t.Errorf("worktree list = %q, want %q", got, want)
	}
	f.recovered(job)
	if got := f.run(f.repo, "rev-parse", "refs/heads/feature"); got != job.Head {
		f.t.Errorf("feature branch = %s, want %s", got, job.Head)
	}
}

func TestStopYieldsAtEveryLadderBoundaryAndARestartFinishes(t *testing.T) {
	seedMoved := func(f *gitFixture) cleanup.Job {
		job := f.accept()
		f.park(&job, "rev-parse", cleanup.PhaseMoved, "identity")
		return job
	}
	seedDetached := func(f *gitFixture) cleanup.Job {
		job := f.accept()
		f.park(&job, "worktree remove", cleanup.PhaseDetached, "git")
		return job
	}
	atOriginal := func(f *gitFixture, _ cleanup.Job) string { return f.worktree }
	atRegistered := func(_ *gitFixture, job cleanup.Job) string { return job.Registered }
	atPayload := func(_ *gitFixture, job cleanup.Job) string { return job.Payload }
	tests := []struct {
		name  string
		seed  func(f *gitFixture) cleanup.Job
		hold  string
		read  bool
		phase cleanup.Phase
		tree  func(f *gitFixture, job cleanup.Job) string
		check func(f *gitFixture, job cleanup.Job)
	}{
		{"the release read", (*gitFixture).intend, "rev-parse", true, cleanup.PhaseQueued, atOriginal, nil},
		{"the move read", (*gitFixture).accept, "status", true, cleanup.PhasePrepared, atOriginal, nil},
		{"the update-ref mutation", (*gitFixture).accept, "update-ref", false, cleanup.PhasePrepared, atOriginal, func(f *gitFixture, job cleanup.Job) {
			f.recovered(job)
			if f.ran("worktree move") {
				f.t.Error("worktree move ran after the stop, want no mutation started under a cancelled step")
			}
		}},
		{"the worktree move mutation", (*gitFixture).accept, "worktree move", false, cleanup.PhaseMoved, atRegistered, (*gitFixture).recovered},
		{"the detach read", seedMoved, "rev-parse", true, cleanup.PhaseMoved, atRegistered, nil},
		{"the worktree remove mutation", seedDetached, "worktree remove", false, cleanup.PhaseUnregistered, atPayload, func(f *gitFixture, _ cleanup.Job) {
			if got, want := f.listing(), f.mainEntry(); got != want {
				f.t.Errorf("worktree list = %q, want %q", got, want)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newGitFixture(t)
			job := tt.seed(f)
			f.hold(tt.hold)
			engine := f.engine()
			running := f.start(engine)
			f.awaitEntry(tt.hold)

			f.drain(engine, tt.hold, tt.read)
			if err := f.await(running, "Run()"); err != nil {
				t.Errorf("Run() = %v, want nil: a stop is not a journal failure", err)
			}
			rested := f.rests(job, tt.phase)
			if got := f.id(tt.tree(f, rested)); got != job.Tree {
				t.Errorf("tree identity at %s = %v, want the tree %v", tt.tree(f, rested), got, job.Tree)
			}
			if tt.check != nil {
				tt.check(f, rested)
			}

			f.finished(f.resume(job))
		})
	}
}

func TestStopWithARemoveInFlightAnswersItsCaller(t *testing.T) {
	tests := []struct {
		name string
		hold string
		read bool
	}{
		{"killed in its preflight read", "status", true},
		{"stopped past its worktree move", "worktree move", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newGitFixture(t)
			tree := f.id(f.worktree)
			f.hold(tt.hold)
			engine := f.engine()
			running := f.start(engine)
			removed := make(chan error, 1)
			go func() {
				_, err := engine.Remove(context.Background(), f.request())
				removed <- err
			}()
			f.awaitEntry(tt.hold)

			f.drain(engine, tt.hold, tt.read)
			if err := f.await(running, "Run()"); err != nil {
				t.Errorf("Run() = %v, want nil: a stop is not a journal failure", err)
			}
			err := f.await(removed, "Remove()")
			jobs, damaged, loadErr := f.journal.Load()
			if loadErr != nil || len(damaged) != 0 {
				t.Fatalf("Load() = damaged %v, error %v", damaged, loadErr)
			}
			if tt.read {
				want := fmt.Sprintf("cleanup: inspect %s: git -C %s status --porcelain=v1 -z --untracked-files=normal --ignore-submodules=none: signal: killed", f.worktree, f.worktree)
				if err == nil || err.Error() != want {
					t.Errorf("Remove() = %v, want %q", err, want)
				}
				if len(jobs) != 0 {
					t.Errorf("journal holds %d jobs after the killed preflight, want none", len(jobs))
				}
				if refs := f.run(f.repo, "for-each-ref", cleanup.RecoveryRefPrefix); refs != "" {
					t.Errorf("recovery refs = %q, want none", refs)
				}
				if got := f.id(f.worktree); got != tree {
					t.Errorf("tree identity at %s = %v, want the untouched tree %v", f.worktree, got, tree)
				}
				return
			}
			var blocked *cleanup.BlockedError
			if !errors.Is(err, ErrStopped) || !errors.Is(err, context.Canceled) || errors.As(err, &blocked) {
				t.Errorf("Remove() across the stop = %v, want ErrStopped wrapping context.Canceled", err)
			}
			if len(jobs) != 1 {
				t.Fatalf("journal holds %d jobs, want the one the remove journaled", len(jobs))
			}
			job := f.rests(jobs[0], cleanup.PhaseMoved)
			if len(job.Errors) != 0 {
				t.Errorf("history = %v, want none", job.Errors)
			}
			if got := f.id(job.Registered); got != tree {
				t.Errorf("tree identity at %s = %v, want the tree %v", job.Registered, got, tree)
			}
			f.recovered(job)
			f.finished(f.resume(job))
		})
	}
}
