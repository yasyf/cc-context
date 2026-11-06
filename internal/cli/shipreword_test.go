package cli

import (
	"testing"

	"github.com/yasyf/cc-context/internal/vcstest"
)

// TestShipAmendRewordsAPublishedTipOnACleanWorkingCopy is dd-331634912-fix: a
// lane that only needed a trailer on its pushed tip found nothing in ship that
// rewords it, and amended with git by hand.
func TestShipAmendRewordsAPublishedTipOnACleanWorkingCopy(t *testing.T) {
	for _, tc := range []struct {
		name    string
		publish func(t *testing.T) (*vcstest.Fixture, string)
	}{
		{"git", func(t *testing.T) (*vcstest.Fixture, string) {
			f := shipRepo(t, vcstest.Remote(), vcstest.Dirty())
			if _, err := runShipCmd(f.Context(), t, "-m", "fix: frobnicate", "--no-watch"); err != nil {
				t.Fatalf("first ship: %v", err)
			}
			return f, "main"
		}},
		{"jj", func(t *testing.T) (*vcstest.Fixture, string) {
			f := shipRepo(t, vcstest.JJ(), vcstest.Remote(), vcstest.Dirty())
			if _, err := runShipCmd(f.Context(), t, "-m", "fix: frobnicate", "--new-branch=feature", "--no-pr", "--no-watch"); err != nil {
				t.Fatalf("first ship: %v", err)
			}
			return f, "feature"
		}},
		{"graphite", func(t *testing.T) (*vcstest.Fixture, string) {
			f := shipGTRepo(t)
			shipGTStack(t, f, "feature")
			if _, _, err := runStackCmd(t, f, "submit"); err != nil {
				t.Fatalf("stack submit: %v", err)
			}
			api := stubGTAPI(t)
			api.openPRs("feature")
			api.bases["feature"] = "main"
			f.Decorate(api.ctx)
			stubStackPRs(t, f, map[string]*stackPR{"feature": {Number: api.prs["feature"], Title: "feature", State: "OPEN", Base: "main"}})
			return f, "feature"
		}},
		{"github stack", func(t *testing.T) (*vcstest.Fixture, string) {
			f := stackNativeChain(t, "feature")
			stackNativePRs(t, f, "feature")
			return f, "feature"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, branch := tc.publish(t)
			published := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", branch)
			tree := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", branch+"^{tree}")
			parent := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", branch+"^")

			if out, err := runShipCmd(f.Context(), t, "--amend", "-m", "fix: frobnicate, reworded", "--no-watch"); err != nil {
				t.Fatalf("ship --amend -m on a clean working copy: %v\n%s", err, out)
			}
			if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", branch); got == published {
				t.Fatalf("origin %s stayed at %s, want the reworded tip pushed", branch, published)
			}
			if got := gitAt(t, f.Env(), f.RemoteDir, "log", "-1", "--format=%s", branch); got != "fix: frobnicate, reworded" {
				t.Errorf("origin %s subject = %q, want the new message", branch, got)
			}
			if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", branch+"^{tree}"); got != tree {
				t.Errorf("origin %s tree = %s, want %s unchanged by a reword", branch, got, tree)
			}
			if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", branch+"^"); got != parent {
				t.Errorf("origin %s parent = %s, want %s: the reword replaces the tip, not adds to it", branch, got, parent)
			}
		})
	}
}
