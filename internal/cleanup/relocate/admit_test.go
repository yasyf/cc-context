package relocate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/yasyf/cc-context/internal/cleanup"
)

func (f *fixture) unregistered() cleanup.Job {
	f.t.Helper()
	job := f.accept()
	f.advance(&job)
	f.finished(&job)
	return job
}

func (f *fixture) admit(job *cleanup.Job) {
	f.t.Helper()
	if err := f.relocator.Admit(context.Background(), job); err != nil {
		f.t.Fatalf("Admit() error = %v", err)
	}
}

func (f *fixture) enroll(name, gitdir string) string {
	f.t.Helper()
	admin := filepath.Join(f.common, "worktrees", name)
	if err := os.MkdirAll(admin, 0o700); err != nil {
		f.t.Fatalf("create admin dir: %v", err)
	}
	f.write(filepath.Join(admin, "gitdir"), gitdir+"\n")
	return admin
}

func (f *fixture) setAside(path string) string {
	f.t.Helper()
	aside := filepath.Join(f.root, "aside")
	if err := os.Rename(path, aside); err != nil {
		f.t.Fatalf("set %s aside: %v", path, err)
	}
	return aside
}

func TestAdmitPassesAProvenPayload(t *testing.T) {
	for _, phase := range []cleanup.Phase{cleanup.PhaseUnregistered, cleanup.PhaseDeleting} {
		t.Run(string(phase), func(t *testing.T) {
			f := newFixture(t)
			job := f.unregistered()
			job.Phase = phase
			if err := f.journal.Save(job); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			record := f.read(f.layout.RecordPath(job.ID))
			guarded, checked := len(f.guarded), len(f.watchers.checked)
			want := job

			f.admit(&job)

			if !reflect.DeepEqual(job, want) {
				t.Errorf("job = %+v, want it unchanged %+v", job, want)
			}
			if got := f.read(f.layout.RecordPath(job.ID)); got != record {
				t.Errorf("record = %q, want it unchanged %q", got, record)
			}
			if got, want := f.guarded[guarded:], []string{job.Payload}; !reflect.DeepEqual(got, want) {
				t.Errorf("guard consulted for %v, want %v", got, want)
			}
			if got, want := f.watchers.checked[checked:], []string{f.layout.JobDir(job.ID)}; !reflect.DeepEqual(got, want) {
				t.Errorf("quarantine checked %v, want %v", got, want)
			}
			if got := f.id(job.Payload); got != job.Tree {
				t.Errorf("payload identity = %v, want the tree %v", got, job.Tree)
			}
		})
	}
}

func TestAdmitPassesAPushedPayloadPastAFailedProbe(t *testing.T) {
	for _, pushed := range []bool{true, false} {
		t.Run(fmt.Sprintf("pushed=%t", pushed), func(t *testing.T) {
			f := newFixture(t)
			if pushed {
				f.run(f.repo, "update-ref", "refs/remotes/origin/feature", "feature")
			}
			job := f.unregistered()
			f.guard = func(context.Context, string) error { return errArgumentsUnread }

			if err := f.relocator.Admit(context.Background(), &job); err != nil {
				t.Fatalf("Admit() error = %v", err)
			}

			if blocked := job.Blocked != nil; blocked == pushed {
				t.Errorf("blockage = %+v with the head pushed = %t, want blocked only when unpushed", job.Blocked, pushed)
			}
		})
	}
}

