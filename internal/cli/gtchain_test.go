package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/nativemeta"
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
	f, heads := gtUntrackedChainRepo(t)
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
}

func TestStackSubmitNeverAdoptsARemoteOnlyBranchBelowTheTip(t *testing.T) {
	f, heads := gtUntrackedChainRepo(t, "a", "b")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "c")
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	cmd.SetContext(f.Context())

	if err := stackAdoptChain(cmd, render.Dir(f.Dir)); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if want := "tracked c onto main\n"; out.String() != want {
		t.Errorf("report = %q, want %q", out.String(), want)
	}
	state, err := gtStateQuery(t.Context(), render.Dir(f.Dir), "test")
	if err != nil {
		t.Fatalf("gt state: %v", err)
	}
	if s := state["c"]; len(s.Parents) != 1 || s.Parents[0].Ref != "main" || s.Parents[0].SHA != heads["main"] {
		t.Errorf("gt state c parents = %v, want main at %s", s.Parents, shortOID(heads["main"]))
	}
	if got := gitAt(t, f.Env(), f.Dir, "for-each-ref", "--format=%(refname)", "refs/heads/a", "refs/heads/b"); got != "" {
		t.Errorf("local refs = %q, want no local copy of a remote-only branch", got)
	}
}

func TestShipTracksABranchOnTrunkBesideARemoteOnlyRef(t *testing.T) {
	for name, past := range map[string]bool{"at its commit": false, "below its commit": true} {
		t.Run(name, func(t *testing.T) {
			f := shipGTRepo(t)
			shipGTUntracked(t, f, "feature")
			mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "HEAD:refs/heads/feature-red")
			if past {
				writeShipFile(t, f.Dir, "fix.txt", "fix\n")
				mustRun(t, f.Env(), f.Dir, "git", "add", "fix.txt")
				mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "fix")
			}
			shipGTReady(t, f)

			got, err := runShipCmd(f.Context(), t, "-m", "fix: frobnicate", "--no-push")
			if err != nil {
				t.Fatalf("ship: %v", err)
			}
			if want := "tracked feature onto main" + shipSep; !strings.HasPrefix(got, want) {
				t.Errorf("summary = %q, want it to open %q", got, want)
			}
			state, err := gtStateQuery(t.Context(), render.Dir(f.Dir), "test")
			if err != nil {
				t.Fatalf("gt state: %v", err)
			}
			if _, tracked := state["feature-red"]; tracked {
				t.Error("gt tracks feature-red, a branch only the remote holds")
			}
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
	mustRun(t, f.Env(), f.Dir, "git", "branch", "a-twin", "a")
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

func TestStackSubmitAdoptsOntoAParentRewrittenSinceTheCut(t *testing.T) {
	f := shipGTRepo(t, vcstest.GTStack("base", "feature"))
	cut := gitAt(t, f.Env(), f.Dir, "rev-parse", "feature")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "-c", "child", "feature")
	writeShipFile(t, f.Dir, "child.txt", "child\n")
	mustRun(t, f.Env(), f.Dir, "git", "add", "child.txt")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "child")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "feature")
	writeShipFile(t, f.Dir, "feature.txt", "amended feature\n")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qa", "--amend", "--no-edit")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "child")
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	cmd.SetContext(f.Context())

	if err := stackAdoptChain(cmd, render.Dir(f.Dir)); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if want := "tracked child onto feature\n"; out.String() != want {
		t.Errorf("report = %q, want %q", out.String(), want)
	}
	state, err := gtStateQuery(t.Context(), render.Dir(f.Dir), "test")
	if err != nil {
		t.Fatalf("gt state: %v", err)
	}
	if s := state["child"]; len(s.Parents) != 1 || s.Parents[0].Ref != "feature" || s.Parents[0].SHA != cut {
		t.Errorf("gt state child parents = %v, want feature at the cut %s", s.Parents, shortOID(cut))
	}
}

