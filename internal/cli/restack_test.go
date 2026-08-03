package cli

import (
	"bytes"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// writeRestackFakes installs fake gt, jj, and git executables into dir. Each
// records its argv as a NUL-delimited record so tests can assert exact calls.
func writeRestackFakes(t *testing.T, dir string, withGT bool) {
	t.Helper()
	log := func(name string) string {
		return "{ printf '" + name + "\\0'; for a in \"$@\"; do printf '%s\\0' \"$a\"; done; printf '\\0'; } >> \"$RESTACK_LOG\"\n"
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o700); err != nil { //nolint:gosec // fake executable must be owner-executable
			t.Fatalf("write fake %s: %v", name, err)
		}
	}

	gt := "#!/bin/sh\n" + log("gt") + `DEFAULT_STATE='{"main":{"trunk":true},"feature":{"parents":[{"ref":"main"}]}}'
SYNCED="$RESTACK_LOG.synced"
case "$1" in
  sync)
    if [ -n "$RESTACK_GT_STDOUT" ]; then printf '%s\n' "$RESTACK_GT_STDOUT"; fi
    if [ -n "$RESTACK_GT_KILL" ]; then kill -9 $$; fi
    if [ -n "$RESTACK_GT_FAIL" ]; then
      if [ -n "$RESTACK_GT_STDERR" ]; then printf '%s\n' "$RESTACK_GT_STDERR" >&2; fi
      exit 1
    fi
    if [ -n "$RESTACK_GT_SKIP" ]; then printf '%s\n' "$RESTACK_GT_SKIP"; fi
    if [ -n "$RESTACK_GT_SYNC_STDERR" ]; then printf '%s\n' "$RESTACK_GT_SYNC_STDERR" >&2; fi
    : > "$SYNCED" ;;
  state)
    if [ -n "$RESTACK_GT_STATE_AFTER" ] && [ -f "$SYNCED" ]; then
      printf '%s' "$RESTACK_GT_STATE_AFTER"
    else
      printf '%s' "${RESTACK_GT_STATE:-$DEFAULT_STATE}"
    fi ;;
  *) printf 'fake gt: unmatched argv: %s\n' "$*" >&2; exit 2 ;;
esac
exit 0
`
	jj := "#!/bin/sh\n" + log("jj") + `if [ "$1" = --ignore-working-copy ]; then shift; fi
case "$1 $2" in
  "git fetch")
    if [ -n "$RESTACK_JJ_FETCH_FAIL" ]; then printf 'jj fetch failed\n' >&2; exit 1; fi ;;
  "log -r")
    if [ -n "$RESTACK_JJ_LOG_FAIL" ]; then printf 'jj log failed\n' >&2; exit 1; fi
    case "$3" in
      "trunk()") for b in ${RESTACK_JJ_TRUNK_NAMES:-main}; do printf '"%s"\n' "$b"; done ;;
      "trunk() & ::@")
        if [ -n "$RESTACK_JJ_UP_TO_DATE" ]; then printf 'aaaaaaa trunk\n'; fi ;;
      "trunk()..@") printf '%s' "${RESTACK_JJ_STACK:-bbbbbbb one
ccccccc two
}" ;;
      "conflicts() & @::") printf '%s' "$RESTACK_JJ_CONFLICTS" ;;
      *) printf 'fake jj: unmatched revset: %s\n' "$3" >&2; exit 2 ;;
    esac ;;
  "rebase -b")
    if [ -n "$RESTACK_JJ_REBASE_FAIL" ]; then printf 'jj rebase failed\n' >&2; exit 1; fi ;;
  "op log") printf 'op123abc' ;;
  "op revert")
    if [ -n "$RESTACK_JJ_REVERT_FAIL" ]; then printf 'jj op revert failed\n' >&2; exit 1; fi ;;
  *) printf 'fake jj: unmatched argv: %s\n' "$*" >&2; exit 2 ;;
esac
exit 0
`
	git := "#!/bin/sh\n" + log("git") + `if [ "$1" = --git-dir ]; then shift 2; fi
case "$1 $2" in
  "branch --show-current") printf '%s\n' "${RESTACK_GIT_BRANCH:-feature}" ;;
  "worktree list")
    for h in $RESTACK_HOLDERS; do printf 'worktree %s\000branch refs/heads/%s\000\000' "${h#*=}" "${h%%=*}"; done ;;
  "config --get")
    case "$3" in
      ccx.nogt) if [ -n "$RESTACK_CONFIG_CCX_NOGT" ]; then printf '%s\n' "$RESTACK_CONFIG_CCX_NOGT"; else exit 1; fi ;;
      branch.*.remote) if [ -n "$RESTACK_GIT_REMOTE" ]; then printf '%s\n' "$RESTACK_GIT_REMOTE"; else exit 1; fi ;;
      *) printf 'fake git: unmatched config key: %s\n' "$3" >&2; exit 2 ;;
    esac ;;
  "symbolic-ref --short")
    if [ -n "$RESTACK_GIT_SYMBOLIC_MISS" ]; then exit 1; fi
    printf '%s/%s\n' "${RESTACK_GIT_REMOTE:-origin}" "${RESTACK_GIT_TRUNK:-main}" ;;
  "show-ref --verify")
    ref=$3; if [ "$3" = --quiet ]; then ref=$4; fi
    case "$ref" in
      */main) if [ "$RESTACK_GIT_MAIN_REF" = 1 ]; then exit 0; fi ;;
      */master) if [ "$RESTACK_GIT_MASTER_REF" = 1 ]; then exit 0; fi ;;
    esac
    if [ "$3" = --quiet ]; then exit 1; fi
    printf "fatal: '%s' - not a valid ref\n" "$ref" >&2; exit 128 ;;
  "merge-base --is-ancestor")
    case " $RESTACK_GIT_GONE_REFS " in
      *" $4 "*) printf 'fatal: Not a valid object name %s\n' "$4" >&2; exit 128 ;;
    esac
    case "$4" in
      HEAD) if [ -z "$RESTACK_GIT_UP_TO_DATE" ]; then exit 1; fi ;;
      *) case " $RESTACK_GT_OFF_TRUNK " in *" $3 $4 "*) exit 1 ;; esac ;;
    esac ;;
  "rev-list --count") printf '2' ;;
  "rebase --autostash")
    if [ -n "$RESTACK_GIT_REBASE_CONFLICT" ]; then
      printf 'CONFLICT (content): Merge conflict in conflict.txt\n' >&2
      exit 1
    fi ;;
  "rev-parse --verify")
    if [ "$4" = REBASE_HEAD ] && [ -n "$RESTACK_GIT_REBASE_CONFLICT" ]; then exit 0; fi
    exit 1 ;;
  "diff --name-only") printf 'conflict.txt\n' ;;
  "rebase --abort") : ;;
  "merge --ff-only") : ;;
  *)
    if [ "$1" = fetch ]; then
      if [ -n "$RESTACK_GIT_FETCH_FAIL" ]; then printf 'git fetch failed\n' >&2; exit 1; fi
    else
      printf 'fake git: unmatched argv: %s\n' "$*" >&2
      exit 2
    fi ;;
esac
exit 0
`

	if withGT {
		write("gt", gt)
	}
	write("jj", jj)
	write("git", git)
}

