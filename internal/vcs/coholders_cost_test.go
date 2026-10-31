package vcs

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"testing"
)

type coHoldersCostFixture struct {
	Checkout  Checkout
	Branch    string
	Holders   []string
	Unrelated []string
	Result    string
}

type coHoldersCostResult struct {
	PID     int
	Branch  string
	Holders []string
}

var coHoldersNamespaceCall = regexp.MustCompile(`^\s*([0-9]+)\s+(?:newfstatat\(AT_FDCWD,\s*|readlink\()("(?:\\.|[^"\\])*")`)

func TestCoHoldersPayloadNamespaceCost(t *testing.T) {
	mode := os.Getenv("CCX_COHOLDER_STRACE")
	if mode == "" {
		t.Skip("set CCX_COHOLDER_STRACE=before or fixed to trace real payload namespace calls")
	}
	if mode != "before" && mode != "fixed" {
		t.Fatalf("CCX_COHOLDER_STRACE = %q, want before or fixed", mode)
	}
	if runtime.GOOS != "linux" {
		t.Fatal("CCX_COHOLDER_STRACE requires Linux")
	}
	strace, err := exec.LookPath("strace")
	if err != nil {
		t.Fatalf("CCX_COHOLDER_STRACE requires strace: %v", err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, unrelated := range []int{32, 1024} {
		t.Run(strconv.Itoa(unrelated), func(t *testing.T) {
			fixture := newCoHoldersCostFixture(t, unrelated)
			output := t.TempDir()
			if root := os.Getenv("CCX_COHOLDER_STRACE_OUTPUT"); root != "" {
				output = filepath.Join(root, mode, strconv.Itoa(unrelated))
				mustMkdir(t, output)
			}
			fixturePath := filepath.Join(output, "fixture.json")
			fixture.Result = filepath.Join(output, "result.json")
			tracePath := filepath.Join(output, "namespace.strace")
			writeCoHoldersCostJSON(t, fixturePath, fixture)
			command := exec.CommandContext(t.Context(), strace, "-f", "-qq", "-s", "0", "-e", "trace=newfstatat,readlink", "-o", tracePath, binary, "-test.run=^TestCoHoldersNamespaceWalkHelper$", "-test.count=1")
			command.Env = append(os.Environ(), "CCX_COHOLDER_STRACE_FIXTURE="+fixturePath, "PATH="+t.TempDir())
			outputText, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("trace CoHolders: %v\n%s", err, outputText)
			}
			var result coHoldersCostResult
			readCoHoldersCostJSON(t, fixture.Result, &result)
			if result.PID <= 0 || result.Branch != fixture.Branch || !reflect.DeepEqual(result.Holders, fixture.Holders) {
				t.Fatalf("CoHolders child result = %+v, want branch %q, holders %q and a positive PID", result, fixture.Branch, fixture.Holders)
			}
			calls, pids := readCoHoldersNamespaceCalls(t, tracePath)
			unrelatedCalls := 0
			unrelatedPaths := 0
			for _, path := range fixture.Unrelated {
				unrelatedCalls += calls[path]
				if calls[path] > 0 {
					unrelatedPaths++
				}
			}
			selected := map[string]int{}
			for _, path := range fixture.Holders {
				selected[path] = calls[path]
			}
			summary := struct {
				Mode           string
				Unrelated      int
				ChildPID       int
				TracePIDs      []int
				SelectedCalls  map[string]int
				UnrelatedPaths int
				UnrelatedCalls int
			}{mode, unrelated, result.PID, pids, selected, unrelatedPaths, unrelatedCalls}
			writeCoHoldersCostJSON(t, filepath.Join(output, "summary.json"), summary)
			t.Logf("mode=%s unrelated=%d child_pid=%d trace_pids=%v selected_calls=%v unrelated_paths=%d unrelated_calls=%d trace=%s", mode, unrelated, result.PID, pids, selected, unrelatedPaths, unrelatedCalls, tracePath)
			for path, count := range selected {
				if count == 0 {
					t.Errorf("selected payload %q had no traced namespace call", path)
				}
			}
			want := 0
			if mode == "before" {
				want = unrelated
			}
			if unrelatedPaths != want || unrelatedCalls != want {
				t.Fatalf("unrelated payload namespace paths/calls = %d/%d, want %d/%d", unrelatedPaths, unrelatedCalls, want, want)
			}
		})
	}
}

func TestCoHoldersNamespaceWalkHelper(t *testing.T) {
	path := os.Getenv("CCX_COHOLDER_STRACE_FIXTURE")
	if path == "" {
		t.Skip("child of TestCoHoldersPayloadNamespaceCost")
	}
	var fixture coHoldersCostFixture
	readCoHoldersCostJSON(t, path, &fixture)
	branch, holders, err := CoHolders(t.Context(), fixture.Checkout)
	if err != nil {
		t.Fatal(err)
	}
	writeCoHoldersCostJSON(t, fixture.Result, coHoldersCostResult{os.Getpid(), branch, holders})
}

func TestCoHoldersPreservesCanonicalCurrentAndBranchDuplicates(t *testing.T) {
	fixture := newCoHoldersCostFixture(t, 1)
	currentAlias := filepath.Join(t.TempDir(), "current-alias")
	if err := os.Symlink(fixture.Checkout.Root, currentAlias); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(fixture.Checkout.GitDir, "gitdir"), filepath.Join(currentAlias, ".git")+"\n")
	peerAlias := filepath.Join(t.TempDir(), "peer-alias")
	if err := os.Symlink(fixture.Holders[1], peerAlias); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(fixture.Checkout.CommonDir, "worktrees", "01-peer", "gitdir"), filepath.Join(peerAlias, ".git")+"\n")
	secondPeer := filepath.Join(canon(t, t.TempDir()), "second-peer")
	writeCoHoldersCostWorktree(t, fixture.Checkout.MainRoot, secondPeer, "02-peer", fixture.Branch)
	branch, holders, err := CoHolders(t.Context(), fixture.Checkout)
	want := []string{fixture.Checkout.Root, peerAlias, secondPeer}
	if err != nil || branch != fixture.Branch || !reflect.DeepEqual(holders, want) {
		t.Fatalf("CoHolders = %q, %q, %v; want %q, %q, nil", branch, holders, err, fixture.Branch, want)
	}
}