func TestStackSubmitAdoptsTheUntrackedBranchTheTipWasCutFromAtItsCommit(t *testing.T) {
	for name, cut := range map[string][][]string{
		"named":     {{"git", "branch", "d", "c"}, {"git", "switch", "-q", "d"}},
		"from HEAD": {{"git", "switch", "-q", "-c", "d"}},
	} {
		t.Run(name, func(t *testing.T) {
			f, heads := gtUntrackedChainRepo(t)
			for _, argv := range cut {
				mustRun(t, f.Env(), f.Dir, argv[0], argv[1:]...)
			}
			var out bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetOut(&out)
			cmd.SetContext(f.Context())

			if err := stackAdoptChain(cmd, render.Dir(f.Dir)); err != nil {
				t.Fatalf("adopt: %v", err)
			}
			if want := strings.Join([]string{"tracked a onto main", "tracked b onto a", "tracked c onto b", "tracked d onto c"}, shipSep) + "\n"; out.String() != want {
				t.Errorf("report = %q, want %q", out.String(), want)
			}
			assertGTChainAdopted(t, f, heads)
		})
	}
}

func TestStackSubmitRefusesAnUntrackedBranchAtTheTipsCommitWithNoCutRecorded(t *testing.T) {
	f, _ := gtUntrackedChainRepo(t)
	mustRun(t, f.Env(), f.Dir, "git", "update-ref", "refs/heads/x", "HEAD")
	cmd := &cobra.Command{}
	cmd.SetContext(f.Context())

	err := stackAdoptChain(cmd, render.Dir(f.Dir))
	if err == nil || !strings.Contains(err.Error(), "c sits at the same commit as untracked x") {
		t.Fatalf("adopt = %v, want a refusal naming x", err)
	}
}

// gtRewrittenBaseRepo is base, a native stack member with an open pull request
// that another clone rebased onto a newer main after this one pushed it, so its
// local head, which another worktree holds, trails origin's; lane, an
// untracked branch at the main commit between; and routes, a pushed branch
// with no pull request cut from base's published head, which only origin holds.
func gtRewrittenBaseRepo(t *testing.T) (*vcstest.Fixture, map[string]string) {
	t.Helper()
	f := shipRepo(t, vcstest.Remote())
	f.Decorate(func(ctx context.Context) context.Context { return withStackProvider(ctx, true) })
	mustRun(t, f.Env(), f.Dir, "git", "config", "ccx.stack", "github")
	trunk := shipHead(t, f)
	shipGTUntracked(t, f, "base")
	heads := map[string]string{"stale": shipHead(t, f)}
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "base")
	common := filepath.Join(f.Dir, ".git")
	if err := nativemeta.Init(f.Context(), common, "main"); err != nil {
		t.Fatal(err)
	}
	if err := nativemeta.AdoptRoot(f.Context(), common, "base", "main", trunk, heads["stale"]); err != nil {
		t.Fatal(err)
	}
	stackAdvanceTrunk(t, f, "lane.txt", "lane\n")
	mustRun(t, f.Env(), f.Dir, "git", "branch", "lane", "origin/main")
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "--detach", "origin/main")
	writeShipFile(t, f.Dir, "base.txt", "base, rebased elsewhere\n")
	mustRun(t, f.Env(), f.Dir, "git", "add", "base.txt")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-q", "-m", "base", "-m", "why base")
	heads["published"] = shipHead(t, f)
	mustRun(t, f.Env(), f.Dir, "git", "push", "-qf", "origin", "HEAD:refs/heads/base")
	shipGTUntracked(t, f, "routes")
	heads["routes"] = shipHead(t, f)
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "routes")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "--detach")
	mustRun(t, f.Env(), f.Dir, "git", "branch", "-qD", "routes")
	mustRun(t, f.Env(), f.Dir, "git", "worktree", "add", "-q", f.WorktreePath("base"), "base")
	api := newGTAPIStub(t)
	api.openPRs("base")
	api.bases["base"], api.stacks["base"] = "main", 46
	f.Decorate(api.ctx)
	stubStackPRs(t, f, map[string]*stackPR{"base": {Number: api.prs["base"], Title: "base", State: "OPEN", Base: "main", Head: heads["published"], Mergeable: "MERGEABLE"}})
	return f, heads
}

