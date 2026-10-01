package cli

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/vcs"
	"github.com/yasyf/cc-context/internal/vcstest"
)

func TestShipPRUnknownBaseRefusesBeforeMutation(t *testing.T) {
	for _, tt := range []struct {
		name     string
		noCommit bool
	}{
		{name: "commit"},
		{name: "already committed", noCommit: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := shipPRFixture(t, vcstest.Branch("feature"), vcstest.NoOriginHead())
			shipPRCreated(t)
			if tt.noCommit {
				mustRun(t, f.Env(), f.Dir, "git", "add", "f.txt")
				mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "waiting for publication")
			}
			before := gitAt(t, f.Env(), f.Dir, "rev-parse", "HEAD")
			beforeStatus := gitAt(t, f.Env(), f.Dir, "status", "--porcelain")
			shipResetLog(t, f)
			args := []string{"--no-gt", "--no-verify", "--no-watch", "--pr-title", "fix: known branch"}
			if tt.noCommit {
				args = append(args, "--no-commit")
			} else {
				args = append(args, "-m", "fix: known branch")
			}
			_, err := runShipCmd(f.Context(), t, args...)
			if !errors.Is(err, vcs.ErrNoTrunk) || !strings.Contains(err.Error(), "--parent") {
				t.Errorf("ship without a base = %v, want actionable no-trunk refusal", err)
			}
			calls := vcstest.Invocations(t, f.ArgvLog)
			assertNoShipMutation(t, calls)
			for _, argv := range calls {
				if len(argv) >= 4 && slices.Equal(argv[:4], []string{"gh", "api", "-X", "POST"}) {
					t.Errorf("unresolved base reached GitHub creation: %v", argv)
				}
			}
			if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "HEAD"); got != before {
				t.Errorf("refusal changed HEAD from %s to %s", before, got)
			}
			if got := gitAt(t, f.Env(), f.Dir, "status", "--porcelain"); got != beforeStatus {
				t.Errorf("refusal changed working copy from %q to %q", beforeStatus, got)
			}
		})
	}
}

func TestShipPRUnknownTrunkAcceptsExplicitBase(t *testing.T) {
	f := shipPRFixture(t, vcstest.Branch("feature"), vcstest.NoOriginHead())
	shipPRCreated(t)
	_, err := runShipCmd(f.Context(), t, "-m", "fix: known branch", "--parent", "main", "--no-gt", "--no-verify", "--no-watch", "--pr-title", "fix: known branch")
	if err != nil {
		t.Fatal(err)
	}
	posts := 0
	for _, argv := range vcstest.Invocations(t, f.ArgvLog) {
		if len(argv) >= 4 && slices.Equal(argv[:4], []string{"gh", "api", "-X", "POST"}) {
			posts++
			if !slices.Contains(argv, "base=main") {
				t.Errorf("explicit base was lost: %v", argv)
			}
		}
	}
	if posts != 1 {
		t.Errorf("created %d pull requests, want one", posts)
	}
}