func TestAdmitPassesAnAbsentPayload(t *testing.T) {
	f := newFixture(t)
	job := f.unregistered()
	if err := os.RemoveAll(job.Payload); err != nil {
		t.Fatalf("remove the payload: %v", err)
	}
	if err := os.Mkdir(job.Registered, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	record := f.read(f.layout.RecordPath(job.ID))
	guarded, checked := len(f.guarded), len(f.watchers.checked)
	want := job

	f.admit(&job)

	if !reflect.DeepEqual(job, want) {
		t.Errorf("job = %+v, want it unchanged %+v", job, want)
	}
	if got := f.read(f.layout.RecordPath(job.ID)); got != record {
		t.Errorf("record = %q, want it unchanged %q", got, record)
	}
	if len(f.guarded) != guarded || len(f.watchers.checked) != checked {
		t.Errorf("guard consulted %d and quarantine checked %d more times for a payload that is gone, want none", len(f.guarded)-guarded, len(f.watchers.checked)-checked)
	}
}

func TestAdmitBlocks(t *testing.T) {
	tests := []struct {
		name    string
		arrange func(f *fixture, job cleanup.Job) (detail string)
		reason  string
		aside   bool
		guarded bool
	}{
		{"the payload is another directory", func(f *fixture, job cleanup.Job) string {
			f.setAside(job.Payload)
			if err := os.Mkdir(job.Payload, 0o700); err != nil {
				f.t.Fatalf("mkdir: %v", err)
			}
			return fmt.Sprintf("payload %s is directory %s, the job captured tree %s", job.Payload, idText(f.id(job.Payload)), idText(job.Tree))
		}, "identity", true, false},
		{"the payload is a symlink to the tree", func(f *fixture, job cleanup.Job) string {
			if err := os.Symlink(f.setAside(job.Payload), job.Payload); err != nil {
				f.t.Fatalf("symlink: %v", err)
			}
			return fmt.Sprintf("payload %s is non-directory %s, the job captured tree %s", job.Payload, idText(f.id(job.Payload)), idText(job.Tree))
		}, "identity", true, false},
		{"something sits at the registered path", func(f *fixture, job cleanup.Job) string {
			if err := os.Mkdir(job.Registered, 0o700); err != nil {
				f.t.Fatalf("mkdir: %v", err)
			}
			return fmt.Sprintf("registered %s is directory %s, want it absent", job.Registered, idText(f.id(job.Registered)))
		}, "reconcile", false, false},
		{"the payload is registered again", func(f *fixture, job cleanup.Job) string {
			return "payload is registered as " + f.enroll("again", filepath.Join(job.Payload, ".git"))
		}, "reconcile", false, false},
		{"a worktree is registered inside the payload", func(f *fixture, job cleanup.Job) string {
			inner := filepath.Join(job.Payload, "inner")
			f.run(f.repo, "worktree", "add", "-q", "-b", "inner", inner)
			return fmt.Sprintf("%s registers %s", filepath.Join(f.common, "worktrees", "inner"), filepath.Join(inner, ".git"))
		}, "reconcile", false, false},
		{"an entry names the emptied registered path", func(f *fixture, job cleanup.Job) string {
			named := filepath.Join(job.Registered, ".git")
			return fmt.Sprintf("%s registers %s", f.enroll("stale", named), named)
		}, "reconcile", false, false},
		{"an entry names the job folder itself", func(f *fixture, job cleanup.Job) string {
			named := f.layout.JobDir(job.ID)
			return fmt.Sprintf("%s registers %s", f.enroll("folder", named), named)
		}, "reconcile", false, false},
		{"an entry reaches the payload through a symlink", func(f *fixture, job cleanup.Job) string {
			alias := filepath.Join(f.root, "alias")
			if err := os.Symlink(f.layout.JobDir(job.ID), alias); err != nil {
				f.t.Fatalf("symlink: %v", err)
			}
			named := filepath.Join(alias, cleanup.PayloadName, ".git")
			return fmt.Sprintf("%s registers %s", f.enroll("alias", named), named)
		}, "reconcile", false, false},
		{"an entry names the payload relative to its admin directory", func(f *fixture, job cleanup.Job) string {
			admin := filepath.Join(f.common, "worktrees", "relative")
			relative, err := filepath.Rel(admin, filepath.Join(job.Payload, ".git"))
			if err != nil {
				f.t.Fatalf("relative path: %v", err)
			}
			return "payload is registered as " + f.enroll("relative", relative)
		}, "reconcile", false, false},
		{"an entry names a tree in the payload whose .git leads outside", func(f *fixture, job cleanup.Job) string {
			alias := filepath.Join(f.root, "alias")
			if err := os.Symlink(job.Payload, alias); err != nil {
				f.t.Fatalf("symlink: %v", err)
			}
			if err := os.Mkdir(filepath.Join(job.Payload, "inner"), 0o700); err != nil {
				f.t.Fatalf("mkdir: %v", err)
			}
			external := filepath.Join(f.root, "external-gitfile")
			f.write(external, "gitdir: "+filepath.Join(f.common, "worktrees", "led")+"\n")
			if err := os.Symlink(external, filepath.Join(job.Payload, "inner", ".git")); err != nil {
				f.t.Fatalf("symlink: %v", err)
			}
			named := filepath.Join(alias, "inner", ".git")
			return fmt.Sprintf("%s registers %s", f.enroll("led", named), named)
		}, "reconcile", false, false},
		{"an entry cannot be resolved", func(f *fixture, job cleanup.Job) string {
			if os.Geteuid() == 0 {
				f.t.Skip("root reads through a closed directory")
			}
			closed := filepath.Join(f.root, "outside", "closed")
			if err := os.MkdirAll(closed, 0o700); err != nil {
				f.t.Fatalf("mkdir: %v", err)
			}
			link := filepath.Join(closed, "link")
			if err := os.Symlink(job.Payload, link); err != nil {
				f.t.Fatalf("symlink: %v", err)
			}
			if err := os.Chmod(closed, 0); err != nil {
				f.t.Fatalf("chmod: %v", err)
			}
			f.t.Cleanup(func() { _ = os.Chmod(closed, 0o700) }) //nolint:gosec // restores a fixture directory so it can be removed
			named := filepath.Join(link, "inner", ".git")
			return fmt.Sprintf("resolve %s, which %s registers: lstat %s: permission denied", named, f.enroll("closed", named), link)
		}, "reconcile", false, false},
		{"a worktree rode in with the tree", func(f *fixture, job cleanup.Job) string {
			if err := os.Mkdir(filepath.Join(job.Payload, "inner"), 0o700); err != nil {
				f.t.Fatalf("mkdir: %v", err)
			}
			carried := filepath.Join(job.Payload, "inner", ".git")
			named := filepath.Join(job.Original, "inner", ".git")
			admin := f.enroll("rode", named)
			f.write(carried, "gitdir: "+admin+"\n")
			return fmt.Sprintf("%s registers %s, which moved to %s with the tree", admin, named, carried)
		}, "reconcile", false, false},
		{"the repository is gone", func(f *fixture, _ cleanup.Job) string {
			if err := os.Rename(f.repo, filepath.Join(f.root, "renamed")); err != nil {
				f.t.Fatalf("rename: %v", err)
			}
			return fmt.Sprintf("read worktree registry of %s: stat %s: no such file or directory", f.common, f.common)
		}, "reconcile", false, false},
		{"the job folder is watched", func(f *fixture, _ cleanup.Job) string {
			f.watchers.quarantine = func(context.Context, string) error { return errors.New("job folder sits under a watched root") }
			return "job folder sits under a watched root"
		}, "quarantine", false, false},
		{"a process holds the payload", func(f *fixture, job cleanup.Job) string {
			return f.holdAt(job.Payload).Error()
		}, "activity", false, true},
		{"activity is unclear", func(f *fixture, job cleanup.Job) string {
			f.guard = func(context.Context, string) error { return errors.New("process table unreadable") }
			return "could not verify that " + job.Payload + " is idle: process table unreadable"
		}, "activity", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			job := f.unregistered()
			detail := tt.arrange(f, job)
			guarded := len(f.guarded)

			f.admit(&job)

			f.blocked(&job, cleanup.PhaseUnregistered, tt.reason, detail)
			tree := job.Payload
			if tt.aside {
				tree = filepath.Join(f.root, "aside")
			}
			if got := f.id(tree); got != job.Tree {
				t.Errorf("tree identity at %s = %v, want the tree %v", tree, got, job.Tree)
			}
			if got := f.read(filepath.Join(tree, "feature.txt")); got != "feature\n" {
				t.Errorf("feature.txt = %q, want %q", got, "feature\n")
			}
			want := []string{}
			if tt.guarded {
				want = append(want, job.Payload)
			}
			if got := f.guarded[guarded:]; !reflect.DeepEqual(got, want) {
				t.Errorf("guard consulted for %v, want %v", got, want)
			}
		})
	}
}

