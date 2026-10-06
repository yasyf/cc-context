package relocate

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/cleanup"
)

type fakeWatchers struct {
	retiring   func(ctx context.Context, worktree string) ([]cleanup.ProcessID, error)
	retire     func(ctx context.Context, worktree string) error
	server     func(ctx context.Context) ([]cleanup.ProcessID, error)
	quarantine func(ctx context.Context, jobDir string) error
	named      []string
	retired    []string
	released   [][]cleanup.ProcessID
	checked    []string
}

func (w *fakeWatchers) Retiring(ctx context.Context, worktree string) ([]cleanup.ProcessID, error) {
	w.named = append(w.named, worktree)
	return w.retiring(ctx, worktree)
}

func (w *fakeWatchers) Retire(ctx context.Context, worktree string) error {
	w.retired = append(w.retired, worktree)
	w.released = append(w.released, cleanup.RetiringFrom(ctx))
	return w.retire(ctx, worktree)
}

func (w *fakeWatchers) Server(ctx context.Context) ([]cleanup.ProcessID, error) {
	return w.server(ctx)
}

func (w *fakeWatchers) CheckQuarantine(ctx context.Context, jobDir string) error {
	w.checked = append(w.checked, jobDir)
	return w.quarantine(ctx, jobDir)
}

var gitBinary = sync.OnceValues(func() (string, error) { return exec.LookPath("git") })

type fixture struct {
	t         *testing.T
	root      string
	git       string
	env       []string
	repo      string
	common    string
	worktree  string
	adminDir  string
	layout    cleanup.Layout
	journal   *cleanup.Journal
	guard     func(ctx context.Context, worktree string) error
	guarded   []string
	discounts [][]cleanup.ProcessID
	watchers  *fakeWatchers
	clock     time.Time
	budget    time.Duration
	relocator *Relocator
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	git, err := gitBinary()
	if err != nil {
		t.Fatalf("find git: %v", err)
	}
	home := filepath.Join(root, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatalf("create home: %v", err)
	}
	f := &fixture{
		t:    t,
		root: root,
		git:  git,
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
		guard:    func(context.Context, string) error { return nil },
		watchers: &fakeWatchers{
			retiring:   func(context.Context, string) ([]cleanup.ProcessID, error) { return nil, nil },
			retire:     func(context.Context, string) error { return nil },
			server:     func(context.Context) ([]cleanup.ProcessID, error) { return nil, nil },
			quarantine: func(context.Context, string) error { return nil },
		},
		clock: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC),
	}
	f.run(root, "init", "-q", "-b", "main", "repo")
	f.write(filepath.Join(f.repo, "base.txt"), "base\n")
	f.run(f.repo, "add", "base.txt")
	f.run(f.repo, "commit", "-q", "-m", "base")
	f.run(f.repo, "worktree", "add", "-q", "-b", "feature", f.worktree)
	f.write(filepath.Join(f.worktree, "feature.txt"), "feature\n")
	f.run(f.worktree, "add", "feature.txt")
	f.run(f.worktree, "commit", "-q", "-m", "feature")

	f.open(filepath.Join(root, "state"))
	return f
}

func (f *fixture) open(stateRoot string) {
	f.t.Helper()
	journal, err := cleanup.OpenJournal(cleanup.Layout{Root: stateRoot})
	if err != nil {
		f.t.Fatalf("open journal: %v", err)
	}
	f.over(journal)
}

func (f *fixture) view(stateRoot string) {
	f.t.Helper()
	journal, err := cleanup.ViewJournal(cleanup.Layout{Root: stateRoot})
	if err != nil {
		f.t.Fatalf("view journal: %v", err)
	}
	f.over(journal)
}

func (f *fixture) over(journal *cleanup.Journal) {
	f.layout, f.journal = journal.Layout(), journal
	f.relocator = New(Config{
		Journal: f.journal,
		Guard: func(ctx context.Context, worktree string) error {
			f.guarded = append(f.guarded, worktree)
			f.discounts = append(f.discounts, cleanup.RetiringFrom(ctx))
			return f.guard(ctx, worktree)
		},
		Watchers:  f.watchers,
		GitEnv:    f.env,
		GitBudget: f.budget,
		Now:       func() time.Time { return f.clock },
	})
}

