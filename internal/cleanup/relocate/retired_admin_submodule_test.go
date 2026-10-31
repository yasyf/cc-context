package relocate

import (
	"path/filepath"
	"testing"

	"github.com/yasyf/cc-context/internal/cleanup"
)

func TestDisplacedResumeRejectsSubmodulesInitializedAfterCapture(t *testing.T) {
	for _, initialized := range []bool{false, true} {
		for _, force := range []bool{false, true} {
			name := "embedded"
			if initialized {
				name = "admin modules"
			}
			if force {
				name += " forced"
			}
			t.Run(name, func(t *testing.T) {
				f := newFixture(t)
				sub := filepath.Join(f.worktree, "vendor", "sub")
				f.write(filepath.Join(f.worktree, ".gitmodules"), "[submodule \"sub\"]\n\tpath = vendor/sub\n\turl = "+f.repo+"\n")
				head := f.run(f.repo, "rev-parse", "HEAD")
				f.run(f.worktree, "update-index", "--add", "--cacheinfo", "160000,"+head+",vendor/sub")
				f.run(f.worktree, "add", ".gitmodules")
				f.run(f.worktree, "commit", "-q", "-m", "uninitialized submodule")
				job := f.intend()
				job.Force = force
				if err := f.journal.Save(job); err != nil {
					t.Fatal(err)
				}
				admin := moveCapturedAdmin(t, f, job)
				if initialized {
					f.run(f.worktree, "-c", "protocol.file.allow=always", "submodule", "update", "--init")
				} else {
					f.run(f.repo, "clone", "--no-local", f.repo, sub)
				}
				if got := f.run(f.worktree, "status", "--porcelain=v1", "--ignore-submodules=none"); got != "" {
					t.Fatalf("status = %q, want clean", got)
				}
				if got := f.run(f.worktree, "rev-parse", "HEAD"); got != job.Head {
					t.Fatalf("HEAD = %q, captured %q", got, job.Head)
				}
				retireFixture(t, f, &job)
				if job.Blocked != nil || job.Retirement != cleanup.RetirementComplete {
					t.Fatalf("retirement = %+v", job)
				}
				pointer := f.read(filepath.Join(job.Original, ".git"))
				f.advance(&job)
				if job.Blocked == nil || job.Blocked.Reason != "submodules" || job.Phase != cleanup.PhaseQueued {
					t.Fatalf("resume = %+v, want logical submodules refusal", job)
				}
				if f.id(job.Original) != job.Tree || f.id(admin) != job.Admin {
					t.Fatal("resume changed captured objects")
				}
				if got := f.read(filepath.Join(job.Original, ".git")); got != pointer {
					t.Fatalf("pointer = %q, want %q", got, pointer)
				}
				if got := f.run(sub, "rev-parse", "HEAD"); got != head {
					t.Fatalf("submodule HEAD = %q, want %q", got, head)
				}
				f.absent(job.Payload)
				f.stored(&job)
			})
		}
	}
}
