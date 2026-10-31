package vcs

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/render"
)

type retiredCensusCalls struct {
	Openat     int
	Newfstatat int
}

var retiredCensusCall = regexp.MustCompile(`^\s*([0-9]+)\s+(openat|newfstatat)\(AT_FDCWD,\s*("(?:\\.|[^"\\])*")`)

func TestCoHoldersRetiredCensusCost(t *testing.T) {
	mode := os.Getenv("CCX_RETIRED_CENSUS_STRACE")
	if mode == "" {
		t.Skip("set CCX_RETIRED_CENSUS_STRACE=before or fixed to trace real retired payload pointer calls")
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
	for _, unrelated := range []int{32, 1024} {
		t.Run(strconv.Itoa(unrelated), func(t *testing.T) {
			fixture, admins := newRetiredCensusFixture(t, unrelated)
			output := t.TempDir()
			if root := os.Getenv("CCX_RETIRED_CENSUS_STRACE_OUTPUT"); root != "" {
				output = filepath.Join(root, mode, strconv.Itoa(unrelated))
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
				t.Fatalf("trace retired CoHolders: code=%d error=%v\n%s%s", code, err, stdout, stderr)
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
				gitdir := filepath.Join(admins[root], "gitdir")
				selected[pointer] = calls[pointer]
				selected[admins[root]] = calls[admins[root]]
				selected[gitdir] = calls[gitdir]
				if calls[pointer].Openat != 1 || calls[gitdir].Openat != 1 || calls[admins[root]].Newfstatat != 2 {
					t.Errorf("selected retired pointer/gitdir/admin %q = %+v/%+v/%+v, want one pointer openat, one gitdir openat and two admin newfstatat", root, calls[pointer], calls[gitdir], calls[admins[root]])
				}
			}
			pointerCalls := retiredCensusCalls{}
			gitdirOpens := 0
			adminStats := 0
			headOpens := 0
			wantPointer, wantStats := 1, 2
			if mode == "fixed" {
				wantPointer, wantStats = 0, 0
			}
			for _, root := range fixture.Unrelated {
				pointer := filepath.Join(root, ".git")
				gitdir := filepath.Join(admins[root], "gitdir")
				pointerCalls.Openat += calls[pointer].Openat
				pointerCalls.Newfstatat += calls[pointer].Newfstatat
				gitdirOpens += calls[gitdir].Openat
				adminStats += calls[admins[root]].Newfstatat
				head := filepath.Join(admins[root], "HEAD")
				headOpens += calls[head].Openat
				if calls[pointer].Openat != wantPointer || calls[pointer].Newfstatat != 0 || calls[gitdir].Openat != 1 || calls[admins[root]].Newfstatat != wantStats || calls[head].Openat != 1 {
					t.Errorf("unrelated retired pointer/gitdir/admin/HEAD %q = %+v/%+v/%+v/%+v, want pointer openat %d, pointer newfstatat0, gitdir openat1, admin newfstatat%d and HEAD openat1", root, calls[pointer], calls[gitdir], calls[admins[root]], calls[head], wantPointer, wantStats)
				}
			}
			summary := struct {
				Mode                  string
				Unrelated             int
				ChildPID              int
				TracePIDs             []int
				Selected              map[string]retiredCensusCalls
				UnrelatedPointerCalls retiredCensusCalls
				UnrelatedGitdirOpens   int
				UnrelatedAdminStats   int
				UnrelatedHeadOpens    int
			}{mode, unrelated, result.PID, pids, selected, pointerCalls, gitdirOpens, adminStats, headOpens}
			writeCoHoldersCostJSON(t, filepath.Join(output, "summary.json"), summary)
			t.Logf("mode=%s unrelated=%d child_pid=%d trace_pids=%v unrelated_pointer_calls=%+v unrelated_gitdir_openat=%d unrelated_admin_newfstatat=%d trace=%s", mode, unrelated, result.PID, pids, pointerCalls, gitdirOpens, adminStats, tracePath)
		})
	}
}

func TestCoHoldersRetiredBacklinkErrorBoundary(t *testing.T) {
	for _, tc := range []struct {
		mutation  string
		wantError bool
	}{
		{"valid backlink", true},
		{"missing gitdir", false},
		{"gitdir directory", false},
		{"missing pointer", false},
		{"pointer directory", false},
		{"malformed pointer", false},
		{"missing target", false},
		{"different target", false},
	} {
		t.Run(tc.mutation, func(t *testing.T) {
			t.Setenv("PATH", t.TempDir())
			fixture, admins := newRetiredCensusFixture(t, 1)
			root := fixture.Unrelated[0]
			admin := admins[root]
			head := filepath.Join(admin, "HEAD")
			if err := os.Remove(head); err != nil {
				t.Fatal(err)
			}
			mustMkdir(t, head)
			var remove string
			switch tc.mutation {
			case "missing gitdir", "gitdir directory":
				remove = filepath.Join(admin, "gitdir")
			case "missing pointer", "pointer directory":
				remove = filepath.Join(root, ".git")
			}
			if remove != "" {
				if err := os.Remove(remove); err != nil {
					t.Fatal(err)
				}
				if strings.HasSuffix(tc.mutation, "directory") {
					mustMkdir(t, remove)
				}
			}
			switch tc.mutation {
			case "malformed pointer":
				mustWriteFile(t, filepath.Join(root, ".git"), "not a gitdir pointer\n")
			case "missing target":
				mustWriteFile(t, filepath.Join(root, ".git"), gitdirPrefix+filepath.Join(admin, "absent")+"\n")
			case "different target":
				mustWriteFile(t, filepath.Join(root, ".git"), gitdirPrefix+admins[fixture.Holders[1]]+"\n")
			}
			branch, holders, err := CoHolders(t.Context(), fixture.Checkout)
			all, allErr := retiredHoldings(fixture.Checkout.CommonDir)
			if tc.wantError {
				var pathError *os.PathError
				if branch != "" || holders != nil || !errors.As(err, &pathError) || pathError.Path != head {
					t.Fatalf("CoHolders = %q, %q, %v; want empty result and error reading %q", branch, holders, err, head)
				}
				if all != nil || !errors.As(allErr, &pathError) || pathError.Path != head {
					t.Fatalf("retiredHoldings = %q, %v; want empty result and error reading %q", all, allErr, head)
				}
				return
			}
			if err != nil || branch != fixture.Branch || !reflect.DeepEqual(holders, fixture.Holders) {
				t.Fatalf("CoHolders = %q, %q, %v; want %q, %q, nil", branch, holders, err, fixture.Branch, fixture.Holders)
			}
			if allErr != nil || len(all) != len(fixture.Holders) {
				t.Fatalf("retiredHoldings = %q, %v; want exactly the valid current/peer holders", all, allErr)
			}
		})
	}
}

func TestParseRetiredCensusCall(t *testing.T) {
	for _, tc := range []struct {
		line string
		pid  int
		op   string
		path string
	}{
		{`123 openat(AT_FDCWD, "/fixture/payload/.git", O_RDONLY|O_CLOEXEC) = 3`, 123, "openat", "/fixture/payload/.git"},
		{`  456 newfstatat(AT_FDCWD, "/fixture/admin", {st_mode=S_IFDIR|0700}, 0) = 0`, 456, "newfstatat", "/fixture/admin"},
		{`123 openat(AT_FDCWD, "/fixture/payload/.git", <unfinished ...>`, 123, "openat", "/fixture/payload/.git"},
		{`123 <... openat resumed> O_RDONLY|O_CLOEXEC) = 3`, 0, "", ""},
		{`123 newfstatat(3, "", {st_mode=S_IFREG|0600}, AT_EMPTY_PATH) = 0`, 0, "", ""},
	} {
		t.Run(tc.line, func(t *testing.T) {
			pid, op, path, err := parseRetiredCensusCall(tc.line)
			if err != nil || pid != tc.pid || op != tc.op || path != tc.path {
				t.Fatalf("parse retired census call = %d, %q, %q, %v; want %d, %q, %q, nil", pid, op, path, err, tc.pid, tc.op, tc.path)
			}
		})
	}
}

func newRetiredCensusFixture(t *testing.T, unrelated int) (coHoldersCostFixture, map[string]string) {
	t.Helper()
	fixture := newCoHoldersCostFixture(t, unrelated)
	group := filepath.Join(fixture.Checkout.CommonDir, "arbitrary-retired-group")
	if err := os.Rename(filepath.Join(fixture.Checkout.CommonDir, "worktrees"), group); err != nil {
		t.Fatal(err)
	}
	admins := map[string]string{fixture.Holders[0]: "00-current", fixture.Holders[1]: "01-peer"}
	for i, root := range fixture.Unrelated {
		admins[root] = fmt.Sprintf("unrelated-%04d", i)
	}
	for root, name := range admins {
		admin := filepath.Join(group, name)
		mustWriteFile(t, filepath.Join(root, ".git"), gitdirPrefix+admin+"\n")
		admins[root] = admin
	}
	checkout, err := ResolveCheckout(fixture.Checkout.Root)
	if err != nil {
		t.Fatal(err)
	}
	fixture.Checkout = checkout
	return fixture, admins
}

func readRetiredCensusCalls(t *testing.T, path string) (map[string]retiredCensusCalls, []int) {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			t.Error(err)
		}
	}()
	calls := map[string]retiredCensusCalls{}
	pids := map[int]bool{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		pid, op, path, err := parseRetiredCensusCall(scanner.Text())
		if err != nil {
			t.Fatal(err)
		}
		if pid == 0 {
			continue
		}
		count := calls[path]
		switch op {
		case "openat":
			count.Openat++
		case "newfstatat":
			count.Newfstatat++
		}
		calls[path] = count
		pids[pid] = true
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	ids := make([]int, 0, len(pids))
	for pid := range pids {
		ids = append(ids, pid)
	}
	slices.Sort(ids)
	return calls, ids
}

func parseRetiredCensusCall(line string) (int, string, string, error) {
	match := retiredCensusCall.FindStringSubmatch(line)
	if match == nil {
		return 0, "", "", nil
	}
	pid, err := strconv.Atoi(match[1])
	if err != nil {
		return 0, "", "", fmt.Errorf("parse strace PID: %w", err)
	}
	path, err := strconv.Unquote(match[3])
	if err != nil {
		return 0, "", "", fmt.Errorf("parse strace path: %w", err)
	}
	return pid, match[2], path, nil
}
