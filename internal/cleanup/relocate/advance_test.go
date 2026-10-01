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

func TestAdvanceRelocatesAndUnregisters(t *testing.T) {
	f := newFixture(t)
	config := f.read(filepath.Join(f.common, "config"))
	job := f.accept()

	f.advance(&job)

	f.finished(&job)
	f.absent(f.worktree)
	f.absent(job.Registered)
	f.absent(f.adminDir)
	if got, want := f.listing(), f.mainEntry(); got != want {
		t.Errorf("worktree list = %q, want %q", got, want)
	}
	if got := f.id(job.Payload); got != job.Tree {
		t.Errorf("payload identity = %v, want the tree %v", got, job.Tree)
	}
	if got := f.read(filepath.Join(job.Payload, "feature.txt")); got != "feature\n" {
		t.Errorf("payload feature.txt = %q, want %q", got, "feature\n")
	}
	if got, want := f.read(filepath.Join(job.Payload, ".git")), expectedDotGit(job); got != want {
		t.Errorf("payload .git = %q, want %q", got, want)
	}
	if got := f.run(f.repo, "rev-parse", job.RecoveryRef); got != job.Head {
		t.Errorf("recovery ref = %s, want %s", got, job.Head)
	}
	if got := f.run(f.repo, "rev-parse", "refs/heads/feature"); got != job.Head {
		t.Errorf("feature branch = %s, want %s", got, job.Head)
	}
	if got := f.read(filepath.Join(f.common, "config")); got != config {
		t.Errorf("repository config = %q, want it unchanged %q", got, config)
	}
	if strings.Contains(strings.ToLower(config), "relativeworktrees") {
		t.Errorf("repository config %q names relativeWorktrees", config)
	}
	if want := []string{f.worktree, f.worktree, job.Registered}; !reflect.DeepEqual(f.guarded, want) {
		t.Errorf("guard consulted for %v, want %v", f.guarded, want)
	}
	if want := []string{f.worktree}; !reflect.DeepEqual(f.watchers.retired, want) {
		t.Errorf("watchers retired %v, want %v", f.watchers.retired, want)
	}
	if want := []string{f.layout.JobDir(job.ID)}; !reflect.DeepEqual(f.watchers.checked, want) {
		t.Errorf("quarantine checked %v, want %v", f.watchers.checked, want)
	}
}

func TestAdvanceIsANoOpOnceUnregistered(t *testing.T) {
	f := newFixture(t)
	job := f.accept()
	f.advance(&job)
	record := f.read(f.layout.RecordPath(job.ID))
	guarded := len(f.guarded)
	marker := f.trap(&job)
	want := job

	f.advance(&job)

	if !reflect.DeepEqual(job, want) {
		t.Errorf("job = %+v, want it unchanged %+v", job, want)
	}
	if got := f.read(f.layout.RecordPath(job.ID)); got != record {
		t.Errorf("record = %q, want it unchanged %q", got, record)
	}
	if len(f.guarded) != guarded {
		t.Errorf("guard consulted %d more times, want none", len(f.guarded)-guarded)
	}
	f.absent(marker)
}

func TestAdvanceResumesAPreparedJobFromItsRecord(t *testing.T) {
	f := newFixture(t)
	accepted := f.accept()
	jobs, _, err := f.journal.Load()
	if err != nil || len(jobs) != 1 {
		t.Fatalf("Load() = %d jobs, error %v, want 1 job", len(jobs), err)
	}
	job := jobs[0]

	f.advance(&job)

	f.finished(&job)
	if job.ID != accepted.ID {
		t.Errorf("resumed job %s, want %s", job.ID, accepted.ID)
	}
	f.absent(f.worktree)
	if got := f.id(job.Payload); got != job.Tree {
		t.Errorf("payload identity = %v, want the tree %v", got, job.Tree)
	}
}

func TestAdvanceReconcilesATornMove(t *testing.T) {
	tests := []struct {
		name        string
		adminGitdir func(job cleanup.Job) string
	}{
		{"neither link rewritten", func(job cleanup.Job) string { return job.Links.AdminGitdir }},
		{"only the admin link rewritten", expectedAdminGitdir},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			job := f.accept()
			if err := os.Rename(f.worktree, job.Registered); err != nil {
				t.Fatalf("rename: %v", err)
			}
			f.write(filepath.Join(f.adminDir, "gitdir"), tt.adminGitdir(job))
			_ = f.holdAt(job.Registered)

			f.advance(&job)

			if job.Phase != cleanup.PhaseMoved {
				t.Fatalf("phase = %s, want moved", job.Phase)
			}
			if got, want := f.read(filepath.Join(job.Registered, ".git")), expectedDotGit(job); got != want {
				t.Errorf("registered .git = %q, want %q", got, want)
			}
			if got, want := f.read(filepath.Join(f.adminDir, "gitdir")), expectedAdminGitdir(job); got != want {
				t.Errorf("admin gitdir = %q, want %q", got, want)
			}
			if len(f.watchers.retired) != 0 {
				t.Errorf("watchers retired %v for a tree that had already left, want none", f.watchers.retired)
			}

			f.release()
			job.Blocked = nil
			f.advance(&job)

			f.finished(&job)
			f.absent(f.adminDir)
			if got, want := f.listing(), f.mainEntry(); got != want {
				t.Errorf("worktree list = %q, want %q", got, want)
			}
			if got := f.id(job.Payload); got != job.Tree {
				t.Errorf("payload identity = %v, want the tree %v", got, job.Tree)
			}
		})
	}
}

func TestAdvanceBlocksOnAForeignAdminLink(t *testing.T) {
	f := newFixture(t)
	job := f.accept()
	if err := os.Rename(f.worktree, job.Registered); err != nil {
		t.Fatalf("rename: %v", err)
	}
	foreign := filepath.Join(f.root, "foreign", ".git") + "\n"
	f.write(filepath.Join(f.adminDir, "gitdir"), foreign)
	marker := f.trap(&job)

	f.advance(&job)

	f.blocked(&job, cleanup.PhasePrepared, "reconcile", fmt.Sprintf(
		"%s reads %q, neither the captured %q nor the expected %q",
		filepath.Join(f.adminDir, "gitdir"), foreign, job.Links.AdminGitdir, expectedAdminGitdir(job),
	))
	if got := f.read(filepath.Join(job.Registered, ".git")); got != job.Links.DotGit {
		t.Errorf("registered .git = %q, want the captured %q", got, job.Links.DotGit)
	}
	if got := f.read(filepath.Join(f.adminDir, "gitdir")); got != foreign {
		t.Errorf("admin gitdir = %q, want it untouched %q", got, foreign)
	}
	if got := f.id(job.Registered); got != job.Tree {
		t.Errorf("registered identity = %v, want the tree %v", got, job.Tree)
	}
	f.absent(job.Payload)
	f.absent(marker)
}