func setupRestack(t *testing.T, marker string, graphite, withGT bool) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake shell scripts are POSIX-only")
	}

	dir := t.TempDir()
	if marker != "" {
		if err := os.MkdirAll(filepath.Join(dir, marker), 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", marker, err)
		}
	}
	if graphite {
		gitDir := filepath.Join(dir, ".git")
		if err := os.MkdirAll(gitDir, 0o750); err != nil {
			t.Fatalf("mkdir .git: %v", err)
		}
		if err := os.WriteFile(filepath.Join(gitDir, ".graphite_repo_config"), []byte("{}"), 0o600); err != nil {
			t.Fatalf("write graphite config: %v", err)
		}
	}
	binDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(binDir, 0o750); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	writeRestackFakes(t, binDir, withGT)

	old, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	logPath := filepath.Join(dir, "restack.log")
	t.Setenv("PATH", binDir)
	// Root the cache under the test's own dir so the lane gate never reads or
	// writes the developer's real ~/Library/Caches/cc-context.
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	t.Setenv("RESTACK_LOG", logPath)
	if graphite {
		seedLaneRecords(t, ".", laneSeed{})
		// The gt lane resolves its verdict oracle from the remote-tracking trunk,
		// which gt sync writes on every fetch; the git lane's tests own the
		// absent-ref cases.
		t.Setenv("RESTACK_GIT_MAIN_REF", "1")
	}
	return logPath
}

// restackWorktreeList is the BranchHolders read the gt preflight makes.
func restackWorktreeList(t *testing.T) []string {
	t.Helper()
	return []string{"git", "--git-dir", filepath.Join(restackRoot(t), ".git"), "worktree", "list", "--porcelain", "-z", "--end-of-options"}
}

// restackRoot is the working copy root in the spelling git and vcs.Checkout both
// canonicalize to.
func restackRoot(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	root, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		t.Fatalf("resolve %s: %v", cwd, err)
	}
	return root
}

// seedRestackHolders hands the git fake a branch=worktree table, which its
// worktree-list arm re-emits as the NUL-framed porcelain records
// vcs.BranchHolders parses — the map is what the fake models, so a branch no
// entry names is simply absent, as it is for a detached or bare checkout. It
// travels as an environment variable the fake formats with builtins because PATH
// holds only the fakes — there is no cat to read a file with.
func seedRestackHolders(t *testing.T, holders map[string]string) {
	t.Helper()
	pairs := make([]string, 0, len(holders))
	for _, branch := range slices.Sorted(maps.Keys(holders)) {
		pairs = append(pairs, branch+"="+holders[branch])
	}
	t.Setenv("RESTACK_HOLDERS", strings.Join(pairs, " "))
}

func runRestackCmd(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	cmd := newRestackCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return strings.TrimSpace(out.String()), errOut.String(), err
}

func readRestackLog(t *testing.T, path string) [][]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read restack log: %v", err)
	}
	var records [][]string
	for _, record := range bytes.Split(data, []byte{0, 0}) {
		if len(record) == 0 {
			continue
		}
		fields := bytes.Split(record, []byte{0})
		row := make([]string, len(fields))
		for i, field := range fields {
			row[i] = string(field)
		}
		records = append(records, row)
	}
	return records
}

func requireRestackRecords(t *testing.T, path string, want [][]string) {
	t.Helper()
	got := readRestackLog(t, path)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv records:\n got: %#v\nwant: %#v", got, want)
	}
}

