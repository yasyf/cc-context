package cli

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

const (
	fakeHeadSHA        = "abcdef0123456789abcdef0123456789abcdef01"
	fakeRunListJSON    = `[{"databaseId":42,"workflowName":"ci","status":"in_progress","url":"https://github.com/x/actions/runs/42"}]`
	fakeRunViewSuccess = `{"workflowName":"ci","conclusion":"success","startedAt":"2026-07-08T18:00:00Z","updatedAt":"2026-07-08T18:00:58Z","url":"https://github.com/x/actions/runs/42","jobs":[]}`
	fakeRunViewFailure = `{"workflowName":"ci","conclusion":"failure","startedAt":"2026-07-08T18:00:00Z","updatedAt":"2026-07-08T18:01:00Z","url":"https://github.com/x/actions/runs/42","jobs":[{"name":"test","conclusion":"failure","steps":[{"name":"go test ./...","conclusion":"failure"}]}]}`
)

// writeShipFakes installs fake jj, git, and (when withGh) gh executables into
// dir. Each records its argv into $SHIP_LOG as a NUL-delimited record (every
// field terminated by \0, the record by one extra \0, so an argv element with
// embedded newlines stays one field) and emits canned stdout so the ship
// command's parsing paths run without a real VCS or network.
func writeShipFakes(t *testing.T, dir string, withGh bool) {
	t.Helper()
	log := func(name string) string {
		return "{ printf '" + name + "\\0'; for a in \"$@\"; do printf '%s\\0' \"$a\"; done; printf '\\0'; } >> \"$SHIP_LOG\"\n"
	}

	jj := "#!/bin/sh\n" + log("jj") + `case "$*" in
  root) printf '%s' "$SHIP_FAKE_ROOT" ;;
  "file list"*) printf 'f.txt\n' ;;
  "file show"*) printf '%s' "$JJ_FILE_SHOW_BASE" ;;
  "git fetch") if [ -n "$JJ_FETCH_FAIL" ]; then printf 'jj: cannot reach origin\n' >&2; exit 1; fi ;;
  "git push"*)
    if [ -n "$JJ_PUSH_REJECT_MARKER" ]; then
      count=0
      if [ -r "$JJ_PUSH_REJECT_MARKER" ]; then IFS= read -r count < "$JJ_PUSH_REJECT_MARKER" || :; fi
      count=${count:-0}
      if [ "$count" -gt 0 ]; then
        count=$((count - 1))
        printf '%s' "$count" > "$JJ_PUSH_REJECT_MARKER"
        printf '%s\n' "${JJ_PUSH_FAIL_STDERR:-Warning: The following references unexpectedly moved on the remote:
  refs/heads/main (reason: stale info)
Hint: Try fetching from the remote, then make the bookmark point to where you want it to be, and push again.
Error: Failed to push some bookmarks}" >&2
        exit 1
      fi
    fi ;;
  "op log "*)
    if [ -n "$JJ_OP_LOG_COUNTER" ]; then
      count=0
      if [ -r "$JJ_OP_LOG_COUNTER" ]; then IFS= read -r count < "$JJ_OP_LOG_COUNTER" || :; fi
      count=${count:-0}
      count=$((count + 1))
      printf '%s' "$count" > "$JJ_OP_LOG_COUNTER"
      printf 'op%03d' "$count"
    else
      printf 'op123abc'
    fi ;;
  "op revert"*) if [ -n "$JJ_OP_REVERT_FAIL" ]; then printf 'jj: op revert failed\n' >&2; exit 1; fi ;;
  rebase*) : ;;
  *"conflicts()"*)
    if [ -n "$JJ_CONFLICT_CHECK_FAIL" ]; then printf 'jj: conflict check failed\n' >&2; exit 1; fi
    printf '%s' "$JJ_CONFLICTS" ;;
  *"..@-"*) if [ -z "$JJ_STACK_EMPTY" ]; then printf 'b2c3d4e one\nc3d4e5f two\n'; fi ;;
  *"& ::@"*)
    diverged=$JJ_DIVERGED
    if [ -n "$JJ_DIVERGED_MARKER" ]; then
      count=0
      if [ -r "$JJ_DIVERGED_MARKER" ]; then IFS= read -r count < "$JJ_DIVERGED_MARKER" || :; fi
      count=${count:-0}
      count=$((count + 1))
      printf '%s' "$count" > "$JJ_DIVERGED_MARKER"
      if [ "$count" -gt "${JJ_DIVERGED_SWITCH_AFTER:-1}" ]; then diverged=1; fi
    fi
    if [ -z "$diverged" ]; then printf 'x '; fi ;;
  *"bookmarks(exact"*)
    case "${JJ_BOOKMARK_HEADS:-1}" in
      0) : ;;
      2) printf 'a1b2c3d subj\nb2c3d4e subj\n' ;;
      *) printf 'a1b2c3d subj\n' ;;
    esac ;;
  *"parents.len()"*) printf '%s' "${JJ_AT_PARENTS:-1}" ;;
  *first_line*)
    if [ -n "${JJ_DESCRIBE_OUTPUT+x}" ]; then
      printf '%s' "$JJ_DESCRIBE_OUTPUT"
    elif [ -n "$JJ_DESCRIBE_MARKER" ] && [ -s "$JJ_DESCRIBE_MARKER" ]; then
      printf '%s\n%s' 'e9f8a7b' 'fix: frobnicate'
    else
      if [ -n "$JJ_DESCRIBE_MARKER" ]; then printf 'x' >> "$JJ_DESCRIBE_MARKER"; fi
      printf '%s\n%s' 'a1b2c3d' 'fix: frobnicate'
    fi ;;
  *remote_bookmarks*) printf '%s' "${JJ_TRUNK_NAMES-main main}" ;;
  *local_bookmarks*) if [ -z "$JJ_NO_BOOKMARK" ]; then printf '%s' "${JJ_BOOKMARK_NAMES:-main}"; fi ;;
  *commit_id*)
    if [ -n "$JJ_COMMIT_ID_FAIL" ]; then printf 'jj: commit id unavailable\n' >&2; exit 1; fi
    printf '%s' '` + fakeHeadSHA + `' ;;
  "diff --name-only"*)
    if [ -n "$JJ_LOG_PWD" ]; then { printf 'pwd\0'; printf '%s\0' "$PWD"; printf '\0'; } >> "$SHIP_LOG"; fi
    names=$JJ_DIFF_NAMES
    if [ -n "$SHIP_DIFF_NAMES_MARKER" ]; then
      count=0
      if [ -r "$SHIP_DIFF_NAMES_MARKER" ]; then IFS= read -r count < "$SHIP_DIFF_NAMES_MARKER" || :; fi
      count=${count:-0}
      count=$((count + 1))
      printf '%s' "$count" > "$SHIP_DIFF_NAMES_MARKER"
      if [ "$count" -gt "${SHIP_DIFF_NAMES_SWITCH_AFTER:-1}" ]; then names=$JJ_DIFF_NAMES_2; fi
    fi
    printf '%s' "$names" ;;
  "bookmark list"*)
    printf '\tuntracked\n'
    printf 'git\ttracked\n'
    origin_tracked=1
    for r in $JJ_UNTRACKED_REMOTES; do
      printf '%s\tuntracked\n' "$r"
      if [ "$r" = origin ]; then origin_tracked=; fi
    done
    if [ -n "$origin_tracked" ]; then printf 'origin\ttracked\n'; fi ;;
  "config get git.push")
    if [ -n "$JJ_PUSH_REMOTE" ]; then printf '%s\n' "$JJ_PUSH_REMOTE"; else printf 'Config error: Value not found for git.push\n' >&2; exit 1; fi ;;
  commit*|squash*|bookmark*) : ;;
  *) printf 'fake jj: unmatched argv: %s\n' "$*" >&2; exit 2 ;;
esac
exit 0
`
	// When GIT_INDEX_FILE is set, log a leading "idx" record naming the temp index
	// basename so a test can assert which git calls carried the throwaway index.
	gitIdxMark := "if [ -n \"$GIT_INDEX_FILE\" ]; then { printf 'idx\\0'; printf '%s\\0' \"${GIT_INDEX_FILE##*/}\"; printf '\\0'; } >> \"$SHIP_LOG\"; fi\n"
	git := "#!/bin/sh\n" + gitIdxMark + log("git") + `case "$1 $2" in
  "log -1") printf '%s\0%s' 'a1b2c3d' 'fix: frobnicate' ;;
  "branch --show-current")
    branch=${GIT_BRANCH-main}
    if [ -n "$GIT_BRANCH_AFTER_COMMIT" ] && [ -e "$SHIP_LOG.git-committed" ]; then branch=$GIT_BRANCH_AFTER_COMMIT; fi
    printf '%s\n' "$branch" ;;
  "commit "*) : > "$SHIP_LOG.git-committed" ;;
  "rev-parse HEAD") printf '%s' '` + fakeHeadSHA + `' ;;
  "rev-parse --show-toplevel") printf '%s' "$SHIP_FAKE_ROOT" ;;
  "show --end-of-options") printf '%s' "$GIT_FILE_SHOW_BASE" ;;
  "ls-tree --full-tree") printf '100644 blob 1111111111111111111111111111111111111111\t%s\n' "$5" ;;
  "hash-object -w") printf '%s' "${GIT_HASH_OID:-2222222222222222222222222222222222222222}" ;;
  "diff --cached")
    if [ "$3" = "--quiet" ]; then
      if [ -n "$GIT_STAGED_EMPTY" ]; then exit 0; else exit 1; fi
    fi
    names=$GIT_DIFF_NAMES
    if [ -n "$SHIP_DIFF_NAMES_MARKER" ]; then
      count=0
      if [ -r "$SHIP_DIFF_NAMES_MARKER" ]; then IFS= read -r count < "$SHIP_DIFF_NAMES_MARKER" || :; fi
      count=${count:-0}
      count=$((count + 1))
      printf '%s' "$count" > "$SHIP_DIFF_NAMES_MARKER"
      if [ "$count" -gt 1 ]; then names=$GIT_DIFF_NAMES_2; fi
    fi
    printf '%s' "$names" | while IFS= read -r line || [ -n "$line" ]; do printf '%s\0' "$line"; done ;;
  "config --get") if [ -n "$GIT_BRANCH_REMOTE" ]; then printf '%s\n' "$GIT_BRANCH_REMOTE"; else exit 1; fi ;;
  fetch*) if [ -n "$GIT_FETCH_FAIL" ]; then printf 'git: cannot reach origin\n' >&2; exit 1; fi ;;
  "rev-parse --verify")
    case "$4" in
      REBASE_HEAD) if [ -n "$GIT_REBASE_CONFLICT" ]; then exit 0; else exit 1; fi ;;
      *) if [ -n "$GIT_REMOTE_REF_MISSING" ]; then exit 1; fi ;;
    esac ;;
  "merge-base --is-ancestor")
    if [ -n "$GIT_DIVERGED" ]; then exit 1; fi
    if [ -n "$GIT_DIVERGED_MARKER" ]; then
      count=0
      if [ -r "$GIT_DIVERGED_MARKER" ]; then IFS= read -r count < "$GIT_DIVERGED_MARKER" || :; fi
      count=${count:-0}
      count=$((count + 1))
      printf '%s' "$count" > "$GIT_DIVERGED_MARKER"
      if [ "$count" -gt "${GIT_DIVERGED_SWITCH_AFTER:-1}" ]; then exit 1; fi
    fi ;;
  "rev-list --count") printf '2' ;;
  "rebase --autostash")
    if [ -n "$GIT_REBASE_NO_START" ]; then printf 'error: cannot rebase: Your index contains uncommitted changes.\n' >&2; exit 1; fi
    if [ -n "$GIT_REBASE_CONFLICT" ]; then printf 'CONFLICT (content): Merge conflict in f.txt\n' >&2; exit 1; fi
    if [ -n "$GIT_AUTOSTASH_WARN" ]; then printf 'Created autostash: 54f649e\nYour local changes are stashed, however applying them\nresulted in conflicts.  You can either resolve the conflicts\nand then discard the stash with "git stash drop".\nSuccessfully rebased and updated refs/heads/main.\n' >&2; fi ;;
  "rebase --abort") : ;;
  "diff --name-only") printf 'f.txt\n' ;;
  "push"*)
    case "$*" in
      *--force-with-lease=*)
        if [ -n "$GIT_LEASE_STALE" ]; then printf '! [rejected] main -> main (stale info)\nerror: failed to push some refs\n' >&2; exit 1; fi ;;
      *)
        if [ -n "$GIT_PUSH_FAIL_STDERR" ]; then printf '%s\n' "$GIT_PUSH_FAIL_STDERR" >&2; exit 1; fi
        if [ -n "$GIT_AMEND_PLAIN_NONFF" ]; then printf '! [rejected] main -> main (non-fast-forward)\nerror: failed to push some refs\n' >&2; exit 1; fi
        if [ -n "$GIT_PUSH_REJECT_MARKER" ]; then
          count=0
          if [ -r "$GIT_PUSH_REJECT_MARKER" ]; then IFS= read -r count < "$GIT_PUSH_REJECT_MARKER" || :; fi
          count=${count:-0}
          if [ "$count" -gt 0 ]; then
            count=$((count - 1))
            printf '%s' "$count" > "$GIT_PUSH_REJECT_MARKER"
            printf '! [rejected] main -> main (non-fast-forward)\nerror: failed to push some refs\n' >&2
            exit 1
          fi
        fi ;;
    esac ;;
  "add"*|"read-tree"*|"update-index"*|"restore"*) : ;;
  *) printf 'fake git: unmatched argv: %s\n' "$*" >&2; exit 2 ;;
esac
exit 0
`
	uvx := "#!/bin/sh\n" + log("uvx") + `if [ -n "$UVX_PREK_FAIL_MARKER" ]; then
  count=0
  if [ -r "$UVX_PREK_FAIL_MARKER" ]; then IFS= read -r count < "$UVX_PREK_FAIL_MARKER" || :; fi
  count=${count:-0}
  if [ "$count" -gt 0 ]; then
    count=$((count - 1))
    printf '%s' "$count" > "$UVX_PREK_FAIL_MARKER"
    printf 'files were modified by this hook\n' >&2
    exit 1
  fi
fi
exit 0
`
	gt := "#!/bin/sh\n" + log("gt") + `case "$1" in
  state) printf '%s' "$GT_STATE_JSON" ;;
  create|modify)
    : > "$SHIP_LOG.git-committed" ;;
  submit)
    if [ -n "$GT_SUBMIT_FAIL_STDERR" ]; then printf '%s\n' "$GT_SUBMIT_FAIL_STDERR" >&2; exit 1; fi ;;
  *) printf 'fake gt: unmatched argv: %s\n' "$*" >&2; exit 2 ;;
esac
exit 0
`
	gh := "#!/bin/sh\n" + log("gh") + `case "$1 $2" in
  "pr view")
    if [ -n "$GH_PR_VIEW_NOT_FOUND" ]; then
      printf 'no pull requests found for branch "%s"\n' "$3" >&2
      exit 1
    fi
    printf '%s' "$GH_PR_VIEW_JSON" ;;
  "run list")
    if [ -n "$GH_LIST_FAIL" ]; then printf 'gh: network timeout\n' >&2; exit 1; fi
    if [ -n "$GH_LIST_FAIL_MARKER" ] && [ -s "$GH_LIST_FAIL_MARKER" ]; then
      : > "$GH_LIST_FAIL_MARKER"; printf 'gh: transient tls timeout\n' >&2; exit 1
    fi
    if [ -n "$GH_LIST_SETTLE_MARKER" ]; then
      count=0
      if [ -r "$GH_LIST_SETTLE_MARKER" ]; then IFS= read -r count < "$GH_LIST_SETTLE_MARKER" || :; fi
      count=${count:-0}
      count=$((count + 1))
      printf '%s' "$count" > "$GH_LIST_SETTLE_MARKER"
      if [ "$count" -le "${GH_LIST_SETTLE_AFTER:-1}" ]; then printf '%s' "$GH_RUN_LIST_JSON"
      else printf '%s' "$GH_RUN_LIST_JSON_2"; fi
    else
      printf '%s' "$GH_RUN_LIST_JSON"
    fi ;;
  "run watch")
    id="$3"
    eval "code=\${GH_WATCH_EXIT_$id:-\${GH_WATCH_EXIT:-0}}"
    printf 'watch stream %s\n' "$id"
    if [ "$code" != 0 ]; then printf 'run %s concluded failure\n' "$id" >&2; fi
    exit "$code" ;;
  "run view")
    id="$3"
    case "$*" in
      *--log-failed*) eval "printf '%s' \"\${GH_LOG_FAILED_$id:-\$GH_LOG_FAILED}\"" ;;
      *) eval "printf '%s' \"\${GH_RUN_VIEW_JSON_$id:-\$GH_RUN_VIEW_JSON}\"" ;;
    esac ;;
esac
exit 0
`
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o700); err != nil { //nolint:gosec // fake executable must be owner-executable
			t.Fatalf("write fake %s: %v", name, err)
		}
	}
	write("jj", jj)
	write("git", git)
	write("uvx", uvx)
	write("gt", gt)
	if withGh {
		write("gh", gh)
	}
}

// setupShip stands up an isolated repo of the given marker (".git" or ".jj"),
// chdirs into it, puts the fakes on PATH, and points $SHIP_LOG at a fresh log.
// It returns the log path so a test can assert the exact argv sequence.
func setupShip(t *testing.T, marker string, withGh bool) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake shell scripts are POSIX-only")
	}
	dir := t.TempDir()
	if marker != "" {
		if err := os.Mkdir(filepath.Join(dir, marker), 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", marker, err)
		}
	}
	binDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(binDir, 0o750); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	writeShipFakes(t, binDir, withGh)

	old, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	// The fakes echo this as the repo root (git rev-parse --show-toplevel / jj
	// root); it is the post-chdir cwd, so it matches the frame rootRel resolves.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Setenv("SHIP_FAKE_ROOT", wd)

	t.Setenv("PATH", binDir)
	t.Setenv("JJ_DIFF_NAMES", "f.txt\n")
	// Zero the session id so subtests asserting bare commit argv stay green even
	// when the suite runs inside a Claude Code session, which exports it.
	t.Setenv(envClaudeSessionKey, "")
	log := filepath.Join(dir, "ship.log")
	t.Setenv("SHIP_LOG", log)
	return log
}

// setupShipGT extends setupShip with a live Graphite config and a default gt
// state tracking the current branch "feature" as a one-deep stack on trunk
// "main", routing ship to the gt lane. withGh mirrors setupShip's fake-gh
// toggle.
func setupShipGT(t *testing.T, withGh bool) string {
	t.Helper()
	log := setupShip(t, ".git", withGh)
	if err := os.WriteFile(filepath.Join(".git", ".graphite_repo_config"), []byte("{}"), 0o644); err != nil { //nolint:gosec // test fixture file
		t.Fatalf("write .graphite_repo_config: %v", err)
	}
	t.Setenv("GIT_BRANCH", "feature")
	t.Setenv("GT_STATE_JSON", `{"main":{"trunk":true},"feature":{"parents":[{"ref":"main","sha":"deadbeef"}]}}`)
	return log
}

func runShipCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newShipCmd()
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetArgs(args)
	err := cmd.Execute()
	// A standalone ship command has no root to set SilenceUsage, so cobra appends
	// its usage to stdout after an error; the summary is always the first line.
	summary := out.String()
	if i := strings.IndexByte(summary, '\n'); i >= 0 {
		summary = summary[:i]
	}
	return summary, err
}

// runShipCmdFull runs ship with usage and cobra error echo silenced so the whole
// captured stdout (summary plus every report line) can be asserted verbatim.
func runShipCmdFull(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	cmd := newShipCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), errBuf.String(), err
}