func assertGTOnPublishedBase(t *testing.T, f *vcstest.Fixture, heads map[string]string, records map[string]string) {
	t.Helper()
	state, err := gtStateQuery(t.Context(), render.Dir(f.Dir), "test")
	if err != nil {
		t.Fatalf("gt state: %v", err)
	}
	for branch, parent := range records {
		if s := state[branch]; len(s.Parents) != 1 || s.Parents[0].Ref != parent {
			t.Errorf("gt state %s parents = %v, want %s", branch, s.Parents, parent)
		}
	}
	for branch := range records {
		if s := state[branch]; s.Parents[0].Ref == "base" && s.Parents[0].SHA != heads["published"] {
			t.Errorf("gt state %s sits on base at %s, want base's published head %s", branch, shortOID(s.Parents[0].SHA), shortOID(heads["published"]))
		}
	}
	if _, tracked := state["lane"]; tracked {
		t.Error("gt tracks lane, a branch at a main commit below base's published head")
	}
	for where, dir := range map[string]string{"local": f.Dir, "origin": f.RemoteDir} {
		want := heads["stale"]
		if where == "origin" {
			want = heads["published"]
		}
		if got := gitAt(t, f.Env(), dir, "rev-parse", "base"); got != want {
			t.Errorf("%s base = %s, want it left at %s", where, shortOID(got), shortOID(want))
		}
	}
}

func TestStackSubmitAdoptsOntoTheHeadAnotherCloneRebasedItsBaseTo(t *testing.T) {
	f, heads := gtRewrittenBaseRepo(t)
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "routes")
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	cmd.SetContext(f.Context())

	if err := stackAdoptChain(cmd, render.Dir(f.Dir)); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if want := "tracked routes onto base\n"; out.String() != want {
		t.Errorf("report = %q, want %q", out.String(), want)
	}
	assertGTOnPublishedBase(t, f, heads, map[string]string{"routes": "base"})
}

func TestStackNewStacksOnAPushedBranchOnTheHeadAnotherCloneRebasedItsBaseTo(t *testing.T) {
	f, heads := gtRewrittenBaseRepo(t)
	child := filepath.Join(t.TempDir(), "sidecar")

	out, errOut, err := runStackCmd(t, f, "new", "sidecar", "--parent", "routes", "--full-history", "--path", child)
	if err != nil {
		t.Fatalf("stack new --parent routes: %v\n%s%s", err, out, errOut)
	}
	assertGTOnPublishedBase(t, f, heads, map[string]string{"routes": "base", "sidecar": "routes"})
}

func TestShipTracksABranchOnTheHeadAnotherCloneRebasedItsBaseTo(t *testing.T) {
	for _, tt := range []struct {
		name, from, parent string
		flags              []string
	}{
		{name: "cut from a pushed branch with no pull request", from: "origin/routes", parent: "routes"},
		{name: "--parent names that branch", from: "origin/routes", parent: "routes", flags: []string{"--parent", "routes"}},
		{name: "cut from base's published head", from: "origin/base", parent: "base"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f, heads := gtRewrittenBaseRepo(t)
			mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "--detach", tt.from)
			shipGTUntracked(t, f, "links")
			links := shipHead(t, f)
			shipGTReady(t, f)
			args := append([]string{"-m", "fix: frobnicate", "--no-pr", "--no-watch"}, tt.flags...)

			report := dryRunReport(t, f, args...)
			if refusals := dryRunValues(report, "refuses"); len(refusals) != 0 {
				t.Errorf("dry run refuses = %q, want none\n%s", refusals, report)
			}
			if got := dryRunValues(report, "parent"); len(got) != 1 || !strings.HasPrefix(got[0], tt.parent+shipSep) {
				t.Errorf("dry run parent = %q, want links on %s\n%s", got, tt.parent, report)
			}

			if _, errStr, err := runShipCmdFull(f.Context(), t, args...); err != nil {
				t.Fatalf("ship = %v (stderr=%q)", err, errStr)
			}
			if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "links~1"); got != links {
				t.Errorf("links was replayed: its commit before the new one is %s, want %s", shortOID(got), shortOID(links))
			}
			records := map[string]string{"links": tt.parent}
			if tt.parent == "routes" {
				records["routes"] = "base"
			}
			assertGTOnPublishedBase(t, f, heads, records)
		})
	}
}

