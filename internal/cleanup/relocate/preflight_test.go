package relocate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/yasyf/cc-context/internal/cleanup"
)

func TestAcceptJournalsPreparedJob(t *testing.T) {
	f := newFixture(t)
	head := f.run(f.worktree, "rev-parse", "HEAD")
	tree, admin := f.id(f.worktree), f.id(f.adminDir)

	job, err := f.relocator.Accept(context.Background(), 7, f.request(false))
	if err != nil {
		t.Fatalf("Accept() error = %v", err)
	}

	want := cleanup.Job{
		Schema:      cleanup.Schema,
		ID:          job.ID,
		Seq:         7,
		Phase:       cleanup.PhasePrepared,
		Repo:        f.common,
		AdminDir:    f.adminDir,
		Admin:       admin,
		Original:    f.worktree,
		Registered:  f.layout.Registered(job.ID),
		Payload:     f.layout.Payload(job.ID),
		Tree:        tree,
		Head:        head,
		Branch:      "feature",
		RecoveryRef: cleanup.RecoveryRefFor(job.ID),
		Git:         f.git,
		Links: cleanup.Links{
			DotGit:      "gitdir: " + f.adminDir + "\n",
			AdminGitdir: f.worktree + "/.git\n",
		},
		Created: f.clock,
		Updated: f.clock,
	}
	if !reflect.DeepEqual(job, want) {
		t.Errorf("Accept() = %+v\nwant %+v", job, want)
	}
	f.stored(&job)
	if got := f.id(f.worktree); got != tree {
		t.Errorf("worktree identity = %v, want %v", got, tree)
	}
	f.absent(job.Registered)
	if refs := f.run(f.repo, "for-each-ref", cleanup.RecoveryRefPrefix); refs != "" {
		t.Errorf("recovery refs after Accept = %q, want none", refs)
	}
	if want := []string{f.worktree}; !reflect.DeepEqual(f.guarded, want) {
		t.Errorf("guard consulted for %v, want %v", f.guarded, want)
	}
	if len(f.watchers.retired) != 0 {
		t.Errorf("watchers retired %v during Accept, want none", f.watchers.retired)
	}
}

func TestPreviewJournalsNothing(t *testing.T) {
	f := newFixture(t)

	job, err := f.relocator.Preview(context.Background(), f.request(false))
	if err != nil {
		t.Fatalf("Preview() error = %v", err)
	}

	if job.Seq != 0 || job.Phase != cleanup.PhasePrepared || job.Original != f.worktree {
		t.Errorf("Preview() = seq %d phase %s original %s, want 0, prepared, %s", job.Seq, job.Phase, job.Original, f.worktree)
	}
	if job.Registered != f.layout.Registered(job.ID) || job.RecoveryRef != cleanup.RecoveryRefFor(job.ID) {
		t.Errorf("Preview() registered %s and ref %s do not belong to job %s", job.Registered, job.RecoveryRef, job.ID)
	}
	f.unjournaled()
}

func TestPreviewThroughAViewedJournalCreatesNothing(t *testing.T) {
	f := newFixture(t)
	unmade := filepath.Join(f.root, "unmade")
	f.view(filepath.Join(unmade, "state"))

	job, err := f.relocator.Preview(context.Background(), f.request(false))
	if err != nil {
		t.Fatalf("Preview() error = %v", err)
	}

	if job.Seq != 0 || job.Phase != cleanup.PhasePrepared || job.Original != f.worktree {
		t.Errorf("Preview() = seq %d phase %s original %s, want 0, prepared, %s", job.Seq, job.Phase, job.Original, f.worktree)
	}
	if want := filepath.Join(unmade, "state", "jobs", job.ID, "registered"); job.Registered != want {
		t.Errorf("Preview() registered = %s, want %s", job.Registered, want)
	}
	f.absent(unmade)
	if refs := f.run(f.repo, "for-each-ref", cleanup.RecoveryRefPrefix); refs != "" {
		t.Errorf("recovery refs = %q, want none", refs)
	}
}

