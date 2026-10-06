package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcstest"
)

func stackPlantClaims(t *testing.T, f *vcstest.Fixture, pid int, started string, age time.Duration, claims []string, branches ...stackRebaseBranch) *stackRebaseRun {
	t.Helper()
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	run := &stackRebaseRun{Trunk: "main", Roots: []string{branches[0].Name}, Claims: claims, Branches: branches, Pid: pid, Started: started, Host: host, Publishing: true, dir: stackRunDir(filepath.Join(f.Dir, ".git"), claims[0])}
	if err := os.MkdirAll(run.dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := stackSaveRun(run); err != nil {
		t.Fatal(err)
	}
	then := time.Now().Add(-age)
	if err := os.Chtimes(stackStatePath(run.dir), then, then); err != nil {
		t.Fatal(err)
	}
	return run
}

func stackExitedPid(t *testing.T) int {
	t.Helper()
	exited := exec.Command("true")
	if err := exited.Run(); err != nil {
		t.Fatal(err)
	}
	return exited.Process.Pid
}

func TestStackRebaseProceedsBesideALiveRunWritingAnotherBranch(t *testing.T) {
	t.Parallel()
	f := stackRebaseRepo(t, "base", "feature")
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	live := exec.Command("sleep", "60")
	if err := live.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = live.Process.Kill(); _ = live.Wait() })
	stackPlantClaims(t, f, live.Process.Pid, stackProcStart(live.Process.Pid), 0, []string{"sibling"},
		stackRebaseBranch{Name: "base", Kept: true}, stackRebaseBranch{Name: "sibling"})

	if _, _, err := runStackCmd(t, f, "rebase", "--no-push"); err != nil {
		t.Fatalf("rebase beside a live run of a sibling lane = %v, want it to proceed", err)
	}
	if !stackOnto(t, f, "origin/main", "base") {
		t.Error("base is not on the new trunk")
	}
	runs, err := stackRuns(filepath.Join(f.Dir, ".git"))
	if err != nil || len(runs) != 1 || !slices.Equal(runs[0].Claims, []string{"sibling"}) {
		t.Fatalf("runs = %v, %v, want only the sibling lane's live run kept", runs, err)
	}
}

func TestStackRebaseRefusalNamesTheRunsBranchesAndItsCommands(t *testing.T) {
	t.Parallel()
	f := stackRebaseRepo(t, "base", "feature")
	stackPlantLive(t, f, "base", "feature")

	_, _, err := runStackCmd(t, f, "rebase", "--no-push")
	want := "writing base, feature — finish it with ccx vcs stack continue --stack base, or drop it with ccx vcs stack abort --stack base"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("rebase beside a live run of its branches = %v, want %q", err, want)
	}
}

func TestStackRebaseDiscardsAnExitedRunWhoseBranchesAreGone(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		landed bool
		local  bool
		want   string
	}{
		{name: "landed", landed: true, local: true, want: "since every branch it writes is gone (shipped landed as #31026)"},
		{name: "deleted", want: "since every branch it writes is gone (shipped deleted)"},
		{name: "open", local: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := shipGTRepo(t, vcstest.GTStack("base", "feature"))
			prs := map[string]*stackPR{"shipped": {Number: 31026, State: "OPEN"}}
			if tc.landed {
				prs["shipped"] = &stackPR{Number: 31026, State: "MERGED", Landed: true}
			}
			stubOpenPRs(t, f, prs, "base", "feature")
			if tc.local {
				mustRun(t, f.Env(), f.Dir, "git", "branch", "shipped", "main")
			}
			stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
			pid := stackExitedPid(t)
			stackPlantClaims(t, f, pid, "", time.Minute, []string{"shipped"}, stackRebaseBranch{Name: "shipped"})

			out, _, err := runStackCmd(t, f, "rebase", "--no-push")
			if err != nil {
				t.Fatalf("rebase beside an exited run of another branch = %v", err)
			}
			runs, err := stackRuns(filepath.Join(f.Dir, ".git"))
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				if strings.Contains(out, "discarded") || len(runs) != 1 {
					t.Fatalf("out = %q, runs = %v, want the exited run of an open branch kept", out, runs)
				}
				return
			}
			line := "discarded the stack rebase of shipped (pid " + strconv.Itoa(pid) + " on "
			if !strings.Contains(out, line) || !strings.Contains(out, tc.want) {
				t.Errorf("out = %q, want %q naming %q", out, line, tc.want)
			}
			stackAssertNoRun(t, f)
		})
	}
}

func TestStackRunForPicksTheRunThatClaimsTheNamedBranch(t *testing.T) {
	t.Parallel()
	first := &stackRebaseRun{Roots: []string{"bottom"}, Claims: []string{"left"}}
	second := &stackRebaseRun{Roots: []string{"bottom"}, Claims: []string{"right"}}
	runs := []*stackRebaseRun{first, second}

	if got, err := stackRunFor(runs, "right", "", ""); err != nil || got != second {
		t.Fatalf("stackRunFor(right) = %v, %v, want the run claiming right", got, err)
	}
	_, err := stackRunFor(runs, "bottom", "", "")
	if want := "2 stack rebases of bottom are in progress — name one: --stack left, --stack right"; err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("stackRunFor(bottom) = %v, want %q", err, want)
	}
}

