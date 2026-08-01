package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// infoFakes overlays the ship fakes with the arms ccx vcs info needs — git
// status/symbolic-ref/show-ref, the repository-wide for-each-ref that names each
// branch's holding worktree, gt --version, and the batched downstack pull
// request query, whose per-branch payload is $GH_PR_VIEW_<branch>. Each
// wrapper handles its own arms and execs the renamed base fake for the rest, so
// writeShipFakes stays untouched.
//
// GIT_BRANCH_HOLDERS is a space-separated branch=worktree list; unset means no
// checkout holds any branch, which is what git reports for a detached working
// copy and the default every report assertion here is written against.
func infoFakes(t *testing.T) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	binDir := filepath.Join(wd, "bin")

	logRec := func(name string) string {
		return "{ printf '" + name + "\\0'; for a in \"$@\"; do printf '%s\\0' \"$a\"; done; printf '\\0'; } >> \"$SHIP_LOG\"\n"
	}
	wrap := func(name, body string) bool {
		base := filepath.Join(binDir, name)
		src, err := os.ReadFile(base)
		if err != nil {
			return false
		}
		for path, content := range map[string][]byte{base + "-base": src, base: []byte(body)} {
			if err := os.WriteFile(path, content, 0o700); err != nil { //nolint:gosec // fake executable must be owner-executable
				t.Fatalf("write fake %s: %v", path, err)
			}
		}
		return true
	}

	wrap("git", "#!/bin/sh\n"+`case "$1 $2" in
  "status --porcelain")
    `+logRec("git")+`    printf '%s' "$GIT_STATUS_PORCELAIN"
    exit 0 ;;
  "symbolic-ref --short")
    `+logRec("git")+`    if [ -z "$GIT_SYMBOLIC_REF" ]; then exit 1; fi
    printf '%s\n' "$GIT_SYMBOLIC_REF"
    exit 0 ;;
  "show-ref --verify")
    `+logRec("git")+`    if [ -n "$GIT_SHOW_REF_FOUND" ]; then exit 0; fi
    exit 1 ;;
  "--git-dir "*)
    case "$3" in
      for-each-ref)
        `+logRec("git")+`        for pair in $GIT_BRANCH_HOLDERS; do
          printf '%s\0%s\n' "${pair%%=*}" "${pair#*=}"
        done
        exit 0 ;;
    esac ;;
esac
exec git-base "$@"
`)
	wrap("gt", "#!/bin/sh\n"+`case "$1" in
  --version)
    `+logRec("gt")+`    printf '%s\n' "${GT_VERSION:-1.8.6}"
    exit 0 ;;
esac
exec gt-base "$@"
`)
	wrap("gh", "#!/bin/sh\n"+`case "$1 $2" in
  "api graphql")
    case "$*" in
      *pullRequests*)
        `+logRec("gh")+`        printf '{"data":{"repository":{'
        sep=
        for a in "$@"; do
          case "$a" in b[0-9]*=*) ;; *) continue ;; esac
          eval "json=\${GH_PR_VIEW_${a#*=}-}"
          printf '%s"%s":{"nodes":[%s]}' "$sep" "${a%%=*}" "$json"
          sep=,
        done
        printf '}}}'
        exit 0 ;;
    esac ;;
esac
exec gh-base "$@"
`)

	t.Setenv("GIT_STATUS_PORCELAIN", "")
	t.Setenv("GIT_SYMBOLIC_REF", "origin/main")
}

func runVcsInfoCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newVcsInfoCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

// infoRoot is the repo root the report prints — the post-chdir cwd, which is
// what DetectRoot resolves and setupShip echoes as SHIP_FAKE_ROOT.
func infoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return wd
}

func infoLine(t *testing.T, out, label string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if before, after, ok := strings.Cut(line, " "); ok && before == label {
			return strings.TrimSpace(after)
		}
	}
	t.Fatalf("no %q line in report:\n%s", label, out)
	return ""
}

func countInvocations(invocations [][]string, want ...string) int {
	n := 0
	for _, inv := range invocations {
		if len(inv) < len(want) {
			continue
		}
		if strings.Join(inv[:len(want)], " ") == strings.Join(want, " ") {
			n++
		}
	}
	return n
}