func TestNearest(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatalf("write %s: %v", file, err)
	}
	tests := []struct {
		name    string
		path    string
		want    string
		wantErr error
	}{
		{"existing path", file, file, nil},
		{"one absent component", filepath.Join(root, "jobs"), root, nil},
		{"several absent components", filepath.Join(root, "state", "jobs", "id"), root, nil},
		{"under a regular file", filepath.Join(file, "jobs"), "", syscall.ENOTDIR},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info, err := nearest(tt.path)

			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("nearest() error = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr != nil {
				return
			}
			want, err := os.Stat(tt.want)
			if err != nil {
				t.Fatalf("stat %s: %v", tt.want, err)
			}
			if !os.SameFile(info, want) {
				t.Errorf("nearest() = %s, want %s", info.Name(), tt.want)
			}
		})
	}
}

func TestAcceptCanonicalizesASymlinkedPath(t *testing.T) {
	f := newFixture(t)
	alias := filepath.Join(f.root, "alias")
	if err := os.Symlink(f.worktree, alias); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	job, err := f.relocator.Accept(context.Background(), 1, cleanup.Request{Worktree: alias, Git: f.git})
	if err != nil {
		t.Fatalf("Accept() error = %v", err)
	}
	if job.Original != f.worktree {
		t.Errorf("Original = %s, want %s", job.Original, f.worktree)
	}
}

func TestAcceptCapturesHeadAndBranch(t *testing.T) {
	tests := []struct {
		name       string
		add        []string
		wantBranch string
		wantHead   bool
	}{
		{"unborn branch", []string{"--orphan", "-b", "orphan"}, "orphan", false},
		{"detached head", []string{"--detach"}, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			other := filepath.Join(f.root, "other")
			f.run(f.repo, append(append([]string{"worktree", "add", "-q"}, tt.add...), other)...)
			wantHead, wantRef := "", ""
			if tt.wantHead {
				wantHead = f.run(other, "rev-parse", "HEAD")
			}

			job, err := f.relocator.Accept(context.Background(), 1, cleanup.Request{Worktree: other, Git: f.git})
			if err != nil {
				t.Fatalf("Accept() error = %v", err)
			}
			if tt.wantHead {
				wantRef = cleanup.RecoveryRefFor(job.ID)
			}
			if job.Head != wantHead || job.Branch != tt.wantBranch || job.RecoveryRef != wantRef {
				t.Errorf("Accept() head %q branch %q ref %q, want %q, %q, %q", job.Head, job.Branch, job.RecoveryRef, wantHead, tt.wantBranch, wantRef)
			}

			f.advance(&job)
			f.finished(&job)
			f.absent(other)
			if refs := f.run(f.repo, "for-each-ref", "--format=%(refname) %(objectname)", cleanup.RecoveryRefPrefix); refs != strings.TrimSpace(wantRef+" "+wantHead) {
				t.Errorf("recovery refs = %q, want %q", refs, strings.TrimSpace(wantRef+" "+wantHead))
			}
		})
	}
}

