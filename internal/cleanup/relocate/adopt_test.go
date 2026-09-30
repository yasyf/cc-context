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

const (
	legacyDate = "20260928"
	legacyID   = "0123456789abcdef0123"
)

type quarantine struct {
	*fixture
	source  string
	ref     string
	head    string
	tree    cleanup.FileID
	request cleanup.AdoptRequest
}

func newQuarantine(t *testing.T) *quarantine {
	t.Helper()
	f := newFixture(t)
	head := f.run(f.worktree, "rev-parse", "HEAD")
	source := filepath.Join(f.root, cleanup.LegacyQuarantinePrefix+legacyDate, legacyID+"-wt")
	if err := os.Mkdir(filepath.Dir(source), 0o755); err != nil {
		t.Fatalf("create quarantine: %v", err)
	}
	if err := os.Rename(f.worktree, source); err != nil {
		t.Fatalf("park the tree: %v", err)
	}
	if err := os.RemoveAll(f.adminDir); err != nil {
		t.Fatalf("drop the registration: %v", err)
	}
	ref := cleanup.LegacyRecoveryPrefix + legacyDate + "/" + legacyID
	f.run(f.repo, "update-ref", ref, head)
	tree := f.id(source)
	return &quarantine{
		fixture: f,
		source:  source,
		ref:     ref,
		head:    head,
		tree:    tree,
		request: cleanup.AdoptRequest{
			Source:      source,
			Tree:        tree,
			CommonDir:   f.common,
			Head:        head,
			RecoveryRef: ref,
			Original:    f.worktree,
			Owner:       "legacy quarantine import",
			Git:         f.git,
		},
	}
}

func (q *quarantine) adopt() cleanup.Job {
	q.t.Helper()
	job, err := q.relocator.Adopt(context.Background(), 1, q.request)
	if err != nil {
		q.t.Fatalf("Adopt() error = %v", err)
	}
	return job
}

func (q *quarantine) register() {
	q.t.Helper()
	if err := os.Mkdir(q.adminDir, 0o755); err != nil {
		q.t.Fatalf("recreate admin dir: %v", err)
	}
	q.write(filepath.Join(q.adminDir, "gitdir"), filepath.Join(q.source, ".git")+"\n")
}

func (q *quarantine) refs() string {
	q.t.Helper()
	return q.run(q.repo, "for-each-ref")
}

func TestAdoptJournalsAPreparedJobAndMovesNothing(t *testing.T) {
	q := newQuarantine(t)
	refs := q.refs()

	job, err := q.relocator.Adopt(context.Background(), 7, q.request)
	if err != nil {
		t.Fatalf("Adopt() error = %v", err)
	}

	want := cleanup.Job{
		Schema:      cleanup.Schema,
		ID:          job.ID,
		Seq:         7,
		Phase:       cleanup.PhasePrepared,
		Adopted:     true,
		Source:      q.source,
		Owner:       "legacy quarantine import",
		Repo:        q.common,
		Original:    q.worktree,
		Registered:  q.layout.Registered(job.ID),
		Payload:     q.layout.Payload(job.ID),
		Tree:        q.tree,
		Head:        q.head,
		RecoveryRef: q.ref,
		Git:         q.git,
		Created:     q.clock,
		Updated:     q.clock,
	}
	if !reflect.DeepEqual(job, want) {
		t.Errorf("Adopt() = %+v\nwant %+v", job, want)
	}
	q.stored(&job)
	if got := q.id(q.source); got != q.tree {
		t.Errorf("source identity = %v, want the tree %v", got, q.tree)
	}
	q.absent(job.Payload)
	q.absent(job.Registered)
	if got := q.refs(); got != refs {
		t.Errorf("refs = %q, want them unchanged %q", got, refs)
	}
	if want := []string{q.source}; !reflect.DeepEqual(q.guarded, want) {
		t.Errorf("guard consulted for %v, want %v", q.guarded, want)
	}
	if want := []string{q.source}; !reflect.DeepEqual(q.watchers.checked, want) {
		t.Errorf("quarantine checked %v, want %v", q.watchers.checked, want)
	}
	if len(q.watchers.retired) != 0 {
		t.Errorf("watchers retired %v, want none", q.watchers.retired)
	}
}

