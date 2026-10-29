package relocate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/daemonkit/durable"

	"github.com/yasyf/cc-context/internal/cleanup"
)

func (f *fixture) retire(job *cleanup.Job) {
	f.t.Helper()
	if err := f.relocator.Retire(context.Background(), job); err != nil {
		f.t.Fatalf("Retire() error = %v", err)
	}
}

func (f *fixture) stuck(job *cleanup.Job, phase cleanup.Phase, reason, fragment string) {
	f.t.Helper()
	if job.Phase != phase || job.Blocked == nil || job.Blocked.Reason != reason || !strings.Contains(job.Blocked.Detail, fragment) {
		f.t.Fatalf("job = phase %s, blockage %+v; want %s blocked on %s naming %q", job.Phase, job.Blocked, phase, reason, fragment)
	}
	f.stored(job)
}

func (f *fixture) registeredInPlace(job *cleanup.Job, tree cleanup.FileID, listing string) {
	f.t.Helper()
	if got := f.id(f.worktree); got != tree {
		f.t.Errorf("worktree identity = %v, want %v", got, tree)
	}
	if got := f.read(filepath.Join(f.worktree, ".git")); got != job.Links.DotGit {
		f.t.Errorf("%s/.git = %q, want %q", f.worktree, got, job.Links.DotGit)
	}
	if got := f.id(f.adminDir); got != job.Admin {
		f.t.Errorf("admin identity = %v, want %v", got, job.Admin)
	}
	if got := f.listing(); got != listing {
		f.t.Errorf("worktree list = %q, want %q", got, listing)
	}
	f.absent(cleanup.RetiredGitDirFor(job.Repo, job.ID))
	if len(f.watchers.retired) != 0 {
		f.t.Errorf("watchers retired for %v, want none", f.watchers.retired)
	}
}

func (f *fixture) retiredInPlace(job *cleanup.Job, tree cleanup.FileID, index string) {
	f.t.Helper()
	gitDir := cleanup.RetiredGitDirFor(job.Repo, job.ID)
	if job.Phase != cleanup.PhaseRetired || job.Blocked != nil {
		f.t.Fatalf("job = phase %s, blockage %+v; want retired in place", job.Phase, job.Blocked)
	}
	f.stored(job)
	if got := f.id(f.worktree); got != tree {
		f.t.Errorf("worktree identity = %v, want the tree left at its path as %v", got, tree)
	}
	if got := f.read(filepath.Join(f.worktree, ".git")); got != "gitdir: "+gitDir+"\n" {
		f.t.Errorf("%s/.git = %q, want it naming %s", f.worktree, got, gitDir)
	}
	f.absent(f.adminDir)
	if got := f.id(gitDir); got != job.Admin {
		f.t.Errorf("%s identity = %v, want the admin directory %v itself, moved", gitDir, got, job.Admin)
	}
	if got := f.read(filepath.Join(gitDir, "index")); got != index {
		f.t.Errorf("%s/index differs from the index the tree had", gitDir)
	}
	if got := f.read(filepath.Join(gitDir, "gitdir")); got != job.Links.AdminGitdir {
		f.t.Errorf("%s/gitdir = %q, want the back-link %q kept", gitDir, got, job.Links.AdminGitdir)
	}
	f.alone(job)
	if got := f.listing(); got != f.mainEntry() {
		f.t.Errorf("worktree list = %q, want only the main worktree", got)
	}
	if got := f.run(f.worktree, "rev-parse", "HEAD"); got != job.Head {
		f.t.Errorf("HEAD in the retired tree = %s, want %s", got, job.Head)
	}
	if got := f.run(f.worktree, "rev-parse", "--absolute-git-dir"); got != gitDir {
		f.t.Errorf("git dir of the retired tree = %s, want %s", got, gitDir)
	}
	if got := f.run(f.worktree, "status", "--porcelain"); got != "" {
		f.t.Errorf("status in the retired tree = %q, want clean", got)
	}
	if refs := f.run(f.repo, "for-each-ref", "refs/ccx/"); refs != "" {
		f.t.Errorf("ccx refs = %q, want none before the tree moves", refs)
	}
	if len(f.watchers.retired) != 0 {
		f.t.Errorf("watchers retired for %v, want none", f.watchers.retired)
	}
}