func assertNoInvocation(t *testing.T, invocations [][]string, want ...string) {
	t.Helper()
	if countInvocations(invocations, want...) > 0 {
		t.Errorf("%v ran, want it served from cache", want)
	}
}

func TestVcsInfoGTLane(t *testing.T) {
	setupShipGT(t, true)
	infoFakes(t)
	t.Setenv("GIT_STATUS_PORCELAIN", " M a.txt\n M b.txt\n?? c.txt\n")
	t.Setenv("GH_PR_VIEW_feature", `{"number":13,"url":"https://github.com/yasyf/cc-context/pull/13","body":"why"}`)

	out, err := runVcsInfoCmd(t)
	if err != nil {
		t.Fatalf("info error = %v", err)
	}
	want := strings.Join([]string{
		"lane        gt",
		"vcs         git",
		"root        " + infoRoot(t),
		"branch      feature",
		"trunk       main",
		"dirty       yes (3 files)",
		"graphite    config live · gt 1.8.6 · reachable",
		"repo        yasyf/cc-context",
		"visibility  private",
		"permission  ADMIN",
		"viewer      yasyf (affiliated: self)",
		"downstack   feature → PR #13 (body)",
		"",
	}, "\n")
	if out != want {
		t.Errorf("report =\n%s\nwant\n%s", out, want)
	}
}

func TestVcsInfoGitLaneNoGraphite(t *testing.T) {
	setupShip(t, ".git", true)
	infoFakes(t)
	seedLaneRecords(t, ".", laneSeed{})
	t.Setenv("GIT_STATUS_PORCELAIN", " M f.txt\n")

	out, err := runVcsInfoCmd(t)
	if err != nil {
		t.Fatalf("info error = %v", err)
	}
	if got := infoLine(t, out, "lane"); got != "git" {
		t.Errorf("lane = %q, want git", got)
	}
	if got := infoLine(t, out, "trunk"); got != "main" {
		t.Errorf("trunk = %q, want main", got)
	}
	if got := infoLine(t, out, "dirty"); got != "yes (1 file)" {
		t.Errorf("dirty = %q, want yes (1 file)", got)
	}
	if strings.Contains(out, "graphite") {
		t.Errorf("report names graphite with no config live:\n%s", out)
	}
}

// TestVcsInfoJJLane proves the jj lane reports the bookmark ship would target
// even when it is not trunk, rather than refusing the way shipPreflightJJ does.
func TestVcsInfoJJLane(t *testing.T) {
	setupShip(t, ".jj", true)
	infoFakes(t)
	seedLaneRecords(t, ".", laneSeed{})
	t.Setenv("JJ_BOOKMARK_NAMES", "feature")

	out, err := runVcsInfoCmd(t)
	if err != nil {
		t.Fatalf("info error = %v", err)
	}
	if got := infoLine(t, out, "lane"); got != "jj" {
		t.Errorf("lane = %q, want jj", got)
	}
	if got := infoLine(t, out, "branch"); got != "feature" {
		t.Errorf("branch = %q, want feature", got)
	}
	if got := infoLine(t, out, "trunk"); got != "main" {
		t.Errorf("trunk = %q, want main", got)
	}
	if got := infoLine(t, out, "dirty"); got != "yes (1 file)" {
		t.Errorf("dirty = %q, want yes (1 file)", got)
	}
}

// TestVcsInfoJJTrunkUnresolvable proves an ambiguous trunk bookmark drops the
// trunk line rather than failing, the way shipPreflightJJ would.
func TestVcsInfoJJTrunkUnresolvable(t *testing.T) {
	setupShip(t, ".jj", true)
	infoFakes(t)
	seedLaneRecords(t, ".", laneSeed{})
	t.Setenv("JJ_TRUNK_NAMES", "main dev")

	out, err := runVcsInfoCmd(t, "--json")
	if err != nil {
		t.Fatalf("info error = %v", err)
	}
	var got vcsInfo
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal report: %v\n%s", err, out)
	}
	if got.Trunk != "" {
		t.Errorf("trunk = %q, want empty on an ambiguous trunk bookmark", got.Trunk)
	}
	if got.BranchKind != "bookmark" {
		t.Errorf("branch_kind = %q, want bookmark", got.BranchKind)
	}
}