func TestAdvanceRenamesAnAdoptedTreeIntoItsPayload(t *testing.T) {
	q := newQuarantine(t)
	refs := q.refs()
	config := q.read(filepath.Join(q.common, "config"))
	job := q.adopt()

	q.advance(&job)

	q.finished(&job)
	q.absent(q.source)
	q.absent(job.Registered)
	if got := q.id(job.Payload); got != q.tree {
		t.Errorf("payload identity = %v, want the tree %v", got, q.tree)
	}
	if got := q.read(filepath.Join(job.Payload, "feature.txt")); got != "feature\n" {
		t.Errorf("payload feature.txt = %q, want %q", got, "feature\n")
	}
	if got := q.run(q.repo, "rev-parse", q.ref); got != q.head {
		t.Errorf("legacy ref = %s, want %s", got, q.head)
	}
	if got := q.refs(); got != refs {
		t.Errorf("refs = %q, want them unchanged %q", got, refs)
	}
	if got := q.read(filepath.Join(q.common, "config")); got != config {
		t.Errorf("repository config = %q, want it unchanged %q", got, config)
	}
	if got, want := q.listing(), q.mainEntry(); got != want {
		t.Errorf("worktree list = %q, want %q", got, want)
	}
	if info, err := os.Stat(filepath.Dir(q.source)); err != nil || !info.IsDir() {
		t.Errorf("quarantine directory: %v, want it left in place", err)
	}
	if want := []string{q.source, q.source}; !reflect.DeepEqual(q.guarded, want) {
		t.Errorf("guard consulted for %v, want %v", q.guarded, want)
	}
	if want := []string{q.source, q.source, q.layout.JobDir(job.ID)}; !reflect.DeepEqual(q.watchers.checked, want) {
		t.Errorf("quarantine checked %v, want %v", q.watchers.checked, want)
	}
	if len(q.watchers.retired) != 0 {
		t.Errorf("watchers retired %v, want none", q.watchers.retired)
	}
}

