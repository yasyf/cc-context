package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/gtmeta"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcstest"
)

// dropSeed is one pull request the fake GitHub starts out holding.
type dropSeed struct {
	number int
	state  string
	base   string
}

// dropGH is a fake GitHub that carries state between calls, because that is the
// only way this failure is reachable: the close a drop must not cause happens
// when git deletes a ref, several calls after the gh that could have prevented
// it.
type dropGH struct {
	t   *testing.T
	dir string
}

// installDropGH overwrites the fixture's own gh and git shims with a stateful
// gh and a git wrapper. The wrapper models the one GitHub behaviour this verb
// exists for: deleting a branch closes every pull request based on it.
func installDropGH(t *testing.T, f *vcstest.Fixture, seeds map[string]dropSeed) *dropGH {
	t.Helper()
	gh := &dropGH{t: t, dir: t.TempDir()}
	for _, sub := range []string{"pr", "branch", "deleted"} {
		if err := os.MkdirAll(filepath.Join(gh.dir, sub), 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", sub, err)
		}
	}
	for branch, seed := range seeds {
		gh.write(filepath.Join("pr", fmt.Sprint(seed.number)), seed.state+" "+seed.base)
		gh.write(filepath.Join("branch", branch), fmt.Sprint(seed.number))
	}
	t.Setenv("DROP_GH", gh.dir)
	writeShipExecutable(t, f.ShimBin, "gh", "#!/bin/sh\n"+vcstest.RecordArgv("gh")+dropGHBody)

	realBin := shipDisplaceShim(t, f, "git")
	writeShipExecutable(t, f.ShimBin, "git", "#!/bin/sh\n"+dropGitBody+"exec '"+realBin+"' \"$@\"\n")
	return gh
}

func (g *dropGH) write(rel, body string) {
	g.t.Helper()
	if err := os.WriteFile(filepath.Join(g.dir, rel), []byte(body+"\n"), 0o600); err != nil {
		g.t.Fatalf("write %s: %v", rel, err)
	}
}

// pr reads one pull request back as "<state> <base>", which is the whole of
// what a drop writes to GitHub.
func (g *dropGH) pr(number int) string {
	g.t.Helper()
	data, err := os.ReadFile(filepath.Join(g.dir, "pr", fmt.Sprint(number)))
	if err != nil {
		g.t.Fatalf("read PR %d: %v", number, err)
	}
	return strings.TrimSpace(string(data))
}

func (g *dropGH) markDeleted(ref string) {
	g.t.Helper()
	g.write(filepath.Join("deleted", ref), "")
}

// dropGHBody answers the four REST calls a drop issues out of the state
// directory, and refuses the two things GitHub itself refuses: changing a
// closed pull request's base, and reopening one whose base ref is gone.
const dropGHBody = `S=$DROP_GH
field() { key=$1; shift; while [ $# -gt 0 ]; do case "$1" in "$key"=*) printf '%s' "${1#*=}"; return 0 ;; esac; shift; done; }
render() {
  read -r st bs < "$S/pr/$1"
  case "$st" in OPEN) rest=open ;; *) rest=closed ;; esac
  merged=null; if [ "$st" = MERGED ]; then merged='"2026-09-01T00:00:00Z"'; fi
  printf '{"number":%s,"html_url":"https://github.com/yasyf/cc-context/pull/%s","state":"%s","merged_at":%s,"base":{"ref":"%s"}}' "$1" "$1" "$rest" "$merged" "$bs"
}
if [ "$1" != api ]; then printf 'fake gh: unmatched argv: %s\n' "$*" >&2; exit 2; fi
method=GET path= prev=
for a in "$@"; do
  case "$a" in repos/*) path=$a ;; esac
  if [ "$prev" = -X ]; then method=$a; fi
  prev=$a
done
case "$method $path" in
  "GET "*/pulls)
    head=$(field head "$@")
    branch=${head#*:}
    want=$(field state "$@")
    if [ ! -r "$S/branch/$branch" ]; then printf '[]'; exit 0; fi
    read -r n < "$S/branch/$branch"
    read -r st bs < "$S/pr/$n"
    if [ "$want" = open ] && [ "$st" != OPEN ]; then printf '[]'; exit 0; fi
    printf '['; render "$n"; printf ']' ;;
  "GET "*/pulls/*) render "${path##*/}" ;;
  "PATCH "*/pulls/*)
    n=${path##*/}
    read -r st bs < "$S/pr/$n"
    base=$(field base "$@")
    if [ -n "$base" ]; then
      if [ "$st" != OPEN ]; then
        printf 'gh: Cannot change the base branch of a closed pull request. (HTTP 422)\n' >&2
        exit 1
      fi
      if [ -z "$DROP_GH_EDIT_NOOP" ]; then printf '%s %s\n' "$st" "$base" > "$S/pr/$n"; fi
    fi
    if [ "$(field state "$@")" = open ]; then
      if [ -e "$S/deleted/$bs" ]; then
        printf 'gh: Validation Failed (HTTP 422)\n' >&2
        exit 1
      fi
      printf 'OPEN %s\n' "$bs" > "$S/pr/$n"
    fi ;;
  *) printf 'fake gh: unmatched argv: %s\n' "$*" >&2; exit 2 ;;
esac
exit 0
`

