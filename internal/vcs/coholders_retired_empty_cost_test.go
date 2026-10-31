package vcs

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/yasyf/cc-context/internal/render"
)

type retiredIncompleteCalls struct {
	HeadOpenat       int
	GitdirOpenat     int
	PayloadGitOpenat int
	AdminOpenat      int
}

func TestCoHoldersRetiredIncompleteGitdirCost(t *testing.T) {
	mode := os.Getenv("CCX_RETIRED_CENSUS_STRACE")
	if mode == "" {
		t.Skip("set CCX_RETIRED_CENSUS_STRACE=before or fixed to trace incomplete retired gitdir files")
	}
	if (mode != "before" && mode != "fixed") || runtime.GOOS != "linux" {
		t.Fatalf("CCX_RETIRED_CENSUS_STRACE requires before or fixed mode on Linux, got %q on %s", mode, runtime.GOOS)
	}
	strace, err := exec.LookPath("strace")
	if err != nil {
		t.Fatalf("CCX_RETIRED_CENSUS_STRACE requires strace: %v", err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	fixture, admins := newRetiredCensusFixture(t, 2)
	empty, missing := fixture.Unrelated[0], fixture.Unrelated[1]
	mustWriteFile(t, filepath.Join(admins[empty], "gitdir"), "")
	if err := os.Remove(filepath.Join(admins[missing], "gitdir")); err != nil {
		t.Fatal(err)
	}
	output := t.TempDir()
	if root := os.Getenv("CCX_RETIRED_CENSUS_STRACE_OUTPUT"); root != "" {
		output = filepath.Join(root, mode, "incomplete-gitdir")
		mustMkdir(t, output)
	}
	fixturePath := filepath.Join(output, "fixture.json")
	fixture.Result = filepath.Join(output, "result.json")
	tracePath := filepath.Join(output, "retired.strace")
	writeCoHoldersCostJSON(t, fixturePath, fixture)
	argv := []string{"-f", "-qq", "-s", "0", "-e", "trace=openat,newfstatat", "-o", tracePath, binary, "-test.run=^TestCoHoldersNamespaceWalkHelper$", "-test.count=1"}
	env := []string{"CCX_COHOLDER_STRACE_FIXTURE=" + fixturePath, "PATH=" + t.TempDir()}
	stdout, code, stderr, err := render.RunCLIExitCodeEnv(t.Context(), render.Ambient, strace, argv, env)
	if err != nil || code != 0 {
		t.Fatalf("trace incomplete retired CoHolders: code=%d error=%v\n%s%s", code, err, stdout, stderr)
	}
	var result coHoldersCostResult
	readCoHoldersCostJSON(t, fixture.Result, &result)
	if result.PID <= 0 || result.Branch != fixture.Branch || !reflect.DeepEqual(result.Holders, fixture.Holders) {
		t.Fatalf("CoHolders child = %+v, want branch %q, holders %q and a positive PID", result, fixture.Branch, fixture.Holders)
	}
	calls, pids := readRetiredCensusCalls(t, tracePath)
	selected := map[string]retiredCensusCalls{}
	for _, root := range fixture.Holders {
		pointer := filepath.Join(root, ".git")
		selected[pointer] = calls[pointer]
		if calls[pointer].Openat != 1 {
			t.Errorf("selected retired payload %q openat = %d, want 1", pointer, calls[pointer].Openat)
		}
	}
	controls := map[string]retiredIncompleteCalls{}
	for name, root := range map[string]string{"empty": empty, "missing": missing} {
		admin := admins[root]
		got := retiredIncompleteCalls{calls[filepath.Join(admin, "HEAD")].Openat, calls[filepath.Join(admin, "gitdir")].Openat, calls[filepath.Join(root, ".git")].Openat, calls[admin].Openat}
		controls[name] = got
		want := retiredIncompleteCalls{GitdirOpenat: 1}
		if name == "empty" && mode == "before" {
			want.AdminOpenat = 1
		}
		if got != want {
			t.Errorf("%s retired gitdir calls = %+v, want %+v", name, got, want)
		}
	}
	summary := struct {
		Mode      string
		ChildPID  int
		TracePIDs []int
		Selected  map[string]retiredCensusCalls
		Controls  map[string]retiredIncompleteCalls
	}{mode, result.PID, pids, selected, controls}
	writeCoHoldersCostJSON(t, filepath.Join(output, "summary.json"), summary)
	t.Logf("mode=%s child_pid=%d trace_pids=%v controls=%+v trace=%s", mode, result.PID, pids, controls, tracePath)
}