func TestAdoptRefusals(t *testing.T) {
	tests := []struct {
		name    string
		arrange func(q *quarantine) (detail string)
		reason  string
	}{
		{"the quarantine carries another date", func(q *quarantine) string {
			q.request.RecoveryRef = cleanup.LegacyRecoveryPrefix + "20260927/" + legacyID
			return fmt.Sprintf("cleanup: adopt request: source %q is not inside a codex-worktree-trash-20260927 quarantine", q.source)
		}, ""},
		{"the ref pins another tree", func(q *quarantine) string {
			q.request.RecoveryRef = cleanup.LegacyRecoveryPrefix + legacyDate + "/ffffffffffffffffffff"
			return fmt.Sprintf("cleanup: adopt request: source %q is not the tree %s pins", q.source, q.request.RecoveryRef)
		}, ""},
		{"the ref is not a legacy ref", func(q *quarantine) string {
			q.request.RecoveryRef = "refs/heads/feature"
			return `cleanup: adopt request: recovery_ref "refs/heads/feature" is not refs/cleanup-worktrees/<date>/<id>`
		}, ""},
		{"the source is reached through a symlink", func(q *quarantine) string {
			alias := filepath.Join(q.root, "alias")
			if err := os.Symlink(q.root, alias); err != nil {
				q.t.Fatalf("symlink: %v", err)
			}
			q.request.Source = filepath.Join(alias, filepath.Base(filepath.Dir(q.source)), filepath.Base(q.source))
			return fmt.Sprintf("%s is not canonical: it resolves to %s", q.request.Source, q.source)
		}, "identity"},
		{"the source is itself a symlink", func(q *quarantine) string {
			q.request.Source = filepath.Join(filepath.Dir(q.source), legacyID+"-link")
			if err := os.Symlink(q.source, q.request.Source); err != nil {
				q.t.Fatalf("symlink: %v", err)
			}
			return fmt.Sprintf("%s is not canonical: it resolves to %s", q.request.Source, q.source)
		}, "identity"},
		{"the source is another inode", func(q *quarantine) string {
			q.request.Tree.Ino++
			return fmt.Sprintf("%s is directory %s, not the tree %s the request names", q.source, idText(q.tree), idText(q.request.Tree))
		}, "identity"},
		{"the source is a repository root", func(q *quarantine) string {
			if err := os.Remove(filepath.Join(q.source, ".git")); err != nil {
				q.t.Fatalf("remove .git: %v", err)
			}
			if err := os.Mkdir(filepath.Join(q.source, ".git"), 0o755); err != nil {
				q.t.Fatalf("mkdir .git: %v", err)
			}
			return q.source + " is a repository's main working copy, not a parked worktree"
		}, "main"},
		{"the source is a working copy with a separate git directory", func(q *quarantine) string {
			separate := filepath.Join(q.root, "separate.git")
			if err := os.Mkdir(separate, 0o755); err != nil {
				q.t.Fatalf("mkdir: %v", err)
			}
			q.write(filepath.Join(separate, "HEAD"), "ref: refs/heads/main\n")
			q.write(filepath.Join(q.source, ".git"), "gitdir: "+separate+"\n")
			return fmt.Sprintf("%s is the working copy of the repository at %s, not a parked worktree", q.source, separate)
		}, "main"},
		{"the source is a git directory", func(q *quarantine) string {
			if err := os.Remove(filepath.Join(q.source, ".git")); err != nil {
				q.t.Fatalf("remove .git: %v", err)
			}
			q.write(filepath.Join(q.source, "HEAD"), "ref: refs/heads/main\n")
			if err := os.Mkdir(filepath.Join(q.source, "objects"), 0o755); err != nil {
				q.t.Fatalf("mkdir: %v", err)
			}
			return q.source + " is a git directory, not a parked worktree"
		}, "main"},
		{"the tree is still registered", func(q *quarantine) string {
			q.register()
			return fmt.Sprintf("%s is still a registered worktree: %s links back to %s", q.source, q.adminDir, filepath.Join(q.source, ".git"))
		}, "registered"},
		{"another admin entry names the tree", func(q *quarantine) string {
			other := filepath.Join(q.common, "worktrees", "other")
			if err := os.Mkdir(other, 0o755); err != nil {
				q.t.Fatalf("mkdir: %v", err)
			}
			q.write(filepath.Join(other, "gitdir"), filepath.Join(q.source, ".git")+"\n")
			return fmt.Sprintf("%s still registers a worktree at %s", q.common, q.source)
		}, "registered"},
		{"a worktree is registered inside", func(q *quarantine) string {
			inner := filepath.Join(q.source, "inner")
			q.run(q.repo, "worktree", "add", "-q", "-b", "inner", inner)
			return fmt.Sprintf("%s still registers a worktree at %s", q.common, inner)
		}, "registered"},
		{"the source holds the repository", func(q *quarantine) string {
			q.run(q.source, "init", "-q", "-b", "main", "held")
			q.request.CommonDir = filepath.Join(q.source, "held", ".git")
			return fmt.Sprintf("%s holds the repository %s", q.source, q.request.CommonDir)
		}, "mismatch"},
		{"the source holds the repository under another spelling", func(q *quarantine) string {
			q.run(q.source, "init", "-q", "-b", "main", "held")
			quarantine := filepath.Dir(q.source)
			respelled := filepath.Join(filepath.Dir(quarantine), strings.ToUpper(filepath.Base(quarantine)), filepath.Base(q.source), "held", ".git")
			if _, err := os.Stat(respelled); err != nil {
				q.t.Skip("the volume is case-sensitive: a directory has one spelling")
			}
			q.request.CommonDir = respelled
			return fmt.Sprintf("%s holds the repository %s", q.source, respelled)
		}, "mismatch"},
		{"the source holds the cleanup folder", func(q *quarantine) string {
			q.open(filepath.Join(q.source, "state"))
			return fmt.Sprintf("%s overlaps the cleanup folder %s", q.source, q.layout.Root)
		}, "mismatch"},
		{"the source sits inside the cleanup folder", func(q *quarantine) string {
			parent := filepath.Join(q.layout.Root, filepath.Base(filepath.Dir(q.source)))
			if err := os.Mkdir(parent, 0o755); err != nil {
				q.t.Fatalf("mkdir: %v", err)
			}
			inside := filepath.Join(parent, filepath.Base(q.source))
			if err := os.Rename(q.source, inside); err != nil {
				q.t.Fatalf("rename: %v", err)
			}
			q.source, q.request.Source = inside, inside
			return fmt.Sprintf("%s overlaps the cleanup folder %s", inside, q.layout.Root)
		}, "mismatch"},
		{"the source sits inside the repository", func(q *quarantine) string {
			parent := filepath.Join(q.common, filepath.Base(filepath.Dir(q.source)))
			if err := os.Mkdir(parent, 0o755); err != nil {
				q.t.Fatalf("mkdir: %v", err)
			}
			inside := filepath.Join(parent, filepath.Base(q.source))
			if err := os.Rename(q.source, inside); err != nil {
				q.t.Fatalf("rename: %v", err)
			}
			q.source, q.request.Source = inside, inside
			return fmt.Sprintf("%s lies inside the repository %s", inside, q.common)
		}, "mismatch"},
		{"the common dir is not a git directory", func(q *quarantine) string {
			q.request.CommonDir = q.root
			return q.root + " is not a git directory: it has no HEAD"
		}, "mismatch"},
		{"the ref is gone", func(q *quarantine) string {
			q.run(q.repo, "update-ref", "-d", q.ref)
			return fmt.Sprintf("%s names no commit in %s", q.ref, q.common)
		}, "recovery"},
		{"the ref resolves to another commit", func(q *quarantine) string {
			base := q.run(q.repo, "rev-parse", "refs/heads/main")
			q.run(q.repo, "update-ref", q.ref, base)
			return fmt.Sprintf("%s resolves to %s in %s, not the saved head %s", q.ref, base, q.common, q.head)
		}, "recovery"},
		{"the quarantine is watched", func(q *quarantine) string {
			q.watchers.quarantine = func(context.Context, string) error { return errors.New("the quarantine sits under a watched root") }
			return "the quarantine sits under a watched root"
		}, "watched"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := newQuarantine(t)
			detail := tt.arrange(q)
			refs := q.refs()

			_, err := q.relocator.Adopt(context.Background(), 1, q.request)

			var refused *cleanup.RefusedError
			switch {
			case tt.reason == "":
				if err == nil || err.Error() != detail || errors.As(err, &refused) {
					t.Errorf("Adopt() error = %v, want the plain error %q", err, detail)
				}
			case !errors.As(err, &refused):
				t.Fatalf("Adopt() error = %v, want a *cleanup.RefusedError", err)
			default:
				if want := (cleanup.RefusedError{Worktree: q.request.Source, Reason: tt.reason, Detail: detail}); *refused != want {
					t.Errorf("Adopt() refused %+v\nwant %+v", *refused, want)
				}
			}
			entries, err := os.ReadDir(q.layout.JobsDir())
			if err != nil || len(entries) != 0 {
				t.Errorf("jobs dir holds %d entries, error %v, want none", len(entries), err)
			}
			if got := q.id(q.source); got != q.tree {
				t.Errorf("source identity = %v, want the tree %v", got, q.tree)
			}
			if got := q.read(filepath.Join(q.source, "feature.txt")); got != "feature\n" {
				t.Errorf("source feature.txt = %q, want %q", got, "feature\n")
			}
			if got := q.refs(); got != refs {
				t.Errorf("refs = %q, want them unchanged %q", got, refs)
			}
		})
	}
}