func TestVcsInfoGraphiteDeclined(t *testing.T) {
	setupShipGT(t, true)
	infoFakes(t)
	note := "cc-context is not synced with graphite (gt auth: does not have the necessary permissions)"
	seedLaneRecords(t, ".", laneSeed{unreachable: true, note: note})

	out, err := runVcsInfoCmd(t)
	if err != nil {
		t.Fatalf("info error = %v", err)
	}
	if got := infoLine(t, out, "lane"); got != "git" {
		t.Errorf("lane = %q, want git", got)
	}
	if got := infoLine(t, out, "lane-note"); got != "graphite declined: "+note {
		t.Errorf("lane-note = %q, want the declining note", got)
	}
	if got := infoLine(t, out, "graphite"); got != "config live · gt 1.8.6 · unreachable" {
		t.Errorf("graphite = %q, want the unreachable probe verdict", got)
	}
	if strings.Contains(out, "downstack") {
		t.Errorf("report carries a downstack off the gt lane:\n%s", out)
	}
}

// TestVcsInfoProbeUnknown proves info reports a probe that never answered as
// unknown rather than as either verdict it could not get: the lane demotes, and
// both the lane note and the graphite line carry the reason, so the report never
// claims a reachability it never established.
func TestVcsInfoProbeUnknown(t *testing.T) {
	setupShipGT(t, true)
	infoFakes(t)
	clearGTRecord(t, ".")
	t.Setenv("GT_AUTH_HANG", "1")

	out, err := runVcsInfoCmd(t, "--json")
	if err != nil {
		t.Fatalf("info error = %v", err)
	}
	var got vcsInfo
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal report: %v\n%s", err, out)
	}
	wantReason := "gt auth did not answer within " + gtProbeTimeout.String()
	if got.Lane != "git" {
		t.Errorf("lane = %q, want git — an unknown verdict demotes", got.Lane)
	}
	if got.LaneReason != infoDeclinedPrefix+wantReason {
		t.Errorf("lane_reason = %q, want %q", got.LaneReason, infoDeclinedPrefix+wantReason)
	}
	if got.Graphite.Reachable != string(gtVerdictUnknown) || got.Graphite.Reason != wantReason {
		t.Errorf("graphite reachable/reason = %q/%q, want %q/%q",
			got.Graphite.Reachable, got.Graphite.Reason, gtVerdictUnknown, wantReason)
	}

	human, err := runVcsInfoCmd(t)
	if err != nil {
		t.Fatalf("info error = %v", err)
	}
	want := "config live · gt 1.8.6 · reachability unknown (" + wantReason + ")"
	if line := infoLine(t, human, "graphite"); line != want {
		t.Errorf("graphite = %q, want %q", line, want)
	}
}