func TestRestackGTSuccess(t *testing.T) {
	logPath := setupRestack(t, ".git", true, true)

	out, _, err := runRestackCmd(t)
	if err != nil {
		t.Fatalf("restack: %v", err)
	}
	if out != "restacked 1 of 1 · trunk main" {
		t.Fatalf("output = %q, want %q", out, "restacked 1 of 1 · trunk main")
	}
	requireRestackRecords(t, logPath, [][]string{
		nogtProbe,
		{"gt", "state"},
		{"git", "branch", "--show-current"},
		restackWorktreeList(t),
		{"gt", "sync", "--no-interactive"},
		{"gt", "state"},
		{"git", "branch", "--show-current"},
		{"git", "config", "--get", "branch.main.remote"},
		{"git", "show-ref", "--verify", "--quiet", "refs/remotes/origin/main"},
		{"git", "merge-base", "--is-ancestor", "refs/remotes/origin/main", "feature"},
	})
}

// TestRestackGTVerdictReadsTheSyncedStack pins the verdict to the stack gt sync
// left behind. Sync deletes the branches whose PRs landed and reparents their
// children, so a verdict over the pre-sync list asks git about a ref sync just
// deleted — merge-base exits 128 there, failing a restack that worked.
func TestRestackGTVerdictReadsTheSyncedStack(t *testing.T) {
	logPath := setupRestack(t, ".git", true, true)
	t.Setenv("RESTACK_GT_STATE", restackStackState)
	// b merged: sync deletes it and reparents c onto a.
	t.Setenv("RESTACK_GT_STATE_AFTER", `{"main":{"trunk":true},"a":{"parents":[{"ref":"main"}]},"c":{"parents":[{"ref":"a"}]}}`)
	t.Setenv("RESTACK_GIT_GONE_REFS", "b")
	t.Setenv("RESTACK_GIT_BRANCH", "c")

	out, _, err := runRestackCmd(t)
	if err != nil {
		t.Fatalf("restack: %v", err)
	}
	if want := "restacked 2 of 2 · trunk main"; out != want {
		t.Fatalf("output = %q, want %q", out, want)
	}
	for _, record := range readRestackLog(t, logPath) {
		if record[0] == "git" && record[1] == "merge-base" && record[len(record)-1] == "b" {
			t.Errorf("verdict probed %v — b is the branch sync deleted", record)
		}
	}
}

// restackStackState tracks the stack c → b → a → main.
const restackStackState = `{"main":{"trunk":true},"a":{"parents":[{"ref":"main"}]},"b":{"parents":[{"ref":"a"}]},"c":{"parents":[{"ref":"b"}]}}`

func TestRestackGTPerBranchVerdict(t *testing.T) {
	// offTrunk lists "<ref> <branch>" pairs git merge-base --is-ancestor answers
	// no for, so a case pins which ref the verdict consulted, not just its answer.
	tests := []struct {
		name     string
		branch   string
		offTrunk string
		gtSkip   string
		want     string
	}{
		{
			name:   "whole stack landed on trunk",
			branch: "c",
			want:   "restacked 3 of 3 · trunk main",
		},
		{
			name:     "one branch never reached trunk",
			branch:   "c",
			offTrunk: "refs/remotes/origin/main b",
			want:     "restacked 2 of 3 · trunk main · skipped b",
		},
		{
			name:     "gt named the working copy that blocked it",
			branch:   "c",
			offTrunk: "refs/remotes/origin/main b",
			gtSkip:   "Did not restack branch b because it is checked out in worktree /w/b",
			want:     "restacked 2 of 3 · trunk main · skipped b (checked out in /w/b)",
		},
		{
			name:   "gt skipped a branch outside this stack",
			branch: "c",
			gtSkip: "Did not restack branch zz because it is checked out in worktree /w/zz",
			want:   "restacked 3 of 3 · trunk main · skipped zz (checked out in /w/zz)",
		},
		{
			name:   "gt declined a branch trunk is already an ancestor of",
			branch: "c",
			gtSkip: "Did not restack branch b because it is checked out in worktree /w/b",
			want:   "restacked 2 of 3 · trunk main · skipped b (checked out in /w/b; already on refs/remotes/origin/main)",
		},
		{
			name:   "gt declined a frozen branch already on trunk",
			branch: "c",
			gtSkip: "Did not restack branch b because it is frozen.",
			want:   "restacked 2 of 3 · trunk main · skipped b (frozen; already on refs/remotes/origin/main)",
		},
		{
			name:     "gt declined a branch mid-merge that never reached trunk",
			branch:   "c",
			offTrunk: "refs/remotes/origin/main b",
			gtSkip:   "Did not restack branch b because it is merging.",
			want:     "restacked 2 of 3 · trunk main · skipped b (merging)",
		},
		{
			name:   "on trunk, nothing to restack",
			branch: "main",
			want:   "synced · trunk main",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setupRestack(t, ".git", true, true)
			t.Setenv("RESTACK_GT_STATE", restackStackState)
			t.Setenv("RESTACK_GIT_BRANCH", tt.branch)
			t.Setenv("RESTACK_GT_OFF_TRUNK", tt.offTrunk)
			t.Setenv("RESTACK_GT_SKIP", tt.gtSkip)

			out, _, err := runRestackCmd(t)
			if err != nil {
				t.Fatalf("restack: %v", err)
			}
			if out != tt.want {
				t.Fatalf("output = %q, want %q", out, tt.want)
			}
		})
	}
}