func TestAdvanceResumesAfterTheDetachingRename(t *testing.T) {
	f := newFixture(t)
	job := f.accept()
	f.park(&job)
	if err := os.Rename(job.Registered, job.Payload); err != nil {
		t.Fatalf("rename: %v", err)
	}
	guarded := len(f.guarded)

	f.advance(&job)

	f.finished(&job)
	f.absent(f.adminDir)
	if got, want := f.listing(), f.mainEntry(); got != want {
		t.Errorf("worktree list = %q, want %q", got, want)
	}
	if got := f.id(job.Payload); got != job.Tree {
		t.Errorf("payload identity = %v, want the tree %v", got, job.Tree)
	}
	if len(f.guarded) != guarded {
		t.Errorf("guard consulted %d more times for an already detached tree, want none", len(f.guarded)-guarded)
	}
}

func TestAdvanceLeavesAnAlreadyPrunedRegistration(t *testing.T) {
	f := newFixture(t)
	job := f.accept()
	f.park(&job)
	if err := os.Rename(job.Registered, job.Payload); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := os.RemoveAll(f.adminDir); err != nil {
		t.Fatalf("remove admin dir: %v", err)
	}
	job.Phase = cleanup.PhaseDetached
	marker := f.trap(&job)

	f.advance(&job)

	f.finished(&job)
	f.absent(marker)
	if got := f.id(job.Payload); got != job.Tree {
		t.Errorf("payload identity = %v, want the tree %v", got, job.Tree)
	}
}

func TestAdvanceLeavesARegistrationThatNamesAnotherPath(t *testing.T) {
	f := newFixture(t)
	job := f.accept()
	f.park(&job)
	if err := os.Rename(job.Registered, job.Payload); err != nil {
		t.Fatalf("rename: %v", err)
	}
	foreign := filepath.Join(f.root, "foreign", ".git") + "\n"
	f.write(filepath.Join(f.adminDir, "gitdir"), foreign)
	job.Phase = cleanup.PhaseDetached
	marker := f.trap(&job)

	f.advance(&job)

	f.finished(&job)
	f.absent(marker)
	if got := f.read(filepath.Join(f.adminDir, "gitdir")); got != foreign {
		t.Errorf("admin gitdir = %q, want it untouched %q", got, foreign)
	}
}