// TestVcsInfoRefreshLaneVerdict proves --refresh re-probes the verdict the lane
// turns on rather than only the line describing it: a cached negative the user
// has since fixed moves the lane itself, and every input is asked for once.
func TestVcsInfoRefreshLaneVerdict(t *testing.T) {
	const staleNote = "graphite has no auth token — run gt auth --token <token>"
	tests := []struct {
		name          string
		args          []string
		wantLane      string
		wantReason    string
		wantReachable gtVerdict
		wantLookups   int
	}{
		{"refresh re-probes", []string{"--json", "--refresh"}, "gt", "", gtVerdictOK, 1},
		{"no refresh serves the cached verdict", []string{"--json"}, "git", infoDeclinedPrefix + staleNote, gtVerdictDenied, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := setupShipGT(t, true)
			infoFakes(t)
			seedLaneRecords(t, ".", laneSeed{unreachable: true, note: staleNote})
			t.Setenv("GH_REPO_VIEW_JSON",
				`{"nameWithOwner":"yasyf/cc-context","owner":{"login":"yasyf"},"isPrivate":true,"viewerPermission":"ADMIN"}`)

			out, err := runVcsInfoCmd(t, tt.args...)
			if err != nil {
				t.Fatalf("info error = %v", err)
			}
			var got vcsInfo
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("unmarshal report: %v\n%s", err, out)
			}
			if got.Lane != tt.wantLane || got.LaneReason != tt.wantReason {
				t.Errorf("lane/lane_reason = %q/%q, want %q/%q", got.Lane, got.LaneReason, tt.wantLane, tt.wantReason)
			}
			if got.Graphite.Reachable != string(tt.wantReachable) {
				t.Errorf("graphite.reachable = %q, want %q — the report contradicts its own lane", got.Graphite.Reachable, tt.wantReachable)
			}
			invocations := readInvocations(t, log)
			if n := countInvocations(invocations, "gt", "auth"); n != tt.wantLookups {
				t.Errorf("gt auth ran %d times, want %d", n, tt.wantLookups)
			}
			if n := countInvocations(invocations, "gh", "repo", "view"); n != tt.wantLookups {
				t.Errorf("gh repo view ran %d times, want %d", n, tt.wantLookups)
			}
		})
	}
}

func TestVcsInfoDetachedHead(t *testing.T) {
	setupShip(t, ".git", true)
	infoFakes(t)
	seedLaneRecords(t, ".", laneSeed{})
	t.Setenv("GIT_BRANCH", "")

	out, err := runVcsInfoCmd(t, "--json")
	if err != nil {
		t.Fatalf("info error = %v", err)
	}
	var got vcsInfo
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal report: %v\n%s", err, out)
	}
	if !got.Detached {
		t.Errorf("detached = false, want true on an empty git branch --show-current")
	}
	if got.Branch != "" {
		t.Errorf("branch = %q, want empty", got.Branch)
	}
}

func TestVcsInfoWithoutGh(t *testing.T) {
	setupShip(t, ".git", false)
	infoFakes(t)
	clearLaneRecords(t, ".")

	out, err := runVcsInfoCmd(t, "--json")
	if err != nil {
		t.Fatalf("info error = %v", err)
	}
	var got vcsInfo
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal report: %v\n%s", err, out)
	}
	if got.GitHub != nil {
		t.Errorf("github = %+v, want null with no gh on PATH", got.GitHub)
	}
	if !strings.Contains(got.GitHubError, "gh not on PATH") {
		t.Errorf("github_error = %q, want it to name the missing gh", got.GitHubError)
	}
}

func TestVcsInfoJSON(t *testing.T) {
	setupShipGT(t, true)
	infoFakes(t)
	t.Setenv("GIT_STATUS_PORCELAIN", "")
	t.Setenv("GH_PR_VIEW_feature", `{"number":13,"url":"https://github.com/yasyf/cc-context/pull/13","body":"why"}`)

	out, err := runVcsInfoCmd(t, "--json")
	if err != nil {
		t.Fatalf("info error = %v", err)
	}
	var got vcsInfo
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal report: %v\n%s", err, out)
	}
	if got.Lane != "gt" || got.VCS != "git" || got.BranchKind != "branch" {
		t.Errorf("lane/vcs/branch_kind = %q/%q/%q, want gt/git/branch", got.Lane, got.VCS, got.BranchKind)
	}
	if got.Root != infoRoot(t) || got.Branch != "feature" || got.Trunk != "main" {
		t.Errorf("root/branch/trunk = %q/%q/%q", got.Root, got.Branch, got.Trunk)
	}
	if got.Dirty || got.DirtyFiles != 0 || got.Detached {
		t.Errorf("dirty/dirty_files/detached = %t/%d/%t, want false/0/false", got.Dirty, got.DirtyFiles, got.Detached)
	}
	if !got.Graphite.Config || !got.Graphite.CLI || got.Graphite.Version != "1.8.6" {
		t.Errorf("graphite = %+v, want a live config on gt 1.8.6", got.Graphite)
	}
	if got.Graphite.Reachable != string(gtVerdictOK) {
		t.Errorf("graphite.reachable = %q, want %q", got.Graphite.Reachable, gtVerdictOK)
	}
	if got.GitHub == nil || got.GitHub.NameWithOwner != "yasyf/cc-context" || got.GitHub.ViewerPermission != "ADMIN" {
		t.Errorf("github = %+v, want the seeded repo record", got.GitHub)
	}
	if got.GitHubError != "" {
		t.Errorf("github_error = %q, want empty", got.GitHubError)
	}
	want := []stackEntry{{Branch: "feature", PR: 13, URL: "https://github.com/yasyf/cc-context/pull/13", HasBody: true}}
	if len(got.Downstack) != 1 || got.Downstack[0] != want[0] {
		t.Errorf("downstack = %+v, want %+v", got.Downstack, want)
	}
}