// TestRestackGTSurfacesSyncDiagnostics covers the half of a sync stdout alone
// cannot see. gt 1.8.6 splits one exit-0 sync across both streams — the phase
// banners on stdout, severity-led lines on stderr — so a restack it declined
// leaves the summary reporting the stack as behind with nothing saying why. The
// warning row is gt 1.8.6's own bytes, captured from a sync whose checked-out
// branch needed a restack and carried a conflicting unstaged edit: exit 0,
// stdout ending at "🥞 Restacking branches...", the explanation on stderr alone.
// The "could not be restacked cleanly" line recurs across 49 of the 9,346 real
// gt runs on this machine, over 27 distinct branches, so it is the durable half
// of the pair. Its remediation rides out with it: the unprefixed sentence gt
// puts a blank line below the warning is the only thing telling the user what to
// run, and the tips row is the other half of that bargain — the same unprefixed
// stderr with no severity line above it stays unreported.
// Exit 0 stays a success, since the remote-trunk oracle already reports the
// stack correctly; the lines explain that report rather than override gt.
func TestRestackGTSurfacesSyncDiagnostics(t *testing.T) {
	tests := []struct {
		name    string
		stderr  string
		want    string
		wantErr []string
		denyErr []string
	}{
		{
			name: "the warnings gt exited 0 on still reach the user",
			stderr: "WARNING: Did not restack checked out branch feature due to conflicting unstaged changes.\n" +
				"WARNING: feature could not be restacked cleanly.\n\n" +
				"Please resolve conflicts in the current stack with gt restack.",
			want: "restacked 1 of 1 · trunk main",
			wantErr: []string{
				"WARNING: Did not restack checked out branch feature due to conflicting unstaged changes.",
				"WARNING: feature could not be restacked cleanly.",
				"Please resolve conflicts in the current stack with gt restack.",
			},
		},
		{
			name:    "tips alone leave the report silent",
			stderr:  "\ntip: If you need to undo the operation you just ran, you can do so with gt undo. [runner.undo ●○○]\n",
			want:    "restacked 1 of 1 · trunk main",
			denyErr: []string{"tip:", "gt undo"},
		},
		{
			name: "a decline is read off whichever stream carried it",
			// gt 1.8.6 was observed putting declines on stdout; the parser is fed
			// both so which stream carries one is not a fact the summary depends on.
			stderr: "Did not restack branch zz because it is frozen.",
			want:   "restacked 1 of 1 · trunk main · skipped zz (frozen)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setupRestack(t, ".git", true, true)
			t.Setenv("RESTACK_GT_SYNC_STDERR", tt.stderr)

			out, errOut, err := runRestackCmd(t)
			if err != nil {
				t.Fatalf("restack: %v", err)
			}
			if out != tt.want {
				t.Fatalf("output = %q, want %q", out, tt.want)
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(errOut, want) {
					t.Errorf("stderr = %q, want it to carry %q", errOut, want)
				}
			}
			for _, deny := range tt.denyErr {
				if strings.Contains(errOut, deny) {
					t.Errorf("stderr = %q, want it to withhold %q", errOut, deny)
				}
			}
		})
	}
}

// TestRestackGTStreamedSyncPrintsDiagnosticsOnce guards the two arms against
// disagreeing the other way. The streaming arm already wires both of gt's
// streams to the writer as they are produced, so a diagnostic pass there would
// print every line the user just watched a second time.
func TestRestackGTStreamedSyncPrintsDiagnosticsOnce(t *testing.T) {
	setupRestack(t, ".git", true, true)
	line := "WARNING: Did not restack checked out branch feature due to conflicting unstaged changes."
	t.Setenv("RESTACK_GT_SYNC_STDERR", line)
	old := shipStreamCI
	t.Cleanup(func() { shipStreamCI = old })
	shipStreamCI = func(io.Writer) bool { return true }

	out, errOut, err := runRestackCmd(t)
	if err != nil {
		t.Fatalf("restack: %v", err)
	}
	if out != "restacked 1 of 1 · trunk main" {
		t.Fatalf("output = %q", out)
	}
	if got := strings.Count(errOut, line); got != 1 {
		t.Fatalf("diagnostic printed %d times in %q, want exactly 1", got, errOut)
	}
}

// TestRestackGTVerdictProbesTheQualifiedTrunkRef pins the ref spelling the
// ancestry probe receives. git resolves a short origin/main through refs/tags
// and refs/heads before refs/remotes, so a local branch or tag of that name
// answers merge-base in place of the ref show-ref verified — measured on git
// 2.55.0, where a decoy refs/heads/origin/main flipped
// `merge-base --is-ancestor origin/main feature` from exit 1 to exit 0 while
// only warning on stderr. The verdict would have counted a stack that never
// moved as restacked.
func TestRestackGTVerdictProbesTheQualifiedTrunkRef(t *testing.T) {
	logPath := setupRestack(t, ".git", true, true)

	if _, _, err := runRestackCmd(t); err != nil {
		t.Fatalf("restack: %v", err)
	}
	probed := 0
	for _, record := range readRestackLog(t, logPath) {
		if len(record) < 4 || record[0] != "git" || record[1] != "merge-base" {
			continue
		}
		probed++
		if record[3] != "refs/remotes/origin/main" {
			t.Errorf("ancestry probe asked %q, want the fully qualified refs/remotes/origin/main", record[3])
		}
	}
	if probed == 0 {
		t.Fatal("no ancestry probe ran — the test proves nothing")
	}
}