func TestAdvanceNeverRunsGitOverAnOccupiedRegisteredPath(t *testing.T) {
	f := newFixture(t)
	job := f.accept()
	f.park(&job)
	if err := os.Rename(job.Registered, job.Payload); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := os.Mkdir(job.Registered, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	f.write(filepath.Join(job.Registered, "keep.txt"), "keep\n")
	squatter := f.id(job.Registered)
	job.Phase = cleanup.PhaseDetached
	marker := f.trap(&job)

	f.advance(&job)

	f.blocked(&job, cleanup.PhaseDetached, "reconcile", fmt.Sprintf(
		"registered %s is directory %s; payload %s is directory %s; admin %s is directory %s; the job captured tree %s and admin %s",
		job.Registered, idText(squatter), job.Payload, idText(job.Tree), job.AdminDir, idText(job.Admin),
		idText(job.Tree), idText(job.Admin),
	))
	f.absent(marker)
	if got := f.id(job.Registered); got != squatter {
		t.Errorf("registered identity = %v, want the squatter %v", got, squatter)
	}
	if got := f.read(filepath.Join(job.Registered, "keep.txt")); got != "keep\n" {
		t.Errorf("keep.txt = %q, want %q", got, "keep\n")
	}
	if got := f.id(f.adminDir); got != job.Admin {
		t.Errorf("admin identity = %v, want %v", got, job.Admin)
	}
	if got := f.id(job.Payload); got != job.Tree {
		t.Errorf("payload identity = %v, want the tree %v", got, job.Tree)
	}
}

func TestAdvanceLeavesAReplacementWorktreeAtTheOriginalPath(t *testing.T) {
	f := newFixture(t)
	job := f.accept()
	f.park(&job)
	f.run(f.repo, "worktree", "add", "-q", "-b", "replacement", f.worktree)
	f.write(filepath.Join(f.worktree, "new.txt"), "new\n")
	replacement := f.id(f.worktree)
	dotGit := f.read(filepath.Join(f.worktree, ".git"))

	f.advance(&job)

	f.finished(&job)
	wantListing := f.mainEntry() + fmt.Sprintf("\n\nworktree %s\nHEAD %s\nbranch refs/heads/replacement", f.worktree, f.run(f.repo, "rev-parse", "refs/heads/main"))
	if got := f.listing(); got != wantListing {
		t.Errorf("worktree list = %q, want %q", got, wantListing)
	}
	if got := f.id(f.worktree); got != replacement {
		t.Errorf("replacement identity = %v, want %v", got, replacement)
	}
	if got := f.read(filepath.Join(f.worktree, ".git")); got != dotGit {
		t.Errorf("replacement .git = %q, want it untouched %q", got, dotGit)
	}
	if got := f.run(f.worktree, "status", "--porcelain"); got != "?? new.txt" {
		t.Errorf("replacement status = %q, want %q", got, "?? new.txt")
	}
	if got := f.id(job.Payload); got != job.Tree {
		t.Errorf("payload identity = %v, want the tree %v", got, job.Tree)
	}
}

func TestAdvanceLeavesASymlinkAtTheOriginalPath(t *testing.T) {
	f := newFixture(t)
	job := f.accept()
	f.park(&job)
	target := filepath.Join(f.root, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	f.write(filepath.Join(target, "precious.txt"), "precious\n")
	if err := os.Symlink(target, f.worktree); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	f.advance(&job)

	f.finished(&job)
	if got, err := os.Readlink(f.worktree); err != nil || got != target {
		t.Errorf("Readlink(original) = %q, %v, want %q", got, err, target)
	}
	if got := f.read(filepath.Join(target, "precious.txt")); got != "precious\n" {
		t.Errorf("precious.txt = %q, want %q", got, "precious\n")
	}
	if got, want := f.listing(), f.mainEntry(); got != want {
		t.Errorf("worktree list = %q, want %q", got, want)
	}
}

func TestAdvanceBlocksOnLateActivityWithTheTreeParked(t *testing.T) {
	f := newFixture(t)
	job := f.accept()
	active := f.holdAt(job.Registered)

	f.advance(&job)

	f.blocked(&job, cleanup.PhaseMoved, "activity", fmt.Sprintf(
		"%s; the tree is parked at %s, still registered; `git worktree move %s %s` restores it",
		active.Error(), job.Registered, job.Registered, job.Original,
	))
	f.absent(f.worktree)
	f.absent(job.Payload)
	if got := f.id(job.Registered); got != job.Tree {
		t.Errorf("registered identity = %v, want the tree %v", got, job.Tree)
	}
	wantListing := f.mainEntry() + fmt.Sprintf("\n\nworktree %s\nHEAD %s\nbranch refs/heads/feature", job.Registered, job.Head)
	if got := f.listing(); got != wantListing {
		t.Errorf("worktree list = %q, want %q", got, wantListing)
	}

	f.run(f.repo, "worktree", "move", job.Registered, job.Original)
	if got := f.id(f.worktree); got != job.Tree {
		t.Errorf("restored identity = %v, want the tree %v", got, job.Tree)
	}
	if got := f.run(f.worktree, "status", "--porcelain"); got != "" {
		t.Errorf("restored status = %q, want clean", got)
	}
}

func TestAdvanceBlocksWhenTheParkedTreeCannotBeVerifiedIdle(t *testing.T) {
	f := newFixture(t)
	job := f.accept()
	f.guard = func(_ context.Context, worktree string) error {
		if worktree == job.Registered {
			return errors.New("process table unreadable")
		}
		return nil
	}

	f.advance(&job)

	f.blocked(&job, cleanup.PhaseMoved, "activity", fmt.Sprintf(
		"could not verify that %s is idle: process table unreadable; the tree is parked at %s, still registered; `git worktree move %s %s` restores it",
		job.Registered, job.Registered, job.Registered, job.Original,
	))
	if got := f.id(job.Registered); got != job.Tree {
		t.Errorf("registered identity = %v, want the tree %v", got, job.Tree)
	}
}

func TestAdvanceBlocksWhenTheParkedHeadMoved(t *testing.T) {
	f := newFixture(t)
	job := f.accept()
	f.park(&job)
	f.run(job.Registered, "commit", "-q", "--allow-empty", "-m", "late")
	late := f.run(job.Registered, "rev-parse", "HEAD")

	f.advance(&job)

	f.blocked(&job, cleanup.PhaseMoved, "identity", fmt.Sprintf(
		"HEAD changed: the job captured %q, %s now reads %q", job.Head, job.Registered, late,
	))
	if got := f.id(job.Registered); got != job.Tree {
		t.Errorf("registered identity = %v, want the tree %v", got, job.Tree)
	}
	f.absent(job.Payload)
}

func TestAdvanceBlocksWhenTheOriginalWasSwapped(t *testing.T) {
	f := newFixture(t)
	job := f.accept()
	aside := filepath.Join(f.root, "aside")
	if err := os.Rename(f.worktree, aside); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := os.Mkdir(f.worktree, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	f.write(filepath.Join(f.worktree, "mine.txt"), "mine\n")
	replacement := f.id(f.worktree)

	f.advance(&job)

	f.blocked(&job, cleanup.PhasePrepared, "reconcile", fmt.Sprintf(
		"original %s is directory %s; registered %s is absent; payload %s is absent; admin %s is directory %s; the job captured tree %s and admin %s",
		job.Original, idText(replacement), job.Registered, job.Payload, job.AdminDir, idText(job.Admin),
		idText(job.Tree), idText(job.Admin),
	))
	if got := f.id(f.worktree); got != replacement {
		t.Errorf("replacement identity = %v, want %v", got, replacement)
	}
	if got := f.read(filepath.Join(f.worktree, "mine.txt")); got != "mine\n" {
		t.Errorf("mine.txt = %q, want %q", got, "mine\n")
	}
	if got := f.id(aside); got != job.Tree {
		t.Errorf("set-aside identity = %v, want the tree %v", got, job.Tree)
	}
	if len(f.watchers.retired) != 0 {
		t.Errorf("watchers retired %v, want none", f.watchers.retired)
	}
	if refs := f.run(f.repo, "for-each-ref", cleanup.RecoveryRefPrefix); refs != "" {
		t.Errorf("recovery refs = %q, want none", refs)
	}
}

func TestAdvanceBlocksBeforeTheMove(t *testing.T) {
	tests := []struct {
		name       string
		arrange    func(f *fixture) (detail string)
		reason     string
		wantRetire bool
	}{
		{"watchers refuse", func(f *fixture) string {
			f.watchers.retire = func(context.Context, string) error { return errors.New("watchman refused to drop the root") }
			return "watchman refused to drop the root"
		}, "watchers", true},
		{"the watchers cannot be named", func(f *fixture) string {
			f.watchers.retiring = func(context.Context, string) ([]cleanup.ProcessID, error) {
				return nil, errors.New("watcher registry unreadable")
			}
			return "name the watchers of " + f.worktree + ": watcher registry unreadable"
		}, "watchers", false},
		{"quarantine refuses", func(f *fixture) string {
			f.watchers.quarantine = func(context.Context, string) error { return errors.New("job folder sits under a watched root") }
			return "job folder sits under a watched root"
		}, "quarantine", true},
		{"tree became active", func(f *fixture) string {
			return f.holdAt(f.worktree).Error()
		}, "activity", true},
		{"activity is unclear", func(f *fixture) string {
			f.guard = func(context.Context, string) error { return errors.New("process table unreadable") }
			return "could not verify that " + f.worktree + " is idle: process table unreadable"
		}, "activity", true},
		{"a worktree was nested inside", func(f *fixture) string {
			inner := filepath.Join(f.worktree, "inner")
			f.run(f.repo, "worktree", "add", "-q", "-b", "inner", inner)
			return fmt.Sprintf("worktree %s is registered inside %s", inner, f.worktree)
		}, "reconcile", false},
		{"the worktree was locked", func(f *fixture) string {
			f.run(f.repo, "worktree", "lock", "--reason", "late lock", f.worktree)
			return fmt.Sprintf(
				"git --git-dir=%s -c worktree.useRelativePaths=false worktree move %s %s: exit status 128: fatal: cannot move a locked working tree, lock reason: late lock\nuse 'move -f -f' to override or unlock first",
				f.common, f.worktree, "%s",
			)
		}, "git", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			job := f.accept()
			detail := tt.arrange(f)
			if strings.Contains(detail, "%s") {
				detail = fmt.Sprintf(detail, job.Registered)
			}

			f.advance(&job)

			f.blocked(&job, cleanup.PhasePrepared, tt.reason, detail)
			if got := f.id(f.worktree); got != job.Tree {
				t.Errorf("worktree identity = %v, want the tree %v", got, job.Tree)
			}
			f.absent(job.Registered)
			f.absent(job.Payload)
			if got := f.read(filepath.Join(f.worktree, ".git")); got != job.Links.DotGit {
				t.Errorf("worktree .git = %q, want the captured %q", got, job.Links.DotGit)
			}
			if got := f.read(filepath.Join(f.adminDir, "gitdir")); got != job.Links.AdminGitdir {
				t.Errorf("admin gitdir = %q, want the captured %q", got, job.Links.AdminGitdir)
			}
			if retired := len(f.watchers.retired) != 0; retired != tt.wantRetire {
				t.Errorf("watchers retired = %t, want %t", retired, tt.wantRetire)
			}
		})
	}
}

func TestAdvanceFinishesGitRewritesUnderACancelledContext(t *testing.T) {
	f := newFixture(t)
	job, err := f.relocator.Accept(context.Background(), 1, f.request(true))
	if err != nil {
		t.Fatalf("Accept() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.guard = func(context.Context, string) error {
		cancel()
		return nil
	}

	if err := f.relocator.Advance(ctx, &job); !errors.Is(err, context.Canceled) {
		t.Fatalf("Advance() error = %v, want context.Canceled", err)
	}

	if job.Phase != cleanup.PhaseMoved || job.Blocked != nil {
		t.Fatalf("job is at %s with blockage %+v, want moved and unblocked", job.Phase, job.Blocked)
	}
	f.stored(&job)
	f.absent(f.worktree)
	if got := f.id(job.Registered); got != job.Tree {
		t.Errorf("registered identity = %v, want the tree %v", got, job.Tree)
	}
	if got, want := f.read(filepath.Join(f.adminDir, "gitdir")), expectedAdminGitdir(job); got != want {
		t.Errorf("admin gitdir = %q, want %q", got, want)
	}
	if got := f.run(f.repo, "rev-parse", job.RecoveryRef); got != job.Head {
		t.Errorf("recovery ref = %s, want %s", got, job.Head)
	}
}

func TestAdvanceDeferredWaitsThenCompletes(t *testing.T) {
	f := newFixture(t)
	job := f.intend()
	if job.Phase != cleanup.PhaseQueued {
		t.Fatalf("Intend() phase = %s, want queued", job.Phase)
	}
	active := f.holdAt(f.worktree)

	f.advance(&job)

	if job.Phase != cleanup.PhaseWaiting || job.Blocked != nil {
		t.Fatalf("job is at %s with blockage %+v, want waiting and unblocked", job.Phase, job.Blocked)
	}
	if want := []cleanup.JobError{{At: f.clock, Phase: cleanup.PhaseWaiting, Message: active.Error()}}; !reflect.DeepEqual(job.Errors, want) {
		t.Errorf("errors = %+v, want %+v", job.Errors, want)
	}
	f.stored(&job)
	if got := f.id(f.worktree); got != job.Tree {
		t.Errorf("worktree identity = %v, want the tree %v", got, job.Tree)
	}
	f.absent(job.Registered)
	if len(f.watchers.retired) != 0 {
		t.Errorf("watchers retired %v while waiting, want none", f.watchers.retired)
	}

	record := f.read(f.layout.RecordPath(job.ID))
	f.advance(&job)
	if got := f.read(f.layout.RecordPath(job.ID)); got != record {
		t.Errorf("record after a still-held recheck = %q, want it unchanged %q", got, record)
	}
	if job.Phase != cleanup.PhaseWaiting || len(job.Errors) != 1 {
		t.Errorf("job is at %s with %d errors, want waiting with 1", job.Phase, len(job.Errors))
	}

	f.release()
	f.advance(&job)

	f.finished(&job)
	f.absent(f.worktree)
	if got, want := f.listing(), f.mainEntry(); got != want {
		t.Errorf("worktree list = %q, want %q", got, want)
	}
	if got := f.run(f.repo, "rev-parse", job.RecoveryRef); got != job.Head {
		t.Errorf("recovery ref = %s, want %s", got, job.Head)
	}
}

func TestAdvanceDeferredBlocks(t *testing.T) {
	tests := []struct {
		name    string
		arrange func(f *fixture, job cleanup.Job) (detail string)
		reason  string
	}{
		{"a commit landed after the intent", func(f *fixture, job cleanup.Job) string {
			f.run(f.worktree, "commit", "-q", "--allow-empty", "-m", "late")
			return fmt.Sprintf("HEAD changed: the job captured %q, %s now reads %q", job.Head, f.worktree, f.run(f.worktree, "rev-parse", "HEAD"))
		}, "identity"},
		{"the tree is dirty", func(f *fixture, _ cleanup.Job) string {
			f.write(filepath.Join(f.worktree, "scratch.txt"), "scratch\n")
			return "uncommitted changes: scratch.txt"
		}, "dirty"},
		{"activity is unclear", func(f *fixture, _ cleanup.Job) string {
			f.guard = func(context.Context, string) error { return errors.New("process table unreadable") }
			return "could not verify that " + f.worktree + " is idle: process table unreadable"
		}, "activity"},
		{"the admin link was rewritten", func(f *fixture, job cleanup.Job) string {
			f.write(filepath.Join(f.adminDir, "gitdir"), "../../../wt/.git\n")
			return fmt.Sprintf("%s reads %q, want %q", filepath.Join(f.adminDir, "gitdir"), "../../../wt/.git\n", job.Links.AdminGitdir)
		}, "identity"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			job := f.intend()
			detail := tt.arrange(f, job)

			f.advance(&job)

			f.blocked(&job, cleanup.PhaseQueued, tt.reason, detail)
			if got := f.id(f.worktree); got != job.Tree {
				t.Errorf("worktree identity = %v, want the tree %v", got, job.Tree)
			}
			f.absent(job.Registered)
			if len(f.watchers.retired) != 0 {
				t.Errorf("watchers retired %v, want none", f.watchers.retired)
			}
		})
	}
}

func TestAdvanceMovesARelativelyLinkedWorktreeToAbsoluteLinks(t *testing.T) {
	f := newFixture(t)
	relative := filepath.Join(f.root, "rel")
	adminDir := filepath.Join(f.common, "worktrees", "rel")
	f.run(f.repo, "config", "worktree.useRelativePaths", "true")
	f.run(f.repo, "worktree", "add", "-q", "-b", "relative", relative)
	config := f.read(filepath.Join(f.common, "config"))
	if !strings.Contains(strings.ToLower(config), "relativeworktrees = true") {
		t.Fatalf("repository config %q does not enable relativeWorktrees", config)
	}

	job, err := f.relocator.Accept(context.Background(), 1, cleanup.Request{Worktree: relative, Git: f.git})
	if err != nil {
		t.Fatalf("Accept() error = %v", err)
	}
	wantLinks := cleanup.Links{DotGit: "gitdir: ../repo/.git/worktrees/rel\n", AdminGitdir: "../../../../rel/.git\n"}
	if job.Links != wantLinks {
		t.Errorf("captured links = %+v, want %+v", job.Links, wantLinks)
	}
	if job.AdminDir != adminDir || job.Repo != f.common {
		t.Errorf("captured admin %s in repo %s, want %s in %s", job.AdminDir, job.Repo, adminDir, f.common)
	}

	f.park(&job)
	if got, want := f.read(filepath.Join(job.Registered, ".git")), "gitdir: "+adminDir+"\n"; got != want {
		t.Errorf("registered .git = %q, want %q", got, want)
	}
	if got, want := f.read(filepath.Join(adminDir, "gitdir")), job.Registered+"/.git\n"; got != want {
		t.Errorf("admin gitdir = %q, want %q", got, want)
	}

	f.advance(&job)

	f.finished(&job)
	f.absent(relative)
	f.absent(adminDir)
	if got := f.id(job.Payload); got != job.Tree {
		t.Errorf("payload identity = %v, want the tree %v", got, job.Tree)
	}
	if got := f.read(filepath.Join(f.common, "config")); got != config {
		t.Errorf("repository config = %q, want it unchanged %q", got, config)
	}
	wantListing := f.mainEntry() + fmt.Sprintf("\n\nworktree %s\nHEAD %s\nbranch refs/heads/feature", f.worktree, f.run(f.worktree, "rev-parse", "HEAD"))
	if got := f.listing(); got != wantListing {
		t.Errorf("worktree list = %q, want %q", got, wantListing)
	}
}

func TestAdvanceThroughASymlinkedLayoutRoot(t *testing.T) {
	f := newFixture(t)
	state, link := filepath.Join(f.root, "real-state"), filepath.Join(f.root, "linked-state")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink(state, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	f.open(link)
	job := f.accept()
	if want := filepath.Join(link, "jobs", job.ID, "registered"); job.Registered != want {
		t.Fatalf("Registered = %s, want %s", job.Registered, want)
	}

	f.park(&job)
	if got, want := f.read(filepath.Join(f.adminDir, "gitdir")), filepath.Join(state, "jobs", job.ID, "registered", ".git")+"\n"; got != want {
		t.Errorf("admin gitdir = %q, want %q", got, want)
	}
	f.advance(&job)

	f.finished(&job)
	f.absent(f.worktree)
	f.absent(f.adminDir)
	if got := f.id(filepath.Join(state, "jobs", job.ID, "payload")); got != job.Tree {
		t.Errorf("payload identity = %v, want the tree %v", got, job.Tree)
	}
}

func TestAdvanceRechecksDirtinessBeforeAnIrreversibleStep(t *testing.T) {
	tests := []struct {
		name    string
		arrange func(f *fixture, job *cleanup.Job) (tree string)
		phase   cleanup.Phase
	}{
		{"resuming prepared before the move", func(f *fixture, _ *cleanup.Job) string {
			return f.worktree
		}, cleanup.PhasePrepared},
		{"resuming moved before the rename", func(f *fixture, job *cleanup.Job) string {
			f.park(job)
			return job.Registered
		}, cleanup.PhaseMoved},
		{"resuming a torn move", func(f *fixture, job *cleanup.Job) string {
			if err := os.Rename(f.worktree, job.Registered); err != nil {
				f.t.Fatalf("rename: %v", err)
			}
			return job.Registered
		}, cleanup.PhaseMoved},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			job := f.accept()
			tree := tt.arrange(f, &job)
			f.write(filepath.Join(tree, "scratch.txt"), "scratch\n")

			f.advance(&job)

			detail := "uncommitted changes: scratch.txt"
			if tt.phase == cleanup.PhaseMoved {
				detail = fmt.Sprintf(
					"%s; the tree is parked at %s, still registered; `git worktree move %s %s` restores it",
					detail, job.Registered, job.Registered, job.Original,
				)
			}
			f.blocked(&job, tt.phase, "dirty", detail)
			if got := f.id(tree); got != job.Tree {
				t.Errorf("tree identity at %s = %v, want the tree %v", tree, got, job.Tree)
			}
			if got := f.read(filepath.Join(tree, "scratch.txt")); got != "scratch\n" {
				t.Errorf("scratch.txt = %q, want %q", got, "scratch\n")
			}
			f.absent(job.Payload)
			if tt.phase != cleanup.PhasePrepared {
				return
			}
			f.absent(job.Registered)
			if refs := f.run(f.repo, "for-each-ref", cleanup.RecoveryRefPrefix); refs != "" {
				t.Errorf("recovery refs = %q, want none", refs)
			}
			if got := f.read(filepath.Join(f.adminDir, "gitdir")); got != job.Links.AdminGitdir {
				t.Errorf("admin gitdir = %q, want the captured %q", got, job.Links.AdminGitdir)
			}
		})
	}
}

func TestAdvanceForceCarriesEditsMadeAfterAccept(t *testing.T) {
	tests := []struct {
		name    string
		arrange func(f *fixture, job *cleanup.Job) (tree string)
	}{
		{"before the move", func(f *fixture, _ *cleanup.Job) string { return f.worktree }},
		{"before the rename", func(f *fixture, job *cleanup.Job) string {
			f.park(job)
			return job.Registered
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			job, err := f.relocator.Accept(context.Background(), 1, f.request(true))
			if err != nil {
				t.Fatalf("Accept() error = %v", err)
			}
			f.write(filepath.Join(tt.arrange(f, &job), "scratch.txt"), "scratch\n")

			f.advance(&job)

			f.finished(&job)
			if got := f.id(job.Payload); got != job.Tree {
				t.Errorf("payload identity = %v, want the tree %v", got, job.Tree)
			}
			if got := f.read(filepath.Join(job.Payload, "scratch.txt")); got != "scratch\n" {
				t.Errorf("payload scratch.txt = %q, want %q", got, "scratch\n")
			}
		})
	}
}

func TestAdvanceBlocksOnAHalfDeletedRegistration(t *testing.T) {
	f := newFixture(t)
	job := f.accept()
	f.park(&job)
	if err := os.Rename(job.Registered, job.Payload); err != nil {
		t.Fatalf("rename: %v", err)
	}
	adminGitdir := filepath.Join(f.adminDir, "gitdir")
	if err := os.Remove(adminGitdir); err != nil {
		t.Fatalf("remove admin gitdir: %v", err)
	}
	job.Phase = cleanup.PhaseDetached
	marker := f.trap(&job)

	f.advance(&job)

	f.blocked(&job, cleanup.PhaseDetached, "reconcile", fmt.Sprintf(
		"admin %s is still the directory %s the job captured but %s is gone: an interrupted removal left the registration half-deleted; remove %s to finish dropping it",
		f.adminDir, idText(job.Admin), adminGitdir, f.adminDir,
	))
	f.absent(marker)
	if got := f.id(f.adminDir); got != job.Admin {
		t.Errorf("admin identity = %v, want %v", got, job.Admin)
	}
	if got := f.read(filepath.Join(f.adminDir, "HEAD")); got != "ref: refs/heads/feature\n" {
		t.Errorf("admin HEAD = %q, want it untouched", got)
	}
	if got := f.id(job.Payload); got != job.Tree {
		t.Errorf("payload identity = %v, want the tree %v", got, job.Tree)
	}

	if err := os.RemoveAll(f.adminDir); err != nil {
		t.Fatalf("remove admin dir: %v", err)
	}
	job.Blocked = nil
	f.advance(&job)

	f.finished(&job)
	f.absent(marker)
}

func TestAdvanceRecoversWithoutTheReleasedOriginalPath(t *testing.T) {
	tests := []struct {
		name    string
		arrange func(f *fixture, job *cleanup.Job)
	}{
		{"a torn move at prepared", func(f *fixture, job *cleanup.Job) {
			if err := os.Rename(job.Original, job.Registered); err != nil {
				f.t.Fatalf("rename: %v", err)
			}
		}},
		{"parked at moved", func(f *fixture, job *cleanup.Job) { f.park(job) }},
		{"detached", func(f *fixture, job *cleanup.Job) {
			f.park(job)
			if err := os.Rename(job.Registered, job.Payload); err != nil {
				f.t.Fatalf("rename: %v", err)
			}
			job.Phase = cleanup.PhaseDetached
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			nest := filepath.Join(f.root, "nest")
			deep := filepath.Join(nest, "deep")
			f.run(f.repo, "worktree", "add", "-q", "-b", "deep", deep)
			job, err := f.relocator.Accept(context.Background(), 1, cleanup.Request{Worktree: deep, Git: f.git})
			if err != nil {
				t.Fatalf("Accept() error = %v", err)
			}
			tt.arrange(f, &job)
			if err := os.Remove(nest); err != nil {
				t.Fatalf("remove the emptied parent: %v", err)
			}
			f.write(nest, "not a directory\n")

			f.advance(&job)

			f.finished(&job)
			if got := f.read(nest); got != "not a directory\n" {
				t.Errorf("the file at the original's parent = %q, want it untouched", got)
			}
			if got := f.id(job.Payload); got != job.Tree {
				t.Errorf("payload identity = %v, want the tree %v", got, job.Tree)
			}
			f.absent(job.AdminDir)
			wantListing := f.mainEntry() + fmt.Sprintf("\n\nworktree %s\nHEAD %s\nbranch refs/heads/feature", f.worktree, f.run(f.worktree, "rev-parse", "HEAD"))
			if got := f.listing(); got != wantListing {
				t.Errorf("worktree list = %q, want %q", got, wantListing)
			}
		})
	}
}

func TestAdvanceReturnsTheContextErrorWithoutBlocking(t *testing.T) {
	interrupted := func(f *fixture, cancel context.CancelFunc) {
		f.guard = func(ctx context.Context, _ string) error {
			cancel()
			return ctx.Err()
		}
	}
	cancelledUnderGit := func(f *fixture, cancel context.CancelFunc) {
		f.guard = func(context.Context, string) error {
			cancel()
			return nil
		}
	}
	tests := []struct {
		name     string
		deferred bool
		arrange  func(f *fixture, job *cleanup.Job, cancel context.CancelFunc)
		tree     func(f *fixture, job cleanup.Job) string
	}{
		{"cancelled before the call", false, func(_ *fixture, _ *cleanup.Job, cancel context.CancelFunc) {
			cancel()
		}, func(f *fixture, _ cleanup.Job) string { return f.worktree }},
		{"the guard is interrupted before the move", false, func(f *fixture, _ *cleanup.Job, cancel context.CancelFunc) {
			interrupted(f, cancel)
		}, func(f *fixture, _ cleanup.Job) string { return f.worktree }},
		{"git status is interrupted before the move", false, func(f *fixture, _ *cleanup.Job, cancel context.CancelFunc) {
			cancelledUnderGit(f, cancel)
		}, func(f *fixture, _ cleanup.Job) string { return f.worktree }},
		{"the guard is interrupted while deferred", true, func(f *fixture, _ *cleanup.Job, cancel context.CancelFunc) {
			interrupted(f, cancel)
		}, func(f *fixture, _ cleanup.Job) string { return f.worktree }},
		{"git rev-parse is interrupted while deferred", true, func(f *fixture, _ *cleanup.Job, cancel context.CancelFunc) {
			cancelledUnderGit(f, cancel)
		}, func(f *fixture, _ cleanup.Job) string { return f.worktree }},
		{"a holder is reported as the context is cancelled", true, func(f *fixture, _ *cleanup.Job, cancel context.CancelFunc) {
			active := f.holdAt(f.worktree)
			f.guard = func(context.Context, string) error {
				cancel()
				return active
			}
		}, func(f *fixture, _ cleanup.Job) string { return f.worktree }},
		{"the guard is interrupted with the tree parked", false, func(f *fixture, job *cleanup.Job, cancel context.CancelFunc) {
			f.park(job)
			interrupted(f, cancel)
		}, func(_ *fixture, job cleanup.Job) string { return job.Registered }},
		{"git status is interrupted with the tree parked", false, func(f *fixture, job *cleanup.Job, cancel context.CancelFunc) {
			f.park(job)
			cancelledUnderGit(f, cancel)
		}, func(_ *fixture, job cleanup.Job) string { return job.Registered }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			begin := f.accept
			if tt.deferred {
				begin = f.intend
			}
			job := begin()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			tt.arrange(f, &job, cancel)
			record := f.read(f.layout.RecordPath(job.ID))
			want := job

			err := f.relocator.Advance(ctx, &job)

			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Advance() error = %v, want context.Canceled", err)
			}
			if !reflect.DeepEqual(job, want) {
				t.Errorf("job = %+v, want it unchanged %+v", job, want)
			}
			if job.Blocked != nil {
				t.Errorf("blockage = %+v, want none for a cancellation", job.Blocked)
			}
			if got := f.read(f.layout.RecordPath(job.ID)); got != record {
				t.Errorf("record = %q, want it unchanged %q", got, record)
			}
			if got := f.id(tt.tree(f, job)); got != job.Tree {
				t.Errorf("tree identity = %v, want the tree %v", got, job.Tree)
			}
			f.absent(job.Payload)
		})
	}
}