// TestVcsInfoDownstackBodies proves the whole submit set is reported base
// first, each entry carrying whether its PR already has a body to draft into.
func TestVcsInfoDownstackBodies(t *testing.T) {
	setupShipGT(t, true)
	infoFakes(t)
	t.Setenv("GT_STATE_JSON", `{"main":{"trunk":true},"base":{"parents":[{"ref":"main","sha":"aa"}]},"feature":{"parents":[{"ref":"base","sha":"bb"}]}}`)
	t.Setenv("GH_PR_VIEW_base", `{"number":12,"url":"https://github.com/yasyf/cc-context/pull/12","body":"why"}`)
	t.Setenv("GH_PR_VIEW_feature", `{"number":13,"url":"https://github.com/yasyf/cc-context/pull/13","body":"   "}`)

	out, err := runVcsInfoCmd(t)
	if err != nil {
		t.Fatalf("info error = %v", err)
	}
	want := "base → PR #12 (body) · feature → PR #13 (no body)"
	if got := infoLine(t, out, "downstack"); got != want {
		t.Errorf("downstack = %q, want %q", got, want)
	}
}

func TestVcsInfoUntrackedBranch(t *testing.T) {
	setupShipGT(t, true)
	infoFakes(t)
	t.Setenv("GT_STATE_JSON", `{"main":{"trunk":true}}`)

	out, err := runVcsInfoCmd(t)
	if err != nil {
		t.Fatalf("info error = %v", err)
	}
	want := "config live · gt 1.8.6 · reachable · branch untracked (gt track --parent main)"
	if got := infoLine(t, out, "graphite"); got != want {
		t.Errorf("graphite = %q, want %q", got, want)
	}
	if strings.Contains(out, "downstack") {
		t.Errorf("report names a downstack for an untracked branch:\n%s", out)
	}
}

// TestVcsInfoBrokenAncestorChainReported proves an unresolvable parent chain is
// the report's answer rather than its failure: a stack nobody can walk is the
// state someone runs info to diagnose, and the branch and dirtiness around it
// stay readable. The error text still carries info's own prefix — it must never
// tell the reader to go look at ship.
func TestVcsInfoBrokenAncestorChainReported(t *testing.T) {
	setupShipGT(t, true)
	infoFakes(t)
	t.Setenv("GIT_BRANCH", "feature2")
	t.Setenv("GT_STATE_JSON", `{"main":{"trunk":true},"feature2":{"parents":[{"ref":"feature","sha":"bb"}]}}`)

	out, err := runVcsInfoCmd(t)
	if err != nil {
		t.Fatalf("info error = %v", err)
	}
	stack := infoLine(t, out, "stack")
	want := "info: gt state has no parent for feature, an ancestor of feature2"
	if !strings.Contains(stack, want) {
		t.Errorf("stack = %q, want it to contain %q", stack, want)
	}
	if strings.Contains(stack, "ship:") {
		t.Errorf("stack = %q, want info's own prefix, not ship's", stack)
	}
	if got := infoLine(t, out, "branch"); got != "feature2" {
		t.Errorf("branch = %q, want the report to survive the unresolvable stack", got)
	}
	if strings.Contains(out, "downstack") {
		t.Errorf("report names a downstack it could not resolve:\n%s", out)
	}
}