func TestStackReclaimAbandonedKeepsAClaimWhoseOwnerIsRecorded(t *testing.T) {
	t.Parallel()
	common := t.TempDir()
	run := &stackRebaseRun{Roots: []string{"bottom"}, Claims: []string{"left", "right"}, Pid: 1}
	if err := stackClaim(common, run); err != nil {
		t.Fatal(err)
	}
	if err := stackSaveRun(run); err != nil {
		t.Fatal(err)
	}
	secondary := stackRunDir(common, "right")
	then := time.Now().Add(-stackStaleAfter - time.Minute)
	if err := os.Chtimes(secondary, then, then); err != nil {
		t.Fatal(err)
	}

	if err := stackReclaimAbandoned(common, secondary); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(secondary); err != nil {
		t.Fatalf("claim of a recorded run was reclaimed: %v", err)
	}
	if err := os.Remove(stackStatePath(run.dir)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(secondary, then, then); err != nil {
		t.Fatal(err)
	}
	if err := stackReclaimAbandoned(common, secondary); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(secondary); !os.IsNotExist(err) {
		t.Fatalf("claim whose owner left no state = %v, want it reclaimed", err)
	}
}

func TestStackWritesClaimsAKeptBranchItAligns(t *testing.T) {
	t.Parallel()
	run := &stackRebaseRun{Branches: []stackRebaseBranch{
		{Name: "landed", Landed: "#1 landed"},
		{Name: "aligned", Kept: true, Local: "a", Head: "a"},
		{Name: "behind", Kept: true, Local: "a", Head: "b"},
		{Name: "moved", Local: "a", Head: "a"},
	}}
	if got, want := stackWrites(run), []string{"behind", "moved"}; !slices.Equal(got, want) {
		t.Fatalf("stackWrites = %v, want %v", got, want)
	}
	run.NoPush = true
	if got, want := stackWrites(run), []string{"aligned", "behind", "moved"}; !slices.Equal(got, want) {
		t.Fatalf("stackWrites(--no-push) = %v, want %v", got, want)
	}
}

func TestStackSettledKeepsAReplayTheLandingLacks(t *testing.T) {
	t.Parallel()
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		landed  string
		settled bool
	}{
		{name: "landed an older head", landed: "published"},
		{name: "landed the replay", landed: "replayed", settled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			run := &stackRebaseRun{
				Trunk: "main", Roots: []string{"shipped"}, Claims: []string{"shipped"}, Pid: stackExitedPid(t), Host: host, Publishing: true,
				Branches: []stackRebaseBranch{{Name: "shipped", Local: "published", Head: "published", NewHead: "replayed"}},
			}
			ctx := withStackPRs(t.Context(), func(context.Context, render.Dir, string, []string) (map[string]*stackPR, error) {
				return map[string]*stackPR{"shipped": {Number: 7, Head: tc.landed, Landed: true}}, nil
			})
			settled, err := stackSettled(ctx, render.Dir(t.TempDir()), []*stackRebaseRun{run})
			if err != nil {
				t.Fatal(err)
			}
			if got := settled[run] != ""; got != tc.settled {
				t.Fatalf("settled = %q, want settled %v", settled[run], tc.settled)
			}
		})
	}
}

func TestStackAdoptReportsARunThatEndedMeanwhile(t *testing.T) {
	t.Parallel()
	run := &stackRebaseRun{Roots: []string{"feature"}, Pid: 1}
	if err := stackClaim(t.TempDir(), run); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(run.dir); err != nil {
		t.Fatal(err)
	}
	if err := stackAdopt(run); !errors.Is(err, errStackRunGone) {
		t.Fatalf("stackAdopt of a removed run = %v, want errStackRunGone", err)
	}
}

func TestStackGateTreatsARunThatEndedMeanwhileAsFinished(t *testing.T) {
	t.Parallel()
	f := stackRebaseRepo(t, "base", "feature")
	stackPlantRun(t, f, stackStaleAfter+time.Minute, "base")
	common := filepath.Join(f.Dir, ".git")
	runs, err := stackRuns(common)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs = %v, %v, want the planted run", runs, err)
	}
	if err := os.RemoveAll(runs[0].dir); err != nil {
		t.Fatal(err)
	}
	l, err := resolveLane(f.Context(), stackRebasePrefix, workingDir(f.Context()), false)
	if err != nil {
		t.Fatal(err)
	}
	cmd := newStackCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)

	done, err := stackGate(f.Context(), cmd, l, common, runs[0], false)
	if err != nil || !done || !strings.Contains(out.String(), "the stack rebase of base ended meanwhile") {
		t.Fatalf("stackGate over a removed run = %v, %v, %q, want it finished", done, err, out.String())
	}
}
