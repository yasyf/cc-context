//go:build !windows

package relocate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/cleanup"
	"github.com/yasyf/cc-context/internal/render"
)

const shortBudget = time.Second

func (f *fixture) fifo(name string) string {
	f.t.Helper()
	path := filepath.Join(f.root, name)
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		f.t.Fatalf("mkfifo %s: %v", path, err)
	}
	return path
}

func (f *fixture) holdingGit(hold string, after bool) (git, marker, ran string) {
	f.t.Helper()
	git = filepath.Join(f.root, "holding-git")
	marker = filepath.Join(f.root, "holding")
	ran = filepath.Join(f.root, "ran-before-holding")
	never := f.fifo("never-written")
	run := ""
	if after {
		run = fmt.Sprintf("\t\t%s \"$@\"\n\t\t: > %s\n", f.git, ran)
	}
	script := fmt.Sprintf(
		"#!/bin/sh\nif [ -e %s ]; then\n\tcase \" $* \" in *\" %s \"*)\n%s\t\tread x < %s\n\t\texit 1\n\tesac\nfi\nexec %s \"$@\"\n",
		marker, hold, run, never, f.git,
	)
	f.write(marker, "")
	if err := os.WriteFile(git, []byte(script), 0o700); err != nil { //nolint:gosec // the script stands in for git and is executed
		f.t.Fatalf("write holding git: %v", err)
	}
	return git, marker, ran
}

func openFIFO(t *testing.T, path string, flag int, stuck string) *os.File {
	t.Helper()
	opened := make(chan *os.File, 1)
	failed := make(chan error, 1)
	go func() {
		file, err := os.OpenFile(path, flag, 0) //nolint:gosec // the fifo is the test's own
		if err != nil {
			failed <- err
			return
		}
		opened <- file
	}()
	select {
	case file := <-opened:
		return file
	case err := <-failed:
		t.Fatalf("open %s: %v", path, err)
	case <-time.After(30 * time.Second):
		t.Fatal(stuck)
	}
	return nil
}

func TestAdvanceBlocksAnInterruptedGitStepAsATimeoutThenRecovers(t *testing.T) {
	moveArgv := func(f *fixture, job cleanup.Job) string {
		return "--git-dir=" + f.common + " -c worktree.useRelativePaths=false worktree move " + f.worktree + " " + job.Registered
	}
	atOriginal := func(f *fixture, _ cleanup.Job) string { return f.worktree }
	tests := []struct {
		name  string
		hold  string
		after bool
		read  bool
		phase cleanup.Phase
		argv  func(f *fixture, job cleanup.Job) string
		tree  func(f *fixture, job cleanup.Job) string
	}{
		{"update-ref killed once it had run", "update-ref", true, false, cleanup.PhasePrepared, func(f *fixture, job cleanup.Job) string {
			return "--git-dir=" + f.common + " update-ref " + job.RecoveryRef + " " + job.Head
		}, atOriginal},
		{"worktree move killed before it ran", "worktree move", false, false, cleanup.PhasePrepared, moveArgv, atOriginal},
		{"worktree move killed once it had run", "worktree move", true, false, cleanup.PhasePrepared, moveArgv, func(_ *fixture, job cleanup.Job) string {
			return job.Registered
		}},
		{"worktree remove killed once it had run", "worktree remove", true, false, cleanup.PhaseDetached, func(f *fixture, job cleanup.Job) string {
			return "--git-dir=" + f.common + " worktree remove " + job.Registered
		}, func(_ *fixture, job cleanup.Job) string { return job.Payload }},
		{"status killed before it answered", "status", false, true, cleanup.PhasePrepared, func(f *fixture, _ cleanup.Job) string {
			return "-C " + f.worktree + " status --porcelain=v1 -z --untracked-files=normal --ignore-submodules=none"
		}, atOriginal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.bound(shortBudget)
			job := f.accept()
			git, marker, ran := f.holdingGit(tt.hold, tt.after)
			job.Git = git

			f.advance(&job)

			killed := "git " + tt.argv(f, job) + ": signal: killed"
			detail := fmt.Sprintf("interrupted at its %s budget: %s", shortBudget, killed)
			if tt.read {
				detail = cleanup.ErrUnprobed.Error() + ": " + killed
			}
			f.blocked(&job, tt.phase, "timeout", detail)
			if _, err := os.Stat(ran); tt.after && err != nil {
				t.Fatalf("the held %s never ran the real git inside its %s budget: %v", tt.hold, shortBudget, err)
			}
			if !tt.after {
				f.absent(ran)
			}
			if got := f.id(tt.tree(f, job)); got != job.Tree {
				t.Fatalf("tree identity at %s = %v, want the tree %v", tt.tree(f, job), got, job.Tree)
			}

			if err := os.Remove(marker); err != nil {
				t.Fatalf("release the holding git: %v", err)
			}
			job.Blocked = nil
			f.advance(&job)

			f.finished(&job)
			f.absent(f.worktree)
			f.absent(job.Registered)
			f.absent(f.adminDir)
			if got := f.id(job.Payload); got != job.Tree {
				t.Errorf("payload identity = %v, want the tree %v", got, job.Tree)
			}
			if got := f.run(f.repo, "rev-parse", job.RecoveryRef); got != job.Head {
				t.Errorf("recovery ref = %s, want %s", got, job.Head)
			}
			if got, want := f.listing(), f.mainEntry(); got != want {
				t.Errorf("worktree list = %q, want %q", got, want)
			}
		})
	}
}