func TestAdvanceBlocksWhenAWorktreeWasAddedUnderTheParkedTree(t *testing.T) {
	f := newFixture(t)
	job, err := f.relocator.Accept(context.Background(), 1, f.request(true))
	if err != nil {
		t.Fatalf("Accept() error = %v", err)
	}
	f.park(&job)
	nested := filepath.Join(job.Registered, "nested")
	f.run(f.repo, "worktree", "add", "-q", "-b", "nested", nested)
	private := filepath.Join(nested, "private.txt")
	f.write(private, "independent nested worktree edit\n")
	nestedAdmin := filepath.Join(f.common, "worktrees", "nested")
	nestedEntry := fmt.Sprintf("worktree %s\nHEAD %s\nbranch refs/heads/nested", nested, f.run(f.repo, "rev-parse", "refs/heads/main"))

	f.advance(&job)

	f.blocked(&job, cleanup.PhaseMoved, "reconcile", fmt.Sprintf(
		"%s registers %s; the tree is parked at %s, still registered; `git worktree move %s %s` restores it",
		nestedAdmin, filepath.Join(nested, ".git"), job.Registered, job.Registered, job.Original,
	))
	f.absent(job.Payload)
	if got := f.id(job.Registered); got != job.Tree {
		t.Errorf("registered identity = %v, want the tree %v", got, job.Tree)
	}
	if got := f.read(private); got != "independent nested worktree edit\n" {
		t.Errorf("private.txt = %q, want it untouched", got)
	}
	if got, want := f.read(filepath.Join(nestedAdmin, "gitdir")), filepath.Join(nested, ".git")+"\n"; got != want {
		t.Errorf("nested admin gitdir = %q, want %q", got, want)
	}
	if got := f.listing(); !strings.Contains(got, nestedEntry) {
		t.Errorf("worktree list = %q, want it to keep %q", got, nestedEntry)
	}
	if got, want := f.read(filepath.Join(f.adminDir, "gitdir")), expectedAdminGitdir(job); got != want {
		t.Errorf("admin gitdir = %q, want %q", got, want)
	}
}