func (f *fixture) alone(job *cleanup.Job) {
	f.t.Helper()
	entries, err := os.ReadDir(filepath.Dir(cleanup.RetiredGitDirFor(job.Repo, job.ID)))
	if err != nil {
		f.t.Fatalf("read the retired admin directories: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if want := []string{job.ID}; !slices.Equal(names, want) {
		f.t.Errorf("retired admin directories = %v, want only %v", names, want)
	}
}

func TestRetireUnregistersAPendingTreeInPlace(t *testing.T) {
	f := newFixture(t)
	job := f.pend()
	tree, index := f.id(f.worktree), f.read(filepath.Join(f.adminDir, "index"))

	f.retire(&job)

	f.retiredInPlace(&job, tree, index)
	if want := []string{f.worktree, f.adminDir}; !slices.Equal(f.guarded, want) {
		t.Errorf("guard consulted for %v, want %v", f.guarded, want)
	}
}

func TestRetireUnregistersAQueuedDeferralInPlace(t *testing.T) {
	f := newFixture(t)
	job := f.intend()
	tree, index := f.id(f.worktree), f.read(filepath.Join(f.adminDir, "index"))

	f.retire(&job)

	f.retiredInPlace(&job, tree, index)
}

func TestRetireCarriesTheAdminDirectoryAsItIs(t *testing.T) {
	f := newFixture(t)
	job := f.pend()
	tree, index := f.id(f.worktree), f.read(filepath.Join(f.adminDir, "index"))
	empty := f.run(f.repo, "hash-object", "-w", "-t", "tree", "/dev/null")
	kept := []struct{ name, content string }{
		{"MERGE_HEAD", job.Head + "\n"},
		{"AUTO_MERGE", empty + "\n"},
		{filepath.Join("logs", "AUTO_MERGE"), fmt.Sprintf("%040x %s Test <t@example.com> 1700000000 +0000\tmerge\n", 0, empty)},
		{filepath.Join("refs", "bisect", "bad"), job.Head + "\n"},
		{filepath.Join("rebase-merge", "head-name"), "refs/heads/feature\n"},
	}
	for _, k := range kept {
		path := filepath.Join(f.adminDir, k.name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("create %s: %v", filepath.Dir(path), err)
		}
		f.write(path, k.content)
	}

	f.retire(&job)

	if job.Phase != cleanup.PhaseRetired || job.Blocked != nil {
		t.Fatalf("job = phase %s, blockage %+v; want retired in place", job.Phase, job.Blocked)
	}
	f.stored(&job)
	gitDir := cleanup.RetiredGitDirFor(job.Repo, job.ID)
	if got := f.id(gitDir); got != job.Admin {
		t.Errorf("%s identity = %v, want the admin directory %v itself", gitDir, got, job.Admin)
	}
	if got := f.id(f.worktree); got != tree {
		t.Errorf("worktree identity = %v, want %v", got, tree)
	}
	if got := f.read(filepath.Join(gitDir, "index")); got != index {
		t.Errorf("%s/index differs from the index the tree had", gitDir)
	}
	for _, k := range kept {
		if got := f.read(filepath.Join(gitDir, k.name)); got != k.content {
			t.Errorf("%s = %q, want %q carried along", filepath.Join(gitDir, k.name), got, k.content)
		}
	}
	if got := f.listing(); got != f.mainEntry() {
		t.Errorf("worktree list = %q, want only the main worktree", got)
	}
}

func TestRetireMovesAHeldDeferralToWaiting(t *testing.T) {
	f := newFixture(t)
	job := f.intend()
	tree, index, listing := f.id(f.worktree), f.read(filepath.Join(f.adminDir, "index")), f.listing()
	active := f.holdAt(f.worktree)

	f.retire(&job)

	if job.Phase != cleanup.PhaseWaiting || job.Blocked != nil || len(job.Errors) == 0 || job.Errors[len(job.Errors)-1].Message != active.Error() {
		t.Fatalf("job = phase %s, blockage %+v, errors %+v; want waiting with the holder noted", job.Phase, job.Blocked, job.Errors)
	}
	f.stored(&job)
	f.registeredInPlace(&job, tree, listing)

	f.retire(&job)
	if job.Phase != cleanup.PhaseWaiting || job.Blocked != nil {
		t.Fatalf("job still held = phase %s, blockage %+v; want it waiting", job.Phase, job.Blocked)
	}

	f.release()
	f.retire(&job)
	f.retiredInPlace(&job, tree, index)
}

func TestRetireSettlesATreeThatAlreadyLeft(t *testing.T) {
	f := newFixture(t)
	job := f.pend()
	f.run(f.repo, "worktree", "remove", f.worktree)

	f.retire(&job)

	if job.Phase != cleanup.PhaseDone || job.Blocked != nil {
		t.Fatalf("job = phase %s, blockage %+v; want done", job.Phase, job.Blocked)
	}
	f.stored(&job)
}

func TestRetireRefusesAndLeavesTheRegistration(t *testing.T) {
	tests := []struct {
		name  string
		setup func(f *fixture, job *cleanup.Job) (reason, detail string)
	}{
		{"held tree", func(f *fixture, _ *cleanup.Job) (string, string) {
			return "activity", f.holdAt(f.worktree).Error()
		}},
		{"held admin directory", func(f *fixture, _ *cleanup.Job) (string, string) {
			return "activity", f.holdAt(f.adminDir).Error()
		}},
		{"locked", func(f *fixture, _ *cleanup.Job) (string, string) {
			f.run(f.repo, "worktree", "lock", "--reason", "maintenance", f.worktree)
			return "locked", "worktree is locked: maintenance"
		}},
		{"head moved", func(f *fixture, job *cleanup.Job) (string, string) {
			f.write(filepath.Join(f.worktree, "late.txt"), "late\n")
			f.run(f.worktree, "add", "late.txt")
			f.run(f.worktree, "commit", "-q", "-m", "late")
			return "identity", headChanged(job, f.worktree, f.run(f.worktree, "rev-parse", "HEAD"))
		}},
		{"branch switched", func(f *fixture, _ *cleanup.Job) (string, string) {
			f.run(f.worktree, "switch", "-q", "-c", "other")
			return "identity", fmt.Sprintf("branch changed: the job captured %q, %s now holds %q", "feature", f.worktree, "other")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			job := f.pend()
			tree := f.id(f.worktree)
			reason, detail := tt.setup(f, &job)
			listing := f.listing()

			f.retire(&job)

			f.blocked(&job, cleanup.PhasePending, reason, detail)
			f.registeredInPlace(&job, tree, listing)
		})
	}
}

func (f *fixture) crash(job *cleanup.Job, steps int) {
	f.t.Helper()
	if _, err := f.relocator.enter(job, cleanup.PhaseRetiring); err != nil {
		f.t.Fatalf("enter retiring: %v", err)
	}
	if steps >= 1 {
		if detail := repoint(job); detail != "" {
			f.t.Fatalf("repoint: %s", detail)
		}
	}
	if steps >= 2 {
		if err := durable.Rename(job.AdminDir, cleanup.RetiredGitDirFor(job.Repo, job.ID)); err != nil {
			f.t.Fatalf("move the admin directory: %v", err)
		}
	}
}

func TestRetireResumesFromEveryJournaledStep(t *testing.T) {
	tests := []struct {
		name  string
		steps int
	}{
		{"journaled only", 0},
		{"repointed", 1},
		{"unregistered", 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			job := f.pend()
			tree, index := f.id(f.worktree), f.read(filepath.Join(f.adminDir, "index"))
			f.crash(&job, tt.steps)

			f.retire(&job)

			f.retiredInPlace(&job, tree, index)
		})
	}
}