func TestAdmitBlocksAnAdoptedPayloadWhoseSourceIsRegistered(t *testing.T) {
	q := newQuarantine(t)
	job := q.adopt()
	q.advance(&job)
	q.finished(&job)
	named := filepath.Join(q.source, ".git")
	admin := q.enroll("returned", named)
	guarded := len(q.guarded)

	q.admit(&job)

	q.blocked(&job, cleanup.PhaseUnregistered, "reconcile", fmt.Sprintf("%s registers %s", admin, named))
	if got := q.id(job.Payload); got != q.tree {
		t.Errorf("payload identity = %v, want the tree %v", got, q.tree)
	}
	if len(q.guarded) != guarded {
		t.Errorf("guard consulted %d more times for a payload that failed an earlier proof, want none", len(q.guarded)-guarded)
	}
}

func TestAdmitReturnsTheContextErrorWithoutBlocking(t *testing.T) {
	tests := []struct {
		name    string
		arrange func(f *fixture, cancel context.CancelFunc)
	}{
		{"cancelled before the call", func(_ *fixture, cancel context.CancelFunc) { cancel() }},
		{"the quarantine check is interrupted", func(f *fixture, cancel context.CancelFunc) {
			f.watchers.quarantine = func(ctx context.Context, _ string) error {
				cancel()
				return ctx.Err()
			}
		}},
		{"the guard is interrupted", func(f *fixture, cancel context.CancelFunc) {
			f.guard = func(ctx context.Context, _ string) error {
				cancel()
				return ctx.Err()
			}
		}},
		{"the guard passes as the context is cancelled", func(f *fixture, cancel context.CancelFunc) {
			f.guard = func(context.Context, string) error {
				cancel()
				return nil
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			job := f.unregistered()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			tt.arrange(f, cancel)
			record := f.read(f.layout.RecordPath(job.ID))
			want := job

			err := f.relocator.Admit(ctx, &job)

			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Admit() error = %v, want context.Canceled", err)
			}
			if !reflect.DeepEqual(job, want) {
				t.Errorf("job = %+v, want it unchanged %+v", job, want)
			}
			if got := f.read(f.layout.RecordPath(job.ID)); got != record {
				t.Errorf("record = %q, want it unchanged %q", got, record)
			}
			if got := f.id(job.Payload); got != job.Tree {
				t.Errorf("payload identity = %v, want the tree %v", got, job.Tree)
			}
		})
	}
}
