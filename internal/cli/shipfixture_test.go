// Shared fixture layer for the ship test suite: the fake jj/git/gt/gh
// executables that record their argv, the setup and run helpers every
// ship-adjacent test file builds on, and the readers and assertions those tests
// check the recorded argv against.
package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/vcstest"
)

const fakeHeadSHA = "abcdef0123456789abcdef0123456789abcdef01"

// ghPkgDir is this package's source directory, captured before any fixture
// chdirs into its own repository and out of reach of the golden corpus's
// relative path.
var ghPkgDir = func() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		panic("cli: cannot locate the package source directory")
	}
	return filepath.Dir(file)
}()

// ghStdout replays one recorded gh run's stdout. Every byte a fake gh hands
// back is a byte real gh printed; the scenarios live in testdata/gh/cli.
func ghStdout(t *testing.T, scenario string) string {
	t.Helper()
	path := filepath.Join(ghPkgDir, ghGoldenDir, "cli", scenario+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("golden %s: %v", scenario, err)
	}
	var rec struct {
		Stdout string `json:"stdout"`
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("golden %s: %v", scenario, err)
	}
	return rec.Stdout
}

// fakeRunListJSON is the one CI payload still hand-modeled: the corpus holds a
// four-run list and an empty one, and no ship test is about watching four runs
// at once. Recording a single-run list needs a commit that triggered exactly
// one workflow, captured with
// `gh run list --commit <sha> --limit 50 --json databaseId,workflowName,status,url`.
const fakeRunListJSON = `[{"databaseId":42,"workflowName":"ci","status":"in_progress","url":"https://github.com/x/actions/runs/42"}]`

// shipRepo builds a real repository per opts behind vcstest's recording shim
// and seeds its lane cache, so a ship under test drives git and jj themselves
// and every byte it parses is one those tools produced.
func shipRepo(t *testing.T, opts ...vcstest.Opt) *vcstest.Fixture {
	t.Helper()
	f := vcstest.Repo(t, opts...)
	seedLaneRecords(t, f.Dir, laneSeed{})
	return f
}

// gitAt reads repository state back out of dir, trimmed. It resolves through
// the shim like any other call, so a test asserting invocations reads the log
// before its state assertions.
func gitAt(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return strings.TrimSpace(mustRun(t, dir, "git", args...))
}

// remoteCount counts branch's commits in f's bare origin — the measure of
// whether a push actually landed.
func remoteCount(t *testing.T, f *vcstest.Fixture, branch string) int {
	t.Helper()
	n, err := strconv.Atoi(gitAt(t, f.Dir, "--git-dir="+f.RemoteDir, "rev-list", "--count", branch))
	if err != nil {
		t.Fatalf("count %s in %s: %v", branch, f.RemoteDir, err)
	}
	return n
}

// gitBranchExists reports whether dir carries a local branch by that name.
func gitBranchExists(t *testing.T, dir, branch string) bool {
	t.Helper()
	return gitAt(t, dir, "branch", "--list", "--format=%(refname:short)", branch) != ""
}

// writeShipUvx installs a uvx that fails its first n prek runs and passes
// after, the timing window shipRunHooks' autofix retry turns on. No repository
// state can express "the hook fails this time and not the next".
func writeShipUvx(t *testing.T, dir string, n int) {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "prek.marker")
	if err := os.WriteFile(marker, []byte(strconv.Itoa(n)), 0o600); err != nil {
		t.Fatalf("write prek marker: %v", err)
	}
	t.Setenv("SHIP_PREK_MARKER", marker)
	script := `#!/bin/sh
count=$(cat "$SHIP_PREK_MARKER")
if [ "$count" -gt 0 ]; then
  printf '%s' "$((count - 1))" > "$SHIP_PREK_MARKER"
  printf 'files were modified by this hook\n' >&2
  exit 1
fi
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "uvx"), []byte(script), 0o700); err != nil { //nolint:gosec // a PATH entry must be owner-executable
		t.Fatalf("write uvx: %v", err)
	}
}

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

	jj := "#!/bin/sh\n" + log("jj") + `if [ "$1" = --ignore-working-copy ]; then shift; fi