func TestGitBudgetKillsOnlyTheDirectChild(t *testing.T) {
	f := newFixture(t)
	f.bound(shortBudget)
	live, block, hold := f.fifo("live"), f.fifo("block"), f.fifo("hold")
	forking := filepath.Join(f.root, "forking-git")
	script := fmt.Sprintf("#!/bin/sh\n( exec 3>%s; read x < %s ) &\nread x < %s\n", live, block, hold)
	if err := os.WriteFile(forking, []byte(script), 0o700); err != nil { //nolint:gosec // the script stands in for git and is executed
		t.Fatalf("write forking git: %v", err)
	}
	child := &exec.Cmd{}
	render.BoundChild(child)
	limit := shortBudget + child.WaitDelay + 5*time.Second

	started := time.Now()
	result := make(chan error, 1)
	go func() {
		_, err := f.relocator.rewrite(context.Background(), forking, "update-ref", "refs/probe", "HEAD")
		result <- err
	}()
	descendant := openFIFO(t, live, os.O_RDONLY, "the descendant never opened the live fifo")
	defer func() { _ = descendant.Close() }()

	var err error
	select {
	case err = <-result:
	case <-time.After(limit):
		t.Fatalf("rewrite did not return within %s: the descendant holding the pipes was waited on past WaitDelay", limit)
	}
	if elapsed := time.Since(started); elapsed < shortBudget {
		t.Errorf("rewrite returned after %s, before its %s budget", elapsed, shortBudget)
	}
	if gitReason(err) != "timeout" || !strings.HasSuffix(err.Error(), "update-ref refs/probe HEAD: signal: killed") {
		t.Errorf("rewrite error = %v, want the direct child killed at its budget", err)
	}

	release := openFIFO(t, block, os.O_WRONLY, "the descendant was signalled: nothing reads the block fifo")
	if _, err := release.Write([]byte("x\n")); err != nil {
		t.Fatalf("release the descendant: %v", err)
	}
	if err := release.Close(); err != nil {
		t.Fatalf("close the block fifo: %v", err)
	}
	exited := make(chan error, 1)
	go func() {
		_, err := descendant.Read(make([]byte, 1))
		exited <- err
	}()
	select {
	case err := <-exited:
		if !errors.Is(err, io.EOF) {
			t.Errorf("live fifo read = %v, want EOF once the released descendant exits", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the released descendant never exited")
	}
}