func TestRetireProvesAJournaledRetirementAgain(t *testing.T) {
	tests := []struct {
		name     string
		deferred bool
		steps    int
		setup    func(f *fixture, job *cleanup.Job) (reason, detail string)
	}{
		{"held removal", false, 0, func(f *fixture, _ *cleanup.Job) (string, string) {
			return "activity", f.holdAt(f.worktree).Error()
		}},
		{"held deferral after the pointer moved", true, 1, func(f *fixture, _ *cleanup.Job) (string, string) {
			return "activity", f.holdAt(f.worktree).Error()
		}},
		{"locked after the pointer moved", false, 1, func(f *fixture, _ *cleanup.Job) (string, string) {
			f.write(filepath.Join(f.adminDir, "locked"), "maintenance\n")
			return "locked", "worktree is locked: maintenance"
		}},
		{"branch switched", false, 0, func(f *fixture, _ *cleanup.Job) (string, string) {
			f.run(f.worktree, "switch", "-q", "-c", "other")
			return "identity", fmt.Sprintf("branch changed: the job captured %q, %s now holds %q", "feature", f.worktree, "other")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			var job cleanup.Job
			if tt.deferred {
				job = f.intend()
			} else {
				job = f.pend()
			}
			tree := f.id(f.worktree)
			reason, detail := tt.setup(f, &job)
			listing := f.listing()
			f.crash(&job, tt.steps)

			f.retire(&job)

			f.blocked(&job, cleanup.PhaseRetiring, reason, detail)
			f.registeredInPlace(&job, tree, listing)
		})
	}
}