case "$*" in
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
      if [ "$count" -gt 1 ]; then diverged=1; fi
    fi
    if [ -z "$diverged" ]; then printf '"x"\n'; fi ;;
  *"bookmarks(exact"*)
    heads=${JJ_BOOKMARK_HEADS:-1}
    # A bookmark ship just created resolves from then on, whatever the knob says.
    if [ -e "$SHIP_LOG.jj-bookmark-created" ]; then heads=1; fi
    case "$heads" in
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
  *remote_bookmarks*)
    # JJ_TRUNK_NAMES_FILTERED answers the template that drops the @git
    # pseudo-remote, so a test can tell the two templates apart.
    names=${JJ_TRUNK_NAMES-main main}
    case "$*" in *'!= "git"'*) names=${JJ_TRUNK_NAMES_FILTERED-$names} ;; esac
    for b in $names; do printf '"%s"\n' "$b"; done ;;
  *local_bookmarks*)
    # escape_json() renders one JSON string per line; the names a test names are
    # plain enough that quoting them is the whole encoding.
    if [ -z "$JJ_NO_BOOKMARK" ]; then
      for b in ${JJ_BOOKMARK_NAMES:-main}; do printf '"%s"\n' "$b"; done
    fi ;;
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
  "bookmark create"*) : > "$SHIP_LOG.jj-bookmark-created" ;;
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
    if [ -n "$GIT_BRANCH_SHOW_FAIL" ]; then printf 'fatal: not a git repository\n' >&2; exit 128; fi
    branch=${GIT_BRANCH-main}
    if [ -n "$GIT_BRANCH_AFTER_COMMIT" ] && [ -e "$SHIP_LOG.git-committed" ]; then branch=$GIT_BRANCH_AFTER_COMMIT; fi
    if [ -r "$SHIP_LOG.git-switched" ]; then IFS= read -r branch < "$SHIP_LOG.git-switched" || :; fi
    if [ -n "$GIT_DETACHED_AFTER_COMMIT" ] && [ -e "$SHIP_LOG.git-committed" ] && [ ! -e "$SHIP_LOG.git-healed" ]; then branch=; fi
    printf '%s\n' "$branch" ;;
  "symbolic-ref --short")
    if [ -z "$GIT_TRUNK" ]; then exit 1; fi
    printf 'origin/%s\n' "$GIT_TRUNK" ;;
  "switch -c") printf '%s\n' "$3" > "$SHIP_LOG.git-switched" ;;
  "switch "*)
    if [ -n "$GIT_SWITCH_BACK_FAIL" ]; then printf 'fatal: invalid reference: %s\n' "$2" >&2; exit 1; fi
    printf '%s\n' "$2" > "$SHIP_LOG.git-switched" ;;
  "branch -D") : ;;
  "checkout -B")
    if [ -n "$GIT_CHECKOUT_B_FAIL" ]; then printf "fatal: '%s' is already used by worktree at '%s'\n" "$3" "$GIT_CHECKOUT_B_FAIL" >&2; exit 1; fi
    printf '%s\n' "$3" > "$SHIP_LOG.git-switched"; : > "$SHIP_LOG.git-healed" ;;
  "commit "*)
    if [ -n "$GIT_COMMIT_FAIL" ]; then printf 'fatal: commit refused\n' >&2; exit 1; fi
    : > "$SHIP_LOG.git-committed" ;;
  "rev-parse HEAD") printf '%s' '` + fakeHeadSHA + `' ;;
  "rev-parse --show-toplevel") printf '%s' "$SHIP_FAKE_ROOT" ;;
  "show --end-of-options") printf '%s' "$GIT_FILE_SHOW_BASE" ;;
  "ls-tree --full-tree") printf '100644 blob 1111111111111111111111111111111111111111\t%s\n' "$5" ;;
  "hash-object -w") printf '%s' '2222222222222222222222222222222222222222' ;;
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
  "config --get")
    case "$3" in
      ccx.nogt) if [ -n "$GIT_CONFIG_CCX_NOGT" ]; then printf '%s\n' "$GIT_CONFIG_CCX_NOGT"; else exit 1; fi ;;
      branch.*.remote) if [ -n "$GIT_BRANCH_REMOTE" ]; then printf '%s\n' "$GIT_BRANCH_REMOTE"; else exit 1; fi ;;
      *) printf 'fake git: unmatched config key: %s\n' "$3" >&2; exit 2 ;;
    esac ;;
  fetch*) if [ -n "$GIT_FETCH_FAIL" ]; then printf 'git: cannot reach origin\n' >&2; exit 1; fi ;;
  "rev-parse --verify")
    case "$4" in
      REBASE_HEAD) if [ -n "$GIT_REBASE_CONFLICT" ]; then exit 0; else exit 1; fi ;;
      *) if [ -n "$GIT_REMOTE_REF_MISSING" ]; then exit 1; fi ;;
    esac ;;
  "merge-base --is-ancestor")
    if [ -n "$GIT_ANCESTOR_EXIT" ]; then printf 'fatal: not a valid object name\n' >&2; exit "$GIT_ANCESTOR_EXIT"; fi
    if [ -n "$GIT_DIVERGED" ]; then exit 1; fi
    if [ -n "$GIT_DIVERGED_MARKER" ]; then
      count=0
      if [ -r "$GIT_DIVERGED_MARKER" ]; then IFS= read -r count < "$GIT_DIVERGED_MARKER" || :; fi
      count=${count:-0}
      count=$((count + 1))
      printf '%s' "$count" > "$GIT_DIVERGED_MARKER"
      if [ "$count" -gt 1 ]; then exit 1; fi
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
  "--git-dir "*)
    case "$3" in
      worktree)
        # git worktree list --porcelain -z frames each record as NUL-terminated
        # "key value" lines closed by one extra NUL, measured off git 2.51.
        # $GIT_HOLDERS is one "branch worktree" pair per line, the held branches
        # alone: git names no holder for a branch no checkout has out.
        printf '%s' "$GIT_HOLDERS" | while IFS=' ' read -r name path || [ -n "$name" ]; do
          if [ -n "$name" ]; then printf 'worktree %s\0HEAD ` + fakeHeadSHA + `\0branch refs/heads/%s\0\0' "$path" "$name"; fi
        done ;;
      *) printf 'fake git: unmatched --git-dir argv: %s\n' "$*" >&2; exit 2 ;;
    esac ;;
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
	gt := "#!/bin/sh\n" + gitIdxMark + log("gt") + `case "$1" in
  auth)
    if [ -n "$GT_AUTH_STDERR" ]; then printf '%s\n' "$GT_AUTH_STDERR" >&2; fi
    # exec so the ctx kill reaps the sleep too: a surviving grandchild would hold
    # the stdout pipe open and block Run past its deadline. PATH is the fake bin
    # dir alone, so sleep needs its absolute path.
    if [ -n "$GT_AUTH_HANG" ]; then exec /bin/sleep 30; fi
    printf '%s\n' "${GT_AUTH_STDOUT-✅ Ready to submit PRs to github.com/yasyf/cc-context}"
    exit "${GT_AUTH_EXIT:-0}" ;;
  state)
    if [ -n "$GT_STATE_STDERR" ]; then printf '%s\n' "$GT_STATE_STDERR" >&2; fi
    if [ -n "$GT_STATE_JSON_MARKER" ]; then
      count=0
      if [ -r "$GT_STATE_JSON_MARKER" ]; then IFS= read -r count < "$GT_STATE_JSON_MARKER" || :; fi
      count=${count:-0}
      count=$((count + 1))
      printf '%s' "$count" > "$GT_STATE_JSON_MARKER"
      if [ "$count" -gt 1 ]; then printf '%s' "$GT_STATE_JSON_2"
      else printf '%s' "$GT_STATE_JSON"; fi
    else
      printf '%s' "$GT_STATE_JSON"
    fi ;;
  add)
    if [ -n "$GT_ADD_STDERR" ]; then printf '%s\n' "$GT_ADD_STDERR" >&2; fi ;;
  track)
    if [ -n "$GT_TRACK_STDERR" ]; then printf '%s\n' "$GT_TRACK_STDERR" >&2; fi
    if [ -n "$GT_TRACK_FAIL" ]; then printf 'gt: track failed\n' >&2; exit 1; fi ;;
  create)
    printf '%s\n' "$2" > "$SHIP_LOG.git-switched"
    : > "$SHIP_LOG.git-committed" ;;
  modify)
    if [ -n "$GT_MODIFY_STDERR" ]; then printf '%s\n' "$GT_MODIFY_STDERR" >&2; fi
    : > "$SHIP_LOG.git-committed" ;;
  submit)
    # Real gt splits one submit's report across both streams and exits 0 on some
    # of the failures it reports, so the fake drives stream and exit code apart.
    if [ -n "$GT_SUBMIT_STDOUT" ]; then printf '%s\n' "$GT_SUBMIT_STDOUT"; fi
    if [ -n "$GT_SUBMIT_FAIL_STDERR" ]; then printf '%s\n' "$GT_SUBMIT_FAIL_STDERR" >&2; exit "${GT_SUBMIT_EXIT:-1}"; fi
    exit "${GT_SUBMIT_EXIT:-0}" ;;
  *) printf 'fake gt: unmatched argv: %s\n' "$*" >&2; exit 2 ;;
esac
exit 0
`
	gh := "#!/bin/sh\n" + log("gh") + `case "$1 $2" in
  "repo view") printf '%s' "$GH_REPO_VIEW_JSON" ;;
  "api graphql")
    case "$*" in
      *pullRequests*)
        # One aliased field per branch, off the same GH_PR_VIEW_* fixtures the
        # per-branch gh pr view read; an empty node set is a branch with no PR.
        printf '{"data":{"repository":{'
        sep=
        for a in "$@"; do
          case "$a" in b[0-9]*=*) ;; *) continue ;; esac
          branch=${a#*=}
          node=
          if [ -z "$GH_PR_VIEW_NOT_FOUND" ]; then
            if [ -n "$GH_PR_VIEW_DIR" ] && [ -r "$GH_PR_VIEW_DIR/$branch" ]; then
              # One node per line, oldest first, the order GitHub's CREATED_AT
              # sorts by; a first:1 query lands on the end the direction names.
              first= last=
              while IFS= read -r line || [ -n "$line" ]; do
                [ -n "$line" ] || continue
                [ -n "$first" ] || first=$line
                last=$line
              done < "$GH_PR_VIEW_DIR/$branch"
              node=$first
              case "$*" in *"direction: DESC"*) node=$last ;; esac
            else
              node=$GH_PR_VIEW_JSON
            fi
          fi
          printf '%s"%s":{"nodes":[%s]}' "$sep" "${a%%=*}" "$node"
          sep=,
        done
        printf '}}}' ;;
      *) printf '%s' "$GH_VIEWER_GOLDEN" ;;
    esac ;;
  "pr view")
    if [ -n "$GH_PR_VIEW_NOT_FOUND" ]; then
      printf 'no pull requests found for branch "%s"\n' "$3" >&2
      exit 1
    fi
    if [ -n "$GH_PR_VIEW_DIR" ] && [ -r "$GH_PR_VIEW_DIR/$3" ]; then
      IFS= read -r branchpr < "$GH_PR_VIEW_DIR/$3" || :
      printf '%s' "$branchpr"
      exit 0
    fi
    printf '%s' "$GH_PR_VIEW_JSON" ;;
  "pr list") printf '%s' "${GH_PR_LIST_JSON:-[]}" ;;
  "pr create") printf '%s\n' "$GH_PR_CREATE_OUT" ;;
  "pr edit"|"pr ready") : ;;
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
  *) printf 'fake gh: unmatched argv: %s\n' "$*" >&2; exit 2 ;;
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
	// Root the cache under the test's own dir so the lane gate never reads or
	// writes the developer's real ~/Library/Caches/cc-context, then seed it: the
	// branch plan looks the repository up whenever the ship sits on trunk, and a
	// seeded record keeps that off gh for every test that is not about the gate.
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	seedLaneRecords(t, ".", laneSeed{})
	t.Setenv("JJ_DIFF_NAMES", "f.txt\n")
	t.Setenv("GH_VIEWER_GOLDEN", ghStdout(t, "viewer-graphql"))
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
	seedLaneRecords(t, ".", laneSeed{})
	return log
}

// setupShipColocated is setupShip over a colocated jj working copy — a .jj with
// a .git beside it — the shape that has a git ref namespace for sibling
// checkouts to contend over, and so the only jj shape BranchHolders can answer
// for. The re-seed is because the git dir moves the repository's cache key off
// the root.
func setupShipColocated(t *testing.T) string {
	t.Helper()
	log := setupShip(t, ".jj", true)
	if err := os.Mkdir(".git", 0o750); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	seedLaneRecords(t, ".", laneSeed{})
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

// normalizeTempPaths rewrites the temp path a "-" body file materializes stdin
// into, which is the one nondeterministic element an argv sequence can carry —
// a --pr-body-file naming a fixture is the path the test itself created.
func normalizeTempPaths(invocations [][]string) [][]string {
	for _, inv := range invocations {
		for i, arg := range inv {
			if strings.Contains(arg, "ccx-pr-body-") {
				inv[i] = "<pr-body>"
			}
		}
	}
	return invocations
}

// jjPlanArgv is the branch-plan preflight every jj-lane ship runs before any
// mutation: resolve trunk, then the bookmark nearest the working copy. A ship
// that pushes probes the resolved target on top of these.
func jjPlanArgv() [][]string {
	return [][]string{
		{"jj", "--ignore-working-copy", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
		{"jj", "--ignore-working-copy", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
	}
}

// gitTrunkArgv is the git lane's trunk read, which follows its branch read in
// every plain-git ship.
var gitTrunkArgv = []string{"git", "symbolic-ref", "--short", "refs/remotes/origin/HEAD"}

// The CI watch's three gh calls, for want-lists long enough that spelling them
// out buries the argv under test.
var (
	ghRunListArgv  = []string{"gh", "run", "list", "--commit", fakeHeadSHA, "--limit", "50", "--json", "databaseId,workflowName,status,url"}
	ghRunWatchArgv = []string{"gh", "run", "watch", "42", "--exit-status"}
	ghRunViewArgv  = []string{"gh", "run", "view", "42", "--json", "workflowName,conclusion,startedAt,updatedAt,url,jobs"}
)

// ghDownstackPRArgv is the one batched pull request lookup a gt-lane report
// makes, in place of a gh pr view per branch. branches are base-first, the order
// infoDownstack aliases them in.
func ghDownstackPRArgv(branches ...string) []string {
	argv := []string{"gh", "api", "graphql", "-F", "owner={owner}", "-F", "repo={repo}"}
	for i, b := range branches {
		argv = append(argv, "-f", fmt.Sprintf("b%d=%s", i, b))
	}
	return append(argv, "-f", "query="+downstackPRQuery(len(branches)))
}

func assertInvocations(t *testing.T, got, want [][]string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("invocation sequence mismatch\n got: %v\nwant: %v", got, want)
	}
}

// shipMutates reports whether inv moved the repository: a commit, or a branch
// cut, moved, or checked out. The reads spelled with a mutating verb — git
// branch --show-current, jj bookmark list — are not among them.
func shipMutates(inv []string) bool {
	if len(inv) < 2 {
		return false
	}
	rest := inv[2:]
	switch inv[0] + " " + inv[1] {
	case "jj commit", "jj squash", "git commit", "git switch", "git checkout":
		return true
	case "jj bookmark":
		return len(rest) > 0 && rest[0] != "list"
	case "git branch":
		return len(rest) > 0 && rest[0] != "--show-current"
	}
	return false
}

func assertNoShipMutation(t *testing.T, invocations [][]string) {
	t.Helper()
	for _, inv := range invocations {
		if shipMutates(inv) {
			t.Errorf("repository mutated before ship refused: %v", inv)
		}
	}
}

var jjIgnoresWorkingCopy = map[string]bool{
	"log":             true,
	"op log":          true,
	"root":            true,
	"file list":       true,
	"file show":       true,
	"config get":      true,
	"bookmark list":   false,
	"diff":            false,
	"commit":          false,
	"squash":          false,
	"rebase":          false,
	"bookmark create": false,
	"bookmark move":   false,
	"bookmark track":  false,
	"git fetch":       false,
	"git push":        false,
	"op revert":       false,
}

func jjVerb(argv []string) string {
	switch argv[0] {
	case "op", "file", "bookmark", "git", "config":
		return argv[0] + " " + argv[1]
	}
	return argv[0]
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

func jjRevID(t *testing.T, dir, rev string) string {
	t.Helper()
	out := mustRun(t, dir, "jj", "--ignore-working-copy", "log", "-r", rev, "--no-graph", "-T", `commit_id ++ "\n"`)
	ids := strings.Fields(out)
	if len(ids) != 1 {
		t.Fatalf("jj log -r %s resolved to %d commits, want 1: %q", rev, len(ids), out)
	}
	return ids[0]
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

// holderLookups counts the holder lookups a ship made, the measure of how lazy
// the tiebreak is: every one is a subprocess the common path must not spawn.
func holderLookups(t *testing.T, log string) int {
	t.Helper()
	n := 0
	for _, inv := range readInvocations(t, log) {
		if inv[0] == "git" && len(inv) > 3 && inv[3] == "worktree" {
			n++
		}
	}
	return n
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