func TestAcceptRefusals(t *testing.T) {
	tests := []struct {
		name    string
		reason  string
		arrange func(f *fixture) (worktree, detail string)
	}{
		{"main working copy", "main", func(f *fixture) (string, string) {
			return f.repo, f.repo + " is a repository's main working copy, not a linked worktree"
		}},
		{"working copy of a separate git dir", "main", func(f *fixture) (string, string) {
			separate, gitDir := filepath.Join(f.root, "separate"), filepath.Join(f.root, "separate.git")
			f.run(f.root, "init", "-q", "-b", "main", "--separate-git-dir", gitDir, separate)
			return separate, fmt.Sprintf("%s is the working copy of the repository at %s, not a linked worktree", separate, gitDir)
		}},
		{"plain directory", "unregistered", func(f *fixture) (string, string) {
			plain := filepath.Join(f.root, "plain")
			if err := os.Mkdir(plain, 0o755); err != nil {
				f.t.Fatalf("mkdir: %v", err)
			}
			return plain, "not a registered linked worktree: stat " + plain + "/.git: no such file or directory"
		}},
		{"missing path", "unregistered", func(f *fixture) (string, string) {
			missing := filepath.Join(f.root, "missing")
			return missing, "not a registered linked worktree: lstat " + missing + ": no such file or directory"
		}},
		{"one-way link", "unregistered", func(f *fixture) (string, string) {
			copied := filepath.Join(f.root, "copied")
			if err := os.Mkdir(copied, 0o755); err != nil {
				f.t.Fatalf("mkdir: %v", err)
			}
			f.write(filepath.Join(copied, ".git"), f.read(filepath.Join(f.worktree, ".git")))
			return copied, fmt.Sprintf("%s registers %s/.git, not %s/.git", f.adminDir, f.worktree, copied)
		}},
		{"locked", "locked", func(f *fixture) (string, string) {
			f.run(f.repo, "worktree", "lock", "--reason", "in use by ci", f.worktree)
			return f.worktree, "worktree is locked: in use by ci"
		}},
		{"initialized submodules", "submodules", func(f *fixture) (string, string) {
			modules := filepath.Join(f.adminDir, "modules")
			if err := os.MkdirAll(filepath.Join(modules, "sub"), 0o755); err != nil {
				f.t.Fatalf("mkdir: %v", err)
			}
			return f.worktree, "worktree has initialized submodules under " + modules
		}},
		{"gitlink in the index", "submodules", func(f *fixture) (string, string) {
			f.write(filepath.Join(f.worktree, ".gitmodules"), "[submodule \"sub\"]\n\tpath = sub\n\turl = ../sub\n")
			f.run(f.worktree, "update-index", "--add", "--cacheinfo", "160000,"+f.run(f.worktree, "rev-parse", "HEAD")+",sub")
			return f.worktree, "worktree has submodules in its index; git cannot move it"
		}},
		{"nested worktree", "nested", func(f *fixture) (string, string) {
			inner := filepath.Join(f.worktree, "inner")
			f.run(f.repo, "worktree", "add", "-q", "-b", "inner", inner)
			return f.worktree, fmt.Sprintf("worktree %s is registered inside %s", inner, f.worktree)
		}},
		{"dirty", "dirty", func(f *fixture) (string, string) {
			f.write(filepath.Join(f.worktree, "feature.txt"), "edited\n")
			f.write(filepath.Join(f.worktree, "scratch.txt"), "scratch\n")
			return f.worktree, "uncommitted changes: feature.txt, scratch.txt"
		}},
		{"dirty beyond the shown paths", "dirty", func(f *fixture) (string, string) {
			for i := range 7 {
				f.write(filepath.Join(f.worktree, fmt.Sprintf("u%d.txt", i)), "untracked\n")
			}
			return f.worktree, "uncommitted changes: u0.txt, u1.txt, u2.txt, u3.txt, u4.txt and 2 more"
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			worktree, detail := tt.arrange(f)
			tree, listing := f.id(f.worktree), f.listing()

			_, err := f.relocator.Accept(context.Background(), 1, cleanup.Request{Worktree: worktree, Git: f.git})

			var refused *cleanup.RefusedError
			if !errors.As(err, &refused) {
				t.Fatalf("Accept() error = %v, want a *RefusedError", err)
			}
			want := cleanup.RefusedError{Worktree: worktree, Reason: tt.reason, Detail: detail}
			if *refused != want {
				t.Errorf("Accept() refused %+v\nwant %+v", *refused, want)
			}
			f.unjournaled()
			if got := f.id(f.worktree); got != tree {
				t.Errorf("worktree identity = %v, want %v", got, tree)
			}
			if got := f.listing(); got != listing {
				t.Errorf("worktree list = %q, want %q", got, listing)
			}
		})
	}
}

func TestAcceptForceOverridesDirtiness(t *testing.T) {
	f := newFixture(t)
	f.write(filepath.Join(f.worktree, "scratch.txt"), "scratch\n")

	job, err := f.relocator.Accept(context.Background(), 1, f.request(true))
	if err != nil {
		t.Fatalf("Accept() error = %v", err)
	}
	f.advance(&job)

	f.finished(&job)
	if !job.Force {
		t.Error("Force = false, want true")
	}
	if got := f.read(filepath.Join(job.Payload, "scratch.txt")); got != "scratch\n" {
		t.Errorf("payload scratch.txt = %q, want %q", got, "scratch\n")
	}
}