func TestRetireBlocksALayoutItDidNotLeave(t *testing.T) {
	tests := []struct {
		name    string
		steps   int
		setup   func(f *fixture, job *cleanup.Job) (fragment string)
		pointer func(job *cleanup.Job) string
	}{
		{"retired git dir occupied", 0, func(f *fixture, job *cleanup.Job) string {
			gitDir := cleanup.RetiredGitDirFor(job.Repo, job.ID)
			if err := os.MkdirAll(gitDir, 0o700); err != nil {
				f.t.Fatalf("create %s: %v", gitDir, err)
			}
			return "retired git dir " + gitDir + " is directory "
		}, func(job *cleanup.Job) string { return job.Links.DotGit }},
		{"pointer rewritten", 0, func(f *fixture, _ *cleanup.Job) string {
			f.write(filepath.Join(f.worktree, ".git"), "gitdir: "+f.root+"/elsewhere\n")
			return fmt.Sprintf("reading %q", "gitdir: "+f.root+"/elsewhere\n")
		}, func(*cleanup.Job) string { return "" }},
		{"admin replaced after the pointer moved", 1, func(f *fixture, _ *cleanup.Job) string {
			if err := os.Rename(f.adminDir, f.adminDir+".aside"); err != nil {
				f.t.Fatalf("move %s: %v", f.adminDir, err)
			}
			if err := os.Mkdir(f.adminDir, 0o700); err != nil {
				f.t.Fatalf("create %s: %v", f.adminDir, err)
			}
			f.write(filepath.Join(f.adminDir, "foreign"), "")
			return "admin " + f.adminDir + " is directory "
		}, retiredPointer},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			job := f.pend()
			tree := f.id(f.worktree)
			f.crash(&job, tt.steps)
			fragment := tt.setup(f, &job)
			want := tt.pointer(&job)
			if want == "" {
				want = f.read(filepath.Join(f.worktree, ".git"))
			}

			f.retire(&job)

			f.stuck(&job, cleanup.PhaseRetiring, "reconcile", fragment)
			if got := f.id(f.worktree); got != tree {
				t.Errorf("worktree identity = %v, want %v", got, tree)
			}
			if got := f.read(filepath.Join(f.worktree, ".git")); got != want {
				t.Errorf("%s/.git = %q, want %q left alone", f.worktree, got, want)
			}
			if tt.steps == 0 {
				if got := f.id(f.adminDir); got != job.Admin {
					t.Errorf("admin identity = %v, want %v", got, job.Admin)
				}
				return
			}
			if got := f.id(f.adminDir + ".aside"); got != job.Admin {
				t.Errorf("aside identity = %v, want the captured admin %v left where it was moved", got, job.Admin)
			}
			if got := f.read(filepath.Join(f.adminDir, "foreign")); got != "" {
				t.Errorf("foreign occupant = %q, want it untouched", got)
			}
			f.absent(cleanup.RetiredGitDirFor(job.Repo, job.ID))
		})
	}
}