// dropGitBody is the wrapper ahead of the fixture's git: a deleted ref closes
// every pull request based on it, and a re-pushed one is reachable again. It
// runs only at depth 0, so the git calls the fixture's own tools make are left
// alone.
const dropGitBody = `if [ -z "$CCX_SHIM_DEPTH" ] && [ -n "$DROP_GH" ] && [ "$1 $2 $3 $4" = "push --no-follow-tags --quiet origin" ]; then
  gone= back= take=
  for a in "$@"; do
    if [ -n "$take" ]; then gone=$a; take=; continue; fi
    case "$a" in
      --delete) take=1 ;;
      *:refs/heads/*) back=${a#*:refs/heads/} ;;
    esac
  done
  if [ -n "$gone" ]; then
    : > "$DROP_GH/deleted/$gone"
    for p in "$DROP_GH"/pr/*; do
      read -r st bs < "$p"
      if [ "$bs" = "$gone" ]; then printf 'CLOSED %s\n' "$bs" > "$p"; fi
    done
  fi
  if [ -n "$back" ]; then rm -f "$DROP_GH/deleted/$back"; fi
fi
`

// dropStep reports where the first invocation carrying every token in want sits
// in the recorded argv, and fails when there is none: an ordering claim over a
// step that never ran holds whatever the order was.
func dropStep(t *testing.T, invocations [][]string, want ...string) int {
	t.Helper()
	for i, inv := range invocations {
		matched := 0
		for _, token := range want {
			for _, arg := range inv {
				if arg == token {
					matched++
					break
				}
			}
		}
		if matched == len(want) {
			return i
		}
	}
	t.Fatalf("no invocation carrying %v; recorded:\n%v", want, invocations)
	return 0
}

// dropStack builds a gt stack whose branches all sit on origin, leaves the
// working copy on the bottom one so nothing holds the branch under test, and
// opens the argv log empty.
func dropStack(t *testing.T, names ...string) *vcstest.Fixture {
	t.Helper()
	f := shipGTRepo(t, vcstest.GTStack(names...))
	mustRun(t, f.Env(), f.Dir, "git", append([]string{"push", "-q", "origin"}, names...)...)
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", names[0])
	shipResetLog(t, f)
	return f
}

func dropGTParent(t *testing.T, f *vcstest.Fixture, branch string) string {
	t.Helper()
	state, err := gtStateQuery(t.Context(), render.Dir(f.Dir), "test")
	if err != nil {
		t.Fatalf("gt state: %v", err)
	}
	s, tracked := state[branch]
	if !tracked || len(s.Parents) == 0 {
		t.Fatalf("gt state has no parent for %s: %v", branch, state)
	}
	return s.Parents[0].Ref
}

