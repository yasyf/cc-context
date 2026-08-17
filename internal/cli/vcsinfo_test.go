package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/vcstest"
)

// infoRepo builds a real repository per opts and seeds its lane cache, so the
// gate resolves without a gh subprocess. Every report these tests assert is git,
// jj, and gt answering for themselves.
func infoRepo(t *testing.T, opts ...vcstest.Opt) *vcstest.Fixture {
	t.Helper()
	f := vcstest.Repo(t, opts...)
	seedLaneRecords(t, ".", laneSeed{})
	return f
}

// infoGTRepo builds a real graphite repository whose stack is main → branches,
// each branch tracked on the one before it, and leaves the working copy on the
// last. gt track is local — it writes .git/.graphite_metadata.db and reaches no
// network — so the stack gt state reports here is one gt itself built.
func infoGTRepo(t *testing.T, branches ...string) *vcstest.Fixture {
	t.Helper()
	f := vcstest.Repo(t, vcstest.GT(), vcstest.Remote())
	parent := "main"
	for i, branch := range branches {
		runTool(t, f.Dir, "git", "switch", "-qc", branch)
		writeInfoFile(t, f.Dir, fmt.Sprintf("b%d.txt", i), branch+"\n")
		runTool(t, f.Dir, "git", "add", "-A")
		runTool(t, f.Dir, "git", "commit", "-qm", branch)
		runTool(t, f.Dir, "gt", "track", "--parent", parent, "--no-interactive")
		parent = branch
	}
	seedLaneRecords(t, ".", laneSeed{})
	return f
}

// gtVersion is the gt behind the fixture, read from gt itself so the report
// assertion pins the segment's shape rather than the version this machine has.
func gtVersion(t *testing.T, f *vcstest.Fixture) string {
	t.Helper()
	return strings.TrimSpace(runTool(t, f.Dir, "gt", "--version"))
}

// resetArgvLog drops the records the fixture's own setup commands wrote, so an
// assertion over ccx's invocations reads only ccx's.
func resetArgvLog(t *testing.T, f *vcstest.Fixture) {
	t.Helper()
	vcstest.Quiesce(t, f.ArgvLog)
	if err := os.Remove(f.ArgvLog); err != nil && !os.IsNotExist(err) {
		t.Fatalf("reset argv log: %v", err)
	}
}

func runTool(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...) //nolint:gosec // name resolves through the fixture's own shim PATH and args are fixture-authored
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return string(out)
}

func writeInfoFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// ghReplayKey names the invocation one recorded scenario answers. gh is a
// network boundary, so it stays a script — but the script picks a payload, it
// never composes one.
func ghReplayKey(t *testing.T, g ghGolden) string {
	t.Helper()
	switch {
	case slices.Equal(g.argv[:min(2, len(g.argv))], []string{"repo", "view"}):
		return "REPO_VIEW"
	case !slices.Equal(g.argv[:min(2, len(g.argv))], []string{"api", "graphql"}):
		t.Fatalf("golden cli/%s: argv %q answers no invocation ccx vcs info makes", g.name, g.argv)
		return ""
	case slices.ContainsFunc(g.argv, func(a string) bool { return strings.Contains(a, "pullRequests") }):
		return "DOWNSTACK"
	default:
		return "VIEWER"
	}
}

// ghReplay installs a gh that answers each recorded scenario's invocation with
// that scenario's own bytes, and records its argv into the fixture's log in the
// shim's framing so a gh call is counted like a real tool's. An invocation no
// loaded scenario answers exits 2 rather than inventing a payload. The goldens
// are loaded by the caller because their loader spells a relative path and a
// fixture has already left the package directory.
func ghReplay(t *testing.T, f *vcstest.Fixture, goldens ...ghGolden) {
	t.Helper()
	for _, g := range goldens {
		key := ghReplayKey(t, g)
		t.Setenv("CCX_GH_STDOUT_"+key, g.stdout)
		t.Setenv("CCX_GH_STDERR_"+key, g.stderr)
		t.Setenv("CCX_GH_EXIT_"+key, strconv.Itoa(g.exit))
	}
	script := "#!/bin/sh\n" +
		`d="${CCX_SHIM_DEPTH:-0}"` + "\n" +
		`printf '%s\0' "$d" "$(($#+1))" gh "$@" >> ` + shQuote(f.ArgvLog) + "\n" +
		`case "$1 $2" in
  "repo view") key=REPO_VIEW ;;
  "api graphql")
    case "$*" in
      *pullRequests*) key=DOWNSTACK ;;
      *) key=VIEWER ;;
    esac ;;
  *) printf 'gh replay: no recorded scenario for: %s\n' "$*" >&2; exit 2 ;;
esac
eval "code=\${CCX_GH_EXIT_$key-unloaded}"
if [ "$code" = unloaded ]; then printf 'gh replay: %s not loaded\n' "$key" >&2; exit 2; fi
eval "printf '%s' \"\$CCX_GH_STDOUT_$key\""
eval "printf '%s' \"\$CCX_GH_STDERR_$key\"" >&2
exit "$code"
`
	writeExecutable(t, filepath.Join(f.ShimBin, "gh"), script)
}

