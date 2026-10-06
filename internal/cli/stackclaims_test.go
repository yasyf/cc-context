package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

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