func TestCoHoldersPreservesArbitraryRetiredAdmins(t *testing.T) {
	fixture := newCoHoldersCostFixture(t, 1)
	for i, name := range []string{"00-current", "01-peer"} {
		oldAdmin := filepath.Join(fixture.Checkout.CommonDir, "worktrees", name)
		admin := filepath.Join(fixture.Checkout.CommonDir, "arbitrary-retired-group", name)
		mustMkdir(t, filepath.Dir(admin))
		if err := os.Rename(oldAdmin, admin); err != nil {
			t.Fatal(err)
		}
		mustWriteFile(t, filepath.Join(fixture.Holders[i], ".git"), gitdirPrefix+admin+"\n")
	}
	checkout, err := ResolveCheckout(fixture.Checkout.Root)
	if err != nil {
		t.Fatal(err)
	}
	branch, holders, err := CoHolders(t.Context(), checkout)
	if err != nil || branch != fixture.Branch || !reflect.DeepEqual(holders, fixture.Holders) {
		t.Fatalf("CoHolders = %q, %q, %v; want %q, %q, nil", branch, holders, err, fixture.Branch, fixture.Holders)
	}
}

func TestCoHoldersUsesCurrentHead(t *testing.T) {
	for _, name := range []string{"unlisted current with duplicate peer", "stale current alias", "duplicate current path"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("PATH", t.TempDir())
			fixture := newCoHoldersCostFixture(t, 0)
			root, branch := fixture.Checkout.Root, fixture.Branch
			want := slices.Clone(fixture.Holders)
			switch name {
			case "unlisted current with duplicate peer":
				admin := filepath.Join(canon(t, t.TempDir()), "outside-admin")
				if err := os.Rename(fixture.Checkout.GitDir, admin); err != nil {
					t.Fatal(err)
				}
				mustWriteFile(t, filepath.Join(fixture.Checkout.Root, ".git"), gitdirPrefix+admin+"\n")
				mustWriteFile(t, filepath.Join(admin, "commondir"), fixture.Checkout.CommonDir+"\n")
				checkout, err := ResolveCheckout(fixture.Checkout.Root)
				if err != nil {
					t.Fatal(err)
				}
				fixture.Checkout = checkout
				root = fixture.Holders[1]
				want = append(want, root)
			case "stale current alias":
				root = filepath.Join(canon(t, t.TempDir()), "current-alias")
				if err := os.Symlink(fixture.Checkout.Root, root); err != nil {
					t.Fatal(err)
				}
				branch = "stale"
			}
			admin := filepath.Join(fixture.Checkout.CommonDir, "worktrees", "zz-shadow")
			mustMkdir(t, admin)
			mustWriteFile(t, filepath.Join(admin, "commondir"), "../..\n")
			mustWriteFile(t, filepath.Join(admin, "gitdir"), filepath.Join(root, ".git")+"\n")
			mustWriteFile(t, filepath.Join(admin, "HEAD"), "ref: refs/heads/"+branch+"\n")
			gotBranch, holders, err := CoHolders(t.Context(), fixture.Checkout)
			if err != nil || gotBranch != fixture.Branch || !reflect.DeepEqual(holders, want) {
				t.Fatalf("CoHolders = %q, %q, %v; want %q, %q, nil", gotBranch, holders, err, fixture.Branch, want)
			}
		})
	}
}

