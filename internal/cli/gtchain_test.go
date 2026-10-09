package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcstest"
)

func gtUntrackedChainRepo(t *testing.T, remoteOnly ...string) (*vcstest.Fixture, map[string]string) {
	t.Helper()
	f := shipGTRepo(t)
	heads := map[string]string{"main": gitAt(t, f.Env(), f.Dir, "rev-parse", "main")}
	for _, name := range []string{"a", "b", "c"} {
		shipGTUntracked(t, f, name)
		heads[name] = shipHead(t, f)
	}
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "a", "b", "c")
	for _, name := range []string{"a", "b", "c"} {
		mustRun(t, f.Env(), f.Dir, "git", "update-ref", "-d", "refs/remotes/origin/"+name)
	}
	if len(remoteOnly) > 0 {
		mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "main")
		mustRun(t, f.Env(), f.Dir, "git", append([]string{"branch", "-D"}, remoteOnly...)...)
	}
	return f, heads
}

func assertGTChainAdopted(t *testing.T, f *vcstest.Fixture, heads map[string]string) {
	t.Helper()
	state, err := gtStateQuery(t.Context(), render.Dir(f.Dir), "test")
	if err != nil {
		t.Fatalf("gt state: %v", err)
	}
	for child, parent := range map[string]string{"a": "main", "b": "a", "c": "b"} {
		s := state[child]
		if len(s.Parents) != 1 || s.Parents[0].Ref != parent || s.Parents[0].SHA != heads[parent] {
			t.Errorf("gt state %s parents = %v, want %s at %s", child, s.Parents, parent, shortOID(heads[parent]))
		}
		if s.NeedsRestack {
			t.Errorf("gt state %s needs a restack after adoption", child)
		}
		if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "refs/heads/"+child); got != heads[child] {
			t.Errorf("local %s = %s, want its pushed head %s", child, got, heads[child])
		}
	}
}

func TestStackSubmitAdoptsAnUntrackedChain(t *testing.T) {
	for name, remoteOnly := range map[string][]string{"local": nil, "remote-only": {"a", "b"}} {
		t.Run(name, func(t *testing.T) {
			f, heads := gtUntrackedChainRepo(t, remoteOnly...)
			if len(remoteOnly) > 0 {
				mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "c")
			}
			var out bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetOut(&out)
			cmd.SetContext(f.Context())

			if err := stackAdoptChain(cmd, render.Dir(f.Dir)); err != nil {
				t.Fatalf("adopt: %v", err)
			}
			if want := strings.Join([]string{"tracked a onto main", "tracked b onto a", "tracked c onto b"}, shipSep) + "\n"; out.String() != want {
				t.Errorf("report = %q, want %q", out.String(), want)
			}
			assertGTChainAdopted(t, f, heads)
		})
	}
}

func TestWorktreeAddAdoptsARemoteOnlyChain(t *testing.T) {
	f, heads := gtUntrackedChainRepo(t, "a", "b", "c")
	f.Isolate(t)

	out, err := runWorktreeCmd(t, f, "add", "c")
	if err != nil {
		t.Fatalf("worktree add c: %v", err)
	}
	for _, seg := range []string{"tracked a onto main", "tracked b onto a", "tracked c onto b"} {
		if !strings.Contains(out, shipSep+seg+shipSep) {
			t.Errorf("summary = %q, want it to name %q", out, seg)
		}
	}
	if got := gitAt(t, f.Env(), worktreeSummaryPath(t, out), "branch", "--show-current"); got != "c" {
		t.Errorf("the new working copy is on %q, want c", got)
	}
	assertGTChainAdopted(t, f, heads)
}

func TestStackSubmitRefusesTwoUntrackedBranchesAtOneCommit(t *testing.T) {
	f, _ := gtUntrackedChainRepo(t)
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "a:refs/heads/a-twin")
	cmd := &cobra.Command{}
	cmd.SetContext(f.Context())

	err := stackAdoptChain(cmd, render.Dir(f.Dir))
	if err == nil || !strings.Contains(err.Error(), "c sits on untracked a, a-twin, b above main") {
		t.Fatalf("adopt = %v, want a refusal naming every candidate", err)
	}
	state, err := gtStateQuery(t.Context(), render.Dir(f.Dir), "test")
	if err != nil {
		t.Fatalf("gt state: %v", err)
	}
	for _, name := range []string{"a", "b", "c"} {
		if _, tracked := state[name]; tracked {
			t.Errorf("gt tracks %s after the refusal", name)
		}
	}
}