// TestStackDropRetargetsEveryChildBeforeDeletingTheBranch is the ordering guard
// this verb exists to be. GitHub closes every pull request whose base branch is
// deleted, so a drop that deletes first loses the child's pull request with
// nothing warning — gt delete reports success and the local stack looks right.
func TestStackDropRetargetsEveryChildBeforeDeletingTheBranch(t *testing.T) {
	f := dropStack(t, "base", "mid", "top")
	gh := installDropGH(t, f, map[string]dropSeed{
		"mid": {number: 1, state: "OPEN", base: "base"},
		"top": {number: 2, state: "OPEN", base: "mid"},
	})

	out, _, err := runStackCmd(t, f, "drop", "mid")
	if err != nil {
		t.Fatalf("stack drop: %v", err)
	}

	invocations := shipGTInvocations(t, f)
	retarget := dropStep(t, invocations, "gh", "api", "PATCH", "repos/yasyf/cc-context/pulls/2", "base=base")
	deleted := dropStep(t, invocations, "git", "push", "--delete", "mid")
	if retarget > deleted {
		t.Errorf("retargeted top's PR at step %d, after the delete at step %d — GitHub closes a PR whose base goes first", retarget, deleted)
	}
	if got := gh.pr(2); got != "OPEN base" {
		t.Errorf("top's PR = %q, want %q", got, "OPEN base")
	}
	if gitBranchExists(t, f.Env(), f.Dir, "mid") {
		t.Error("mid is still a local branch")
	}
	if gitBranchExists(t, f.Env(), f.RemoteDir, "mid") {
		t.Error("origin still carries mid")
	}
	if parent := dropGTParent(t, f, "top"); parent != "base" {
		t.Errorf("gt records top's parent as %q, want base", parent)
	}
	if subjects := gitAt(t, f.Env(), f.Dir, "log", "--format=%s", "base..top"); subjects != "top" {
		t.Errorf("base..top = %q, want top alone — the child keeps the dropped commit", subjects)
	}
	if !strings.Contains(out, "retargeted 1 onto base") {
		t.Errorf("report = %q, want it to name the retarget", out)
	}
}

// TestStackDropRefusesARetargetThatDidNotTake pins the gate between the two
// halves: the retarget is read back before anything is deleted, so a move
// GitHub did not make costs a refusal rather than the child's pull request.
func TestStackDropRefusesARetargetThatDidNotTake(t *testing.T) {
	f := dropStack(t, "base", "mid", "top")
	gh := installDropGH(t, f, map[string]dropSeed{
		"mid": {number: 1, state: "OPEN", base: "base"},
		"top": {number: 2, state: "OPEN", base: "mid"},
	})
	t.Setenv("DROP_GH_EDIT_NOOP", "1")

	_, _, err := runStackCmd(t, f, "drop", "mid")
	if err == nil {
		t.Fatal("stack drop succeeded over a retarget that did not take")
	}
	if !strings.Contains(err.Error(), "still targets mid") {
		t.Errorf("error = %v, want it to name the base that did not move", err)
	}
	if got := gh.pr(2); got != "OPEN mid" {
		t.Errorf("top's PR = %q, want it untouched at %q", got, "OPEN mid")
	}
	if !gitBranchExists(t, f.Env(), f.Dir, "mid") {
		t.Error("mid was deleted locally after the verification refused")
	}
	if !gitBranchExists(t, f.Env(), f.RemoteDir, "mid") {
		t.Error("origin lost mid after the verification refused")
	}
}