// TestRestackGTMeasuresTheRemoteTrunk pins the decline-free stale-trunk path.
// gt sync writes refs/remotes/origin/main from its fetch and only then tries to
// move the local branch; when that second step fails — a sibling working copy
// holding trunk with conflicting unstaged changes, measured against gt 1.8.6 —
// gt exits 0, declines nothing, and leaves the local ref behind the remote. A
// verdict that asks the local ref calls a stack that never moved current.
func TestRestackGTMeasuresTheRemoteTrunk(t *testing.T) {
	logPath := setupRestack(t, ".git", true, true)
	t.Setenv("RESTACK_GT_STATE", restackStackState)
	t.Setenv("RESTACK_GIT_BRANCH", "c")
	t.Setenv("RESTACK_GT_OFF_TRUNK", "refs/remotes/origin/main a refs/remotes/origin/main b refs/remotes/origin/main c")

	out, _, err := runRestackCmd(t)
	if err != nil {
		t.Fatalf("restack: %v", err)
	}
	want := "restacked 0 of 3 · trunk main · skipped c, b, a"
	if out != want {
		t.Fatalf("output = %q, want %q", out, want)
	}
	for _, branch := range []string{"a", "b", "c"} {
		found := false
		for _, record := range readRestackLog(t, logPath) {
			if reflect.DeepEqual(record, []string{"git", "merge-base", "--is-ancestor", "refs/remotes/origin/main", branch}) {
				found = true
			}
		}
		if !found {
			t.Fatalf("no ancestry check of %s against refs/remotes/origin/main — the verdict asked the local trunk ref", branch)
		}
	}
}

// TestRestackGTNamesTheWorkingCopyHoldingTrunk covers the reason the stale-trunk
// summary otherwise withholds. gt declines nothing when it cannot pull a held
// trunk, so the stack reads as behind with no cause attached; the holder is the
// cause, and BranchHolders already has it. It names only a holder git reports —
// an unheld trunk, and one this working copy holds, both stay silent rather than
// assert a working copy that is not there.
func TestRestackGTNamesTheWorkingCopyHoldingTrunk(t *testing.T) {
	tests := []struct {
		name    string
		holders map[string]string
		want    string
	}{
		{
			name:    "a sibling working copy holds trunk",
			holders: map[string]string{"main": "/w/trunk"},
			want:    "restacked 0 of 3 · trunk main (checked out in /w/trunk) · skipped c, b, a",
		},
		{
			name: "git names no holder for trunk",
			want: "restacked 0 of 3 · trunk main · skipped c, b, a",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setupRestack(t, ".git", true, true)
			t.Setenv("RESTACK_GT_STATE", restackStackState)
			t.Setenv("RESTACK_GIT_BRANCH", "c")
			t.Setenv("RESTACK_GT_OFF_TRUNK", "refs/remotes/origin/main a refs/remotes/origin/main b refs/remotes/origin/main c")
			if tt.holders != nil {
				seedRestackHolders(t, tt.holders)
			}

			out, _, err := runRestackCmd(t)
			if err != nil {
				t.Fatalf("restack: %v", err)
			}
			if out != tt.want {
				t.Fatalf("output = %q, want %q", out, tt.want)
			}
		})
	}

	t.Run("this working copy holds trunk", func(t *testing.T) {
		setupRestack(t, ".git", true, true)
		t.Setenv("RESTACK_GT_STATE", restackStackState)
		t.Setenv("RESTACK_GIT_BRANCH", "c")
		t.Setenv("RESTACK_GT_OFF_TRUNK", "refs/remotes/origin/main a refs/remotes/origin/main b refs/remotes/origin/main c")
		seedRestackHolders(t, map[string]string{"main": restackRoot(t)})

		out, _, err := runRestackCmd(t)
		if err != nil {
			t.Fatalf("restack: %v", err)
		}
		want := "restacked 0 of 3 · trunk main · skipped c, b, a"
		if out != want {
			t.Fatalf("output = %q, want %q", out, want)
		}
	})
}

func TestRestackGTRefusesMissingRemoteTrunk(t *testing.T) {
	setupRestack(t, ".git", true, true)
	t.Setenv("RESTACK_GIT_MAIN_REF", "")

	_, _, err := runRestackCmd(t)
	if err == nil {
		t.Fatal("restack succeeded without a remote-tracking trunk, want a refusal")
	}
	want := "restack: refs/remotes/origin/main does not exist — run git fetch origin main"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err, want)
	}
}

func TestRestackGTRefusesBranchHeldElsewhere(t *testing.T) {
	logPath := setupRestack(t, ".git", true, true)
	t.Setenv("RESTACK_GT_STATE", restackStackState)
	t.Setenv("RESTACK_GIT_BRANCH", "c")
	seedRestackHolders(t, map[string]string{"c": restackRoot(t), "b": "/w/b"})

	_, _, err := runRestackCmd(t)
	if err == nil {
		t.Fatal("restack succeeded, want a refusal naming the holder")
	}
	want := "restack: b is checked out in /w/b — gt cannot restack a branch another working copy holds; restack from there, or release it first"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err, want)
	}
	requireRestackRecords(t, logPath, [][]string{
		nogtProbe,
		{"gt", "state"},
		{"git", "branch", "--show-current"},
		restackWorktreeList(t),
	})
}