func TestAcceptForceOverridesNothingElse(t *testing.T) {
	f := newFixture(t)
	f.run(f.repo, "worktree", "lock", f.worktree)

	_, err := f.relocator.Accept(context.Background(), 1, f.request(true))

	var refused *cleanup.RefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("Accept() error = %v, want a *RefusedError", err)
	}
	if want := (cleanup.RefusedError{Worktree: f.worktree, Reason: "locked", Detail: "worktree is locked"}); *refused != want {
		t.Errorf("Accept() refused %+v, want %+v", *refused, want)
	}
	f.unjournaled()
}

func TestAcceptGuardActive(t *testing.T) {
	f := newFixture(t)
	active := f.holdAt(f.worktree)

	_, err := f.relocator.Accept(context.Background(), 1, f.request(false))

	var got *cleanup.ActiveError
	if !errors.As(err, &got) || got != active {
		t.Fatalf("Accept() error = %v, want the guard's *ActiveError", err)
	}
	f.unjournaled()
}

func TestAcceptGuardUnclear(t *testing.T) {
	f := newFixture(t)
	unreadable := errors.New("process table unreadable")
	f.guard = func(context.Context, string) error { return unreadable }

	_, err := f.relocator.Accept(context.Background(), 1, f.request(false))

	if !errors.Is(err, unreadable) {
		t.Fatalf("Accept() error = %v, want it to wrap %v", err, unreadable)
	}
	if want := "cleanup: could not verify that " + f.worktree + " is idle: process table unreadable"; err.Error() != want {
		t.Errorf("Accept() error = %q, want %q", err.Error(), want)
	}
	var refused *cleanup.RefusedError
	var active *cleanup.ActiveError
	if errors.As(err, &refused) || errors.As(err, &active) {
		t.Errorf("Accept() error = %#v, want neither a refusal nor an activity verdict", err)
	}
	f.unjournaled()
}