func TestAdoptGuardActive(t *testing.T) {
	q := newQuarantine(t)
	active := q.holdAt(q.source)

	_, err := q.relocator.Adopt(context.Background(), 1, q.request)

	if err != error(active) {
		t.Errorf("Adopt() error = %v, want the guard's own %v", err, active)
	}
	q.unjournaled()
	if got := q.id(q.source); got != q.tree {
		t.Errorf("source identity = %v, want the tree %v", got, q.tree)
	}
}

func TestAdoptGuardUnclear(t *testing.T) {
	q := newQuarantine(t)
	unreadable := errors.New("process table unreadable")
	q.guard = func(context.Context, string) error { return unreadable }

	_, err := q.relocator.Adopt(context.Background(), 1, q.request)

	var (
		refused *cleanup.RefusedError
		active  *cleanup.ActiveError
	)
	if !errors.Is(err, unreadable) || errors.As(err, &refused) || errors.As(err, &active) {
		t.Fatalf("Adopt() error = %v, want a plain error wrapping the guard's", err)
	}
	if want := "cleanup: could not verify that " + q.source + " is idle: process table unreadable"; err.Error() != want {
		t.Errorf("Adopt() error = %q, want %q", err, want)
	}
	q.unjournaled()
	if got := q.id(q.source); got != q.tree {
		t.Errorf("source identity = %v, want the tree %v", got, q.tree)
	}
}