func TestAdvanceBlocksWhenThePayloadWasRegisteredAgain(t *testing.T) {
	f := newFixture(t)
	job := f.accept()
	f.park(&job)
	if err := os.Rename(job.Registered, job.Payload); err != nil {
		t.Fatalf("rename: %v", err)
	}
	job.Phase = cleanup.PhaseDetached
	repointed := filepath.Join(job.Payload, ".git") + "\n"
	f.write(filepath.Join(f.adminDir, "gitdir"), repointed)
	private := filepath.Join(job.Payload, "private.txt")
	f.write(private, "edit after repair\n")
	payloadEntry := fmt.Sprintf("worktree %s\nHEAD %s\nbranch refs/heads/feature", job.Payload, job.Head)
	if got := f.listing(); !strings.Contains(got, payloadEntry) {
		t.Fatalf("worktree list = %q, want the payload registered as %q", got, payloadEntry)
	}
	marker := f.trap(&job)

	f.advance(&job)

	f.blocked(&job, cleanup.PhaseDetached, "reconcile", "payload is registered as "+f.adminDir)
	f.absent(marker)
	f.absent(job.Registered)
	if got := f.id(job.Payload); got != job.Tree {
		t.Errorf("payload identity = %v, want the tree %v", got, job.Tree)
	}
	if got := f.read(private); got != "edit after repair\n" {
		t.Errorf("private.txt = %q, want it untouched", got)
	}
	if got := f.id(f.adminDir); got != job.Admin {
		t.Errorf("admin identity = %v, want %v", got, job.Admin)
	}
	if got := f.read(filepath.Join(f.adminDir, "gitdir")); got != repointed {
		t.Errorf("admin gitdir = %q, want it untouched %q", got, repointed)
	}
	if got := f.listing(); !strings.Contains(got, payloadEntry) {
		t.Errorf("worktree list = %q, want it to keep %q", got, payloadEntry)
	}
}