func TestParseCoHoldersNamespaceCall(t *testing.T) {
	for _, tc := range []struct {
		name string
		line string
		pid  int
		path string
	}{
		{"stat", `123 newfstatat(AT_FDCWD, "/fixture/payload", {st_mode=S_IFDIR|0700}, AT_SYMLINK_NOFOLLOW) = 0`, 123, "/fixture/payload"},
		{"readlink", `  456  readlink("/fixture/alias", "/fixture/payload", 128) = 16`, 456, "/fixture/alias"},
		{"unfinished", `123 newfstatat(AT_FDCWD, "/fixture/payload", <unfinished ...>`, 123, "/fixture/payload"},
		{"resumed", `123 <... newfstatat resumed>{st_mode=S_IFDIR|0700}, AT_SYMLINK_NOFOLLOW) = 0`, 0, ""},
		{"other syscall", `123 openat(AT_FDCWD, "/fixture/payload", O_RDONLY) = 4`, 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pid, path, err := parseCoHoldersNamespaceCall(tc.line)
			if err != nil || pid != tc.pid || path != tc.path {
				t.Fatalf("parse namespace call = %d, %q, %v; want %d, %q, nil", pid, path, err, tc.pid, tc.path)
			}
		})
	}
}

func newCoHoldersCostFixture(t *testing.T, unrelated int) coHoldersCostFixture {
	t.Helper()
	base := canon(t, t.TempDir())
	main := filepath.Join(base, "main")
	common := filepath.Join(main, ".git")
	mustMkdir(t, common)
	mustWriteFile(t, filepath.Join(common, "HEAD"), "ref: refs/heads/trunk\n")
	current := filepath.Join(base, "current-payload")
	peer := filepath.Join(base, "peer-payload")
	branch := "shared"
	writeCoHoldersCostWorktree(t, main, current, "00-current", branch)
	writeCoHoldersCostWorktree(t, main, peer, "01-peer", branch)
	fixture := coHoldersCostFixture{Branch: branch, Holders: []string{current, peer}}
	for i := range unrelated {
		name := fmt.Sprintf("unrelated-%04d", i)
		root := filepath.Join(base, name+"-payload")
		writeCoHoldersCostWorktree(t, main, root, name, name)
		fixture.Unrelated = append(fixture.Unrelated, root)
	}
	checkout, err := ResolveCheckout(current)
	if err != nil {
		t.Fatal(err)
	}
	fixture.Checkout = checkout
	return fixture
}

func writeCoHoldersCostWorktree(t *testing.T, main, root, name, branch string) {
	t.Helper()
	admin := filepath.Join(main, ".git", "worktrees", name)
	writeGitWorktree(t, main, root, name, admin)
	mustWriteFile(t, filepath.Join(admin, "gitdir"), filepath.Join(root, ".git")+"\n")
	mustWriteFile(t, filepath.Join(admin, "HEAD"), "ref: refs/heads/"+branch+"\n")
}

func writeCoHoldersCostJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, path, string(data)+"\n")
}

func readCoHoldersCostJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, value); err != nil {
		t.Fatal(err)
	}
}

func readCoHoldersNamespaceCalls(t *testing.T, path string) (map[string]int, []int) {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	calls := map[string]int{}
	pids := map[int]bool{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		pid, path, err := parseCoHoldersNamespaceCall(scanner.Text())
		if err != nil {
			t.Fatal(err)
		}
		if pid != 0 {
			calls[path]++
			pids[pid] = true
		}
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

func parseCoHoldersNamespaceCall(line string) (int, string, error) {
	match := coHoldersNamespaceCall.FindStringSubmatch(line)
	if match == nil {
		return 0, "", nil
	}
	pid, err := strconv.Atoi(match[1])
	if err != nil {
		return 0, "", fmt.Errorf("parse strace PID: %w", err)
	}
	path, err := strconv.Unquote(match[2])
	if err != nil {
		return 0, "", fmt.Errorf("parse strace path: %w", err)
	}
	return pid, path, nil
}