func quoted(path string) string { return "'" + path + "'" }

func (f *fixture) interpose(job *cleanup.Job, trigger, mutation string) {
	f.t.Helper()
	script, once := filepath.Join(f.root, "interposed-git"), filepath.Join(f.root, "interposed-once")
	body := fmt.Sprintf("#!/bin/sh\n%s \"$@\"\nstatus=$?\ncase \" $* \" in\n*%s*)\n\tif [ ! -e %s ]; then\n\t\t: > %s\n\t\t%s\n\tfi\n\t;;\nesac\nexit $status\n",
		quoted(f.git), quoted(trigger), quoted(once), quoted(once), mutation)
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil { //nolint:gosec // the script stands in for git and is executed
		f.t.Fatalf("write %s: %v", script, err)
	}
	job.Git = script
}

func TestRetireStopsWhenTheAdminIsReplacedBeforeThePointerMoves(t *testing.T) {
	f := newFixture(t)
	job := f.pend()
	tree := f.id(f.worktree)
	aside, foreign := f.adminDir+".aside", filepath.Join(f.adminDir, "foreign")
	f.interpose(&job, "symbolic-ref --quiet --short HEAD",
		"mv "+quoted(f.adminDir)+" "+quoted(aside)+" && mkdir "+quoted(f.adminDir)+" && : > "+quoted(foreign))

	f.retire(&job)

	f.stuck(&job, cleanup.PhaseRetiring, "reconcile", f.adminDir+" is directory ")
	if got := f.id(f.worktree); got != tree {
		t.Errorf("worktree identity = %v, want %v", got, tree)
	}
	if got := f.read(filepath.Join(f.worktree, ".git")); got != job.Links.DotGit {
		t.Errorf("%s/.git = %q, want %q untouched", f.worktree, got, job.Links.DotGit)
	}
	if got := f.id(aside); got != job.Admin {
		t.Errorf("%s = %v, want the captured admin %v left where it was moved", aside, got, job.Admin)
	}
	if got := f.read(foreign); got != "" {
		t.Errorf("%s = %q, want the foreign occupant untouched", foreign, got)
	}
	f.absent(cleanup.RetiredGitDirFor(job.Repo, job.ID))
}

func TestRetireEvictsAForeignDirectoryTheRenameCarried(t *testing.T) {
	tests := []struct {
		name     string
		occupied bool
	}{
		{"back to the vacant admin path", false},
		{"aside when the admin path is taken", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			job := f.pend()
			f.crash(&job, 1)
			gitDir := cleanup.RetiredGitDirFor(job.Repo, job.ID)
			aside := f.adminDir + ".aside"
			if err := os.Rename(f.adminDir, aside); err != nil {
				t.Fatalf("move %s: %v", f.adminDir, err)
			}
			if err := os.Mkdir(gitDir, 0o700); err != nil {
				t.Fatalf("create %s: %v", gitDir, err)
			}
			f.write(filepath.Join(gitDir, "foreign"), "foreign\n")
			foreign := f.id(gitDir)
			if tt.occupied {
				if err := os.Mkdir(f.adminDir, 0o700); err != nil {
					t.Fatalf("create %s: %v", f.adminDir, err)
				}
			}
			moved, err := sight(gitDir)
			if err != nil {
				t.Fatal(err)
			}

			detail := evict(&job, moved)

			f.absent(gitDir)
			if got := f.read(filepath.Join(f.worktree, ".git")); got != retiredPointer(&job) {
				t.Errorf("%s/.git = %q, want it still naming the now empty %s", f.worktree, got, gitDir)
			}
			if got := f.id(aside); got != job.Admin {
				t.Errorf("%s = %v, want the captured admin %v untouched", aside, got, job.Admin)
			}
			home := f.adminDir
			if tt.occupied {
				matches, err := filepath.Glob(filepath.Join(filepath.Dir(gitDir), "."+job.ID+".foreign-*", filepath.Base(f.adminDir)))
				if err != nil || len(matches) != 1 {
					t.Fatalf("foreign set aside at %v, %v; want exactly one", matches, err)
				}
				home = matches[0]
			}
			if got := f.id(home); got != foreign {
				t.Errorf("%s = %v, want the foreign directory %v", home, got, foreign)
			}
			if !strings.Contains(detail, "it is at "+home) || !strings.Contains(detail, "reconcile by hand") {
				t.Errorf("evict() = %q, want it naming %s and asking for an operator", detail, home)
			}
		})
	}
}