func TestAdvanceAdoptedResumesAfterTheRename(t *testing.T) {
	q := newQuarantine(t)
	refs := q.refs()
	job := q.adopt()
	if err := os.Rename(q.source, job.Payload); err != nil {
		t.Fatalf("rename: %v", err)
	}
	guarded, checked := len(q.guarded), len(q.watchers.checked)
	marker := q.trap(&job)

	q.advance(&job)

	q.finished(&job)
	q.absent(marker)
	q.absent(q.source)
	if got := q.id(job.Payload); got != q.tree {
		t.Errorf("payload identity = %v, want the tree %v", got, q.tree)
	}
	if len(q.guarded) != guarded || len(q.watchers.checked) != checked {
		t.Errorf("guard consulted %d and quarantine checked %d more times for an already relocated tree, want none", len(q.guarded)-guarded, len(q.watchers.checked)-checked)
	}
	if got := q.refs(); got != refs {
		t.Errorf("refs = %q, want them unchanged %q", got, refs)
	}
}

func TestAdvanceAdoptedLeavesAReplacementAtTheSource(t *testing.T) {
	q := newQuarantine(t)
	job := q.adopt()
	aside := filepath.Join(q.root, "aside")
	if err := os.Rename(q.source, aside); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := os.Mkdir(q.source, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	q.write(filepath.Join(q.source, "mine.txt"), "mine\n")
	replacement := q.id(q.source)
	guarded := len(q.guarded)
	marker := q.trap(&job)

	q.advance(&job)

	q.blocked(&job, cleanup.PhasePrepared, "reconcile", fmt.Sprintf(
		"source %s is directory %s; payload %s is absent; the job captured tree %s",
		q.source, idText(replacement), job.Payload, idText(q.tree),
	))
	q.absent(marker)
	q.absent(job.Payload)
	if got := q.id(q.source); got != replacement {
		t.Errorf("replacement identity = %v, want %v", got, replacement)
	}
	if got := q.read(filepath.Join(q.source, "mine.txt")); got != "mine\n" {
		t.Errorf("mine.txt = %q, want %q", got, "mine\n")
	}
	if got := q.id(aside); got != q.tree {
		t.Errorf("set-aside identity = %v, want the tree %v", got, q.tree)
	}
	if len(q.guarded) != guarded {
		t.Errorf("guard consulted %d more times for a tree that is not the job's, want none", len(q.guarded)-guarded)
	}
}

func TestAdvanceAdoptedBlocks(t *testing.T) {
	tests := []struct {
		name        string
		arrange     func(q *quarantine, job cleanup.Job) (detail string)
		reason      string
		wantPayload bool
	}{
		{"the ref moved", func(q *quarantine, _ cleanup.Job) string {
			base := q.run(q.repo, "rev-parse", "refs/heads/main")
			q.run(q.repo, "update-ref", q.ref, base)
			return fmt.Sprintf("%s resolves to %s in %s, not the saved head %s", q.ref, base, q.common, q.head)
		}, "identity", false},
		{"the ref is gone", func(q *quarantine, _ cleanup.Job) string {
			q.run(q.repo, "update-ref", "-d", q.ref)
			return fmt.Sprintf("%s names no commit in %s", q.ref, q.common)
		}, "identity", false},
		{"the tree was registered again", func(q *quarantine, _ cleanup.Job) string {
			q.register()
			return fmt.Sprintf("%s is still a registered worktree: %s links back to %s", q.source, q.adminDir, filepath.Join(q.source, ".git"))
		}, "reconcile", false},
		{"a worktree was registered inside", func(q *quarantine, _ cleanup.Job) string {
			inner := filepath.Join(q.source, "inner")
			q.run(q.repo, "worktree", "add", "-q", "-b", "inner", inner)
			return fmt.Sprintf("%s still registers a worktree at %s", q.common, inner)
		}, "reconcile", false},
		{"the job folder was registered", func(q *quarantine, job cleanup.Job) string {
			return "payload is registered as " + q.enroll("squat", filepath.Join(job.Payload, ".git"))
		}, "reconcile", false},
		{"a path inside the job folder was registered", func(q *quarantine, job cleanup.Job) string {
			named := filepath.Join(job.Registered, ".git")
			return fmt.Sprintf("%s registers %s", q.enroll("squat", named), named)
		}, "reconcile", false},
		{"the quarantine became watched", func(q *quarantine, _ cleanup.Job) string {
			q.watchers.quarantine = func(_ context.Context, dir string) error {
				if dir == q.source {
					return errors.New("the quarantine sits under a watched root")
				}
				return nil
			}
			return "the quarantine sits under a watched root"
		}, "quarantine", false},
		{"the job folder is watched", func(q *quarantine, job cleanup.Job) string {
			q.watchers.quarantine = func(_ context.Context, dir string) error {
				if dir == q.layout.JobDir(job.ID) {
					return errors.New("job folder sits under a watched root")
				}
				return nil
			}
			return "job folder sits under a watched root"
		}, "quarantine", false},
		{"the tree became active", func(q *quarantine, _ cleanup.Job) string {
			return q.holdAt(q.source).Error()
		}, "activity", false},
		{"activity is unclear", func(q *quarantine, _ cleanup.Job) string {
			q.guard = func(context.Context, string) error { return errors.New("process table unreadable") }
			return "could not verify that " + q.source + " is idle: process table unreadable"
		}, "activity", false},
		{"something already sits at the payload", func(q *quarantine, job cleanup.Job) string {
			if err := os.Mkdir(job.Payload, 0o755); err != nil {
				q.t.Fatalf("mkdir: %v", err)
			}
			return fmt.Sprintf(
				"source %s is directory %s; payload %s is directory %s; the job captured tree %s",
				q.source, idText(q.tree), job.Payload, idText(q.id(job.Payload)), idText(q.tree),
			)
		}, "reconcile", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := newQuarantine(t)
			job := q.adopt()
			detail := tt.arrange(q, job)
			refs := q.refs()

			q.advance(&job)

			q.blocked(&job, cleanup.PhasePrepared, tt.reason, detail)
			if got := q.id(q.source); got != q.tree {
				t.Errorf("source identity = %v, want the tree %v", got, q.tree)
			}
			if got := q.read(filepath.Join(q.source, "feature.txt")); got != "feature\n" {
				t.Errorf("source feature.txt = %q, want %q", got, "feature\n")
			}
			if !tt.wantPayload {
				q.absent(job.Payload)
			}
			if got := q.refs(); got != refs {
				t.Errorf("refs = %q, want them unchanged %q", got, refs)
			}
			if len(q.watchers.retired) != 0 {
				t.Errorf("watchers retired %v, want none", q.watchers.retired)
			}
		})
	}
}