func TestAdvanceBlocksWhenAnotherAdminDirectoryClaimsTheRegisteredPath(t *testing.T) {
	f := newFixture(t)
	job := f.accept()
	f.park(&job)
	if err := os.Rename(job.Registered, job.Payload); err != nil {
		t.Fatalf("rename: %v", err)
	}
	job.Phase = cleanup.PhaseDetached
	named := filepath.Join(job.Registered, ".git")
	fresh := f.enroll("fresh", named)
	if err := os.Rename(f.adminDir, filepath.Join(f.root, "old-admin")); err != nil {
		t.Fatalf("set the captured admin dir aside: %v", err)
	}
	if err := os.Rename(fresh, f.adminDir); err != nil {
		t.Fatalf("rename: %v", err)
	}
	replacement := f.id(f.adminDir)
	if replacement == job.Admin {
		t.Fatalf("replacement admin dir kept the captured identity %v", job.Admin)
	}
	marker := f.trap(&job)

	f.advance(&job)

	f.blocked(&job, cleanup.PhaseDetached, "reconcile", fmt.Sprintf("%s registers %s", f.adminDir, named))
	f.absent(marker)
	if got := f.id(f.adminDir); got != replacement {
		t.Errorf("admin identity = %v, want the replacement %v", got, replacement)
	}
	if got := f.read(filepath.Join(f.adminDir, "gitdir")); got != named+"\n" {
		t.Errorf("admin gitdir = %q, want it untouched %q", got, named+"\n")
	}
	if got := f.id(job.Payload); got != job.Tree {
		t.Errorf("payload identity = %v, want the tree %v", got, job.Tree)
	}
}