func TestRetireRefusesAJobThatIsNotRetirable(t *testing.T) {
	f := newFixture(t)
	job := f.accept()

	if err := f.relocator.Retire(context.Background(), &job); err == nil || job.Phase != cleanup.PhasePrepared {
		t.Fatalf("Retire() on a prepared job = %v at %s, want an error and nothing changed", err, job.Phase)
	}
}

func TestReclaimMovesARetiredTreeToItsPayload(t *testing.T) {
	f := newFixture(t)
	job := f.pend()
	tree, index := f.id(f.worktree), f.read(filepath.Join(f.adminDir, "index"))
	f.retire(&job)
	f.retiredInPlace(&job, tree, index)

	f.advance(&job)

	f.finished(&job)
	f.absent(f.worktree)
	if got := f.id(cleanup.RetiredGitDirFor(job.Repo, job.ID)); got != job.Admin {
		t.Errorf("retired admin identity = %v, want %v before physical deletion", got, job.Admin)
	}
	if got := f.id(job.Payload); got != tree {
		t.Errorf("payload identity = %v, want the tree %v", got, tree)
	}
	if got := f.run(f.repo, "for-each-ref", "--format=%(refname) %(objectname)", cleanup.RecoveryRefPrefix); got != job.RecoveryRef+" "+job.Head {
		t.Errorf("recovery refs = %q, want %s at %s", got, job.RecoveryRef, job.Head)
	}
	if want := []string{f.worktree}; !slices.Equal(f.watchers.retired, want) {
		t.Errorf("watchers retired for %v, want %v", f.watchers.retired, want)
	}
	if got := f.listing(); got != f.mainEntry() {
		t.Errorf("worktree list = %q, want only the main worktree", got)
	}
}

func TestReclaimRefusesARetiredTreeThatChanged(t *testing.T) {
	tests := []struct {
		name  string
		setup func(f *fixture, job *cleanup.Job) (reason, fragment string)
	}{
		{"re-pointed checkout", func(f *fixture, job *cleanup.Job) (string, string) {
			f.write(filepath.Join(f.worktree, ".git"), job.Links.DotGit)
			return "identity", fmt.Sprintf("reads %q", job.Links.DotGit)
		}},
		{"re-registered path", func(f *fixture, _ *cleanup.Job) (string, string) {
			admin := filepath.Join(f.common, "worktrees", "again")
			if err := os.Mkdir(admin, 0o700); err != nil {
				f.t.Fatalf("create %s: %v", admin, err)
			}
			f.write(filepath.Join(admin, "gitdir"), filepath.Join(f.worktree, ".git")+"\n")
			return "identity", "inside the retired tree " + f.worktree
		}},
		{"swapped admin directory", func(f *fixture, job *cleanup.Job) (string, string) {
			gitDir := cleanup.RetiredGitDirFor(job.Repo, job.ID)
			if err := os.Rename(gitDir, gitDir+".moved"); err != nil {
				f.t.Fatalf("move %s: %v", gitDir, err)
			}
			if err := os.Mkdir(gitDir, 0o700); err != nil {
				f.t.Fatalf("create %s: %v", gitDir, err)
			}
			return "identity", gitDir + " is directory "
		}},
		{"head moved", func(f *fixture, job *cleanup.Job) (string, string) {
			f.write(filepath.Join(f.worktree, "late.txt"), "late\n")
			f.run(f.worktree, "add", "late.txt")
			f.run(f.worktree, "commit", "-q", "-m", "late")
			return "identity", headChanged(job, f.worktree, f.run(f.worktree, "rev-parse", "HEAD"))
		}},
		{"initialized submodule", func(f *fixture, job *cleanup.Job) (string, string) {
			modules := filepath.Join(cleanup.RetiredGitDirFor(job.Repo, job.ID), "modules")
			if err := os.MkdirAll(filepath.Join(modules, "sub"), 0o700); err != nil {
				f.t.Fatalf("create %s: %v", modules, err)
			}
			return "submodules", "worktree has initialized submodules under " + modules
		}},
		{"dirty", func(f *fixture, _ *cleanup.Job) (string, string) {
			f.write(filepath.Join(f.worktree, "scratch.txt"), "scratch\n")
			return "dirty", "uncommitted changes: scratch.txt"
		}},
		{"held", func(f *fixture, _ *cleanup.Job) (string, string) {
			return "activity", f.holdAt(f.worktree).Error()
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			job := f.pend()
			tree := f.id(f.worktree)
			f.retire(&job)
			reason, fragment := tt.setup(f, &job)

			f.advance(&job)

			f.stuck(&job, cleanup.PhaseRetired, reason, fragment)
			if got := f.id(f.worktree); got != tree {
				t.Errorf("worktree identity = %v, want the tree left at its path as %v", got, tree)
			}
			f.absent(job.Payload)
			if refs := f.run(f.repo, "for-each-ref", cleanup.RecoveryRefPrefix); refs != "" {
				t.Errorf("recovery refs = %q, want none", refs)
			}
		})
	}
}