// TestVcsInfoGTStateFailureCarriesInfoPrefix proves gt state info cannot parse
// is reported, still under info's own prefix rather than ship's.
func TestVcsInfoGTStateFailureCarriesInfoPrefix(t *testing.T) {
	setupShipGT(t, true)
	infoFakes(t)
	t.Setenv("GT_STATE_JSON", "not json")

	out, err := runVcsInfoCmd(t)
	if err != nil {
		t.Fatalf("info error = %v", err)
	}
	if got := infoLine(t, out, "stack"); !strings.HasPrefix(got, "info: parse gt state:") {
		t.Errorf("stack = %q, want it to lead with info's own prefix", got)
	}
	if strings.Contains(out, "trunk") {
		t.Errorf("report names a trunk gt state never gave it:\n%s", out)
	}
}

// TestVcsInfoGitTrunkFailureCarriesInfoPrefix pins the git lane's half of the
// same rule: gitRemoteTrunk is restack's helper, and its error would otherwise
// send someone running info off to restack.
func TestVcsInfoGitTrunkFailureCarriesInfoPrefix(t *testing.T) {
	setupShip(t, ".git", true)
	infoFakes(t)
	seedLaneRecords(t, ".", laneSeed{})
	t.Setenv("GIT_SYMBOLIC_REF", "")

	out, err := runVcsInfoCmd(t)
	if err == nil {
		t.Fatalf("info succeeded with no resolvable default branch:\n%s", out)
	}
	want := "info: cannot resolve origin's default branch"
	if !strings.HasPrefix(err.Error(), want) {
		t.Errorf("error = %v, want it to lead with %q", err, want)
	}
	if strings.Contains(err.Error(), "restack:") {
		t.Errorf("error = %v, want info's own prefix, not restack's", err)
	}
}

// TestVcsInfoTrunkHolder proves info names the working copy holding trunk — the
// thing that explains a gt restack skipping a branch "because it is checked out
// in worktree W" and still exiting 0 — and stays silent when no checkout holds
// it, which is what git reports for a detached main working copy.
func TestVcsInfoTrunkHolder(t *testing.T) {
	tests := []struct {
		name    string
		holders string
		want    string
	}{
		{"trunk held elsewhere", "main=/wt/main feature=", "/wt/main"},
		{"trunk held by nobody", "main= feature=", ""},
		{"no checkout holds anything", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setupShipGT(t, true)
			infoFakes(t)
			t.Setenv("GIT_BRANCH_HOLDERS", tt.holders)

			out, err := runVcsInfoCmd(t, "--json")
			if err != nil {
				t.Fatalf("info error = %v", err)
			}
			var got vcsInfo
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("unmarshal report: %v\n%s", err, out)
			}
			if got.TrunkHolder != tt.want {
				t.Errorf("trunk_holder = %q, want %q", got.TrunkHolder, tt.want)
			}
			human, err := runVcsInfoCmd(t)
			if err != nil {
				t.Fatalf("info error = %v", err)
			}
			if tt.want == "" {
				if strings.Contains(human, "trunk-held") {
					t.Errorf("report names a trunk holder nobody is:\n%s", human)
				}
				return
			}
			if line := infoLine(t, human, "trunk-held"); line != tt.want {
				t.Errorf("trunk-held = %q, want %q", line, tt.want)
			}
		})
	}
}

// TestVcsInfoMainCheckoutHasNoWorktreeBlock proves the worktree block is the
// linked checkout's answer alone: a repository's own working copy is not "inside"
// anything, so reporting a shape, a main root, and a repo key that all restate
// root would be noise.
func TestVcsInfoMainCheckoutHasNoWorktreeBlock(t *testing.T) {
	setupShipGT(t, true)
	infoFakes(t)

	out, err := runVcsInfoCmd(t, "--json")
	if err != nil {
		t.Fatalf("info error = %v", err)
	}
	var got vcsInfo
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal report: %v\n%s", err, out)
	}
	if got.Worktree != nil {
		t.Errorf("worktree = %+v, want none for the repository's own working copy", got.Worktree)
	}
}