func TestRestackGTRunsWhenThisWorkingCopyHoldsTheStack(t *testing.T) {
	setupRestack(t, ".git", true, true)
	seedRestackHolders(t, map[string]string{"feature": restackRoot(t)})

	out, _, err := runRestackCmd(t)
	if err != nil {
		t.Fatalf("restack: %v", err)
	}
	if out != "restacked 1 of 1 · trunk main" {
		t.Fatalf("output = %q, want %q", out, "restacked 1 of 1 · trunk main")
	}
}

// TestRestackGTFailures pins the classifier to gt's own streams. The conflict
// banner rides stdout with stderr empty — reproduced twice against gt 1.8.6.
// The auth rows drive stderr instead, so the two together prove neither stream
// alone is enough. The last two rows are the sentences gt 1.8.6 prints for a
// trunk it could not move — a dirty worktree holding it, and a local trunk
// diverged from remote — both exit 1 under --no-interactive without --force,
// where the classifier's default arm must carry them through verbatim; the
// trailing space is splog's, from an error template that always appends one.
// Every row also asserts gt's failure survives the advice that replaces its
// sentence.
func TestRestackGTFailures(t *testing.T) {
	tests := []struct {
		name   string
		stdout string
		stderr string
		want   string
		exact  bool
	}{
		{
			name:   "conflict",
			stdout: "Hit conflict restacking branch feature",
			want:   "restack: conflict — resolve the listed files, then gt continue (or gt abort); see the output above",
			exact:  true,
		},
		{
			name:   "auth required",
			stderr: "Please authenticate your Graphite CLI",
			want:   "restack: graphite auth required — run gt auth",
			exact:  true,
		},
		{
			name:   "expired auth",
			stderr: "Your Graphite auth token is invalid/expired",
			want:   "restack: graphite auth required — run gt auth",
			exact:  true,
		},
		{
			name:   "trunk held dirty",
			stderr: "ERROR: Cannot pull trunk due to conflicting unstaged changes. ",
			want:   "ERROR: Cannot pull trunk due to conflicting unstaged changes.",
		},
		{
			name:   "trunk diverged",
			stderr: "WARNING: main could not be fast-forwarded.",
			want:   "WARNING: main could not be fast-forwarded.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logPath := setupRestack(t, ".git", true, true)
			t.Setenv("RESTACK_GT_FAIL", "1")
			t.Setenv("RESTACK_GT_STDOUT", tt.stdout)
			t.Setenv("RESTACK_GT_STDERR", tt.stderr)

			_, _, err := runRestackCmd(t)
			if err == nil {
				t.Fatal("restack succeeded, want failure")
			}
			if tt.exact && err.Error() != tt.want {
				t.Fatalf("error = %q, want %q", err, tt.want)
			}
			if !tt.exact && !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %q, want substring %q", err, tt.want)
			}
			var gerr *gtError
			if !errors.As(err, &gerr) {
				t.Fatalf("error = %q, want gt's own failure reachable through errors.As", err)
			}
			if line := tt.stdout + tt.stderr; !strings.Contains(gerr.Output, line) {
				t.Fatalf("gtError.Output = %q, want it to carry gt's line %q", gerr.Output, line)
			}
			requireRestackRecords(t, logPath, [][]string{
				nogtProbe,
				{"gt", "state"},
				{"git", "branch", "--show-current"},
				restackWorktreeList(t),
				{"gt", "sync", "--no-interactive"},
			})
		})
	}
}

// TestRestackGTClassifiesWhatGTPrinted separates the two channels a classifier
// could read. gt prints its conflict banner and is then killed, so the error
// carries only the signal while the run's output carries the banner: a
// classifier reading the error's prose — which is string-matching an error, the
// thing this package does not do — recovers nothing, while one reading the run's
// own output still hands back the recovery step.
func TestRestackGTClassifiesWhatGTPrinted(t *testing.T) {
	setupRestack(t, ".git", true, true)
	t.Setenv("RESTACK_GT_STDOUT", "Hit conflict restacking branch feature")
	t.Setenv("RESTACK_GT_KILL", "1")

	_, _, err := runRestackCmd(t)
	if err == nil {
		t.Fatal("restack succeeded on a killed gt, want the conflict advice")
	}
	want := "restack: conflict — resolve the listed files, then gt continue (or gt abort); see the output above"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err, want)
	}
}

func TestRestackHelperFailuresCarryRestackPrefix(t *testing.T) {
	t.Run("gt lane", func(t *testing.T) {
		setupRestack(t, ".git", true, true)
		t.Setenv("RESTACK_GT_STATE", "not json")

		_, _, err := runRestackCmd(t)
		if err == nil {
			t.Fatal("restack succeeded on unparseable gt state, want failure")
		}
		if !strings.HasPrefix(err.Error(), "restack: parse gt state:") {
			t.Errorf("error = %v, want it to lead with restack's own prefix", err)
		}
		if strings.Contains(err.Error(), "ship:") {
			t.Errorf("error = %v, want restack's prefix, not ship's", err)
		}
	})

	t.Run("jj lane", func(t *testing.T) {
		setupRestack(t, ".jj", false, false)
		t.Setenv("RESTACK_JJ_LOG_FAIL", "1")

		_, _, err := runRestackCmd(t)
		if err == nil {
			t.Fatal("restack succeeded on a failed jj log, want failure")
		}
		if !strings.HasPrefix(err.Error(), "restack: jj trunk bookmark:") {
			t.Errorf("error = %v, want it to lead with restack's own prefix", err)
		}
		if strings.Contains(err.Error(), "ship:") {
			t.Errorf("error = %v, want restack's prefix, not ship's", err)
		}
	})
}