func TestReclaimSettlesATreeThatLeftItsPath(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(f *fixture, job *cleanup.Job)
		phase  cleanup.Phase
		reason string
	}{
		{"moved to its payload before a crash", func(f *fixture, job *cleanup.Job) {
			if err := os.Rename(f.worktree, job.Payload); err != nil {
				f.t.Fatalf("move %s: %v", f.worktree, err)
			}
		}, cleanup.PhaseUnregistered, ""},
		{"removed with its admin directory", func(f *fixture, job *cleanup.Job) {
			for _, path := range []string{f.worktree, cleanup.RetiredGitDirFor(job.Repo, job.ID)} {
				if err := os.RemoveAll(path); err != nil {
					f.t.Fatalf("remove %s: %v", path, err)
				}
			}
		}, cleanup.PhaseDone, ""},
		{"moved elsewhere, its admin directory left", func(f *fixture, _ *cleanup.Job) {
			if err := os.Rename(f.worktree, f.worktree+".elsewhere"); err != nil {
				f.t.Fatalf("move %s: %v", f.worktree, err)
			}
		}, cleanup.PhaseRetired, "identity"},
		{"payload landed beside a foreign admin directory", func(f *fixture, job *cleanup.Job) {
			if err := os.Rename(f.worktree, job.Payload); err != nil {
				f.t.Fatalf("move %s: %v", f.worktree, err)
			}
			gitDir := cleanup.RetiredGitDirFor(job.Repo, job.ID)
			if err := os.Rename(gitDir, gitDir+".moved"); err != nil {
				f.t.Fatalf("move %s: %v", gitDir, err)
			}
			if err := os.Mkdir(gitDir, 0o700); err != nil {
				f.t.Fatalf("create %s: %v", gitDir, err)
			}
		}, cleanup.PhaseRetired, "reconcile"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			job := f.pend()
			f.retire(&job)
			gitDir := cleanup.RetiredGitDirFor(job.Repo, job.ID)
			tt.setup(f, &job)

			f.advance(&job)

			if tt.reason != "" {
				f.stuck(&job, tt.phase, tt.reason, gitDir)
				if _, err := os.Lstat(gitDir); err != nil {
					t.Errorf("lstat %s = %v, want it kept", gitDir, err)
				}
				return
			}
			if job.Phase != tt.phase || job.Blocked != nil {
				t.Fatalf("job = phase %s, blockage %+v; want %s", job.Phase, job.Blocked, tt.phase)
			}
			f.stored(&job)
			if tt.phase == cleanup.PhaseDone {
				f.absent(gitDir)
			} else if got := f.id(gitDir); got != job.Admin {
				t.Errorf("retired admin identity = %v, want %v", got, job.Admin)
			}
		})
	}
}