func (f *fixture) bound(budget time.Duration) {
	f.budget = budget
	f.over(f.journal)
}

func (f *fixture) try(dir string, args ...string) (string, error) {
	cmd := exec.Command(f.git, args...) //nolint:gosec // the fixture's resolved git over argv the tests author
	cmd.Dir = dir
	cmd.Env = f.env
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (f *fixture) run(dir string, args ...string) string {
	f.t.Helper()
	out, err := f.try(dir, args...)
	if err != nil {
		f.t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return out
}

func (f *fixture) write(path, content string) {
	f.t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		f.t.Fatalf("write %s: %v", path, err)
	}
}

func (f *fixture) read(path string) string {
	f.t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		f.t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func (f *fixture) id(path string) cleanup.FileID {
	f.t.Helper()
	id, _, err := cleanup.LstatID(path)
	if err != nil {
		f.t.Fatalf("lstat %s: %v", path, err)
	}
	return id
}

func (f *fixture) absent(path string) {
	f.t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		f.t.Fatalf("lstat %s = %v, want not-exist", path, err)
	}
}

func (f *fixture) holdAt(path string) *cleanup.ActiveError {
	active := &cleanup.ActiveError{
		Worktree: path,
		Holders:  []cleanup.Holder{{PID: 4242, Name: "zsh", TTY: true, Evidence: cleanup.EvidenceCwd, Path: path}},
	}
	f.guard = func(_ context.Context, worktree string) error {
		if worktree == path {
			return active
		}
		return nil
	}
	return active
}

func (f *fixture) release() {
	f.guard = func(context.Context, string) error { return nil }
}

func (f *fixture) request(force bool) cleanup.Request {
	return cleanup.Request{Worktree: f.worktree, Git: f.git, Force: force}
}

func (f *fixture) accept() cleanup.Job {
	f.t.Helper()
	job, err := f.relocator.Accept(context.Background(), 1, f.request(false))
	if err != nil {
		f.t.Fatalf("Accept() error = %v", err)
	}
	return job
}

func (f *fixture) observe(worktree string) cleanup.Registration {
	f.t.Helper()
	observed, err := Observe(worktree)
	if err != nil {
		f.t.Fatalf("Observe(%s) error = %v", worktree, err)
	}
	return observed
}

func (f *fixture) deferral() cleanup.DeferRequest {
	f.t.Helper()
	return cleanup.DeferRequest{
		Worktree: f.worktree, CommonDir: f.common, Owner: "stack", Expected: f.observe(f.worktree), Git: f.git,
	}
}

func (f *fixture) intend() cleanup.Job {
	f.t.Helper()
	job, err := f.relocator.Intend(context.Background(), 1, f.deferral())
	if err != nil {
		f.t.Fatalf("Intend() error = %v", err)
	}
	return job
}

func (f *fixture) advance(job *cleanup.Job) {
	f.t.Helper()
	if err := f.relocator.Advance(context.Background(), job); err != nil {
		f.t.Fatalf("Advance() error = %v", err)
	}
}

func (f *fixture) park(job *cleanup.Job) {
	f.t.Helper()
	_ = f.holdAt(job.Registered)
	f.advance(job)
	if job.Phase != cleanup.PhaseMoved || job.Blocked == nil || job.Blocked.Reason != "activity" {
		f.t.Fatalf("parked job is at %s with blockage %+v, want moved and blocked on activity", job.Phase, job.Blocked)
	}
	f.release()
	job.Blocked = nil
}

func (f *fixture) finished(job *cleanup.Job) {
	f.t.Helper()
	if job.Phase != cleanup.PhaseUnregistered || job.Blocked != nil {
		f.t.Fatalf("job is at %s with blockage %+v, want unregistered and unblocked", job.Phase, job.Blocked)
	}
	f.stored(job)
}

func (f *fixture) blocked(job *cleanup.Job, phase cleanup.Phase, reason, detail string) {
	f.t.Helper()
	if job.Phase != phase {
		f.t.Errorf("phase = %s, want %s", job.Phase, phase)
	}
	if job.Blocked == nil {
		f.t.Fatalf("job is not blocked, want %s: %s", reason, detail)
	}
	if job.Blocked.Reason != reason || job.Blocked.Detail != detail {
		f.t.Errorf("blockage = %s: %s\nwant %s: %s", job.Blocked.Reason, job.Blocked.Detail, reason, detail)
	}
	f.stored(job)
}

func (f *fixture) refused(err error, want cleanup.RefusedError) {
	f.t.Helper()
	var refused *cleanup.RefusedError
	if !errors.As(err, &refused) {
		f.t.Errorf("error = %v, want the refusal %+v", err, want)
		return
	}
	if *refused != want {
		f.t.Errorf("refused %+v\nwant %+v", *refused, want)
	}
}

func (f *fixture) unconsulted() {
	f.t.Helper()
	if len(f.guarded) != 0 {
		f.t.Errorf("guard consulted for %v, want it never asked", f.guarded)
	}
	if len(f.watchers.named) != 0 {
		f.t.Errorf("watchers named for %v, want them never asked", f.watchers.named)
	}
}

func (f *fixture) stored(job *cleanup.Job) {
	f.t.Helper()
	jobs, damaged, err := f.journal.Load()
	if err != nil || len(damaged) != 0 {
		f.t.Fatalf("Load() = damaged %v, error %v", damaged, err)
	}
	for _, stored := range jobs {
		if stored.ID == job.ID {
			if !reflect.DeepEqual(stored, *job) {
				f.t.Errorf("journal holds %+v\nwant %+v", stored, *job)
			}
			return
		}
	}
	f.t.Fatalf("journal holds no job %s", job.ID)
}

func (f *fixture) unjournaled() {
	f.t.Helper()
	entries, err := os.ReadDir(f.layout.JobsDir())
	if err != nil {
		f.t.Fatalf("read jobs dir: %v", err)
	}
	if len(entries) != 0 {
		f.t.Errorf("jobs dir holds %d entries, want none", len(entries))
	}
	if refs := f.run(f.repo, "for-each-ref", cleanup.RecoveryRefPrefix); refs != "" {
		f.t.Errorf("recovery refs = %q, want none", refs)
	}
}

func (f *fixture) listing() string {
	f.t.Helper()
	return f.run(f.repo, "worktree", "list", "--porcelain")
}

func (f *fixture) mainEntry() string {
	f.t.Helper()
	return fmt.Sprintf("worktree %s\nHEAD %s\nbranch refs/heads/main", f.repo, f.run(f.repo, "rev-parse", "refs/heads/main"))
}

func (f *fixture) trap(job *cleanup.Job) string {
	f.t.Helper()
	marker := filepath.Join(f.root, "git-was-run")
	script := filepath.Join(f.root, "trap-git")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho \"$@\" >> "+marker+"\nexit 1\n"), 0o700); err != nil { //nolint:gosec // the script stands in for git and is executed
		f.t.Fatalf("write trap: %v", err)
	}
	job.Git = script
	return marker
}

func idText(id cleanup.FileID) string {
	return fmt.Sprintf("%d:%d", id.Dev, id.Ino)
}

func replacedDetail(path string, holds, named cleanup.Registration) string {
	return fmt.Sprintf(
		"%s no longer holds the worktree the request named: it holds tree %s registered under %s (%s), the request named tree %s registered under %s (%s)",
		path, idText(holds.Tree), holds.AdminDir, idText(holds.Admin), idText(named.Tree), named.AdminDir, idText(named.Admin),
	)
}

func expectedDotGit(job cleanup.Job) string {
	return "gitdir: " + job.AdminDir + "\n"
}

func expectedAdminGitdir(job cleanup.Job) string {
	return job.Registered + "/.git\n"
}