// TestStackDropRepairWalksTheOrderGitHubAllows pins the recovery. A closed pull
// request's base cannot be changed and a reopen needs a base that exists, so
// the ref goes back first, the reopen and the retarget happen while it does,
// and it goes again last.
func TestStackDropRepairWalksTheOrderGitHubAllows(t *testing.T) {
	f := dropStack(t, "base", "top")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "top")
	gh := installDropGH(t, f, map[string]dropSeed{
		"top": {number: 2, state: "CLOSED", base: "mid"},
	})
	gh.markDeleted("mid")
	shipResetLog(t, f)

	out, _, err := runStackCmd(t, f, "drop", "--repair")
	if err != nil {
		t.Fatalf("stack drop --repair: %v", err)
	}

	// The resurrection is addressed by its refspec, not by "git push origin":
	// the re-delete carries that too, and a step matched by both orders proves
	// nothing about either.
	invocations := shipGTInvocations(t, f)
	pushed := dropStep(t, invocations, "git", "push", "origin", "refs/heads/base:refs/heads/mid")
	reopened := dropStep(t, invocations, "gh", "api", "PATCH", "repos/yasyf/cc-context/pulls/2", "state=open")
	retargeted := dropStep(t, invocations, "gh", "api", "PATCH", "repos/yasyf/cc-context/pulls/2", "base=base")
	redeleted := dropStep(t, invocations, "git", "push", "--delete", "mid")
	if pushed >= reopened || reopened >= retargeted || retargeted >= redeleted {
		t.Errorf("steps ran at push=%d reopen=%d retarget=%d delete=%d, want that order", pushed, reopened, retargeted, redeleted)
	}
	if got := gh.pr(2); got != "OPEN base" {
		t.Errorf("top's PR = %q, want %q", got, "OPEN base")
	}
	if gitBranchExists(t, f.Env(), f.RemoteDir, "mid") {
		t.Error("origin still carries the resurrected mid")
	}
	if !strings.Contains(out, "#2 mid → base") {
		t.Errorf("report = %q, want it to name the move", out)
	}
}

// TestStackDropRepairRefusesBeforeDeletingTheRefBack pins the repair's own
// read-back. The resurrected ref is the one thing holding the pull request
// open, so deleting it while a retarget has not taken closes the pull request
// the repair was called to reopen.
func TestStackDropRepairRefusesBeforeDeletingTheRefBack(t *testing.T) {
	f := dropStack(t, "base", "top")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "top")
	gh := installDropGH(t, f, map[string]dropSeed{
		"top": {number: 2, state: "CLOSED", base: "mid"},
	})
	gh.markDeleted("mid")
	t.Setenv("DROP_GH_EDIT_NOOP", "1")

	_, _, err := runStackCmd(t, f, "drop", "--repair")
	if err == nil {
		t.Fatal("stack drop --repair succeeded over a retarget that did not take")
	}
	if !strings.Contains(err.Error(), "finish with: gh api -X PATCH repos/yasyf/cc-context/pulls/2 --silent -f base=base") {
		t.Errorf("error = %v, want it to name the state and the commands that finish it", err)
	}
	if got := gh.pr(2); got != "OPEN mid" {
		t.Errorf("top's PR = %q, want it left open on the resurrected ref", got)
	}
	if !gitBranchExists(t, f.Env(), f.RemoteDir, "mid") {
		t.Error("origin lost the resurrected mid, which is what was holding the PR open")
	}
}

func TestStackDropReplaysAChildGTRecordsAsCarryingTheDroppedCommits(t *testing.T) {
	f := dropStack(t, "base", "mid", "top")
	installDropGH(t, f, map[string]dropSeed{
		"mid": {number: 1, state: "OPEN", base: "base"},
		"top": {number: 2, state: "OPEN", base: "mid"},
	})
	commonDir, err := gtCommonDir(t.Context(), render.Dir(f.Dir), "test")
	if err != nil {
		t.Fatalf("gt common dir: %v", err)
	}
	base := gitAt(t, f.Env(), f.Dir, "rev-parse", "refs/heads/base")
	if err := gtmeta.RecordRestacked(t.Context(), commonDir, map[string]string{"top": base}); err != nil {
		t.Fatalf("record top as restacked onto base: %v", err)
	}

	if _, _, err := runStackCmd(t, f, "drop", "mid"); err != nil {
		t.Fatalf("stack drop: %v", err)
	}
	if subjects := gitAt(t, f.Env(), f.Dir, "log", "--format=%s", "base..top"); subjects != "top" {
		t.Errorf("base..top = %q, want top alone — mid's commit left the stack with mid", subjects)
	}
	if gitBranchExists(t, f.Env(), f.Dir, "mid") {
		t.Error("mid is still a local branch")
	}
}