// gtAuthGolden makes gt auth — the one network verb the reachability probe runs
// — answer with g's recorded bytes, leaving every local verb to the real gt.
func gtAuthGolden(t *testing.T, f *vcstest.Fixture, g gtGolden) {
	t.Helper()
	if g.argv[0] != "auth" {
		t.Fatalf("golden %s records gt %s, not the auth probe", g.name, g.argv[0])
	}
	t.Setenv("CCX_GT_AUTH_STDOUT", g.stdout)
	t.Setenv("CCX_GT_AUTH_STDERR", g.stderr)
	t.Setenv("CCX_GT_AUTH_EXIT", strconv.Itoa(g.exit))
	gtAuthShim(t, f)
}

// gtAuthHangs leaves the probe unanswered past its deadline. It is a timing
// fixture: it claims nothing about what gt prints, only that nobody answered.
func gtAuthHangs(t *testing.T, f *vcstest.Fixture) {
	t.Helper()
	t.Setenv("CCX_GT_AUTH_HANG", "1")
	gtAuthShim(t, f)
}

func gtAuthShim(t *testing.T, f *vcstest.Fixture) {
	t.Helper()
	shim := filepath.Join(f.ShimBin, "gt")
	passthrough := filepath.Join(f.ShimBin, "gt-shim")
	if err := os.Rename(shim, passthrough); err != nil {
		t.Fatalf("rename gt shim: %v", err)
	}
	// exec so the probe's process-group kill reaps the sleep too: a surviving
	// grandchild holds the stdout pipe open past the deadline.
	script := "#!/bin/sh\n" +
		`if [ "$1" = auth ]; then` + "\n" +
		`  d="${CCX_SHIM_DEPTH:-0}"` + "\n" +
		`  printf '%s\0' "$d" "$(($#+1))" gt "$@" >> ` + shQuote(f.ArgvLog) + "\n" +
		`  if [ -n "$CCX_GT_AUTH_HANG" ]; then exec /bin/sleep 30; fi` + "\n" +
		`  printf '%s' "$CCX_GT_AUTH_STDOUT"` + "\n" +
		`  printf '%s' "$CCX_GT_AUTH_STDERR" >&2` + "\n" +
		`  exit "$CCX_GT_AUTH_EXIT"` + "\n" +
		"fi\n" +
		"exec " + shQuote(passthrough) + ` "$@"` + "\n"
	writeExecutable(t, shim, script)
}

func writeExecutable(t *testing.T, path, script string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // a PATH entry must be owner-executable
		t.Fatalf("write %s: %v", path, err)
	}
}

func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
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

func runVcsInfoJSON(t *testing.T, args ...string) vcsInfo {
	t.Helper()
	out, err := runVcsInfoCmd(t, append([]string{"--json"}, args...)...)
	if err != nil {
		t.Fatalf("info error = %v", err)
	}
	var got vcsInfo
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal report: %v\n%s", err, out)
	}
	return got
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

// downstackOne and downstackThree are the branches the recorded downstack
// queries name. A fixture stacks exactly these, so the payloads replayed back
// are GitHub's answers to the query ccx builds here.
var (
	downstackOne   = []string{"fix-ship-help-graphite-demote"}
	downstackThree = []string{"fix-ship-help-graphite-demote", "yasyf/transcript-ccx-issues", "no-such-branch"}
)

