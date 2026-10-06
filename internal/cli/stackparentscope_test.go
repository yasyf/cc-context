package cli

import (
	"strings"
	"testing"
)

func TestStackRebaseParentLeavesTheNamedParentsLaneWhereItIs(t *testing.T) {
	for _, args := range [][]string{nil, {"--no-push"}} {
		t.Run(strings.Join(append([]string{"rebase"}, args...), " "), func(t *testing.T) {
			t.Parallel()
			f := shipGTRepo(t)
			stubStackPRs(t, f, nil)
			shipGTStack(t, f, "yasyf/m-01", "yasyf/m-02")
			mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "main")
			shipGTStack(t, f, "yasyf/m-03")
			mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "yasyf/m-01", "yasyf/m-02", "yasyf/m-03")
			held := map[string]string{"yasyf/m-01": restackSiblingPath(t, "one"), "yasyf/m-02": restackSiblingPath(t, "two")}
			before := map[string]string{}
			for name, ws := range held {
				mustRun(t, f.Env(), f.Dir, "git", "worktree", "add", "-q", ws, name)
				before[name] = gitAt(t, f.Env(), f.Dir, "rev-parse", name)
			}
			stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")

			out, errOut, err := runStackCmd(t, f, append([]string{"rebase", "--parent", "yasyf/m-03=yasyf/m-02"}, args...)...)
			if err != nil {
				t.Fatalf("stack rebase: %v\n%s%s", err, out, errOut)
			}
			for name, ws := range held {
				if got := gitAt(t, f.Env(), f.Dir, "rev-parse", name); got != before[name] {
					t.Errorf("%s moved to %s, want it left at %s:\n%s", name, got, before[name], out)
				}
				if got := gitAt(t, f.Env(), ws, "rev-parse", "HEAD"); got != before[name] {
					t.Errorf("%s's worktree moved to %s", name, got)
				}
				if !strings.Contains(out, name+shipSep+"kept at its local head") {
					t.Errorf("plan = %q, want %s kept at its local head", out, name)
				}
			}
			if !stackOnto(t, f, "yasyf/m-02", "yasyf/m-03") {
				t.Errorf("yasyf/m-03 is not on yasyf/m-02:\n%s", out)
			}
			if got := stackParent(t, f, "yasyf/m-03"); got != "yasyf/m-02" {
				t.Errorf("gt parent of yasyf/m-03 = %s, want yasyf/m-02", got)
			}
		})
	}
}
