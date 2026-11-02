package cli

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcstest"
)

func stackBotRestack(t *testing.T, f *vcstest.Fixture) {
	t.Helper()
	clone := filepath.Join(t.TempDir(), "bot")
	restackRun(t, f, filepath.Dir(clone), "git", "clone", "-q", f.RemoteDir, clone)
	restackRun(t, f, clone, "git", "config", "user.email", "bot@b.b")
	restackRun(t, f, clone, "git", "config", "user.name", "bot")
	restackRun(t, f, clone, "git", "switch", "-q", "base")
	restackRun(t, f, clone, "git", "rebase", "-q", "origin/main")
	restackRun(t, f, clone, "git", "switch", "-q", "feature")
	restackRun(t, f, clone, "git", "rebase", "-q", "--onto", "base", "origin/base")
	restackRun(t, f, clone, "git", "push", "-qf", "origin", "base", "feature")
}

// TestShipTipOnlyFollowsABotRestackOfItsStack is monorepo #48253: Trunk
// restacked its parent onto newer trunk twice after #48244 merged, and the lane
// had committed onto the first restack of its own branch, which no ref or
// reflog of the parent remembers.
func TestShipTipOnlyFollowsABotRestackOfItsStack(t *testing.T) {
	for _, tc := range []struct {
		name         string
		twice, amend bool
	}{
		{"restacked once", false, false},
		{"restacked twice, the lane on the first", true, false},
		{"restacked once, its commit amended here", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := shipGTRepo(t)
			shipGTStack(t, f, "base", "feature")
			if _, _, err := runStackCmd(t, f, "submit"); err != nil {
				t.Fatalf("stack submit: %v", err)
			}
			stubStackPRs(t, f, map[string]*stackPR{
				"base":    {Number: 100, Title: "base", State: "OPEN", Base: "main", Mergeable: "MERGEABLE"},
				"feature": {Number: 101, Title: "feature", State: "OPEN", Base: "base", Mergeable: "MERGEABLE"},
			})
			local := gitAt(t, f.Env(), f.Dir, "rev-parse", "base")
			if tc.twice {
				restackAdvanceRemote(t, f, "main", "first.txt", "first\n")
				stackBotRestack(t, f)
				mustRun(t, f.Env(), f.Dir, "git", "fetch", "-q", "origin", "+refs/heads/feature:refs/remotes/origin/feature")
				mustRun(t, f.Env(), f.Dir, "git", "reset", "-q", "--hard", "origin/feature")
			}
			if tc.amend {
				writeShipFile(t, f.Dir, "feature.txt", "feat ure\n")
				mustRun(t, f.Env(), f.Dir, "git", "commit", "-qa", "--amend", "--no-edit")
			}
			stackCommit(t, f, "new.txt")
			restackAdvanceRemote(t, f, "main", "second.txt", "second\n")
			stackBotRestack(t, f)
			base := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base")
			restacked := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature")
			shipResetLog(t, f)

			if _, errStr, err := runShipCmdFull(f.Context(), t, "--no-commit", "--no-watch", "--tip-only"); err != nil {
				t.Fatalf("ship --tip-only after a bot restack = %v (stderr=%q)", err, errStr)
			}
			if refs := gtPushedRefs(shipGTInvocations(t, f)); !slices.Equal(refs, []string{"feature"}) {
				t.Errorf("pushed refs = %v, want only feature", refs)
			}
			if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base"); got != base {
				t.Errorf("origin base moved from %s to %s", base, got)
			}
			if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "base"); got != local {
				t.Errorf("local base moved from %s to %s", local, got)
			}
			feature := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature")
			if tc.amend {
				if got := gitAt(t, f.Env(), f.Dir, "show", feature+":feature.txt"); got != "feat ure" {
					t.Errorf("origin feature.txt = %q, want the local amendment kept over the bot's restack of the old commit", got)
				}
				if !stackOnto(t, f, base, feature) || stackOnto(t, f, restacked, feature) {
					t.Errorf("origin feature %.12s, want it replayed onto base's restacked head %.12s, not onto its own restack %.12s", feature, base, restacked)
				}
			} else {
				if got := gitAt(t, f.Env(), f.Dir, "rev-parse", feature+"^"); got != restacked {
					t.Errorf("origin feature %.12s sits on %.12s, want only its new commit fast-forwarded onto the restacked %.12s", feature, got, restacked)
				}
				if files := gitAt(t, f.Env(), f.Dir, "diff", "--name-only", restacked, feature); files != "new.txt" {
					t.Errorf("origin feature adds %q over its restacked head, want only new.txt", files)
				}
			}
			state, err := gtStateQuery(f.Context(), render.Dir(f.Dir), "test")
			if err != nil {
				t.Fatal(err)
			}
			if got := state["feature"].Parents; len(got) != 1 || got[0] != (gtRef{Ref: "base", SHA: base}) {
				t.Errorf("feature's recorded parent = %v, want base at its restacked head %.12s", got, base)
			}
		})
	}
}