// TestVcsInfoDownstackArgvIsTheRecordedOne pins the batched query production
// builds to the one GitHub answered, so a replayed payload is the answer to
// ccx's own call rather than to a query nobody makes.
func TestVcsInfoDownstackArgvIsTheRecordedOne(t *testing.T) {
	t.Parallel()
	tests := []struct {
		golden   string
		branches []string
	}{
		{"downstack-graphql-one", downstackOne},
		{"downstack-graphql-three", downstackThree},
	}
	for _, tt := range tests {
		t.Run(tt.golden, func(t *testing.T) {
			t.Parallel()
			want := loadGHGolden(t, tt.golden).argv
			if got := ghDownstackPRArgv(tt.branches...)[1:]; !slices.Equal(got, want) {
				t.Errorf("ghDownstackPRArgv() = %q, want the recorded %q", got, want)
			}
		})
	}
}

func TestVcsInfoGTLane(t *testing.T) {
	downstack := loadGHGolden(t, "downstack-graphql-one")
	f := infoGTRepo(t, downstackOne...)
	ghReplay(t, f, downstack)
	version := gtVersion(t, f)
	writeInfoFile(t, f.Dir, "f.txt", "dirty\n")
	writeInfoFile(t, f.Dir, "b0.txt", "dirty\n")
	writeInfoFile(t, f.Dir, "untracked.txt", "new\n")

	out, err := runVcsInfoCmd(t)
	if err != nil {
		t.Fatalf("info error = %v", err)
	}
	want := strings.Join([]string{
		"lane        gt",
		"vcs         git",
		"root        " + f.Dir,
		"branch      fix-ship-help-graphite-demote",
		"trunk       main",
		"dirty       yes (3 files)",
		"graphite    config live · gt " + version + " · reachable",
		"repo        yasyf/cc-context",
		"visibility  private",
		"permission  ADMIN",
		"viewer      yasyf (affiliated: self)",
		"downstack   fix-ship-help-graphite-demote → PR #3 (body, merged, checks success)",
		"",
	}, "\n")
	if out != want {
		t.Errorf("report =\n%s\nwant\n%s", out, want)
	}
}