func TestRestackGraphiteFirst(t *testing.T) {
	t.Run("colocated routes to gt", func(t *testing.T) {
		logPath := setupRestack(t, ".jj", true, true)

		if _, _, err := runRestackCmd(t); err != nil {
			t.Fatalf("restack: %v", err)
		}
		records := readRestackLog(t, logPath)
		if len(records) < 2 || records[1][0] != "gt" {
			t.Fatalf("argv after the lane gate = %#v, want a gt command (routed to the gt lane, not jj)", records)
		}
	})

	t.Run("no gt routes to jj", func(t *testing.T) {
		logPath := setupRestack(t, ".jj", true, true)

		out, _, err := runRestackCmd(t, "--no-gt")
		if err != nil {
			t.Fatalf("restack --no-gt: %v", err)
		}
		if out != "fetched · rebased 2 commit(s) onto main" {
			t.Fatalf("output = %q", out)
		}
		for _, record := range readRestackLog(t, logPath) {
			if record[0] == "gt" {
				t.Fatalf("gt invoked under --no-gt: %#v", record)
			}
		}
	})
}

func TestRestackJJRebase(t *testing.T) {
	logPath := setupRestack(t, ".jj", false, true)

	out, _, err := runRestackCmd(t)
	if err != nil {
		t.Fatalf("restack: %v", err)
	}
	if out != "fetched · rebased 2 commit(s) onto main" {
		t.Fatalf("output = %q", out)
	}
	requireRestackRecords(t, logPath, [][]string{
		{"jj", "--ignore-working-copy", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
		{"jj", "git", "fetch"},
		{"jj", "--ignore-working-copy", "log", "-r", jjRestackAncestorRevset, "--no-graph", "-T", jjStackLineTemplate},
		{"jj", "--ignore-working-copy", "log", "-r", jjRestackStackRevset, "--no-graph", "-T", jjStackLineTemplate},
		{"jj", "rebase", "-b", "@", "--destination", "trunk()"},
		{"jj", "--ignore-working-copy", "op", "log", "-n", "1", "--no-graph", "-T", jjOpIDTemplate},
		{"jj", "--ignore-working-copy", "log", "-r", jjRestackConflictRevset, "--no-graph", "-T", jjStackLineTemplate},
	})
}

func TestRestackJJConflictRollsBack(t *testing.T) {
	logPath := setupRestack(t, ".jj", false, true)
	t.Setenv("RESTACK_JJ_CONFLICTS", "ddddddd conflict one\neeeeeee conflict two\n")

	_, _, err := runRestackCmd(t)
	if err == nil {
		t.Fatal("restack succeeded, want conflict")
	}
	for _, want := range []string{
		`restack: rebase onto "main" conflicts in 2 commit(s)`,
		"rolled back to the pre-rebase state",
		"ddddddd conflict one",
		"eeeeeee conflict two",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want substring %q", err, want)
		}
	}
	records := readRestackLog(t, logPath)
	if got := records[len(records)-1]; !reflect.DeepEqual(got, []string{"jj", "op", "revert", "op123abc"}) {
		t.Fatalf("last argv = %#v, want op revert", got)
	}
}

