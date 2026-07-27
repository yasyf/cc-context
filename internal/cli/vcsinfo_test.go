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
// status/symbolic-ref/show-ref, gt --version, and a per-branch gh pr view. Each
// wrapper handles its own arms and execs the renamed base fake for the rest, so
// writeShipFakes stays untouched.
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
  "pr view")
    `+logRec("gh")+`    eval "json=\${GH_PR_VIEW_$3-}"
    if [ -z "$json" ]; then
      printf 'no pull requests found for branch "%s"\n' "$3" >&2
      exit 1
    fi
    printf '%s' "$json"
    exit 0 ;;
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
		wantReachable bool
		wantLookups   int
	}{
		{"refresh re-probes", []string{"--json", "--refresh"}, "gt", "", true, 1},
		{"no refresh serves the cached verdict", []string{"--json"}, "git", infoDeclinedPrefix + staleNote, false, 0},
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
			if got.Graphite.Reachable == nil {
				t.Fatalf("graphite.reachable = null, want a probed %t", tt.wantReachable)
			}
			if *got.Graphite.Reachable != tt.wantReachable {
				t.Errorf("graphite.reachable = %t, want %t — the report contradicts its own lane", *got.Graphite.Reachable, tt.wantReachable)
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
	if got.Graphite.Reachable == nil || !*got.Graphite.Reachable {
		t.Errorf("graphite.reachable = %v, want a probed true", got.Graphite.Reachable)
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

// TestVcsInfoWarmCacheSkipsRepoView proves a seeded record answers the GitHub
// lookup outright: info is an orientation call, not a network round trip.
func TestVcsInfoWarmCacheSkipsRepoView(t *testing.T) {
	log := setupShipGT(t, true)
	infoFakes(t)

	if _, err := runVcsInfoCmd(t); err != nil {
		t.Fatalf("info error = %v", err)
	}
	invocations := readInvocations(t, log)
	assertNoInvocation(t, invocations, "gh", "repo", "view")
	assertNoInvocation(t, invocations, "gh", "api", "graphql")
	assertNoInvocation(t, invocations, "gt", "auth")
}