// TestVcsInfoLinkedWorktree proves a linked worktree reports where it sits
// rather than refusing, and that root stays this checkout's own tree — pointing
// it at the main working copy would name bytes ccx is not looking at.
func TestVcsInfoLinkedWorktree(t *testing.T) {
	setupShip(t, "", true)
	infoFakes(t)
	root := infoRoot(t)
	main, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve main checkout: %v", err)
	}
	common := filepath.Join(main, ".git")
	admin := filepath.Join(common, "worktrees", "wt")
	if err := os.MkdirAll(admin, 0o750); err != nil {
		t.Fatalf("mkdir admin: %v", err)
	}
	if err := os.WriteFile(filepath.Join(admin, "commondir"), []byte("../..\n"), 0o600); err != nil {
		t.Fatalf("write commondir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: "+admin+"\n"), 0o600); err != nil {
		t.Fatalf("write gitdir pointer: %v", err)
	}
	// The lane cache keys on the repository, which the pointer only now names, so
	// the seed a linked worktree reads has to be written after it.
	seedLaneRecords(t, ".", laneSeed{})

	out, err := runVcsInfoCmd(t, "--json")
	if err != nil {
		t.Fatalf("info error = %v", err)
	}
	var got vcsInfo
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal report: %v\n%s", err, out)
	}
	if got.Root != root {
		t.Errorf("root = %q, want this checkout's own tree %q", got.Root, root)
	}
	// The repository's own paths are the gitdir pointer's bytes, resolved
	// symlink-free, so two checkouts reaching one repository key it identically.
	want := worktreeInfo{
		Shape:     "git worktree",
		MainRoot:  main,
		CommonDir: common,
		RepoKey:   common,
	}
	if got.Worktree == nil || *got.Worktree != want {
		t.Errorf("worktree = %+v, want %+v", got.Worktree, want)
	}
}

// TestVcsInfoBrokenCheckoutReported proves the command whose whole output is a
// diagnosis of the working copy survives one: a gitdir pointer resolving to
// nothing is reported, not an exit 1, and the report stops there rather than
// claiming a branch or a dirtiness it cannot read.
func TestVcsInfoBrokenCheckoutReported(t *testing.T) {
	setupShip(t, "", true)
	infoFakes(t)
	seedLaneRecords(t, ".", laneSeed{})
	root := infoRoot(t)
	dangling := filepath.Join(t.TempDir(), "gone")
	if err := os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: "+dangling+"\n"), 0o600); err != nil {
		t.Fatalf("write gitdir pointer: %v", err)
	}

	out, err := runVcsInfoCmd(t)
	if err != nil {
		t.Fatalf("info error = %v", err)
	}
	got := infoLine(t, out, "checkout")
	for _, want := range []string{"gitdir pointer resolves to nothing", dangling} {
		if !strings.Contains(got, want) {
			t.Errorf("checkout = %q, want it to contain %q", got, want)
		}
	}
	for _, absent := range []string{"dirty", "branch", "graphite"} {
		if strings.Contains(out, absent) {
			t.Errorf("report names %q it could not read:\n%s", absent, out)
		}
	}
}

// TestVcsInfoWarmCacheSkipsRepoView proves a seeded record answers the GitHub
// lookup outright: info is an orientation call, and the one round trip it still
// makes is the downstack's own pull request lookup — one for the whole stack.
func TestVcsInfoWarmCacheSkipsRepoView(t *testing.T) {
	log := setupShipGT(t, true)
	infoFakes(t)

	if _, err := runVcsInfoCmd(t); err != nil {
		t.Fatalf("info error = %v", err)
	}
	invocations := readInvocations(t, log)
	assertNoInvocation(t, invocations, "gh", "repo", "view")
	assertNoInvocation(t, invocations, "gt", "auth")
	var graphql [][]string
	for _, inv := range invocations {
		if len(inv) > 2 && inv[0] == "gh" && inv[1] == "api" && inv[2] == "graphql" {
			graphql = append(graphql, inv)
		}
	}
	assertInvocations(t, graphql, [][]string{ghDownstackPRArgv("feature")})
}