func TestRestackJJAlreadyUpToDate(t *testing.T) {
	logPath := setupRestack(t, ".jj", false, true)
	t.Setenv("RESTACK_JJ_UP_TO_DATE", "1")

	out, _, err := runRestackCmd(t)
	if err != nil {
		t.Fatalf("restack: %v", err)
	}
	if out != "fetched · already up to date" {
		t.Fatalf("output = %q", out)
	}
	requireRestackRecords(t, logPath, [][]string{
		{"jj", "--ignore-working-copy", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
		{"jj", "git", "fetch"},
		{"jj", "--ignore-working-copy", "log", "-r", jjRestackAncestorRevset, "--no-graph", "-T", jjStackLineTemplate},
	})
}

func TestRestackGitSymbolicHeadRebases(t *testing.T) {
	logPath := setupRestack(t, ".git", false, true)

	out, _, err := runRestackCmd(t)
	if err != nil {
		t.Fatalf("restack: %v", err)
	}
	if out != "fetched · rebased onto origin/main" {
		t.Fatalf("output = %q", out)
	}
	requireRestackRecords(t, logPath, [][]string{
		{"git", "branch", "--show-current"},
		{"git", "config", "--get", "branch.feature.remote"},
		{"git", "fetch", "origin"},
		{"git", "symbolic-ref", "--short", "refs/remotes/origin/HEAD"},
		{"git", "merge-base", "--is-ancestor", "origin/main", "HEAD"},
		{"git", "rev-list", "--count", "refs/remotes/origin/main..HEAD"},
		{"git", "rebase", "--autostash", "refs/remotes/origin/main"},
	})
}

func TestRestackGitProbesMainWhenSymbolicHeadMissing(t *testing.T) {
	logPath := setupRestack(t, ".git", false, true)
	t.Setenv("RESTACK_GIT_SYMBOLIC_MISS", "1")
	t.Setenv("RESTACK_GIT_MAIN_REF", "1")

	out, _, err := runRestackCmd(t)
	if err != nil {
		t.Fatalf("restack: %v", err)
	}
	if out != "fetched · rebased onto origin/main" {
		t.Fatalf("output = %q", out)
	}
	records := readRestackLog(t, logPath)
	want := []string{"git", "show-ref", "--verify", "--quiet", "refs/remotes/origin/main"}
	if len(records) < 5 || !reflect.DeepEqual(records[4], want) {
		t.Fatalf("show-ref argv = %#v, want %#v", records, want)
	}
}

// TestRestackGitFallsBackToMasterWhenMainAbsent covers the second candidate the
// probe loop tries. git answers a missing ref with a fatal, not the 1 an absent
// match reports, so a probe that does not ask for --quiet never reaches master.
func TestRestackGitFallsBackToMasterWhenMainAbsent(t *testing.T) {
	logPath := setupRestack(t, ".git", false, true)
	t.Setenv("RESTACK_GIT_SYMBOLIC_MISS", "1")
	t.Setenv("RESTACK_GIT_MASTER_REF", "1")

	out, _, err := runRestackCmd(t)
	if err != nil {
		t.Fatalf("restack: %v", err)
	}
	if out != "fetched · rebased onto origin/master" {
		t.Fatalf("output = %q, want %q", out, "fetched · rebased onto origin/master")
	}
	records := readRestackLog(t, logPath)
	for _, want := range [][]string{
		{"git", "show-ref", "--verify", "--quiet", "refs/remotes/origin/main"},
		{"git", "show-ref", "--verify", "--quiet", "refs/remotes/origin/master"},
		{"git", "rebase", "--autostash", "refs/remotes/origin/master"},
	} {
		found := false
		for _, record := range records {
			if reflect.DeepEqual(record, want) {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing %#v in %#v", want, records)
		}
	}
}

func TestRestackGitRefusesUnknownTrunk(t *testing.T) {
	logPath := setupRestack(t, ".git", false, true)
	t.Setenv("RESTACK_GIT_SYMBOLIC_MISS", "1")

	_, _, err := runRestackCmd(t)
	if err == nil {
		t.Fatal("restack succeeded, want refusal")
	}
	want := "restack: cannot resolve origin's default branch — run git remote set-head origin -a"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err, want)
	}
	records := readRestackLog(t, logPath)
	for _, wantRef := range []string{"refs/remotes/origin/main", "refs/remotes/origin/master"} {
		found := false
		for _, record := range records {
			if reflect.DeepEqual(record, []string{"git", "show-ref", "--verify", "--quiet", wantRef}) {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing show-ref probe for %s in %#v", wantRef, records)
		}
	}
}

func TestRestackGitFastForwardsTrunk(t *testing.T) {
	logPath := setupRestack(t, ".git", false, true)
	t.Setenv("RESTACK_GIT_BRANCH", "main")

	out, _, err := runRestackCmd(t)
	if err != nil {
		t.Fatalf("restack: %v", err)
	}
	if out != "fetched · fast-forwarded main" {
		t.Fatalf("output = %q", out)
	}
	records := readRestackLog(t, logPath)
	got := records[len(records)-1]
	want := []string{"git", "merge", "--ff-only", "origin/main"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("last argv = %#v, want %#v", got, want)
	}
}

func TestRestackGitAlreadyUpToDate(t *testing.T) {
	logPath := setupRestack(t, ".git", false, true)
	t.Setenv("RESTACK_GIT_UP_TO_DATE", "1")

	out, _, err := runRestackCmd(t)
	if err != nil {
		t.Fatalf("restack: %v", err)
	}
	if out != "fetched · already up to date" {
		t.Fatalf("output = %q", out)
	}
	records := readRestackLog(t, logPath)
	for _, record := range records {
		if len(record) > 1 && (record[1] == "rebase" || record[1] == "merge") {
			t.Fatalf("unexpected update command: %#v", record)
		}
	}
}

func TestRestackGitConflictUsesExistingAbortPath(t *testing.T) {
	logPath := setupRestack(t, ".git", false, true)
	t.Setenv("RESTACK_GIT_REBASE_CONFLICT", "1")

	_, _, err := runRestackCmd(t)
	if err == nil {
		t.Fatal("restack succeeded, want conflict")
	}
	if !strings.Contains(err.Error(), "conflicts in: conflict.txt; aborted back to the pre-rebase state") {
		t.Fatalf("error = %q", err)
	}
	records := readRestackLog(t, logPath)
	got := records[len(records)-1]
	if !reflect.DeepEqual(got, []string{"git", "rebase", "--abort"}) {
		t.Fatalf("last argv = %#v, want rebase --abort", got)
	}
}

func TestRestackRefusesMissingGT(t *testing.T) {
	logPath := setupRestack(t, ".git", true, false)

	_, _, err := runRestackCmd(t)
	if err == nil {
		t.Fatal("restack succeeded, want missing-gt refusal")
	}
	want := "restack: graphite config found but gt not on PATH — install graphite (brew install graphite) or pass --no-gt"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err, want)
	}
	requireRestackRecords(t, logPath, [][]string{nogtProbe})
}

func TestRestackRegisteredWithRebaseAlias(t *testing.T) {
	cmd := newVcsCmd()
	found, args, err := cmd.Find([]string{"rebase"})
	if err != nil {
		t.Fatalf("find rebase: %v", err)
	}
	if found.Name() != "restack" || len(args) != 0 {
		t.Fatalf("find rebase = %s %#v, want restack", found.Name(), args)
	}
}