func TestStackDropReplaysABranchReparentedPastTheDroppedOne(t *testing.T) {
	f := dropStack(t, "landed", "feature")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "main")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "--delete", "landed")
	installDropGH(t, f, map[string]dropSeed{"feature": {number: 2, state: "OPEN", base: "main"}})
	commonDir, err := gtCommonDir(t.Context(), render.Dir(f.Dir), "test")
	if err != nil {
		t.Fatalf("gt common dir: %v", err)
	}
	if err := gtmeta.Reparent(t.Context(), commonDir, map[string]string{"feature": "main"}); err != nil {
		t.Fatalf("reparent feature onto main: %v", err)
	}
	main := gitAt(t, f.Env(), f.Dir, "rev-parse", "refs/heads/main")
	if err := gtmeta.RecordRestacked(t.Context(), commonDir, map[string]string{"feature": main}); err != nil {
		t.Fatalf("record feature as restacked onto main: %v", err)
	}
	landed := gitAt(t, f.Env(), f.Dir, "rev-parse", "refs/heads/landed")

	if _, _, err := runStackCmd(t, f, "drop", "landed"); err != nil {
		t.Fatalf("stack drop: %v", err)
	}
	if stackOnto(t, f, landed, "feature") {
		t.Errorf("feature still carries landed's commit %s", landed)
	}
	if subjects := gitAt(t, f.Env(), f.Dir, "log", "--format=%s", "main..feature"); subjects != "feature" {
		t.Errorf("main..feature = %q, want feature alone", subjects)
	}
	if parent := dropGTParent(t, f, "feature"); parent != "main" {
		t.Errorf("gt records feature's parent as %q, want main", parent)
	}
	if gitBranchExists(t, f.Env(), f.Dir, "landed") {
		t.Error("landed is still a local branch")
	}
}

// TestStackDropRefusesTheBranchAWorkingCopyHolds pins the one refusal that
// comes before any lookup: git deletes no branch a checkout has out, so a drop
// that tried would fail after retargeting every child.
func TestStackDropRefusesTheBranchAWorkingCopyHolds(t *testing.T) {
	f := shipGTRepo(t, vcstest.GTStack("base", "mid"))

	_, _, err := runStackCmd(t, f, "drop", "mid")
	if err == nil {
		t.Fatal("stack drop succeeded on the branch this checkout holds")
	}
	if !strings.Contains(err.Error(), "is checked out in") || !strings.Contains(err.Error(), "switch that one to base") {
		t.Errorf("error = %v, want it to name the holder and the parent to switch to", err)
	}
}

// TestStackDropDryRunMutatesNothing pins that the plan is read-only: it names
// the retarget and touches neither GitHub nor a ref.
func TestStackDropDryRunMutatesNothing(t *testing.T) {
	f := dropStack(t, "base", "mid", "top")
	gh := installDropGH(t, f, map[string]dropSeed{
		"mid": {number: 1, state: "OPEN", base: "base"},
		"top": {number: 2, state: "OPEN", base: "mid"},
	})

	out, _, err := runStackCmd(t, f, "drop", "mid", "--dry-run")
	if err != nil {
		t.Fatalf("stack drop --dry-run: %v", err)
	}
	if !strings.Contains(out, "would drop mid") || !strings.Contains(out, "would retarget 1 onto base") {
		t.Errorf("report = %q, want it to name the drop and the retarget it would make", out)
	}
	if got := gh.pr(2); got != "OPEN mid" {
		t.Errorf("top's PR = %q, want it untouched at %q", got, "OPEN mid")
	}
	if !gitBranchExists(t, f.Env(), f.Dir, "mid") || !gitBranchExists(t, f.Env(), f.RemoteDir, "mid") {
		t.Error("the dry run deleted mid")
	}
	if parent := dropGTParent(t, f, "top"); parent != "mid" {
		t.Errorf("gt records top's parent as %q, want mid — the dry run rewrote a row", parent)
	}
}

// TestStackDropRefusesTrunk pins the scope: trunk is every stack's floor, and a
// drop of it would retarget and delete the branch everything else sits on.
func TestStackDropRefusesTrunk(t *testing.T) {
	f := dropStack(t, "base")

	_, _, err := runStackCmd(t, f, "drop", "main")
	if err == nil {
		t.Fatal("stack drop succeeded on trunk")
	}
	if !strings.Contains(err.Error(), "is trunk") {
		t.Errorf("error = %v, want it to name trunk", err)
	}
}