func TestGTMergeBaseNamesWhyNoCommonCommitExists(t *testing.T) {
	f := shipRepo(t, vcstest.Remote())
	shipGTUntracked(t, f, "feat")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "feat")
	shallow := filepath.Join(t.TempDir(), "shallow")
	mustRun(t, f.Env(), filepath.Dir(shallow), "git", "clone", "-q", "--no-local", "--depth", "1", "--branch", "main", f.RemoteDir, shallow)
	mustRun(t, f.Env(), shallow, "git", "fetch", "-q", "--depth", "1", "origin", "feat:feat")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "--orphan", "lone")
	writeShipFile(t, f.Dir, "lone.txt", "lone\n")
	mustRun(t, f.Env(), f.Dir, "git", "add", "lone.txt")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-q", "-m", "lone")

	for _, tt := range []struct{ name, dir, branch, want string }{
		{name: "shallow clone", dir: shallow, branch: "feat", want: "test: git merge-base main feat found no common commit in this shallow clone — fetch the missing history with git fetch --deepen=<commits>, then run again"},
		{name: "unrelated histories", dir: f.Dir, branch: "lone", want: "test: git merge-base main lone found no common commit: the two share no history"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := gtMergeBase(f.Context(), render.Dir(tt.dir), "test", "main", tt.branch)
			if err == nil || err.Error() != tt.want {
				t.Errorf("gtMergeBase error = %v, want %q", err, tt.want)
			}
		})
	}

	base, err := gtMergeBaseDeepening(f.Context(), render.Dir(shallow), "test", "origin", "main", "feat")
	if want := gitAt(t, f.Env(), f.Dir, "rev-parse", "main"); err != nil || base != want {
		t.Errorf("gtMergeBaseDeepening = %q, %v, want %s once the shallow clone is deepened", base, err, want)
	}
}

func TestStackQueueRestackBaseDeepensAShallowCloneBeforeCountingCommits(t *testing.T) {
	f := shipRepo(t, vcstest.Remote())
	fork := gitAt(t, f.Env(), f.Dir, "rev-parse", "main")
	for _, name := range []string{"t1", "t2", "t3"} {
		writeShipFile(t, f.Dir, name+".txt", name+"\n")
		mustRun(t, f.Env(), f.Dir, "git", "add", name+".txt")
		mustRun(t, f.Env(), f.Dir, "git", "commit", "-q", "-m", name)
	}
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "-c", "ours", fork)
	for _, name := range []string{"f1", "f2"} {
		writeShipFile(t, f.Dir, name+".txt", name+"\n")
		mustRun(t, f.Env(), f.Dir, "git", "add", name+".txt")
		mustRun(t, f.Env(), f.Dir, "git", "commit", "-q", "-m", name)
	}
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "-c", "theirs", "main")
	mustRun(t, f.Env(), f.Dir, "git", "cherry-pick", fork+"..ours")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "main", "ours", "theirs")
	shallow := filepath.Join(t.TempDir(), "shallow")
	mustRun(t, f.Env(), filepath.Dir(shallow), "git", "clone", "-q", "--no-local", "--depth", "1", "--branch", "main", f.RemoteDir, shallow)
	mustRun(t, f.Env(), shallow, "git", "fetch", "-q", "--depth", "1", "origin", "ours:ours", "theirs:theirs")

	base, err := stackQueueRestackBase(f.Context(), render.Dir(shallow), "origin", "main", "theirs", "ours")
	if want := gitAt(t, f.Env(), f.Dir, "rev-parse", "main"); err != nil || base != want {
		t.Errorf("stackQueueRestackBase = %q, %v, want main %s once the shallow clone reaches the fork", base, err, want)
	}
}