func readInvocations(t *testing.T, log string) [][]string {
	t.Helper()
	data, err := os.ReadFile(log)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read log: %v", err)
	}
	var got [][]string
	for _, rec := range strings.Split(string(data), "\x00\x00") {
		rec = strings.Trim(rec, "\x00")
		if rec == "" {
			continue
		}
		got = append(got, strings.Split(rec, "\x00"))
	}
	return got
}

func assertInvocations(t *testing.T, got, want [][]string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("invocation sequence mismatch\n got: %v\nwant: %v", got, want)
	}
}

func assertNoShipCommit(t *testing.T, invocations [][]string) {
	t.Helper()
	for _, inv := range invocations {
		if len(inv) > 1 && (inv[0] == "jj" && (inv[1] == "commit" || inv[1] == "squash") || inv[0] == "git" && inv[1] == "commit") {
			t.Errorf("commit ran before target resolution refused: %v", inv)
		}
	}
}

func TestShipCommitPushWatch(t *testing.T) {
	tests := []struct {
		name    string
		marker  string
		args    []string
		want    [][]string
		summary string
	}{
		{
			name:   "jj happy path",
			marker: ".jj",
			args:   []string{"-m", "fix: frobnicate"},
			want: [][]string{
				{"jj", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
				{"jj", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "log", "-r", `bookmarks(exact:"main")`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "diff", "--name-only"},
				{"jj", "commit", "-m", "fix: frobnicate"},
				{"jj", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
				{"jj", "bookmark", "list", jjExactPattern("main"), "--all-remotes", "-T", jjRemoteBookmarkTemplate},
				{"jj", "git", "fetch"},
				{"jj", "log", "-r", `bookmarks(exact:"main")`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "log", "-r", fmt.Sprintf(jjAncestorRevsetFmt, "main"), "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "bookmark", "move", "exact:main", "--to", "@-"},
				{"jj", "op", "log", "-n", "1", "--no-graph", "-T", jjOpIDTemplate},
				{"jj", "git", "push", "--bookmark", "exact:main"},
				{"jj", "log", "-r", "@-", "--no-graph", "-T", "commit_id"},
				{"gh", "run", "list", "--commit", fakeHeadSHA, "--limit", "50", "--json", "databaseId,workflowName,status,url"},
				{"gh", "run", "watch", "42", "--exit-status"},
				{"gh", "run", "view", "42", "--json", "workflowName,conclusion,startedAt,updatedAt,url,jobs"},
				{"gh", "run", "list", "--commit", fakeHeadSHA, "--limit", "50", "--json", "databaseId,workflowName,status,url"},
				{"gh", "run", "list", "--commit", fakeHeadSHA, "--limit", "50", "--json", "databaseId,workflowName,status,url"},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · pushed main → origin · CI success`,
		},
		{
			name:   "git happy path",
			marker: ".git",
			args:   []string{"-m", "fix: frobnicate"},
			want: [][]string{
				{"git", "branch", "--show-current"},
				{"git", "add", "-A"},
				{"git", "commit", "-m", "fix: frobnicate"},
				{"git", "log", "-1", "--format=%h%x00%s"},
				{"git", "branch", "--show-current"},
				{"git", "config", "--get", "branch.main.remote"},
				{"git", "fetch", "origin"},
				{"git", "rev-parse", "--verify", "--quiet", "refs/remotes/origin/main"},
				{"git", "merge-base", "--is-ancestor", "refs/remotes/origin/main", "HEAD"},
				{"git", "push", "origin", "main"},
				{"git", "rev-parse", "HEAD"},
				{"gh", "run", "list", "--commit", fakeHeadSHA, "--limit", "50", "--json", "databaseId,workflowName,status,url"},
				{"gh", "run", "watch", "42", "--exit-status"},
				{"gh", "run", "view", "42", "--json", "workflowName,conclusion,startedAt,updatedAt,url,jobs"},
				{"gh", "run", "list", "--commit", fakeHeadSHA, "--limit", "50", "--json", "databaseId,workflowName,status,url"},
				{"gh", "run", "list", "--commit", fakeHeadSHA, "--limit", "50", "--json", "databaseId,workflowName,status,url"},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · pushed main → origin · CI success`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := setupShip(t, tt.marker, true)
			t.Setenv("GH_RUN_LIST_JSON", fakeRunListJSON)
			t.Setenv("GH_RUN_VIEW_JSON", fakeRunViewSuccess)
			shipCIPollInterval = 0

			got, err := runShipCmd(t, tt.args...)
			if err != nil {
				t.Fatalf("ship error = %v", err)
			}
			if got != tt.summary {
				t.Errorf("summary = %q, want %q", got, tt.summary)
			}
			assertInvocations(t, readInvocations(t, log), tt.want)
		})
	}
}

// writeShipHookFiles writes a prek config and the named files (empty content) at
// root, so shipHookFiles' on-disk filter and prek's --files scope both see them.
func writeShipHookFiles(t *testing.T, root string, names ...string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, ".pre-commit-config.yaml"), []byte("repos: []\n"), 0o600); err != nil {
		t.Fatalf("write pre-commit config: %v", err)
	}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(root, name), []byte("x"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

func TestShipHooksPass(t *testing.T) {
	tests := []struct {
		name   string
		marker string
		want   [][]string
	}{
		{
			name:   "jj",
			marker: ".jj",
			want: [][]string{
				{"jj", "diff", "--name-only"},
				{"jj", "diff", "--name-only"},
				{"uvx", "prek", "run", "--cd", "ROOT", "--files", "f1.go"},
				{"jj", "commit", "-m", "fix: frobnicate"},
				{"jj", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
			},
		},
		{
			name:   "git",
			marker: ".git",
			want: [][]string{
				{"git", "add", "-A"},
				{"git", "diff", "--cached", "--name-only", "--diff-filter=d", "-z"},
				{"uvx", "prek", "run", "--cd", "ROOT", "--files", "f1.go"},
				{"git", "commit", "-m", "fix: frobnicate"},
				{"git", "log", "-1", "--format=%h%x00%s"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := setupShip(t, tt.marker, false)
			root, err := os.Getwd()
			if err != nil {
				t.Fatalf("getwd: %v", err)
			}
			if tt.marker == ".jj" {
				if err := os.Mkdir(filepath.Join(root, ".git"), 0o750); err != nil {
					t.Fatalf("mkdir .git: %v", err)
				}
			}
			writeShipHookFiles(t, root, "f1.go")
			t.Setenv("JJ_DIFF_NAMES", "f1.go\n")
			t.Setenv("GIT_DIFF_NAMES", "f1.go\n")

			for i, rec := range tt.want {
				for j, field := range rec {
					if field == "ROOT" {
						tt.want[i][j] = root
					}
				}
			}

			got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
			if err != nil {
				t.Fatalf("ship error = %v", err)
			}
			want := `hooks ok · committed a1b2c3d "fix: frobnicate" · not pushed`
			if got != want {
				t.Errorf("summary = %q, want %q", got, want)
			}
			assertInvocations(t, readInvocations(t, log), tt.want)
		})
	}
}

func TestShipHooksJJAmend(t *testing.T) {
	log := setupShip(t, ".jj", false)
	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o750); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	writeShipHookFiles(t, root, "folded.go")
	t.Setenv("JJ_DIFF_NAMES", "folded.go\n")

	got, err := runShipCmd(t, "--amend", "--no-push")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := `hooks ok · committed a1b2c3d "fix: frobnicate" · not pushed`
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	assertInvocations(t, readInvocations(t, log), [][]string{
		{"jj", "diff", "--name-only"},
		{"uvx", "prek", "run", "--cd", root, "--files", "folded.go"},
		{"jj", "squash", "--use-destination-message"},
		{"jj", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
	})
}

func TestShipHooksSubdirRunsAtRoot(t *testing.T) {
	log := setupShip(t, ".jj", false)
	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o750); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o750); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	writeShipHookFiles(t, root, "sub/x.go")
	t.Setenv("JJ_DIFF_NAMES", "sub/x.go\n")
	t.Setenv("JJ_LOG_PWD", "1")
	if err := os.Chdir(filepath.Join(root, "sub")); err != nil {
		t.Fatalf("chdir sub: %v", err)
	}

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "x.go")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := `hooks ok · committed a1b2c3d "fix: frobnicate" · not pushed`
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	assertInvocations(t, readInvocations(t, log), [][]string{
		{"jj", "diff", "--name-only", "--", "sub/x.go"},
		{"pwd", root},
		{"jj", "diff", "--name-only", "--", "sub/x.go"},
		{"pwd", root},
		{"uvx", "prek", "run", "--cd", root, "--files", "sub/x.go"},
		{"jj", "commit", "-m", "fix: frobnicate", "--", "x.go"},
		{"jj", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
	})
}

func TestShipHooksAutoFixLeavingNothingAborts(t *testing.T) {
	tests := []struct {
		name   string
		marker string
	}{
		{"jj", ".jj"},
		{"git", ".git"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := setupShip(t, tt.marker, false)
			root, err := os.Getwd()
			if err != nil {
				t.Fatalf("getwd: %v", err)
			}
			if tt.marker == ".jj" {
				if err := os.Mkdir(filepath.Join(root, ".git"), 0o750); err != nil {
					t.Fatalf("mkdir .git: %v", err)
				}
			}
			writeShipHookFiles(t, root, "f1.go")
			t.Setenv("JJ_DIFF_NAMES", "f1.go\n")
			t.Setenv("GIT_DIFF_NAMES", "f1.go\n")
			// The hooks' second derivation returns nothing: the auto-fixer reverted the change.
			namesMarker := filepath.Join(root, "names.marker")
			if err := os.WriteFile(namesMarker, []byte("0"), 0o600); err != nil {
				t.Fatalf("write names marker: %v", err)
			}
			t.Setenv("SHIP_DIFF_NAMES_MARKER", namesMarker)
			t.Setenv("JJ_DIFF_NAMES_2", "")
			t.Setenv("GIT_DIFF_NAMES_2", "")
			if tt.marker == ".jj" {
				t.Setenv("SHIP_DIFF_NAMES_SWITCH_AFTER", "2")
			}
			failMarker := filepath.Join(root, "prek.marker")
			if err := os.WriteFile(failMarker, []byte("1"), 0o600); err != nil {
				t.Fatalf("write fail marker: %v", err)
			}
			t.Setenv("UVX_PREK_FAIL_MARKER", failMarker)

			_, err = runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
			if err == nil || !strings.Contains(err.Error(), "nothing to commit") {
				t.Fatalf("ship error = %v, want nothing-to-commit", err)
			}
			uvxCount, jjDiffCount := 0, 0
			for _, inv := range readInvocations(t, log) {
				if inv[0] == "uvx" {
					uvxCount++
				}
				if len(inv) >= 3 && inv[0] == "jj" && inv[1] == "diff" && inv[2] == "--name-only" {
					jjDiffCount++
				}
				if inv[0] == "jj" && (inv[1] == "commit" || inv[1] == "squash") {
					t.Errorf("jj commit ran after hooks emptied the change: %v", inv)
				}
				if inv[0] == "git" && inv[1] == "commit" {
					t.Errorf("git commit ran after hooks emptied the change: %v", inv)
				}
			}
			if uvxCount != 1 {
				t.Errorf("uvx invocation count = %d, want 1 (no retry on an empty re-derive)", uvxCount)
			}
			if tt.marker == ".jj" && jjDiffCount != 3 {
				t.Errorf("jj diff --name-only invocation count = %d, want 3", jjDiffCount)
			}
		})
	}
}

func TestShipHooksAutoFixThenPass(t *testing.T) {
	tests := []struct {
		name   string
		marker string
	}{
		{"jj", ".jj"},
		{"git", ".git"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := setupShip(t, tt.marker, false)
			root, err := os.Getwd()
			if err != nil {
				t.Fatalf("getwd: %v", err)
			}
			if tt.marker == ".jj" {
				if err := os.Mkdir(filepath.Join(root, ".git"), 0o750); err != nil {
					t.Fatalf("mkdir .git: %v", err)
				}
			}
			writeShipHookFiles(t, root, "f1.go")
			t.Setenv("JJ_DIFF_NAMES", "f1.go\n")
			t.Setenv("GIT_DIFF_NAMES", "f1.go\n")
			marker := filepath.Join(root, "prek.marker")
			if err := os.WriteFile(marker, []byte("1"), 0o600); err != nil {
				t.Fatalf("write marker: %v", err)
			}
			t.Setenv("UVX_PREK_FAIL_MARKER", marker)

			got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
			if err != nil {
				t.Fatalf("ship error = %v", err)
			}
			want := `hooks fixed · committed a1b2c3d "fix: frobnicate" · not pushed`
			if got != want {
				t.Errorf("summary = %q, want %q", got, want)
			}
			uvxCount, gitAddCount := 0, 0
			for _, inv := range readInvocations(t, log) {
				if inv[0] == "uvx" {
					uvxCount++
				}
				if tt.marker == ".git" && len(inv) >= 3 && inv[0] == "git" && inv[1] == "add" && inv[2] == "-A" {
					gitAddCount++
				}
			}
			if uvxCount != 2 {
				t.Errorf("uvx invocation count = %d, want 2", uvxCount)
			}
			if tt.marker == ".git" && gitAddCount != 2 {
				t.Errorf("git add -A invocation count = %d, want 2", gitAddCount)
			}
		})
	}
}

func TestShipHooksRetryRederivesFiles(t *testing.T) {
	tests := []struct {
		name   string
		marker string
	}{
		{"jj", ".jj"},
		{"git", ".git"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := setupShip(t, tt.marker, false)
			root, err := os.Getwd()
			if err != nil {
				t.Fatalf("getwd: %v", err)
			}
			if tt.marker == ".jj" {
				if err := os.Mkdir(filepath.Join(root, ".git"), 0o750); err != nil {
					t.Fatalf("mkdir .git: %v", err)
				}
			}
			writeShipHookFiles(t, root, "first.go", "generated.go")
			t.Setenv("JJ_DIFF_NAMES", "first.go\n")
			t.Setenv("JJ_DIFF_NAMES_2", "generated.go\n")
			t.Setenv("GIT_DIFF_NAMES", "first.go\n")
			t.Setenv("GIT_DIFF_NAMES_2", "generated.go\n")
			diffMarker := filepath.Join(root, "diff.marker")
			t.Setenv("SHIP_DIFF_NAMES_MARKER", diffMarker)
			if tt.marker == ".jj" {
				t.Setenv("SHIP_DIFF_NAMES_SWITCH_AFTER", "2")
			}
			failMarker := filepath.Join(root, "prek.marker")
			if err := os.WriteFile(failMarker, []byte("1"), 0o600); err != nil {
				t.Fatalf("write prek marker: %v", err)
			}
			t.Setenv("UVX_PREK_FAIL_MARKER", failMarker)

			got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
			if err != nil {
				t.Fatalf("ship error = %v", err)
			}
			want := `hooks fixed · committed a1b2c3d "fix: frobnicate" · not pushed`
			if got != want {
				t.Errorf("summary = %q, want %q", got, want)
			}
			var uvx [][]string
			jjDiffCount := 0
			for _, inv := range readInvocations(t, log) {
				if inv[0] == "uvx" {
					uvx = append(uvx, inv)
				}
				if len(inv) >= 3 && inv[0] == "jj" && inv[1] == "diff" && inv[2] == "--name-only" {
					jjDiffCount++
				}
			}
			wantUVX := [][]string{
				{"uvx", "prek", "run", "--cd", root, "--files", "first.go"},
				{"uvx", "prek", "run", "--cd", root, "--files", "generated.go"},
			}
			assertInvocations(t, uvx, wantUVX)
			if tt.marker == ".jj" && jjDiffCount != 3 {
				t.Errorf("jj diff --name-only invocation count = %d, want 3", jjDiffCount)
			}
		})
	}
}

func TestShipHooksPersistentFailure(t *testing.T) {
	log := setupShip(t, ".git", false)
	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	writeShipHookFiles(t, root, "f1.go")
	t.Setenv("GIT_DIFF_NAMES", "f1.go\n")
	marker := filepath.Join(root, "prek.marker")
	if err := os.WriteFile(marker, []byte("2"), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	t.Setenv("UVX_PREK_FAIL_MARKER", marker)

	_, err = runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
	if err == nil || !strings.Contains(err.Error(), "ship: hooks:") {
		t.Fatalf("ship error = %v, want containing %q", err, "ship: hooks:")
	}
	for _, inv := range readInvocations(t, log) {
		if inv[0] == "git" && len(inv) > 1 && inv[1] == "commit" {
			t.Errorf("commit ran after persistent hook failure: %v", inv)
		}
	}
}

func TestShipHooksNoVerify(t *testing.T) {
	log := setupShip(t, ".git", false)
	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	writeShipHookFiles(t, root, "f1.go")
	t.Setenv("GIT_DIFF_NAMES", "f1.go\n")

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "--no-verify")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := `committed a1b2c3d "fix: frobnicate" · not pushed`
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	var commit []string
	for _, inv := range readInvocations(t, log) {
		if inv[0] == "uvx" {
			t.Errorf("uvx invoked with --no-verify: %v", inv)
		}
		if len(inv) > 1 && inv[0] == "git" && inv[1] == "commit" {
			commit = inv
		}
	}
	wantCommit := []string{"git", "commit", "-m", "fix: frobnicate", "--no-verify"}
	if !reflect.DeepEqual(commit, wantCommit) {
		t.Errorf("commit argv = %v, want %v", commit, wantCommit)
	}
}

func TestShipHooksNoConfig(t *testing.T) {
	log := setupShip(t, ".git", false)

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := `committed a1b2c3d "fix: frobnicate" · not pushed`
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	for _, inv := range readInvocations(t, log) {
		if inv[0] == "uvx" {
			t.Errorf("uvx invoked without a config file: %v", inv)
		}
	}
}

func TestShipHooksUvxMissing(t *testing.T) {
	log := setupShip(t, ".git", false)
	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	writeShipHookFiles(t, root, "f1.go")
	t.Setenv("GIT_DIFF_NAMES", "f1.go\n")
	if err := os.Remove(filepath.Join(filepath.Dir(log), "bin", "uvx")); err != nil {
		t.Fatalf("remove fake uvx: %v", err)
	}

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := `hooks uvx-missing · committed a1b2c3d "fix: frobnicate" · not pushed`
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
}

func TestShipHooksJJNoGitMarker(t *testing.T) {
	log := setupShip(t, ".jj", false)
	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	writeShipHookFiles(t, root, "f1.go")
	t.Setenv("JJ_DIFF_NAMES", "f1.go\n")

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := `hooks no-git · committed a1b2c3d "fix: frobnicate" · not pushed`
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	for _, inv := range readInvocations(t, log) {
		if inv[0] == "uvx" {
			t.Errorf("uvx invoked for a jj repo without a .git marker: %v", inv)
		}
	}
}