func TestSameVolume(t *testing.T) {
	tests := []struct {
		name string
		tree cleanup.FileID
		jobs cleanup.FileID
		want bool
	}{
		{"one device", cleanup.FileID{Dev: 16777231, Ino: 10}, cleanup.FileID{Dev: 16777231, Ino: 99}, true},
		{"two devices", cleanup.FileID{Dev: 16777231, Ino: 10}, cleanup.FileID{Dev: 16777232, Ino: 10}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sameVolume(tt.tree, tt.jobs); got != tt.want {
				t.Errorf("sameVolume() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestEachRecordReadsOnlyTheHeadOfALongRecord(t *testing.T) {
	long := "160000 " + strings.Repeat("x", 9000)
	input := "100644 short\n" + long + "\n100755 tail"

	var heads []string
	err := eachRecord(strings.NewReader(input), '\n', func(head []byte) {
		heads = append(heads, string(head))
	})
	if err != nil {
		t.Fatalf("eachRecord() error = %v", err)
	}

	want := []string{"100644 short", long[:4096], "100755 tail"}
	if !reflect.DeepEqual(heads, want) {
		t.Errorf("eachRecord() heads have lengths %d, want %d; first %q", len(heads), len(want), heads[0])
	}
}

func TestDirtSkipsTheOriginOfARename(t *testing.T) {
	f := newFixture(t)
	f.run(f.worktree, "mv", "feature.txt", "renamed.txt")
	f.write(filepath.Join(f.worktree, "scratch.txt"), "scratch\n")

	dirt, err := f.relocator.dirt(context.Background(), f.git, f.worktree)
	if err != nil {
		t.Fatalf("dirt() error = %v", err)
	}
	if want := "uncommitted changes: renamed.txt, scratch.txt"; dirt != want {
		t.Errorf("dirt() = %q, want %q", dirt, want)
	}
}

func TestIntendRefusesAnotherRepository(t *testing.T) {
	f := newFixture(t)
	f.run(f.root, "init", "-q", "-b", "main", "elsewhere")
	elsewhere := filepath.Join(f.root, "elsewhere", ".git")

	_, err := f.relocator.Intend(context.Background(), 1, cleanup.DeferRequest{
		Worktree: f.worktree, CommonDir: elsewhere, Owner: "stack", Git: f.git,
	})

	var refused *cleanup.RefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("Intend() error = %v, want a *RefusedError", err)
	}
	want := cleanup.RefusedError{
		Worktree: f.worktree,
		Reason:   "mismatch",
		Detail:   fmt.Sprintf("%s belongs to %s, not %s", f.worktree, f.common, elsewhere),
	}
	if *refused != want {
		t.Errorf("Intend() refused %+v\nwant %+v", *refused, want)
	}
	f.unjournaled()
}

func TestIntendRefusesAnUnregisteredTree(t *testing.T) {
	f := newFixture(t)

	_, err := f.relocator.Intend(context.Background(), 1, cleanup.DeferRequest{
		Worktree: f.repo, CommonDir: f.common, Owner: "stack", Git: f.git,
	})

	var refused *cleanup.RefusedError
	if !errors.As(err, &refused) || refused.Reason != "main" {
		t.Fatalf("Intend() error = %v, want a main refusal", err)
	}
	f.unjournaled()
}

func TestIntendJournalsByActivity(t *testing.T) {
	tests := []struct {
		name      string
		guard     func(f *fixture) error
		cancelled bool
		wantPhase cleanup.Phase
		wantNote  func(f *fixture) string
	}{
		{"idle tree queues", func(*fixture) error { return nil }, false, cleanup.PhaseQueued, nil},
		{"held tree waits", func(f *fixture) error {
			return &cleanup.ActiveError{Worktree: f.worktree, Holders: []cleanup.Holder{{PID: 7, Name: "vim", Evidence: cleanup.EvidenceFD, Path: filepath.Join(f.worktree, "feature.txt")}}}
		}, false, cleanup.PhaseWaiting, func(f *fixture) string {
			return f.worktree + " is in use: vim (pid 7) holding open " + filepath.Join(f.worktree, "feature.txt")
		}},
		{"uninspected tree waits", func(*fixture) error { return errors.New("process table unreadable") }, false, cleanup.PhaseWaiting, func(f *fixture) string {
			return "could not verify that " + f.worktree + " is idle: process table unreadable"
		}},
		{"cancelled inspection journals nothing", func(*fixture) error { return fmt.Errorf("list processes: %w", context.Canceled) }, true, "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			verdict := tt.guard(f)
			f.guard = func(context.Context, string) error {
				if tt.cancelled {
					cancel()
				}
				return verdict
			}
			f.write(filepath.Join(f.worktree, "scratch.txt"), "scratch\n")
			tree := f.id(f.worktree)

			job, err := f.relocator.Intend(ctx, 1, cleanup.DeferRequest{
				Worktree: f.worktree, CommonDir: f.common, Owner: "stack", Git: f.git,
			})

			if got := f.id(f.worktree); got != tree {
				t.Errorf("worktree identity = %v, want %v", got, tree)
			}
			if tt.cancelled {
				if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(job, cleanup.Job{}) {
					t.Fatalf("Intend() = %+v, %v; want no job and %v", job, err, context.Canceled)
				}
				f.unjournaled()
				return
			}
			if err != nil {
				t.Fatalf("Intend() error = %v", err)
			}
			if job.Phase != tt.wantPhase || !job.Deferred || job.Owner != "stack" || job.Seq != 1 {
				t.Errorf("Intend() = phase %s deferred %t owner %q seq %d, want %s, true, stack, 1", job.Phase, job.Deferred, job.Owner, job.Seq, tt.wantPhase)
			}
			var wantErrors []cleanup.JobError
			if tt.wantNote != nil {
				wantErrors = []cleanup.JobError{{At: f.clock, Phase: cleanup.PhaseWaiting, Message: tt.wantNote(f)}}
			}
			if !reflect.DeepEqual(job.Errors, wantErrors) {
				t.Errorf("Intend() errors = %+v, want %+v", job.Errors, wantErrors)
			}
			if job.Blocked != nil {
				t.Errorf("Intend() blocked = %+v, want none", job.Blocked)
			}
			f.stored(&job)
			f.absent(job.Registered)
		})
	}
}
