package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
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

	gt := "#!/bin/sh\n" + log("gt") + `case "$1" in
  sync)
    if [ -n "$RESTACK_GT_FAIL" ]; then
      printf '%s\n' "$RESTACK_GT_STDERR" >&2
      exit 1
    fi ;;
  state) printf '%s' "${RESTACK_GT_STATE:-{\"main\":{\"trunk\":true}}}" ;;
  *) printf 'fake gt: unmatched argv: %s\n' "$*" >&2; exit 2 ;;
esac
exit 0
`
	jj := "#!/bin/sh\n" + log("jj") + `case "$1 $2" in
  "git fetch")
    if [ -n "$RESTACK_JJ_FETCH_FAIL" ]; then printf 'jj fetch failed\n' >&2; exit 1; fi ;;
  "log -r")
    case "$3" in
      "trunk()") printf '%s' "${RESTACK_JJ_TRUNK_NAMES:-main}" ;;
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
	git := "#!/bin/sh\n" + log("git") + `case "$1 $2" in
  "branch --show-current") printf '%s\n' "${RESTACK_GIT_BRANCH:-feature}" ;;
  "config --get")
    if [ -n "$RESTACK_GIT_REMOTE" ]; then printf '%s\n' "$RESTACK_GIT_REMOTE"; else exit 1; fi ;;
  "symbolic-ref --short")
    if [ -n "$RESTACK_GIT_SYMBOLIC_MISS" ]; then exit 1; fi
    printf '%s/%s\n' "${RESTACK_GIT_REMOTE:-origin}" "${RESTACK_GIT_TRUNK:-main}" ;;
  "show-ref --verify")
    case "$3" in
      */main) if [ "$RESTACK_GIT_MAIN_REF" != 1 ]; then exit 1; fi ;;
      */master) if [ "$RESTACK_GIT_MASTER_REF" != 1 ]; then exit 1; fi ;;
      *) exit 1 ;;
    esac ;;
  "merge-base --is-ancestor")
    if [ -z "$RESTACK_GIT_UP_TO_DATE" ]; then exit 1; fi ;;
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
	t.Setenv("RESTACK_LOG", logPath)
	return logPath
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
	if out != "restacked · trunk main" {
		t.Fatalf("output = %q, want %q", out, "restacked · trunk main")
	}
	requireRestackRecords(t, logPath, [][]string{
		{"gt", "sync", "--no-interactive"},
		{"gt", "state"},
	})
}

func TestRestackGTFailures(t *testing.T) {
	tests := []struct {
		name   string
		stderr string
		want   string
		exact  bool
	}{
		{
			name:   "conflict",
			stderr: "Hit conflict restacking branch feature",
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
			name:   "unknown",
			stderr: "unclassified graphite failure",
			want:   "unclassified graphite failure",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logPath := setupRestack(t, ".git", true, true)
			t.Setenv("RESTACK_GT_FAIL", "1")
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
			requireRestackRecords(t, logPath, [][]string{
				{"gt", "sync", "--no-interactive"},
			})
		})
	}
}

func TestRestackGraphiteFirst(t *testing.T) {
	t.Run("colocated routes to gt", func(t *testing.T) {
		logPath := setupRestack(t, ".jj", true, true)

		if _, _, err := runRestackCmd(t); err != nil {
			t.Fatalf("restack: %v", err)
		}
		records := readRestackLog(t, logPath)
		if len(records) == 0 || !reflect.DeepEqual(records[0], []string{"gt", "sync", "--no-interactive"}) {
			t.Fatalf("first argv = %#v, want gt sync", records)
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
		{"jj", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
		{"jj", "git", "fetch"},
		{"jj", "log", "-r", jjRestackAncestorRevset, "--no-graph", "-T", jjStackLineTemplate},
		{"jj", "log", "-r", jjRestackStackRevset, "--no-graph", "-T", jjStackLineTemplate},
		{"jj", "rebase", "-b", "@", "--destination", "trunk()"},
		{"jj", "op", "log", "-n", "1", "--no-graph", "-T", jjOpIDTemplate},
		{"jj", "log", "-r", jjRestackConflictRevset, "--no-graph", "-T", jjStackLineTemplate},
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
		{"jj", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
		{"jj", "git", "fetch"},
		{"jj", "log", "-r", jjRestackAncestorRevset, "--no-graph", "-T", jjStackLineTemplate},
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
	want := []string{"git", "show-ref", "--verify", "refs/remotes/origin/main"}
	if len(records) < 5 || !reflect.DeepEqual(records[4], want) {
		t.Fatalf("show-ref argv = %#v, want %#v", records, want)
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
			if reflect.DeepEqual(record, []string{"git", "show-ref", "--verify", wantRef}) {
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
	if records := readRestackLog(t, logPath); len(records) != 0 {
		t.Fatalf("unexpected argv records: %#v", records)
	}
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