func TestVcsInfoGitLaneNoGraphite(t *testing.T) {
	f := infoRepo(t, vcstest.Remote(), vcstest.Dirty())

	out, err := runVcsInfoCmd(t)
	if err != nil {
		t.Fatalf("info error = %v", err)
	}
	if got := infoLine(t, out, "lane"); got != "git" {
		t.Errorf("lane = %q, want git", got)
	}
	head := strings.TrimSpace(runTool(t, f.Dir, "git", "symbolic-ref", "refs/remotes/origin/HEAD"))
	if want := "refs/remotes/origin/main"; head != want {
		t.Fatalf("fixture origin/HEAD = %q, want %q", head, want)
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
	f := infoRepo(t, vcstest.JJ(), vcstest.Remote())
	writeInfoFile(t, f.Dir, "g.txt", "feature\n")
	runTool(t, f.Dir, "jj", "commit", "-m", "feature")
	runTool(t, f.Dir, "jj", "bookmark", "create", "feature", "-r", "@-")
	writeInfoFile(t, f.Dir, "f.txt", "dirty\n")

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
	f := infoRepo(t, vcstest.JJ(), vcstest.Remote())
	runTool(t, f.Dir, "jj", "bookmark", "create", "dev", "-r", "main")
	runTool(t, f.Dir, "jj", "git", "push", "--bookmark", "dev")

	got := runVcsInfoJSON(t)
	if got.Trunk != "" {
		t.Errorf("trunk = %q, want empty on an ambiguous trunk bookmark", got.Trunk)
	}
	if got.BranchKind != "bookmark" {
		t.Errorf("branch_kind = %q, want bookmark", got.BranchKind)
	}
}

func TestVcsInfoGraphiteDeclined(t *testing.T) {
	f := infoGTRepo(t, "feature")
	version := gtVersion(t, f)
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
	if got := infoLine(t, out, "graphite"); got != "config live · gt "+version+" · unreachable" {
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
	f := infoGTRepo(t)
	version := gtVersion(t, f)
	clearGTRecord(t, ".")
	gtAuthHangs(t, f)
	shortenGTProbe(t)

	got := runVcsInfoJSON(t)
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
	want := "config live · gt " + version + " · reachability unknown (" + wantReason + ")"
	if line := infoLine(t, human, "graphite"); line != want {
		t.Errorf("graphite = %q, want %q", line, want)
	}
}

// TestVcsInfoRefreshLaneVerdict proves --refresh re-probes the verdict the lane
// turns on rather than only the line describing it: a cached positive Graphite
// has since withdrawn moves the lane itself, and every input is asked for once.
func TestVcsInfoRefreshLaneVerdict(t *testing.T) {
	tests := []struct {
		name          string
		args          []string
		wantLane      string
		wantReachable gtVerdict
		wantLookups   int
	}{
		{"refresh re-probes", []string{"--refresh"}, "git", gtVerdictDenied, 1},
		{"no refresh serves the cached verdict", nil, "gt", gtVerdictOK, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repoView := loadGHGolden(t, "repo-view-own")
			viewer := loadGHGolden(t, "viewer-graphql")
			probe := loadGTGolden(t, "auth-no-perms")
			f := infoGTRepo(t)
			ghReplay(t, f, repoView, viewer)
			gtAuthGolden(t, f, probe)
			resetArgvLog(t, f)

			got := runVcsInfoJSON(t, tt.args...)
			wantReason := ""
			if tt.wantReachable == gtVerdictDenied {
				// The note is gt's own refusal, quoted whole — the line it wrote
				// first, read off the golden rather than through the matcher under
				// test.
				wantReason = infoDeclinedPrefix + strings.TrimSpace(strings.Split(probe.stderr, "\n")[0])
			}
			if got.Lane != tt.wantLane || got.LaneReason != wantReason {
				t.Errorf("lane/lane_reason = %q/%q, want %q/%q", got.Lane, got.LaneReason, tt.wantLane, wantReason)
			}
			if got.Graphite.Reachable != string(tt.wantReachable) {
				t.Errorf("graphite.reachable = %q, want %q — the report contradicts its own lane", got.Graphite.Reachable, tt.wantReachable)
			}
			vcstest.Quiesce(t, f.ArgvLog)
			invocations := vcstest.Invocations(t, f.ArgvLog)
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
	infoRepo(t, vcstest.Remote(), vcstest.Detached())

	got := runVcsInfoJSON(t)
	if !got.Detached {
		t.Errorf("detached = false, want true on a detached HEAD")
	}
	if got.Branch != "" {
		t.Errorf("branch = %q, want empty", got.Branch)
	}
	if got.Trunk != "main" {
		t.Errorf("trunk = %q, want main — a detached HEAD still has a default branch", got.Trunk)
	}
}

func TestVcsInfoWithoutGh(t *testing.T) {
	f := vcstest.Repo(t, vcstest.Remote())
	// The fixture's brew-free PATH keeps /usr/bin, which is where CI installs
	// gh, so only the shim directory alone holds no gh to find.
	f.OnlyShimPATH(t)
	clearLaneRecords(t, ".")
	if path, err := exec.LookPath("gh"); err == nil {
		t.Fatalf("gh resolved to %s; the fixture PATH must hold none", path)
	}

	got := runVcsInfoJSON(t)
	if got.GitHub != nil {
		t.Errorf("github = %+v, want null with no gh on PATH", got.GitHub)
	}
	if !strings.Contains(got.GitHubError, "gh not on PATH") {
		t.Errorf("github_error = %q, want it to name the missing gh", got.GitHubError)
	}
}

func TestVcsInfoJSON(t *testing.T) {
	downstack := loadGHGolden(t, "downstack-graphql-one")
	f := infoGTRepo(t, downstackOne...)
	ghReplay(t, f, downstack)
	version := gtVersion(t, f)

	got := runVcsInfoJSON(t)
	if got.Lane != "gt" || got.VCS != "git" || got.BranchKind != "branch" {
		t.Errorf("lane/vcs/branch_kind = %q/%q/%q, want gt/git/branch", got.Lane, got.VCS, got.BranchKind)
	}
	if got.Root != f.Dir || got.Branch != downstackOne[0] || got.Trunk != "main" {
		t.Errorf("root/branch/trunk = %q/%q/%q", got.Root, got.Branch, got.Trunk)
	}
	if got.Dirty || got.DirtyFiles != 0 || got.Detached {
		t.Errorf("dirty/dirty_files/detached = %t/%d/%t, want false/0/false", got.Dirty, got.DirtyFiles, got.Detached)
	}
	if !got.Graphite.Config || !got.Graphite.CLI || got.Graphite.Version != version {
		t.Errorf("graphite = %+v, want a live config on gt %s", got.Graphite, version)
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
	if len(got.Downstack) != 1 {
		t.Fatalf("downstack = %+v, want the one branch of the stack", got.Downstack)
	}
	entry := got.Downstack[0]
	want := stackEntry{
		Branch:   downstackOne[0],
		PR:       3,
		URL:      "https://github.com/yasyf/cc-context/pull/3",
		HasBody:  true,
		State:    "MERGED",
		Merged:   true,
		MergedAt: entry.MergedAt,
		Checks:   "SUCCESS",
	}
	if entry != want {
		t.Errorf("downstack entry = %+v, want %+v", entry, want)
	}
	if entry.MergedAt == nil || !entry.MergedAt.Equal(infoTime(t, "2026-07-29T10:32:39Z")) {
		t.Errorf("downstack merged_at = %v, want the recorded 2026-07-29T10:32:39Z", entry.MergedAt)
	}
}

func infoTime(t *testing.T, text string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, text)
	if err != nil {
		t.Fatalf("parse %q: %v", text, err)
	}
	return at
}

// TestVcsInfoDownstackBodies proves the whole submit set is reported base first,
// each entry carrying whether its PR already has a body to draft into. The
// recorded query answers three branches: two with a pull request, one with none.
func TestVcsInfoDownstackBodies(t *testing.T) {
	downstack := loadGHGolden(t, "downstack-graphql-three")
	f := infoGTRepo(t, downstackThree...)
	ghReplay(t, f, downstack)

	out, err := runVcsInfoCmd(t)
	if err != nil {
		t.Fatalf("info error = %v", err)
	}
	want := "fix-ship-help-graphite-demote → PR #3 (body, merged, checks success) · " +
		"yasyf/transcript-ccx-issues → PR #2 (body, merged, checks success) · no-such-branch"
	if got := infoLine(t, out, "downstack"); got != want {
		t.Errorf("downstack = %q, want %q", got, want)
	}
}

// TestInfoDownstackValue pins the shapes one stack entry renders as, including
// the pull request the graphite merge queue landed: it reports CLOSED, and the
// line has to say merged anyway. The empty-bodied and closed pull requests have
// no recorded payload behind them — every branch the downstack corpus reaches is
// a merged one carrying a body — so those arms are pinned over the decoded entry
// rather than over bytes nobody captured.
func TestInfoDownstackValue(t *testing.T) {
	t.Parallel()
	entries := []stackEntry{
		{Branch: "base", PR: 12, URL: "https://github.com/yasyf/cc-context/pull/12", HasBody: true, State: "MERGED", Merged: true, Checks: "SUCCESS"},
		{Branch: "queued", PR: 14, URL: "https://github.com/yasyf/cc-context/pull/14", HasBody: true, State: "CLOSED", Merged: true},
		{Branch: "abandoned", PR: 15, URL: "https://github.com/yasyf/cc-context/pull/15", HasBody: true, State: "CLOSED"},
		{Branch: "mid", PR: 13, URL: "https://github.com/yasyf/cc-context/pull/13", State: "OPEN", Checks: "PENDING"},
		{Branch: "tip"},
	}
	want := "base → PR #12 (body, merged, checks success) · queued → PR #14 (body, merged) · " +
		"abandoned → PR #15 (body, closed) · mid → PR #13 (no body, open, checks pending) · tip"
	if got := infoDownstackValue(entries); got != want {
		t.Errorf("infoDownstackValue() = %q, want %q", got, want)
	}
}

func TestVcsInfoUntrackedBranch(t *testing.T) {
	f := infoGTRepo(t)
	version := gtVersion(t, f)
	runTool(t, f.Dir, "git", "switch", "-qc", "feature")

	out, err := runVcsInfoCmd(t)
	if err != nil {
		t.Fatalf("info error = %v", err)
	}
	want := "config live · gt " + version + " · reachable · branch untracked (gt track --parent main)"
	if got := infoLine(t, out, "graphite"); got != want {
		t.Errorf("graphite = %q, want %q", got, want)
	}
	if strings.Contains(out, "downstack") {
		t.Errorf("report names a downstack for an untracked branch:\n%s", out)
	}
}

// TestVcsInfoGTStateFailureReported proves gt state failing is the report's
// answer rather than its failure: a stack nobody can read is the state someone
// runs info to diagnose, and the branch and dirtiness around it stay readable.
// The error still carries info's own prefix — it must never tell the reader to
// go look at ship.
func TestVcsInfoGTStateFailureReported(t *testing.T) {
	f := infoGTRepo(t, "feature")
	writeInfoFile(t, f.Dir, filepath.Join(".git", ".graphite_metadata.db"), "not a database\n")

	out, err := runVcsInfoCmd(t)
	if err != nil {
		t.Fatalf("info error = %v", err)
	}
	stack := infoLine(t, out, "stack")
	if !strings.HasPrefix(stack, "info: gt state:") {
		t.Errorf("stack = %q, want it to lead with info's own prefix", stack)
	}
	if strings.Contains(stack, "ship:") {
		t.Errorf("stack = %q, want info's own prefix, not ship's", stack)
	}
	if got := infoLine(t, out, "branch"); got != "feature" {
		t.Errorf("branch = %q, want the report to survive the unreadable stack", got)
	}
	if got := infoLine(t, out, "dirty"); got != "no" {
		t.Errorf("dirty = %q, want the report to survive the unreadable stack", got)
	}
	for _, absent := range []string{"trunk", "downstack"} {
		if strings.Contains(out, absent) {
			t.Errorf("report names %q gt state never gave it:\n%s", absent, out)
		}
	}
}

// TestVcsInfoGitTrunkFailureCarriesInfoPrefix pins the git lane's half of the
// same rule: a git that cannot answer at all aborts the report, and the error is
// info's — vcs.ResolveTrunk is shared, so an unprefixed one would send someone
// running info off to restack.
func TestVcsInfoGitTrunkFailureCarriesInfoPrefix(t *testing.T) {
	f := infoRepo(t, vcstest.Remote())
	writeInfoFile(t, f.Dir, filepath.Join(".git", "refs", "remotes", "origin", "HEAD"), "not-a-ref\n")

	out, err := runVcsInfoCmd(t)
	if err == nil {
		t.Fatalf("info succeeded over a corrupt origin/HEAD:\n%s", out)
	}
	if !strings.HasPrefix(err.Error(), "info: ") {
		t.Errorf("error = %v, want it to lead with info's own prefix", err)
	}
	if !strings.Contains(err.Error(), "exit 128") {
		t.Errorf("error = %v, want git's exit 128 surfaced", err)
	}
	if strings.Contains(err.Error(), "restack:") {
		t.Errorf("error = %v, want info's own prefix, not restack's", err)
	}
}

// TestVcsInfoGitTrunkMissRendersEmpty proves a repository that designates no
// default branch reports an empty trunk instead of aborting: --quiet keeps the
// miss (exit 1) apart from a git that broke (exit 128), and only the second is
// worth withholding the rest of the report over.
func TestVcsInfoGitTrunkMissRendersEmpty(t *testing.T) {
	f := infoRepo(t, vcstest.Remote(), vcstest.NoOriginHead(), vcstest.Dirty())
	// The miss must not be mistaken for a repository with no main branch at all:
	// refs/remotes/origin/main is here, it is simply not designated.
	runTool(t, f.Dir, "git", "rev-parse", "--verify", "refs/remotes/origin/main")

	out, err := runVcsInfoCmd(t)
	if err != nil {
		t.Fatalf("info error = %v", err)
	}
	if strings.Contains(out, "trunk") {
		t.Errorf("report names a trunk the repository designates none of:\n%s", out)
	}
	if got := infoLine(t, out, "branch"); got != "main" {
		t.Errorf("branch = %q, want the rest of the report to survive", got)
	}
	if got := infoLine(t, out, "dirty"); got != "yes (1 file)" {
		t.Errorf("dirty = %q, want the rest of the report to survive", got)
	}
}

// TestVcsInfoTrunkHolder proves info names the working copy holding trunk — the
// thing that explains a gt restack skipping a branch "because it is checked out
// in worktree W" and still exiting 0 — and stays silent when no checkout holds
// it, which is what git reports once trunk is nobody's current branch.
func TestVcsInfoTrunkHolder(t *testing.T) {
	tests := []struct {
		name string
		held bool
	}{
		{"trunk held elsewhere", true},
		{"trunk held by nobody", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := infoGTRepo(t, "feature")
			want := ""
			if tt.held {
				want = f.WorktreePath("trunk")
				if err := os.MkdirAll(filepath.Dir(want), 0o750); err != nil {
					t.Fatalf("mkdir worktree parent: %v", err)
				}
				runTool(t, f.Dir, "git", "worktree", "add", "-q", want, "main")
			}

			got := runVcsInfoJSON(t)
			if got.TrunkHolder != want {
				t.Errorf("trunk_holder = %q, want %q", got.TrunkHolder, want)
			}
			human, err := runVcsInfoCmd(t)
			if err != nil {
				t.Fatalf("info error = %v", err)
			}
			if want == "" {
				if strings.Contains(human, "trunk-held") {
					t.Errorf("report names a trunk holder nobody is:\n%s", human)
				}
				return
			}
			if line := infoLine(t, human, "trunk-held"); line != want {
				t.Errorf("trunk-held = %q, want %q", line, want)
			}
		})
	}
}

// TestVcsInfoMainCheckoutHasNoWorktreeBlock proves the worktree block is the
// linked checkout's answer alone: a repository's own working copy is not "inside"
// anything, so reporting a shape, a main root, and a repo key that all restate
// root would be noise.
func TestVcsInfoMainCheckoutHasNoWorktreeBlock(t *testing.T) {
	infoRepo(t, vcstest.Remote())

	if got := runVcsInfoJSON(t); got.Worktree != nil {
		t.Errorf("worktree = %+v, want none for the repository's own working copy", got.Worktree)
	}
}

// TestVcsInfoLinkedWorktree proves a linked worktree reports where it sits
// rather than refusing, and that root stays this checkout's own tree — pointing
// it at the main working copy would name bytes ccx is not looking at.
func TestVcsInfoLinkedWorktree(t *testing.T) {
	f := vcstest.Repo(t, vcstest.Remote(), vcstest.Worktree("feat"))
	root := f.WorktreePath("feat")
	t.Chdir(root)
	seedLaneRecords(t, ".", laneSeed{})

	got := runVcsInfoJSON(t)
	if got.Root != root {
		t.Errorf("root = %q, want this checkout's own tree %q", got.Root, root)
	}
	// The repository's own paths are the gitdir pointer's bytes, resolved
	// symlink-free, so two checkouts reaching one repository key it identically.
	want := worktreeInfo{
		Shape:     "git worktree",
		MainRoot:  f.Dir,
		CommonDir: filepath.Join(f.Dir, ".git"),
		RepoKey:   filepath.Join(f.Dir, ".git"),
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
	vcstest.Repo(t, vcstest.BrokenGitDir())

	out, err := runVcsInfoCmd(t)
	if err != nil {
		t.Fatalf("info error = %v", err)
	}
	got := infoLine(t, out, "checkout")
	for _, want := range []string{"gitdir pointer resolves to nothing", "/nonexistent-repo"} {
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
	downstack := loadGHGolden(t, "downstack-graphql-three")
	f := infoGTRepo(t, downstackThree...)
	ghReplay(t, f, downstack)
	resetArgvLog(t, f)

	if _, err := runVcsInfoCmd(t); err != nil {
		t.Fatalf("info error = %v", err)
	}
	vcstest.Quiesce(t, f.ArgvLog)
	invocations := vcstest.Invocations(t, f.ArgvLog)
	assertNoInvocation(t, invocations, "gh", "repo", "view")
	assertNoInvocation(t, invocations, "gt", "auth")
	var graphql [][]string
	for _, inv := range invocations {
		if len(inv) > 2 && inv[0] == "gh" && inv[1] == "api" && inv[2] == "graphql" {
			graphql = append(graphql, inv)
		}
	}
	assertInvocations(t, graphql, [][]string{ghDownstackPRArgv(downstackThree...)})
}