func TestShipHooksEmptyFilesSkipSoftGuards(t *testing.T) {
	tests := []struct {
		name      string
		marker    string
		removeUVX bool
	}{
		{"jj without git", ".jj", false},
		{"git without uvx", ".git", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := setupShip(t, tt.marker, false)
			root, err := os.Getwd()
			if err != nil {
				t.Fatalf("getwd: %v", err)
			}
			writeShipHookFiles(t, root)
			if tt.marker == ".jj" {
				t.Setenv("JJ_DIFF_NAMES", "")
				_, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
				if err == nil || !strings.Contains(err.Error(), "nothing to commit") {
					t.Fatalf("ship error = %v, want nothing-to-commit", err)
				}
				invocations := readInvocations(t, log)
				assertInvocations(t, invocations, [][]string{
					{"jj", "diff", "--name-only"},
					{"jj", "log", "-r", "@", "--no-graph", "-T", jjAtStateTemplate},
					{"jj", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
				})
				for _, inv := range invocations {
					if inv[0] == "uvx" || inv[0] == "jj" && (inv[1] == "commit" || inv[1] == "squash") {
						t.Errorf("empty jj ship ran hooks or committed: %v", inv)
					}
				}
				return
			}
			if tt.removeUVX {
				if err := os.Remove(filepath.Join(filepath.Dir(log), "bin", "uvx")); err != nil {
					t.Fatalf("remove fake uvx: %v", err)
				}
			}

			got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
			if err != nil {
				t.Fatalf("ship error = %v", err)
			}
			want := `committed a1b2c3d "fix: frobnicate" · not pushed`
			if got != want {
				t.Errorf("summary = %q, want %q", got, want)
			}
			for _, inv := range readInvocations(t, log) {
				if inv[0] == "uvx" {
					t.Errorf("uvx invoked with no changed files: %v", inv)
				}
			}
		})
	}
}

func TestShipHooksScopedPaths(t *testing.T) {
	log := setupShip(t, ".git", false)
	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	writeShipHookFiles(t, root)
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o750); err != nil {
		t.Fatalf("mkdir src: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "a.go"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write src/a.go: %v", err)
	}
	t.Setenv("GIT_DIFF_NAMES", "src/a.go\n")

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "src/a.go")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := `hooks ok · committed a1b2c3d "fix: frobnicate" · not pushed`
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	want2 := [][]string{
		{"git", "add", "-A", "--", "src/a.go"},
		{"git", "diff", "--cached", "--name-only", "--diff-filter=d", "-z", "--", "src/a.go"},
		{"uvx", "prek", "run", "--cd", root, "--files", "src/a.go"},
		{"git", "commit", "-m", "fix: frobnicate", "--", "src/a.go"},
		{"git", "log", "-1", "--format=%h%x00%s"},
	}
	assertInvocations(t, readInvocations(t, log), want2)
}

func TestShipHooksFiltersMissingFile(t *testing.T) {
	log := setupShip(t, ".git", false)
	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	writeShipHookFiles(t, root, "f1.go")
	t.Setenv("GIT_DIFF_NAMES", "f1.go\ngone.go\n")

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := `hooks ok · committed a1b2c3d "fix: frobnicate" · not pushed`
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	for _, inv := range readInvocations(t, log) {
		if inv[0] == "uvx" {
			for _, f := range inv {
				if f == "gone.go" {
					t.Errorf("--files listed a deleted file: %v", inv)
				}
			}
		}
	}
}

func TestShipHooksPreserveHookableFilenames(t *testing.T) {
	tests := []struct {
		name     string
		filename string
		create   func(*testing.T, string)
	}{
		{
			name:     "non-ASCII",
			filename: "café.go",
			create: func(t *testing.T, path string) {
				t.Helper()
				if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
					t.Fatalf("write %s: %v", path, err)
				}
			},
		},
		{
			name:     "broken symlink",
			filename: "broken.go",
			create: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Symlink("missing-target", path); err != nil {
					t.Fatalf("symlink %s: %v", path, err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := setupShip(t, ".git", false)
			root, err := os.Getwd()
			if err != nil {
				t.Fatalf("getwd: %v", err)
			}
			writeShipHookFiles(t, root)
			tt.create(t, filepath.Join(root, tt.filename))
			t.Setenv("GIT_DIFF_NAMES", tt.filename+"\n")

			got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
			if err != nil {
				t.Fatalf("ship error = %v", err)
			}
			want := `hooks ok · committed a1b2c3d "fix: frobnicate" · not pushed`
			if got != want {
				t.Errorf("summary = %q, want %q", got, want)
			}
			var uvx []string
			for _, inv := range readInvocations(t, log) {
				if inv[0] == "uvx" {
					uvx = inv
				}
			}
			wantUVX := []string{"uvx", "prek", "run", "--cd", root, "--files", tt.filename}
			if !reflect.DeepEqual(uvx, wantUVX) {
				t.Errorf("uvx argv = %v, want %v", uvx, wantUVX)
			}
		})
	}
}

func TestShipJJNeverInvokesGitCommit(t *testing.T) {
	log := setupShip(t, ".jj", true)
	t.Setenv("GH_RUN_LIST_JSON", fakeRunListJSON)
	t.Setenv("GH_RUN_VIEW_JSON", fakeRunViewSuccess)
	shipCIPollInterval = 0

	if _, err := runShipCmd(t, "-m", "fix: frobnicate"); err != nil {
		t.Fatalf("ship error = %v", err)
	}
	for _, inv := range readInvocations(t, log) {
		if inv[0] == "git" {
			t.Errorf("jj path invoked git: %v", inv)
		}
	}
}