func TestAdvanceAdoptedReturnsTheContextErrorWithoutBlocking(t *testing.T) {
	q := newQuarantine(t)
	job := q.adopt()
	record := q.read(q.layout.RecordPath(job.ID))
	want := job
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q.guard = func(ctx context.Context, _ string) error {
		cancel()
		return ctx.Err()
	}

	err := q.relocator.Advance(ctx, &job)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Advance() error = %v, want context.Canceled", err)
	}
	if !reflect.DeepEqual(job, want) {
		t.Errorf("job = %+v, want it unchanged %+v", job, want)
	}
	if got := q.read(q.layout.RecordPath(job.ID)); got != record {
		t.Errorf("record = %q, want it unchanged %q", got, record)
	}
	if got := q.id(q.source); got != q.tree {
		t.Errorf("source identity = %v, want the tree %v", got, q.tree)
	}
	q.absent(job.Payload)
}

func TestAdvanceAdoptedBlocksWhenTheRenamedPayloadIsRegistered(t *testing.T) {
	q := newQuarantine(t)
	job := q.adopt()
	if err := os.Rename(q.source, job.Payload); err != nil {
		t.Fatalf("rename: %v", err)
	}
	repointed := filepath.Join(job.Payload, ".git") + "\n"
	if err := os.Mkdir(q.adminDir, 0o755); err != nil {
		t.Fatalf("recreate admin dir: %v", err)
	}
	q.write(filepath.Join(q.adminDir, "gitdir"), repointed)
	guarded := len(q.guarded)
	marker := q.trap(&job)

	q.advance(&job)

	q.blocked(&job, cleanup.PhasePrepared, "reconcile", "payload is registered as "+q.adminDir)
	q.absent(marker)
	q.absent(q.source)
	if got := q.id(job.Payload); got != q.tree {
		t.Errorf("payload identity = %v, want the tree %v", got, q.tree)
	}
	if got := q.read(filepath.Join(job.Payload, "feature.txt")); got != "feature\n" {
		t.Errorf("payload feature.txt = %q, want %q", got, "feature\n")
	}
	if got := q.read(filepath.Join(q.adminDir, "gitdir")); got != repointed {
		t.Errorf("admin gitdir = %q, want it untouched %q", got, repointed)
	}
	if len(q.guarded) != guarded {
		t.Errorf("guard consulted %d more times, want none", len(q.guarded)-guarded)
	}
}