func TestAdvanceReturnsTheContextErrorOnceUnregistered(t *testing.T) {
	f := newFixture(t)
	job := f.unregistered()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	record := f.read(f.layout.RecordPath(job.ID))
	want := job

	err := f.relocator.Advance(ctx, &job)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Advance() error = %v, want context.Canceled", err)
	}
	if !reflect.DeepEqual(job, want) {
		t.Errorf("job = %+v, want it unchanged %+v", job, want)
	}
	if got := f.read(f.layout.RecordPath(job.ID)); got != record {
		t.Errorf("record = %q, want it unchanged %q", got, record)
	}
}

func TestAdvanceLeavesAReplacementOfTheParkedTree(t *testing.T) {
	f := newFixture(t)
	job, err := f.relocator.Accept(context.Background(), 1, f.request(true))
	if err != nil {
		t.Fatalf("Accept() error = %v", err)
	}
	f.park(&job)
	registered, aside := job.Registered, filepath.Join(f.root, "aside")
	kept := filepath.Join(registered, "kept.txt")
	f.guard = func(context.Context, string) error {
		if err := os.Rename(registered, aside); err != nil {
			t.Fatalf("set the parked tree aside: %v", err)
		}
		if err := os.Mkdir(registered, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		f.write(kept, "another actor's work\n")
		return nil
	}

	f.advance(&job)

	replacement := f.id(registered)
	f.blocked(&job, cleanup.PhaseMoved, "reconcile", fmt.Sprintf(
		"registered %s is directory %s; payload %s is absent; admin %s is directory %s; the job captured tree %s and admin %s",
		registered, idText(replacement), job.Payload, f.adminDir, idText(job.Admin), idText(job.Tree), idText(job.Admin),
	))
	f.absent(job.Payload)
	if got := f.read(kept); got != "another actor's work\n" {
		t.Errorf("kept.txt = %q, want it untouched", got)
	}
	if got := f.id(aside); got != job.Tree {
		t.Errorf("set-aside identity = %v, want the tree %v", got, job.Tree)
	}
	if got, want := f.read(filepath.Join(f.adminDir, "gitdir")), expectedAdminGitdir(job); got != want {
		t.Errorf("admin gitdir = %q, want %q", got, want)
	}
}

func TestAdvanceBlocksWhenAWorktreeRodeInWithTheMove(t *testing.T) {
	f := newFixture(t)
	job, err := f.relocator.Accept(context.Background(), 1, f.request(true))
	if err != nil {
		t.Fatalf("Accept() error = %v", err)
	}
	named := filepath.Join(f.worktree, "nested", ".git")
	f.watchers.retire = func(context.Context, string) error {
		f.run(f.repo, "worktree", "add", "-q", "-b", "nested", filepath.Dir(named))
		f.write(filepath.Join(f.worktree, "nested", "private.txt"), "independent nested worktree edit\n")
		return nil
	}
	nestedAdmin := filepath.Join(f.common, "worktrees", "nested")

	f.advance(&job)

	carried := filepath.Join(job.Registered, "nested")
	f.blocked(&job, cleanup.PhaseMoved, "reconcile", fmt.Sprintf(
		"%s registers %s, which moved to %s with the tree; the tree is parked at %s, still registered; `git worktree move %s %s` restores it",
		nestedAdmin, named, filepath.Join(carried, ".git"), job.Registered, job.Registered, job.Original,
	))
	f.absent(job.Payload)
	if got := f.id(job.Registered); got != job.Tree {
		t.Errorf("registered identity = %v, want the tree %v", got, job.Tree)
	}
	if got := f.read(filepath.Join(carried, "private.txt")); got != "independent nested worktree edit\n" {
		t.Errorf("private.txt = %q, want it untouched", got)
	}
	if got, want := f.read(filepath.Join(nestedAdmin, "gitdir")), named+"\n"; got != want {
		t.Errorf("nested admin gitdir = %q, want %q", got, want)
	}
	if got, want := f.read(filepath.Join(f.adminDir, "gitdir")), expectedAdminGitdir(job); got != want {
		t.Errorf("admin gitdir = %q, want %q", got, want)
	}
}

func TestAdvanceDeferredWhoseTreeVanished(t *testing.T) {
	f := newFixture(t)
	job := f.intend()
	if err := os.RemoveAll(f.worktree); err != nil {
		t.Fatal(err)
	}

	f.advance(&job)

	f.blocked(&job, cleanup.PhaseQueued, "identity", fmt.Sprintf("%s is gone, but git still registers it at %s; git worktree prune drops the registration, then ccx vcs cleanup retry finishes the job", f.worktree, f.adminDir))

	f.run(f.repo, "worktree", "prune")
	job.Blocked = nil
	f.advance(&job)

	if job.Phase != cleanup.PhaseDone || job.Blocked != nil {
		t.Fatalf("job is at %s with blockage %+v, want done", job.Phase, job.Blocked)
	}
	f.stored(&job)
	if got, want := f.listing(), f.mainEntry(); got != want {
		t.Errorf("worktree list = %q, want %q", got, want)
	}
}