func TestShipCommitOnlyVariants(t *testing.T) {
	tests := []struct {
		name    string
		marker  string
		args    []string
		want    [][]string
		summary string
	}{
		{
			name:   "jj no-push",
			marker: ".jj",
			args:   []string{"-m", "fix: frobnicate", "--no-push"},
			want: [][]string{
				{"jj", "diff", "--name-only"},
				{"jj", "commit", "-m", "fix: frobnicate"},
				{"jj", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · not pushed`,
		},
		{
			name:   "git no-push",
			marker: ".git",
			args:   []string{"-m", "fix: frobnicate", "--no-push"},
			want: [][]string{
				{"git", "add", "-A"},
				{"git", "commit", "-m", "fix: frobnicate"},
				{"git", "log", "-1", "--format=%h%x00%s"},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · not pushed`,
		},
		{
			name:   "jj amend no message",
			marker: ".jj",
			args:   []string{"--amend", "--no-push"},
			want: [][]string{
				{"jj", "squash", "--use-destination-message"},
				{"jj", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · not pushed`,
		},
		{
			name:   "jj amend with message",
			marker: ".jj",
			args:   []string{"--amend", "-m", "fix: frobnicate", "--no-push"},
			want: [][]string{
				{"jj", "squash", "-m", "fix: frobnicate"},
				{"jj", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · not pushed`,
		},
		{
			name:   "git amend no message",
			marker: ".git",
			args:   []string{"--amend", "--no-push"},
			want: [][]string{
				{"git", "add", "-A"},
				{"git", "commit", "--amend", "--no-edit"},
				{"git", "log", "-1", "--format=%h%x00%s"},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · not pushed`,
		},
		{
			name:   "git amend no verify",
			marker: ".git",
			args:   []string{"--amend", "--no-verify", "--no-push"},
			want: [][]string{
				{"git", "add", "-A"},
				{"git", "commit", "--amend", "--no-edit", "--no-verify"},
				{"git", "log", "-1", "--format=%h%x00%s"},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · not pushed`,
		},
		{
			name:   "jj scoped paths",
			marker: ".jj",
			args:   []string{"-m", "fix: frobnicate", "--no-push", "src/a.go", "docs"},
			want: [][]string{
				{"jj", "diff", "--name-only", "--", "src/a.go", "docs"},
				{"jj", "commit", "-m", "fix: frobnicate", "--", "src/a.go", "docs"},
				{"jj", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · not pushed`,
		},
		{
			name:   "git scoped paths",
			marker: ".git",
			args:   []string{"-m", "fix: frobnicate", "--no-push", "src/a.go", "docs"},
			want: [][]string{
				{"git", "add", "-A", "--", "src/a.go", "docs"},
				{"git", "commit", "-m", "fix: frobnicate", "--", "src/a.go", "docs"},
				{"git", "log", "-1", "--format=%h%x00%s"},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · not pushed`,
		},
		{
			name:   "jj amend scoped no message",
			marker: ".jj",
			args:   []string{"--amend", "--no-push", "src/a.go"},
			want: [][]string{
				{"jj", "squash", "--use-destination-message", "--", "src/a.go"},
				{"jj", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · not pushed`,
		},
		{
			name:   "jj amend scoped with message",
			marker: ".jj",
			args:   []string{"--amend", "-m", "fix: frobnicate", "--no-push", "src/a.go"},
			want: [][]string{
				{"jj", "squash", "-m", "fix: frobnicate", "--", "src/a.go"},
				{"jj", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · not pushed`,
		},
		{
			name:   "git amend scoped",
			marker: ".git",
			args:   []string{"--amend", "-m", "fix: frobnicate", "--no-push", "src/a.go"},
			want: [][]string{
				{"git", "add", "-A", "--", "src/a.go"},
				{"git", "commit", "--amend", "-m", "fix: frobnicate", "--", "src/a.go"},
				{"git", "log", "-1", "--format=%h%x00%s"},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · not pushed`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := setupShip(t, tt.marker, true)
			got, err := runShipCmd(t, tt.args...)
			if err != nil {
				t.Fatalf("ship error = %v", err)
			}
			if got != tt.summary {
				t.Errorf("summary = %q, want %q", got, tt.summary)
			}
			assertInvocations(t, readInvocations(t, log), tt.want)
		})
	}
}

func TestShipJJEmptyRefuses(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		env         map[string]string
		wantErr     string
		wantSummary string
		want        [][]string
	}{
		{
			name:    "unscoped",
			args:    []string{"-m", "fix: frobnicate", "--no-watch"},
			wantErr: `ship: nothing to commit — did a prior ship already land a1b2c3d "fix: frobnicate"? push it: jj bookmark move exact:main --to @- && jj git push --bookmark exact:main`,
			want: [][]string{
				{"jj", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
				{"jj", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "log", "-r", `bookmarks(exact:"main")`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "diff", "--name-only"},
				{"jj", "log", "-r", "@", "--no-graph", "-T", jjAtStateTemplate},
				{"jj", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
			},
		},
		{
			name:    "path scoped",
			args:    []string{"-m", "fix: frobnicate", "--no-watch", "src/a.go"},
			wantErr: `ship: nothing to commit in src/a.go — did a prior ship already land a1b2c3d "fix: frobnicate"? push it: jj bookmark move exact:main --to @- && jj git push --bookmark exact:main`,
			want: [][]string{
				{"jj", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
				{"jj", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "log", "-r", `bookmarks(exact:"main")`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "diff", "--name-only", "--", "src/a.go"},
				{"jj", "log", "-r", "@", "--no-graph", "-T", jjAtStateTemplate},
				{"jj", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
			},
		},
		{
			name:    "bookmark hint",
			args:    []string{"-m", "fix: frobnicate", "--no-watch", "--bookmark", "someone/probe"},
			wantErr: `ship: nothing to commit — did a prior ship already land a1b2c3d "fix: frobnicate"? push it: jj bookmark move exact:someone/probe --to @- && jj git push --bookmark exact:someone/probe`,
			want: [][]string{
				{"jj", "log", "-r", `bookmarks(exact:"someone/probe")`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "diff", "--name-only"},
				{"jj", "log", "-r", "@", "--no-graph", "-T", jjAtStateTemplate},
				{"jj", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
			},
		},
		{
			name:    "no push omits hint",
			args:    []string{"-m", "fix: frobnicate", "--no-push", "--bookmark", "someone/probe"},
			wantErr: `ship: nothing to commit — did a prior ship already land a1b2c3d "fix: frobnicate"?`,
			want: [][]string{
				{"jj", "diff", "--name-only"},
				{"jj", "log", "-r", "@", "--no-graph", "-T", jjAtStateTemplate},
				{"jj", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
			},
		},
		{
			name:    "description only working copy refuses",
			args:    []string{"-m", "description only", "--no-push"},
			wantErr: `ship: nothing to commit — did a prior ship already land a1b2c3d "fix: frobnicate"?`,
			want: [][]string{
				{"jj", "diff", "--name-only"},
				{"jj", "log", "-r", "@", "--no-graph", "-T", jjAtStateTemplate},
				{"jj", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
			},
		},
		{
			name:        "conflicted merge working copy commits",
			args:        []string{"-m", "fix: frobnicate", "--no-push"},
			env:         map[string]string{"JJ_AT_PARENTS": "2"},
			wantSummary: `committed a1b2c3d "fix: frobnicate" · not pushed`,
			want: [][]string{
				{"jj", "diff", "--name-only"},
				{"jj", "log", "-r", "@", "--no-graph", "-T", jjAtStateTemplate},
				{"jj", "commit", "-m", "fix: frobnicate"},
				{"jj", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
			},
		},
		{
			name:    "conflicted single-parent working copy refuses",
			args:    []string{"-m", "fix: frobnicate", "--no-push"},
			wantErr: `ship: nothing to commit — did a prior ship already land a1b2c3d "fix: frobnicate"?`,
			want: [][]string{
				{"jj", "diff", "--name-only"},
				{"jj", "log", "-r", "@", "--no-graph", "-T", jjAtStateTemplate},
				{"jj", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
			},
		},
		{
			name:    "empty root refuses",
			args:    []string{"-m", "fix: frobnicate", "--no-push"},
			env:     map[string]string{"JJ_DESCRIBE_OUTPUT": "000000000000\n"},
			wantErr: `ship: nothing to commit — did a prior ship already land 000000000000 ""?`,
			want: [][]string{
				{"jj", "diff", "--name-only"},
				{"jj", "log", "-r", "@", "--no-graph", "-T", jjAtStateTemplate},
				{"jj", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
			},
		},
		{
			name:    "description without separator errors",
			args:    []string{"-m", "fix: frobnicate", "--no-push"},
			env:     map[string]string{"JJ_DESCRIBE_OUTPUT": "000000000000"},
			wantErr: `ship: malformed commit description "000000000000"`,
			want: [][]string{
				{"jj", "diff", "--name-only"},
				{"jj", "log", "-r", "@", "--no-graph", "-T", jjAtStateTemplate},
				{"jj", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := setupShip(t, ".jj", false)
			t.Setenv("JJ_DIFF_NAMES", "")
			for key, value := range tt.env {
				t.Setenv(key, value)
			}

			got, err := runShipCmd(t, tt.args...)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatal("expected empty ship refusal, got nil")
				}
				if err.Error() != tt.wantErr {
					t.Errorf("error = %q, want %q", err, tt.wantErr)
				}
			} else {
				if err != nil {
					t.Fatalf("ship error = %v", err)
				}
				if got != tt.wantSummary {
					t.Errorf("summary = %q, want %q", got, tt.wantSummary)
				}
			}
			assertInvocations(t, readInvocations(t, log), tt.want)
		})
	}
}

func TestShipJJEmptyAmendExempt(t *testing.T) {
	log := setupShip(t, ".jj", false)
	t.Setenv("JJ_DIFF_NAMES", "")

	got, err := runShipCmd(t, "--amend", "--no-push")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := `committed a1b2c3d "fix: frobnicate" · not pushed`
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	assertInvocations(t, readInvocations(t, log), [][]string{
		{"jj", "squash", "--use-destination-message"},
		{"jj", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
	})
}

func TestShipGitDetachedHeadRefusesBeforeCommit(t *testing.T) {
	log := setupShip(t, ".git", false)
	t.Setenv("GIT_BRANCH", "")

	_, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch")
	if err == nil || err.Error() != "ship: detached HEAD; no branch to push" {
		t.Fatalf("ship error = %v, want detached HEAD refusal", err)
	}
	invocations := readInvocations(t, log)
	assertInvocations(t, invocations, [][]string{{"git", "branch", "--show-current"}})
	assertNoShipCommit(t, invocations)
}

func TestShipGitUsesPostCommitBranch(t *testing.T) {
	log := setupShip(t, ".git", false)
	t.Setenv("GIT_BRANCH", "main")
	t.Setenv("GIT_BRANCH_AFTER_COMMIT", "other")

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := `committed a1b2c3d "fix: frobnicate" · pushed other → origin`
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	assertInvocations(t, readInvocations(t, log), [][]string{
		{"git", "branch", "--show-current"},
		{"git", "add", "-A"},
		{"git", "commit", "-m", "fix: frobnicate"},
		{"git", "log", "-1", "--format=%h%x00%s"},
		{"git", "branch", "--show-current"},
		{"git", "config", "--get", "branch.other.remote"},
		{"git", "fetch", "origin"},
		{"git", "rev-parse", "--verify", "--quiet", "refs/remotes/origin/other"},
		{"git", "merge-base", "--is-ancestor", "refs/remotes/origin/other", "HEAD"},
		{"git", "push", "origin", "other"},
	})
}

func TestShipSessionTrailer(t *testing.T) {
	tests := []struct {
		name    string
		marker  string
		args    []string
		want    [][]string
		summary string
	}{
		{
			name:   "jj commit appends trailer",
			marker: ".jj",
			args:   []string{"-m", "fix: frobnicate", "--no-push"},
			want: [][]string{
				{"jj", "diff", "--name-only"},
				{"jj", "commit", "-m", "fix: frobnicate\n\nClaude-Session-Id: some-uuid"},
				{"jj", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · not pushed`,
		},
		{
			name:   "git commit appends trailer",
			marker: ".git",
			args:   []string{"-m", "fix: frobnicate", "--no-push"},
			want: [][]string{
				{"git", "add", "-A"},
				{"git", "commit", "-m", "fix: frobnicate\n\nClaude-Session-Id: some-uuid"},
				{"git", "log", "-1", "--format=%h%x00%s"},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · not pushed`,
		},
		{
			name:   "jj amend with message appends trailer",
			marker: ".jj",
			args:   []string{"--amend", "-m", "fix: frobnicate", "--no-push"},
			want: [][]string{
				{"jj", "squash", "-m", "fix: frobnicate\n\nClaude-Session-Id: some-uuid"},
				{"jj", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · not pushed`,
		},
		{
			name:   "git amend with message appends trailer",
			marker: ".git",
			args:   []string{"--amend", "-m", "fix: frobnicate", "--no-push"},
			want: [][]string{
				{"git", "add", "-A"},
				{"git", "commit", "--amend", "-m", "fix: frobnicate\n\nClaude-Session-Id: some-uuid"},
				{"git", "log", "-1", "--format=%h%x00%s"},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · not pushed`,
		},
		{
			name:   "jj amend without message carries no trailer",
			marker: ".jj",
			args:   []string{"--amend", "--no-push"},
			want: [][]string{
				{"jj", "squash", "--use-destination-message"},
				{"jj", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · not pushed`,
		},
		{
			name:   "git amend without message carries no trailer",
			marker: ".git",
			args:   []string{"--amend", "--no-push"},
			want: [][]string{
				{"git", "add", "-A"},
				{"git", "commit", "--amend", "--no-edit"},
				{"git", "log", "-1", "--format=%h%x00%s"},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · not pushed`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := setupShip(t, tt.marker, true)
			t.Setenv(envClaudeSessionKey, "some-uuid")
			got, err := runShipCmd(t, tt.args...)
			if err != nil {
				t.Fatalf("ship error = %v", err)
			}
			if got != tt.summary {
				t.Errorf("summary = %q, want %q", got, tt.summary)
			}
			assertInvocations(t, readInvocations(t, log), tt.want)
		})
	}
}

func TestShipGitAmendFastForwardPush(t *testing.T) {
	log := setupShip(t, ".git", true)
	got, err := runShipCmd(t, "--amend", "-m", "fix: frobnicate", "--no-watch")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := `committed a1b2c3d "fix: frobnicate" · pushed main → origin`
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	invocations := readInvocations(t, log)
	assertInvocations(t, invocations, [][]string{
		{"git", "branch", "--show-current"},
		{"git", "rev-parse", "HEAD"},
		{"git", "add", "-A"},
		{"git", "commit", "--amend", "-m", "fix: frobnicate"},
		{"git", "log", "-1", "--format=%h%x00%s"},
		{"git", "branch", "--show-current"},
		{"git", "config", "--get", "branch.main.remote"},
		{"git", "push", "origin", "main"},
	})
	// An amend of an unpushed commit fast-forwards: a plain push lands with no
	// force at all, and the lane must never fetch (a fetch would refresh the lease).
	for _, inv := range invocations {
		if len(inv) >= 2 && inv[0] == "git" && inv[1] == "fetch" {
			t.Errorf("fast-forward amend must not fetch, got %v", inv)
		}
		for _, arg := range inv {
			if strings.HasPrefix(arg, "--force-with-lease") {
				t.Errorf("fast-forward amend must not force-push, got %v", inv)
			}
		}
	}
}

// captureSlog redirects the default slog logger to a buffer for the test's
// duration so an assertion can read the warnings the ship lanes emit.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &buf
}

func TestShipGitRebase(t *testing.T) {
	tests := []struct {
		name     string
		env      map[string]string
		want     [][]string
		summary  string
		wantErr  []string
		wantWarn bool
	}{
		{
			name: "no divergence pushes clean",
			want: [][]string{
				{"git", "branch", "--show-current"},
				{"git", "add", "-A"},
				{"git", "commit", "-m", "fix: frobnicate"},
				{"git", "log", "-1", "--format=%h%x00%s"},
				{"git", "branch", "--show-current"},
				{"git", "config", "--get", "branch.main.remote"},
				{"git", "fetch", "origin"},
				{"git", "rev-parse", "--verify", "--quiet", "refs/remotes/origin/main"},
				{"git", "merge-base", "--is-ancestor", "refs/remotes/origin/main", "HEAD"},
				{"git", "push", "origin", "main"},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · pushed main → origin`,
		},
		{
			name: "diverged rebases then pushes",
			env:  map[string]string{"GIT_DIVERGED": "1"},
			want: [][]string{
				{"git", "branch", "--show-current"},
				{"git", "add", "-A"},
				{"git", "commit", "-m", "fix: frobnicate"},
				{"git", "log", "-1", "--format=%h%x00%s"},
				{"git", "branch", "--show-current"},
				{"git", "config", "--get", "branch.main.remote"},
				{"git", "fetch", "origin"},
				{"git", "rev-parse", "--verify", "--quiet", "refs/remotes/origin/main"},
				{"git", "merge-base", "--is-ancestor", "refs/remotes/origin/main", "HEAD"},
				{"git", "rev-list", "--count", "refs/remotes/origin/main..HEAD"},
				{"git", "rebase", "--autostash", "refs/remotes/origin/main"},
				{"git", "push", "origin", "main"},
				{"git", "log", "-1", "--format=%h%x00%s"},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · rebased 2 commit(s) onto main · pushed main → origin`,
		},
		{
			name: "rebase conflict aborts and reports",
			env:  map[string]string{"GIT_DIVERGED": "1", "GIT_REBASE_CONFLICT": "1"},
			want: [][]string{
				{"git", "branch", "--show-current"},
				{"git", "add", "-A"},
				{"git", "commit", "-m", "fix: frobnicate"},
				{"git", "log", "-1", "--format=%h%x00%s"},
				{"git", "branch", "--show-current"},
				{"git", "config", "--get", "branch.main.remote"},
				{"git", "fetch", "origin"},
				{"git", "rev-parse", "--verify", "--quiet", "refs/remotes/origin/main"},
				{"git", "merge-base", "--is-ancestor", "refs/remotes/origin/main", "HEAD"},
				{"git", "rev-list", "--count", "refs/remotes/origin/main..HEAD"},
				{"git", "rebase", "--autostash", "refs/remotes/origin/main"},
				{"git", "rev-parse", "--verify", "--quiet", "REBASE_HEAD"},
				{"git", "diff", "--name-only", "--diff-filter=U"},
				{"git", "rebase", "--abort"},
			},
			wantErr: []string{"rebase onto origin/main conflicts in: f.txt", "resolve manually"},
		},
		{
			name: "missing remote branch skips rebase",
			env:  map[string]string{"GIT_REMOTE_REF_MISSING": "1"},
			want: [][]string{
				{"git", "branch", "--show-current"},
				{"git", "add", "-A"},
				{"git", "commit", "-m", "fix: frobnicate"},
				{"git", "log", "-1", "--format=%h%x00%s"},
				{"git", "branch", "--show-current"},
				{"git", "config", "--get", "branch.main.remote"},
				{"git", "fetch", "origin"},
				{"git", "rev-parse", "--verify", "--quiet", "refs/remotes/origin/main"},
				{"git", "push", "origin", "main"},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · pushed main → origin`,
		},
		{
			name:     "autostash pop conflict warns",
			env:      map[string]string{"GIT_DIVERGED": "1", "GIT_AUTOSTASH_WARN": "1"},
			wantWarn: true,
			want: [][]string{
				{"git", "branch", "--show-current"},
				{"git", "add", "-A"},
				{"git", "commit", "-m", "fix: frobnicate"},
				{"git", "log", "-1", "--format=%h%x00%s"},
				{"git", "branch", "--show-current"},
				{"git", "config", "--get", "branch.main.remote"},
				{"git", "fetch", "origin"},
				{"git", "rev-parse", "--verify", "--quiet", "refs/remotes/origin/main"},
				{"git", "merge-base", "--is-ancestor", "refs/remotes/origin/main", "HEAD"},
				{"git", "rev-list", "--count", "refs/remotes/origin/main..HEAD"},
				{"git", "rebase", "--autostash", "refs/remotes/origin/main"},
				{"git", "push", "origin", "main"},
				{"git", "log", "-1", "--format=%h%x00%s"},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · rebased 2 commit(s) onto main · pushed main → origin`,
		},
		{
			name: "resolves the configured remote",
			env:  map[string]string{"GIT_BRANCH_REMOTE": "backup"},
			want: [][]string{
				{"git", "branch", "--show-current"},
				{"git", "add", "-A"},
				{"git", "commit", "-m", "fix: frobnicate"},
				{"git", "log", "-1", "--format=%h%x00%s"},
				{"git", "branch", "--show-current"},
				{"git", "config", "--get", "branch.main.remote"},
				{"git", "fetch", "backup"},
				{"git", "rev-parse", "--verify", "--quiet", "refs/remotes/backup/main"},
				{"git", "merge-base", "--is-ancestor", "refs/remotes/backup/main", "HEAD"},
				{"git", "push", "backup", "main"},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · pushed main → backup`,
		},
		{
			name: "rebase failing before it starts is not a conflict",
			env:  map[string]string{"GIT_DIVERGED": "1", "GIT_REBASE_NO_START": "1"},
			want: [][]string{
				{"git", "branch", "--show-current"},
				{"git", "add", "-A"},
				{"git", "commit", "-m", "fix: frobnicate"},
				{"git", "log", "-1", "--format=%h%x00%s"},
				{"git", "branch", "--show-current"},
				{"git", "config", "--get", "branch.main.remote"},
				{"git", "fetch", "origin"},
				{"git", "rev-parse", "--verify", "--quiet", "refs/remotes/origin/main"},
				{"git", "merge-base", "--is-ancestor", "refs/remotes/origin/main", "HEAD"},
				{"git", "rev-list", "--count", "refs/remotes/origin/main..HEAD"},
				{"git", "rebase", "--autostash", "refs/remotes/origin/main"},
				{"git", "rev-parse", "--verify", "--quiet", "REBASE_HEAD"},
			},
			wantErr: []string{"git rebase onto origin/main", "uncommitted changes"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := setupShip(t, ".git", false)
			for key, value := range tt.env {
				t.Setenv(key, value)
			}
			buf := captureSlog(t)

			got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch")
			if len(tt.wantErr) == 0 {
				if err != nil {
					t.Fatalf("ship error = %v", err)
				}
				if got != tt.summary {
					t.Errorf("summary = %q, want %q", got, tt.summary)
				}
			} else {
				if err == nil {
					t.Fatal("expected ship error, got nil")
				}
				for _, want := range tt.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error = %q, want it to contain %q", err, want)
					}
				}
			}
			if warned := strings.Contains(buf.String(), "git stash pop"); warned != tt.wantWarn {
				t.Errorf("autostash warning = %v, want %v (log: %q)", warned, tt.wantWarn, buf.String())
			}
			assertInvocations(t, readInvocations(t, log), tt.want)
		})
	}
}

func TestShipGitPushRetry(t *testing.T) {
	tests := []struct {
		name           string
		args           []string
		env            map[string]string
		pushReject     int
		divergedSwitch bool
		want           [][]string
		summary        string
		wantErr        []string
	}{
		{
			name:           "rejected push refetches and lands",
			pushReject:     1,
			divergedSwitch: true,
			want: [][]string{
				{"git", "branch", "--show-current"},
				{"git", "add", "-A"},
				{"git", "commit", "-m", "fix: frobnicate"},
				{"git", "log", "-1", "--format=%h%x00%s"},
				{"git", "branch", "--show-current"},
				{"git", "config", "--get", "branch.main.remote"},
				{"git", "fetch", "origin"},
				{"git", "rev-parse", "--verify", "--quiet", "refs/remotes/origin/main"},
				{"git", "merge-base", "--is-ancestor", "refs/remotes/origin/main", "HEAD"},
				{"git", "push", "origin", "main"},
				{"git", "fetch", "origin"},
				{"git", "rev-parse", "--verify", "--quiet", "refs/remotes/origin/main"},
				{"git", "merge-base", "--is-ancestor", "refs/remotes/origin/main", "HEAD"},
				{"git", "rev-list", "--count", "refs/remotes/origin/main..HEAD"},
				{"git", "rebase", "--autostash", "refs/remotes/origin/main"},
				{"git", "push", "origin", "main"},
				{"git", "log", "-1", "--format=%h%x00%s"},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · rebased 2 commit(s) onto main · pushed main → origin`,
		},
		{
			name:       "retries exhausted names the remedy",
			pushReject: 3,
			want: [][]string{
				{"git", "branch", "--show-current"},
				{"git", "add", "-A"},
				{"git", "commit", "-m", "fix: frobnicate"},
				{"git", "log", "-1", "--format=%h%x00%s"},
				{"git", "branch", "--show-current"},
				{"git", "config", "--get", "branch.main.remote"},
				{"git", "fetch", "origin"},
				{"git", "rev-parse", "--verify", "--quiet", "refs/remotes/origin/main"},
				{"git", "merge-base", "--is-ancestor", "refs/remotes/origin/main", "HEAD"},
				{"git", "push", "origin", "main"},
				{"git", "fetch", "origin"},
				{"git", "rev-parse", "--verify", "--quiet", "refs/remotes/origin/main"},
				{"git", "merge-base", "--is-ancestor", "refs/remotes/origin/main", "HEAD"},
				{"git", "push", "origin", "main"},
				{"git", "fetch", "origin"},
				{"git", "rev-parse", "--verify", "--quiet", "refs/remotes/origin/main"},
				{"git", "merge-base", "--is-ancestor", "refs/remotes/origin/main", "HEAD"},
				{"git", "push", "origin", "main"},
			},
			wantErr: []string{"rejected 3 times", "git fetch origin && git rebase --autostash origin/main && git push", "non-fast-forward"},
		},
		{
			name:           "conflict during retry rebase is terminal",
			pushReject:     1,
			divergedSwitch: true,
			env:            map[string]string{"GIT_REBASE_CONFLICT": "1"},
			want: [][]string{
				{"git", "branch", "--show-current"},
				{"git", "add", "-A"},
				{"git", "commit", "-m", "fix: frobnicate"},
				{"git", "log", "-1", "--format=%h%x00%s"},
				{"git", "branch", "--show-current"},
				{"git", "config", "--get", "branch.main.remote"},
				{"git", "fetch", "origin"},
				{"git", "rev-parse", "--verify", "--quiet", "refs/remotes/origin/main"},
				{"git", "merge-base", "--is-ancestor", "refs/remotes/origin/main", "HEAD"},
				{"git", "push", "origin", "main"},
				{"git", "fetch", "origin"},
				{"git", "rev-parse", "--verify", "--quiet", "refs/remotes/origin/main"},
				{"git", "merge-base", "--is-ancestor", "refs/remotes/origin/main", "HEAD"},
				{"git", "rev-list", "--count", "refs/remotes/origin/main..HEAD"},
				{"git", "rebase", "--autostash", "refs/remotes/origin/main"},
				{"git", "rev-parse", "--verify", "--quiet", "REBASE_HEAD"},
				{"git", "diff", "--name-only", "--diff-filter=U"},
				{"git", "rebase", "--abort"},
			},
			wantErr: []string{"rebase onto origin/main conflicts", "f.txt"},
		},
		{
			name: "amend stale lease never fetches or retries",
			args: []string{"--amend"},
			env:  map[string]string{"GIT_AMEND_PLAIN_NONFF": "1", "GIT_LEASE_STALE": "1"},
			want: [][]string{
				{"git", "branch", "--show-current"},
				{"git", "rev-parse", "HEAD"},
				{"git", "add", "-A"},
				{"git", "commit", "--amend", "-m", "fix: frobnicate"},
				{"git", "log", "-1", "--format=%h%x00%s"},
				{"git", "branch", "--show-current"},
				{"git", "config", "--get", "branch.main.remote"},
				{"git", "push", "origin", "main"},
				{"git", "push", "origin", "--force-with-lease=main:" + fakeHeadSHA, "main"},
			},
			wantErr: []string{"built on the commit you amended"},
		},
		{
			name: "hook decline does not retry",
			env:  map[string]string{"GIT_PUSH_FAIL_STDERR": "! [remote rejected] main -> main (pre-receive hook declined)"},
			want: [][]string{
				{"git", "branch", "--show-current"},
				{"git", "add", "-A"},
				{"git", "commit", "-m", "fix: frobnicate"},
				{"git", "log", "-1", "--format=%h%x00%s"},
				{"git", "branch", "--show-current"},
				{"git", "config", "--get", "branch.main.remote"},
				{"git", "fetch", "origin"},
				{"git", "rev-parse", "--verify", "--quiet", "refs/remotes/origin/main"},
				{"git", "merge-base", "--is-ancestor", "refs/remotes/origin/main", "HEAD"},
				{"git", "push", "origin", "main"},
			},
			wantErr: []string{"ship: git push:", "pre-receive hook declined"},
		},
		{
			name:       "both attempts rebase reports the count once",
			pushReject: 1,
			env:        map[string]string{"GIT_DIVERGED": "1"},
			want: [][]string{
				{"git", "branch", "--show-current"},
				{"git", "add", "-A"},
				{"git", "commit", "-m", "fix: frobnicate"},
				{"git", "log", "-1", "--format=%h%x00%s"},
				{"git", "branch", "--show-current"},
				{"git", "config", "--get", "branch.main.remote"},
				{"git", "fetch", "origin"},
				{"git", "rev-parse", "--verify", "--quiet", "refs/remotes/origin/main"},
				{"git", "merge-base", "--is-ancestor", "refs/remotes/origin/main", "HEAD"},
				{"git", "rev-list", "--count", "refs/remotes/origin/main..HEAD"},
				{"git", "rebase", "--autostash", "refs/remotes/origin/main"},
				{"git", "push", "origin", "main"},
				{"git", "fetch", "origin"},
				{"git", "rev-parse", "--verify", "--quiet", "refs/remotes/origin/main"},
				{"git", "merge-base", "--is-ancestor", "refs/remotes/origin/main", "HEAD"},
				{"git", "rev-list", "--count", "refs/remotes/origin/main..HEAD"},
				{"git", "rebase", "--autostash", "refs/remotes/origin/main"},
				{"git", "push", "origin", "main"},
				{"git", "log", "-1", "--format=%h%x00%s"},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · rebased 2 commit(s) onto main · pushed main → origin`,
		},
		{
			name: "remote-rejected veto beats a mixed non-fast-forward token",
			env:  map[string]string{"GIT_PUSH_FAIL_STDERR": "! [remote rejected] main -> main (pre-receive hook declined)\n! [rejected] feature -> feature (non-fast-forward)"},
			want: [][]string{
				{"git", "branch", "--show-current"},
				{"git", "add", "-A"},
				{"git", "commit", "-m", "fix: frobnicate"},
				{"git", "log", "-1", "--format=%h%x00%s"},
				{"git", "branch", "--show-current"},
				{"git", "config", "--get", "branch.main.remote"},
				{"git", "fetch", "origin"},
				{"git", "rev-parse", "--verify", "--quiet", "refs/remotes/origin/main"},
				{"git", "merge-base", "--is-ancestor", "refs/remotes/origin/main", "HEAD"},
				{"git", "push", "origin", "main"},
			},
			wantErr: []string{"ship: git push:", "pre-receive hook declined"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := setupShip(t, ".git", false)
			for key, value := range tt.env {
				t.Setenv(key, value)
			}
			if tt.pushReject > 0 {
				marker := filepath.Join(t.TempDir(), "gitpush")
				if err := os.WriteFile(marker, []byte(fmt.Sprintf("%d", tt.pushReject)), 0o600); err != nil {
					t.Fatalf("write push marker: %v", err)
				}
				t.Setenv("GIT_PUSH_REJECT_MARKER", marker)
			}
			if tt.divergedSwitch {
				t.Setenv("GIT_DIVERGED_MARKER", filepath.Join(t.TempDir(), "gitdiverged"))
			}

			args := append([]string{"-m", "fix: frobnicate", "--no-watch"}, tt.args...)
			got, err := runShipCmd(t, args...)
			if len(tt.wantErr) == 0 {
				if err != nil {
					t.Fatalf("ship error = %v", err)
				}
				if got != tt.summary {
					t.Errorf("summary = %q, want %q", got, tt.summary)
				}
			} else {
				if err == nil {
					t.Fatal("expected ship error, got nil")
				}
				for _, want := range tt.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error = %q, want it to contain %q", err, want)
					}
				}
			}
			assertInvocations(t, readInvocations(t, log), tt.want)
		})
	}
}

func TestShipNoWatchSkipsCI(t *testing.T) {
	log := setupShip(t, ".jj", true)
	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := `committed a1b2c3d "fix: frobnicate" · pushed main → origin`
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	assertInvocations(t, readInvocations(t, log), [][]string{
		{"jj", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
		{"jj", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
		{"jj", "log", "-r", `bookmarks(exact:"main")`, "--no-graph", "-T", jjStackLineTemplate},
		{"jj", "diff", "--name-only"},
		{"jj", "commit", "-m", "fix: frobnicate"},
		{"jj", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
		{"jj", "bookmark", "list", jjExactPattern("main"), "--all-remotes", "-T", jjRemoteBookmarkTemplate},
		{"jj", "git", "fetch"},
		{"jj", "log", "-r", `bookmarks(exact:"main")`, "--no-graph", "-T", jjStackLineTemplate},
		{"jj", "log", "-r", fmt.Sprintf(jjAncestorRevsetFmt, "main"), "--no-graph", "-T", jjBookmarkTemplate},
		{"jj", "bookmark", "move", "exact:main", "--to", "@-"},
		{"jj", "op", "log", "-n", "1", "--no-graph", "-T", jjOpIDTemplate},
		{"jj", "git", "push", "--bookmark", "exact:main"},
	})
}

func TestShipCIStates(t *testing.T) {
	tests := []struct {
		name      string
		withGh    bool
		runList   string
		viewJSON  string
		watchExit string
		summary   string
		wantErr   bool
		wantWatch bool
	}{
		{
			name:      "gh missing",
			withGh:    false,
			summary:   `committed a1b2c3d "fix: frobnicate" · pushed main → origin · CI gh-missing`,
			wantWatch: false,
		},
		{
			name:      "no run",
			withGh:    true,
			runList:   "[]",
			summary:   `committed a1b2c3d "fix: frobnicate" · pushed main → origin · CI no-run`,
			wantWatch: false,
		},
		{
			name:      "failure",
			withGh:    true,
			runList:   fakeRunListJSON,
			viewJSON:  fakeRunViewFailure,
			watchExit: "1",
			summary:   `committed a1b2c3d "fix: frobnicate" · pushed main → origin · CI failure`,
			wantErr:   true,
			wantWatch: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := setupShip(t, ".jj", tt.withGh)
			if tt.runList != "" {
				t.Setenv("GH_RUN_LIST_JSON", tt.runList)
			}
			if tt.viewJSON != "" {
				t.Setenv("GH_RUN_VIEW_JSON", tt.viewJSON)
			}
			if tt.watchExit != "" {
				t.Setenv("GH_WATCH_EXIT", tt.watchExit)
			}
			shipCIPollInterval = 0

			got, err := runShipCmd(t, "-m", "fix: frobnicate")
			if tt.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error = %v", err)
			}
			if got != tt.summary {
				t.Errorf("summary = %q, want %q", got, tt.summary)
			}
			watched := false
			for _, inv := range readInvocations(t, log) {
				if len(inv) >= 3 && inv[0] == "gh" && inv[1] == "run" && inv[2] == "watch" {
					watched = true
				}
			}
			if watched != tt.wantWatch {
				t.Errorf("gh run watch invoked = %v, want %v", watched, tt.wantWatch)
			}
		})
	}
}

func TestShipCINoRunWithWorkflowIsUnconfirmed(t *testing.T) {
	setupShip(t, ".jj", true)
	workflowDir := filepath.Join(".github", "workflows")
	if err := os.MkdirAll(workflowDir, 0o750); err != nil {
		t.Fatalf("mkdir workflows: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workflowDir, "ci.yml"), []byte("name: ci\n"), 0o600); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	t.Setenv("GH_RUN_LIST_JSON", "[]")
	shipCIPollInterval = 0

	got, err := runShipCmd(t, "-m", "fix: frobnicate")
	if err == nil {
		t.Fatal("expected error when workflows exist but no run was registered")
	}
	if want := "· CI unconfirmed"; !strings.Contains(got, want) {
		t.Errorf("summary = %q, want it to contain %q", got, want)
	}
	for _, want := range []string{"no CI run was registered", "paths-filtered", "dispatch-only", "on: workflow_dispatch", "gh run list --commit " + fakeHeadSHA} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err, want)
		}
	}
}

func TestShipHeadSHAFailurePrintsCommitPushSummary(t *testing.T) {
	log := setupShip(t, ".jj", true)
	t.Setenv("JJ_COMMIT_ID_FAIL", "1")

	out, _, err := runShipCmdFull(t, "-m", "fix: frobnicate")
	if err == nil {
		t.Fatal("expected head SHA error, got nil")
	}
	want := "committed a1b2c3d \"fix: frobnicate\" · pushed main → origin\n"
	if out != want {
		t.Errorf("output = %q, want %q", out, want)
	}
	if strings.Contains(out, "CI ") {
		t.Errorf("head SHA failure must not print a CI segment, got %q", out)
	}
	if !strings.Contains(err.Error(), "jj log commit_id") {
		t.Errorf("error = %v, want jj log commit_id failure", err)
	}
	for _, inv := range readInvocations(t, log) {
		if inv[0] == "gh" {
			t.Errorf("head SHA failure must stop before gh, got invocation %v", inv)
		}
	}
}

func TestJJExactPattern(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "main", `exact:"main"`},
		{"slash", "someone/probe", `exact:"someone/probe"`},
		{"at sign", "foo@bar", `exact:"foo@bar"`},
		{"double quote", `has"quote`, `exact:"has\"quote"`},
		{"backslash", `back\slash`, `exact:"back\\slash"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := jjExactPattern(tt.in); got != tt.want {
				t.Errorf("jjExactPattern(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestShipJJRebase(t *testing.T) {
	tests := []struct {
		name           string
		args           []string
		env            map[string]string
		describeMarker bool
		pushReject     int
		divergedSwitch bool
		want           [][]string
		summary        string
		wantErr        []string
	}{
		{
			name: "untracked trunk auto-tracks before fetch then pushes",
			env:  map[string]string{"JJ_UNTRACKED_REMOTES": "origin"},
			want: [][]string{
				{"jj", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
				{"jj", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "log", "-r", `bookmarks(exact:"main")`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "diff", "--name-only"},
				{"jj", "commit", "-m", "fix: frobnicate"},
				{"jj", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
				{"jj", "bookmark", "list", jjExactPattern("main"), "--all-remotes", "-T", jjRemoteBookmarkTemplate},
				{"jj", "bookmark", "track", jjExactPattern("main"), "--remote=origin"},
				{"jj", "git", "fetch"},
				{"jj", "log", "-r", `bookmarks(exact:"main")`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "log", "-r", fmt.Sprintf(jjAncestorRevsetFmt, "main"), "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "bookmark", "move", "exact:main", "--to", "@-"},
				{"jj", "op", "log", "-n", "1", "--no-graph", "-T", jjOpIDTemplate},
				{"jj", "git", "push", "--bookmark", "exact:main"},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · pushed main → origin`,
		},
		{
			// The trunk's counterpart is untracked on a non-origin remote (main@backup)
			// while main@origin is tracked: ship must track the remote the untracked
			// counterpart actually sits on, not a hard-coded origin.
			name: "untracked counterpart on a non-origin remote tracks that remote",
			env:  map[string]string{"JJ_UNTRACKED_REMOTES": "backup"},
			want: [][]string{
				{"jj", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
				{"jj", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "log", "-r", `bookmarks(exact:"main")`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "diff", "--name-only"},
				{"jj", "commit", "-m", "fix: frobnicate"},
				{"jj", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
				{"jj", "bookmark", "list", jjExactPattern("main"), "--all-remotes", "-T", jjRemoteBookmarkTemplate},
				{"jj", "bookmark", "track", jjExactPattern("main"), "--remote=backup"},
				{"jj", "git", "fetch"},
				{"jj", "log", "-r", `bookmarks(exact:"main")`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "log", "-r", fmt.Sprintf(jjAncestorRevsetFmt, "main"), "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "bookmark", "move", "exact:main", "--to", "@-"},
				{"jj", "op", "log", "-n", "1", "--no-graph", "-T", jjOpIDTemplate},
				{"jj", "git", "push", "--bookmark", "exact:main"},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · pushed main → origin`,
		},
		{
			// Two remotes carry an untracked counterpart, so ship breaks the tie on the
			// remote jj git push targets — the git.push config setting.
			name: "multiple untracked counterparts track the push target",
			env:  map[string]string{"JJ_UNTRACKED_REMOTES": "backup origin", "JJ_PUSH_REMOTE": "backup"},
			want: [][]string{
				{"jj", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
				{"jj", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "log", "-r", `bookmarks(exact:"main")`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "diff", "--name-only"},
				{"jj", "commit", "-m", "fix: frobnicate"},
				{"jj", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
				{"jj", "bookmark", "list", jjExactPattern("main"), "--all-remotes", "-T", jjRemoteBookmarkTemplate},
				{"jj", "config", "get", "git.push"},
				{"jj", "bookmark", "track", jjExactPattern("main"), "--remote=backup"},
				{"jj", "git", "fetch"},
				{"jj", "log", "-r", `bookmarks(exact:"main")`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "log", "-r", fmt.Sprintf(jjAncestorRevsetFmt, "main"), "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "bookmark", "move", "exact:main", "--to", "@-"},
				{"jj", "op", "log", "-n", "1", "--no-graph", "-T", jjOpIDTemplate},
				{"jj", "git", "push", "--bookmark", "exact:main"},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · pushed main → origin`,
		},
		{
			name:           "diverged trunk rebases",
			env:            map[string]string{"JJ_NO_BOOKMARK": "1", "JJ_DIVERGED": "1"},
			describeMarker: true,
			want: [][]string{
				{"jj", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
				{"jj", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "log", "-r", `bookmarks(exact:"main")`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "diff", "--name-only"},
				{"jj", "commit", "-m", "fix: frobnicate"},
				{"jj", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
				{"jj", "bookmark", "list", jjExactPattern("main"), "--all-remotes", "-T", jjRemoteBookmarkTemplate},
				{"jj", "git", "fetch"},
				{"jj", "log", "-r", `bookmarks(exact:"main")`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "log", "-r", fmt.Sprintf(jjAncestorRevsetFmt, "main"), "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "log", "-r", fmt.Sprintf(jjStackRevsetFmt, "main"), "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "rebase", "-b", "@-", "--destination", `bookmarks(exact:"main")`},
				{"jj", "op", "log", "-n", "1", "--no-graph", "-T", jjOpIDTemplate},
				{"jj", "log", "-r", `conflicts() & (bookmarks(exact:"main")..@-)::`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "bookmark", "move", "exact:main", "--to", "@-"},
				{"jj", "op", "log", "-n", "1", "--no-graph", "-T", jjOpIDTemplate},
				{"jj", "git", "push", "--bookmark", "exact:main"},
				{"jj", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
			},
			summary: `committed e9f8a7b "fix: frobnicate" · rebased 2 commit(s) onto main · pushed main → origin`,
		},
		{
			name:           "diverged --bookmark rebases",
			args:           []string{"--bookmark", "someone/probe"},
			env:            map[string]string{"JJ_DIVERGED": "1"},
			describeMarker: true,
			want: [][]string{
				{"jj", "log", "-r", `bookmarks(exact:"someone/probe")`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "diff", "--name-only"},
				{"jj", "commit", "-m", "fix: frobnicate"},
				{"jj", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
				{"jj", "bookmark", "list", jjExactPattern("someone/probe"), "--all-remotes", "-T", jjRemoteBookmarkTemplate},
				{"jj", "git", "fetch"},
				{"jj", "log", "-r", `bookmarks(exact:"someone/probe")`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "log", "-r", fmt.Sprintf(jjAncestorRevsetFmt, "someone/probe"), "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "log", "-r", fmt.Sprintf(jjStackRevsetFmt, "someone/probe"), "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "rebase", "-b", "@-", "--destination", `bookmarks(exact:"someone/probe")`},
				{"jj", "op", "log", "-n", "1", "--no-graph", "-T", jjOpIDTemplate},
				{"jj", "log", "-r", `conflicts() & (bookmarks(exact:"someone/probe")..@-)::`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "bookmark", "move", "exact:someone/probe", "--to", "@-"},
				{"jj", "op", "log", "-n", "1", "--no-graph", "-T", jjOpIDTemplate},
				{"jj", "git", "push", "--bookmark", "exact:someone/probe"},
				{"jj", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
			},
			summary: `committed e9f8a7b "fix: frobnicate" · rebased 2 commit(s) onto someone/probe · pushed someone/probe → origin`,
		},
		{
			name: "conflicted target refuses",
			env: map[string]string{
				"JJ_NO_BOOKMARK":    "1",
				"JJ_BOOKMARK_HEADS": "2",
			},
			want: [][]string{
				{"jj", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
				{"jj", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "log", "-r", `bookmarks(exact:"main")`, "--no-graph", "-T", jjStackLineTemplate},
			},
			wantErr: []string{`bookmark "main" is conflicted (2 heads)`, "resolve it"},
		},
		{
			name: "conflicted rebase rolls back",
			env: map[string]string{
				"JJ_NO_BOOKMARK": "1",
				"JJ_DIVERGED":    "1",
				"JJ_CONFLICTS":   "c0ffee1 fix: frobnicate\n",
			},
			want: [][]string{
				{"jj", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
				{"jj", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "log", "-r", `bookmarks(exact:"main")`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "diff", "--name-only"},
				{"jj", "commit", "-m", "fix: frobnicate"},
				{"jj", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
				{"jj", "bookmark", "list", jjExactPattern("main"), "--all-remotes", "-T", jjRemoteBookmarkTemplate},
				{"jj", "git", "fetch"},
				{"jj", "log", "-r", `bookmarks(exact:"main")`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "log", "-r", fmt.Sprintf(jjAncestorRevsetFmt, "main"), "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "log", "-r", fmt.Sprintf(jjStackRevsetFmt, "main"), "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "rebase", "-b", "@-", "--destination", `bookmarks(exact:"main")`},
				{"jj", "op", "log", "-n", "1", "--no-graph", "-T", jjOpIDTemplate},
				{"jj", "log", "-r", `conflicts() & (bookmarks(exact:"main")..@-)::`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "op", "revert", "op123abc"},
			},
			wantErr: []string{`rebase onto "main" conflicts in 1 commit`, "c0ffee1", "rolled back"},
		},
		{
			name: "conflict check failure rolls back",
			env: map[string]string{
				"JJ_NO_BOOKMARK":         "1",
				"JJ_DIVERGED":            "1",
				"JJ_CONFLICT_CHECK_FAIL": "1",
			},
			want: [][]string{
				{"jj", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
				{"jj", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "log", "-r", `bookmarks(exact:"main")`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "diff", "--name-only"},
				{"jj", "commit", "-m", "fix: frobnicate"},
				{"jj", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
				{"jj", "bookmark", "list", jjExactPattern("main"), "--all-remotes", "-T", jjRemoteBookmarkTemplate},
				{"jj", "git", "fetch"},
				{"jj", "log", "-r", `bookmarks(exact:"main")`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "log", "-r", fmt.Sprintf(jjAncestorRevsetFmt, "main"), "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "log", "-r", fmt.Sprintf(jjStackRevsetFmt, "main"), "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "rebase", "-b", "@-", "--destination", `bookmarks(exact:"main")`},
				{"jj", "op", "log", "-n", "1", "--no-graph", "-T", jjOpIDTemplate},
				{"jj", "log", "-r", `conflicts() & (bookmarks(exact:"main")..@-)::`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "op", "revert", "op123abc"},
			},
			wantErr: []string{`conflict check after rebase onto "main" failed (rebase rolled back)`},
		},
		{
			name: "already landed refuses",
			env: map[string]string{
				"JJ_NO_BOOKMARK": "1",
				"JJ_DIVERGED":    "1",
				"JJ_STACK_EMPTY": "1",
			},
			want: [][]string{
				{"jj", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
				{"jj", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "log", "-r", `bookmarks(exact:"main")`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "diff", "--name-only"},
				{"jj", "commit", "-m", "fix: frobnicate"},
				{"jj", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
				{"jj", "bookmark", "list", jjExactPattern("main"), "--all-remotes", "-T", jjRemoteBookmarkTemplate},
				{"jj", "git", "fetch"},
				{"jj", "log", "-r", `bookmarks(exact:"main")`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "log", "-r", fmt.Sprintf(jjAncestorRevsetFmt, "main"), "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "log", "-r", fmt.Sprintf(jjStackRevsetFmt, "main"), "--no-graph", "-T", jjStackLineTemplate},
			},
			wantErr: []string{"already landed", "refusing to move the bookmark backwards"},
		},
		{
			name: "fetch failure is fatal",
			env:  map[string]string{"JJ_FETCH_FAIL": "1"},
			want: [][]string{
				{"jj", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
				{"jj", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "log", "-r", `bookmarks(exact:"main")`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "diff", "--name-only"},
				{"jj", "commit", "-m", "fix: frobnicate"},
				{"jj", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
				{"jj", "bookmark", "list", jjExactPattern("main"), "--all-remotes", "-T", jjRemoteBookmarkTemplate},
				{"jj", "git", "fetch"},
			},
			wantErr: []string{"jj git fetch"},
		},
		{
			name:           "rejected push restores and lands",
			env:            map[string]string{"JJ_NO_BOOKMARK": "1"},
			describeMarker: true,
			pushReject:     1,
			divergedSwitch: true,
			want: [][]string{
				{"jj", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
				{"jj", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "log", "-r", `bookmarks(exact:"main")`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "diff", "--name-only"},
				{"jj", "commit", "-m", "fix: frobnicate"},
				{"jj", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
				{"jj", "bookmark", "list", jjExactPattern("main"), "--all-remotes", "-T", jjRemoteBookmarkTemplate},
				{"jj", "git", "fetch"},
				{"jj", "log", "-r", `bookmarks(exact:"main")`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "log", "-r", fmt.Sprintf(jjAncestorRevsetFmt, "main"), "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "bookmark", "move", "exact:main", "--to", "@-"},
				{"jj", "op", "log", "-n", "1", "--no-graph", "-T", jjOpIDTemplate},
				{"jj", "git", "push", "--bookmark", "exact:main"},
				{"jj", "op", "revert", "op123abc"},
				{"jj", "git", "fetch"},
				{"jj", "log", "-r", `bookmarks(exact:"main")`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "log", "-r", fmt.Sprintf(jjAncestorRevsetFmt, "main"), "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "log", "-r", fmt.Sprintf(jjStackRevsetFmt, "main"), "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "rebase", "-b", "@-", "--destination", `bookmarks(exact:"main")`},
				{"jj", "op", "log", "-n", "1", "--no-graph", "-T", jjOpIDTemplate},
				{"jj", "log", "-r", `conflicts() & (bookmarks(exact:"main")..@-)::`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "bookmark", "move", "exact:main", "--to", "@-"},
				{"jj", "op", "log", "-n", "1", "--no-graph", "-T", jjOpIDTemplate},
				{"jj", "git", "push", "--bookmark", "exact:main"},
				{"jj", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
			},
			summary: `committed e9f8a7b "fix: frobnicate" · rebased 2 commit(s) onto main · pushed main → origin`,
		},
		{
			name:       "retries exhausted restores last state",
			env:        map[string]string{"JJ_NO_BOOKMARK": "1"},
			pushReject: 3,
			want: [][]string{
				{"jj", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
				{"jj", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "log", "-r", `bookmarks(exact:"main")`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "diff", "--name-only"},
				{"jj", "commit", "-m", "fix: frobnicate"},
				{"jj", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
				{"jj", "bookmark", "list", jjExactPattern("main"), "--all-remotes", "-T", jjRemoteBookmarkTemplate},
				{"jj", "git", "fetch"},
				{"jj", "log", "-r", `bookmarks(exact:"main")`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "log", "-r", fmt.Sprintf(jjAncestorRevsetFmt, "main"), "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "bookmark", "move", "exact:main", "--to", "@-"},
				{"jj", "op", "log", "-n", "1", "--no-graph", "-T", jjOpIDTemplate},
				{"jj", "git", "push", "--bookmark", "exact:main"},
				{"jj", "op", "revert", "op123abc"},
				{"jj", "git", "fetch"},
				{"jj", "log", "-r", `bookmarks(exact:"main")`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "log", "-r", fmt.Sprintf(jjAncestorRevsetFmt, "main"), "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "bookmark", "move", "exact:main", "--to", "@-"},
				{"jj", "op", "log", "-n", "1", "--no-graph", "-T", jjOpIDTemplate},
				{"jj", "git", "push", "--bookmark", "exact:main"},
				{"jj", "op", "revert", "op123abc"},
				{"jj", "git", "fetch"},
				{"jj", "log", "-r", `bookmarks(exact:"main")`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "log", "-r", fmt.Sprintf(jjAncestorRevsetFmt, "main"), "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "bookmark", "move", "exact:main", "--to", "@-"},
				{"jj", "op", "log", "-n", "1", "--no-graph", "-T", jjOpIDTemplate},
				{"jj", "git", "push", "--bookmark", "exact:main"},
				{"jj", "op", "revert", "op123abc"},
			},
			wantErr: []string{"rejected 3 times", "jj git fetch && jj rebase -b @-", "unexpectedly moved"},
		},
		{
			name:       "amend rejection refuses",
			args:       []string{"--amend"},
			env:        map[string]string{"JJ_NO_BOOKMARK": "1"},
			pushReject: 1,
			want: [][]string{
				{"jj", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
				{"jj", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "log", "-r", `bookmarks(exact:"main")`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "squash", "-m", "fix: frobnicate"},
				{"jj", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
				{"jj", "bookmark", "list", jjExactPattern("main"), "--all-remotes", "-T", jjRemoteBookmarkTemplate},
				{"jj", "git", "fetch"},
				{"jj", "log", "-r", `bookmarks(exact:"main")`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "log", "-r", fmt.Sprintf(jjAncestorRevsetFmt, "main"), "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "bookmark", "move", "exact:main", "--to", "@-"},
				{"jj", "op", "log", "-n", "1", "--no-graph", "-T", jjOpIDTemplate},
				{"jj", "git", "push", "--bookmark", "exact:main"},
				{"jj", "op", "revert", "op123abc"},
			},
			wantErr: []string{"not force-retrying over their work"},
		},
		{
			name:       "permission failure passes through",
			env:        map[string]string{"JJ_NO_BOOKMARK": "1", "JJ_PUSH_FAIL_STDERR": "The remote rejected the following updates:\nError: Failed to push some bookmarks"},
			pushReject: 1,
			want: [][]string{
				{"jj", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
				{"jj", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "log", "-r", `bookmarks(exact:"main")`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "diff", "--name-only"},
				{"jj", "commit", "-m", "fix: frobnicate"},
				{"jj", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
				{"jj", "bookmark", "list", jjExactPattern("main"), "--all-remotes", "-T", jjRemoteBookmarkTemplate},
				{"jj", "git", "fetch"},
				{"jj", "log", "-r", `bookmarks(exact:"main")`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "log", "-r", fmt.Sprintf(jjAncestorRevsetFmt, "main"), "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "bookmark", "move", "exact:main", "--to", "@-"},
				{"jj", "op", "log", "-n", "1", "--no-graph", "-T", jjOpIDTemplate},
				{"jj", "git", "push", "--bookmark", "exact:main"},
			},
			wantErr: []string{"ship: jj git push:", "Failed to push some bookmarks"},
		},
		{
			name:           "conflict during retry rebase rolls back",
			env:            map[string]string{"JJ_NO_BOOKMARK": "1", "JJ_CONFLICTS": "c0ffee1 fix: frobnicate\n"},
			pushReject:     1,
			divergedSwitch: true,
			want: [][]string{
				{"jj", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
				{"jj", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "log", "-r", `bookmarks(exact:"main")`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "diff", "--name-only"},
				{"jj", "commit", "-m", "fix: frobnicate"},
				{"jj", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
				{"jj", "bookmark", "list", jjExactPattern("main"), "--all-remotes", "-T", jjRemoteBookmarkTemplate},
				{"jj", "git", "fetch"},
				{"jj", "log", "-r", `bookmarks(exact:"main")`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "log", "-r", fmt.Sprintf(jjAncestorRevsetFmt, "main"), "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "bookmark", "move", "exact:main", "--to", "@-"},
				{"jj", "op", "log", "-n", "1", "--no-graph", "-T", jjOpIDTemplate},
				{"jj", "git", "push", "--bookmark", "exact:main"},
				{"jj", "op", "revert", "op123abc"},
				{"jj", "git", "fetch"},
				{"jj", "log", "-r", `bookmarks(exact:"main")`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "log", "-r", fmt.Sprintf(jjAncestorRevsetFmt, "main"), "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "log", "-r", fmt.Sprintf(jjStackRevsetFmt, "main"), "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "rebase", "-b", "@-", "--destination", `bookmarks(exact:"main")`},
				{"jj", "op", "log", "-n", "1", "--no-graph", "-T", jjOpIDTemplate},
				{"jj", "log", "-r", `conflicts() & (bookmarks(exact:"main")..@-)::`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "op", "revert", "op123abc"},
			},
			wantErr: []string{`rebase onto "main" conflicts`, "c0ffee1", "rolled back"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := setupShip(t, ".jj", true)
			for key, value := range tt.env {
				t.Setenv(key, value)
			}
			if tt.describeMarker {
				marker := filepath.Join(t.TempDir(), "describe")
				if err := os.WriteFile(marker, nil, 0o600); err != nil {
					t.Fatalf("write describe marker: %v", err)
				}
				t.Setenv("JJ_DESCRIBE_MARKER", marker)
			}
			if tt.pushReject > 0 {
				marker := filepath.Join(t.TempDir(), "jjpush")
				if err := os.WriteFile(marker, []byte(fmt.Sprintf("%d", tt.pushReject)), 0o600); err != nil {
					t.Fatalf("write push marker: %v", err)
				}
				t.Setenv("JJ_PUSH_REJECT_MARKER", marker)
			}
			if tt.divergedSwitch {
				t.Setenv("JJ_DIVERGED_MARKER", filepath.Join(t.TempDir(), "jjdiverged"))
			}

			args := append([]string{"-m", "fix: frobnicate", "--no-watch"}, tt.args...)
			got, err := runShipCmd(t, args...)
			if len(tt.wantErr) == 0 {
				if err != nil {
					t.Fatalf("ship error = %v", err)
				}
				if got != tt.summary {
					t.Errorf("summary = %q, want %q", got, tt.summary)
				}
			} else {
				if err == nil {
					t.Fatal("expected ship error, got nil")
				}
				for _, want := range tt.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error = %q, want it to contain %q", err, want)
					}
				}
			}
			assertInvocations(t, readInvocations(t, log), tt.want)
		})
	}
}

func TestShipJJPushRevertTargetsBookmarkMove(t *testing.T) {
	log := setupShip(t, ".jj", true)
	t.Setenv("JJ_NO_BOOKMARK", "1")
	t.Setenv("JJ_DIVERGED", "1")
	t.Setenv("JJ_OP_LOG_COUNTER", filepath.Join(t.TempDir(), "opcounter"))
	marker := filepath.Join(t.TempDir(), "jjpush")
	if err := os.WriteFile(marker, []byte("1"), 0o600); err != nil {
		t.Fatalf("write push marker: %v", err)
	}
	t.Setenv("JJ_PUSH_REJECT_MARKER", marker)

	if _, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch"); err != nil {
		t.Fatalf("ship error = %v", err)
	}
	// Attempt 1 logs op001 after the rebase and op002 after the bookmark move; the
	// rejected push must revert op002 (the move), never op001 (the rebase, which is
	// kept and replayed onto the fresh remote tip).
	var reverted []string
	for _, inv := range readInvocations(t, log) {
		if len(inv) == 4 && inv[0] == "jj" && inv[1] == "op" && inv[2] == "revert" {
			reverted = append(reverted, inv[3])
		}
	}
	if want := []string{"op002"}; !reflect.DeepEqual(reverted, want) {
		t.Errorf("op revert targets = %v, want %v", reverted, want)
	}
}

func TestShipJJPushRevertFailureIsTerminal(t *testing.T) {
	log := setupShip(t, ".jj", true)
	t.Setenv("JJ_NO_BOOKMARK", "1")
	t.Setenv("JJ_OP_REVERT_FAIL", "1")
	marker := filepath.Join(t.TempDir(), "jjpush")
	if err := os.WriteFile(marker, []byte("1"), 0o600); err != nil {
		t.Fatalf("write push marker: %v", err)
	}
	t.Setenv("JJ_PUSH_REJECT_MARKER", marker)

	_, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch")
	if err == nil {
		t.Fatal("expected terminal error when op revert fails")
	}
	if !strings.Contains(err.Error(), "jj op revert op123abc") {
		t.Errorf("error = %q, want it to name the manual revert command", err)
	}
	// A failed undo is terminal: exactly one fetch, no retry.
	fetches := 0
	for _, inv := range readInvocations(t, log) {
		if len(inv) >= 3 && inv[0] == "jj" && inv[1] == "git" && inv[2] == "fetch" {
			fetches++
		}
	}
	if fetches != 1 {
		t.Errorf("jj git fetch count = %d, want 1 (a failed undo must not retry)", fetches)
	}
}

func TestShipJJRebasePreservesHookSummary(t *testing.T) {
	setupShip(t, ".jj", false)
	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o750); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	writeShipHookFiles(t, root, "f1.go")
	t.Setenv("JJ_DIFF_NAMES", "f1.go\n")
	t.Setenv("JJ_NO_BOOKMARK", "1")
	t.Setenv("JJ_DIVERGED", "1")
	describeMarker := filepath.Join(t.TempDir(), "describe")
	if err := os.WriteFile(describeMarker, nil, 0o600); err != nil {
		t.Fatalf("write describe marker: %v", err)
	}
	t.Setenv("JJ_DESCRIBE_MARKER", describeMarker)

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := `hooks ok · committed e9f8a7b "fix: frobnicate" · rebased 2 commit(s) onto main · pushed main → origin`
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	if count := strings.Count(got, "hooks ok"); count != 1 {
		t.Errorf("hooks segment count = %d, want 1 in %q", count, got)
	}
	if count := strings.Count(got, "committed "); count != 1 {
		t.Errorf("committed segment count = %d, want 1 in %q", count, got)
	}
}

func TestShipJJForeignBookmarkFails(t *testing.T) {
	log := setupShip(t, ".jj", true)
	t.Setenv("JJ_BOOKMARK_NAMES", "someone/probe")

	_, err := runShipCmd(t, "-m", "fix: frobnicate")
	if err == nil {
		t.Fatal("expected error when nearest bookmark is not trunk, got nil")
	}
	if !strings.Contains(err.Error(), `"someone/probe" is not trunk`) {
		t.Errorf("error = %v, want it to name the foreign bookmark", err)
	}
	if !strings.Contains(err.Error(), "--bookmark someone/probe") {
		t.Errorf("error = %v, want it to suggest --bookmark someone/probe", err)
	}
	invocations := readInvocations(t, log)
	assertInvocations(t, invocations, [][]string{
		{"jj", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
		{"jj", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
	})
	assertNoShipCommit(t, invocations)
}

func TestShipJJMultipleNearestBookmarksFails(t *testing.T) {
	log := setupShip(t, ".jj", true)
	t.Setenv("JJ_BOOKMARK_NAMES", "main other")

	_, err := runShipCmd(t, "-m", "fix: frobnicate")
	if err == nil {
		t.Fatal("expected error when several bookmarks are nearest, got nil")
	}
	if !strings.Contains(err.Error(), "multiple nearest bookmarks") {
		t.Errorf("error = %v, want it to mention multiple nearest bookmarks", err)
	}
	invocations := readInvocations(t, log)
	assertInvocations(t, invocations, [][]string{
		{"jj", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
		{"jj", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
	})
	assertNoShipCommit(t, invocations)
}

func TestShipJJUnresolvableTrunkFails(t *testing.T) {
	tests := []struct {
		name       string
		trunkNames string
	}{
		{"no trunk bookmark", ""},
		{"ambiguous trunk names", "main dev"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := setupShip(t, ".jj", true)
			t.Setenv("JJ_TRUNK_NAMES", tt.trunkNames)

			_, err := runShipCmd(t, "-m", "fix: frobnicate")
			if err == nil {
				t.Fatal("expected error when trunk is unresolvable, got nil")
			}
			if !strings.Contains(err.Error(), "cannot resolve the trunk bookmark") {
				t.Errorf("error = %v, want it to mention the unresolvable trunk", err)
			}
			invocations := readInvocations(t, log)
			assertInvocations(t, invocations, [][]string{
				{"jj", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
			})
			assertNoShipCommit(t, invocations)
		})
	}
}

func TestShipJJBookmarkOverride(t *testing.T) {
	log := setupShip(t, ".jj", true)

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch", "--bookmark", "someone/probe")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := `committed a1b2c3d "fix: frobnicate" · pushed someone/probe → origin`
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	assertInvocations(t, readInvocations(t, log), [][]string{
		{"jj", "log", "-r", `bookmarks(exact:"someone/probe")`, "--no-graph", "-T", jjStackLineTemplate},
		{"jj", "diff", "--name-only"},
		{"jj", "commit", "-m", "fix: frobnicate"},
		{"jj", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
		{"jj", "bookmark", "list", jjExactPattern("someone/probe"), "--all-remotes", "-T", jjRemoteBookmarkTemplate},
		{"jj", "git", "fetch"},
		{"jj", "log", "-r", `bookmarks(exact:"someone/probe")`, "--no-graph", "-T", jjStackLineTemplate},
		{"jj", "log", "-r", fmt.Sprintf(jjAncestorRevsetFmt, "someone/probe"), "--no-graph", "-T", jjBookmarkTemplate},
		{"jj", "bookmark", "move", "exact:someone/probe", "--to", "@-"},
		{"jj", "op", "log", "-n", "1", "--no-graph", "-T", jjOpIDTemplate},
		{"jj", "git", "push", "--bookmark", "exact:someone/probe"},
	})
}

func TestShipJJBookmarkOverrideNotFound(t *testing.T) {
	log := setupShip(t, ".jj", true)
	t.Setenv("JJ_BOOKMARK_HEADS", "0")

	_, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch", "--bookmark", "someone/probe")
	if err == nil {
		t.Fatal("expected error for a nonexistent --bookmark, got nil")
	}
	if !strings.Contains(err.Error(), `bookmark "someone/probe" not found`) {
		t.Errorf("error = %v, want it to say the bookmark was not found", err)
	}
	invocations := readInvocations(t, log)
	assertInvocations(t, invocations, [][]string{
		{"jj", "log", "-r", `bookmarks(exact:"someone/probe")`, "--no-graph", "-T", jjStackLineTemplate},
	})
	assertNoShipCommit(t, invocations)
}

func TestShipGitBookmarkFlagFails(t *testing.T) {
	log := setupShip(t, ".git", true)

	_, err := runShipCmd(t, "-m", "fix: frobnicate", "--bookmark", "main")
	if err == nil {
		t.Fatal("expected error for --bookmark in a git repo, got nil")
	}
	if !strings.Contains(err.Error(), "applies only to jj") {
		t.Errorf("error = %v, want it to say --bookmark applies only to jj", err)
	}
	if inv := readInvocations(t, log); inv != nil {
		t.Errorf("no VCS command should run when --bookmark is rejected, got %v", inv)
	}
}

func TestShipRequiresMessage(t *testing.T) {
	log := setupShip(t, ".jj", true)
	_, err := runShipCmd(t)
	if err == nil {
		t.Fatal("expected error when message missing, got nil")
	}
	if !strings.Contains(err.Error(), "required") {
		t.Errorf("error = %v, want it to mention message required", err)
	}
	if inv := readInvocations(t, log); inv != nil {
		t.Errorf("no VCS command should run when message is missing, got %v", inv)
	}
}

func TestShipNoRepoFails(t *testing.T) {
	log := setupShip(t, "", true)
	_, err := runShipCmd(t, "-m", "fix: frobnicate")
	if err == nil {
		t.Fatal("expected error outside a repo, got nil")
	}
	if !strings.Contains(err.Error(), "no git or jj repository") {
		t.Errorf("error = %v, want it to mention no repository", err)
	}
	if inv := readInvocations(t, log); inv != nil {
		t.Errorf("no VCS command should run outside a repo, got %v", inv)
	}
}

func TestShipCISuccessReportLine(t *testing.T) {
	setupShip(t, ".jj", true)
	t.Setenv("GH_RUN_LIST_JSON", fakeRunListJSON)
	t.Setenv("GH_RUN_VIEW_JSON", fakeRunViewSuccess)
	shipCIPollInterval = 0

	out, _, err := runShipCmdFull(t, "-m", "fix: frobnicate")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := "ci · success · 58s · https://github.com/x/actions/runs/42"
	if !strings.Contains(out, want) {
		t.Errorf("output missing run line %q\ngot:\n%s", want, out)
	}
}

func TestShipCIFailureDetail(t *testing.T) {
	setupShip(t, ".jj", true)
	t.Setenv("GH_RUN_LIST_JSON", fakeRunListJSON)
	t.Setenv("GH_RUN_VIEW_JSON", fakeRunViewFailure)
	t.Setenv("GH_WATCH_EXIT", "1")
	t.Setenv("GH_LOG_FAILED", "test\tgo test ./...\t##[error]FAIL: TestFrobnicate (0.01s)\n")
	shipCIPollInterval = 0

	out, _, err := runShipCmdFull(t, "-m", "fix: frobnicate")
	if err == nil {
		t.Fatal("expected error on CI failure, got nil")
	}
	for _, want := range []string{
		"· CI failure",
		"failed: test · go test ./...",
		"##[error]FAIL: TestFrobnicate",
		"full log: gh run view 42 --log-failed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\ngot:\n%s", want, out)
		}
	}
}

func TestShipCIBudgetCapsLog(t *testing.T) {
	bigLog := strings.Repeat("a padded log line stretched to about fifty chars\n", 900) // ~44 KB

	tests := []struct {
		name       string
		args       []string
		wantCapped bool
	}{
		{"default budget caps the excerpt", nil, true},
		{"budget 0 leaves it uncapped", []string{"--budget", "0"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setupShip(t, ".jj", true)
			t.Setenv("GH_RUN_LIST_JSON", fakeRunListJSON)
			t.Setenv("GH_RUN_VIEW_JSON", fakeRunViewFailure)
			t.Setenv("GH_LOG_FAILED", bigLog)
			shipCIPollInterval = 0

			args := append([]string{"-m", "fix: frobnicate"}, tt.args...)
			out, _, err := runShipCmdFull(t, args...)
			if err == nil {
				t.Fatal("expected error on CI failure, got nil")
			}
			capped := strings.Contains(out, "tokens omitted")
			if capped != tt.wantCapped {
				t.Errorf("capped = %v, want %v", capped, tt.wantCapped)
			}
			if !tt.wantCapped && !strings.Contains(out, bigLog[:len(bigLog)-1]) {
				t.Errorf("uncapped output should contain the whole log")
			}
			// The pointer line survives regardless of capping.
			if !strings.Contains(out, "full log: gh run view 42 --log-failed") {
				t.Errorf("missing full-log pointer\ngot tail:\n%s", out[max(0, len(out)-200):])
			}
		})
	}
}

func TestShipCIStripsANSI(t *testing.T) {
	setupShip(t, ".jj", true)
	t.Setenv("GH_RUN_LIST_JSON", fakeRunListJSON)
	t.Setenv("GH_RUN_VIEW_JSON", fakeRunViewFailure)
	t.Setenv("GH_LOG_FAILED", "\x1b[31mERROR\x1b[0m the build \x1b[1mboom\x1b[0m\n")
	shipCIPollInterval = 0

	out, _, err := runShipCmdFull(t, "-m", "fix: frobnicate")
	if err == nil {
		t.Fatal("expected error on CI failure, got nil")
	}
	if strings.Contains(out, "\x1b") {
		t.Errorf("ANSI escapes leaked into output: %q", out)
	}
	if !strings.Contains(out, "ERROR the build boom") {
		t.Errorf("stripped log text missing\ngot:\n%s", out)
	}
}

func TestShipCITransientPollTolerated(t *testing.T) {
	log := setupShip(t, ".jj", true)
	marker := filepath.Join(t.TempDir(), "fail-once")
	if err := os.WriteFile(marker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	t.Setenv("GH_LIST_FAIL_MARKER", marker)
	t.Setenv("GH_RUN_LIST_JSON", fakeRunListJSON)
	t.Setenv("GH_RUN_VIEW_JSON", fakeRunViewSuccess)
	shipCIPollInterval = 0

	got, err := runShipCmd(t, "-m", "fix: frobnicate")
	if err != nil {
		t.Fatalf("transient list error should be tolerated, got %v", err)
	}
	if want := "· CI success"; !strings.Contains(got, want) {
		t.Errorf("summary = %q, want it to contain %q", got, want)
	}
	listCalls := 0
	for _, inv := range readInvocations(t, log) {
		if len(inv) >= 3 && inv[0] == "gh" && inv[1] == "run" && inv[2] == "list" {
			listCalls++
		}
	}
	if listCalls < 2 {
		t.Errorf("expected the poll to retry (>=2 list calls), got %d", listCalls)
	}
}

func TestShipCIAllPollsFailStillReports(t *testing.T) {
	setupShip(t, ".jj", true)
	t.Setenv("GH_LIST_FAIL", "1")
	shipCIPollTries = 3
	shipCIPollInterval = 0
	t.Cleanup(func() { shipCIPollTries = 12 })

	out, _, err := runShipCmdFull(t, "-m", "fix: frobnicate")
	if err == nil {
		t.Fatal("expected error when every poll fails, got nil")
	}
	summary := out
	if i := strings.IndexByte(summary, '\n'); i >= 0 {
		summary = summary[:i]
	}
	if want := "· CI error"; !strings.Contains(summary, want) {
		t.Errorf("summary = %q, want it to contain %q (abort-before-summary regression)", summary, want)
	}
	if want := "check: gh run list --commit " + fakeHeadSHA; !strings.Contains(out, want) {
		t.Errorf("output missing %q\ngot:\n%s", want, out)
	}
}

func TestShipCIViewFailureIsError(t *testing.T) {
	setupShip(t, ".jj", true)
	t.Setenv("GH_RUN_LIST_JSON", fakeRunListJSON)
	// GH_RUN_VIEW_JSON unset: gh run view emits empty stdout, so the parse fails.
	shipCIPollInterval = 0

	out, _, err := runShipCmdFull(t, "-m", "fix: frobnicate")
	if err == nil {
		t.Fatal("expected error when gh run view cannot be parsed, got nil")
	}
	if want := "· CI error"; !strings.Contains(out, want) {
		t.Errorf("output missing %q\ngot:\n%s", want, out)
	}
}

func TestShipCIWatchErrViewGreenIsSuccess(t *testing.T) {
	setupShip(t, ".jj", true)
	t.Setenv("GH_RUN_LIST_JSON", fakeRunListJSON)
	t.Setenv("GH_RUN_VIEW_JSON", fakeRunViewSuccess)
	t.Setenv("GH_WATCH_EXIT", "1") // watch drops, view says success — view wins
	shipCIPollInterval = 0

	got, err := runShipCmd(t, "-m", "fix: frobnicate")
	if err != nil {
		t.Fatalf("view-green run should heal a dropped watch, got %v", err)
	}
	if want := "· CI success"; !strings.Contains(got, want) {
		t.Errorf("summary = %q, want it to contain %q", got, want)
	}
}

func TestShipCIMultiRunWatchesAll(t *testing.T) {
	log := setupShip(t, ".jj", true)
	t.Setenv("GH_RUN_LIST_JSON", `[`+
		`{"databaseId":42,"workflowName":"ci","status":"completed","url":"https://x/42"},`+
		`{"databaseId":43,"workflowName":"cc-notes","status":"completed","url":"https://x/43"}]`)
	t.Setenv("GH_RUN_VIEW_JSON_42", fakeRunViewSuccess)
	t.Setenv("GH_RUN_VIEW_JSON_43", `{"workflowName":"cc-notes","conclusion":"failure","startedAt":"2026-07-08T18:00:00Z","updatedAt":"2026-07-08T18:00:05Z","url":"https://x/43","jobs":[{"name":"notes","conclusion":"failure","steps":[{"name":"sync","conclusion":"failure"}]}]}`)
	t.Setenv("GH_WATCH_EXIT_43", "1")
	t.Setenv("GH_LOG_FAILED_43", "notes sync exploded\n")
	shipCIPollInterval = 0

	out, _, err := runShipCmdFull(t, "-m", "fix: frobnicate")
	if err == nil {
		t.Fatal("expected error when one of several runs is red, got nil")
	}
	watched := map[string]bool{}
	for _, inv := range readInvocations(t, log) {
		if len(inv) >= 4 && inv[0] == "gh" && inv[1] == "run" && inv[2] == "watch" {
			watched[inv[3]] = true
		}
	}
	if !watched["42"] || !watched["43"] {
		t.Errorf("expected both runs watched, got %v", watched)
	}
	for _, want := range []string{
		"· CI failure",
		"ci · success",
		"cc-notes · failure",
		"failed: notes · sync",
		"full log: gh run view 43 --log-failed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\ngot:\n%s", want, out)
		}
	}
}

func TestShipCIMoreThanTenRunsWatchesAll(t *testing.T) {
	log := setupShip(t, ".jj", true)
	var runList strings.Builder
	runList.WriteByte('[')
	for id := 100; id < 112; id++ {
		if id > 100 {
			runList.WriteByte(',')
		}
		fmt.Fprintf(&runList, `{"databaseId":%d,"workflowName":"workflow-%d","status":"completed","url":"https://x/%d"}`, id, id, id)
		t.Setenv(fmt.Sprintf("GH_RUN_VIEW_JSON_%d", id), fmt.Sprintf(`{"workflowName":"workflow-%d","conclusion":"success","startedAt":"2026-07-08T18:00:00Z","updatedAt":"2026-07-08T18:00:01Z","url":"https://x/%d","jobs":[]}`, id, id))
	}
	runList.WriteByte(']')
	t.Setenv("GH_RUN_LIST_JSON", runList.String())
	shipCIPollInterval = 0

	out, _, err := runShipCmdFull(t, "-m", "fix: frobnicate")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	watched := map[string]int{}
	limit50 := false
	for _, inv := range readInvocations(t, log) {
		if len(inv) >= 4 && inv[0] == "gh" && inv[1] == "run" && inv[2] == "watch" {
			watched[inv[3]]++
		}
		if len(inv) >= 3 && inv[0] == "gh" && inv[1] == "run" && inv[2] == "list" {
			for i := 3; i+1 < len(inv); i++ {
				if inv[i] == "--limit" && inv[i+1] == "50" {
					limit50 = true
				}
			}
		}
	}
	if !limit50 {
		t.Error("gh run list did not use --limit 50")
	}
	if len(watched) != 12 {
		t.Errorf("watched %d runs, want 12: %v", len(watched), watched)
	}
	for id := 100; id < 112; id++ {
		key := fmt.Sprintf("%d", id)
		if watched[key] != 1 {
			t.Errorf("run %s watched %d times, want 1", key, watched[key])
		}
		want := fmt.Sprintf("workflow-%d · success · 1s · https://x/%d", id, id)
		if !strings.Contains(out, want) {
			t.Errorf("output missing report line %q\ngot:\n%s", want, out)
		}
	}
}

func TestShipCISettleWatchesLateRuns(t *testing.T) {
	log := setupShip(t, ".jj", true)
	marker := filepath.Join(t.TempDir(), "settle")
	t.Setenv("GH_LIST_SETTLE_MARKER", marker)
	t.Setenv("GH_LIST_SETTLE_AFTER", "2")
	t.Setenv("GH_RUN_LIST_JSON", fakeRunListJSON) // first list: run 42 only
	t.Setenv("GH_RUN_LIST_JSON_2", `[`+
		`{"databaseId":42,"workflowName":"ci","status":"completed","url":"https://x/42"},`+
		`{"databaseId":44,"workflowName":"settle-late","status":"completed","url":"https://x/44"}]`)
	t.Setenv("GH_RUN_VIEW_JSON_42", fakeRunViewSuccess)
	t.Setenv("GH_RUN_VIEW_JSON_44", `{"workflowName":"settle-late","conclusion":"success","startedAt":"2026-07-08T18:00:00Z","updatedAt":"2026-07-08T18:00:10Z","url":"https://x/44","jobs":[]}`)
	shipCIPollInterval = 0

	out, _, err := runShipCmdFull(t, "-m", "fix: frobnicate")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	watched := map[string]bool{}
	listCalls := 0
	for _, inv := range readInvocations(t, log) {
		if len(inv) >= 3 && inv[0] == "gh" && inv[1] == "run" && inv[2] == "list" {
			listCalls++
		}
		if len(inv) >= 4 && inv[0] == "gh" && inv[1] == "run" && inv[2] == "watch" {
			watched[inv[3]] = true
		}
	}
	if listCalls < 5 {
		t.Errorf("expected initial discovery, a quiet re-list, the straggler, and the quiet horizon; got %d list calls", listCalls)
	}
	if !watched["42"] || !watched["44"] {
		t.Errorf("expected the settle pass to watch both runs, got %v", watched)
	}
	for _, want := range []string{"ci · success", "settle-late · success"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing settle report line %q\ngot:\n%s", want, out)
		}
	}
}

func TestShipCIBudgetFloorsPerRunShare(t *testing.T) {
	setupShip(t, ".jj", true)
	bigLog := strings.Repeat("a padded log line stretched to about fifty chars\n", 900) // ~44 KB
	t.Setenv("GH_RUN_LIST_JSON", `[`+
		`{"databaseId":42,"workflowName":"ci","status":"completed","url":"https://x/42"},`+
		`{"databaseId":43,"workflowName":"cc-notes","status":"completed","url":"https://x/43"}]`)
	t.Setenv("GH_RUN_VIEW_JSON_42", fakeRunViewFailure)
	t.Setenv("GH_RUN_VIEW_JSON_43", `{"workflowName":"cc-notes","conclusion":"failure","startedAt":"2026-07-08T18:00:00Z","updatedAt":"2026-07-08T18:00:05Z","url":"https://x/43","jobs":[{"name":"notes","conclusion":"failure","steps":[{"name":"sync","conclusion":"failure"}]}]}`)
	t.Setenv("GH_LOG_FAILED_42", bigLog)
	t.Setenv("GH_LOG_FAILED_43", bigLog)
	shipCIPollInterval = 0

	// --budget 1 with two red runs floors the per-run share to 1 (not 0 = uncapped).
	out, _, err := runShipCmdFull(t, "-m", "fix: frobnicate", "--budget", "1")
	if err == nil {
		t.Fatal("expected error on CI failure, got nil")
	}
	if !strings.Contains(out, "tokens omitted") {
		t.Errorf("expected both excerpts capped (tokens omitted footer)\ngot tail:\n%s", out[max(0, len(out)-400):])
	}
	if strings.Contains(out, bigLog[:len(bigLog)-1]) {
		t.Errorf("full log leaked past the floored budget")
	}
}

func TestShipCIEmptyConclusionIsIndeterminate(t *testing.T) {
	log := setupShip(t, ".jj", true)
	t.Setenv("GH_RUN_LIST_JSON", fakeRunListJSON)
	t.Setenv("GH_RUN_VIEW_JSON", `{"workflowName":"ci","conclusion":"","startedAt":"2026-07-08T18:00:00Z","updatedAt":"2026-07-08T18:00:05Z","url":"https://x/42","jobs":[]}`)
	shipCIPollInterval = 0

	out, _, err := runShipCmdFull(t, "-m", "fix: frobnicate")
	if err == nil {
		t.Fatal("expected error when a run has not concluded, got nil")
	}
	summary := out
	if i := strings.IndexByte(summary, '\n'); i >= 0 {
		summary = summary[:i]
	}
	if want := "· CI error"; !strings.Contains(summary, want) {
		t.Errorf("summary = %q, want it to contain %q", summary, want)
	}
	if want := "run 42 has not concluded; check: gh run view 42"; !strings.Contains(out, want) {
		t.Errorf("output missing not-concluded pointer %q\ngot:\n%s", want, out)
	}
	for _, inv := range readInvocations(t, log) {
		for _, a := range inv {
			if a == "--log-failed" {
				t.Errorf("indeterminate run must not fetch --log-failed, got %v", inv)
			}
		}
	}
}

func TestShipCIStreamingSeam(t *testing.T) {
	tests := []struct {
		name        string
		stream      bool
		wantCompact bool
		wantErrText bool
	}{
		{"tty streams to stderr with --compact", true, true, true},
		{"non-tty buffers watch output away", false, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := setupShip(t, ".jj", true)
			t.Setenv("GH_RUN_LIST_JSON", fakeRunListJSON)
			t.Setenv("GH_RUN_VIEW_JSON", fakeRunViewSuccess)
			shipCIPollInterval = 0

			old := shipStreamCI
			t.Cleanup(func() { shipStreamCI = old })
			shipStreamCI = func(io.Writer) bool { return tt.stream }

			_, errStr, err := runShipCmdFull(t, "-m", "fix: frobnicate")
			if err != nil {
				t.Fatalf("ship error = %v", err)
			}
			compact := false
			for _, inv := range readInvocations(t, log) {
				if len(inv) >= 3 && inv[0] == "gh" && inv[1] == "run" && inv[2] == "watch" {
					for _, a := range inv {
						if a == "--compact" {
							compact = true
						}
					}
				}
			}
			if compact != tt.wantCompact {
				t.Errorf("watch --compact = %v, want %v", compact, tt.wantCompact)
			}
			if got := strings.Contains(errStr, "watch stream 42"); got != tt.wantErrText {
				t.Errorf("stderr carries watch stream = %v, want %v (stderr=%q)", got, tt.wantErrText, errStr)
			}
		})
	}
}

func TestCIDuration(t *testing.T) {
	start := time.Date(2026, 7, 8, 18, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		start time.Time
		end   time.Time
		want  string
	}{
		{"normal span", start, start.Add(58 * time.Second), "58s"},
		{"zero start omits", time.Time{}, start, ""},
		{"negative span omits", start, start.Add(-time.Second), ""},
		{"equal is zero seconds", start, start, "0s"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ciDuration(tt.start, tt.end); got != tt.want {
				t.Errorf("ciDuration = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWithSessionTrailer(t *testing.T) {
	tests := []struct {
		name    string
		id      string
		message string
		want    string
	}{
		{"env set appends trailer", "sess-abc", "fix: frobnicate", "fix: frobnicate\n\nClaude-Session-Id: sess-abc"},
		{"env empty leaves message", "", "fix: frobnicate", "fix: frobnicate"},
		{"empty message stays empty", "sess-abc", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(envClaudeSessionKey, tt.id)
			if got := withSessionTrailer(tt.message); got != tt.want {
				t.Errorf("withSessionTrailer(%q) = %q, want %q", tt.message, got, tt.want)
			}
		})
	}
}

func TestCIGreen(t *testing.T) {
	tests := []struct {
		conclusion string
		want       bool
	}{
		{"success", true},
		{"skipped", true},
		{"neutral", true},
		{"failure", false},
		{"cancelled", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.conclusion, func(t *testing.T) {
			if got := ciGreen(tt.conclusion); got != tt.want {
				t.Errorf("ciGreen(%q) = %v, want %v", tt.conclusion, got, tt.want)
			}
		})
	}
}

// assertNoGTCommit fails the test if a gt create or modify ran, for a refusal
// that must fire before any commit side effect.
func assertNoGTCommit(t *testing.T, invocations [][]string) {
	t.Helper()
	for _, inv := range invocations {
		if len(inv) > 1 && inv[0] == "gt" && (inv[1] == "create" || inv[1] == "modify") {
			t.Errorf("commit ran before refusal: %v", inv)
		}
	}
}

func TestShipGTPrecedenceOverJJ(t *testing.T) {
	t.Run("graphite wins over a colocated jj marker", func(t *testing.T) {
		log := setupShipGT(t, false)
		if err := os.Mkdir(".jj", 0o750); err != nil {
			t.Fatalf("mkdir .jj: %v", err)
		}
		got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
		if err != nil {
			t.Fatalf("ship error = %v", err)
		}
		want := `committed a1b2c3d "fix: frobnicate" · not pushed`
		if got != want {
			t.Errorf("summary = %q, want %q", got, want)
		}
		assertInvocations(t, readInvocations(t, log), [][]string{
			{"git", "branch", "--show-current"},
			{"gt", "state"},
			{"git", "add", "-A"},
			{"git", "diff", "--cached", "--quiet"},
			{"gt", "modify", "-c", "-m", "fix: frobnicate", "--no-interactive"},
			{"git", "log", "-1", "--format=%h%x00%s"},
		})
	})

	t.Run("--no-gt falls back to jj", func(t *testing.T) {
		log := setupShipGT(t, false)
		if err := os.Mkdir(".jj", 0o750); err != nil {
			t.Fatalf("mkdir .jj: %v", err)
		}
		got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "--no-gt")
		if err != nil {
			t.Fatalf("ship error = %v", err)
		}
		want := `committed a1b2c3d "fix: frobnicate" · not pushed`
		if got != want {
			t.Errorf("summary = %q, want %q", got, want)
		}
		assertInvocations(t, readInvocations(t, log), [][]string{
			{"jj", "diff", "--name-only"},
			{"jj", "commit", "-m", "fix: frobnicate"},
			{"jj", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
		})
	})
}

func TestShipGTStackedHappyPath(t *testing.T) {
	tests := []struct {
		name      string
		branch    string
		stateJSON string
		wantSeg   string
	}{
		{
			name:      "depth 1",
			branch:    "feature",
			stateJSON: `{"main":{"trunk":true},"feature":{"parents":[{"ref":"main","sha":"deadbeef"}]}}`,
			wantSeg:   "submitted feature → PR #7 https://github.com/x/pull/7",
		},
		{
			name:   "depth 2",
			branch: "feature2",
			stateJSON: `{"main":{"trunk":true},"feature":{"parents":[{"ref":"main","sha":"deadbeef"}]},` +
				`"feature2":{"parents":[{"ref":"feature","sha":"beadfeed"}]}}`,
			wantSeg: "submitted feature2 → PR #7 https://github.com/x/pull/7 (stack of 2)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := setupShipGT(t, true)
			t.Setenv("GIT_BRANCH", tt.branch)
			t.Setenv("GT_STATE_JSON", tt.stateJSON)
			t.Setenv("GH_RUN_LIST_JSON", fakeRunListJSON)
			t.Setenv("GH_RUN_VIEW_JSON", fakeRunViewSuccess)
			t.Setenv("GH_PR_VIEW_JSON", `{"number":7,"url":"https://github.com/x/pull/7"}`)
			shipCIPollInterval = 0

			got, err := runShipCmd(t, "-m", "fix: frobnicate")
			if err != nil {
				t.Fatalf("ship error = %v", err)
			}
			want := `committed a1b2c3d "fix: frobnicate" · ` + tt.wantSeg + ` · CI success`
			if got != want {
				t.Errorf("summary = %q, want %q", got, want)
			}
			assertInvocations(t, readInvocations(t, log), [][]string{
				{"git", "branch", "--show-current"},
				{"gt", "state"},
				{"git", "add", "-A"},
				{"git", "diff", "--cached", "--quiet"},
				{"gt", "modify", "-c", "-m", "fix: frobnicate", "--no-interactive"},
				{"git", "log", "-1", "--format=%h%x00%s"},
				{"git", "branch", "--show-current"},
				{"gt", "submit", "--no-interactive", "--no-edit", "--no-ai", "--no-stack", "--publish"},
				{"gh", "pr", "view", tt.branch, "--json", "number,url"},
				{"git", "rev-parse", "HEAD"},
				{"gh", "run", "list", "--commit", fakeHeadSHA, "--limit", "50", "--json", "databaseId,workflowName,status,url"},
				{"gh", "run", "watch", "42", "--exit-status"},
				{"gh", "run", "view", "42", "--json", "workflowName,conclusion,startedAt,updatedAt,url,jobs"},
				{"gh", "run", "list", "--commit", fakeHeadSHA, "--limit", "50", "--json", "databaseId,workflowName,status,url"},
				{"gh", "run", "list", "--commit", fakeHeadSHA, "--limit", "50", "--json", "databaseId,workflowName,status,url"},
			})
		})
	}
}

func TestShipGTTrunkAutoCreate(t *testing.T) {
	log := setupShipGT(t, true)
	t.Setenv("GIT_BRANCH", "main")
	t.Setenv("GT_STATE_JSON", `{"main":{"trunk":true}}`)
	t.Setenv("GIT_BRANCH_AFTER_COMMIT", "feat-branch")
	t.Setenv("GH_RUN_LIST_JSON", fakeRunListJSON)
	t.Setenv("GH_RUN_VIEW_JSON", fakeRunViewSuccess)
	t.Setenv("GH_PR_VIEW_JSON", `{"number":9,"url":"https://github.com/x/pull/9"}`)
	shipCIPollInterval = 0

	got, err := runShipCmd(t, "-m", "fix: frobnicate")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := `committed a1b2c3d "fix: frobnicate" · submitted feat-branch → PR #9 https://github.com/x/pull/9 · CI success`
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	assertInvocations(t, readInvocations(t, log), [][]string{
		{"git", "branch", "--show-current"},
		{"gt", "state"},
		{"git", "add", "-A"},
		{"git", "diff", "--cached", "--quiet"},
		{"gt", "create", "-m", "fix: frobnicate", "--no-ai", "--no-interactive"},
		{"git", "log", "-1", "--format=%h%x00%s"},
		{"git", "branch", "--show-current"},
		{"gt", "submit", "--no-interactive", "--no-edit", "--no-ai", "--no-stack", "--publish"},
		{"gh", "pr", "view", "feat-branch", "--json", "number,url"},
		{"git", "rev-parse", "HEAD"},
		{"gh", "run", "list", "--commit", fakeHeadSHA, "--limit", "50", "--json", "databaseId,workflowName,status,url"},
		{"gh", "run", "watch", "42", "--exit-status"},
		{"gh", "run", "view", "42", "--json", "workflowName,conclusion,startedAt,updatedAt,url,jobs"},
		{"gh", "run", "list", "--commit", fakeHeadSHA, "--limit", "50", "--json", "databaseId,workflowName,status,url"},
		{"gh", "run", "list", "--commit", fakeHeadSHA, "--limit", "50", "--json", "databaseId,workflowName,status,url"},
	})
}

func TestShipGTCreateFlag(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{"explicit name", []string{"--create=newbranch"}, []string{"gt", "create", "newbranch", "-m", "fix: frobnicate", "--no-ai", "--no-interactive"}},
		{"bare create derives from message", []string{"--create"}, []string{"gt", "create", "-m", "fix: frobnicate", "--no-ai", "--no-interactive"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := setupShipGT(t, false)
			args := append([]string{"-m", "fix: frobnicate", "--no-push"}, tt.args...)
			if _, err := runShipCmd(t, args...); err != nil {
				t.Fatalf("ship error = %v", err)
			}
			var commit []string
			for _, inv := range readInvocations(t, log) {
				if inv[0] == "gt" && (inv[1] == "create" || inv[1] == "modify") {
					commit = inv
				}
			}
			if !reflect.DeepEqual(commit, tt.want) {
				t.Errorf("commit argv = %v, want %v", commit, tt.want)
			}
		})
	}
}

func TestShipGTAmend(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{"with message", []string{"--amend", "-m", "fix: frobnicate"}, []string{"gt", "modify", "-m", "fix: frobnicate", "--no-interactive"}},
		{"without message", []string{"--amend"}, []string{"gt", "modify", "--no-interactive"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := setupShipGT(t, false)
			args := append(append([]string{}, tt.args...), "--no-push")
			if _, err := runShipCmd(t, args...); err != nil {
				t.Fatalf("ship error = %v", err)
			}
			var commit []string
			for _, inv := range readInvocations(t, log) {
				if inv[0] == "gt" && inv[1] == "modify" {
					commit = inv
				}
				if inv[0] == "git" && inv[1] == "diff" {
					t.Errorf("amend must not probe git diff --cached --quiet: %v", inv)
				}
			}
			if !reflect.DeepEqual(commit, tt.want) {
				t.Errorf("commit argv = %v, want %v", commit, tt.want)
			}
		})
	}

	t.Run("amend on trunk refuses", func(t *testing.T) {
		log := setupShipGT(t, false)
		t.Setenv("GIT_BRANCH", "main")
		t.Setenv("GT_STATE_JSON", `{"main":{"trunk":true}}`)
		_, err := runShipCmd(t, "--amend", "-m", "fix: frobnicate", "--no-push")
		if err == nil {
			t.Fatal("expected refusal, got nil")
		}
		wantErr := "ship: --amend on trunk is refused in the graphite lane — create a stacked branch instead (gt create)"
		if err.Error() != wantErr {
			t.Errorf("error = %q, want %q", err.Error(), wantErr)
		}
		assertNoGTCommit(t, readInvocations(t, log))
	})
}

func TestShipGTPathScoped(t *testing.T) {
	log := setupShipGT(t, false)
	if _, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "src/a.go", "docs"); err != nil {
		t.Fatalf("ship error = %v", err)
	}
	assertInvocations(t, readInvocations(t, log), [][]string{
		{"git", "branch", "--show-current"},
		{"gt", "state"},
		{"git", "add", "-A", "--", "src/a.go", "docs"},
		{"git", "diff", "--cached", "--quiet"},
		{"gt", "modify", "-c", "-m", "fix: frobnicate", "--no-interactive"},
		{"git", "log", "-1", "--format=%h%x00%s"},
	})
}

func TestShipGTHunkScoped(t *testing.T) {
	log := setupShipGT(t, false)
	if err := os.WriteFile("f.txt", []byte(hunkCurrent), 0o644); err != nil { //nolint:gosec // test fixture file
		t.Fatalf("write f.txt: %v", err)
	}
	t.Setenv("GIT_FILE_SHOW_BASE", hunkBase)
	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	writeShipHookFiles(t, root)
	ref := hunkRefFor(t, "f.txt", hunkBase, hunkCurrent, 0)

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "--only-hunk", ref, "f.txt")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := `hooks hunk-skip · committed a1b2c3d "fix: frobnicate" · not pushed`
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	assertInvocations(t, readInvocations(t, log), [][]string{
		{"git", "rev-parse", "--show-toplevel"},
		{"git", "ls-tree", "--full-tree", "HEAD", "--", "f.txt"},
		{"git", "show", "--end-of-options", "HEAD:f.txt"},
		{"git", "branch", "--show-current"},
		{"gt", "state"},
		{"git", "read-tree", "HEAD"},
		{"git", "ls-tree", "--full-tree", "HEAD", "--", "f.txt"},
		{"git", "show", "--end-of-options", "HEAD:f.txt"},
		{"git", "ls-tree", "--full-tree", "HEAD", "--", "f.txt"},
		{"git", "hash-object", "-w", "--stdin"},
		{"git", "update-index", "--add", "--cacheinfo", "100644,2222222222222222222222222222222222222222,f.txt"},
		{"gt", "modify", "-c", "-m", "fix: frobnicate", "--no-interactive"},
		{"git", "log", "-1", "--format=%h%x00%s"},
	})
	for _, inv := range readInvocations(t, log) {
		if inv[0] == "idx" {
			t.Errorf("gt hunk-scoped commit must use the real index, but an idx marker was logged: %v", inv)
		}
		if inv[0] == "git" && inv[1] == "restore" {
			t.Errorf("gt hunk-scoped commit must not restore --staged: %v", inv)
		}
	}
}

func TestShipGTRefusals(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(t *testing.T)
		wantErr string
	}{
		{
			name: "needs restack",
			setup: func(t *testing.T) {
				t.Setenv("GT_STATE_JSON", `{"main":{"trunk":true},"feature":{"needs_restack":true,"parents":[{"ref":"main","sha":"deadbeef"}]}}`)
			},
			wantErr: "ship: stack needs restack — run gt restack (gt continue / gt abort on conflict), then re-run ship",
		},
		{
			name: "untracked branch",
			setup: func(t *testing.T) {
				t.Setenv("GT_STATE_JSON", `{"main":{"trunk":true}}`)
			},
			wantErr: "ship: branch feature is not tracked by graphite — run gt track, or pass --no-gt",
		},
		{
			name: "staged empty",
			setup: func(t *testing.T) {
				t.Setenv("GIT_STAGED_EMPTY", "1")
			},
			wantErr: `ship: nothing to commit — did a prior ship already land a1b2c3d "fix: frobnicate"?`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := setupShipGT(t, false)
			tt.setup(t)
			_, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
			if err == nil {
				t.Fatal("expected refusal, got nil")
			}
			if err.Error() != tt.wantErr {
				t.Errorf("error = %q, want %q", err.Error(), tt.wantErr)
			}
			assertNoGTCommit(t, readInvocations(t, log))
		})
	}
}

func TestShipGTClassifySubmit(t *testing.T) {
	tests := []struct {
		name    string
		stderr  string
		wantErr string
	}{
		{"restack needed (primary wording)", gtRestackNeeded1, "ship: stack drifted since preflight — run gt restack, then re-run ship"},
		{"restack needed (conflict wording)", gtRestackNeeded2 + "feature", "ship: stack drifted since preflight — run gt restack, then re-run ship"},
		{"trunk stale", gtTrunkStale, "ship: trunk is out of sync — run gt sync (or ccx vcs restack), then re-run ship"},
		{"remote changed (updated wording)", gtRemoteChanged1, "ship: remote branch changed since last submit — reconcile manually (gt sync), then re-run ship"},
		{"remote changed (lease wording)", gtRemoteChanged2, "ship: remote branch changed since last submit — reconcile manually (gt sync), then re-run ship"},
		{"auth required (please wording)", gtAuthRequired1, "ship: graphite auth required — run gt auth"},
		{"auth required (invalid wording)", gtAuthRequired2, "ship: graphite auth required — run gt auth"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := setupShipGT(t, false)
			t.Setenv("GT_SUBMIT_FAIL_STDERR", tt.stderr)
			_, err := runShipCmd(t, "-m", "fix: frobnicate")
			if err == nil {
				t.Fatal("expected submit failure, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tt.wantErr)
			}
			submits := 0
			for _, inv := range readInvocations(t, log) {
				if inv[0] == "gt" && inv[1] == "submit" {
					submits++
				}
			}
			if submits != 1 {
				t.Errorf("submit ran %d times, want exactly 1 (gt owns restacking, ship never retries)", submits)
			}
		})
	}

	t.Run("unknown stderr wraps verbatim", func(t *testing.T) {
		setupShipGT(t, false)
		t.Setenv("GT_SUBMIT_FAIL_STDERR", "some other gt error")
		_, err := runShipCmd(t, "-m", "fix: frobnicate")
		if err == nil {
			t.Fatal("expected submit failure, got nil")
		}
		if !strings.Contains(err.Error(), "ship: gt submit:") || !strings.Contains(err.Error(), "some other gt error") {
			t.Errorf("error = %q, want it to wrap ship: gt submit: and the raw stderr", err.Error())
		}
	})
}

func TestShipGTDraftPublish(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"draft", []string{"--draft"}, "--draft"},
		{"default publishes", nil, "--publish"},
		{"explicit publish", []string{"--publish"}, "--publish"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := setupShipGT(t, false)
			args := append([]string{"-m", "fix: frobnicate"}, tt.args...)
			if _, err := runShipCmd(t, args...); err != nil {
				t.Fatalf("ship error = %v", err)
			}
			var submit []string
			for _, inv := range readInvocations(t, log) {
				if inv[0] == "gt" && inv[1] == "submit" {
					submit = inv
				}
			}
			want := []string{"gt", "submit", "--no-interactive", "--no-edit", "--no-ai", "--no-stack", tt.want}
			if !reflect.DeepEqual(submit, want) {
				t.Errorf("submit argv = %v, want %v", submit, want)
			}
		})
	}

	t.Run("draft and publish are mutually exclusive", func(t *testing.T) {
		log := setupShipGT(t, false)
		_, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "--draft", "--publish")
		wantErr := "if any flags in the group [draft publish] are set none of the others can be; [draft publish] were all set"
		if err == nil || err.Error() != wantErr {
			t.Errorf("error = %v, want %q", err, wantErr)
		}
		if inv := readInvocations(t, log); inv != nil {
			t.Errorf("no VCS command may run before flag validation, got %v", inv)
		}
	})
}

func TestShipGTFlagsOutsideGTLane(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"--create", []string{"--create"}},
		{"--draft", []string{"--draft"}},
		{"--publish", []string{"--publish"}},
	}
	wantErr := "ship: --create/--draft/--publish apply only to graphite repos; pass --no-gt only when .git/.graphite_repo_config exists, or drop these flags"
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := setupShip(t, ".git", false)
			args := append(append([]string{}, tt.args...), "--no-push")
			_, err := runShipCmd(t, args...)
			if err == nil || err.Error() != wantErr {
				t.Errorf("error = %v, want %q", err, wantErr)
			}
			if inv := readInvocations(t, log); inv != nil {
				t.Errorf("no VCS command may run before the graphite-only flag check, got %v", inv)
			}
		})
	}
}

func TestShipGTGHMissing(t *testing.T) {
	log := setupShipGT(t, false)
	got, err := runShipCmd(t, "-m", "fix: frobnicate")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := `committed a1b2c3d "fix: frobnicate" · submitted feature · CI gh-missing`
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	for _, inv := range readInvocations(t, log) {
		if inv[0] == "gh" {
			t.Errorf("gh invoked despite missing from PATH: %v", inv)
		}
	}
}

func TestShipGTNoPush(t *testing.T) {
	log := setupShipGT(t, false)
	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := `committed a1b2c3d "fix: frobnicate" · not pushed`
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	sawState, sawSubmit := false, false
	for _, inv := range readInvocations(t, log) {
		if inv[0] == "gt" && inv[1] == "state" {
			sawState = true
		}
		if inv[0] == "gt" && inv[1] == "submit" {
			sawSubmit = true
		}
	}
	if !sawState {
		t.Error("gt state never ran — preflight must run even under --no-push")
	}
	if sawSubmit {
		t.Error("gt submit ran despite --no-push")
	}
}

func TestShipGTNoVerify(t *testing.T) {
	log := setupShipGT(t, false)
	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	writeShipHookFiles(t, root)

	if _, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "--no-verify"); err != nil {
		t.Fatalf("ship error = %v", err)
	}
	var commit []string
	for _, inv := range readInvocations(t, log) {
		if inv[0] == "uvx" {
			t.Errorf("uvx invoked despite --no-verify: %v", inv)
		}
		if inv[0] == "gt" && inv[1] == "modify" {
			commit = inv
		}
	}
	want := []string{"gt", "modify", "-c", "-m", "fix: frobnicate", "--no-interactive", "--no-verify"}
	if !reflect.DeepEqual(commit, want) {
		t.Errorf("commit argv = %v, want %v", commit, want)
	}
}

func TestShipGTSessionTrailer(t *testing.T) {
	log := setupShipGT(t, false)
	t.Setenv(envClaudeSessionKey, "some-uuid")
	if _, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push"); err != nil {
		t.Fatalf("ship error = %v", err)
	}
	var commit []string
	for _, inv := range readInvocations(t, log) {
		if inv[0] == "gt" && inv[1] == "modify" {
			commit = inv
		}
	}
	want := []string{"gt", "modify", "-c", "-m", "fix: frobnicate\n\nClaude-Session-Id: some-uuid", "--no-interactive"}
	if !reflect.DeepEqual(commit, want) {
		t.Errorf("commit argv = %v, want %v", commit, want)
	}
}

func TestShipReviewsWiring(t *testing.T) {
	t.Run("--reviews requires push", func(t *testing.T) {
		log := setupShip(t, ".git", false)
		_, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "--reviews")
		wantErr := "ship: --reviews requires push (drop --no-push)"
		if err == nil || err.Error() != wantErr {
			t.Errorf("error = %v, want %q", err, wantErr)
		}
		if inv := readInvocations(t, log); inv != nil {
			t.Errorf("no VCS command may run before the --reviews/--no-push refusal, got %v", inv)
		}
	})

	t.Run("git lane with no open PR", func(t *testing.T) {
		setupShip(t, ".git", true)
		t.Setenv("GH_RUN_LIST_JSON", fakeRunListJSON)
		t.Setenv("GH_RUN_VIEW_JSON", fakeRunViewSuccess)
		t.Setenv("GH_PR_VIEW_NOT_FOUND", "1")
		shipCIPollInterval = 0

		out, _, err := runShipCmdFull(t, "-m", "fix: frobnicate", "--reviews")
		if err != nil {
			t.Fatalf("ship error = %v", err)
		}
		summaryIdx := strings.Index(out, `committed a1b2c3d "fix: frobnicate" · pushed main → origin · CI success`)
		notFoundIdx := strings.Index(out, "reviews: no open PR for main")
		if summaryIdx < 0 || notFoundIdx < 0 {
			t.Fatalf("stdout missing expected lines:\n%s", out)
		}
		if notFoundIdx < summaryIdx {
			t.Errorf("the ship report must print before the reviews no-PR note:\n%s", out)
		}
		if !strings.HasSuffix(strings.TrimRight(out, "\n"), "reviews: no open PR for main") {
			t.Errorf("the reviews no-PR note must be the last line:\n%s", out)
		}
	})

	t.Run("gt lane watches every downstack branch", func(t *testing.T) {
		log := setupShipGT(t, true)
		t.Setenv("GIT_BRANCH", "feature2")
		t.Setenv("GT_STATE_JSON", `{"main":{"trunk":true},"feature":{"parents":[{"ref":"main","sha":"deadbeef"}]},`+
			`"feature2":{"parents":[{"ref":"feature","sha":"beadfeed"}]}}`)
		t.Setenv("GH_RUN_LIST_JSON", fakeRunListJSON)
		t.Setenv("GH_RUN_VIEW_JSON", fakeRunViewSuccess)
		t.Setenv("GH_PR_VIEW_NOT_FOUND", "1")
		shipCIPollInterval = 0

		out, _, err := runShipCmdFull(t, "-m", "fix: frobnicate", "--reviews")
		if err != nil {
			t.Fatalf("ship error = %v", err)
		}
		var prViewBranches []string
		for _, inv := range readInvocations(t, log) {
			if len(inv) > 3 && inv[0] == "gh" && inv[1] == "pr" && inv[2] == "view" {
				prViewBranches = append(prViewBranches, inv[3])
			}
		}
		// gtPRSegment resolves feature2 first, then shipWatchReviews resolves the
		// whole downstack, feature2 first.
		want := []string{"feature2", "feature2", "feature"}
		if !reflect.DeepEqual(prViewBranches, want) {
			t.Errorf("gh pr view branches = %v, want %v", prViewBranches, want)
		}
		for _, w := range []string{"reviews: no open PR for feature2", "reviews: no open PR for feature"} {
			if !strings.Contains(out, w) {
				t.Errorf("stdout %q missing %q", out, w)
			}
		}
	})

	t.Run("red CI plus clean reviews watch preserves the CI error", func(t *testing.T) {
		setupShipGT(t, true)
		t.Setenv("GH_RUN_LIST_JSON", fakeRunListJSON)
		t.Setenv("GH_RUN_VIEW_JSON", fakeRunViewFailure)
		t.Setenv("GH_WATCH_EXIT", "1")
		t.Setenv("GH_LOG_FAILED", "go test failed\n")
		t.Setenv("GH_PR_VIEW_NOT_FOUND", "1")
		shipCIPollInterval = 0

		_, _, err := runShipCmdFull(t, "-m", "fix: frobnicate", "--reviews")
		if err == nil {
			t.Fatal("expected a non-nil error from the failed CI run")
		}
		if !strings.Contains(err.Error(), "ship: CI failed for 1 run(s) on the pushed commit") {
			t.Errorf("error = %q, want it to preserve the CI failure", err.Error())
		}
	})
}
